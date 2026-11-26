package tasks

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
)

//go:embed sqlite.sql
var SQLiteSchema string

// Fixed-width UTC nanosecond timestamp so SQLite TEXT comparisons match chronological order exactly.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

func formatSQLiteTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

func parseSQLiteTime(s string) (time.Time, error) {
	if t, err := time.Parse(sqliteTimeLayout, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t.UTC(), err
}

// EnsureSQLiteSchemaStatements splits SQLiteSchema into executable DDL statements.
func EnsureSQLiteSchemaStatements() []string {
	parts := strings.Split(SQLiteSchema, ";")
	var out []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		panic(fmt.Sprintf("empty sqlite.sql schema"))
	}
	return out
}

// OpenSQLite opens a WAL + BEGIN IMMEDIATE SQLite DB and initializes both
// vehicle/turns and vehicle/tasks tables in the same database file.
func OpenSQLite(path string) (*sql.DB, error) {
	db, err := turns.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	for _, stmt := range EnsureSQLiteSchemaStatements() {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
}

// SQLiteStore persists vehicle tasks, turn writes, command outbox, and result events
// in SQLite as a zero-dependency drop-in alternative to MySQLStore.
type SQLiteStore struct {
	DB        *sql.DB
	Validator ParamValidator
	Now       func() time.Time
}

func NewSQLiteStore(db *sql.DB, validator ParamValidator) *SQLiteStore {
	return &SQLiteStore{
		DB:        db,
		Validator: validator,
	}
}

func (s *SQLiteStore) now(override time.Time) time.Time {
	if !override.IsZero() {
		return override.UTC()
	}
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *SQLiteStore) lockTurnInTx(ctx context.Context, tx *sql.Tx, req TaskWriteRequest, now time.Time) (contract.TurnContext, error) {
	if req.TxLock != nil {
		return req.TxLock(ctx, tx, now)
	}
	if req.Metadata.TurnHandle != "" && req.Metadata.HermesSessionID != "" {
		return turns.LockWritableTurnSQLite(ctx, tx, req.Metadata, now)
	}
	if !req.Turn.Scope.Valid() || req.Turn.RequestID == "" {
		return contract.TurnContext{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid turn context"}
	}
	return req.Turn, nil
}

func (s *SQLiteStore) ListPending(ctx context.Context, scope contract.Scope, now time.Time) ([]contract.PendingCandidate, error) {
	if !scope.Valid() {
		return nil, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	nowStr := formatSQLiteTime(s.now(now))
	rows, err := s.DB.QueryContext(ctx, `
		SELECT task_id, device, tool, version, known_params_json, missing_fields_json, question, created_at, expires_at
		FROM vehicle_tasks
		WHERE scope_key = ? AND status = ? AND expires_at > ?
		ORDER BY created_at DESC, task_id DESC
		LIMIT ?`,
		scopeKey(scope), string(contract.PendingReply), nowStr, contract.PendingLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []contract.PendingCandidate
	for rows.Next() {
		var (
			taskID, device, tool, question string
			version                        uint64
			knownJSON, missingJSON         string
			createdStr, expiresStr         string
		)
		if err := rows.Scan(&taskID, &device, &tool, &version, &knownJSON, &missingJSON, &question, &createdStr, &expiresStr); err != nil {
			return nil, err
		}
		createdAt, err := parseSQLiteTime(createdStr)
		if err != nil {
			return nil, err
		}
		expiresAt, err := parseSQLiteTime(expiresStr)
		if err != nil {
			return nil, err
		}
		var known map[string]any
		_ = json.Unmarshal([]byte(knownJSON), &known)
		if known == nil {
			known = map[string]any{}
		}
		var missing []string
		_ = json.Unmarshal([]byte(missingJSON), &missing)

		op := contract.Operation{Device: device, Tool: tool}
		var schema json.RawMessage
		if s.Validator != nil {
			if sc, err := s.Validator.SchemaFor(scope.VehicleID, op); err == nil {
				schema = sc
			}
		}
		out = append(out, contract.PendingCandidate{
			Operation:     op,
			TaskID:        taskID,
			Version:       version,
			CreatedAt:     createdAt,
			ExpiresAt:     expiresAt,
			KnownParams:   known,
			MissingFields: missing,
			Question:      question,
			ParamsSchema:  schema,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortPendingCandidates(out)
	return out, nil
}

func (s *SQLiteStore) ReadTurnProgress(ctx context.Context, scope contract.Scope, requestID string) (contract.TaskProgress, error) {
	if !scope.Valid() || requestID == "" {
		return contract.TaskProgress{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope or request_id"}
	}
	var (
		taskID, status, question string
		commandID                sql.NullString
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT t.task_id, COALESCE(t.command_id, ''), t.status, t.question
		FROM vehicle_turn_writes w
		JOIN vehicle_tasks t ON t.task_id = w.task_id
		WHERE w.scope_key = ? AND w.request_id = ?`,
		scopeKey(scope), requestID,
	).Scan(&taskID, &commandID, &status, &question)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.TaskProgress{}, contract.ErrTurnHasNoWrite
	}
	if err != nil {
		return contract.TaskProgress{}, err
	}
	return contract.TaskProgress{
		TaskID:    taskID,
		CommandID: commandID.String,
		Status:    contract.Status(status),
		Question:  question,
	}, nil
}

func (s *SQLiteStore) lookupTurnWriteTx(ctx context.Context, tx *sql.Tx, sKey, requestID, fp string) (*contract.TaskResult, error) {
	var existingFP, resultJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT fingerprint, result_json
		FROM vehicle_turn_writes
		WHERE scope_key = ? AND request_id = ?`,
		sKey, requestID,
	).Scan(&existingFP, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if existingFP != fp {
		return nil, &contract.BusinessError{
			Code:    contract.IdempotencyConflict,
			Message: "同一轮次已提交另一项操作",
		}
	}
	var res contract.TaskResult
	if err := json.Unmarshal([]byte(resultJSON), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *SQLiteStore) insertTurnWriteTx(ctx context.Context, tx *sql.Tx, sKey, requestID, opKind, fp string, res contract.TaskResult, now time.Time) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vehicle_turn_writes
			(scope_key, request_id, operation_kind, fingerprint, task_id, command_id, result_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sKey, requestID, opKind, fp, res.TaskID, res.CommandID, string(raw), formatSQLiteTime(now),
	)
	if isDuplicateKeyErr(err) {
		return &contract.BusinessError{
			Code:    contract.IdempotencyConflict,
			Message: "同一轮次已提交另一项操作",
		}
	}
	return err
}

func (s *SQLiteStore) CreateOrUpdateTask(ctx context.Context, req TaskWriteRequest) (contract.TaskResult, error) {
	if s.Validator == nil {
		return contract.TaskResult{}, errors.New("tasks validator is required")
	}
	now := s.now(req.Now)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return contract.TaskResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	turn, err := s.lockTurnInTx(ctx, tx, req, now)
	if err != nil {
		return contract.TaskResult{}, err
	}
	sKey := scopeKey(turn.Scope)
	fp := requestFingerprint(OpKindCreateTask, req.Mutation)

	if replay, err := s.lookupTurnWriteTx(ctx, tx, sKey, turn.RequestID, fp); err != nil {
		return contract.TaskResult{}, err
	} else if replay != nil {
		if err := tx.Commit(); err != nil {
			return contract.TaskResult{}, err
		}
		return *replay, nil
	}

	var res contract.TaskResult
	if req.Mutation.TaskID != "" {
		res, err = s.partialUpdateTaskTx(ctx, tx, turn, sKey, req, now)
	} else {
		res, err = s.createNewPendingTaskTx(ctx, tx, turn, sKey, req, now)
	}
	if err != nil {
		return contract.TaskResult{}, err
	}

	if err := s.insertTurnWriteTx(ctx, tx, sKey, turn.RequestID, OpKindCreateTask, fp, res, now); err != nil {
		return contract.TaskResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return contract.TaskResult{}, err
	}
	return res, nil
}

func (s *SQLiteStore) createNewPendingTaskTx(ctx context.Context, tx *sql.Tx, turn contract.TurnContext, sKey string, req TaskWriteRequest, now time.Time) (contract.TaskResult, error) {
	val, err := s.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, req.Mutation.Params, false)
	if err != nil {
		return contract.TaskResult{}, err
	}
	if len(val.MissingFields) == 0 {
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "all required parameters are present; use dispatch_vehicle_command",
		}
	}
	taskID := newID("T")
	exp := now.Add(contract.PendingTTL)
	knownJSON, _ := json.Marshal(val.Normalized)
	missingJSON, _ := json.Marshal(val.MissingFields)
	nowStr := formatSQLiteTime(now)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO vehicle_tasks
			(task_id, scope_key, user_id, session_id, vehicle_id, device, tool, status, version,
			 known_params_json, missing_fields_json, question, command_id, last_request_id, created_at, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, NULL, ?, ?, ?, ?)`,
		taskID, sKey, turn.Scope.UserID, turn.Scope.SessionID, turn.Scope.VehicleID,
		req.Mutation.Device, req.Mutation.Tool, string(contract.PendingReply),
		string(knownJSON), string(missingJSON), val.Question, turn.RequestID, nowStr, formatSQLiteTime(exp), nowStr,
	)
	if err != nil {
		return contract.TaskResult{}, err
	}
	return contract.TaskResult{
		TaskID:        taskID,
		Status:        contract.PendingReply,
		MissingFields: cloneStrings(val.MissingFields),
		Question:      val.Question,
		ExpiresAt:     &exp,
	}, nil
}

func (s *SQLiteStore) partialUpdateTaskTx(ctx context.Context, tx *sql.Tx, turn contract.TurnContext, sKey string, req TaskWriteRequest, now time.Time) (contract.TaskResult, error) {
	var (
		device, tool, status string
		version              uint64
		knownJSON            string
		expiresStr           sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT device, tool, status, version, known_params_json, expires_at
		FROM vehicle_tasks
		WHERE task_id = ? AND scope_key = ?`,
		req.Mutation.TaskID, sKey,
	).Scan(&device, &tool, &status, &version, &knownJSON, &expiresStr)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "task not found in current scope"}
	}
	if err != nil {
		return contract.TaskResult{}, err
	}
	var expiresAt time.Time
	if expiresStr.Valid {
		expiresAt, err = parseSQLiteTime(expiresStr.String)
		if err != nil {
			return contract.TaskResult{}, err
		}
	}
	if contract.Status(status) == contract.Expired || (contract.Status(status) == contract.PendingReply && (!expiresStr.Valid || !expiresAt.After(now))) {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskExpired, Message: "待回复任务已到期"}
	}
	if contract.Status(status) != contract.PendingReply {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "task is no longer waiting for reply"}
	}
	if req.ExpectedVersion != 0 && version != req.ExpectedVersion {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskConflict, Message: "task version changed concurrently"}
	}
	if device != req.Mutation.Device || tool != req.Mutation.Tool {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskConflict, Message: "device or tool does not match existing task"}
	}
	var existing map[string]any
	_ = json.Unmarshal([]byte(knownJSON), &existing)
	merged, err := mergeParams(existing, req.Mutation.Params)
	if err != nil {
		return contract.TaskResult{}, err
	}
	val, err := s.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, merged, false)
	if err != nil {
		return contract.TaskResult{}, err
	}
	if len(val.MissingFields) == 0 {
		return contract.TaskResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: "all required parameters are present; use dispatch_vehicle_command",
		}
	}

	newKnown, _ := json.Marshal(val.Normalized)
	newMissing, _ := json.Marshal(val.MissingFields)
	nowStr := formatSQLiteTime(now)
	resExec, err := tx.ExecContext(ctx, `
		UPDATE vehicle_tasks
		SET known_params_json = ?, missing_fields_json = ?, question = ?, version = version + 1,
		    last_request_id = ?, updated_at = ?
		WHERE task_id = ? AND scope_key = ? AND status = ? AND version = ? AND expires_at > ?`,
		string(newKnown), string(newMissing), val.Question, turn.RequestID, nowStr,
		req.Mutation.TaskID, sKey, string(contract.PendingReply), version, nowStr,
	)
	if err != nil {
		return contract.TaskResult{}, err
	}
	affected, _ := resExec.RowsAffected()
	if affected != 1 {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskConflict, Message: "task updated or expired concurrently"}
	}
	return contract.TaskResult{
		TaskID:        req.Mutation.TaskID,
		Status:        contract.PendingReply,
		MissingFields: cloneStrings(val.MissingFields),
		Question:      val.Question,
		ExpiresAt:     &expiresAt,
	}, nil
}

