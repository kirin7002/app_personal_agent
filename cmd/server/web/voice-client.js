// Browser Push-to-Talk (PTT) WebSocket voice client strictly implementing
// app_personal_agent's GET /voice/audio contract (contract/audio.go):
//   - Uplink : 16 kHz mono PCM16LE (session.open -> speech.started -> audio.input -> speech.ended)
//   - Downlink: 24 kHz mono PCM16LE (playback.start -> audio.output -> playback.stop -> playback.ack)
//   - Barge-in: starting a new turn stops local playback immediately without cancelling
//     already-committed vehicle commands; result-* playback IDs are deduplicated locally.

const MAX_TURN_AUDIO_BYTES = 16000 * 2 * 30; // 30 seconds max per turn (gateway limit)
const TARGET_CHUNK_BYTES = 1920; // 60 ms at 16 kHz PCM16LE (even, <= 64 KiB)
const HANDSHAKE_TIMEOUT_MS = 10000;

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

export class VehicleVoiceClient {
  constructor({
    sessionId,
    vehicleId,
    wsUrl = '',
    onStateChange = () => {},
    onReply = () => {},
    onResult = () => {},
    onError = () => {},
  } = {}) {
    this.sessionId = sessionId;
    this.vehicleId = vehicleId;
    this.wsUrl = wsUrl;
    this.onStateChange = onStateChange;
    this.onReply = onReply;
    this.onResult = onResult;
    this.onError = onError;

    this.socket = null;
    this.audioCtx = null;
    this.mediaStream = null;
    this.micSource = null;
    this.workletNode = null;
    this.muteGain = null;

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
    this.currentPlayback = null; // { id, generation, expectedSeq, skip, stopReceived, reason }
    this.consumedResultPlaybacks = new Set();
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
      this.audioCtx = new AudioCtx();
      await this.audioCtx.resume();
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

      await this.audioCtx.audioWorklet.addModule('/static/pcm-worklet.js');
      if (this.closed) {
        this.cleanupMedia();
        return;
      }

      this.workletNode = new AudioWorkletNode(this.audioCtx, 'pcm-capture');
      this.micSource = this.audioCtx.createMediaStreamSource(this.mediaStream);
      this.muteGain = this.audioCtx.createGain();
      this.muteGain.gain.value = 0;
      this.micSource.connect(this.workletNode);
      this.workletNode.connect(this.muteGain);
      this.muteGain.connect(this.audioCtx.destination);

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
        if (!fmt || fmt.encoding !== 'pcm_s16le' || fmt.sample_rate !== 24000 || fmt.channels !== 1) {
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
        const id = frame.playback_id || '';
        const gen = Number(frame.playback_generation) || 0;
        const skip = id.startsWith('result-') && this.consumedResultPlaybacks.has(id);
        this.currentPlayback = {
          id,
          generation: gen,
          expectedSeq: 1,
          skip,
          stopReceived: false,
          reason: '',
          receivedAudio: false,
        };
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
        if (!pb.skip && frame.audio) {
          this.enqueue24kPCM(frame.audio, pb);
        }
        break;
      }

      case 'playback.stop': {
        const pb = this.currentPlayback;
        if (!pb || frame.playback_id !== pb.id || Number(frame.playback_generation) !== pb.generation) {
          return;
        }
        pb.stopReceived = true;
        pb.reason = frame.reason || '';
        if (pb.reason === 'completed') {
          if (pb.skip || this.activeSources.size === 0) {
            this.ackPlayback(pb);
          }
        } else {
          this.stopPlayback();
          this.currentPlayback = null;
        }
        break;
      }

      default:
        break;
    }
  }

  startTalking() {
    if (!this.connected || this.closed || this.recording || !this.generation) return;

    // Barge-in: immediately stop local playback when the user starts talking.
    this.stopPlayback();
    this.audioCtx?.resume?.().catch(() => {});

    const suffix = typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID().slice(0, 12)
      : `${Date.now()}-${Math.random().toString(16).slice(2, 8)}`;

    this.voiceTurnId = `web-v-${suffix}`;
    this.inputSeq = 0;
    this.sentBytes = 0;
    this.pendingChunks = [];
    this.pendingBytes = 0;
    this.recording = true;

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
    this.workletNode?.port.postMessage({ type: 'stop' });
    this.pendingChunks = [];
    this.pendingBytes = 0;
  }

  enqueue24kPCM(encoded, playbackRef) {
    if (this.closed || !this.audioCtx) return;
    const bytes = base64ToBytes(encoded);
    if (bytes.length < 2) return;

    const sampleCount = Math.floor(bytes.length / 2);
    const view = new DataView(bytes.buffer, bytes.byteOffset, sampleCount * 2);
    const audioBuffer = this.audioCtx.createBuffer(1, sampleCount, 24000);
    const channel = audioBuffer.getChannelData(0);
    for (let i = 0; i < sampleCount; i++) {
      channel[i] = view.getInt16(i * 2, true) / 32768;
    }

    const source = this.audioCtx.createBufferSource();
    source.buffer = audioBuffer;
    source.connect(this.audioCtx.destination);

    const startAt = Math.max(this.audioCtx.currentTime + 0.025, this.nextPlayTime);
    this.nextPlayTime = startAt + audioBuffer.duration;
    this.activeSources.add(source);

    if (!this.playing) {
      this.playing = true;
      this.emitState();
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
    this.micSource = null;
    this.workletNode = null;
    this.muteGain = null;
    if (this.audioCtx && this.audioCtx.state !== 'closed') {
      this.audioCtx.close().catch(() => {});
    }
    this.audioCtx = null;
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
