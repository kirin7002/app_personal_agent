// Package executor implements H05 asynchronous command execution, downstream
// vehicle adapters, receipt handling, telemetry reconciliation, and result outbox publishing.
package executor

import (
	"context"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

// ChannelCapabilities declares the downstream vehicle transport's reliability contract.
type ChannelCapabilities struct {
	// SupportsIdempotency indicates whether resending the same command_id within
	// RetryWindow is guaranteed not to cause duplicate physical actuation.
	SupportsIdempotency bool
	// SupportsStatusQuery indicates whether the channel can be queried by command_id
	// or downstream_id during crash recovery / uncertain-delivery reconciliation.
	SupportsStatusQuery bool
	// SupportsTelemetryConfirm indicates whether vehicle telemetry (H06) can confirm
	// command completion after dispatch.
	SupportsTelemetryConfirm bool
	RetryWindow              time.Duration
	ConfirmTimeout           time.Duration
	ReconcileWindow          time.Duration
}

type SendOutcomeKind string

const (
	OutcomeAccepted       SendOutcomeKind = "accepted"
	OutcomeCompleted      SendOutcomeKind = "completed"
	OutcomeRetryableError SendOutcomeKind = "retryable_error"
	OutcomeRejected       SendOutcomeKind = "rejected"
	OutcomeUncertain      SendOutcomeKind = "uncertain"
)

type SendResponse struct {
	Kind         SendOutcomeKind
	DownstreamID string
	Reason       string
}

type ReceiptStatus string

const (
	ReceiptAccepted  ReceiptStatus = "accepted"
	ReceiptCompleted ReceiptStatus = "completed"
	ReceiptFailed    ReceiptStatus = "failed"
)

type VehicleReceipt struct {
	CommandID    string
	DownstreamID string
	Status       ReceiptStatus
	Reason       string
	OccurredAt   time.Time
}

// VehicleChannel abstracts simulated or real vehicle command transports.
type VehicleChannel interface {
	Capabilities() ChannelCapabilities
	SendCommand(ctx context.Context, cmd contract.VehicleCommand) (SendResponse, error)
	QueryCommand(ctx context.Context, commandID, downstreamID string) (VehicleReceipt, bool, error)
}
