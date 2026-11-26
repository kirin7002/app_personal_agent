package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/coder/websocket/wsjson"
)

// ProbeReadAloud asks the realtime model to read text and returns its actual
// response transcript. A generative response may differ from the requested text.
func (f *Factory) ProbeReadAloud(ctx context.Context, text string, emit func([]byte) error) (string, error) {
	if strings.TrimSpace(text) == "" || len(text) > 4096 {
		return "", errors.New("read-aloud text must be nonempty and at most 4096 bytes")
	}
	conn, _, err := f.connect(ctx, "你是播报器，请逐字复述用户指定的句子，不加任何开头、结尾或解释。")
	if err != nil {
		return "", err
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "只朗读这一句：" + text}},
		},
	}); err != nil {
		return "", connectionError("conversation.item.create", err)
	}
	if _, err := waitEvent(ctx, conn, "conversation.item.created"); err != nil {
		return "", err
	}
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "response.create"}); err != nil {
		return "", connectionError("response.create", err)
	}
	created, err := waitEvent(ctx, conn, "response.created")
	if err != nil {
		return "", err
	}
	if created.Response.ID == "" {
		return "", &ProtocolError{Stage: "response.created", Code: "missing_response_id"}
	}
	responseID := created.Response.ID
	var spoken strings.Builder
	for {
		var current event
		if err := wsjson.Read(ctx, conn, &current); err != nil {
			return "", connectionError("read-aloud", err)
		}
		switch current.Type {
		case "error":
			return "", current.failure("read-aloud")
		case "response.audio.delta":
			if current.ResponseID != responseID {
				continue
			}
			var pcm []byte
			if err := json.Unmarshal(current.Delta, &pcm); err != nil || len(pcm) == 0 || len(pcm)%2 != 0 {
				return "", &ProtocolError{Stage: current.Type, Code: "invalid_pcm"}
			}
			if err := emit(pcm); err != nil {
				return "", err
			}
		case "response.audio_transcript.done":
			if current.ResponseID == responseID {
				spoken.WriteString(current.Transcript)
			}
		case "response.done":
			if current.Response.ID != responseID {
				continue
			}
			if current.Response.Status != "completed" {
				return "", &ProtocolError{Stage: current.Type, Code: current.Response.Status}
			}
			return spoken.String(), nil
		}
	}
}
