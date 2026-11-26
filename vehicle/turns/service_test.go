package turns

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/runs"
)

type fakeData struct {
	now time.Time
}

func (f fakeData) ListPending(_ context.Context, scope contract.Scope, now time.Time) ([]contract.PendingCandidate, error) {
	if scope.VehicleID != "car-1" {
		return nil, nil
	}
	return []contract.PendingCandidate{
		{TaskID: "expired", CreatedAt: now.Add(-contract.PendingTTL), ExpiresAt: now},
		{TaskID: "T101", Operation: contract.Operation{Device: "window", Tool: "close"}, Version: 3,
			CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9 * time.Minute),
			KnownParams: map[string]any{}, MissingFields: []string{"position"}, Question: "要关哪扇车窗？",
			ParamsSchema: json.RawMessage(`{"type":"object","required":["position"]}`)},
	}, nil
}

func (fakeData) FindCapabilities(context.Context, contract.Scope, string) ([]contract.Capability, error) {
	return nil, nil
}

func (fakeData) ReadTurnProgress(_ context.Context, scope contract.Scope, request string) (contract.TaskProgress, error) {
	if scope.VehicleID != "car-1" {
		return contract.TaskProgress{}, contract.ErrTurnHasNoWrite
	}
	return contract.TaskProgress{TaskID: "T101", CommandID: "C88", Status: contract.PendingDispatch}, nil
}

type fakeRuns struct {
	start func(context.Context, runs.StartRequest, contract.TurnMetadata, string) (runs.Run, error)
	wait  func(context.Context, string) (runs.Run, error)
}

func (f fakeRuns) Start(ctx context.Context, in runs.StartRequest, meta contract.TurnMetadata, key string) (runs.Run, error) {
	return f.start(ctx, in, meta, key)
}

func (f fakeRuns) Wait(ctx context.Context, id string) (runs.Run, error) { return f.wait(ctx, id) }
func (fakeRuns) Stop(context.Context, string) error                      { return nil }

func TestConcurrentReplayAndCancellation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Now().UTC()
	data := fakeData{now: now}
	var count atomic.Int32
	var binding contract.TurnMetadata
	s := &Service{
		Store: store, Pending: data, Capabilities: data, Progress: data, Now: func() time.Time { return now },
		Runs: fakeRuns{
			start: func(ctx context.Context, in runs.StartRequest, meta contract.TurnMetadata, _ string) (runs.Run, error) {
				count.Add(1)
				binding = meta
				trusted, err := store.ResolveTurn(ctx, meta, now)
				if err != nil || len(trusted.Candidates) != 1 || trusted.Candidates[0].TaskID != "T101" || trusted.Candidates[0].Version != 3 {
					t.Errorf("callback before admission response failed: %+v %v", trusted, err)
				}
				if !strings.Contains(in.Instructions, `"required":["position"]`) || strings.Contains(in.Instructions, meta.TurnHandle) {
					t.Error("missing pending schema or leaked metadata")
				}
				return runs.Run{ID: "run_1"}, nil
			},
			wait: func(context.Context, string) (runs.Run, error) {
				return runs.Run{ID: "run_1", Status: "failed", Output: "已经关闭"}, nil
			},
		},
	}
	turn := contract.VehicleTurn{
		Scope:       contract.Scope{UserID: "U", SessionID: "S", VehicleID: "car-1"},
		VoiceTurnID: "voice-1", RequestID: "req-1",
		FinalTranscript: contract.FinalTranscript{ProviderSessionID: "Q", InputItemID: "I", Text: "左后窗"},
	}
	var work sync.WaitGroup
	for i := 0; i < 10; i++ {
		work.Add(1)
		go func() {
			defer work.Done()
			reply, err := s.HandleVehicleTurn(ctx, turn)
			if err != nil || reply.Status != contract.PendingDispatch || reply.CommandID != "C88" || strings.Contains(reply.Text, "已经关闭") {
				t.Errorf("unexpected reply: %+v %v", reply, err)
			}
		}()
	}
	work.Wait()
	if count.Load() != 1 {
		t.Fatalf("started %d Runs", count.Load())
	}
	if _, err := store.ResolveTurn(ctx, binding, now); err == nil {
		t.Fatal("terminal Run left the turn writable")
	}
	turn.VoiceTurnID = "voice-2"
	turn.RequestID = "req-2"
	if err := s.CancelVehicleTurn(ctx, turn.Scope, turn.VoiceTurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleVehicleTurn(ctx, turn); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled turn started: %v", err)
	}
}
