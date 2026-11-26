package gateway

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/kirin7002/app_personal_agent/contract"
)

type capture struct {
	clientID  string
	turnID    string
	requestID string
	sequence  uint64
	bytes     int
	started   time.Time
}

type job struct {
	ctx         context.Context
	cancel      context.CancelFunc
	kind        string
	capture     capture
	nativeReady bool
}

type jobEvent struct {
	job        *job
	transcript *contract.FinalTranscript
	routed     bool
	vehicle    bool
	reply      *contract.SpeechReply
	playbackID string
	pcm        []byte
	finished   bool
	err        error
}

type playback struct {
	id         string
	generation uint64
	sequence   uint64
	started    time.Time
}

type connection struct {
	gateway    *Gateway
	receipts   contract.ResultPlaybackReceipts
	state      *sessionState
	ctx        context.Context
	cancel     context.CancelFunc
	socket     *websocket.Conn
	scope      contract.Scope
	media      contract.S2SSession
	generation uint64
	frames     chan contract.CarFrame
	work       chan jobEvent
	recording  *capture
	current    *job
	playing    *playback
	signalled  map[string]bool
	attempted  map[string]bool
	items      map[string]bool
}

func (c *connection) run() {
	defer func() {
		c.interrupt(false)
		if c.media != nil {
			_ = c.media.Close()
		}
	}()
	go c.read()
	if !c.connect() {
		return
	}
	c.refreshResults(true)
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame, ok := <-c.frames:
			if !ok {
				return
			}
			if err := c.onFrame(frame); err != nil && !c.recoverMedia(err) {
				return
			}
		case event := <-c.work:
			if event.job == c.current {
				c.onWork(event)
			}
		case <-tick.C:
			if c.recording == nil && c.current == nil {
				c.refreshResults(true)
			}
		}
	}
}

