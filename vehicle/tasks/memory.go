package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

type turnWriteKey struct {
	scopeKey  string
	requestID string
}

type turnWriteRecord struct {
	OperationKind string
	Fingerprint   string
	TaskID        string
	CommandID     string
	Result        contract.TaskResult
	CreatedAt     time.Time
}

// MemoryStore implements Store with full atomic rollback and concurrency safety
// for single-process simulations and deterministic tests.
type MemoryStore struct {
	Validator ParamValidator
	Now       func() time.Time
	// FailOutboxInsertOnce is a test hook simulating mid-transaction persistence failure.
	FailOutboxInsertOnce bool

	mu         sync.Mutex
	tasks      map[string]*TaskRecord
	turnWrites map[turnWriteKey]turnWriteRecord
	commands   map[string]*OutboxCommand
	cmdByTask  map[string]string
	events     map[string]contract.VehicleResultEvent
	taskSeq    int64
	cmdSeq     int64
}

func NewMemoryStore(validator ParamValidator) *MemoryStore {
	return &MemoryStore{
		Validator:  validator,
		tasks:      make(map[string]*TaskRecord),
		turnWrites: make(map[turnWriteKey]turnWriteRecord),
		commands:   make(map[string]*OutboxCommand),
		cmdByTask:  make(map[string]string),
		events:     make(map[string]contract.VehicleResultEvent),
		taskSeq:    100,
		cmdSeq:     87,
	}
}

func (m *MemoryStore) now(override time.Time) time.Time {
	if !override.IsZero() {
		return override.UTC()
	}
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *MemoryStore) nextTaskIDLocked() string {
	m.taskSeq++
	return fmt.Sprintf("T%d", m.taskSeq)
}

func (m *MemoryStore) nextCommandIDLocked() string {
	m.cmdSeq++
	return fmt.Sprintf("C%d", m.cmdSeq)
}

