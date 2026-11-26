package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kirin7002/app_personal_agent/contract"
	"github.com/kirin7002/app_personal_agent/vehicle/turns"
	"github.com/kirin7002/app_personal_agent/voice/gateway"
)

type carAuth struct {
	token string
	scope contract.Scope
}

func (auth carAuth) Authenticate(_ context.Context, token, sessionID, vehicleID string) (contract.Scope, error) {
	if token != auth.token || sessionID != auth.scope.SessionID || vehicleID != auth.scope.VehicleID {
		return contract.Scope{}, errors.New("simulated car unavailable")
	}
	return auth.scope, nil
}

type scenarioEvidence struct {
	taskID    string
	commandID string
	result    *contract.VehicleResultEvent
}

func runScenarioA(t *testing.T, ctx context.Context, svc *turns.Service, results contract.ResultReader, media contract.S2SFactory,
	tts contract.SpeechSynthesizer, scope contract.Scope, live bool, confirm func(string)) scenarioEvidence {
	t.Helper()
	auth := carAuth{token: rand.Text(), scope: scope}
	service := &gateway.Gateway{Auth: auth, Media: media, Turns: svc, Pending: svc, Results: results, TTS: tts}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	car := connectCar(t, ctx, server.URL, auth.token, scope)

	firstPCM, secondPCM := []byte{1, 0}, []byte{2, 0}
	if live {
		firstPCM = sample(t, "close-window.pcm")
		secondPCM = sample(t, "rear-left.pcm")
	}
	car.say("request-close", firstPCM, live)
	first := car.hear()
	if first.reply == nil || first.reply.Status != contract.PendingReply || first.reply.RunID == "" ||
		first.reply.TaskID == "" || first.reply.CommandID != "" || !strings.Contains(first.reply.Text, "窗") || first.result != nil {
		t.Fatalf("first turn did not produce a persisted pending task and spoken clarification: %+v", first.reply)
	}
	stored, err := svc.Store.Get(ctx, scope, first.reply.VoiceTurnID)
	if err != nil || stored.Context.FinalTranscript.InputItemID == "" ||
		!strings.Contains(stored.Context.FinalTranscript.Text, "关") || !strings.Contains(stored.Context.FinalTranscript.Text, "窗") {
		t.Fatalf("first final transcript missing or wrong: %v", err)
	}
	t.Logf("clarification transcript=%q voice_turn_id=%s request_id=%s run_id=%s task_id=%s playback_id=%s audio_bytes=%d",
		stored.Context.FinalTranscript.Text, first.reply.VoiceTurnID, first.reply.RequestID, first.reply.RunID,
		first.reply.TaskID, first.playbackID, len(first.pcm))

	car.say("answer-rear-left", secondPCM, live)
	progress := car.hear()
	if progress.reply == nil || progress.reply.TaskID != first.reply.TaskID || progress.reply.CommandID == "" ||
		progress.reply.RunID == first.reply.RunID || progress.reply.RequestID == first.reply.RequestID ||
		progress.reply.VoiceTurnID == first.reply.VoiceTurnID {
		t.Fatalf("second turn did not continue the original task with a new Run: %+v", progress.reply)
	}
	if progress.result != nil || (progress.reply.Status != contract.PendingDispatch && progress.reply.Status != contract.Executing) {
		t.Fatalf("gateway reported success before the vehicle completed: %+v", progress.reply)
	}
	confirm(progress.reply.CommandID)
	confirmation := car.hear()
	result := confirmation.result
	if result == nil || result.Status != contract.Completed || result.TaskID != first.reply.TaskID ||
		result.CommandID != progress.reply.CommandID || result.RequestID != progress.reply.RequestID || result.EventID == "" ||
		!strings.HasPrefix(confirmation.playbackID, "result-") {
		t.Fatalf("confirmed vehicle result did not produce its own playback: %+v", result)
	}
	stored, err = svc.Store.Get(ctx, scope, progress.reply.VoiceTurnID)
	if err != nil || !strings.Contains(stored.Context.FinalTranscript.Text, "左后") ||
		len(stored.Context.Candidates) != 1 || stored.Context.Candidates[0].TaskID != first.reply.TaskID {
		t.Fatalf("second final transcript or trusted task snapshot missing: %v", err)
	}
	t.Logf("confirmation transcript=%q voice_turn_id=%s request_id=%s run_id=%s task_id=%s command_id=%s event_id=%s playback_id=%s audio_bytes=%d",
		stored.Context.FinalTranscript.Text, progress.reply.VoiceTurnID, progress.reply.RequestID, progress.reply.RunID,
		result.TaskID, result.CommandID, result.EventID, confirmation.playbackID, len(confirmation.pcm))
	if live {
		output := os.Getenv("H07_ARTIFACT_DIR")
		saveWAV(t, output, "clarification.wav", first.pcm)
		saveWAV(t, output, "vehicle-confirmation.wav", confirmation.pcm)
	}
	return scenarioEvidence{taskID: first.reply.TaskID, commandID: result.CommandID, result: result}
}

func sample(t *testing.T, name string) []byte {
	t.Helper()
	dir := os.Getenv("H07_PCM_DIR")
	if dir == "" {
		t.Fatal("set H07_PCM_DIR to a directory with close-window.pcm and rear-left.pcm")
	}
	input, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if len(input) == 0 || len(input)%2 != 0 || len(input) > 16000*2*30 {
		t.Fatalf("%s must contain up to 30 seconds of PCM16LE audio", name)
	}
	return input
}
