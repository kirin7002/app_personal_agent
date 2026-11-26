// Package tasks implements the vehicle task domain, 10-minute reply expiry,
// per-turn write idempotency, command outbox, and monotonic result outbox (H03).
package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

const (
	OpKindCreateTask      = "create_vehicle_task"
	OpKindDispatchCommand = "dispatch_vehicle_command"

	defaultListLimit = 20
	maxListLimit     = 100
)

// SchemaProvider resolves the JSON Schema for an operation on a target vehicle.
// Implemented by vehicle/catalog.Service.
type SchemaProvider interface {
	SchemaFor(vehicleID string, op contract.Operation) (json.RawMessage, error)
}

// ParamValidator validates merged parameters against the capability map.
// Implemented by vehicle/catalog.Service via an adapter or directly.
type ValidationResult struct {
	Normalized    map[string]any
	MissingFields []string
	Question      string
}

type ParamValidator interface {
	SchemaProvider
	ValidateTaskParams(vehicleID string, op contract.Operation, params map[string]any, requireComplete bool) (ValidationResult, error)
}

// TxTurnLock is invoked inside the MySQL write transaction (typically wrapping
// vehicle/turns.LockWritableTurn) to serialize writes against CancelVehicleTurn.
type TxTurnLock func(ctx context.Context, tx *sql.Tx, now time.Time) (contract.TurnContext, error)

// MemTurnLock is invoked inside MemoryStore's critical section to enforce the same
// turn validity and cancellation fence in single-process simulated deployments.
type MemTurnLock func(ctx context.Context, now time.Time) (contract.TurnContext, error)

// TaskWriteRequest carries a validated write mutation scoped to a trusted turn.
type TaskWriteRequest struct {
	Turn            contract.TurnContext
	Metadata        contract.TurnMetadata
	Mutation        contract.TaskMutation
	ExpectedVersion uint64 // required when Mutation.TaskID != ""
	Now             time.Time

	// Optional transaction-level turn locks. When MySQLStore executes a write,
	// if TxLock is non-nil it runs inside the write transaction before any mutation.
	TxLock  TxTurnLock
	MemLock MemTurnLock
}

// TaskRecord is the full persistent representation of a vehicle task.
type TaskRecord struct {
	TaskID        string
	Scope         contract.Scope
	Operation     contract.Operation
	Status        contract.Status
	Version       uint64
	KnownParams   map[string]any
	MissingFields []string
	Question      string
	CommandID     string
	LastRequestID string
	CreatedAt     time.Time
	ExpiresAt     *time.Time
	UpdatedAt     time.Time
}

