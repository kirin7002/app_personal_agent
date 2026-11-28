package catalog

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

const (
	AvailabilityFresh       = "fresh"
	AvailabilityStale       = "stale"
	AvailabilityUnavailable = "unavailable"

	DefaultTelemetryMaxAge = 30 * time.Second
)

// TelemetryPoint represents a raw vehicle telemetry sample.
type TelemetryPoint struct {
	Value     any
	SampledAt *time.Time
	Available bool
}

// TelemetryProvider reads raw signals for a specific vehicle.
type TelemetryProvider interface {
	ReadSignals(ctx context.Context, vehicleID string, signalNames []string) (map[string]TelemetryPoint, error)
}

// MemoryTelemetry is a thread-safe in-memory telemetry store for tests and simulation.
type MemoryTelemetry struct {
	mu     sync.RWMutex
	points map[string]map[string]TelemetryPoint // vehicleID -> signalName -> point
}

func NewMemoryTelemetry() *MemoryTelemetry {
	return &MemoryTelemetry{
		points: make(map[string]map[string]TelemetryPoint),
	}
}

func (m *MemoryTelemetry) Set(vehicleID, signalName string, value any, sampledAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.points[vehicleID] == nil {
		m.points[vehicleID] = make(map[string]TelemetryPoint)
	}
	t := sampledAt.UTC()
	m.points[vehicleID][signalName] = TelemetryPoint{
		Value:     value,
		SampledAt: &t,
		Available: true,
	}
}

func (m *MemoryTelemetry) SetUnavailable(vehicleID, signalName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.points[vehicleID] == nil {
		m.points[vehicleID] = make(map[string]TelemetryPoint)
	}
	m.points[vehicleID][signalName] = TelemetryPoint{
		Value:     "unknown",
		SampledAt: nil,
		Available: false,
	}
}

func (m *MemoryTelemetry) ReadSignals(_ context.Context, vehicleID string, signalNames []string) (map[string]TelemetryPoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]TelemetryPoint, len(signalNames))
	vMap := m.points[vehicleID]
	for _, name := range signalNames {
		if pt, ok := vMap[name]; ok {
			out[name] = pt
		}
	}
	return out, nil
}

// ClassifyAvailability evaluates signal freshness strictly against Go's current clock.
func ClassifyAvailability(pt TelemetryPoint, now time.Time, maxAge time.Duration) string {
	if !pt.Available || pt.SampledAt == nil {
		return AvailabilityUnavailable
	}
	if maxAge <= 0 {
		maxAge = DefaultTelemetryMaxAge
	}
	age := now.Sub(*pt.SampledAt)
	if age > maxAge || age < -5*time.Second {
		return AvailabilityStale
	}
	return AvailabilityFresh
}

// GetVehicleStatus serves the get_vehicle_status MCP read tool using the same capability
// schema for validation and returning independent per-signal freshness metadata.
func (s *Service) GetVehicleStatus(ctx context.Context, scope contract.Scope, input contract.VehicleStatusInput, now time.Time) (contract.VehicleStatusResult, error) {
	if !scope.Valid() {
		return contract.VehicleStatusResult{}, &contract.BusinessError{
			Code:    contract.TurnContextInvalid,
			Message: "invalid scope",
		}
	}
	outcome, spec, err := s.ValidateMutation(scope.VehicleID, input.Operation, input.Params, true)
	if err != nil {
		return contract.VehicleStatusResult{}, err
	}
	if spec.MCPTool != contract.GetVehicleStatus {
		return contract.VehicleStatusResult{}, &contract.BusinessError{
			Code:    contract.InvalidArgument,
			Message: fmt.Sprintf("operation %s.%s is not a read-only status tool", input.Device, input.Tool),
		}
	}

	signalNames, err := signalsForStatusQuery(spec, outcome.Normalized)
	if err != nil {
		return contract.VehicleStatusResult{}, err
	}
	signals, err := s.readClassifiedSignals(ctx, scope.VehicleID, signalNames, now)
	if err != nil {
		return contract.VehicleStatusResult{}, err
	}
	return contract.VehicleStatusResult{Signals: signals}, nil
}

func signalsForStatusQuery(spec OperationSpec, normalized map[string]any) ([]string, error) {
	if spec.Operation == (contract.Operation{Device: "window", Tool: "get_status"}) {
		positions, _ := normalized["positions"].([]string)
		names := make([]string, 0, len(positions))
		for _, pos := range positions {
			names = append(names, "window."+pos)
		}
		return names, nil
	}
	if len(spec.StatusSignals) > 0 {
		// Support optional local post-filtering by "names" (e.g., car_airconditioner_status).
		if requested, ok := normalized["names"].([]string); ok && len(requested) > 0 {
			allowed := make(map[string]bool, len(spec.StatusSignals))
			for _, sig := range spec.StatusSignals {
				allowed[sig] = true
			}
			filtered := make([]string, 0, len(requested))
			for _, name := range requested {
				if allowed[name] {
					filtered = append(filtered, name)
				}
			}
			if len(filtered) > 0 {
				return filtered, nil
			}
		}
		out := make([]string, len(spec.StatusSignals))
		copy(out, spec.StatusSignals)
		return out, nil
	}
	return nil, &contract.BusinessError{
		Code:    contract.CapabilityUnavailable,
		Message: fmt.Sprintf("unsupported status operation %s.%s", spec.Operation.Device, spec.Operation.Tool),
	}
}

