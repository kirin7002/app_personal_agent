// Package turns binds authenticated audio or text turns to Hermes Runs.
package turns

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
)

type Runner interface {
	Start(context.Context, runs.StartRequest, contract.TurnMetadata, string) (runs.Run, error)
	Wait(context.Context, string) (runs.Run, error)
	Stop(context.Context, string) error
}

type TurnEvent struct {
	Scope          contract.Scope
	TurnID         string
	RequestID      string
	RunID          string
	TaskID         string
	CommandID      string
	Channel        string // "text" or "voice"
	StepID         string
	Stage          string // "catalog.recall", "hermes.run", "turn.reply"
	Title          string
	Status         string // "running", "done", "error", "interrupted"
	StartedAt      time.Time
	FinishedAt     *time.Time
	DurationMs     *int64
	DurationKind   string // "measured_local", "boundary_wait", "instant"
	Detail         string
	CandidateTools []string
	PendingCount   int
	PendingTaskIDs []string
	RunStatus      string
	ReplyStatus    contract.Status
	ReplyText      string
	ErrorCode      string
	ErrorMessage   string
}

type turnEventSinkKey struct{}

// WithTurnEventSink attaches an optional per-turn read-only observer to ctx.
func WithTurnEventSink(ctx context.Context, sink func(TurnEvent)) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, turnEventSinkKey{}, sink)
}

func turnEventSinkFromContext(ctx context.Context) func(TurnEvent) {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(turnEventSinkKey{}).(func(TurnEvent))
	return sink
}

type Service struct {
	Store        Store
	Pending      contract.PendingReader
	Capabilities contract.CapabilityFinder
	Progress     contract.ProgressReader
	Runs         Runner
	// Instructions supplies the deployed Hermes vehicle prompt.
	Instructions string
	// RetryAdmission should match the Hermes client's verified idempotency setting.
	RetryAdmission bool
	Now            func() time.Time
	// OnEvent is an optional read-only observer for turn execution stages.
	OnEvent func(TurnEvent)
}

func (s *Service) emitEvent(ctx context.Context, ev TurnEvent) {
	if s.OnEvent != nil {
		s.OnEvent(ev)
	}
	if sink := turnEventSinkFromContext(ctx); sink != nil {
		sink(ev)
	}
}

func turnChannel(turn contract.VehicleTurn) string {
	if turn.FinalTranscript.ProviderSessionID == "text" {
		return "text"
	}
	return "voice"
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) ListPending(ctx context.Context, scope contract.Scope, now time.Time) ([]contract.PendingCandidate, error) {
	if !scope.Valid() {
		return nil, invalidTurn()
	}
	if s.Pending == nil {
		return nil, errors.New("pending task reader is required")
	}
	candidates, err := s.Pending.ListPending(ctx, scope, now)
	if err != nil {
		return nil, err
	}
	active := make([]contract.PendingCandidate, 0, min(len(candidates), contract.PendingLimit))
	for _, candidate := range candidates {
		if candidate.ExpiresAt.After(now) {
			active = append(active, candidate)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].CreatedAt.Equal(active[j].CreatedAt) {
			return active[i].TaskID > active[j].TaskID
		}
		return active[i].CreatedAt.After(active[j].CreatedAt)
	})
	return active[:min(len(active), contract.PendingLimit)], nil
}