func (s *SQLiteStore) DispatchCommand(ctx context.Context, req TaskWriteRequest) (contract.TaskResult, error) {
	if s.Validator == nil {
		return contract.TaskResult{}, errors.New("tasks validator is required")
	}
	now := s.now(req.Now)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return contract.TaskResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	turn, err := s.lockTurnInTx(ctx, tx, req, now)
	if err != nil {
		return contract.TaskResult{}, err
	}
	sKey := scopeKey(turn.Scope)
	fp := requestFingerprint(OpKindDispatchCommand, req.Mutation)

	if replay, err := s.lookupTurnWriteTx(ctx, tx, sKey, turn.RequestID, fp); err != nil {
		return contract.TaskResult{}, err
	} else if replay != nil {
		if err := tx.Commit(); err != nil {
			return contract.TaskResult{}, err
		}
		return *replay, nil
	}

	var res contract.TaskResult
	if req.Mutation.TaskID != "" {
		res, err = s.resumeAndDispatchTx(ctx, tx, turn, sKey, req, now)
	} else {
		res, err = s.createAndDispatchTx(ctx, tx, turn, sKey, req, now)
	}
	if err != nil {
		return contract.TaskResult{}, err
	}

	if err := s.insertTurnWriteTx(ctx, tx, sKey, turn.RequestID, OpKindDispatchCommand, fp, res, now); err != nil {
		return contract.TaskResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return contract.TaskResult{}, err
	}
	return res, nil
}

