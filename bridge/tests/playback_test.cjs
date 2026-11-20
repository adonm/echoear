const assert = require("node:assert/strict");
const {test} = require("node:test");

async function audioFixture(label) {
  const sources = [], events = [], tracks = [], constraints = [], nodes = [];
  const connect = function () { return this; };
  const context = {
    state: "running",
    currentTime: 0,
    resume() { this.state = "running"; },
    audioWorklet: {addModule: async () => {}},
    createMediaStreamSource: () => ({connect, disconnect() {}}),
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
    AudioWorkletNode: class {
      constructor(_, __, options) { this.options = options; nodes.push(this); }
      port = {close() { this.closed = true; }};
      connect = connect;
      disconnect() { this.disconnected = true; }
    },
    document: {
      addEventListener() {},
      getElementById(id) {
        if (!dom.has(id)) dom.set(id, {addEventListener() {}, setAttribute() {}, showModal() {}});
        return dom.get(id);
      },
    },
    matchMedia: () => ({addEventListener() {}}),
    WebSocket: {OPEN: 1},
  });
  Object.defineProperty(globalThis, "navigator", {configurable:true, value:{mediaDevices:{
    async getUserMedia(options) {
      constraints.push(options);
      const track = {stop() { this.stopped = true; }};
      tracks.push(track);
      return {getTracks: () => [track]};
    },
  }}});
  globalThis.window = {isSecureContext:true, AudioWorkletNode};
  const audio = await import(`../web/audio.js?${label}`);
  const {state} = await import("../web/state.js");
  state.view = {virtual:true};
  state.ws = {readyState:1, bufferedAmount:0, send:raw => events.push(typeof raw === "string" ? JSON.parse(raw) : raw)};
  assert.equal(await audio.audioReady(), true);
  return {audio, sources, events, tracks, constraints, nodes, context, state};
}

test("schedules a 12-second reply gaplessly and acknowledges only after its final source ends", async () => {
  const {audio:{handleTTS, playPCM}, sources, events, context} = await audioFixture("tts");
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

test("call plays and captures together, drops stale audio, and mutes only the uplink", async () => {
  const {audio:{handleCall, playPCM}, sources, events, tracks, constraints, nodes, state} = await audioFixture("call");
  const starting = handleCall({state:"start", call:17, muted:false});
  const inPCM = new Uint8Array(1281), outPCM = new Uint8Array(1281);
  inPCM[0] = 2;
  outPCM[0] = 1;
  playPCM(inPCM.buffer);
  await starting;
  assert.ok(!sources[0].stopped);
  assert.deepEqual(constraints, [{audio:{channelCount:1, echoCancellation:true, noiseSuppression:true, autoGainControl:true}}]);
  assert.equal(nodes[0].options.processorOptions.mode, "call");
  assert.deepEqual(events, []);
  for (let i = 0; i < 3; i++) playPCM(inPCM.buffer);
  assert.equal(sources.length, 4); // Playback continues with a live worklet.
  for (const source of sources) assert.ok(source.at + source.buffer.duration <= .18);
  assert.ok(sources[0].stopped); // The fourth packet would exceed 180 ms.
  assert.equal(sources[3].at, sources[0].at);
  nodes[0].port.onmessage({data:outPCM.buffer});
  handleCall({state:"mute",call:17,muted:true});
  nodes[0].port.onmessage({data:outPCM.buffer});
  playPCM(inPCM.buffer);
  assert.equal(sources.length, 5);
  assert.deepEqual(events, [outPCM.buffer]);
  handleCall({state:"mute",call:17,muted:false});
  state.ws.bufferedAmount = 10000;
  nodes[0].port.onmessage({data:outPCM.buffer});
  assert.deepEqual(events, [outPCM.buffer]);
  handleCall({state:"end",call:17});
  assert.ok(tracks[0].stopped && nodes[0].port.closed && nodes[0].disconnected);
  assert.ok(sources.every(s => s.stopped));
  assert.deepEqual(events, [outPCM.buffer]);
});