func (s *Service) readClassifiedSignals(ctx context.Context, vehicleID string, signalNames []string, now time.Time) ([]contract.Signal, error) {
	raw, err := s.telemetry.ReadSignals(ctx, vehicleID, signalNames)
	if err != nil {
		return nil, err
	}
	signals := make([]contract.Signal, 0, len(signalNames))
	for _, name := range signalNames {
		pt, exists := raw[name]
		if !exists || !pt.Available || pt.SampledAt == nil {
			signals = append(signals, contract.Signal{
				Name:         name,
				Value:        "unknown",
				SampledAt:    nil,
				Availability: AvailabilityUnavailable,
			})
			continue
		}
		sampled := pt.SampledAt.UTC()
		signals = append(signals, contract.Signal{
			Name:         name,
			Value:        pt.Value,
			SampledAt:    &sampled,
			Availability: ClassifyAvailability(pt, now, DefaultTelemetryMaxAge),
		})
	}
	return signals, nil
}

// AllWindowsClosed returns true ONLY when all four window signals are present,
// fresh, and report "closed". Any stale or unavailable window causes false.
func AllWindowsClosed(res contract.VehicleStatusResult) bool {
	byName := make(map[string]contract.Signal, len(res.Signals))
	for _, sig := range res.Signals {
		byName[sig.Name] = sig
	}
	for _, pos := range WindowPositions {
		sig, ok := byName["window."+pos]
		if !ok || sig.Availability != AvailabilityFresh || sig.Value != "closed" {
			return false
		}
	}
	return true
}

// ConfirmationResult describes whether fresh post-dispatch telemetry confirms a command.
type ConfirmationResult struct {
	Confirmed bool
	Reason    string
	Signals   []contract.Signal
}

// ConfirmCommandEffect reuses H06's signal mapping and freshness checks for H05's executor.
// Only samples collected on or after dispatchedAt and still fresh at now can confirm success.
func (s *Service) ConfirmCommandEffect(
	ctx context.Context,
	vehicleID string,
	op contract.Operation,
	params map[string]any,
	dispatchedAt time.Time,
	now time.Time,
) (ConfirmationResult, error) {
	outcome, _, err := s.ValidateMutation(vehicleID, op, params, true)
	if err != nil {
		return ConfirmationResult{}, err
	}

	type expectation struct {
		signal string
		check  func(any) bool
	}
	var expected []expectation
	switch op {
	case contract.Operation{Device: "window", Tool: "close"}:
		pos, _ := outcome.Normalized["position"].(string)
		expected = []expectation{{
			signal: "window." + pos,
			check:  func(v any) bool { return v == "closed" },
		}}
	case contract.Operation{Device: "window", Tool: "open"}:
		pos, _ := outcome.Normalized["position"].(string)
		expected = []expectation{{
			signal: "window." + pos,
			check:  func(v any) bool { return v == "open" },
		}}
	case contract.Operation{Device: "hvac", Tool: "set_temperature"}:
		targetTemp, _ := toFloat64(outcome.Normalized["temperature_c"])
		// Design §5: HVAC confirmation must verify both power=on and target temperature.
		expected = []expectation{
			{
				signal: "hvac.power",
				check:  func(v any) bool { return v == "on" },
			},
			{
				signal: "hvac.temperature_c",
				check: func(v any) bool {
					got, ok := toFloat64(v)
					return ok && math.Abs(got-targetTemp) < 0.05
				},
			},
		}
	case contract.Operation{Device: "hvac", Tool: "set_power"}:
		targetPower, _ := outcome.Normalized["power"].(string)
		expected = []expectation{{
			signal: "hvac.power",
			check:  func(v any) bool { return v == targetPower },
		}}
	case contract.Operation{Device: "hvac", Tool: "set_climate"}:
		targetPower, _ := outcome.Normalized["power"].(string)
		targetTemp, _ := toFloat64(outcome.Normalized["temperature_c"])
		expected = []expectation{
			{
				signal: "hvac.power",
				check:  func(v any) bool { return v == targetPower },
			},
			{
				signal: "hvac.temperature_c",
				check: func(v any) bool {
					got, ok := toFloat64(v)
					return ok && math.Abs(got-targetTemp) < 0.05
				},
			},
		}
	default:
		return ConfirmationResult{}, &contract.BusinessError{
			Code:    contract.CapabilityUnavailable,
			Message: fmt.Sprintf("telemetry confirmation unsupported for %s.%s", op.Device, op.Tool),
		}
	}

	names := make([]string, len(expected))
	for i, e := range expected {
		names[i] = e.signal
	}
	signals, err := s.readClassifiedSignals(ctx, vehicleID, names, now)
	if err != nil {
		return ConfirmationResult{}, err
	}

	for i, exp := range expected {
		sig := signals[i]
		if sig.Availability != AvailabilityFresh {
			return ConfirmationResult{
				Confirmed: false,
				Reason:    fmt.Sprintf("signal %s availability is %s", sig.Name, sig.Availability),
				Signals:   signals,
			}, nil
		}
		if sig.SampledAt == nil || sig.SampledAt.Before(dispatchedAt) {
			return ConfirmationResult{
				Confirmed: false,
				Reason:    fmt.Sprintf("signal %s sampled before dispatch", sig.Name),
				Signals:   signals,
			}, nil
		}
		if !exp.check(sig.Value) {
			return ConfirmationResult{
				Confirmed: false,
				Reason:    fmt.Sprintf("signal %s value %v does not match target", sig.Name, sig.Value),
				Signals:   signals,
			}, nil
		}
	}

	return ConfirmationResult{
		Confirmed: true,
		Reason:    "target telemetry confirmed",
		Signals:   signals,
	}, nil
}
