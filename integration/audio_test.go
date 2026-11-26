package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
)

type sampleTTS struct{}

func (sampleTTS) Synthesize(ctx context.Context, text string, emit func([]byte) error) error {
	dir, err := os.MkdirTemp("", "vehicle-tts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "speech.wav")
	cmd := exec.CommandContext(ctx, "say", "-v", "Tingting", "--data-format=LEI16@24000", "-o", path, "-f", "-")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("synthesize fixture audio: %w", err)
	}
	wav, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pcm, err := wavePCM(wav)
	if err != nil {
		return err
	}
	for len(pcm) > 0 {
		n := min(len(pcm), 4800)
		if err := emit(pcm[:n]); err != nil {
			return err
		}
		pcm = pcm[n:]
	}
	return nil
}

func wavePCM(wav []byte) ([]byte, error) {
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("invalid WAVE container")
	}
	var pcm []byte
	valid := false
	for part := wav[12:]; len(part) >= 8; {
		length := int(binary.LittleEndian.Uint32(part[4:8]))
		if length > len(part)-8 {
			return nil, io.ErrUnexpectedEOF
		}
		data := part[8 : 8+length]
		switch string(part[:4]) {
		case "fmt ":
			valid = len(data) >= 16 && binary.LittleEndian.Uint16(data[:2]) == 1 &&
				binary.LittleEndian.Uint16(data[2:4]) == 1 && binary.LittleEndian.Uint32(data[4:8]) == 24000 &&
				binary.LittleEndian.Uint16(data[14:16]) == 16
		case "data":
			pcm = data
		}
		step := 8 + length + length%2
		if step > len(part) {
			break
		}
		part = part[step:]
	}
	if !valid || len(pcm) == 0 || len(pcm)%2 != 0 {
		return nil, errors.New("speech WAVE must contain 24 kHz mono PCM16LE")
	}
	return pcm, nil
}

func saveWAV(t *testing.T, dir, name string, pcm []byte) {
	t.Helper()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var header [44]byte
	copy(header[:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(pcm)))
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], 24000)
	binary.LittleEndian.PutUint32(header[28:32], 48000)
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(pcm)))
	if err := os.WriteFile(filepath.Join(dir, name), append(header[:], pcm...), 0600); err != nil {
		t.Fatal(err)
	}
}

type carClient struct {
	socket     *websocket.Conn
	generation uint64
	ctx        context.Context
	t          *testing.T
}

func connectCar(t *testing.T, ctx context.Context, address, token string, scope contract.Scope) *carClient {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(address, "http"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	car := &carClient{socket: conn, ctx: ctx, t: t}
	format := contract.InputFormat()
	car.send(contract.CarFrame{Type: contract.SessionOpen, SessionID: scope.SessionID, VehicleID: scope.VehicleID, InputFormat: &format})
	ready := car.receive()
	if ready.Type != contract.SessionReady || ready.OutputFormat == nil || *ready.OutputFormat != contract.OutputFormat() {
		t.Fatalf("session not ready: %+v", ready)
	}
	car.generation = ready.ConnectionGeneration
	return car
}

func (c *carClient) send(frame contract.CarFrame) {
	c.t.Helper()
	if err := wsjson.Write(c.ctx, c.socket, frame); err != nil {
		c.t.Fatal(err)
	}
}

func (c *carClient) receive() contract.CarFrame {
	c.t.Helper()
	var frame contract.CarFrame
	if err := wsjson.Read(c.ctx, c.socket, &frame); err != nil {
		c.t.Fatal(err)
	}
	if frame.Type == contract.SessionError || frame.Type == contract.SessionReconnecting {
		c.t.Fatalf("unexpected session error: %+v", frame)
	}
	return frame
}

func (c *carClient) say(id string, pcm []byte, realtime bool) {
	c.t.Helper()
	frame := contract.CarFrame{Type: contract.SpeechStarted, VoiceTurnID: id, ConnectionGeneration: c.generation}
	c.send(frame)
	frame.Type = contract.AudioInput
	for len(pcm) > 0 {
		n := min(len(pcm), 3200)
		frame.Sequence++
		frame.Audio = pcm[:n]
		c.send(frame)
		pcm = pcm[n:]
		if realtime {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-c.ctx.Done():
				timer.Stop()
				c.t.Fatal(c.ctx.Err())
			}
		}
	}
	frame.Type, frame.Sequence, frame.Audio = contract.SpeechEnded, 0, nil
	c.send(frame)
}

type heard struct {
	firstAudioAt time.Time
	playbackID   string
	reply        *contract.SpeechReply
	result       *contract.VehicleResultEvent
	pcm          []byte
}

func (c *carClient) hear() heard {
	c.t.Helper()
	var heard heard
	var generation, seq uint64
	for {
		frame := c.receive()
		switch frame.Type {
		case contract.SpeechReplyEvent:
			heard.reply = frame.Reply
		case contract.VehicleResult:
			heard.result = frame.Result
		case contract.PlaybackStart:
			heard.playbackID, generation = frame.PlaybackID, frame.PlaybackGeneration
		case contract.AudioOutput:
			if frame.PlaybackID != heard.playbackID || frame.PlaybackGeneration != generation || frame.Sequence != seq+1 {
				c.t.Fatalf("out-of-order playback frame: %+v", frame)
			}
			seq = frame.Sequence
			if seq == 1 {
				heard.firstAudioAt = time.Now()
			}
			heard.pcm = append(heard.pcm, frame.Audio...)
		case contract.PlaybackStop:
			if frame.PlaybackID != heard.playbackID || frame.PlaybackGeneration != generation || frame.Reason != "completed" ||
				len(heard.pcm) < 2 || bytes.Equal(heard.pcm, make([]byte, len(heard.pcm))) {
				c.t.Fatalf("incomplete or silent playback: id=%s bytes=%d reason=%s", heard.playbackID, len(heard.pcm), frame.Reason)
			}
			c.send(contract.CarFrame{Type: contract.PlaybackAck, PlaybackID: heard.playbackID, PlaybackGeneration: generation})
			return heard
		}
	}
}
