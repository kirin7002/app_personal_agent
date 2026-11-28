package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
)

func selectDemoTTS(mode string, qwenFactory *qwen.Factory) (contract.SpeechSynthesizer, string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("say"); err == nil {
				return macSayTTS{voice: "Tingting"}, "say", nil
			}
		}
		return toneTTS{}, "tone", nil
	case "say":
		if _, err := exec.LookPath("say"); err != nil {
			return nil, "", errors.New("TTS_MODE=say requires the macOS 'say' command")
		}
		return macSayTTS{voice: "Tingting"}, "say", nil
	case "qwen":
		if qwenFactory == nil {
			return nil, "", errors.New("TTS_MODE=qwen requires DASHSCOPE_API_KEY")
		}
		return qwenReadAloudTTS{factory: qwenFactory}, "qwen", nil
	case "tone":
		return toneTTS{}, "tone", nil
	default:
		return nil, "", fmt.Errorf("unsupported TTS_MODE %q (use auto, say, qwen, or tone)", mode)
	}
}

type macSayTTS struct {
	voice string
}

func (t macSayTTS) Synthesize(ctx context.Context, text string, emit func([]byte) error) error {
	dir, err := os.MkdirTemp("", "vehicle-demo-tts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "speech.wav")
	voice := t.voice
	if voice == "" {
		voice = "Tingting"
	}
	cmd := exec.CommandContext(ctx, "say", "-v", voice, "--data-format=LEI16@24000", "-o", path, "-f", "-")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("macOS say TTS failed: %w", err)
	}
	wav, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pcm, err := extractMono24kPCM(wav)
	if err != nil {
		return err
	}
	return emitPCMChunks(pcm, 4800, emit)
}

type qwenReadAloudTTS struct {
	factory *qwen.Factory
}

func (t qwenReadAloudTTS) Synthesize(ctx context.Context, text string, emit func([]byte) error) error {
	_, err := t.factory.ProbeReadAloud(ctx, text, emit)
	return err
}

// toneTTS is a cross-platform fallback emitting a soft 0.35s 440 Hz 24 kHz mono
// PCM16LE tone so voice playback remains protocol-valid without external TTS tools.
type toneTTS struct{}

func (toneTTS) Synthesize(_ context.Context, _ string, emit func([]byte) error) error {
	const (
		sampleRate = 24000
		samples    = 8400 // 0.35s
		freq       = 440.0
		amplitude  = 3000.0
	)
	pcm := make([]byte, samples*2)
	for i := range samples {
		v := int16(amplitude * math.Sin(2*math.Pi*freq*float64(i)/sampleRate))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	return emitPCMChunks(pcm, 4800, emit)
}

func emitPCMChunks(pcm []byte, chunkSize int, emit func([]byte) error) error {
	for len(pcm) > 0 {
		n := min(len(pcm), chunkSize)
		if err := emit(pcm[:n]); err != nil {
			return err
		}
		pcm = pcm[n:]
	}
	return nil
}

func extractMono24kPCM(wav []byte) ([]byte, error) {
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
			valid = len(data) >= 16 &&
				binary.LittleEndian.Uint16(data[:2]) == 1 &&
				binary.LittleEndian.Uint16(data[2:4]) == 1 &&
				binary.LittleEndian.Uint32(data[4:8]) == 24000 &&
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
