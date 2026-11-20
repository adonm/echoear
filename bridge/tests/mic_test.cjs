const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {join} = require("node:path");
const {test} = require("node:test");
const {runInNewContext} = require("node:vm");

const source = readFileSync(join(__dirname, "../web/mic.js"), "utf8");

function worklet(sampleRate) {
  const messages = [];
  let Mic;
  runInNewContext(source, {
    sampleRate,
    AudioWorkletProcessor: class {
      port = {postMessage: data => messages.push(data)};
    },
    registerProcessor: (_, processor) => { Mic = processor; },
  });
  const mic = new Mic();
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

test("detects speech during calibration and honors hysteresis and 150 quiet frames", () => {
  const {mic, frames, ended} = worklet(44100);
  frames(9, .02);
  frames(1, .1);
  frames(149, .02);
  frames(1, .03); // Above half the calibrated threshold (.05), but below onset.
  frames(149, .02);
  frames(1, .02, false);
  mic.port.onmessage({data:"flush"});
  ended("auto_end", 310);
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
