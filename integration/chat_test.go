package integration

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
	"github.com/kirin7002/app_personal_agent/vehicle/tasks"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
	"github.com/kirin7002/app_personal_agent/voice/gateway"
)

func TestLiveChatLatency(t *testing.T) {
	if os.Getenv("H07_LIVE") != "1" {
		t.Skip("set H07_LIVE=1 to run live voice integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	factory, err := qwen.New(qwen.Config{
		APIKey: os.Getenv("DASHSCOPE_API_KEY"), WorkspaceID: os.Getenv("DASHSCOPE_WORKSPACE_ID"),
		Region: os.Getenv("DASHSCOPE_REGION"),
	})
	if err != nil {
		t.Fatal(err)
	}
	pcm := sample(t, "chat.pcm")
	scope := contract.Scope{UserID: "h07-user", SessionID: rand.Text(), VehicleID: "h07-car"}
	auth := carAuth{token: rand.Text(), scope: scope}
	store := tasks.NewMemoryStore(nil)
	server := httptest.NewServer(&gateway.Gateway{
		Auth: auth, Media: factory, Turns: &turns.Service{}, Pending: store, Results: store, TTS: fixtureTTS{},
	})
	defer server.Close()
	start := time.Now()
	car := connectCar(t, ctx, server.URL, auth.token, scope)
	t.Logf("latency workflow=chat connection=cold session_ready_ms=%d", time.Since(start).Milliseconds())
	for _, connection := range []string{"cold", "warm"} {
		car.say("chat-"+connection, pcm, true)
		endOfInput := time.Now()
		heard := car.hear()
		if heard.reply != nil || heard.result != nil {
			t.Fatalf("chat did not use native Qwen audio: %+v", heard.reply)
		}
		t.Logf("latency workflow=chat connection=%s end_to_first_audio_ms=%d", connection, heard.firstAudioAt.Sub(endOfInput).Milliseconds())
	}
}
