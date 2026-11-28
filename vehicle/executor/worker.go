package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
)

// LateEvidence records terminal receipts arriving after the reconciliation window closed.
type LateEvidence struct {
	CommandID  string
	Status     ReceiptStatus
	Reason     string
	OccurredAt time.Time
	ReceivedAt time.Time
}

// StageEvent is an optional read-only observation of executor dispatch and receipt milestones.
type StageEvent struct {
	Stage           string // "executor.dispatch" or "executor.receipt"
	Status          string // "running", "done", "waiting", "error"
	Scope           contract.Scope
	Operation       contract.Operation
	TaskID          string
	CommandID       string
	RequestID       string
	DownstreamID    string
	EventID         string
	CommandStatus   contract.Status
	Params          map[string]any
	Reason          string
	ReconcileMethod string
	Attempts        uint32
	StartedAt       time.Time
	FinishedAt      time.Time
	DurationMs      int64
}

// Worker claims pending commands from H03's outbox, validates capabilities before
// sending, dispatches via VehicleChannel, handles receipts & telemetry confirmation,
// and emits deduplicated VehicleResultEvents for H08.
type Worker struct {
	WorkerID        string
	Store           tasks.Store
	Catalog         *catalog.Service
	Channel         VehicleChannel
	LeaseTTL        time.Duration
	PollInterval    time.Duration
	RetryBackoff    time.Duration
	ConfirmTimeout  time.Duration
	ReconcileWindow time.Duration
	Now             func() time.Time

	OnEvent        func(contract.VehicleResultEvent)
	OnStage        func(StageEvent)
	OnLateEvidence func(LateEvidence)

	// CrashBeforeSendOnce and CrashAfterSendOnce are deterministic test hooks
	// simulating worker process crashes around the vehicle send boundary.
	CrashBeforeSendOnce bool
	CrashAfterSendOnce  bool

	mu            sync.Mutex
	cmdByDownstrm map[string]string
	lateEvidence  []LateEvidence
}

func NewWorker(workerID string, store tasks.Store, cat *catalog.Service, ch VehicleChannel) *Worker {
	caps := ch.Capabilities()
	confirmTimeout := caps.ConfirmTimeout
	if confirmTimeout <= 0 {
		confirmTimeout = 30 * time.Second
	}
	reconcileWindow := caps.ReconcileWindow
	if reconcileWindow <= 0 {
		reconcileWindow = 5 * time.Minute
	}
	return &Worker{
		WorkerID:        workerID,
		Store:           store,
		Catalog:         cat,
		Channel:         ch,
		LeaseTTL:        15 * time.Second,
		PollInterval:    200 * time.Millisecond,
		RetryBackoff:    2 * time.Second,
		ConfirmTimeout:  confirmTimeout,
		ReconcileWindow: reconcileWindow,
		cmdByDownstrm:   make(map[string]string),
	}
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Worker) LateEvidences() []LateEvidence {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]LateEvidence, len(w.lateEvidence))
	copy(out, w.lateEvidence)
	return out
}