func (s *SQLiteStore) resumeAndDispatchTx(ctx context.Context, tx *sql.Tx, turn contract.TurnContext, sKey string, req TaskWriteRequest, now time.Time) (contract.TaskResult, error) {
	var (
		device, tool, status string
		version              uint64
		knownJSON            string
		expiresStr           sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT device, tool, status, version, known_params_json, expires_at
		FROM vehicle_tasks
		WHERE task_id = ? AND scope_key = ?`,
		req.Mutation.TaskID, sKey,
	).Scan(&device, &tool, &status, &version, &knownJSON, &expiresStr)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "task not found in current scope"}
	}
	if err != nil {
		return contract.TaskResult{}, err
	}
	var expiresAt time.Time
	if expiresStr.Valid {
		expiresAt, err = parseSQLiteTime(expiresStr.String)
		if err != nil {
			return contract.TaskResult{}, err
		}
	}
	if contract.Status(status) == contract.Expired || (contract.Status(status) == contract.PendingReply && (!expiresStr.Valid || !expiresAt.After(now))) {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskExpired, Message: "待回复任务已到期"}
	}
	if contract.Status(status) != contract.PendingReply {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "task is not eligible for dispatch"}
	}
	if req.ExpectedVersion != 0 && version != req.ExpectedVersion {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskConflict, Message: "task version mismatch"}
	}
	if device != req.Mutation.Device || tool != req.Mutation.Tool {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskConflict, Message: "device or tool does not match existing task"}
	}
	var existing map[string]any
	_ = json.Unmarshal([]byte(knownJSON), &existing)
	merged, err := mergeParams(existing, req.Mutation.Params)
	if err != nil {
		return contract.TaskResult{}, err
	}
	val, err := s.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, merged, true)
	if err != nil {
		return contract.TaskResult{}, err
	}

	cmdID := newID("C")
	paramsJSON, _ := json.Marshal(val.Normalized)
	nowStr := formatSQLiteTime(now)

	resExec, err := tx.ExecContext(ctx, `
		UPDATE vehicle_tasks
		SET status = ?, known_params_json = ?, missing_fields_json = '[]', command_id = ?,
		    version = version + 1, last_request_id = ?, updated_at = ?
		WHERE task_id = ? AND scope_key = ? AND status = ? AND version = ? AND expires_at > ?`,
		string(contract.PendingDispatch), string(paramsJSON), cmdID, turn.RequestID, nowStr,
		req.Mutation.TaskID, sKey, string(contract.PendingReply), version, nowStr,
	)
	if err != nil {
		return contract.TaskResult{}, err
	}
	affected, _ := resExec.RowsAffected()
	if affected != 1 {
		return contract.TaskResult{}, &contract.BusinessError{Code: contract.TaskConflict, Message: "task already resumed or expired"}
	}

	if err := s.insertOutboxCommandTx(ctx, tx, cmdID, req.Mutation.TaskID, sKey, turn, req.Mutation.Operation, string(paramsJSON), nowStr); err != nil {
		return contract.TaskResult{}, err
	}
	return contract.TaskResult{
		TaskID:    req.Mutation.TaskID,
		CommandID: cmdID,
		Status:    contract.PendingDispatch,
	}, nil
}

func (s *SQLiteStore) createAndDispatchTx(ctx context.Context, tx *sql.Tx, turn contract.TurnContext, sKey string, req TaskWriteRequest, now time.Time) (contract.TaskResult, error) {
	val, err := s.Validator.ValidateTaskParams(turn.Scope.VehicleID, req.Mutation.Operation, req.Mutation.Params, true)
	if err != nil {
		return contract.TaskResult{}, err
	}
	taskID := newID("T")
	cmdID := newID("C")
	paramsJSON, _ := json.Marshal(val.Normalized)
	nowStr := formatSQLiteTime(now)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO vehicle_tasks
			(task_id, scope_key, user_id, session_id, vehicle_id, device, tool, status, version,
			 known_params_json, missing_fields_json, question, command_id, last_request_id, created_at, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, '[]', '', ?, ?, ?, NULL, ?)`,
		taskID, sKey, turn.Scope.UserID, turn.Scope.SessionID, turn.Scope.VehicleID,
		req.Mutation.Device, req.Mutation.Tool, string(contract.PendingDispatch),
		string(paramsJSON), cmdID, turn.RequestID, nowStr, nowStr,
	)
	if err != nil {
		return contract.TaskResult{}, err
	}
	if err := s.insertOutboxCommandTx(ctx, tx, cmdID, taskID, sKey, turn, req.Mutation.Operation, string(paramsJSON), nowStr); err != nil {
		return contract.TaskResult{}, err
	}
	return contract.TaskResult{
		TaskID:    taskID,
		CommandID: cmdID,
		Status:    contract.PendingDispatch,
	}, nil
}

