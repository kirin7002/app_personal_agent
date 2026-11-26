package qwen

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func newFake(t *testing.T, timeout time.Duration, script func(context.Context, *websocket.Conn)) *Factory {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-key" {
			http.Error(w, "no authorization", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, stop := context.WithTimeout(r.Context(), 3*time.Second)
		defer stop()
		script(ctx, conn)
	}))
	t.Cleanup(server.Close)
	return &Factory{
		config: Config{APIKey: "mock-key", Timeout: timeout},
		url:    "ws" + strings.TrimPrefix(server.URL, "http"),
	}
}

func send(t *testing.T, ctx context.Context, c *websocket.Conn, kind string, extra map[string]any) bool {
	t.Helper()
	fields := map[string]any{"type": kind}
	for k, v := range extra {
		fields[k] = v
	}
	if err := wsjson.Write(ctx, c, fields); err != nil {
		t.Error(err)
		return false
	}
	return true
}

func expect(t *testing.T, ctx context.Context, c *websocket.Conn, kind string) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := wsjson.Read(ctx, c, &fields); err != nil {
		t.Error(err)
		return nil
	}
	if fields["type"] != kind {
		t.Errorf("got %v, expected %s", fields["type"], kind)
		return nil
	}
	return fields
}

func fakeSetup(t *testing.T, ctx context.Context, c *websocket.Conn) bool {
	t.Helper()
	id := map[string]any{"session": map[string]string{"id": "mock-session"}}
	if !send(t, ctx, c, "session.created", id) {
		return false
	}
	setup := expect(t, ctx, c, "session.update")
	if setup == nil {
		return false
	}
	config, _ := setup["session"].(map[string]any)
	if vad, ok := config["turn_detection"]; !ok || vad != nil {
		t.Error("expected push-to-talk")
		return false
	}
	return send(t, ctx, c, "session.updated", id)
}

