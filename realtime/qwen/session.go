package qwen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
)

type commitResult struct {
	final contract.FinalTranscript
	err   error
}

type commitState struct {
	itemID string
	early  map[string]event
	result chan commitResult
}

type responseState struct {
	id      string
	created chan struct{}
	done    chan struct{}
	chunks  chan []byte
	err     error
}

type session struct {
	conn *websocket.Conn
	id   string
	done chan struct{}

	mu         sync.Mutex
	buffered   bool
	commit     *commitState
	lastItemID string
	response   *responseState
	terminal   error
}

func (s *session) Close() error { return s.conn.CloseNow() }

func (s *session) read() {
	ctx := context.Background()
	for {
		var current event
		if err := wsjson.Read(ctx, s.conn, &current); err != nil {
			s.finish(connectionError("read", err))
			return
		}
		s.mu.Lock()
		err := s.handle(current)
		s.mu.Unlock()
		if err != nil {
			s.finish(err)
			return
		}
	}
}

func (s *session) finish(err error) {
	s.mu.Lock()
	s.terminal = err
	s.mu.Unlock()
	_ = s.Close()
	close(s.done)
}

func (s *session) handle(e event) error {
	switch e.Type {
	case "error":
		return e.failure("provider")
	case "input_audio_buffer.committed":
		if s.commit == nil || e.ItemID == s.lastItemID || e.ItemID == "" {
			return nil
		}
		if s.commit.itemID != "" && s.commit.itemID != e.ItemID {
			return &ProtocolError{Stage: "commit", Code: "conflicting_item_id"}
		}
		s.commit.itemID = e.ItemID
		if early, ok := s.commit.early[e.ItemID]; ok {
			s.completeTranscript(early)
		}
	case "conversation.item.input_audio_transcription.completed", "conversation.item.input_audio_transcription.failed":
		if s.commit == nil || e.ItemID == "" || e.ItemID == s.lastItemID {
			return nil
		}
		if s.commit.itemID == e.ItemID {
			s.completeTranscript(e)
		} else if s.commit.itemID == "" && len(s.commit.early) < 8 {
			s.commit.early[e.ItemID] = e
		}
	case "response.created":
		if s.response == nil {
			return &ProtocolError{Stage: "response.created", EventID: e.EventID, Code: "unsolicited_response"}
		}
		if r := s.response; r != nil && r.id == "" && e.Response.ID != "" {
			r.id = e.Response.ID
			close(r.created)
		}
	case "response.audio.delta":
		if r := s.response; r != nil && r.id != "" && r.id == e.ResponseID {
			var pcm []byte
			if err := json.Unmarshal(e.Delta, &pcm); err != nil || len(pcm) == 0 || len(pcm)%2 != 0 {
				return &ProtocolError{Stage: "response.audio.delta", Code: "invalid_pcm"}
			}
			select {
			case r.chunks <- pcm:
			default:
				return &ProtocolError{Stage: "response.audio.delta", Code: "consumer_too_slow"}
			}
		}
	case "response.done":
		if r := s.response; r != nil && r.id != "" && r.id == e.Response.ID {
			switch e.Response.Status {
			case "completed":
			case "cancelled", "canceled":
				r.err = context.Canceled
			default:
				r.err = &ProtocolError{Stage: "response.done", Code: e.Response.Status}
			}
			close(r.chunks)
			close(r.done)
			s.response = nil
		}
	}
	return nil
}

// Caller holds s.mu.
func (s *session) completeTranscript(e event) {
	s.lastItemID = e.ItemID
	result := commitResult{}
	switch {
	case e.Type == "conversation.item.input_audio_transcription.failed":
		result.err = e.failure("transcription")
	case strings.TrimSpace(e.Transcript) == "":
		result.err = &ProtocolError{Stage: "transcription", EventID: e.EventID, Code: "empty_transcript"}
	default:
		result.final = contract.FinalTranscript{ProviderSessionID: s.id, InputItemID: e.ItemID, Text: e.Transcript}
	}
	s.commit.result <- result
	s.commit = nil
}

func (s *session) transportError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *session) write(ctx context.Context, stage string, message any) error {
	if err := wsjson.Write(ctx, s.conn, message); err != nil {
		_ = s.Close()
		return connectionError(stage, err)
	}
	return nil
}

func (s *session) AppendAudio(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 1<<16 {
		return errors.New("audio append requires a nonempty PCM16 chunk of at most 64 KiB")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commit != nil || s.response != nil {
		return errors.New("input buffer is busy")
	}
	if err := s.write(ctx, "append", map[string]any{"type": "input_audio_buffer.append", "audio": pcm}); err != nil {
		return err
	}
	s.buffered = true
	return nil
}

func (s *session) Commit(ctx context.Context) (contract.FinalTranscript, error) {
	s.mu.Lock()
	if !s.buffered || s.commit != nil || s.response != nil {
		s.mu.Unlock()
		return contract.FinalTranscript{}, errors.New("no uncommitted audio available")
	}
	pending := &commitState{result: make(chan commitResult, 1), early: make(map[string]event)}
	s.commit = pending
	s.buffered = false
	s.mu.Unlock()
	if err := s.write(ctx, "commit", map[string]string{"type": "input_audio_buffer.commit"}); err != nil {
		return contract.FinalTranscript{}, err
	}
	select {
	case result := <-pending.result:
		return result.final, result.err
	case <-s.done:
		return contract.FinalTranscript{}, s.transportError()
	case <-ctx.Done():
		_ = s.Close()
		return contract.FinalTranscript{}, connectionError("transcription", ctx.Err())
	}
}

func (s *session) CreateResponse(ctx context.Context, inputItemID string) (contract.S2SResponse, error) {
	s.mu.Lock()
	if inputItemID == "" || inputItemID != s.lastItemID || s.commit != nil || s.response != nil || s.buffered {
		s.mu.Unlock()
		return nil, errors.New("response requires the latest committed input item and an idle session")
	}
	r := &responseState{created: make(chan struct{}), done: make(chan struct{}), chunks: make(chan []byte, 128)}
	s.response = r
	s.mu.Unlock()
	if err := s.write(ctx, "response.create", map[string]string{"type": "response.create"}); err != nil {
		return nil, err
	}
	select {
	case <-r.created:
		return &response{state: r, session: s}, nil
	case <-s.done:
		return nil, s.transportError()
	case <-ctx.Done():
		_ = s.Close()
		return nil, connectionError("response.created", ctx.Err())
	}
}

func (s *session) CancelResponse(ctx context.Context) error {
	s.mu.Lock()
	r := s.response
	if r == nil {
		s.mu.Unlock()
		return nil
	}
	err := s.write(ctx, "response.cancel", map[string]string{"type": "response.cancel"})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-r.done:
		return nil
	case <-s.done:
		return s.transportError()
	case <-ctx.Done():
		_ = s.Close()
		return connectionError("response.cancel", ctx.Err())
	}
}

type response struct {
	state   *responseState
	session *session
}

func (r *response) ID() string { return r.state.id }

func (r *response) Next(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case chunk, ok := <-r.state.chunks:
		if ok {
			return chunk, nil
		}
		if r.state.err != nil {
			return nil, r.state.err
		}
		return nil, io.EOF
	case <-r.session.done:
		return nil, r.session.transportError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ contract.S2SSession = (*session)(nil)