func (s *SQLiteStore) insertOutboxCommandTx(ctx context.Context, tx *sql.Tx, cmdID, taskID, sKey string, turn contract.TurnContext, op contract.Operation, paramsJSON, nowStr string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO vehicle_command_outbox
			(command_id, task_id, scope_key, user_id, session_id, vehicle_id, request_id,
			 device, tool, params_json, status, attempts, downstream_id, lease_owner, lease_until,
			 dispatched_at, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', NULL, NULL, '', ?, ?)`,
		cmdID, taskID, sKey, turn.Scope.UserID, turn.Scope.SessionID, turn.Scope.VehicleID, turn.RequestID,
		op.Device, op.Tool, paramsJSON, string(contract.PendingDispatch), nowStr, nowStr,
	)
	if isDuplicateKeyErr(err) {
		return &contract.BusinessError{Code: contract.TaskConflict, Message: "command outbox already exists for task"}
	}
	return err
}

func (s *SQLiteStore) ListTasks(ctx context.Context, scope contract.Scope, input contract.ListTasksInput, now time.Time) (contract.ListTasksResult, error) {
	if !scope.Valid() {
		return contract.ListTasksResult{}, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	now = s.now(now)
	offset := 0
	if input.Cursor != "" {
		parsed, err := strconv.Atoi(input.Cursor)
		if err != nil || parsed < 0 {
			return contract.ListTasksResult{}, &contract.BusinessError{Code: contract.InvalidArgument, Message: "invalid cursor"}
		}
		offset = parsed
	}

	allowed := defaultIncompleteStatuses()
	if len(input.Status) > 0 {
		allowed = make(map[contract.Status]bool, len(input.Status))
		for _, st := range input.Status {
			allowed[st] = true
		}
	}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT task_id, COALESCE(command_id, ''), status, missing_fields_json, question, expires_at
		FROM vehicle_tasks
		WHERE scope_key = ?
		ORDER BY created_at DESC, task_id DESC
		LIMIT ?`,
		scopeKey(scope), maxListLimit,
	)
	if err != nil {
		return contract.ListTasksResult{}, err
	}
	defer rows.Close()

	var filtered []contract.TaskResult
	for rows.Next() {
		var (
			taskID, cmdID, rawStatus, missingJSON, question string
			expiresStr                                      sql.NullString
		)
		if err := rows.Scan(&taskID, &cmdID, &rawStatus, &missingJSON, &question, &expiresStr); err != nil {
			return contract.ListTasksResult{}, err
		}
		var expPtr *time.Time
		if expiresStr.Valid {
			if t, err := parseSQLiteTime(expiresStr.String); err == nil {
				expPtr = &t
			}
		}
		st := contract.Status(rawStatus)
		if st == contract.PendingReply && (expPtr == nil || !expPtr.After(now)) {
			st = contract.Expired
		}
		if !allowed[st] {
			continue
		}
		var missing []string
		_ = json.Unmarshal([]byte(missingJSON), &missing)
		filtered = append(filtered, contract.TaskResult{
			TaskID:        taskID,
			CommandID:     cmdID,
			Status:        st,
			MissingFields: missing,
			Question:      question,
			ExpiresAt:     expPtr,
		})
	}
	if err := rows.Err(); err != nil {
		return contract.ListTasksResult{}, err
	}
	if offset >= len(filtered) {
		return contract.ListTasksResult{Tasks: []contract.TaskResult{}}, nil
	}
	end := min(offset+defaultListLimit, len(filtered))
	nextCursor := ""
	if end < len(filtered) {
		nextCursor = strconv.Itoa(end)
	}
	return contract.ListTasksResult{Tasks: filtered[offset:end], NextCursor: nextCursor}, nil
}

func (s *SQLiteStore) GetTask(ctx context.Context, scope contract.Scope, taskID string) (TaskRecord, error) {
	var (
		rec                                          TaskRecord
		status, knownJSON, missingJSON               string
		cmdID, expiresStr                            sql.NullString
		createdStr, updatedStr                       string
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT task_id, user_id, session_id, vehicle_id, device, tool, status, version,
		       known_params_json, missing_fields_json, question, command_id, last_request_id,
		       created_at, expires_at, updated_at
		FROM vehicle_tasks
		WHERE task_id = ? AND scope_key = ?`,
		taskID, scopeKey(scope),
	).Scan(
		&rec.TaskID, &rec.Scope.UserID, &rec.Scope.SessionID, &rec.Scope.VehicleID,
		&rec.Operation.Device, &rec.Operation.Tool, &status, &rec.Version,
		&knownJSON, &missingJSON, &rec.Question, &cmdID, &rec.LastRequestID,
		&createdStr, &expiresStr, &updatedStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRecord{}, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "task not found"}
	}
	if err != nil {
		return TaskRecord{}, err
	}
	rec.Status = contract.Status(status)
	rec.CommandID = cmdID.String
	_ = json.Unmarshal([]byte(knownJSON), &rec.KnownParams)
	_ = json.Unmarshal([]byte(missingJSON), &rec.MissingFields)
	rec.CreatedAt, _ = parseSQLiteTime(createdStr)
	rec.UpdatedAt, _ = parseSQLiteTime(updatedStr)
	if expiresStr.Valid {
		if t, err := parseSQLiteTime(expiresStr.String); err == nil {
			rec.ExpiresAt = &t
		}
	}
	return rec, nil
}