func (w *Worker) Run(ctx context.Context) error {
	interval := w.PollInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		for {
			processed, err := w.ProcessNext(ctx)
			if err != nil || !processed {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ProcessNext claims and processes at most one eligible command from H03's outbox.
// Returns (true, nil) when a command was claimed and processed, (false, nil) when idle.
func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	now := w.now()
	cmd, err := w.Store.ClaimNextCommand(ctx, w.WorkerID, w.leaseTTL(), now)
	if err != nil || cmd == nil {
		return false, err
	}

	// Case 1: Crash-recovery or reconciliation of an already-dispatched executing command.
	// Never blindly re-send an already executing command.
	if cmd.Status == contract.Executing {
		return true, w.reconcileCommand(ctx, cmd, now)
	}

	// Case 2: Re-check vehicle capability and preconditions right before dispatch.
	if w.Catalog != nil {
		if _, _, err := w.Catalog.ValidateMutation(cmd.Scope.VehicleID, cmd.Operation, cmd.Params, true); err != nil {
			return true, w.finalizeOutcome(ctx, cmd.CommandID, contract.Failed, "precondition failed: "+err.Error(), "precondition", now)
		}
	}

	if w.CrashBeforeSendOnce {
		w.CrashBeforeSendOnce = false
		// Worker dies before sending; lease remains in pending_dispatch until LeaseTTL expires.
		return true, nil
	}

	// Persist the send intent under the current lease. Recovery must reconcile
	// an interrupted send because the transport may already have received it.
	now = w.now()
	started, err := w.Store.BeginCommandSend(ctx, cmd.CommandID, w.WorkerID, cmd.Attempts, now)
	if err != nil || !started {
		return true, err
	}

	sendStart := time.Now().UTC()
	resp, sendErr := w.Channel.SendCommand(ctx, cmd.VehicleCommand)
	sendEnd := time.Now().UTC()
	sendDur := sendEnd.Sub(sendStart).Milliseconds()
	if w.CrashAfterSendOnce {
		w.CrashAfterSendOnce = false
		return true, nil
	}
	if resp.DownstreamID != "" {
		w.recordDownstreamID(cmd.CommandID, resp.DownstreamID)
	}
	if sendErr != nil {
		resp.Kind = OutcomeUncertain
		resp.Reason = sendErr.Error()
	}

	caps := w.Channel.Capabilities()
	switch resp.Kind {
	case OutcomeAccepted:
		// Acceptance receipt advances ONLY to executing; never emit completed yet.
		if err := w.Store.MarkCommandExecuting(ctx, cmd.CommandID, w.WorkerID, now, resp.DownstreamID); err != nil {
			return true, err
		}
		w.emitStage(StageEvent{
			Stage:         "executor.dispatch",
			Status:        "done",
			Scope:         cmd.Scope,
			Operation:     cmd.Operation,
			TaskID:        cmd.TaskID,
			CommandID:     cmd.CommandID,
			RequestID:     cmd.RequestID,
			DownstreamID:  resp.DownstreamID,
			CommandStatus: contract.Executing,
			Params:        cloneParams(cmd.Params),
			Attempts:      cmd.Attempts,
			StartedAt:     sendStart,
			FinishedAt:    sendEnd,
			DurationMs:    sendDur,
		})
		return true, nil

	case OutcomeCompleted:
		if err := w.Store.MarkCommandExecuting(ctx, cmd.CommandID, w.WorkerID, now, resp.DownstreamID); err != nil {
			return true, err
		}
		w.emitStage(StageEvent{
			Stage:         "executor.dispatch",
			Status:        "done",
			Scope:         cmd.Scope,
			Operation:     cmd.Operation,
			TaskID:        cmd.TaskID,
			CommandID:     cmd.CommandID,
			RequestID:     cmd.RequestID,
			DownstreamID:  resp.DownstreamID,
			CommandStatus: contract.Executing,
			Params:        cloneParams(cmd.Params),
			Attempts:      cmd.Attempts,
			StartedAt:     sendStart,
			FinishedAt:    sendEnd,
			DurationMs:    sendDur,
		})
		return true, w.finalizeOutcome(ctx, cmd.CommandID, contract.Completed, "vehicle_confirmed", "vehicle_confirmed", now)

	case OutcomeRejected:
		w.emitStage(StageEvent{
			Stage:         "executor.dispatch",
			Status:        "error",
			Scope:         cmd.Scope,
			Operation:     cmd.Operation,
			TaskID:        cmd.TaskID,
			CommandID:     cmd.CommandID,
			RequestID:     cmd.RequestID,
			DownstreamID:  resp.DownstreamID,
			CommandStatus: contract.Failed,
			Params:        cloneParams(cmd.Params),
			Reason:        resp.Reason,
			Attempts:      cmd.Attempts,
			StartedAt:     sendStart,
			FinishedAt:    sendEnd,
			DurationMs:    sendDur,
		})
		return true, w.finalizeOutcome(ctx, cmd.CommandID, contract.Failed, resp.Reason, "rejected", now)

	case OutcomeRetryableError:
		if caps.SupportsIdempotency && (caps.RetryWindow <= 0 || now.Sub(cmd.CreatedAt) < caps.RetryWindow) {
			if err := w.Store.ReleaseCommandForRetry(ctx, cmd.CommandID, w.WorkerID, now.Add(w.retryBackoff()), resp.Reason); err != nil {
				return true, err
			}
			w.emitStage(StageEvent{
				Stage:         "executor.dispatch",
				Status:        "waiting",
				Scope:         cmd.Scope,
				Operation:     cmd.Operation,
				TaskID:        cmd.TaskID,
				CommandID:     cmd.CommandID,
				RequestID:     cmd.RequestID,
				DownstreamID:  resp.DownstreamID,
				CommandStatus: contract.PendingDispatch,
				Params:        cloneParams(cmd.Params),
				Reason:        resp.Reason,
				Attempts:      cmd.Attempts,
				StartedAt:     sendStart,
				FinishedAt:    sendEnd,
				DurationMs:    sendDur,
			})
			return true, nil
		}
		return true, w.finalizeOutcome(ctx, cmd.CommandID, contract.Failed, "retry window exhausted: "+resp.Reason, "retry_exhausted", now)

	case OutcomeUncertain:
		// Delivery outcome is ambiguous: record executing and reconcile FIRST.
		if err := w.Store.MarkCommandExecuting(ctx, cmd.CommandID, w.WorkerID, now, resp.DownstreamID); err != nil {
			return true, err
		}
		w.emitStage(StageEvent{
			Stage:         "executor.dispatch",
			Status:        "waiting",
			Scope:         cmd.Scope,
			Operation:     cmd.Operation,
			TaskID:        cmd.TaskID,
			CommandID:     cmd.CommandID,
			RequestID:     cmd.RequestID,
			DownstreamID:  resp.DownstreamID,
			CommandStatus: contract.Executing,
			Params:        cloneParams(cmd.Params),
			Reason:        resp.Reason,
			Attempts:      cmd.Attempts,
			StartedAt:     sendStart,
			FinishedAt:    sendEnd,
			DurationMs:    sendDur,
		})
		refreshed, err := w.Store.GetCommand(ctx, cmd.CommandID)
		if err != nil {
			return true, err
		}
		// If downstream lacks idempotency, strictly never re-send; reconcile via query/telemetry or mark result_unknown.
		if !caps.SupportsIdempotency {
			if err := w.reconcileCommand(ctx, refreshed, now); err != nil {
				return true, err
			}
			// If neither query nor telemetry resolved it immediately and channel cannot confirm later, mark result_unknown.
			after, _ := w.Store.GetCommand(ctx, cmd.CommandID)
			if after != nil && after.Status == contract.Executing && !caps.SupportsStatusQuery && !caps.SupportsTelemetryConfirm {
				return true, w.finalizeOutcome(ctx, cmd.CommandID, contract.ResultUnknown, "uncertain delivery without downstream idempotency", "uncertain_delivery", now)
			}
			return true, nil
		}
		// Downstream supports idempotency: reconcile first before considering redelivery.
		return true, w.reconcileCommand(ctx, refreshed, now)

	default:
		return true, w.finalizeOutcome(ctx, cmd.CommandID, contract.ResultUnknown, "unknown send outcome", "unknown_outcome", now)
	}
}

// ReconcileCommand inspects downstream query and post-dispatch fresh telemetry for
// an executing command, advancing to completed/failed/result_unknown monotonically.
func (w *Worker) ReconcileCommand(ctx context.Context, commandID string) error {
	cmd, err := w.Store.GetCommand(ctx, commandID)
	if err != nil {
		return err
	}
	if cmd.Status != contract.Executing {
		return nil
	}
	return w.reconcileCommand(ctx, cmd, w.now())
}

func (w *Worker) reconcileCommand(ctx context.Context, cmd *tasks.OutboxCommand, now time.Time) error {
	caps := w.Channel.Capabilities()

	// 1. Downstream order query (if supported).
	if caps.SupportsStatusQuery {
		if rec, found, err := w.Channel.QueryCommand(ctx, cmd.CommandID, cmd.DownstreamID); err == nil && found {
			switch rec.Status {
			case ReceiptCompleted:
				ts := rec.OccurredAt
				if ts.IsZero() {
					ts = now
				}
				return w.finalizeOutcome(ctx, cmd.CommandID, contract.Completed, "downstream_query_confirmed", "downstream_query", ts)
			case ReceiptFailed:
				ts := rec.OccurredAt
				if ts.IsZero() {
					ts = now
				}
				return w.finalizeOutcome(ctx, cmd.CommandID, contract.Failed, rec.Reason, "downstream_query", ts)
			}
		}
	}

	// 2. Telemetry confirmation reusing H06 (target vehicle, target value, sampled_at >= dispatchedAt, fresh).
	if caps.SupportsTelemetryConfirm && w.Catalog != nil && cmd.DispatchedAt != nil {
		conf, err := w.Catalog.ConfirmCommandEffect(ctx, cmd.Scope.VehicleID, cmd.Operation, cmd.Params, *cmd.DispatchedAt, now)
		if err == nil && conf.Confirmed {
			return w.finalizeOutcome(ctx, cmd.CommandID, contract.Completed, "target telemetry confirmed", "telemetry", now)
		}
	}

	// 3. Confirm timeout -> result_unknown.
	dispatched := cmd.CreatedAt
	if cmd.DispatchedAt != nil {
		dispatched = *cmd.DispatchedAt
	}
	if now.Sub(dispatched) >= w.confirmTimeout() {
		return w.finalizeOutcome(ctx, cmd.CommandID, contract.ResultUnknown, "confirmation timed out", "timeout", now)
	}
	return nil
}

// HandleReceipt processes asynchronous vehicle receipts (accepted, completed, failed),
// enforcing idempotency, monotonic progression, and reconciliation-window boundaries.
func (w *Worker) HandleReceipt(ctx context.Context, receipt VehicleReceipt) error {
	now := w.now()
	cmdID := receipt.CommandID
	if cmdID == "" && receipt.DownstreamID != "" {
		cmdID = w.lookupByDownstreamID(receipt.DownstreamID)
	}
	if cmdID == "" {
		return &contract.BusinessError{Code: contract.InvalidArgument, Message: "receipt missing command_id"}
	}

	cmd, err := w.Store.GetCommand(ctx, cmdID)
	if err != nil {
		return err
	}
	if receipt.DownstreamID != "" {
		w.recordDownstreamID(cmdID, receipt.DownstreamID)
	}

	occurred := receipt.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}

	switch receipt.Status {
	case ReceiptAccepted:
		// Acceptance only promotes pending_dispatch -> executing; out-of-order acceptance
		// arriving after completed/failed is ignored cleanly.
		if cmd.Status == contract.PendingDispatch {
			if err := w.Store.MarkCommandExecuting(ctx, cmdID, "", occurred, receipt.DownstreamID); err != nil {
				return err
			}
			doneAt := time.Now().UTC()
			w.emitStage(StageEvent{
				Stage:         "executor.dispatch",
				Status:        "done",
				Scope:         cmd.Scope,
				Operation:     cmd.Operation,
				TaskID:        cmd.TaskID,
				CommandID:     cmd.CommandID,
				RequestID:     cmd.RequestID,
				DownstreamID:  receipt.DownstreamID,
				CommandStatus: contract.Executing,
				Params:        cloneParams(cmd.Params),
				StartedAt:     doneAt,
				FinishedAt:    doneAt,
			})
		}
		return nil

	case ReceiptCompleted, ReceiptFailed:
		// If already in result_unknown past the reconciliation window, record late evidence
		// without mutating the closed task state.
		if cmd.Status == contract.ResultUnknown && now.Sub(cmd.UpdatedAt) > w.reconcileWindow() {
			ev := LateEvidence{
				CommandID:  cmdID,
				Status:     receipt.Status,
				Reason:     receipt.Reason,
				OccurredAt: occurred,
				ReceivedAt: now,
			}
			w.mu.Lock()
			w.lateEvidence = append(w.lateEvidence, ev)
			w.mu.Unlock()
			if w.OnLateEvidence != nil {
				w.OnLateEvidence(ev)
			}
			return nil
		}
		next := contract.Completed
		reason := receipt.Reason
		if receipt.Status == ReceiptFailed {
			next = contract.Failed
		} else if reason == "" {
			reason = "vehicle_receipt_confirmed"
		}
		// If receipt arrives directly while still pending_dispatch, promote to executing first
		// so monotonic transition pending_dispatch -> executing -> completed/failed succeeds.
		if cmd.Status == contract.PendingDispatch {
			if err := w.Store.MarkCommandExecuting(ctx, cmdID, "", occurred, receipt.DownstreamID); err != nil {
				return err
			}
		}
		return w.finalizeOutcome(ctx, cmdID, next, reason, "receipt", occurred)

	default:
		return &contract.BusinessError{Code: contract.InvalidArgument, Message: fmt.Sprintf("unknown receipt status %q", receipt.Status)}
	}
}

func (w *Worker) finalizeOutcome(ctx context.Context, commandID string, next contract.Status, reason, method string, occurredAt time.Time) error {
	applied, ev, err := w.Store.TransitionCommandOutcome(ctx, commandID, next, reason, occurredAt)
	if err != nil {
		return err
	}
	if applied && ev != nil {
		if w.OnStage != nil {
			var (
				downstreamID string
				params       map[string]any
				startedAt    = occurredAt
			)
			if cmd, cmdErr := w.Store.GetCommand(ctx, commandID); cmdErr == nil && cmd != nil {
				downstreamID = cmd.DownstreamID
				params = cloneParams(cmd.Params)
				if cmd.DispatchedAt != nil && !cmd.DispatchedAt.IsZero() {
					startedAt = *cmd.DispatchedAt
				} else if !cmd.CreatedAt.IsZero() {
					startedAt = cmd.CreatedAt
				}
			}
			finishedAt := time.Now().UTC()
			dur := finishedAt.Sub(startedAt).Milliseconds()
			if dur < 0 {
				dur = 0
			}
			stepStatus := "done"
			switch ev.Status {
			case contract.Failed:
				stepStatus = "error"
			case contract.ResultUnknown:
				stepStatus = "waiting"
			}
			w.emitStage(StageEvent{
				Stage:           "executor.receipt",
				Status:          stepStatus,
				Scope:           ev.Scope,
				Operation:       ev.Operation,
				TaskID:          ev.TaskID,
				CommandID:       ev.CommandID,
				RequestID:       ev.RequestID,
				DownstreamID:    downstreamID,
				EventID:         ev.EventID,
				CommandStatus:   ev.Status,
				Params:          params,
				Reason:          reason,
				ReconcileMethod: method,
				StartedAt:       startedAt,
				FinishedAt:      finishedAt,
				DurationMs:      dur,
			})
		}
		if w.OnEvent != nil {
			w.OnEvent(*ev)
		}
	}
	return nil
}

func (w *Worker) emitStage(ev StageEvent) {
	if w.OnStage != nil {
		w.OnStage(ev)
	}
}

func cloneParams(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (w *Worker) leaseTTL() time.Duration {
	if w.LeaseTTL > 0 {
		return w.LeaseTTL
	}
	return 15 * time.Second
}

func (w *Worker) retryBackoff() time.Duration {
	if w.RetryBackoff > 0 {
		return w.RetryBackoff
	}
	return 2 * time.Second
}

func (w *Worker) confirmTimeout() time.Duration {
	if w.ConfirmTimeout > 0 {
		return w.ConfirmTimeout
	}
	return 30 * time.Second
}

func (w *Worker) reconcileWindow() time.Duration {
	if w.ReconcileWindow > 0 {
		return w.ReconcileWindow
	}
	return 5 * time.Minute
}

func (w *Worker) recordDownstreamID(cmdID, downstreamID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cmdByDownstrm[downstreamID] = cmdID
}

func (w *Worker) lookupByDownstreamID(downstreamID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cmdByDownstrm[downstreamID]
}

// FeedbackText renders static Chinese speech feedback aligned with H08's result announcements.
func FeedbackText(ev contract.VehicleResultEvent) string {
	switch ev.Status {
	case contract.Completed:
		switch ev.Operation {
		case contract.Operation{Device: "window", Tool: "close"}:
			return "车窗已关闭。"
		case contract.Operation{Device: "window", Tool: "open"}:
			return "车窗已打开。"
		case contract.Operation{Device: "hvac", Tool: "set_climate"}, contract.Operation{Device: "hvac", Tool: "set_temperature"}:
			return "空调已调整完成。"
		default:
			return "车辆指令已完成。"
		}
	case contract.Failed:
		return "车辆指令执行失败，请稍后重试。"
	case contract.ResultUnknown:
		return "车辆执行结果暂未确认，请稍后检查车辆状态。"
	default:
		return "正在处理车辆指令。"
	}
}