func (m *MemoryStore) ListPending(_ context.Context, scope contract.Scope, now time.Time) ([]contract.PendingCandidate, error) {
	if !scope.Valid() {
		return nil, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	now = m.now(now)

	m.mu.Lock()
	var matched []*TaskRecord
	for _, t := range m.tasks {
		if t.Scope != scope || t.Status != contract.PendingReply {
			continue
		}
		if t.ExpiresAt == nil || !t.ExpiresAt.After(now) {
			continue
		}
		cp := *t
		cp.KnownParams = cloneMap(t.KnownParams)
		cp.MissingFields = cloneStrings(t.MissingFields)
		matched = append(matched, &cp)
	}
	m.mu.Unlock()

	candidates := make([]contract.PendingCandidate, 0, len(matched))
	for _, t := range matched {
		var schema json.RawMessage
		if m.Validator != nil {
			s, err := m.Validator.SchemaFor(scope.VehicleID, t.Operation)
			if err == nil {
				schema = s
			}
		}
		candidates = append(candidates, contract.PendingCandidate{
			Operation:     t.Operation,
			TaskID:        t.TaskID,
			Version:       t.Version,
			CreatedAt:     t.CreatedAt,
			ExpiresAt:     *t.ExpiresAt,
			KnownParams:   t.KnownParams,
			MissingFields: t.MissingFields,
			Question:      t.Question,
			ParamsSchema:  schema,
		})
	}
	sortPendingCandidates(candidates)
	if len(candidates) > contract.PendingLimit {
		candidates = candidates[:contract.PendingLimit]
	}
	return candidates, nil
}

func (m *MemoryStore) ReadTurnProgress(_ context.Context, scope contract.Scope, requestID string) (contract.TaskProgress, error) {
	if !scope.Valid() || requestID == "" {
		return contract.TaskProgress{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope or request_id"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	wr, ok := m.turnWrites[turnWriteKey{scopeKey: scopeKey(scope), requestID: requestID}]
	if !ok {
		return contract.TaskProgress{}, contract.ErrTurnHasNoWrite
	}
	task, exists := m.tasks[wr.TaskID]
	if !exists {
		return contract.TaskProgress{
			TaskID:    wr.TaskID,
			CommandID: wr.CommandID,
			Status:    wr.Result.Status,
			Question:  wr.Result.Question,
		}, nil
	}
	return contract.TaskProgress{
		TaskID:    task.TaskID,
		CommandID: task.CommandID,
		Status:    task.Status,
		Question:  task.Question,
	}, nil
}

func (m *MemoryStore) CreateOrUpdateTask(ctx context.Context, req TaskWriteRequest) (contract.TaskResult, error) {
	now := m.now(req.Now)
	if m.Validator == nil {
		return contract.TaskResult{}, fmt.Errorf("tasks validator is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	turn := req.Turn
	if req.MemLock != nil {
		locked, err := req.MemLock(ctx, now)
		if err != nil {
			return contract.TaskResult{}, err
		}
		turn = locked
	}
	if !turn.Scope.Valid() || turn.RequestID == "" {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid turn context"}
	}

	fp := requestFingerprint(OpKindCreateTask, req.Mutation)
	wk := turnWriteKey{scopeKey: scopeKey(turn.Scope), requestID: turn.RequestID}
	if prev, exists := m.turnWrites[wk]; exists {
		if prev.Fingerprint == fp {
			return prev.Result, nil
		}
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.IdempotencyConflict,
			Message: "同一轮次已提交另一项操作",
		}
	}

	// Case 1: Partial parameter update on an existing pending_reply task.
	if req.Mutation.TaskID != "" {
		task, ok := m.tasks[req.Mutation.TaskID]
		if !ok || task.Scope != turn.Scope {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task not found in current scope",
			}
		}
		if task.Status == contract.Expired || (task.Status == contract.PendingReply && (task.ExpiresAt == nil || !task.ExpiresAt.After(now))) {
			if task.Status == contract.PendingReply {
				task.Status = contract.Expired
				task.UpdatedAt = now
			}
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskExpired,
				Message: "待回复任务已到期",
			}
		}
		if task.Status != contract.PendingReply {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task is no longer waiting for reply",
			}
		}
		if req.ExpectedVersion != 0 && task.Version != req.ExpectedVersion {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskConflict,
				Message: "task version changed concurrently",
			}
		}
		if task.Operation != req.Mutation.Operation {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskConflict,
				Message: "device or tool does not match existing pending task",
			}
		}
		merged, err := mergeParams(task.KnownParams, req.Mutation.Params)
		if err != nil {
			return contract.TaskResult{}, err
		}
		val, err := m.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, merged, false)
		if err != nil {
			return contract.TaskResult{}, err
		}
		if len(val.MissingFields) == 0 {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.InvalidArgument,
				Message: "all required parameters are present; use dispatch_vehicle_command",
			}
		}

		// Preserve original expires_at!
		task.KnownParams = val.Normalized
		task.MissingFields = val.MissingFields
		task.Question = val.Question
		task.Version++
		task.LastRequestID = turn.RequestID
		task.UpdatedAt = now

		exp := *task.ExpiresAt
		res := contract.TaskResult{
			TaskID:        task.TaskID,
			Status:        contract.PendingReply,
			MissingFields: cloneStrings(task.MissingFields),
			Question:      task.Question,
			ExpiresAt:     &exp,
		}
		m.turnWrites[wk] = turnWriteRecord{
			OperationKind: OpKindCreateTask,
			Fingerprint:   fp,
			TaskID:        task.TaskID,
			Result:        res,
			CreatedAt:     now,
		}
		return res, nil
	}

	// Case 2: Brand-new pending_reply task.
	val, err := m.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, req.Mutation.Params, false)
	if err != nil {
		return contract.TaskResult{}, err
	}
	if len(val.MissingFields) == 0 {
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "all required parameters are present; use dispatch_vehicle_command",
		}
	}

	taskID := m.nextTaskIDLocked()
	exp := now.Add(contract.PendingTTL)
	task := &TaskRecord{
		TaskID:        taskID,
		Scope:         turn.Scope,
		Operation:     req.Mutation.Operation,
		Status:        contract.PendingReply,
		Version:       1,
		KnownParams:   val.Normalized,
		MissingFields: val.MissingFields,
		Question:      val.Question,
		LastRequestID: turn.RequestID,
		CreatedAt:     now,
		ExpiresAt:     &exp,
		UpdatedAt:     now,
	}
	res := contract.TaskResult{
		TaskID:        taskID,
		Status:        contract.PendingReply,
		MissingFields: cloneStrings(val.MissingFields),
		Question:      val.Question,
		ExpiresAt:     &exp,
	}
	m.tasks[taskID] = task
	m.turnWrites[wk] = turnWriteRecord{
		OperationKind: OpKindCreateTask,
		Fingerprint:   fp,
		TaskID:        taskID,
		Result:        res,
		CreatedAt:     now,
	}
	return res, nil
}

