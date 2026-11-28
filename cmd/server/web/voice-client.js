// Browser Push-to-Talk (PTT) WebSocket voice client strictly implementing
// app_personal_agent's GET /voice/audio contract (contract/audio.go):
//   - Uplink : 16 kHz mono PCM16LE (session.open -> speech.started -> audio.input -> speech.ended)
//   - Downlink: 24 kHz mono PCM16LE (playback.start -> audio.output -> playback.stop -> playback.ack)
//   - Safari/WebKit compatibility:
//       * Separate capture AudioContext from playback so getUserMedia(echoCancellation)
//         does not silence speaker output.
//       * Prefer native HTML5 <audio> WAV Blob playback for complete TTS clips (reply-*/result-*),
//         and automatically stream via a dedicated native-sample-rate playback AudioContext
//         for long-running S2S streams (chat-*/qwen TTS).
//   - Barge-in: starting a new turn stops local playback immediately without cancelling
//     already-committed vehicle commands; result-* playback IDs are deduplicated locally.

const MAX_TURN_AUDIO_BYTES = 16000 * 2 * 30; // 30 seconds max per turn (gateway limit)
const TARGET_CHUNK_BYTES = 1920; // 60 ms at 16 kHz PCM16LE (even, <= 64 KiB)
const HANDSHAKE_TIMEOUT_MS = 10000;
const WAV_COALESCE_MS = 180; // Wait briefly for playback.stop before switching to live WebAudio streaming
const OUTPUT_SAMPLE_RATE = 24000;

// Tiny 1 ms silent 24 kHz mono WAV used to unlock Safari <audio> playback inside a user gesture.
const SILENT_WAV_DATA_URI =
  'data:audio/wav;base64,UklGRkYAAABXQVZFZm10IBAAAAABAAEAwF0AAIC7AAACABAAZGF0YSIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';

export function bytesToBase64(bytes) {
  let binary = '';
  const step = 0x8000;
  for (let i = 0; i < bytes.length; i += step) {
    binary += String.fromCharCode(...bytes.subarray(i, i + step));
  }
  return btoa(binary);
}

export function base64ToBytes(encoded) {
  const binary = atob(encoded);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    out[i] = binary.charCodeAt(i);
  }
  return out;
}

export function encodeWav24kMono(chunks) {
  const list = Array.isArray(chunks) ? chunks : [chunks];
  let totalBytes = 0;
  for (const c of list) {
    if (c && c.byteLength) totalBytes += c.byteLength;
  }
  const dataLen = totalBytes - (totalBytes % 2);
  const wav = new Uint8Array(44 + dataLen);
  const view = new DataView(wav.buffer);

  // RIFF header
  wav.set([0x52, 0x49, 0x46, 0x46], 0); // "RIFF"
  view.setUint32(4, 36 + dataLen, true);
  wav.set([0x57, 0x41, 0x56, 0x45], 8); // "WAVE"

  // fmt chunk: PCM, mono, 24000 Hz, 16-bit
  wav.set([0x66, 0x6d, 0x74, 0x20], 12); // "fmt "
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, OUTPUT_SAMPLE_RATE, true);
  view.setUint32(28, OUTPUT_SAMPLE_RATE * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);

  // data chunk
  wav.set([0x64, 0x61, 0x74, 0x61], 36); // "data"
  view.setUint32(40, dataLen, true);

  let offset = 44;
  let remaining = dataLen;
  for (const c of list) {
    if (!c || !c.byteLength || remaining <= 0) continue;
    const take = Math.min(c.byteLength, remaining);
    wav.set(c.subarray(0, take), offset);
    offset += take;
    remaining -= take;
  }
  return wav;
}

export function resample24kToRate(pcmBytes, targetSampleRate) {
  const inCount = Math.floor((pcmBytes?.byteLength || 0) / 2);
  if (inCount <= 0) return new Float32Array(0);
  const view = new DataView(pcmBytes.buffer, pcmBytes.byteOffset, inCount * 2);
  const rate = Number(targetSampleRate) || OUTPUT_SAMPLE_RATE;

  if (rate === OUTPUT_SAMPLE_RATE) {
    const out = new Float32Array(inCount);
    for (let i = 0; i < inCount; i++) {
      out[i] = view.getInt16(i * 2, true) / 32768;
    }
    return out;
  }

  const outCount = Math.max(1, Math.round((inCount * rate) / OUTPUT_SAMPLE_RATE));
  const out = new Float32Array(outCount);
  const step = OUTPUT_SAMPLE_RATE / rate;
  for (let i = 0; i < outCount; i++) {
    const pos = i * step;
    const idx = Math.floor(pos);
    const frac = pos - idx;
    const s0 = view.getInt16(Math.min(idx, inCount - 1) * 2, true) / 32768;
    const s1 = view.getInt16(Math.min(idx + 1, inCount - 1) * 2, true) / 32768;
    out[i] = s0 + (s1 - s0) * frac;
  }
  return out;
}

