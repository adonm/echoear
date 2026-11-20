const assert = require("node:assert/strict");
const {test} = require("node:test");

test("schedules a 12-second reply gaplessly and acknowledges only after its final source ends", async () => {
  const sources = [], events = [];
  const context = {
    state: "running",
    currentTime: 0,
    createGain: () => ({gain:{value:0}, connect() {}}),
    createBuffer: (_, count, rate) => ({duration: count / rate, getChannelData: () => new Float32Array(count)}),
    createBufferSource() {
      return {
        connect() {},
        disconnect() {},
        start(at) { this.at = at; sources.push(this); },
        stop() { this.stopped = true; },
      };
    },
  };
  const dom = new Map();
  Object.assign(globalThis, {
    AudioContext: class { constructor() { return context; } },
    document: {
      addEventListener() {},
      getElementById(id) {
        if (!dom.has(id)) dom.set(id, {addEventListener() {}, showModal() {}});
        return dom.get(id);
      },
    },
    matchMedia: () => ({addEventListener() {}}),
    WebSocket: {OPEN: 1},
  });
  const {audioReady, handleTTS, playPCM} = await import("../web/audio.js");
  const {state} = await import("../web/state.js");
  state.ws = {readyState:1, send:raw => events.push(JSON.parse(raw))};
  assert.equal(await audioReady(), true);

  const packet = new Uint8Array(4001);
  packet[0] = 2;
  handleTTS({state:"start", audio:42});
  for (let i = 0; i < 8; i++) playPCM(packet.buffer);
  context.currentTime = .5;
  context.state = "suspended";
  context.onstatechange();
  for (let i = 8; i < 96; i++) playPCM(packet.buffer);
  handleTTS({state:"end", audio:42});
  assert.equal(sources.length, 96);
  assert.ok(sources[0].at > 0);
  for (const [i, s] of sources.entries()) {
    assert.ok(!s.stopped);
    if (i) assert.equal(s.at, sources[i-1].at + sources[i-1].buffer.duration);
  }
  context.state = "running";
  context.onstatechange();
  for (const s of sources.slice(0, -1)) s.onended();
  assert.deepEqual(events, []);
  sources.at(-1).onended();
  assert.deepEqual(events, [{ev:"tts_ack", state:"done", audio:42}]);
});
