package turns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
)

type Record struct {
	Context            contract.TurnContext
	Payload            runs.StartRequest
	IdempotencyKey     string
	AdmissionAttempted bool
	RunID              string
	Reply              *contract.SpeechReply
	Closed             bool
	Cancelled          bool
}

var ErrNotFound = errors.New("vehicle turn not found")

type Store interface {
	contract.TurnRegistry
	LockScope(ctx context.Context, scope contract.Scope) (func(), error)
	Get(ctx context.Context, scope contract.Scope, voiceTurnID string) (Record, error)
	Register(ctx context.Context, record Record) error
	Save(ctx context.Context, record Record) error
	Cancel(ctx context.Context, scope contract.Scope, voiceTurnID string) error
}

func conflict() error {
	return &contract.BusinessError{Code: contract.IdempotencyConflict, Message: "voice turn or request ID already registered"}
}

func invalidTurn() error {
	return &contract.BusinessError{Code: contract.TurnContextInvalid, Message: "turn unavailable"}
}

func resolve(record Record, metadata contract.TurnMetadata, now time.Time) (contract.TurnContext, error) {
	if record.Closed || record.Cancelled || metadata.TurnHandle == "" || metadata.HermesSessionID == "" ||
		record.Context.TurnHandle != metadata.TurnHandle || record.Context.HermesSessionID != metadata.HermesSessionID ||
		!record.Context.ExpiresAt.After(now) || !record.Context.Scope.Valid() {
		return contract.TurnContext{}, invalidTurn()
	}
	return record.Context, nil
}

func stableID(parts ...string) string {
	payload, _ := json.Marshal(parts)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func scopeKey(scope contract.Scope) string {
	return stableID(scope.UserID, scope.SessionID, scope.VehicleID)
}