export function createTestChimePCM24k() {
  const sampleRate = OUTPUT_SAMPLE_RATE;
  const totalSamples = Math.floor(sampleRate * 0.36);
  const pcm = new Uint8Array(totalSamples * 2);
  const view = new DataView(pcm.buffer);
  for (let i = 0; i < totalSamples; i++) {
    const t = i / sampleRate;
    const freq = t < 0.16 ? 523.25 : 659.25; // C5 -> E5 pleasant chime
    const env = Math.min(1, t / 0.015) * Math.min(1, (0.36 - t) / 0.06);
    const sample = Math.round(Math.sin(2 * Math.PI * freq * t) * 11000 * Math.max(0, env));
    view.setInt16(i * 2, sample, true);
  }
  return pcm;
}

export class VehicleVoiceClient {
  constructor({
    sessionId,
    vehicleId,
    wsUrl = '',
    playbackCtx = null,
    audioEl = null,
    wavCoalesceMs = WAV_COALESCE_MS,
    onStateChange = () => {},
    onReply = () => {},
    onResult = () => {},
    onError = () => {},
  } = {}) {
    this.sessionId = sessionId;
    this.vehicleId = vehicleId;
    this.wsUrl = wsUrl;
    this.wavCoalesceMs = wavCoalesceMs;
    this.onStateChange = onStateChange;
    this.onReply = onReply;
    this.onResult = onResult;
    this.onError = onError;

    this.socket = null;

    // Separate capture pipeline from playback pipeline to avoid Safari/WebKit AEC muting.
    this.captureCtx = null;
    this.mediaStream = null;
    this.micSource = null;
    this.workletNode = null;
    this.muteGain = null;

    // Playback pipeline (native <audio> WAV Blob primary + dedicated playbackCtx streaming fallback).
    this.playbackCtx = playbackCtx;
    this.playbackGain = null;
    this.audioEl = audioEl;
    this.audioElUnlocked = Boolean(audioEl?.dataset?.unlocked === '1');
    this.activeAudioUrl = '';
    this.activeAudioPlayback = null;

    this.connected = false;
    this.closed = false;
    this.generation = 0;

    // Recording turn state
    this.recording = false;
    this.voiceTurnId = '';
    this.inputSeq = 0;
    this.sentBytes = 0;
    this.pendingChunks = [];
    this.pendingBytes = 0;

    // Downlink playback state
    this.activeSources = new Set();
    this.nextPlayTime = 0;
    this.playing = false;
    this.currentPlayback = null;
    this.consumedResultPlaybacks = new Set();
  }

  get audioCtx() {
    return this.playbackCtx || this.captureCtx;
  }

  emitState(extra = {}) {
    this.onStateChange({
      connected: this.connected,
      recording: this.recording,
      playing: this.playing,
      generation: this.generation,
      ...extra,
    });
  }

