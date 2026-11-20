const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {join} = require("node:path");
const {test} = require("node:test");
const {runInNewContext} = require("node:vm");

const source = readFileSync(join(__dirname, "../web/mic.js"), "utf8");

function worklet(sampleRate, mode) {
  const messages = [];
  let Mic;
  runInNewContext(source, {
    sampleRate,
    AudioWorkletProcessor: class {
      port = {postMessage: data => messages.push(data)};
    },
    registerProcessor: (_, processor) => { Mic = processor; },
  });
  const mic = new Mic({processorOptions:{mode}});
  function frames(count, level, live = true) {
    const frame = new Float32Array(sampleRate / 50).fill(level);
    for (let i = 0; i < count; i++) {
      // AudioWorklet render quanta need not line up with PCM frame boundaries.
      for (let offset = 0; offset < frame.length; offset += 128) {
        const chunk = frame.subarray(offset, offset + 128);
        assert.equal(mic.process([[chunk]]), live || i < count - 1 || offset + 128 < frame.length);
      }
    }
  }
  function ended(marker, packets) {
    assert.equal(messages.length, packets + 1);
    assert.equal(messages.at(-1), marker);
    for (const packet of messages.slice(0, -1)) {
      assert.equal(new Uint8Array(packet)[0], 1);
    }
  }
  return {mic, messages, frames, ended};
}

test("sends after 75 quiet frames below 4 seconds of voiced audio", () => {
  const {mic, frames, ended} = worklet(44100);
  frames(9, .02);
  frames(1, .1);
  frames(197, .1);
  frames(74, .02);
  frames(1, .03); // The release threshold resets silence and counts voiced time.
  frames(74, .02);
  frames(1, .02, false);
  mic.port.onmessage({data:"flush"});
  ended("auto_end", 357);
});

test("sends after 150 quiet frames at 4 seconds of voiced audio", () => {
  const {frames, ended} = worklet(48000);
  frames(10, .02);
  frames(100, .1);
  frames(74, .02); // Gaps do not count as voiced time.
  frames(100, .1);
  frames(149, .02);
  frames(1, .02, false);
  ended("auto_end", 434);
});

test("sends quiet recordings at 8 s and caps voiced recordings at 60 s", () => {
  const quiet = worklet(48000);
  quiet.frames(10, .02);
  quiet.frames(389, .04); // Below the ambient-based onset threshold.
  quiet.frames(1, .04, false);
  quiet.ended("auto_end", 400);

  const voice = worklet(48000);
  voice.frames(10, 0);
  voice.frames(2989, .1);
  voice.frames(1, .1, false);
  voice.ended("auto_end", 3000);
});

test("manual flush sends the partial PCM packet before ending", () => {
  const {mic, messages, frames, ended} = worklet(48000);
  frames(1, .1);
  assert.equal(mic.process([[new Float32Array(128).fill(.1)]]), true);
  mic.port.onmessage({data:"flush"});
  ended("end", 2);
  assert.equal(messages[0].byteLength, 641);
  assert.equal(messages[1].byteLength, 85);
});

test("call mic sends continuous 40 ms PCM without VAD or a duration limit", () => {
  const {mic, frames, messages} = worklet(44100, "call");
  frames(10, .125);
  frames(3000, 0); // Well past the normal silence and duration limits.
  mic.port.onmessage({data:"flush"}); // Call packets are never partially flushed.
  frames(1, -.25);
  assert.equal(messages.length, 1505);
  frames(1, -.25);
  assert.equal(messages.length, 1506);
  for (const packet of messages) {
    assert.equal(packet.byteLength, 1281);
    assert.equal(new Uint8Array(packet)[0], 1);
  }
  assert.equal(new DataView(messages[0]).getInt16(1, true), 4096);
  assert.equal(new DataView(messages.at(-1)).getInt16(1279, true), -8192);
});
