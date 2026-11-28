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
	"sync"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
)

func selectLocalFallbackTTS() (contract.SpeechSynthesizer, string) {
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("say"); err == nil {
			return macSayTTS{voice: "Tingting"}, "say"
		}
	}
	return toneTTS{}, "tone"
}

func selectDemoTTS(mode string, qwenFactory *qwen.Factory) (contract.SpeechSynthesizer, string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		fallback, fallbackName := selectLocalFallbackTTS()
		if qwenFactory != nil {
			return newQwenReadAloudTTS(qwenFactory, fallback), "qwen", nil
		}
		return fallback, fallbackName, nil
	case "say":
		if _, err := exec.LookPath("say"); err != nil {
			return nil, "", errors.New("TTS_MODE=say requires the macOS 'say' command")
		}
		return macSayTTS{voice: "Tingting"}, "say", nil
	case "qwen":
		if qwenFactory == nil {
			return nil, "", errors.New("TTS_MODE=qwen requires DASHSCOPE_API_KEY")
		}
		fallback, _ := selectLocalFallbackTTS()
		return newQwenReadAloudTTS(qwenFactory, fallback), "qwen", nil
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

type recentSpeechEntry struct {
	key string
	at  time.Time
}

type qwenReadAloudTTS struct {
	factory  *qwen.Factory
	fallback contract.SpeechSynthesizer
	now      func() time.Time

	mu     sync.Mutex
	recent []recentSpeechEntry
}

func newQwenReadAloudTTS(factory *qwen.Factory, fallback contract.SpeechSynthesizer) *qwenReadAloudTTS {
	return &qwenReadAloudTTS{
		factory:  factory,
		fallback: fallback,
		now:      time.Now,
	}
}

func normalizeSpeechKey(text string) string {
	r := strings.NewReplacer(" ", "", "\n", "", "\t", "", "。", "", "，", "", "？", "", "！", "", "?", "", "!", "", "：", "", ":", "")
	return strings.TrimSpace(r.Replace(text))
}

var pushyToneKeywords = []string{
	"快点", "赶紧", "快一点", "怎么这么慢", "好慢", "磨蹭", "墨迹",
	"听不懂", "听不清吗", "耳朵", "烦死", "真烦", "啰嗦", "废话",
	"到底行不行", "行不行啊", "搞什么", "干嘛呢", "干什么吃的", "笨", "傻", "有病", "气死", "急死了",
}

func isPushyOrHarshTone(userText string) bool {
	trimmed := strings.TrimSpace(userText)
	if trimmed == "" {
		return false
	}
	for _, kw := range pushyToneKeywords {
		if strings.Contains(trimmed, kw) {
			return true
		}
	}
	return false
}

func (t *qwenReadAloudTTS) nextStyleHint(ctx context.Context, text string) string {
	clock := time.Now
	if t.now != nil {
		clock = t.now
	}
	now := clock().UTC()
	cutoff := now.Add(-2 * time.Minute)
	key := normalizeSpeechKey(text)
	userText := contract.UserUtteranceFromContext(ctx)
	pushyTone := isPushyOrHarshTone(userText)

	t.mu.Lock()
	kept := t.recent[:0]
	repeatCount := 0
	for _, entry := range t.recent {
		if entry.at.Before(cutoff) {
			continue
		}
		kept = append(kept, entry)
		if key != "" && entry.key == key {
			repeatCount++
		}
	}
	kept = append(kept, recentSpeechEntry{key: key, at: now})
	if len(kept) > 16 {
		kept = kept[len(kept)-16:]
	}
	t.recent = kept
	recentTurns := len(kept)
	t.mu.Unlock()

	if pushyTone && repeatCount >= 1 {
		return fmt.Sprintf("【当前语境】用户刚才语气比较冲或催促，而且同类问题已是近期第 %d 次反复触发。请在柔和声线底色上自然流露出明显一点的无奈和不耐烦，轻轻回一句嘴，但依然稳妥准确地把关键事实和结果交代清楚；不要套用固定开头词。", repeatCount+1)
	}
	if pushyTone {
		return "【当前语境】用户刚才说话语气有些催促或不太客气。可以在柔和声线里带出一点点被催烦了的小不耐烦，但保持礼貌克制，依然认认真真把事情办妥并说清事实；不要用固定套话开头。"
	}
	if repeatCount >= 1 {
		return fmt.Sprintf("【当前语境】同类内容在近期已被反复触发（第 %d 次）。可以自然带出一点被问烦了的轻微不耐烦或无奈，但依然保持柔和声线并准确传达全部关键信息；换一种自然说法，不要重复固定开头词。", repeatCount+1)
	}
	if recentTurns >= 4 {
		return fmt.Sprintf("【当前语境】短时间内已经连续来回沟通了 %d 轮，略显折腾。语气里可以带一丝很轻的嫌麻烦感，但依然靠谱、耐心地把关键信息交代清楚；不要用固定口头禅开头。", recentTurns)
	}
	return "【当前语境】普通正常对话。请用柔和、自然、平和干脆的日常口语直接表达重点，不要表现不耐烦，也不要加多余的开头口头禅。"
}

func (t *qwenReadAloudTTS) Synthesize(ctx context.Context, text string, emit func([]byte) error) error {
	hint := t.nextStyleHint(ctx, text)
	emitted := false
	_, err := t.factory.SpeakWithStyle(ctx, text, hint, func(pcm []byte) error {
		emitted = true
		return emit(pcm)
	})
	if err != nil && !emitted && t.fallback != nil {
		return t.fallback.Synthesize(ctx, text, emit)
	}
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