func (c *connection) read() {
	defer close(c.frames)
	for {
		var frame contract.CarFrame
		if err := wsjson.Read(c.ctx, c.socket, &frame); err != nil {
			return
		}
		select {
		case c.frames <- frame:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *connection) write(frame contract.CarFrame) bool {
	writeCtx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	if err := wsjson.Write(writeCtx, c.socket, frame); err != nil {
		c.cancel()
		return false
	}
	return true
}

func (c *connection) connect() bool {
	start := time.Now()
	media, err := c.gateway.Media.Connect(c.ctx)
	if err != nil {
		if c.gateway.Logger != nil {
			c.gateway.Logger.Warn("provider connection failed", "error", err)
		}
		c.write(contract.CarFrame{Type: contract.SessionError, Reason: "provider_unavailable"})
		c.cancel()
		return false
	}
	c.media = media
	c.items = make(map[string]bool)
	c.generation = c.gateway.nextGeneration.Add(1)
	input, output := contract.InputFormat(), contract.OutputFormat()
	c.gateway.timing("session.ready", start)
	return c.write(contract.CarFrame{Type: contract.SessionReady, SessionID: c.scope.SessionID, VehicleID: c.scope.VehicleID,
		ConnectionGeneration: c.generation, InputFormat: &input, OutputFormat: &output})
}

func (c *connection) recoverMedia(err error) bool {
	if c.gateway.Logger != nil {
		c.gateway.Logger.Warn("audio turn restarting", "error", err)
	}
	c.recording = nil
	if c.media != nil {
		_ = c.media.Close()
	}
	c.write(contract.CarFrame{Type: contract.SessionReconnecting, Reason: "recapture"})
	return c.connect()
}

func (c *connection) onFrame(frame contract.CarFrame) error {
	if frame.Type == contract.PlaybackAck {
		// Acknowledgments are accepted only after all frames have been sent.
		if c.playing == nil || c.playing.id != frame.PlaybackID {
			c.settlePlayback(frame.PlaybackID, frame.PlaybackGeneration)
		}
		return nil
	}
	if frame.ConnectionGeneration != c.generation || frame.VoiceTurnID == "" || len(frame.VoiceTurnID) > 128 {
		return nil
	}
	switch frame.Type {
	case contract.SpeechStarted:
		if c.state.used(frame.VoiceTurnID) || (c.recording != nil && c.recording.clientID == frame.VoiceTurnID) {
			return nil
		}
		wasRecording := c.recording != nil
		err := c.interrupt(true)
		if wasRecording || err != nil {
			return errors.New("incomplete audio interrupted")
		}
		turnID := digest(c.scope.UserID, c.scope.SessionID, c.scope.VehicleID, frame.VoiceTurnID)
		c.recording = &capture{clientID: frame.VoiceTurnID, turnID: turnID, requestID: "voice-" + turnID, started: time.Now()}
	case contract.AudioInput:
		recording := c.recording
		if recording == nil || recording.clientID != frame.VoiceTurnID || frame.Sequence <= recording.sequence {
			return nil
		}
		if frame.Sequence != recording.sequence+1 || len(frame.Audio) == 0 || len(frame.Audio)%2 != 0 ||
			len(frame.Audio) > 64<<10 || recording.bytes+len(frame.Audio) > 16000*2*30 {
			return errSequence
		}
		if recording.sequence == 0 {
			c.gateway.timing("audio.input", recording.started, "voice_turn_id", recording.turnID)
		}
		recording.sequence, recording.bytes = frame.Sequence, recording.bytes+len(frame.Audio)
		call, cancel := context.WithTimeout(c.ctx, 2*time.Second)
		defer cancel()
		return c.media.AppendAudio(call, frame.Audio)
	case contract.SpeechEnded:
		if c.recording == nil || c.recording.clientID != frame.VoiceTurnID || c.recording.sequence == 0 {
			return nil
		}
		recording := *c.recording
		c.recording = nil
		c.gateway.timing("speech.ended", recording.started, "voice_turn_id", recording.turnID)
		j := c.newJob("commit", recording)
		go c.commit(j, c.media)
	}
	return nil
}

func (c *connection) newJob(kind string, input capture) *job {
	ctx, cancel := context.WithCancel(c.ctx)
	j := &job{ctx: ctx, cancel: cancel, kind: kind, capture: input}
	c.current = j
	return j
}

func (c *connection) emit(j *job, event jobEvent) bool {
	event.job = j
	select {
	case c.work <- event:
		return true
	case <-j.ctx.Done():
		return false
	}
}

func (c *connection) commit(j *job, media contract.S2SSession) {
	ctx, cancel := context.WithTimeout(j.ctx, 12*time.Second)
	defer cancel()
	start := time.Now()
	final, err := media.Commit(ctx)
	c.gateway.timing("transcript.final", start, "voice_turn_id", j.capture.turnID)
	if err != nil || final.InputItemID == "" || final.ProviderSessionID == "" || strings.TrimSpace(final.Text) == "" {
		if err == nil {
			err = errors.New("invalid final transcript")
		}
		c.emit(j, jobEvent{finished: true, err: err})
		return
	}
	c.emit(j, jobEvent{transcript: &final})
}

func (c *connection) route(j *job, final contract.FinalTranscript) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(j.ctx, 2*time.Second)
	defer cancel()
	pending, err := c.gateway.Pending.ListPending(ctx, c.scope, time.Now().UTC())
	vehicle := err != nil || !plainChat(final.Text, pending)
	c.gateway.timing("route", start, "voice_turn_id", j.capture.turnID, "vehicle", vehicle)
	c.emit(j, jobEvent{routed: true, vehicle: vehicle, transcript: &final})
}

func plainChat(text string, pending []contract.PendingCandidate) bool {
	text = strings.Trim(strings.TrimSpace(text), "。！!？?，, ")
	switch text {
	case "你好", "嗨", "哈喽", "早上好", "下午好", "晚上好", "谢谢", "你是谁":
		return true
	case "讲个笑话", "给我讲个笑话", "讲个故事":
		return len(pending) == 0
	default:
		return false
	}
}

func (c *connection) vehicle(j *job, final contract.FinalTranscript) {
	turn := contract.VehicleTurn{Scope: c.scope, VoiceTurnID: j.capture.turnID, RequestID: j.capture.requestID, FinalTranscript: final}
	start := time.Now()
	reply, err := c.gateway.Turns.HandleVehicleTurn(j.ctx, turn)
	c.gateway.timing("hermes.done", start, "voice_turn_id", turn.VoiceTurnID, "request_id", turn.RequestID,
		"run_id", reply.RunID, "task_id", reply.TaskID, "command_id", reply.CommandID)
	if err != nil && j.ctx.Err() == nil {
		reply = contract.SpeechReply{VoiceTurnID: turn.VoiceTurnID, RequestID: turn.RequestID, Text: "这次未能处理，请再说一次。"}
	}
	c.emit(j, jobEvent{reply: &reply})
}

func (c *connection) native(j *job, media contract.S2SSession, itemID string) {
	createCtx, cancel := context.WithTimeout(j.ctx, 8*time.Second)
	response, err := media.CreateResponse(createCtx, itemID)
	cancel()
	if err != nil {
		c.emit(j, jobEvent{finished: true, err: err})
		return
	}
	if !c.emit(j, jobEvent{playbackID: "chat-" + j.capture.turnID}) {
		return
	}
	playCtx, stop := context.WithTimeout(j.ctx, 60*time.Second)
	defer stop()
	for {
		pcm, err := response.Next(playCtx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			c.emit(j, jobEvent{finished: true, err: err})
			return
		}
		if !c.emit(j, jobEvent{pcm: pcm}) {
			return
		}
	}
}

func (c *connection) synthesize(j *job, text string) {
	ctx, stop := context.WithTimeout(j.ctx, 30*time.Second)
	defer stop()
	err := c.gateway.TTS.Synthesize(ctx, text, func(pcm []byte) error {
		if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 64<<10 {
			return errors.New("TTS returned invalid PCM16")
		}
		if !c.emit(j, jobEvent{pcm: append([]byte(nil), pcm...)}) {
			return j.ctx.Err()
		}
		return nil
	})
	c.emit(j, jobEvent{finished: true, err: err})
}

func (c *connection) onWork(event jobEvent) {
	j := event.job
	switch {
	case event.routed:
		if event.vehicle {
			j.kind = "vehicle"
			go c.vehicle(j, *event.transcript)
		} else {
			j.kind = "native"
			go c.native(j, c.media, event.transcript.InputItemID)
		}
	case event.transcript != nil:
		item := digest(event.transcript.ProviderSessionID, event.transcript.InputItemID)
		if c.items[item] || !c.state.take(j.capture.clientID) {
			j.cancel()
			c.current = nil
			return
		}
		c.items[item] = true
		j.kind = "route"
		go c.route(j, *event.transcript)
	case event.reply != nil:
		reply := *event.reply
		for _, result := range c.refreshResults(false) {
			if reply.CommandID != "" && result.CommandID == reply.CommandID && result.Status.IsVehicleResult() {
				reply.Status, reply.Text = result.Status, ""
			}
		}
		c.write(contract.CarFrame{Type: contract.SpeechReplyEvent, VoiceTurnID: j.capture.turnID, Reply: &reply})
		j.kind = "tts"
		if reply.Text == "" {
			c.onWork(jobEvent{job: j, finished: true})
			return
		}
		c.startPlayback("reply-" + j.capture.turnID)
		go c.synthesize(j, reply.Text)
	case event.playbackID != "":
		j.nativeReady = true
		c.startPlayback(event.playbackID)
	case event.pcm != nil:
		c.audio(event.pcm)
	case event.finished:
		j.cancel()
		c.current = nil
		reason := "completed"
		if event.err != nil || (c.playing != nil && c.playing.sequence == 0) {
			reason = "failed"
		}
		c.stopPlayback(reason)
		if event.err != nil && (j.kind == "commit" || j.kind == "native") {
			c.recoverMedia(event.err)
			return
		}
		c.refreshResults(true)
	}
}

func (c *connection) startPlayback(id string) {
	p := &playback{id: id, generation: c.gateway.nextPlayback.Add(1), started: time.Now()}
	c.playing = p
	c.state.issued(p.id, p.generation)
	c.write(contract.CarFrame{Type: contract.PlaybackStart, PlaybackID: p.id, PlaybackGeneration: p.generation})
}

func (c *connection) audio(pcm []byte) {
	if p := c.playing; p != nil {
		if p.sequence == 0 {
			c.gateway.timing("audio.output", p.started, "playback_id", p.id)
		}
		p.sequence++
		c.write(contract.CarFrame{Type: contract.AudioOutput, PlaybackID: p.id, PlaybackGeneration: p.generation, Sequence: p.sequence, Audio: pcm})
	}
}

func (c *connection) stopPlayback(reason string) {
	if p := c.playing; p != nil {
		c.playing = nil
		c.write(contract.CarFrame{Type: contract.PlaybackStop, PlaybackID: p.id, PlaybackGeneration: p.generation, Reason: reason})
		if reason == "interrupted" {
			c.settlePlayback(p.id, p.generation)
		}
	}
}

func (c *connection) settlePlayback(id string, generation uint64) {
	if !c.state.canSettle(id, generation) {
		return
	}
	if c.receipts != nil && strings.HasPrefix(id, "result-") {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 2*time.Second)
		defer cancel()
		if err := c.receipts.SettleResultPlayback(ctx, c.scope, id, time.Now().UTC()); err != nil {
			if c.gateway.Logger != nil {
				c.gateway.Logger.Warn("vehicle result playback receipt unavailable", "playback_id", id, "error", err)
			}
			return
		}
	}
	c.state.settle(id, generation)
}

