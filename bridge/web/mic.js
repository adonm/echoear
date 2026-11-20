const frameSamples = 320; // 20 ms at 16 kHz.

class EchoMic extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ratio = sampleRate / 16000;
    this.left = this.ratio;
    this.sum = 0;
    this.packet = new Uint8Array(1 + 2 * frameSamples);
    this.buffer = new DataView(this.packet.buffer);
    this.used = 1;
    this.live = true;
    this.energy = 0;
    this.frames = 0;
    this.calibration = [];
    this.threshold = 0;
    this.voiced = false;
    this.quiet = 0;
    this.port.onmessage = () => this.finish("end");
  }
  finish(message) {
    if (!this.live) return;
    this.live = false;
    if (this.used > 1) this.send();
    this.port.postMessage(message);
  }
  hear(rms) {
    if (rms >= this.threshold) this.voiced = true;
    if (this.voiced) this.quiet = rms >= this.threshold * 0.5 ? 0 : this.quiet + 1;
  }
  frame() {
    const rms = Math.sqrt(this.energy / frameSamples);
    this.energy = 0;
    this.frames++;
    if (this.frames <= 10) {
      this.calibration.push(rms);
      if (this.frames !== 10) return;
      // Estimate the floor from the quietest frames, then detect any early speech.
      const [a, b, c] = this.calibration.slice().sort((x, y) => x - y);
      this.threshold = Math.max(0.01, (a + b + c) / 3 * 2.5);
      for (const level of this.calibration) this.hear(level);
    } else {
      this.hear(rms);
    }
    // End at 1.8 s of silence after speech, 25 s total, or 6 s without detection.
    if (this.quiet >= 90 || this.frames >= 1250 || (!this.voiced && this.frames >= 300)) {
      this.finish("auto_end");
    }
  }
  send() {
    this.packet[0] = 1;
    const frame = this.used === this.packet.length ? this.packet.buffer : this.packet.buffer.slice(0, this.used);
    this.port.postMessage(frame, [frame]);
    this.packet = new Uint8Array(1 + 2 * frameSamples);
    this.buffer = new DataView(this.packet.buffer);
    this.used = 1;
  }
  process(inputs) {
    if (!this.live) return false;
    const mono = inputs[0]?.[0];
    if (!mono) return true;
    for (const value of mono) {
      let weight = 1;
      // Average across each destination sample's interval, carrying fractional
      // input and packet boundaries over to the next render quantum.
      while (weight > 1e-8) {
        const part = Math.min(weight, this.left);
        this.sum += value * part;
        this.left -= part;
        weight -= part;
        if (this.left <= 1e-8) {
          const sample = Math.max(-1, Math.min(1, this.sum / this.ratio));
          this.buffer.setInt16(this.used, Math.round(sample * (sample < 0 ? 32768 : 32767)), true);
          this.used += 2;
          this.energy += sample * sample;
          if (this.used === this.packet.length) {
            this.send();
            this.frame();
            if (!this.live) return false;
          }
          this.left = this.ratio;
          this.sum = 0;
        }
      }
    }
    return true;
  }
}

registerProcessor("echo-mic", EchoMic);