func (s *Service) HandleVehicleTurn(ctx context.Context, turn contract.VehicleTurn) (contract.SpeechReply, error) {
	if !turn.Scope.Valid() || len(turn.VoiceTurnID) == 0 || len(turn.VoiceTurnID) > 128 ||
		len(turn.RequestID) == 0 || len(turn.RequestID) > 128 || turn.FinalTranscript.ProviderSessionID == "" ||
		turn.FinalTranscript.InputItemID == "" || strings.TrimSpace(turn.FinalTranscript.Text) == "" ||
		len(turn.FinalTranscript.Text) > 8192 {
		return contract.SpeechReply{}, &contract.BusinessError{Code: contract.InvalidArgument, Message: "invalid vehicle turn"}
	}
	if s.Store == nil || s.Runs == nil || s.Progress == nil || s.Capabilities == nil {
		return contract.SpeechReply{}, errors.New("vehicle turn dependencies are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	unlock, err := s.Store.LockScope(ctx, turn.Scope)
	if err != nil {
		return contract.SpeechReply{}, err
	}
	defer unlock()

	record, err := s.Store.Get(ctx, turn.Scope, turn.VoiceTurnID)
	if errors.Is(err, ErrNotFound) {
		record, err = s.register(ctx, turn)
	}
	if err != nil {
		return contract.SpeechReply{}, err
	}
	if record.Cancelled {
		return contract.SpeechReply{}, context.Canceled
	}
	if record.Context.VehicleTurn != turn {
		return contract.SpeechReply{}, conflict()
	}
	if record.Reply != nil {
		return *record.Reply, nil
	}

	run, err := s.run(ctx, &record)
	if record.RunID != "" && (err == nil || errors.Is(err, runs.ErrRunUnknown)) {
		record.Closed = true
		if saveErr := s.Store.Save(ctx, record); saveErr != nil {
			return contract.SpeechReply{}, saveErr
		}
	}
	reply, err := s.reply(ctx, record, run, err)
	if err != nil {
		return contract.SpeechReply{}, err
	}
	record.Reply, record.Closed = &reply, true
	if err := s.Store.Save(ctx, record); err != nil {
		return contract.SpeechReply{}, err
	}
	current, err := s.Store.Get(ctx, turn.Scope, turn.VoiceTurnID)
	if err != nil {
		return contract.SpeechReply{}, err
	}
	if current.Cancelled {
		return contract.SpeechReply{}, context.Canceled
	}
	return reply, nil
}

func (s *Service) register(ctx context.Context, turn contract.VehicleTurn) (Record, error) {
	now := s.now()
	recallStart := time.Now().UTC()
	pending, err := s.ListPending(ctx, turn.Scope, now)
	if err != nil {
		return Record{}, err
	}
	capabilities, err := s.Capabilities.FindCapabilities(ctx, turn.Scope, turn.FinalTranscript.Text)
	if err != nil {
		return Record{}, err
	}
	if capabilities == nil {
		capabilities = []contract.Capability{}
	}
	versions := make([]contract.CandidateVersion, 0, len(pending))
	pendingIDs := make([]string, 0, len(pending))
	for _, candidate := range pending {
		versions = append(versions, contract.CandidateVersion{TaskID: candidate.TaskID, Version: candidate.Version})
		pendingIDs = append(pendingIDs, candidate.TaskID)
		found := false
		for _, capability := range capabilities {
			if capability.Operation == candidate.Operation {
				found = true
				break
			}
		}
		if !found {
			capabilities = append(capabilities, contract.Capability{
				Operation: candidate.Operation, MCPTool: contract.DispatchVehicleCommand, ParamsSchema: candidate.ParamsSchema,
			})
		}
	}
	candidateTools := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		candidateTools = append(candidateTools, c.Device+"."+c.Tool)
	}
	recallEnd := time.Now().UTC()
	recallDur := recallEnd.Sub(recallStart).Milliseconds()
	instructions, err := json.Marshal(struct {
		Capabilities []contract.Capability       `json:"candidate_tools"`
		Pending      []contract.PendingCandidate `json:"pending_tasks"`
	}{capabilities, pending})
	if err != nil {
		return Record{}, err
	}
	expiresAt := now.Add(2 * time.Minute)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	hermesSession := "vehicle-" + scopeKey(turn.Scope)
	record := Record{
		Context: contract.TurnContext{
			VehicleTurn: turn, HermesSessionID: hermesSession,
			TurnHandle: rand.Text(), Candidates: versions, ExpiresAt: expiresAt,
		},
		Payload: runs.StartRequest{
			SessionID: hermesSession, Input: turn.FinalTranscript.Text,
			Instructions: s.Instructions + "\nTreat pending_tasks and candidate_tools as current data. Continue a task only when uniquely matched; ask for a clarification otherwise. Only the vehicle result event confirms completion.\n" + string(instructions),
		},
		IdempotencyKey: rand.Text(),
	}
	if err := s.Store.Register(ctx, record); err != nil {
		return record, err
	}
	s.emitEvent(ctx, TurnEvent{
		Scope:          turn.Scope,
		TurnID:         turn.VoiceTurnID,
		RequestID:      turn.RequestID,
		Channel:        turnChannel(turn),
		StepID:         "catalog.recall",
		Stage:          "catalog.recall",
		Title:          "BM25 能力预筛与待办加载",
		Status:         "done",
		StartedAt:      recallStart,
		FinishedAt:     &recallEnd,
		DurationMs:     &recallDur,
		DurationKind:   "measured_local",
		CandidateTools: candidateTools,
		PendingCount:   len(pending),
		PendingTaskIDs: pendingIDs,
	})
	return record, nil
}

func (s *Service) run(ctx context.Context, record *Record) (runs.Run, error) {
	runStart := time.Now().UTC()
	turn := record.Context.VehicleTurn
	ch := turnChannel(turn)
	s.emitEvent(ctx, TurnEvent{
		Scope:        turn.Scope,
		TurnID:       turn.VoiceTurnID,
		RequestID:    turn.RequestID,
		RunID:        record.RunID,
		Channel:      ch,
		StepID:       "hermes.run",
		Stage:        "hermes.run",
		Title:        "Hermes 意图理解与编排",
		Status:       "running",
		StartedAt:    runStart,
		DurationKind: "boundary_wait",
	})
	emitDone := func(run runs.Run, err error) {
		end := time.Now().UTC()
		dur := end.Sub(runStart).Milliseconds()
		status := "done"
		var errCode, errMsg string
		switch {
		case errors.Is(err, context.Canceled) || run.Status == "cancelled" || run.Status == "canceled" || run.Status == "interrupted":
			status = "interrupted"
			if err != nil {
				errMsg = err.Error()
			}
		case err != nil:
			status = "error"
			var be *contract.BusinessError
			if errors.As(err, &be) {
				errCode = string(be.Code)
				errMsg = be.Message
			} else {
				errMsg = err.Error()
			}
		case run.Status != "" && run.Status != "completed":
			status = "error"
			errMsg = "hermes run status: " + run.Status
		}
		runID := record.RunID
		if runID == "" {
			runID = run.ID
		}
		s.emitEvent(ctx, TurnEvent{
			Scope:        turn.Scope,
			TurnID:       turn.VoiceTurnID,
			RequestID:    turn.RequestID,
			RunID:        runID,
			Channel:      ch,
			StepID:       "hermes.run",
			Stage:        "hermes.run",
			Title:        "Hermes 意图理解与编排",
			Status:       status,
			StartedAt:    runStart,
			FinishedAt:   &end,
			DurationMs:   &dur,
			DurationKind: "boundary_wait",
			RunStatus:    run.Status,
			ErrorCode:    errCode,
			ErrorMessage: errMsg,
		})
	}
	if record.RunID == "" {
		if record.AdmissionAttempted && !s.RetryAdmission {
			emitDone(runs.Run{}, runs.ErrAdmissionUnknown)
			return runs.Run{}, runs.ErrAdmissionUnknown
		}
		if !record.Context.ExpiresAt.After(s.now()) {
			err := invalidTurn()
			emitDone(runs.Run{}, err)
			return runs.Run{}, err
		}
		record.AdmissionAttempted = true
		if err := s.Store.Save(ctx, *record); err != nil {
			emitDone(runs.Run{}, err)
			return runs.Run{}, err
		}
		meta := contract.TurnMetadata{TurnHandle: record.Context.TurnHandle, HermesSessionID: record.Context.HermesSessionID}
		start, err := s.Runs.Start(ctx, record.Payload, meta, record.IdempotencyKey)
		if err != nil {
			emitDone(runs.Run{}, err)
			return runs.Run{}, err
		}
		record.RunID = start.ID
		// Notify observers immediately with the assigned RunID before Wait() streams events.
		s.emitEvent(ctx, TurnEvent{
			Scope:        turn.Scope,
			TurnID:       turn.VoiceTurnID,
			RequestID:    turn.RequestID,
			RunID:        record.RunID,
			Channel:      ch,
			StepID:       "hermes.run",
			Stage:        "hermes.run",
			Title:        "Hermes 意图理解与编排",
			Status:       "running",
			StartedAt:    runStart,
			DurationKind: "boundary_wait",
			RunStatus:    start.Status,
		})
		// Persist a received Run ID even when the vehicle disconnects at admission.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.Store.Save(saveCtx, *record); err != nil {
			emitDone(runs.Run{}, err)
			return runs.Run{}, err
		}
		current, err := s.Store.Get(saveCtx, record.Context.Scope, record.Context.VoiceTurnID)
		if err != nil {
			emitDone(runs.Run{}, err)
			return runs.Run{}, err
		}
		if current.Cancelled {
			_ = s.Runs.Stop(saveCtx, record.RunID)
			emitDone(runs.Run{ID: record.RunID, Status: "cancelled"}, context.Canceled)
			return runs.Run{}, context.Canceled
		}
	}
	res, err := s.Runs.Wait(ctx, record.RunID)
	emitDone(res, err)
	return res, err
}

