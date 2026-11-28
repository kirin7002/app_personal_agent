// Streaming microphone resampler AudioWorkletProcessor.
// Resamples arbitrary browser input sampleRate to 16 kHz mono signed 16-bit
// little-endian PCM and emits 20 ms (320 samples = 640 bytes) transferable frames.

class PcmCaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.samples = [];
    this.position = 0;
    this.frame = [];
    this.ratio = sampleRate / 16000;
    this.active = false;

    this.port.onmessage = (event) => {
      const cmd = event.data?.type;
      if (cmd === 'start') {
        this.samples = [];
        this.position = 0;
        this.frame = [];
        this.active = true;
      } else if (cmd === 'stop') {
        this.flushRemainder();
        this.active = false;
        this.samples = [];
        this.position = 0;
        this.frame = [];
      }
    };
  }

  emitFrame(floatSamples) {
    if (!floatSamples.length) return;
    const buffer = new ArrayBuffer(floatSamples.length * 2);
    const view = new DataView(buffer);
    for (let i = 0; i < floatSamples.length; i++) {
      const clamped = Math.max(-1, Math.min(1, floatSamples[i]));
      view.setInt16(i * 2, Math.round(clamped < 0 ? clamped * 32768 : clamped * 32767), true);
    }
    this.port.postMessage(buffer, [buffer]);
  }

  flushRemainder() {
    if (this.frame.length > 0) {
      this.emitFrame(this.frame);
      this.frame = [];
    }
  }

  process(inputs) {
    if (!this.active) return true;
    const channel = inputs[0]?.[0];
    if (!channel) return true;

    for (let i = 0; i < channel.length; i++) {
      this.samples.push(channel[i]);
    }

    while (this.position + 1 < this.samples.length) {
      const idx = Math.floor(this.position);
      const alpha = this.position - idx;
      const sample = this.samples[idx] * (1 - alpha) + this.samples[idx + 1] * alpha;
      this.frame.push(sample);
      this.position += this.ratio;

      if (this.frame.length === 320) {
        this.emitFrame(this.frame);
        this.frame = [];
      }
    }

    const consumed = Math.floor(this.position);
    if (consumed > 0) {
      this.samples.splice(0, consumed);
      this.position -= consumed;
    }
    return true;
  }
}

registerProcessor('pcm-capture', PcmCaptureProcessor);