// OutboxCommand is the persistent command record claimed and executed by H05.
type OutboxCommand struct {
	contract.VehicleCommand
	Status       contract.Status `json:"status"`
	Attempts     uint32          `json:"attempts"`
	DownstreamID string          `json:"downstream_id,omitempty"`
	LeaseOwner   string          `json:"lease_owner,omitempty"`
	LeaseUntil   *time.Time      `json:"lease_until,omitempty"`
	DispatchedAt *time.Time      `json:"dispatched_at,omitempty"`
	LastError    string          `json:"last_error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Store defines the H03 task persistence surface shared by H02, H04, and H05.
type Store interface {
	contract.PendingReader
	contract.ProgressReader
	contract.ResultReader

	// CreateOrUpdateTask creates a new pending_reply task (TaskID=="") or applies
	// partial parameter completion to an existing unexpired pending_reply task
	// (TaskID!=""), preserving the original expires_at.
	CreateOrUpdateTask(ctx context.Context, req TaskWriteRequest) (contract.TaskResult, error)

	// DispatchCommand creates a new pending_dispatch task or atomically transitions
	// an eligible unexpired pending_reply task to pending_dispatch, registering a
	// unique command_id in the outbox within the same transaction.
	DispatchCommand(ctx context.Context, req TaskWriteRequest) (contract.TaskResult, error)

	// ListTasks serves the list_vehicle_tasks MCP tool within scope.
	ListTasks(ctx context.Context, scope contract.Scope, input contract.ListTasksInput, now time.Time) (contract.ListTasksResult, error)

	// GetTask fetches a single task by ID (primarily for verification and H04 checks).
	GetTask(ctx context.Context, scope contract.Scope, taskID string) (TaskRecord, error)

	// ExpirePending transitions all pending_reply tasks with expires_at <= now to expired.
	ExpirePending(ctx context.Context, now time.Time) (int64, error)

	// Outbox & H05 execution lifecycle methods.
	ClaimNextCommand(ctx context.Context, workerID string, leaseTTL time.Duration, now time.Time) (*OutboxCommand, error)
	GetCommand(ctx context.Context, commandID string) (*OutboxCommand, error)
	// BeginCommandSend durably fences the current lease before transport sees the command.
	BeginCommandSend(ctx context.Context, commandID, workerID string, attempt uint32, now time.Time) (bool, error)
	MarkCommandExecuting(ctx context.Context, commandID, workerID string, dispatchedAt time.Time, downstreamID string) error
	ReleaseCommandForRetry(ctx context.Context, commandID, workerID string, retryAt time.Time, lastErr string) error
	TransitionCommandOutcome(ctx context.Context, commandID string, next contract.Status, reason string, occurredAt time.Time) (bool, *contract.VehicleResultEvent, error)
}

func scopeKey(scope contract.Scope) string {
	payload, _ := json.Marshal([]string{scope.UserID, scope.SessionID, scope.VehicleID})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + strings.ToUpper(hex.EncodeToString(b[:]))
}

// requestFingerprint builds a canonical SHA-256 hash of a turn write mutation so
// exact replays return the original result while changed mutations raise IDEMPOTENCY_CONFLICT.
func requestFingerprint(kind string, mut contract.TaskMutation) string {
	normalized := mut.Params
	if normalized == nil {
		normalized = map[string]any{}
	}
	payload, _ := json.Marshal(struct {
		Kind   string         `json:"kind"`
		Device string         `json:"device"`
		Tool   string         `json:"tool"`
		TaskID string         `json:"task_id"`
		Params map[string]any `json:"params"`
	}{
		Kind:   kind,
		Device: mut.Device,
		Tool:   mut.Tool,
		TaskID: mut.TaskID,
		Params: normalized,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// mergeParams combines existing known_params with incoming params and rejects any
// attempt to overwrite an already-known parameter with a conflicting value.
func mergeParams(existing, incoming map[string]any) (map[string]any, error) {
	merged := make(map[string]any, len(existing)+len(incoming))
	maps.Copy(merged, existing)
	for k, v := range incoming {
		if v == nil {
			continue
		}
		if prev, exists := merged[k]; exists && prev != nil {
			if !valuesEqual(prev, v) {
				return nil, &contract.BusinessError{
					Code:    contract.TaskConflict,
					Message: fmt.Sprintf("parameter %q conflicts with already known value (%v != %v)", k, prev, v),
				}
			}
		}
		merged[k] = v
	}
	return merged, nil
}

func valuesEqual(a, b any) bool {
	af, aok := numFloat(a)
	bf, bok := numFloat(b)
	if aok && bok {
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

func numFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func stableEventID(commandID string, status contract.Status) string {
	return commandID + ":" + string(status)
}

// canTransitionOutcome enforces monotonic status transitions for H05 execution results.
func canTransitionOutcome(current, next contract.Status) bool {
	if !next.IsVehicleResult() {
		return false
	}
	switch current {
	case contract.PendingDispatch:
		// Pre-dispatch permanent rejection (e.g., capability unavailable at execution time).
		return next == contract.Failed
	case contract.Executing:
		return next == contract.Completed || next == contract.Failed || next == contract.ResultUnknown
	case contract.ResultUnknown:
		// Reconciliation window can promote result_unknown to a final verdict.
		return next == contract.Completed || next == contract.Failed
	case contract.Completed, contract.Failed:
		// Terminal states never regress or re-apply.
		return false
	default:
		return false
	}
}

func sortPendingCandidates(candidates []contract.PendingCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].TaskID > candidates[j].TaskID
		}
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
}

func defaultIncompleteStatuses() map[contract.Status]bool {
	return map[contract.Status]bool{
		contract.PendingReply:    true,
		contract.PendingDispatch: true,
		contract.Executing:       true,
		contract.ResultUnknown:   true,
	}
}

func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") || strings.Contains(msg, "unique constraint") || strings.Contains(msg, "1062")
}