func (m *MemoryStore) DispatchCommand(ctx context.Context, req TaskWriteRequest) (contract.TaskResult, error) {
	now := m.now(req.Now)
	if m.Validator == nil {
		return contract.TaskResult{}, fmt.Errorf("tasks validator is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	turn := req.Turn
	if req.MemLock != nil {
		locked, err := req.MemLock(ctx, now)
		if err != nil {
			return contract.TaskResult{}, err
		}
		turn = locked
	}
	if !turn.Scope.Valid() || turn.RequestID == "" {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid turn context"}
	}

	fp := requestFingerprint(OpKindDispatchCommand, req.Mutation)
	wk := turnWriteKey{scopeKey: scopeKey(turn.Scope), requestID: turn.RequestID}
	if prev, exists := m.turnWrites[wk]; exists {
		if prev.Fingerprint == fp {
			return prev.Result, nil
		}
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.IdempotencyConflict,
			Message: "同一轮次已提交另一项操作",
		}
	}

	// Resume existing pending_reply task.
	if req.Mutation.TaskID != "" {
		task, ok := m.tasks[req.Mutation.TaskID]
		if !ok || task.Scope != turn.Scope {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task not found in current scope",
			}
		}
		if task.Status == contract.Expired || (task.Status == contract.PendingReply && (task.ExpiresAt == nil || !task.ExpiresAt.After(now))) {
			if task.Status == contract.PendingReply {
				task.Status = contract.Expired
				task.UpdatedAt = now
			}
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskExpired,
				Message: "待回复任务已到期",
			}
		}
		if task.Status != contract.PendingReply {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskNotEligible,
				Message: "task is not eligible for dispatch",
			}
		}
		if req.ExpectedVersion != 0 && task.Version != req.ExpectedVersion {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskConflict,
				Message: "task version mismatch",
			}
		}
		if task.Operation != req.Mutation.Operation {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskConflict,
				Message: "device or tool does not match existing task",
			}
		}
		if _, dup := m.cmdByTask[task.TaskID]; dup {
			return contract.TaskResult{}, &contract.BusinessError{
				Code:    contract.TaskConflict,
				Message: "command already registered for task",
			}
		}
		merged, err := mergeParams(task.KnownParams, req.Mutation.Params)
		if err != nil {
			return contract.TaskResult{}, err
		}
		val, err := m.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, merged, true)
		if err != nil {
			return contract.TaskResult{}, err
		}

		// Simulate transactional failure before committing any state change -> zero orphans!
		if m.FailOutboxInsertOnce {
			m.FailOutboxInsertOnce = false
			return contract.TaskResult{}, fmt.Errorf("simulated outbox write failure")
		}

		cmdID := m.nextCommandIDLocked()
		cmd := &OutboxCommand{
			VehicleCommand: contract.VehicleCommand{
				Scope:     turn.Scope,
				Operation: task.Operation,
				CommandID: cmdID,
				TaskID:    task.TaskID,
				RequestID: turn.RequestID,
				Params:    cloneMap(val.Normalized),
			},
			Status:    contract.PendingDispatch,
			CreatedAt: now,
			UpdatedAt: now,
		}

		task.Status = contract.PendingDispatch
		task.KnownParams = val.Normalized
		task.MissingFields = nil
		task.CommandID = cmdID
		task.Version++
		task.LastRequestID = turn.RequestID
		task.UpdatedAt = now

		m.commands[cmdID] = cmd
		m.cmdByTask[task.TaskID] = cmdID

		res := contract.TaskResult{
			TaskID:    task.TaskID,
			CommandID: cmdID,
			Status:    contract.PendingDispatch,
		}
		m.turnWrites[wk] = turnWriteRecord{
			OperationKind: OpKindDispatchCommand,
			Fingerprint:   fp,
			TaskID:        task.TaskID,
			CommandID:     cmdID,
			Result:        res,
			CreatedAt:     now,
		}
		return res, nil
	}

	// Brand-new complete command -> create task in pending_dispatch + command outbox atomically.
	val, err := m.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, req.Mutation.Params, true)
	if err != nil {
		return contract.TaskResult{}, err
	}
	if m.FailOutboxInsertOnce {
		m.FailOutboxInsertOnce = false
		return contract.TaskResult{}, fmt.Errorf("simulated outbox write failure")
	}

	taskID := m.nextTaskIDLocked()
	cmdID := m.nextCommandIDLocked()
	task := &TaskRecord{
		TaskID:        taskID,
		Scope:         turn.Scope,
		Operation:     req.Mutation.Operation,
		Status:        contract.PendingDispatch,
		Version:       1,
		KnownParams:   val.Normalized,
		CommandID:     cmdID,
		LastRequestID: turn.RequestID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	cmd := &OutboxCommand{
		VehicleCommand: contract.VehicleCommand{
			Scope:     turn.Scope,
			Operation: req.Mutation.Operation,
			CommandID: cmdID,
			TaskID:    taskID,
			RequestID: turn.RequestID,
			Params:    cloneMap(val.Normalized),
		},
		Status:    contract.PendingDispatch,
		CreatedAt: now,
		UpdatedAt: now,
	}

	res := contract.TaskResult{
		TaskID:    taskID,
		CommandID: cmdID,
		Status:    contract.PendingDispatch,
	}
	m.tasks[taskID] = task
	m.commands[cmdID] = cmd
	m.cmdByTask[taskID] = cmdID
	m.turnWrites[wk] = turnWriteRecord{
		OperationKind: OpKindDispatchCommand,
		Fingerprint:   fp,
		TaskID:        taskID,
		CommandID:     cmdID,
		Result:        res,
		CreatedAt:     now,
	}
	return res, nil
}

