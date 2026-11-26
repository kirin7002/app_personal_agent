package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

func TestAdmissionAndStreamRecovery(t *testing.T) {
	var starts, streams atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing service authentication")
		}
		switch r.URL.Path {
		case "/v1/runs":
			var in StartRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Input != "左后窗" || in.SessionID != "S" ||
				r.Header.Get("Idempotency-Key") != "key" || r.Header.Get(contract.TurnHandleHeader) != "handle" {
				t.Errorf("unexpected start: %+v %v", in, err)
			}
			if starts.Add(1) == 1 {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"run_id":"run_1","status":"started","new_option":true}`)
		case "/v1/runs/run_1":
			fmt.Fprint(w, `{"run_id":"run_1","status":"running"}`)
		case "/v1/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"event\":\"tool.completed\",\"run_id\":\"run_1\",\"seq\":1}\n\n")
			if streams.Add(1) == 2 {
				if r.Header.Get("Last-Event-ID") != "1" {
					t.Error("missing event cursor")
				}
				fmt.Fprint(w, "data: {\"event\":\"run.completed\",\ndata: \"run_id\":\"run_1\",\"output\":\"正在处理\",\"seq\":2}\n\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.RetryAdmission = true
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	run, err := c.Start(ctx, StartRequest{SessionID: "S", Input: "左后窗"}, contract.TurnMetadata{HermesSessionID: "S", TurnHandle: "handle"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Wait(ctx, run.ID)
	if err != nil || result.Status != "completed" || result.Output != "正在处理" || starts.Load() != 2 || streams.Load() != 2 {
		t.Fatalf("unexpected result %+v, %v, starts=%d streams=%d", result, err, starts.Load(), streams.Load())
	}
}

func TestPollingTerminalBeforeStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run_2" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"run_id":"run_2","status":"failed"}`)
	}))
	defer server.Close()
	c, err := New(server.URL, "key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Wait(context.Background(), "run_2")
	if err != nil || got.Status != "failed" {
		t.Fatalf("unexpected result %+v %v", got, err)
	}
}
