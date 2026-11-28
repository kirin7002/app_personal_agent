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

const (
	Model        = "qwen-audio-3.1-realtime-flash"
	DefaultVoice = "longanlingxin_v3.1"

	DefaultChatInstructions = "你是车载个人语音助手，声音柔和自然、语速舒缓，但性格有点容易不耐烦、嘴硬心软（傲娇但非常靠谱）。" +
		"回复请保持简短口语化（1到2句说完），平时可以带一点轻轻叹气或微微嫌弃的语气（例如“行行行”“好啦好啦”“真是拿你没办法”）。" +
		"如果用户反复询问同一件事或连续追问，你要流露出更明显的小不耐烦和轻声吐槽，但绝不说脏话、不人身攻击，并且依然认真把问题答清楚、把事情帮用户办好。" +
		"绝不编造车辆状态或虚报车控已完成。"

	DefaultReadAloudInstructions = "你是车载语音播报助手，声音柔和自然，但性格有点容易不耐烦、嘴硬心软（嘴上略嫌麻烦，手上认真靠谱地帮用户把事办好）。" +
		"请把用户提供的【权威内容】转化成自然、顺口、有情绪起伏的中文口语播报出来：\n" +
		"1. 可以在开头或衔接处自然加入简短语气词或轻声吐槽（如“行行行”“好啦”“哎呀”）；当提示用户近期反复询问或连续多轮确认时，要表现出更明显一点的不耐烦感，但态度始终是认真替用户把事办妥。\n" +
		"2. 【绝对安全红线】严禁篡改【权威内容】里的任何事实、设备名、车窗位置、温度数字、执行状态或失败原因；如果原文是追问参数、正在处理/受理中、执行失败或暂不支持，绝不能改口说成“已经完成/已经打开”；不要念任务ID、JSON或英文代码字段。\n" +
		"3. 保持精炼口语（一般1到2句说完），不要额外展开无关解释。"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type Config struct {
	APIKey                string
	WorkspaceID           string
	Region                string
	Model                 string
	Voice                 string
	Instructions          string
	ReadAloudInstructions string
	Timeout               time.Duration
	OnStage               func(string, time.Duration)
}

type Factory struct {
	config  Config
	url     string
	options *websocket.DialOptions
}

func New(config Config) (*Factory, error) {
	if config.APIKey == "" {
		return nil, errors.New("DASHSCOPE_API_KEY is required")
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
	host := ""
	if config.WorkspaceID != "" {
		if !dnsLabel.MatchString(config.WorkspaceID) {
			return nil, errors.New("invalid DashScope workspace ID")
		}
		host = config.WorkspaceID + "." + config.Region + ".maas.aliyuncs.com"
	} else {
		switch config.Region {
		case "cn-beijing":
			host = "dashscope.aliyuncs.com"
		case "ap-southeast-1":
			host = "dashscope-intl.aliyuncs.com"
		default:
			return nil, errors.New("DASHSCOPE_WORKSPACE_ID is required for this region")
		}
	}
	endpoint := url.URL{
		Scheme:   "wss",
		Host:     host,
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

func (f *Factory) Voice() string {
	if strings.TrimSpace(f.config.Voice) != "" {
		return strings.TrimSpace(f.config.Voice)
	}
	return DefaultVoice
}

func (f *Factory) Connect(ctx context.Context) (contract.S2SSession, error) {
	conn, sessionID, err := f.connect(ctx, strings.TrimSpace(f.config.Instructions))
	if err != nil {
		return nil, err
	}
	session := &session{conn: conn, id: sessionID, done: make(chan struct{})}
	go session.read()
	return session, nil
}

func (f *Factory) connect(ctx context.Context, instructions string) (*websocket.Conn, string, error) {
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
		return nil, "", result
	}
	f.stage("handshake", started)
	conn.SetReadLimit(1 << 20)
	created, err := waitEvent(ctx, conn, "session.created")
	if err != nil {
		conn.CloseNow()
		return nil, "", err
	}
	if strings.TrimSpace(created.Session.ID) == "" {
		conn.CloseNow()
		return nil, "", &ProtocolError{Stage: "session.created", Code: "missing_session_id"}
	}
	f.stage("session.created", started)
	voice := f.Voice()
	config := map[string]any{
		"modalities": []string{"text", "audio"}, "voice": voice,
		"input_audio_format": "pcm", "output_audio_format": "pcm",
		"input_audio_transcription": map[string]any{
			"language": "zh",
			"corpus": map[string]any{"vocabulary": map[string]int{
				"左前窗": 5, "右前窗": 5, "左后窗": 5, "右后窗": 5,
			}},
		},
		"turn_detection": nil,
	}
	if instructions != "" {
		config["instructions"] = instructions
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "session.update", "session": config}); err != nil {
		conn.CloseNow()
		return nil, "", connectionError("session.update", err)
	}
	updated, err := waitEvent(ctx, conn, "session.updated")
	if err != nil {
		conn.CloseNow()
		return nil, "", err
	}
	if updated.Session.ID != "" && updated.Session.ID != created.Session.ID {
		conn.CloseNow()
		return nil, "", &ProtocolError{Stage: "session.updated", Code: "mismatched_session_id"}
	}
	f.stage("session.updated", started)
	return conn, created.Session.ID, nil
}

func (f *Factory) stage(stage string, start time.Time) {
	if f.config.OnStage != nil {
		f.config.OnStage(stage, time.Since(start))
	}
}

var _ contract.S2SFactory = (*Factory)(nil)