func (m *MemoryStore) ListTasks(_ context.Context, scope contract.Scope, input contract.ListTasksInput, now time.Time) (contract.ListTasksResult, error) {
	if !scope.Valid() {
		return contract.ListTasksResult{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	now = m.now(now)

	allowed := defaultIncompleteStatuses()
	explicit := len(input.Status) > 0
	if explicit {
		allowed = make(map[contract.Status]bool, len(input.Status))
		for _, st := range input.Status {
			allowed[st] = true
		}
	}

	offset := 0
	if input.Cursor != "" {
		parsed, err := strconv.Atoi(input.Cursor)
		if err != nil || parsed < 0 {
			return contract.ListTasksResult{}, &contract.BusinessError{Code: contract.InvalidArgument, Message: "invalid cursor"}
		}
		offset = parsed
	}

	m.mu.Lock()
	var rows []*TaskRecord
	for _, t := range m.tasks {
		if t.Scope != scope {
			continue
		}
		effectiveStatus := t.Status
		if t.Status == contract.PendingReply && (t.ExpiresAt == nil || !t.ExpiresAt.After(now)) {
			// Immediately treat overdue pending_reply as expired even if scanner lagged.
			effectiveStatus = contract.Expired
		}
		if !allowed[effectiveStatus] {
			continue
		}
		cp := *t
		cp.Status = effectiveStatus
		cp.MissingFields = cloneStrings(t.MissingFields)
		rows = append(rows, &cp)
	}
	m.mu.Unlock()

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].TaskID > rows[j].TaskID
		}
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})

	if offset >= len(rows) {
		return contract.ListTasksResult{Tasks: []contract.TaskResult{}}, nil
	}
	end := min(offset+defaultListLimit, len(rows))
	out := make([]contract.TaskResult, 0, end-offset)
	for _, t := range rows[offset:end] {
		var exp *time.Time
		if t.ExpiresAt != nil {
			v := *t.ExpiresAt
			exp = &v
		}
		out = append(out, contract.TaskResult{
			TaskID:        t.TaskID,
			CommandID:     t.CommandID,
			Status:        t.Status,
			MissingFields: t.MissingFields,
			Question:      t.Question,
			ExpiresAt:     exp,
		})
	}
	nextCursor := ""
	if end < len(rows) {
		nextCursor = strconv.Itoa(end)
	}
	return contract.ListTasksResult{Tasks: out, NextCursor: nextCursor}, nil
}

func (m *MemoryStore) GetTask(_ context.Context, scope contract.Scope, taskID string) (TaskRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok || t.Scope != scope {
		return TaskRecord{}, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "task not found"}
	}
	cp := *t
	cp.KnownParams = cloneMap(t.KnownParams)
	cp.MissingFields = cloneStrings(t.MissingFields)
	return cp, nil
}

func (m *MemoryStore) ExpirePending(_ context.Context, now time.Time) (int64, error) {
	now = m.now(now)
	m.mu.Lock()
	defer m.mu.Unlock()

	var expired int64
	for _, t := range m.tasks {
		if t.Status == contract.PendingReply && t.ExpiresAt != nil && !t.ExpiresAt.After(now) {
			t.Status = contract.Expired
			t.Version++
			t.UpdatedAt = now
			expired++
		}
	}
	return expired, nil
}

func (m *MemoryStore) ClaimNextCommand(_ context.Context, workerID string, leaseTTL time.Duration, now time.Time) (*OutboxCommand, error) {
	now = m.now(now)
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var eligible []*OutboxCommand
	for _, c := range m.commands {
		if c.Status == contract.PendingDispatch {
			if c.LeaseUntil == nil || !c.LeaseUntil.After(now) {
				eligible = append(eligible, c)
			}
		} else if c.Status == contract.Executing && c.LeaseUntil != nil && !c.LeaseUntil.After(now) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return nil, nil
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].CreatedAt.Equal(eligible[j].CreatedAt) {
			return eligible[i].CommandID < eligible[j].CommandID
		}
		return eligible[i].CreatedAt.Before(eligible[j].CreatedAt)
	})
	picked := eligible[0]
	until := now.Add(leaseTTL)
	picked.LeaseOwner = workerID
	picked.LeaseUntil = &until
	picked.Attempts++
	picked.UpdatedAt = now

	cp := *picked
	cp.Params = cloneMap(picked.Params)
	return &cp, nil
}