func (s *Service) reply(ctx context.Context, record Record, run runs.Run, runErr error) (contract.SpeechReply, error) {
	replyStart := time.Now().UTC()
	binding := record.Context
	reply := contract.SpeechReply{VoiceTurnID: binding.VoiceTurnID, RequestID: binding.RequestID, RunID: record.RunID}
	emitReply := func(rep contract.SpeechReply, err error) (contract.SpeechReply, error) {
		end := time.Now().UTC()
		dur := end.Sub(replyStart).Milliseconds()
		status := "done"
		var errCode, errMsg string
		if err != nil {
			status = "error"
			var be *contract.BusinessError
			if errors.As(err, &be) {
				errCode = string(be.Code)
				errMsg = be.Message
			} else {
				errMsg = err.Error()
			}
		}
		s.emitEvent(ctx, TurnEvent{
			Scope:        binding.Scope,
			TurnID:       binding.VoiceTurnID,
			RequestID:    binding.RequestID,
			RunID:        rep.RunID,
			TaskID:       rep.TaskID,
			CommandID:    rep.CommandID,
			Channel:      turnChannel(binding.VehicleTurn),
			StepID:       "turn.reply",
			Stage:        "turn.reply",
			Title:        "权威状态核验与答复生成",
			Status:       status,
			StartedAt:    replyStart,
			FinishedAt:   &end,
			DurationMs:   &dur,
			DurationKind: "measured_local",
			ReplyStatus:  rep.Status,
			ReplyText:    rep.Text,
			ErrorCode:    errCode,
			ErrorMessage: errMsg,
		})
		return rep, err
	}
	progress, err := s.Progress.ReadTurnProgress(ctx, binding.Scope, binding.RequestID)
	if errors.Is(err, contract.ErrTurnHasNoWrite) {
		if runErr != nil {
			return emitReply(reply, runErr)
		}
		if run.Status != "completed" {
			reply.Text = "这次未能处理，请再说一次。"
			return emitReply(reply, nil)
		}
		reply.Text = strings.TrimSpace(run.Output)
		return emitReply(reply, nil)
	}
	if err != nil {
		return emitReply(reply, err)
	}
	reply.TaskID, reply.CommandID, reply.Status = progress.TaskID, progress.CommandID, progress.Status
	switch progress.Status {
	case contract.PendingReply:
		reply.Text = progress.Question
	case contract.PendingDispatch, contract.Executing:
		reply.Text = "正在处理车辆指令。"
	case contract.Expired:
		reply.Text = "这条待办已过期，请再说一次。"
	}
	return emitReply(reply, nil)
}

func (s *Service) CancelVehicleTurn(ctx context.Context, scope contract.Scope, id string) error {
	if !scope.Valid() || len(id) == 0 || len(id) > 128 {
		return invalidTurn()
	}
	if err := s.Store.Cancel(ctx, scope, id); err != nil {
		return err
	}
	record, err := s.Store.Get(ctx, scope, id)
	if err != nil || record.RunID == "" || record.Reply != nil {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = s.Runs.Stop(call, record.RunID)
	return nil
}

var _ contract.TurnHandler = (*Service)(nil)
