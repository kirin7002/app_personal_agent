package executor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/catalog"
)

type SimMode string

const (
	SimAutoComplete       SimMode = "auto_complete"
	SimAcceptedOnly       SimMode = "accepted_only"
	SimRetryableOnce      SimMode = "retryable_once"
	SimRejected           SimMode = "rejected"
	SimUncertainExecuted  SimMode = "uncertain_executed"
	SimUncertainDropped   SimMode = "uncertain_dropped"
)

// SimulatedChannel is a deterministic vehicle channel supporting window/HVAC
// actuation, telemetry updates, and fault injection for H05/H07 verification.
type SimulatedChannel struct {
	Caps                  ChannelCapabilities
	Mode                  SimMode
	Telemetry             *catalog.MemoryTelemetry
	UpdateTelemetryOnSend bool
	Now                   func() time.Time

	mu             sync.Mutex
	sentCounts     map[string]int
	downstreamByID map[string]string
	statusByCmd    map[string]VehicleReceipt
	failOnceDone   map[string]bool
	seq            int
}

func NewSimulatedChannel(tel *catalog.MemoryTelemetry) *SimulatedChannel {
	return &SimulatedChannel{
		Caps: ChannelCapabilities{
			SupportsIdempotency:      true,
			SupportsStatusQuery:      true,
			SupportsTelemetryConfirm: true,
			RetryWindow:              2 * time.Minute,
			ConfirmTimeout:           30 * time.Second,
			ReconcileWindow:          5 * time.Minute,
		},
		Mode:                  SimAutoComplete,
		Telemetry:             tel,
		UpdateTelemetryOnSend: true,
		sentCounts:            make(map[string]int),
		downstreamByID:        make(map[string]string),
		statusByCmd:           make(map[string]VehicleReceipt),
		failOnceDone:          make(map[string]bool),
	}
}

func (s *SimulatedChannel) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *SimulatedChannel) Capabilities() ChannelCapabilities {
	return s.Caps
}

func (s *SimulatedChannel) SendCount(commandID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sentCounts[commandID]
}

func (s *SimulatedChannel) SendCommand(_ context.Context, cmd contract.VehicleCommand) (SendResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sentCounts[cmd.CommandID]++
	now := s.now()

	downstreamID, ok := s.downstreamByID[cmd.CommandID]
	if !ok {
		s.seq++
		downstreamID = fmt.Sprintf("VEH-%04d", s.seq)
		s.downstreamByID[cmd.CommandID] = downstreamID
	}

	switch s.Mode {
	case SimRejected:
		s.statusByCmd[cmd.CommandID] = VehicleReceipt{
			CommandID: cmd.CommandID, DownstreamID: downstreamID,
			Status: ReceiptFailed, Reason: "vehicle rejected command", OccurredAt: now,
		}
		return SendResponse{Kind: OutcomeRejected, DownstreamID: downstreamID, Reason: "vehicle rejected command"}, nil

	case SimRetryableOnce:
		if !s.failOnceDone[cmd.CommandID] {
			s.failOnceDone[cmd.CommandID] = true
			return SendResponse{Kind: OutcomeRetryableError, DownstreamID: downstreamID, Reason: "transient gateway timeout"}, nil
		}
		s.applyTelemetryLocked(cmd, now)
		s.statusByCmd[cmd.CommandID] = VehicleReceipt{
			CommandID: cmd.CommandID, DownstreamID: downstreamID,
			Status: ReceiptCompleted, OccurredAt: now,
		}
		return SendResponse{Kind: OutcomeCompleted, DownstreamID: downstreamID}, nil

	case SimAcceptedOnly:
		s.statusByCmd[cmd.CommandID] = VehicleReceipt{
			CommandID: cmd.CommandID, DownstreamID: downstreamID,
			Status: ReceiptAccepted, OccurredAt: now,
		}
		return SendResponse{Kind: OutcomeAccepted, DownstreamID: downstreamID}, nil

	case SimUncertainExecuted:
		// Downstream actually executed the command, but the network response timed out.
		s.applyTelemetryLocked(cmd, now)
		s.statusByCmd[cmd.CommandID] = VehicleReceipt{
			CommandID: cmd.CommandID, DownstreamID: downstreamID,
			Status: ReceiptCompleted, OccurredAt: now,
		}
		return SendResponse{Kind: OutcomeUncertain, DownstreamID: downstreamID, Reason: "ambiguous transport timeout"}, nil

	case SimUncertainDropped:
		// Downstream never produced a terminal status.
		return SendResponse{Kind: OutcomeUncertain, DownstreamID: downstreamID, Reason: "ambiguous transport timeout"}, nil

	default: // SimAutoComplete
		s.applyTelemetryLocked(cmd, now)
		s.statusByCmd[cmd.CommandID] = VehicleReceipt{
			CommandID: cmd.CommandID, DownstreamID: downstreamID,
			Status: ReceiptCompleted, OccurredAt: now,
		}
		return SendResponse{Kind: OutcomeCompleted, DownstreamID: downstreamID}, nil
	}
}

func (s *SimulatedChannel) QueryCommand(_ context.Context, commandID, downstreamID string) (VehicleReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.statusByCmd[commandID]; ok {
		return rec, true, nil
	}
	if downstreamID != "" {
		for _, rec := range s.statusByCmd {
			if rec.DownstreamID == downstreamID {
				return rec, true, nil
			}
		}
	}
	return VehicleReceipt{}, false, nil
}

// ApplyTelemetry simulates the vehicle finishing actuation and reporting fresh telemetry.
func (s *SimulatedChannel) ApplyTelemetry(cmd contract.VehicleCommand, sampledAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyTelemetryLocked(cmd, sampledAt)
}

func (s *SimulatedChannel) applyTelemetryLocked(cmd contract.VehicleCommand, sampledAt time.Time) {
	if !s.UpdateTelemetryOnSend || s.Telemetry == nil {
		return
	}
	switch cmd.Operation {
	case contract.Operation{Device: "window", Tool: "close"}:
		if pos, ok := cmd.Params["position"].(string); ok && pos != "" {
			s.Telemetry.Set(cmd.Scope.VehicleID, "window."+pos, "closed", sampledAt)
		}
	case contract.Operation{Device: "window", Tool: "open"}:
		if pos, ok := cmd.Params["position"].(string); ok && pos != "" {
			s.Telemetry.Set(cmd.Scope.VehicleID, "window."+pos, "open", sampledAt)
		}
	case contract.Operation{Device: "hvac", Tool: "set_power"}, contract.Operation{Device: "hvac", Tool: "set_climate"}:
		if power, ok := cmd.Params["power"].(string); ok && power != "" {
			s.Telemetry.Set(cmd.Scope.VehicleID, "hvac.power", power, sampledAt)
		}
		if temp, ok := cmd.Params["temperature_c"]; ok && temp != nil {
			s.Telemetry.Set(cmd.Scope.VehicleID, "hvac.temperature_c", temp, sampledAt)
		}
	case contract.Operation{Device: "hvac", Tool: "set_temperature"}:
		s.Telemetry.Set(cmd.Scope.VehicleID, "hvac.power", "on", sampledAt)
		if temp, ok := cmd.Params["temperature_c"]; ok && temp != nil {
			s.Telemetry.Set(cmd.Scope.VehicleID, "hvac.temperature_c", temp, sampledAt)
		}
	}
}