func (m *MemoryStore) GetCommand(_ context.Context, commandID string) (*OutboxCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.commands[commandID]
	if !ok {
		return nil, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	cp := *c
	cp.Params = cloneMap(c.Params)
	return &cp, nil
}

func (m *MemoryStore) MarkCommandExecuting(_ context.Context, commandID, workerID string, dispatchedAt time.Time, downstreamID string) error {
	dispatchedAt = m.now(dispatchedAt)
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.commands[commandID]
	if !ok {
		return &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	if c.Status == contract.Completed || c.Status == contract.Failed {
		return nil
	}
	if workerID != "" && c.LeaseOwner != "" && c.LeaseOwner != workerID {
		return &contract.BusinessError{Code: contract.TaskConflict, Message: "command lease owned by another worker"}
	}
	if c.DispatchedAt == nil {
		d := dispatchedAt
		c.DispatchedAt = &d
	}
	if downstreamID != "" {
		c.DownstreamID = downstreamID
	}
	if c.Status == contract.PendingDispatch {
		c.Status = contract.Executing
	}
	c.UpdatedAt = dispatchedAt

	if task, exists := m.tasks[c.TaskID]; exists && task.Status == contract.PendingDispatch {
		task.Status = contract.Executing
		task.Version++
		task.UpdatedAt = dispatchedAt
	}
	return nil
}

func (m *MemoryStore) ReleaseCommandForRetry(_ context.Context, commandID, workerID string, retryAt time.Time, lastErr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.commands[commandID]
	if !ok {
		return &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	if c.Status == contract.Completed || c.Status == contract.Failed {
		return nil
	}
	if workerID != "" && c.LeaseOwner != "" && c.LeaseOwner != workerID {
		return &contract.BusinessError{Code: contract.TaskConflict, Message: "command lease owned by another worker"}
	}
	t := retryAt.UTC()
	c.LeaseOwner = ""
	c.LeaseUntil = &t
	c.LastError = lastErr
	c.UpdatedAt = m.now(time.Time{})
	return nil
}

func (m *MemoryStore) TransitionCommandOutcome(
	_ context.Context,
	commandID string,
	next contract.Status,
	reason string,
	occurredAt time.Time,
) (bool, *contract.VehicleResultEvent, error) {
	occurredAt = m.now(occurredAt)
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.commands[commandID]
	if !ok {
		return false, nil, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	if !canTransitionOutcome(c.Status, next) {
		return false, nil, nil
	}

	c.Status = next
	c.LeaseOwner = ""
	c.LeaseUntil = nil
	if reason != "" {
		c.LastError = reason
	}
	c.UpdatedAt = occurredAt

	if task, exists := m.tasks[c.TaskID]; exists {
		task.Status = next
		task.Version++
		task.UpdatedAt = occurredAt
	}

	evID := stableEventID(c.CommandID, next)
	ev := contract.VehicleResultEvent{
		Scope:      c.Scope,
		Operation:  c.Operation,
		EventID:    evID,
		RequestID:  c.RequestID,
		TaskID:     c.TaskID,
		CommandID:  c.CommandID,
		Status:     next,
		OccurredAt: occurredAt,
	}
	if _, dup := m.events[evID]; !dup {
		m.events[evID] = ev
	}
	return true, &ev, nil
}

func (m *MemoryStore) ListVehicleResults(_ context.Context, scope contract.Scope, since time.Time) ([]contract.VehicleResultEvent, error) {
	if !scope.Valid() {
		return nil, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	now := m.now(time.Time{})
	retentionFloor := now.Add(-contract.ResultRetention)
	cutoff := since.UTC()
	if cutoff.Before(retentionFloor) {
		cutoff = retentionFloor
	}

	m.mu.Lock()
	var out []contract.VehicleResultEvent
	for _, ev := range m.events {
		if ev.Scope != scope {
			continue
		}
		if ev.OccurredAt.Before(cutoff) {
			continue
		}
		out = append(out, ev)
	}
	m.mu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].EventID < out[j].EventID
		}
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
	return out, nil
}

var _ Store = (*MemoryStore)(nil)