  sendFrame(frame) {
    if (this.socket?.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify(frame));
      return true;
    }
    return false;
  }

  configureAudioSession() {
    try {
      if (typeof navigator !== 'undefined' && navigator.audioSession) {
        navigator.audioSession.type = 'play-and-record';
      }
    } catch {}
  }

  ensurePlaybackOutput() {
    if (typeof window !== 'undefined') {
      const AudioCtx = window.AudioContext || window.webkitAudioContext;
      if (!this.playbackCtx && AudioCtx) {
        this.playbackCtx = new AudioCtx();
      }
      if (this.playbackCtx && !this.playbackGain && typeof this.playbackCtx.createGain === 'function') {
        this.playbackGain = this.playbackCtx.createGain();
        this.playbackGain.gain.value = 1.0;
        this.playbackGain.connect(this.playbackCtx.destination);
      }
    }
    if (!this.audioEl && typeof document !== 'undefined') {
      const el = document.createElement('audio');
      el.preload = 'auto';
      el.playsInline = true;
      el.muted = false;
      el.volume = 1.0;
      el.style.display = 'none';
      document.body?.appendChild(el);
      this.audioEl = el;
      this.ownsAudioEl = true;
    }
  }

  unlockPlayback() {
    this.configureAudioSession();
    this.ensurePlaybackOutput();
    if (this.playbackCtx && this.playbackCtx.state === 'suspended') {
      this.playbackCtx.resume?.().catch(() => {});
    }
    if (this.audioEl && !this.audioElUnlocked && !this.playing) {
      try {
        this.audioEl.muted = false;
        this.audioEl.volume = 1.0;
        this.audioEl.src = SILENT_WAV_DATA_URI;
        const p = this.audioEl.play?.();
        if (p && typeof p.then === 'function') {
          p.then(() => {
            this.audioElUnlocked = true;
            if (!this.activeAudioPlayback) {
              this.audioEl?.pause?.();
            }
          }).catch(() => {});
        } else {
          this.audioElUnlocked = true;
        }
      } catch {}
    }
  }

  setMicEnabled(enabled) {
    if (!this.mediaStream) return;
    try {
      for (const track of this.mediaStream.getAudioTracks()) {
        track.enabled = Boolean(enabled);
      }
    } catch {}
  }

  async connect() {
    if (this.closed) throw new Error('voice client already closed');
    if (this.connected) return;

    if (!navigator.mediaDevices?.getUserMedia) {
      throw new Error('当前浏览器不支持麦克风采集，请通过 localhost 或 HTTPS 访问');
    }

    const AudioCtx = window.AudioContext || window.webkitAudioContext;
    if (!AudioCtx) {
      throw new Error('当前浏览器不支持 Web Audio API');
    }

    try {
      this.unlockPlayback();

      this.captureCtx = new AudioCtx();
      await this.captureCtx.resume();
      if (this.closed) return;

      this.mediaStream = await navigator.mediaDevices.getUserMedia({
        audio: {
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
          channelCount: 1,
        },
        video: false,
      });
      if (this.closed) {
        this.cleanupMedia();
        return;
      }

      // Safari may suspend AudioContexts when getUserMedia switches CoreAudio session category.
      if (this.captureCtx.state !== 'running') {
        await this.captureCtx.resume().catch(() => {});
      }
      if (this.playbackCtx && this.playbackCtx.state !== 'running') {
        await this.playbackCtx.resume().catch(() => {});
      }

      await this.captureCtx.audioWorklet.addModule('/static/pcm-worklet.js');
      if (this.closed) {
        this.cleanupMedia();
        return;
      }

      this.workletNode = new AudioWorkletNode(this.captureCtx, 'pcm-capture');
      this.micSource = this.captureCtx.createMediaStreamSource(this.mediaStream);
      this.muteGain = this.captureCtx.createGain();
      this.muteGain.gain.value = 0;
      this.micSource.connect(this.workletNode);
      this.workletNode.connect(this.muteGain);
      this.muteGain.connect(this.captureCtx.destination);
      this.setMicEnabled(false);

      this.workletNode.port.onmessage = (event) => {
        if (!this.recording || !this.connected || this.closed) return;
        this.handleCapturedPCM(new Uint8Array(event.data));
      };

      const url = this.wsUrl || `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/voice/audio`;
      await this.openSocket(url);
    } catch (err) {
      this.disconnect();
      throw err;
    }
  }

  openSocket(url) {
    return new Promise((resolve, reject) => {
      let settled = false;
      const timer = setTimeout(() => {
        if (!settled) {
          settled = true;
          reject(new Error('语音会话握手超时，请检查后端和百炼连接'));
        }
      }, HANDSHAKE_TIMEOUT_MS);

      const finish = (fn, arg) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        fn(arg);
      };

      const ws = new WebSocket(url);
      this.socket = ws;

      ws.onopen = () => {
        this.sendFrame({
          type: 'session.open',
          session_id: this.sessionId,
          vehicle_id: this.vehicleId,
          input_format: { encoding: 'pcm_s16le', sample_rate: 16000, channels: 1 },
        });
      };

      ws.onmessage = (event) => {
        let frame;
        try {
          frame = JSON.parse(event.data);
        } catch (err) {
          this.onError(new Error('收到无法解析的语音控制帧'));
          return;
        }
        this.handleServerFrame(frame, (err) => {
          if (err) finish(reject, err);
          else finish(resolve);
        });
      };

      ws.onerror = () => {
        const err = new Error('语音 WebSocket 连接失败');
        if (!settled) finish(reject, err);
        else this.onError(err);
      };

      ws.onclose = (ev) => {
        const reason = ev.reason || `code ${ev.code}`;
        if (!settled) {
          finish(reject, new Error(`语音连接已关闭（${reason}）`));
        } else if (!this.closed) {
          this.connected = false;
          this.recording = false;
          this.setMicEnabled(false);
          this.stopPlayback();
          this.emitState({ reason: 'disconnected' });
        }
      };
    });
  }

  handleServerFrame(frame, onReadySettled) {
    if (this.closed || !frame || !frame.type) return;

    switch (frame.type) {
      case 'session.ready': {
        const fmt = frame.output_format;
        if (!fmt || fmt.encoding !== 'pcm_s16le' || fmt.sample_rate !== OUTPUT_SAMPLE_RATE || fmt.channels !== 1) {
          const err = new Error('服务端返回了不支持的输出音频格式');
          onReadySettled?.(err);
          this.onError(err);
          return;
        }
        this.connected = true;
        this.generation = Number(frame.connection_generation) || this.generation + 1;
        this.emitState();
        onReadySettled?.(null);
        break;
      }

      case 'session.reconnecting': {
        this.abortRecordingLocally();
        this.stopPlayback();
        this.emitState({ reason: 'reconnecting' });
        break;
      }

      case 'session.error': {
        const msg = frame.reason || '语音会话不可用';
        this.abortRecordingLocally();
        const err = new Error(msg);
        onReadySettled?.(err);
        this.onError(err);
        break;
      }

      case 'speech.reply': {
        if (frame.reply) this.onReply(frame.reply);
        break;
      }

      case 'vehicle.result': {
        if (frame.result) this.onResult(frame.result);
        break;
      }

      case 'playback.start': {
        this.stopPlayback();
        const id = frame.playback_id || '';
        const gen = Number(frame.playback_generation) || 0;
        const skip = id.startsWith('result-') && this.consumedResultPlaybacks.has(id);
        const pb = {
          id,
          generation: gen,
          expectedSeq: 1,
          skip,
          stopReceived: false,
          reason: '',
          receivedAudio: false,
          mode: this.canUseAudioElement() ? 'buffering' : 'stream',
          chunks: [],
          streamTimer: null,
          acked: false,
        };
        this.currentPlayback = pb;
        break;
      }

      case 'audio.output': {
        const pb = this.currentPlayback;
        if (!pb || frame.playback_id !== pb.id || Number(frame.playback_generation) !== pb.generation) {
          return;
        }
        if (Number(frame.sequence) !== pb.expectedSeq) {
          return;
        }
        pb.expectedSeq++;
        pb.receivedAudio = true;
        if (pb.skip || !frame.audio) return;

        const pcmBytes = base64ToBytes(frame.audio);
        if (pcmBytes.length < 2) return;

        if (pb.mode === 'buffering') {
          pb.chunks.push(pcmBytes);
          // Arm the streaming fallback timer only after the first PCM chunk arrives,
          // so server-side TTS synthesis latency never triggers premature switching.
          if (!pb.streamTimer && this.wavCoalesceMs > 0) {
            pb.streamTimer = setTimeout(() => this.switchPlaybackToStream(pb), this.wavCoalesceMs);
          }
        } else if (pb.mode === 'stream') {
          this.enqueue24kPCMBytes(pcmBytes, pb);
        }
        break;
      }

      case 'playback.stop': {
        const pb = this.currentPlayback;
        if (!pb || frame.playback_id !== pb.id || Number(frame.playback_generation) !== pb.generation) {
          return;
        }
        if (pb.streamTimer) {
          clearTimeout(pb.streamTimer);
          pb.streamTimer = null;
        }
        pb.stopReceived = true;
        pb.reason = frame.reason || '';
        if (pb.reason !== 'completed') {
          this.stopPlayback();
          this.currentPlayback = null;
          return;
        }
        if (pb.skip) {
          this.ackPlayback(pb);
          return;
        }
        if (pb.mode === 'buffering') {
          if (pb.chunks.length === 0) {
            this.ackPlayback(pb);
            return;
          }
          pb.mode = 'element';
          this.playBufferedWav(pb);
        } else if (pb.mode === 'stream' && this.activeSources.size === 0) {
          this.ackPlayback(pb);
        }
        break;
      }

      default:
        break;
    }
  }

  canUseAudioElement() {
    return Boolean(
      this.audioEl &&
      typeof Blob !== 'undefined' &&
      typeof URL !== 'undefined' &&
      typeof URL.createObjectURL === 'function'
    );
  }

  switchPlaybackToStream(pb) {
    if (this.closed || this.currentPlayback !== pb || pb.mode !== 'buffering' || pb.stopReceived) {
      return;
    }
    pb.streamTimer = null;
    pb.mode = 'stream';
    const buffered = pb.chunks;
    pb.chunks = [];
    for (const chunk of buffered) {
      this.enqueue24kPCMBytes(chunk, pb);
    }
  }

  playBufferedWav(pb) {
    if (this.closed || this.currentPlayback !== pb) return;
    if (!this.canUseAudioElement()) {
      this.playChunksViaWebAudio(pb);
      return;
    }

    const chunks = pb.chunks;
    pb.chunks = [];
    const wavBytes = encodeWav24kMono(chunks);
    const blob = new Blob([wavBytes], { type: 'audio/wav' });
    const url = URL.createObjectURL(blob);

    this.revokeActiveAudioUrl();
    this.activeAudioUrl = url;
    this.activeAudioPlayback = pb;
    this.playing = true;
    this.emitState({ playbackMode: 'audio-element' });

    const el = this.audioEl;
    el.muted = false;
    el.volume = 1.0;

    const finishElement = () => {
      if (this.activeAudioPlayback !== pb) return;
      el.onended = null;
      el.onerror = null;
      this.activeAudioPlayback = null;
      this.revokeActiveAudioUrl();
      this.playing = false;
      this.ackPlayback(pb);
      this.emitState();
    };

    const fallbackToWebAudio = () => {
      if (this.activeAudioPlayback !== pb) return;
      el.onended = null;
      el.onerror = null;
      try { el.pause(); } catch {}
      this.activeAudioPlayback = null;
      this.revokeActiveAudioUrl();
      pb.chunks = chunks;
      pb.mode = 'stream';
      this.playChunksViaWebAudio(pb);
    };

    el.onended = finishElement;
    el.onerror = fallbackToWebAudio;
    el.src = url;
    try { el.currentTime = 0; } catch {}

    try {
      const p = el.play();
      if (p && typeof p.catch === 'function') {
        p.catch(() => fallbackToWebAudio());
      }
    } catch {
      fallbackToWebAudio();
    }
  }

  playChunksViaWebAudio(pb) {
    const chunks = pb.chunks || [];
    pb.chunks = [];
    pb.mode = 'stream';
    if (!this.playbackCtx || chunks.length === 0) {
      this.playing = false;
      if (chunks.length > 0) {
        this.onError(new Error('浏览器未允许音频播放，请点击界面任意按钮或「测试声音」解除静音限制'));
      }
      this.ackPlayback(pb);
      this.emitState();
      return;
    }
    for (const chunk of chunks) {
      this.enqueue24kPCMBytes(chunk, pb);
    }
  }

  revokeActiveAudioUrl() {
    if (this.activeAudioUrl && typeof URL !== 'undefined' && typeof URL.revokeObjectURL === 'function') {
      try {
        URL.revokeObjectURL(this.activeAudioUrl);
      } catch {}
    }
    this.activeAudioUrl = '';
  }

  async playTestTone() {
    this.unlockPlayback();
    const pcm = createTestChimePCM24k();

    if (this.canUseAudioElement()) {
      const wav = encodeWav24kMono([pcm]);
      const url = URL.createObjectURL(new Blob([wav], { type: 'audio/wav' }));
      const el = this.audioEl;
      this.stopPlayback();
      this.activeAudioUrl = url;
      el.muted = false;
      el.volume = 1.0;
      el.src = url;
      try { el.currentTime = 0; } catch {}
      try {
        await el.play();
        el.onended = () => {
          this.revokeActiveAudioUrl();
        };
        return { ok: true, mode: 'HTML5 Audio (WAV)', state: this.playbackCtx?.state || 'ok' };
      } catch (err) {
        this.revokeActiveAudioUrl();
        // Fall through to WebAudio playbackCtx test
      }
    }

    if (this.playbackCtx) {
      if (this.playbackCtx.state !== 'running') {
        await this.playbackCtx.resume();
      }
      const samples = resample24kToRate(pcm, this.playbackCtx.sampleRate);
      const buf = this.playbackCtx.createBuffer(1, samples.length, this.playbackCtx.sampleRate);
      buf.getChannelData(0).set(samples);
      const src = this.playbackCtx.createBufferSource();
      src.buffer = buf;
      src.connect(this.playbackGain || this.playbackCtx.destination);
      src.start();
      return { ok: true, mode: 'WebAudio', state: this.playbackCtx.state };
    }
    throw new Error('当前浏览器无可用音频输出设备');
  }

  startTalking() {
    if (!this.connected || this.closed || this.recording || !this.generation) return;

    // Barge-in: immediately stop local playback when the user starts talking.
    this.stopPlayback();
    this.unlockPlayback();
    this.captureCtx?.resume?.().catch(() => {});

    const suffix = typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID().slice(0, 12)
      : `${Date.now()}-${Math.random().toString(16).slice(2, 8)}`;

    this.voiceTurnId = `web-v-${suffix}`;
    this.inputSeq = 0;
    this.sentBytes = 0;
    this.pendingChunks = [];
    this.pendingBytes = 0;
    this.recording = true;
    this.setMicEnabled(true);

    this.sendFrame({
      type: 'speech.started',
      voice_turn_id: this.voiceTurnId,
      connection_generation: this.generation,
    });
    this.workletNode?.port.postMessage({ type: 'start' });
    this.emitState();
  }

  handleCapturedPCM(chunk) {
    if (!chunk || chunk.length === 0) return;
    this.pendingChunks.push(chunk);
    this.pendingBytes += chunk.length;

    while (this.pendingBytes >= TARGET_CHUNK_BYTES && this.recording) {
      const frameBytes = this.takeBufferedBytes(TARGET_CHUNK_BYTES);
      this.sendAudioChunk(frameBytes);
      if (this.sentBytes >= MAX_TURN_AUDIO_BYTES) {
        this.stopTalking();
        return;
      }
    }
  }

  takeBufferedBytes(maxBytes) {
    const count = Math.min(maxBytes, this.pendingBytes);
    const evenCount = count - (count % 2);
    if (evenCount <= 0) return new Uint8Array(0);

    const out = new Uint8Array(evenCount);
    let offset = 0;
    while (offset < evenCount && this.pendingChunks.length > 0) {
      const head = this.pendingChunks[0];
      const need = evenCount - offset;
      if (head.length <= need) {
        out.set(head, offset);
        offset += head.length;
        this.pendingChunks.shift();
      } else {
        out.set(head.subarray(0, need), offset);
        this.pendingChunks[0] = head.subarray(need);
        offset += need;
      }
    }
    this.pendingBytes -= evenCount;
    return out;
  }

  sendAudioChunk(bytes) {
    if (!bytes || bytes.length < 2 || bytes.length % 2 !== 0) return;
    const remainingBudget = MAX_TURN_AUDIO_BYTES - this.sentBytes;
    if (remainingBudget < 2) return;
    const toSend = bytes.length > remainingBudget
      ? bytes.subarray(0, remainingBudget - (remainingBudget % 2))
      : bytes;
    if (toSend.length < 2) return;

    this.inputSeq += 1;
    this.sentBytes += toSend.length;
    this.sendFrame({
      type: 'audio.input',
      voice_turn_id: this.voiceTurnId,
      connection_generation: this.generation,
      sequence: this.inputSeq,
      audio: bytesToBase64(toSend),
    });
  }

  stopTalking() {
    if (!this.recording) return;
    this.recording = false;
    this.setMicEnabled(false);
    this.workletNode?.port.postMessage({ type: 'stop' });

    if (this.pendingBytes >= 2) {
      const tail = this.takeBufferedBytes(this.pendingBytes);
      this.sendAudioChunk(tail);
    }
    this.pendingChunks = [];
    this.pendingBytes = 0;

    if (this.inputSeq > 0) {
      this.sendFrame({
        type: 'speech.ended',
        voice_turn_id: this.voiceTurnId,
        connection_generation: this.generation,
      });
    } else {
      this.onError(new Error('按键时间过短，未采集到有效语音，请按住后再说'));
    }
    this.emitState();
  }

  abortRecordingLocally() {
    if (!this.recording) return;
    this.recording = false;
    this.setMicEnabled(false);
    this.workletNode?.port.postMessage({ type: 'stop' });
    this.pendingChunks = [];
    this.pendingBytes = 0;
  }

  enqueue24kPCM(encoded, playbackRef) {
    const bytes = base64ToBytes(encoded);
    this.enqueue24kPCMBytes(bytes, playbackRef);
  }

  enqueue24kPCMBytes(bytes, playbackRef) {
    if (this.closed || !this.playbackCtx || !bytes || bytes.length < 2) return;
    if (this.playbackCtx.state === 'suspended') {
      this.playbackCtx.resume?.().catch(() => {});
    }

    const targetRate = this.playbackCtx.sampleRate || OUTPUT_SAMPLE_RATE;
    const resampled = resample24kToRate(bytes, targetRate);
    if (resampled.length === 0) return;

    const audioBuffer = this.playbackCtx.createBuffer(1, resampled.length, targetRate);
    audioBuffer.getChannelData(0).set(resampled);

    const source = this.playbackCtx.createBufferSource();
    source.buffer = audioBuffer;
    source.connect(this.playbackGain || this.playbackCtx.destination);

    const startAt = Math.max(this.playbackCtx.currentTime + 0.025, this.nextPlayTime);
    this.nextPlayTime = startAt + audioBuffer.duration;
    this.activeSources.add(source);

    if (!this.playing) {
      this.playing = true;
      this.emitState({ playbackMode: 'webaudio' });
    }

    source.onended = () => {
      this.activeSources.delete(source);
      if (this.activeSources.size === 0) {
        this.playing = false;
        this.nextPlayTime = 0;
        if (this.currentPlayback === playbackRef && playbackRef.stopReceived && playbackRef.reason === 'completed') {
          this.ackPlayback(playbackRef);
        }
        this.emitState();
      }
    };
    source.start(startAt);
  }

  ackPlayback(pb) {
    if (!pb || pb.acked) return;
    pb.acked = true;
    if (pb.id.startsWith('result-')) {
      this.consumedResultPlaybacks.add(pb.id);
    }
    this.sendFrame({
      type: 'playback.ack',
      playback_id: pb.id,
      playback_generation: pb.generation,
    });
    if (this.currentPlayback === pb) {
      this.currentPlayback = null;
    }
  }

  stopPlayback() {
    if (this.currentPlayback?.streamTimer) {
      clearTimeout(this.currentPlayback.streamTimer);
      this.currentPlayback.streamTimer = null;
    }
    if (this.audioEl) {
      this.audioEl.onended = null;
      this.audioEl.onerror = null;
      try {
        this.audioEl.pause();
      } catch {}
    }
    this.activeAudioPlayback = null;
    this.revokeActiveAudioUrl();

    for (const src of this.activeSources) {
      src.onended = null;
      try {
        src.stop();
      } catch {}
    }
    this.activeSources.clear();
    this.nextPlayTime = 0;
    if (this.playing) {
      this.playing = false;
      this.emitState();
    }
  }

  cleanupMedia() {
    this.mediaStream?.getTracks().forEach((t) => t.stop());
    this.mediaStream = null;
    try { this.micSource?.disconnect(); } catch {}
    try { this.workletNode?.disconnect(); } catch {}
    try { this.muteGain?.disconnect(); } catch {}
    try { this.playbackGain?.disconnect(); } catch {}
    this.micSource = null;
    this.workletNode = null;
    this.muteGain = null;
    this.playbackGain = null;

    if (this.captureCtx && this.captureCtx.state !== 'closed') {
      this.captureCtx.close().catch(() => {});
    }
    this.captureCtx = null;

    if (this.playbackCtx && this.playbackCtx.state !== 'closed') {
      this.playbackCtx.close().catch(() => {});
    }
    this.playbackCtx = null;

    if (this.audioEl) {
      try {
        this.audioEl.pause();
        this.audioEl.remove?.();
      } catch {}
    }
    this.audioEl = null;
  }

  disconnect() {
    if (this.closed) return;
    this.closed = true;
    this.connected = false;
    this.abortRecordingLocally();
    this.stopPlayback();
    try {
      this.socket?.close();
    } catch {}
    this.socket = null;
    this.cleanupMedia();
    this.emitState({ reason: 'disconnected' });
  }
}
