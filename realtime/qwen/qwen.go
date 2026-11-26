// Package qwen adapts the Bailian Audio Realtime push-to-talk protocol.
package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
)

const Model = "qwen-audio-3.1-realtime-flash"

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type Config struct {
	APIKey      string
	WorkspaceID string
	Region      string
	Model       string
	Voice       string
	Timeout     time.Duration
	OnStage     func(string, time.Duration)
}

type Factory struct {
	config  Config
	url     string
	options *websocket.DialOptions
}

func New(config Config) (*Factory, error) {
	if config.APIKey == "" || !dnsLabel.MatchString(config.WorkspaceID) {
		return nil, errors.New("DASHSCOPE_API_KEY and a valid DASHSCOPE_WORKSPACE_ID are required")
	}
	if config.Region == "" {
		config.Region = "cn-beijing"
	}
	if !dnsLabel.MatchString(config.Region) {
		return nil, errors.New("invalid DashScope region")
	}
	if config.Model == "" {
		config.Model = Model
	}
	endpoint := url.URL{
		Scheme:   "wss",
		Host:     config.WorkspaceID + "." + config.Region + ".maas.aliyuncs.com",
		Path:     "/api-ws/v1/realtime",
		RawQuery: url.Values{"model": {config.Model}}.Encode(),
	}
	return &Factory{config: config, url: endpoint.String()}, nil
}

// ProtocolError reports a failing protocol stage and excludes response bodies and credentials.
type ProtocolError struct {
	Stage      string
	HTTPStatus int
	CloseCode  websocket.StatusCode
	EventID    string
	Code       string
	cause      error
}

func (e *ProtocolError) Error() string {
	msg := "qwen realtime " + e.Stage
	if e.HTTPStatus != 0 {
		msg += fmt.Sprintf(" HTTP %d", e.HTTPStatus)
	}
	if e.CloseCode > 0 {
		msg += fmt.Sprintf(" close %d", e.CloseCode)
	}
	if e.EventID != "" {
		msg += " event=" + e.EventID
	}
	if e.Code != "" {
		msg += " code=" + e.Code
	}
	if errors.Is(e.cause, context.DeadlineExceeded) || errors.Is(e.cause, context.Canceled) {
		msg += ": " + e.cause.Error()
	}
	return msg
}

func (e *ProtocolError) Unwrap() error { return e.cause }

type event struct {
	Type       string          `json:"type"`
	EventID    string          `json:"event_id"`
	ItemID     string          `json:"item_id"`
	ResponseID string          `json:"response_id"`
	Transcript string          `json:"transcript"`
	Delta      json.RawMessage `json:"delta"`
	Session    struct {
		ID string `json:"id"`
	} `json:"session"`
	Response struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"response"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (e event) failure(stage string) error {
	return &ProtocolError{Stage: stage, EventID: e.EventID, Code: e.Error.Code}
}

func connectionError(stage string, err error) error {
	return &ProtocolError{Stage: stage, CloseCode: websocket.CloseStatus(err), cause: err}
}

func waitEvent(ctx context.Context, conn *websocket.Conn, want string) (event, error) {
	for {
		var current event
		if err := wsjson.Read(ctx, conn, &current); err != nil {
			return event{}, connectionError(want, err)
		}
		if current.Type == "error" {
			return event{}, current.failure(want)
		}
		if current.Type == want {
			return current, nil
		}
	}
}

func (f *Factory) Connect(ctx context.Context) (contract.S2SSession, error) {
	timeout := f.config.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var options websocket.DialOptions
	if f.options != nil {
		options = *f.options
	}
	options.HTTPHeader = http.Header{
		"Authorization": {"Bearer " + f.config.APIKey},
	}
	// A WebSocket redirect must never forward the service credential to another endpoint.
	client := http.DefaultClient
	if options.HTTPClient != nil {
		client = options.HTTPClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	options.HTTPClient = &copyClient
	started := time.Now()
	conn, response, err := websocket.Dial(ctx, f.url, &options)
	if err != nil {
		result := &ProtocolError{Stage: "handshake", cause: err}
		if response != nil {
			result.HTTPStatus = response.StatusCode
		}
		return nil, result
	}
	f.stage("handshake", started)
	conn.SetReadLimit(1 << 20)
	created, err := waitEvent(ctx, conn, "session.created")
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	if strings.TrimSpace(created.Session.ID) == "" {
		conn.CloseNow()
		return nil, &ProtocolError{Stage: "session.created", Code: "missing_session_id"}
	}
	f.stage("session.created", started)
	voice := f.config.Voice
	if voice == "" {
		voice = "longanqian_v3.1"
	}
	config := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"modalities": []string{"text", "audio"}, "voice": voice,
			"input_audio_format": "pcm", "output_audio_format": "pcm",
			"input_audio_transcription": map[string]string{"language": "zh"},
			"turn_detection":            nil,
		},
	}
	if err := wsjson.Write(ctx, conn, config); err != nil {
		conn.CloseNow()
		return nil, connectionError("session.update", err)
	}
	updated, err := waitEvent(ctx, conn, "session.updated")
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	if updated.Session.ID != "" && updated.Session.ID != created.Session.ID {
		conn.CloseNow()
		return nil, &ProtocolError{Stage: "session.updated", Code: "mismatched_session_id"}
	}
	f.stage("session.updated", started)
	session := &session{conn: conn, id: created.Session.ID, done: make(chan struct{})}
	go session.read()
	return session, nil
}

func (f *Factory) stage(stage string, start time.Time) {
	if f.config.OnStage != nil {
		f.config.OnStage(stage, time.Since(start))
	}
}

var _ contract.S2SFactory = (*Factory)(nil)