func (s *SQLiteStore) ExpirePending(ctx context.Context, now time.Time) (int64, error) {
	nowStr := formatSQLiteTime(s.now(now))
	res, err := s.DB.ExecContext(ctx, `
		UPDATE vehicle_tasks
		SET status = ?, version = version + 1, updated_at = ?
		WHERE status = ? AND expires_at <= ?`,
		string(contract.Expired), nowStr, string(contract.PendingReply), nowStr,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) ClaimNextCommand(ctx context.Context, workerID string, leaseTTL time.Duration, now time.Time) (*OutboxCommand, error) {
	now = s.now(now)
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}
	nowStr := formatSQLiteTime(now)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		cmd                                  OutboxCommand
		status, paramsJSON                   string
		leaseStr, dispatchedStr              sql.NullString
		createdStr, updatedStr               string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT command_id, task_id, user_id, session_id, vehicle_id, request_id,
		       device, tool, params_json, status, attempts, downstream_id,
		       lease_owner, lease_until, dispatched_at, last_error, created_at, updated_at
		FROM vehicle_command_outbox
		WHERE (status = ? AND (lease_until IS NULL OR lease_until <= ?))
		   OR (status = ? AND lease_until IS NOT NULL AND lease_until <= ?)
		ORDER BY created_at ASC, command_id ASC
		LIMIT 1`,
		string(contract.PendingDispatch), nowStr, string(contract.Executing), nowStr,
	).Scan(
		&cmd.CommandID, &cmd.TaskID, &cmd.Scope.UserID, &cmd.Scope.SessionID, &cmd.Scope.VehicleID,
		&cmd.RequestID, &cmd.Operation.Device, &cmd.Operation.Tool, &paramsJSON, &status,
		&cmd.Attempts, &cmd.DownstreamID, &cmd.LeaseOwner, &leaseStr, &dispatchedStr,
		&cmd.LastError, &createdStr, &updatedStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	until := now.Add(leaseTTL)
	_, err = tx.ExecContext(ctx, `
		UPDATE vehicle_command_outbox
		SET lease_owner = ?, lease_until = ?, attempts = attempts + 1, updated_at = ?
		WHERE command_id = ?`,
		workerID, formatSQLiteTime(until), nowStr, cmd.CommandID,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	cmd.Status = contract.Status(status)
	_ = json.Unmarshal([]byte(paramsJSON), &cmd.Params)
	cmd.Attempts++
	cmd.LeaseOwner = workerID
	cmd.LeaseUntil = &until
	cmd.CreatedAt, _ = parseSQLiteTime(createdStr)
	cmd.UpdatedAt = now
	if dispatchedStr.Valid {
		if t, err := parseSQLiteTime(dispatchedStr.String); err == nil {
			cmd.DispatchedAt = &t
		}
	}
	return &cmd, nil
}

func (s *SQLiteStore) GetCommand(ctx context.Context, commandID string) (*OutboxCommand, error) {
	var (
		cmd                    OutboxCommand
		status, paramsJSON     string
		leaseStr, dispatchedStr sql.NullString
		createdStr, updatedStr string
	)
	err := s.DB.QueryRowContext(ctx, `
		SELECT command_id, task_id, user_id, session_id, vehicle_id, request_id,
		       device, tool, params_json, status, attempts, downstream_id,
		       lease_owner, lease_until, dispatched_at, last_error, created_at, updated_at
		FROM vehicle_command_outbox
		WHERE command_id = ?`,
		commandID,
	).Scan(
		&cmd.CommandID, &cmd.TaskID, &cmd.Scope.UserID, &cmd.Scope.SessionID, &cmd.Scope.VehicleID,
		&cmd.RequestID, &cmd.Operation.Device, &cmd.Operation.Tool, &paramsJSON, &status,
		&cmd.Attempts, &cmd.DownstreamID, &cmd.LeaseOwner, &leaseStr, &dispatchedStr,
		&cmd.LastError, &createdStr, &updatedStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	if err != nil {
		return nil, err
	}
	cmd.Status = contract.Status(status)
	_ = json.Unmarshal([]byte(paramsJSON), &cmd.Params)
	cmd.CreatedAt, _ = parseSQLiteTime(createdStr)
	cmd.UpdatedAt, _ = parseSQLiteTime(updatedStr)
	if leaseStr.Valid {
		if t, err := parseSQLiteTime(leaseStr.String); err == nil {
			cmd.LeaseUntil = &t
		}
	}
	if dispatchedStr.Valid {
		if t, err := parseSQLiteTime(dispatchedStr.String); err == nil {
			cmd.DispatchedAt = &t
		}
	}
	return &cmd, nil
}

func (s *SQLiteStore) BeginCommandSend(ctx context.Context, commandID, workerID string, attempt uint32, now time.Time) (bool, error) {
	at := formatSQLiteTime(s.now(now))
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE vehicle_command_outbox
		SET status = ?, dispatched_at = ?, updated_at = ?
		WHERE command_id = ? AND status = ? AND lease_owner = ? AND attempts = ? AND lease_until > ?`,
		string(contract.Executing), at, at, commandID, string(contract.PendingDispatch), workerID, attempt, at)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE vehicle_tasks SET status = ?, version = version + 1, updated_at = ?
		WHERE command_id = ? AND status = ?`,
		string(contract.Executing), at, commandID, string(contract.PendingDispatch)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) MarkCommandExecuting(ctx context.Context, commandID, workerID string, dispatchedAt time.Time, downstreamID string) error {
	dispatchedStr := formatSQLiteTime(s.now(dispatchedAt))
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var taskID, curStatus, owner string
	err = tx.QueryRowContext(ctx, `
		SELECT task_id, status, lease_owner
		FROM vehicle_command_outbox
		WHERE command_id = ?`,
		commandID,
	).Scan(&taskID, &curStatus, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	if err != nil {
		return err
	}
	st := contract.Status(curStatus)
	if st == contract.Completed || st == contract.Failed {
		return tx.Commit()
	}
	if workerID != "" && owner != "" && owner != workerID {
		return &contract.BusinessError{Code: contract.TaskConflict, Message: "command lease owned by another worker"}
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE vehicle_command_outbox
		SET status = CASE WHEN status = ? THEN ? ELSE status END,
		    dispatched_at = COALESCE(dispatched_at, ?),
		    downstream_id = CASE WHEN ? <> '' THEN ? ELSE downstream_id END,
		    updated_at = ?
		WHERE command_id = ?`,
		string(contract.PendingDispatch), string(contract.Executing),
		dispatchedStr, downstreamID, downstreamID, dispatchedStr, commandID,
	)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE vehicle_tasks
		SET status = ?, version = version + 1, updated_at = ?
		WHERE task_id = ? AND status = ?`,
		string(contract.Executing), dispatchedStr, taskID, string(contract.PendingDispatch),
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ReleaseCommandForRetry(ctx context.Context, commandID, workerID string, retryAt time.Time, lastErr string) error {
	nowStr := formatSQLiteTime(s.now(time.Time{}))
	_, err := s.DB.ExecContext(ctx, `
		UPDATE vehicle_command_outbox
		SET status = ?, dispatched_at = NULL, downstream_id = '', lease_owner = '', lease_until = ?, last_error = ?, updated_at = ?
		WHERE command_id = ? AND lease_owner = ? AND status IN (?, ?)`,
		string(contract.PendingDispatch), formatSQLiteTime(retryAt), lastErr, nowStr, commandID, workerID, string(contract.Executing), string(contract.PendingDispatch),
	)
	return err
}

func (s *SQLiteStore) TransitionCommandOutcome(
	ctx context.Context,
	commandID string,
	next contract.Status,
	reason string,
	occurredAt time.Time,
) (bool, *contract.VehicleResultEvent, error) {
	occurredAt = s.now(occurredAt)
	occurredStr := formatSQLiteTime(occurredAt)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var taskID, sKey, userID, sessionID, vehicleID, requestID, device, tool, curStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT task_id, scope_key, user_id, session_id, vehicle_id, request_id, device, tool, status
		FROM vehicle_command_outbox
		WHERE command_id = ?`,
		commandID,
	).Scan(&taskID, &sKey, &userID, &sessionID, &vehicleID, &requestID, &device, &tool, &curStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, &contract.BusinessError{Code: contract.TaskNotEligible, Message: "command not found"}
	}
	if err != nil {
		return false, nil, err
	}
	if !canTransitionOutcome(contract.Status(curStatus), next) {
		return false, nil, tx.Commit()
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE vehicle_command_outbox
		SET status = ?, lease_owner = '', lease_until = NULL,
		    last_error = CASE WHEN ? <> '' THEN ? ELSE last_error END,
		    updated_at = ?
		WHERE command_id = ?`,
		string(next), reason, reason, occurredStr, commandID,
	)
	if err != nil {
		return false, nil, err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE vehicle_tasks
		SET status = ?, version = version + 1, updated_at = ?
		WHERE task_id = ?`,
		string(next), occurredStr, taskID,
	)
	if err != nil {
		return false, nil, err
	}

	ev := contract.VehicleResultEvent{
		Scope:      contract.Scope{UserID: userID, SessionID: sessionID, VehicleID: vehicleID},
		Operation:  contract.Operation{Device: device, Tool: tool},
		EventID:    stableEventID(commandID, next),
		RequestID:  requestID,
		TaskID:     taskID,
		CommandID:  commandID,
		Status:     next,
		OccurredAt: occurredAt,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO vehicle_result_events
			(event_id, scope_key, user_id, session_id, vehicle_id, device, tool, request_id, task_id, command_id, status, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.EventID, sKey, userID, sessionID, vehicleID, device, tool, requestID, taskID, commandID, string(next), occurredStr,
	)
	if err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, &ev, nil
}