func (c *connection) interrupt(userStarted bool) error {
	j := c.current
	if j == nil {
		return nil
	}
	c.current = nil
	if userStarted {
		c.stopPlayback("interrupted")
	}
	j.cancel()
	switch j.kind {
	case "vehicle":
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 3*time.Second)
		defer cancel()
		if err := c.gateway.Turns.CancelVehicleTurn(ctx, c.scope, j.capture.turnID); err != nil && c.gateway.Logger != nil {
			c.gateway.Logger.Warn("vehicle turn cancellation failed", "voice_turn_id", j.capture.turnID, "error", err)
		}
	case "native":
		if userStarted && j.nativeReady {
			ctx, stop := context.WithTimeout(c.ctx, time.Second)
			defer stop()
			return c.media.CancelResponse(ctx)
		}
		return errors.New("response creation interrupted before confirmation")
	case "commit":
		return errors.New("audio commit interrupted before final transcript")
	}
	return nil
}

func (c *connection) refreshResults(play bool) []contract.VehicleResultEvent {
	ctx, cancel := context.WithTimeout(c.ctx, 500*time.Millisecond)
	defer cancel()
	since := time.Now().UTC().Add(-contract.ResultRetention)
	results, err := c.gateway.Results.ListVehicleResults(ctx, c.scope, since)
	if err != nil {
		return nil
	}
	filtered := make([]contract.VehicleResultEvent, 0, len(results))
	for _, result := range results {
		if result.Scope == c.scope && result.EventID != "" && result.CommandID != "" && result.Status.IsVehicleResult() &&
			!result.OccurredAt.Before(since) {
			filtered = append(filtered, result)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].OccurredAt.Before(filtered[j].OccurredAt)
	})
	if !play {
		return filtered
	}
	newest := make(map[string]string)
	for _, result := range filtered {
		newest[result.CommandID] = result.EventID
	}
	for _, result := range filtered {
		if !c.signalled[result.EventID] {
			c.signalled[result.EventID] = true
			c.write(contract.CarFrame{Type: contract.VehicleResult, Result: &result})
		}
		id := "result-" + digest(c.scope.UserID, c.scope.SessionID, c.scope.VehicleID, result.EventID)
		if newest[result.CommandID] != result.EventID || c.attempted[id] || c.state.wasSettled(id) || c.current != nil || c.recording != nil {
			continue
		}
		if c.receipts != nil {
			settled, err := c.receipts.ResultPlaybackSettled(ctx, c.scope, id, since)
			if err != nil || settled {
				// Keep delivering the result event, but wait for the durable
				// receipt lookup before deciding whether to play audio.
				continue
			}
		}
		c.attempted[id] = true
		c.startPlayback(id)
		j := c.newJob("result", capture{})
		go c.synthesize(j, vehicleResultText(result))
	}
	return filtered
}

func vehicleResultText(result contract.VehicleResultEvent) string {
	switch result.Status {
	case contract.Completed:
		if result.Device == "window" && result.Tool == "close" {
			return "车窗已关闭。"
		}
		return "车辆已确认执行完成。"
	case contract.Failed:
		return "车辆未能执行这条指令。"
	default:
		return "车辆执行结果暂未确认。"
	}
}
