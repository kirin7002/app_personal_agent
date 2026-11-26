package contract

import (
	"encoding/json"
	"os"
	"testing"
)

type fixture struct {
	Steps []struct {
		Turn    VehicleTurn        `json:"turn"`
		Pending []PendingCandidate `json:"pending_tasks"`
		Call    struct {
			Name string       `json:"name"`
			Args TaskMutation `json:"arguments"`
		} `json:"mcp_call"`
		Result TaskResult `json:"mcp_result"`
	} `json:"steps"`
	Result VehicleResultEvent `json:"vehicle_result"`
}

func loadFixture[T any](t *testing.T, name string) T {
	t.Helper()
	content, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestScenarios(t *testing.T) {
	a := loadFixture[fixture](t, "scenario-a")
	if len(a.Steps) != 2 {
		t.Fatal("scenario A must have two turns")
	}
	first, second := a.Steps[0], a.Steps[1]
	if first.Call.Name != CreateVehicleTask || first.Result.Status != PendingReply ||
		first.Turn.Scope != second.Turn.Scope || second.Call.Args.TaskID != first.Result.TaskID ||
		second.Call.Name != DispatchVehicleCommand || second.Call.Args.Params["position"] != "rear_left" ||
		second.Result.Status != PendingDispatch || a.Result.Status != Completed ||
		a.Result.CommandID != second.Result.CommandID || a.Result.RequestID != second.Turn.RequestID {
		t.Fatalf("broken scenario A correlation: %+v", a)
	}
	b := loadFixture[fixture](t, "scenario-b")
	if len(b.Steps) != 1 || len(b.Steps[0].Pending) != 2 {
		t.Fatal("scenario B must have two candidates")
	}
	turn := b.Steps[0]
	if !turn.Pending[0].CreatedAt.After(turn.Pending[1].CreatedAt) ||
		turn.Call.Args.TaskID != turn.Pending[1].TaskID ||
		turn.Pending[1].ExpiresAt.Sub(turn.Pending[1].CreatedAt) != PendingTTL {
		t.Fatalf("incorrect candidate order, selection or expiry: %+v", turn)
	}
}

func TestFailureFixtures(t *testing.T) {
	failures := loadFixture[[]BusinessError](t, "failures")
	want := []ErrorCode{TurnContextInvalid, ClarificationRequired, TaskExpired, IdempotencyConflict}
	if len(failures) != len(want) {
		t.Fatalf("got %d failures", len(failures))
	}
	for i, failure := range failures {
		if failure.Code != want[i] || failure.Message == "" || failure.Retryable {
			t.Fatalf("unexpected failure: %+v", failure)
		}
	}
}