func (s *SQLiteStore) ListVehicleResults(ctx context.Context, scope contract.Scope, since time.Time) ([]contract.VehicleResultEvent, error) {
	if !scope.Valid() {
		return nil, &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "invalid scope"}
	}
	now := s.now(time.Time{})
	cutoff := since.UTC()
	if floor := now.Add(-contract.ResultRetention); cutoff.Before(floor) {
		cutoff = floor
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT event_id, user_id, session_id, vehicle_id, device, tool, request_id, task_id, command_id, status, occurred_at
		FROM vehicle_result_events
		WHERE scope_key = ? AND occurred_at >= ?
		ORDER BY occurred_at ASC, event_id ASC`,
		scopeKey(scope), formatSQLiteTime(cutoff),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []contract.VehicleResultEvent
	for rows.Next() {
		var (
			ev                  contract.VehicleResultEvent
			status, occurredStr string
		)
		if err := rows.Scan(
			&ev.EventID, &ev.Scope.UserID, &ev.Scope.SessionID, &ev.Scope.VehicleID,
			&ev.Operation.Device, &ev.Operation.Tool, &ev.RequestID, &ev.TaskID, &ev.CommandID,
			&status, &occurredStr,
		); err != nil {
			return nil, err
		}
		ev.Status = contract.Status(status)
		ev.OccurredAt, _ = parseSQLiteTime(occurredStr)
		out = append(out, ev)
	}
	return out, rows.Err()
}

var _ Store = (*SQLiteStore)(nil)
