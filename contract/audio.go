package contract

import "context"

type AudioFormat struct {
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

func InputFormat() AudioFormat {
	return AudioFormat{Encoding: "pcm_s16le", SampleRate: 16000, Channels: 1}
}

func OutputFormat() AudioFormat {
	return AudioFormat{Encoding: "pcm_s16le", SampleRate: 24000, Channels: 1}
}

const (
	SessionOpen         = "session.open"
	SessionReady        = "session.ready"
	SessionReconnecting = "session.reconnecting"
	SessionError        = "session.error"
	SpeechStarted       = "speech.started"
	SpeechEnded         = "speech.ended"
	AudioInput          = "audio.input"
	AudioOutput         = "audio.output"
	SpeechReplyEvent    = "speech.reply"
	PlaybackStart       = "playback.start"
	PlaybackStop        = "playback.stop"
	PlaybackAck         = "playback.ack"
	VehicleResult       = "vehicle.result"
)

// CarFrame is a WebSocket JSON message; Audio is encoded as base64.
// ConnectionGeneration scopes microphone frames. PlaybackGeneration scopes audio output.
type CarFrame struct {
	Type                 string              `json:"type"`
	SessionID            string              `json:"session_id,omitempty"`
	VehicleID            string              `json:"vehicle_id,omitempty"`
	InputFormat          *AudioFormat        `json:"input_format,omitempty"`
	OutputFormat         *AudioFormat        `json:"output_format,omitempty"`
	ConnectionGeneration uint64              `json:"connection_generation,omitempty"`
	VoiceTurnID          string              `json:"voice_turn_id,omitempty"`
	PlaybackID           string              `json:"playback_id,omitempty"`
	PlaybackGeneration   uint64              `json:"playback_generation,omitempty"`
	Sequence             uint64              `json:"sequence,omitempty"`
	Audio                []byte              `json:"audio,omitempty"`
	Reply                *SpeechReply        `json:"reply,omitempty"`
	Result               *VehicleResultEvent `json:"result,omitempty"`
	Reason               string              `json:"reason,omitempty"`
}

// S2SFactory returns only after the provider confirms its audio configuration.
type S2SFactory interface {
	Connect(ctx context.Context) (S2SSession, error)
}

// S2SSession serializes input submission and response creation per connection.
// Commit waits for the final transcript; CreateResponse is only used after routing.
// The Connect context bounds the handshake. Close ends the session.
type S2SSession interface {
	AppendAudio(ctx context.Context, pcm []byte) error
	Commit(ctx context.Context) (FinalTranscript, error)
	CreateResponse(ctx context.Context, inputItemID string) (S2SResponse, error)
	CancelResponse(ctx context.Context) error
	Close() error
}

type S2SResponse interface {
	ID() string
	// Next returns PCM chunks, io.EOF on completion or a terminal error.
	Next(ctx context.Context) ([]byte, error)
}

// SpeechSynthesizer emits mono 24 kHz signed 16-bit little-endian PCM.
type SpeechSynthesizer interface {
	Synthesize(ctx context.Context, text string, emit func([]byte) error) error
}
