const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {join} = require("node:path");
const {test} = require("node:test");
const {runInNewContext} = require("node:vm");

test("schedules a 12-second reply gaplessly and acknowledges only after its final source ends", () => {
  const sources = [], events = [];
  const context = {
    state: "running",
    currentTime: 0,
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
  const source = readFileSync(join(__dirname, "../web/client.js"), "utf8");
  const client = {
    ArrayBuffer, URL, context,
    location: {href: "https://echoear.test/app"},
    localStorage: {getItem: key => key === "echoear-phone" ? "phone-1234" : null},
    document: {
      addEventListener() {},
      getElementById(id) {
        if (!dom.has(id)) dom.set(id, {addEventListener() {}, showModal() {}});
        return dom.get(id);
      },
    },
    matchMedia: () => ({addEventListener() {}}),
    WebSocket: {OPEN: 1},
    socket: {readyState: 1, send: raw => events.push(JSON.parse(raw))},
  };
  runInNewContext(source + "\naudio = context; view = {virtual:true}; ws = socket;", client);

  const packet = new Uint8Array(4001);
  packet[0] = 2;
  client.command({op:"tts", state:"start", audio:42});
  for (let i = 0; i < 8; i++) client.playPCM(packet.buffer);
  context.currentTime = .5;
  context.state = "suspended";
  client.syncSpeaker();
  for (let i = 8; i < 96; i++) client.playPCM(packet.buffer);
  client.command({op:"tts", state:"end", audio:42});
  assert.equal(sources.length, 96);
  assert.ok(sources[0].at > 0);
  for (const [i, s] of sources.entries()) {
    assert.ok(!s.stopped);
    if (i) assert.equal(s.at, sources[i-1].at + sources[i-1].buffer.duration);
  }
  context.state = "running";
  client.syncSpeaker();
  for (const s of sources.slice(0, -1)) s.onended();
  assert.deepEqual(events, []);
  sources.at(-1).onended();
  assert.deepEqual(events, [{ev:"tts_ack", state:"done", audio:42}]);
});
