// car-sim submits recorded car turns to the voice gateway and saves playback PCM.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
)

func main() {
	address := flag.String("url", "ws://127.0.0.1:8080/voice/audio", "voice WebSocket URL")
	session := flag.String("session", "car-test", "business session ID")
	vehicle := flag.String("vehicle", "sim-car", "selected vehicle ID")
	output := flag.String("out", ".", "output directory for 24 kHz PCM files")
	waitResult := flag.Bool("wait-result", false, "wait for the completed vehicle result and its playback after the last turn")
	prefix := flag.String("turn-prefix", rand.Text(), "prefix for stable client turn IDs")
	flag.Parse()
	if err := run(*address, *session, *vehicle, *output, *prefix, *waitResult, flag.Args()); err != nil {
		log.Fatal(err)
	}
}

func run(address, session, vehicle, output, prefix string, waitResult bool, inputs []string) error {
	token := os.Getenv("CAR_SIM_TOKEN")
	if token == "" || len(inputs) == 0 {
		return fmt.Errorf("set CAR_SIM_TOKEN and provide one or more raw 16 kHz PCM files")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(inputs))*90*time.Second)
	defer cancel()
	socket, handshake, err := websocket.Dial(ctx, address, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	if err != nil {
		if handshake != nil {
			return fmt.Errorf("voice handshake returned HTTP %d", handshake.StatusCode)
		}
		return fmt.Errorf("voice handshake failed: %w", err)
	}
	defer socket.CloseNow()
	socket.SetReadLimit(128 << 10)
	format := contract.InputFormat()
	if err := wsjson.Write(ctx, socket, contract.CarFrame{Type: contract.SessionOpen,
		SessionID: session, VehicleID: vehicle, InputFormat: &format}); err != nil {
		return err
	}
	var ready contract.CarFrame
	if err := wsjson.Read(ctx, socket, &ready); err != nil {
		return err
	}
	if ready.Type != contract.SessionReady || ready.OutputFormat == nil || *ready.OutputFormat != contract.OutputFormat() {
		return fmt.Errorf("voice session unavailable: %s", ready.Reason)
	}
	for i, input := range inputs {
		turnID := fmt.Sprintf("%s-%d", prefix, i+1)
		if err := sendRecording(ctx, socket, ready.ConnectionGeneration, turnID, input); err != nil {
			return err
		}
		if err := record(ctx, socket, output, i+1, waitResult && i == len(inputs)-1); err != nil {
			return err
		}
	}
	return nil
}

func sendRecording(ctx context.Context, socket *websocket.Conn, generation uint64, turnID, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data)%2 != 0 || len(data) > 16000*2*30 {
		return fmt.Errorf("%s: expected raw 16 kHz mono PCM16LE up to 30 seconds", file)
	}
	frame := contract.CarFrame{Type: contract.SpeechStarted, VoiceTurnID: turnID, ConnectionGeneration: generation}
	if err := wsjson.Write(ctx, socket, frame); err != nil {
		return err
	}
	frame.Type = contract.AudioInput
	for len(data) > 0 {
		chunk := min(len(data), 1600)
		frame.Sequence++
		frame.Audio = data[:chunk]
		if err := wsjson.Write(ctx, socket, frame); err != nil {
			return err
		}
		data = data[chunk:]
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	frame.Type, frame.Audio, frame.Sequence = contract.SpeechEnded, nil, 0
	return wsjson.Write(ctx, socket, frame)
}

func record(ctx context.Context, socket *websocket.Conn, output string, turn int, waitResult bool) error {
	var id string
	var generation, sequence uint64
	var gotResult bool
	var pcm []byte
	part := 0
	for {
		var frame contract.CarFrame
		if err := wsjson.Read(ctx, socket, &frame); err != nil {
			return err
		}
		switch frame.Type {
		case contract.SessionError, contract.SessionReconnecting:
			return fmt.Errorf("audio requires recapture: %s", frame.Reason)
		case contract.SpeechReplyEvent:
			if r := frame.Reply; r != nil {
				log.Printf("speech.reply voice_turn_id=%s request_id=%s run_id=%s task_id=%s command_id=%s status=%s text=%q",
					r.VoiceTurnID, r.RequestID, r.RunID, r.TaskID, r.CommandID, r.Status, r.Text)
			}
		case contract.VehicleResult:
			if result := frame.Result; result != nil {
				log.Printf("vehicle.result event_id=%s status=%s task_id=%s command_id=%s",
					result.EventID, result.Status, result.TaskID, result.CommandID)
				gotResult = result.Status == contract.Completed
			}
		case contract.PlaybackStart:
			id, generation, sequence, pcm = frame.PlaybackID, frame.PlaybackGeneration, 0, nil
		case contract.AudioOutput:
			if frame.PlaybackID != id || frame.PlaybackGeneration != generation || frame.Sequence != sequence+1 {
				return fmt.Errorf("invalid playback audio sequence")
			}
			sequence = frame.Sequence
			if len(pcm)+len(frame.Audio) > 24000*2*60 {
				return fmt.Errorf("playback exceeds 60 seconds")
			}
			pcm = append(pcm, frame.Audio...)
		case contract.PlaybackStop:
			if frame.PlaybackID != id || frame.PlaybackGeneration != generation || frame.Reason != "completed" || len(pcm) == 0 {
				return fmt.Errorf("playback failed: %s", frame.Reason)
			}
			part++
			path, replayed, err := savePlayback(output, id, turn, part, pcm)
			if err != nil {
				return err
			}
			if replayed {
				log.Printf("playback=%s already saved path=%s", id, path)
			} else {
				log.Printf("playback=%s chunks=%d bytes=%d path=%s", id, sequence, len(pcm), path)
			}
			if err := wsjson.Write(ctx, socket, contract.CarFrame{Type: contract.PlaybackAck,
				PlaybackID: id, PlaybackGeneration: generation}); err != nil {
				return err
			}
			if !waitResult || gotResult {
				return nil
			}
		}
	}
}

// Result audio uses its stable ID as both the output filename and the local
// consumption record, including when the network drops before the ACK.
func savePlayback(output, id string, turn, part int, pcm []byte) (path string, replayed bool, err error) {
	hash, result := strings.CutPrefix(id, "result-")
	if !result {
		path = filepath.Join(output, fmt.Sprintf("turn-%d-playback-%d.pcm", turn, part))
		return path, false, os.WriteFile(path, pcm, 0600)
	}
	if raw, err := hex.DecodeString(hash); err != nil || len(raw) != 32 {
		return "", false, errors.New("invalid result playback ID")
	}
	path = filepath.Join(output, id+".pcm")
	if _, err := os.Stat(path); err == nil {
		return path, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	file, err := os.CreateTemp(output, ".result-*.pcm")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(pcm)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", false, err
	}
	// Linking publishes the complete file without overwriting an existing result.
	if err := os.Link(file.Name(), path); errors.Is(err, os.ErrExist) {
		return path, true, nil
	} else if err != nil {
		return "", false, err
	}
	return path, false, nil
}
