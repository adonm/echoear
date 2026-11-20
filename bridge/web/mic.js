class EchoMic extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ratio = sampleRate / 16000;
    this.left = this.ratio;
    this.sum = 0;
    this.packet = new Uint8Array(641); // 20 ms and a type byte.
    this.buffer = new DataView(this.packet.buffer);
    this.used = 1;
    this.live = true;
    this.port.onmessage = () => {
      if (this.used > 1) this.send();
      this.live = false;
      this.port.postMessage("end");
    };
  }
  send() {
    this.packet[0] = 1;
    const frame = this.used === this.packet.length ? this.packet.buffer : this.packet.buffer.slice(0, this.used);
    this.port.postMessage(frame, [frame]);
    this.packet = new Uint8Array(641);
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
          if (this.used === this.packet.length) this.send();
          this.left = this.ratio;
          this.sum = 0;
        }
      }
    }
    return true;
  }
}

registerProcessor("echo-mic", EchoMic);