func TestManualCommitAndStreaming(t *testing.T) {
	factory := newFake(t, time.Second, func(ctx context.Context, c *websocket.Conn) {
		if !fakeSetup(t, ctx, c) {
			return
		}
		for _, item := range []struct{ id, text string }{{"car", "把车窗关一下"}, {"chat", "你好"}} {
			if expect(t, ctx, c, "input_audio_buffer.append") == nil ||
				expect(t, ctx, c, "input_audio_buffer.commit") == nil {
				return
			}
			if !send(t, ctx, c, "conversation.item.input_audio_transcription.delta", map[string]any{"item_id": item.id, "text": "草稿"}) ||
				!send(t, ctx, c, "conversation.item.input_audio_transcription.completed", map[string]any{"item_id": item.id, "transcript": item.text}) ||
				!send(t, ctx, c, "input_audio_buffer.committed", map[string]any{"item_id": item.id}) {
				return
			}
		}
		if expect(t, ctx, c, "response.create") == nil {
			return
		}
		send(t, ctx, c, "response.created", map[string]any{"response": map[string]string{"id": "r1"}})
		send(t, ctx, c, "response.audio_transcript.delta", map[string]any{"response_id": "r1", "delta": "您好"})
		send(t, ctx, c, "response.audio.delta", map[string]any{"response_id": "old", "delta": []byte{3, 4}})
		send(t, ctx, c, "response.audio.delta", map[string]any{"response_id": "r1", "delta": []byte{1, 2}})
		if expect(t, ctx, c, "response.cancel") == nil {
			return
		}
		send(t, ctx, c, "response.done", map[string]any{"response": map[string]string{"id": "r1", "status": "cancelled"}})
		<-ctx.Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := factory.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, want := range []struct{ id, final string }{{"car", "把车窗关一下"}, {"chat", "你好"}} {
		if err := session.AppendAudio(ctx, []byte{1, 2}); err != nil {
			t.Fatal(err)
		}
		got, err := session.Commit(ctx)
		if err != nil || got.InputItemID != want.id || got.ProviderSessionID != "mock-session" || got.Text != want.final {
			t.Fatalf("commit = %+v, %v", got, err)
		}
	}
	response, err := session.CreateResponse(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := response.Next(ctx)
	if err != nil || len(chunk) != 2 || chunk[0] != 1 {
		t.Fatalf("unexpected response chunk %v, %v", chunk, err)
	}
	if err := session.CancelResponse(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := response.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled, got %v", err)
	}
}

func TestConfigurationFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		close websocket.StatusCode
	}{
		{"timeout", -1},
		{"close 1002", websocket.StatusProtocolError},
	} {
		t.Run(test.name, func(t *testing.T) {
			factory := newFake(t, 120*time.Millisecond, func(ctx context.Context, c *websocket.Conn) {
				if !send(t, ctx, c, "session.created", map[string]any{"session": map[string]string{"id": "mock-session"}}) {
					return
				}
				if expect(t, ctx, c, "session.update") == nil {
					return
				}
				if test.close > 0 {
					_ = c.Close(test.close, "config rejected")
					return
				}
				_, _, _ = c.Read(ctx)
			})
			_, err := factory.Connect(context.Background())
			var diagnostic *ProtocolError
			if !errors.As(err, &diagnostic) || diagnostic.Stage != "session.updated" || diagnostic.CloseCode != test.close {
				t.Fatalf("unexpected diagnostic: %+v", err)
			}
		})
	}
}

func TestTranscriptionFailureAndNewSession(t *testing.T) {
	f := newFake(t, time.Second, func(ctx context.Context, c *websocket.Conn) {
		if !fakeSetup(t, ctx, c) || expect(t, ctx, c, "input_audio_buffer.append") == nil || expect(t, ctx, c, "input_audio_buffer.commit") == nil {
			return
		}
		send(t, ctx, c, "input_audio_buffer.committed", map[string]any{"item_id": "failed"})
		send(t, ctx, c, "conversation.item.input_audio_transcription.failed", map[string]any{
			"item_id": "failed", "event_id": "err1", "error": map[string]string{"code": "transcription_failed"},
		})
		_, _, _ = c.Read(ctx)
	})
	for i := 0; i < 2; i++ {
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		session, err := f.Connect(ctx)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		if err := session.AppendAudio(ctx, []byte{1, 2}); err != nil {
			t.Error(err)
		}
		_, err = session.Commit(ctx)
		_ = session.Close()
		stop()
		var diagnostic *ProtocolError
		if !errors.As(err, &diagnostic) || diagnostic.Stage != "transcription" || diagnostic.EventID != "err1" || errors.Is(err, io.EOF) {
			t.Fatalf("unexpected diagnostic: %+v", err)
		}
	}
}

func TestBailianEndpoint(t *testing.T) {
	for _, test := range []struct {
		workspace, region, endpoint string
	}{
		{"llm-example", "cn-beijing", "wss://llm-example.cn-beijing.maas.aliyuncs.com"},
		{"", "cn-beijing", "wss://dashscope.aliyuncs.com"},
		{"", "ap-southeast-1", "wss://dashscope-intl.aliyuncs.com"},
	} {
		factory, err := New(Config{APIKey: "fixture", WorkspaceID: test.workspace, Region: test.region})
		if err != nil || factory.url != test.endpoint+"/api-ws/v1/realtime?model="+Model {
			t.Fatalf("failed to resolve region %q and workspace %q: %v", test.region, test.workspace, err)
		}
	}
}

func TestProbeReadAloudUsesMatchingResponse(t *testing.T) {
	const text = "左后车窗已关闭。"
	factory := newFake(t, time.Second, func(ctx context.Context, c *websocket.Conn) {
		if !fakeSetup(t, ctx, c) {
			return
		}
		if expect(t, ctx, c, "conversation.item.create") == nil ||
			!send(t, ctx, c, "conversation.item.created", map[string]any{"item": map[string]string{"id": "input"}}) ||
			expect(t, ctx, c, "response.create") == nil {
			return
		}
		send(t, ctx, c, "response.created", map[string]any{"response": map[string]string{"id": "speech"}})
		send(t, ctx, c, "response.audio_transcript.done", map[string]any{"response_id": "other", "transcript": "ignore"})
		send(t, ctx, c, "response.audio.delta", map[string]any{"response_id": "speech", "delta": []byte{1, 2}})
		send(t, ctx, c, "response.audio_transcript.done", map[string]any{"response_id": "speech", "transcript": text})
		send(t, ctx, c, "response.done", map[string]any{"response": map[string]string{"id": "speech", "status": "completed"}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var received []byte
	spoken, err := factory.ProbeReadAloud(ctx, text, func(pcm []byte) error {
		received = append(received, pcm...)
		return nil
	})
	if err != nil || spoken != text || len(received) != 2 {
		t.Fatalf("unexpected read-aloud result transcript=%q audio=%v err=%v", spoken, received, err)
	}
}
