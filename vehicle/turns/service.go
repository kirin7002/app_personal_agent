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

type Service struct {
	Store        Store
	Pending      contract.PendingReader
	Capabilities contract.CapabilityFinder
	Progress     contract.ProgressReader
	Runs         Runner
	// RetryAdmission should match the Hermes client's verified idempotency setting.
	RetryAdmission bool
	Now            func() time.Time
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
	for _, candidate := range pending {
		versions = append(versions, contract.CandidateVersion{TaskID: candidate.TaskID, Version: candidate.Version})
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
	instructions, err := json.Marshal(struct {
		Capabilities []contract.Capability       `json:"candidate_tools"`
		Pending      []contract.PendingCandidate `json:"pending_tasks"`
	}{capabilities, pending})
	if err != nil {
		return Record{}, err
	}
	hermesSession := "vehicle-" + scopeKey(turn.Scope)
	record := Record{
		Context: contract.TurnContext{
			VehicleTurn: turn, HermesSessionID: hermesSession,
			TurnHandle: rand.Text(), Candidates: versions, ExpiresAt: now.Add(2 * time.Minute),
		},
		Payload: runs.StartRequest{
			SessionID: hermesSession, Input: turn.FinalTranscript.Text,
			Instructions: "Treat pending_tasks and candidate_tools as current data. Continue a task only when uniquely matched; ask for a clarification otherwise. Only the vehicle result event confirms completion.\n" + string(instructions),
		},
		IdempotencyKey: rand.Text(),
	}
	return record, s.Store.Register(ctx, record)
}

func (s *Service) run(ctx context.Context, record *Record) (runs.Run, error) {
	if record.RunID == "" {
		if record.AdmissionAttempted && !s.RetryAdmission {
			return runs.Run{}, runs.ErrAdmissionUnknown
		}
		if !record.Context.ExpiresAt.After(s.now()) {
			return runs.Run{}, invalidTurn()
		}
		record.AdmissionAttempted = true
		if err := s.Store.Save(ctx, *record); err != nil {
			return runs.Run{}, err
		}
		meta := contract.TurnMetadata{TurnHandle: record.Context.TurnHandle, HermesSessionID: record.Context.HermesSessionID}
		start, err := s.Runs.Start(ctx, record.Payload, meta, record.IdempotencyKey)
		if err != nil {
			return runs.Run{}, err
		}
		record.RunID = start.ID
		// Persist a received Run ID even when the vehicle disconnects at admission.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := s.Store.Save(saveCtx, *record); err != nil {
			return runs.Run{}, err
		}
		current, err := s.Store.Get(saveCtx, record.Context.Scope, record.Context.VoiceTurnID)
		if err != nil {
			return runs.Run{}, err
		}
		if current.Cancelled {
			_ = s.Runs.Stop(saveCtx, record.RunID)
			return runs.Run{}, context.Canceled
		}
	}
	return s.Runs.Wait(ctx, record.RunID)
}

func (s *Service) reply(ctx context.Context, record Record, run runs.Run, runErr error) (contract.SpeechReply, error) {
	binding := record.Context
	reply := contract.SpeechReply{VoiceTurnID: binding.VoiceTurnID, RequestID: binding.RequestID, RunID: record.RunID}
	progress, err := s.Progress.ReadTurnProgress(ctx, binding.Scope, binding.RequestID)
	if errors.Is(err, contract.ErrTurnHasNoWrite) {
		if runErr != nil {
			return reply, runErr
		}
		if run.Status != "completed" {
			reply.Text = "这次未能处理，请再说一次。"
			return reply, nil
		}
		reply.Text = strings.TrimSpace(run.Output)
		return reply, nil
	}
	if err != nil {
		return reply, err
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
	return reply, nil
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
