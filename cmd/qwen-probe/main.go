// qwen-probe verifies final ASR before response.create, then records streaming PCM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/realtime/qwen"
)

func main() {
	log.SetFlags(0)
	car := flag.String("car", "", "16 kHz mono PCM16LE vehicle utterance")
	carWant := flag.String("car-expect", "关.*车?窗|车?窗.*关", "regexp for the final vehicle transcript")
	chat := flag.String("chat", "", "16 kHz mono PCM16LE chat utterance")
	chatWant := flag.String("chat-expect", "", "optional regexp for the final chat transcript")
	output := flag.String("out", "reply.pcm", "24 kHz mono PCM16LE output")
	readText := flag.String("read-text", "", "probe whether Qwen speaks the exact supplied text")
	cancelAfterFirst := flag.Bool("cancel-after-first", false, "cancel after the first audio chunk")
	flag.Parse()
	if err := run(*car, *carWant, *chat, *chatWant, *output, *readText, *cancelAfterFirst); err != nil {
		log.Fatal(err)
	}
}

func run(car, carWant, chat, chatWant, output, readText string, cancelAfterFirst bool) error {
	if readText == "" && (car == "" || chat == "") {
		return errors.New("provide both -car and -chat PCM16LE samples, or -read-text")
	}
	factory, err := qwen.New(qwen.Config{
		APIKey: os.Getenv("DASHSCOPE_API_KEY"), WorkspaceID: os.Getenv("DASHSCOPE_WORKSPACE_ID"),
		Region: os.Getenv("DASHSCOPE_REGION"), Model: os.Getenv("DASHSCOPE_MODEL"),
		OnStage: func(stage string, elapsed time.Duration) {
			log.Printf("%s elapsed=%s", stage, elapsed.Round(time.Millisecond))
		},
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if readText != "" {
		return probeReadAloud(ctx, factory, readText, output)
	}
	session, err := factory.Connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	vehicle, err := submit(ctx, session, car, carWant)
	if err != nil {
		return err
	}
	dialogue, err := submit(ctx, session, chat, chatWant)
	if err != nil {
		return err
	}
	if vehicle.InputItemID == dialogue.InputItemID || vehicle.ProviderSessionID != dialogue.ProviderSessionID {
		return errors.New("expected distinct input items on the same session")
	}
	start := time.Now()
	response, err := session.CreateResponse(ctx, dialogue.InputItemID)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	bytes, chunks := 0, 0
	for {
		pcm, err := response.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if chunks == 0 {
			log.Printf("response.audio.delta response_id=%s first_audio=%s", response.ID(), time.Since(start).Round(time.Millisecond))
		}
		if _, err := file.Write(pcm); err != nil {
			return err
		}
		bytes += len(pcm)
		chunks++
		if cancelAfterFirst {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := session.CancelResponse(ctx); err != nil {
				return err
			}
			for err == nil {
				_, err = response.Next(ctx)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, context.Canceled) {
				return fmt.Errorf("response ended before cancellation was confirmed: %w", err)
			}
			log.Print("response.done status=cancelled")
			break
		}
	}
	if chunks == 0 {
		return errors.New("no streamed audio received")
	}
	log.Printf("PCM16LE 24000 Hz mono chunks=%d bytes=%d", chunks, bytes)
	return file.Close()
}

func probeReadAloud(ctx context.Context, factory *qwen.Factory, text, output string) error {
	var audio []byte
	transcript, err := factory.ProbeReadAloud(ctx, text, func(pcm []byte) error {
		if len(audio)+len(pcm) > 24000*2*60 {
			return errors.New("read-aloud output exceeds 60 seconds")
		}
		audio = append(audio, pcm...)
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("read-aloud transcript=%q expected=%q PCM16LE_bytes=%d", transcript, text, len(audio))
	if transcript != text || len(audio) == 0 {
		return errors.New("read-aloud requires a verbatim transcript and nonempty audio")
	}
	return os.WriteFile(output, audio, 0600)
}

func submit(ctx context.Context, session contract.S2SSession, fileName, pattern string) (contract.FinalTranscript, error) {
	var result contract.FinalTranscript
	pcm, err := os.ReadFile(fileName)
	if err != nil {
		return result, err
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 16000*2*30 {
		return result, errors.New("fixture must contain up to 30 seconds of raw PCM16LE")
	}
	for len(pcm) > 0 {
		count := min(len(pcm), 3200)
		if err := session.AppendAudio(ctx, pcm[:count]); err != nil {
			return result, err
		}
		pcm = pcm[count:]
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		}
	}
	start := time.Now()
	result, err = session.Commit(ctx)
	if err != nil {
		return result, err
	}
	match, err := regexp.MatchString(pattern, result.Text)
	if err != nil {
		return result, err
	}
	log.Printf("transcript.final item_id=%s commit_to_final=%s text=%q", result.InputItemID, time.Since(start).Round(time.Millisecond), result.Text)
	if !match {
		return result, errors.New("final transcript did not match expectation")
	}
	return result, nil
}
