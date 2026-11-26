// Package contract contains the shared boundaries for the voice and vehicle services.
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	PendingLimit    = 3
	PendingTTL      = 10 * time.Minute
	ResultRetention = 24 * time.Hour

	TurnHandleHeader    = "X-Vehicle-Turn-Handle"
	HermesSessionHeader = "X-Vehicle-Hermes-Session-ID"
)

type Scope struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	VehicleID string `json:"vehicle_id"`
}

func (s Scope) Valid() bool {
	return s.UserID != "" && s.SessionID != "" && s.VehicleID != ""
}

// Authenticator resolves a selected vehicle and binds it to the authenticated principal and session.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer, sessionID, vehicleID string) (Scope, error)
}

type Status string

const (
	PendingReply    Status = "pending_reply"
	PendingDispatch Status = "pending_dispatch"
	Executing       Status = "executing"
	Completed       Status = "completed"
	Expired         Status = "expired"
	Failed          Status = "failed"
	ResultUnknown   Status = "result_unknown"
)

func (s Status) IsVehicleResult() bool {
	return s == Completed || s == Failed || s == ResultUnknown
}

type ErrorCode string

const (
	TurnContextInvalid    ErrorCode = "TURN_CONTEXT_INVALID"
	ClarificationRequired ErrorCode = "CLARIFICATION_REQUIRED"
	TaskNotEligible       ErrorCode = "TASK_NOT_ELIGIBLE"
	TaskExpired           ErrorCode = "TASK_EXPIRED"
	TaskConflict          ErrorCode = "TASK_CONFLICT"
	InvalidArgument       ErrorCode = "INVALID_ARGUMENT"
	CapabilityUnavailable ErrorCode = "CAPABILITY_UNAVAILABLE"
	IdempotencyConflict   ErrorCode = "IDEMPOTENCY_CONFLICT"
)

type BusinessError struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
}

func (e *BusinessError) Error() string { return string(e.Code) + ": " + e.Message }

type Operation struct {
	Device string `json:"device"`
	Tool   string `json:"tool"`
}

type Capability struct {
	Operation
	MCPTool      string          `json:"mcp_tool"`
	ParamsSchema json.RawMessage `json:"params_schema"`
}

type PendingCandidate struct {
	Operation
	TaskID        string          `json:"task_id"`
	Version       uint64          `json:"version"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	KnownParams   map[string]any  `json:"known_params"`
	MissingFields []string        `json:"missing_fields"`
	Question      string          `json:"question"`
	ParamsSchema  json.RawMessage `json:"params_schema"`
}

// PendingReader returns at most PendingLimit candidates, strictly unexpired at now,
// from scope, ordered by created_at DESC, task_id DESC.
type PendingReader interface {
	ListPending(ctx context.Context, scope Scope, now time.Time) ([]PendingCandidate, error)
}

type CapabilityFinder interface {
	FindCapabilities(ctx context.Context, scope Scope, text string) ([]Capability, error)
}

type FinalTranscript struct {
	ProviderSessionID string `json:"provider_session_id"`
	InputItemID       string `json:"input_item_id"`
	Text              string `json:"text"`
}

type VehicleTurn struct {
	Scope           Scope           `json:"scope"`
	VoiceTurnID     string          `json:"voice_turn_id"`
	RequestID       string          `json:"request_id"`
	FinalTranscript FinalTranscript `json:"final_transcript"`
}

type CandidateVersion struct {
	TaskID  string `json:"task_id"`
	Version uint64 `json:"version"`
}

// TurnContext is a server-side record. Only TurnMetadata travels via authenticated transport.
type TurnContext struct {
	VehicleTurn
	HermesSessionID string             `json:"hermes_session_id"`
	TurnHandle      string             `json:"turn_handle"`
	Candidates      []CandidateVersion `json:"candidates"`
	ExpiresAt       time.Time          `json:"expires_at"`
}

type TurnMetadata struct {
	TurnHandle      string
	HermesSessionID string
}

type SpeechReply struct {
	VoiceTurnID string `json:"voice_turn_id"`
	RequestID   string `json:"request_id"`
	RunID       string `json:"run_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	CommandID   string `json:"command_id,omitempty"`
	Status      Status `json:"status,omitempty"`
	Text        string `json:"text"`
}

type TurnHandler interface {
	HandleVehicleTurn(ctx context.Context, turn VehicleTurn) (SpeechReply, error)
	CancelVehicleTurn(ctx context.Context, scope Scope, voiceTurnID string) error
}

// TaskProgress is read from Go's task store for the current turn before authorizing speech.
type TaskProgress struct {
	TaskID    string `json:"task_id"`
	CommandID string `json:"command_id,omitempty"`
	Status    Status `json:"status"`
	Question  string `json:"question,omitempty"`
}

// ErrTurnHasNoWrite indicates that a turn currently has no persisted task write.
var ErrTurnHasNoWrite = errors.New("turn has no persisted write")

type ProgressReader interface {
	ReadTurnProgress(ctx context.Context, scope Scope, requestID string) (TaskProgress, error)
}

type VehicleCommand struct {
	Scope Scope `json:"scope"`
	Operation
	CommandID string         `json:"command_id"`
	TaskID    string         `json:"task_id"`
	RequestID string         `json:"request_id"`
	Params    map[string]any `json:"params"`
}

type VehicleResultEvent struct {
	Scope Scope `json:"scope"`
	Operation
	EventID    string    `json:"event_id"`
	RequestID  string    `json:"request_id"`
	TaskID     string    `json:"task_id"`
	CommandID  string    `json:"command_id"`
	Status     Status    `json:"status"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ResultReader lists retained results in an authenticated scope. Results may be
// delivered again on every read; consumers deduplicate by EventID.
type ResultReader interface {
	ListVehicleResults(ctx context.Context, scope Scope, since time.Time) ([]VehicleResultEvent, error)
}

// TurnRegistry is the trusted turn lookup used by MCP after authenticating the caller.
// Any mutation rechecks the handle, the session binding, expiry and cancellation
// inside its write transaction, along with the one-write-per-turn constraint.
type TurnRegistry interface {
	ResolveTurn(ctx context.Context, metadata TurnMetadata, now time.Time) (TurnContext, error)
}
