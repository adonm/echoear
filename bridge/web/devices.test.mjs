import assert from "node:assert/strict";
import {test} from "node:test";

test("renders the selected live device after the dashboard modules load", async () => {
  function element(tag) {
    return {
      tag, dataset:{}, style:{setProperty() {}}, attrs:{}, children:[], listeners:{},
      addEventListener(name, handler) { this.listeners[name] = handler; },
      setPointerCapture() {},
      getScreenCTM: () => ({inverse() {}}),
      setAttribute(name, value) { this.attrs[name] = value; },
      removeAttribute(name) { delete this.attrs[name]; },
      replaceChildren(...children) { this.children = children; },
      querySelector(tag) { return this.children.find(child => child.tag === tag); },
      querySelectorAll(tag) { return this.children.filter(child => child.tag === tag); },
      append(...children) {
        for (const child of children) {
          child.parent = this;
          child.isConnected = true;
        }
        this.children.push(...children);
      },
      remove() {
        this.parent.children = this.parent.children.filter(child => child !== this);
        this.isConnected = false;
      },
    };
  }
  const els = new Map();
  const activation = [];
  let audio;
  Object.assign(globalThis, {
    document: {
      createElement: element,
      getElementById(id) {
        if (!els.has(id)) els.set(id, element(id));
        return els.get(id);
      },
    },
    matchMedia: () => ({matches:true, addEventListener() {}}),
    localStorage: {getItem: key => key === "echoear-phone" ? "phone-1234" : null},
    location: {href:"https://echoear.test/app"},
    Option: class {},
    WebSocket: {OPEN:1},
    window: {isSecureContext:true, AudioWorkletNode:class {}},
    DOMPoint: class {
      constructor(x, y) { Object.assign(this, {x,y}); }
      matrixTransform() { return this; }
    },
    AudioContext: class {
      state = "suspended";
      constructor() { audio = this; }
      createGain() { return {gain:{}, connect() {}}; }
      resume() { activation.push("resume"); this.state = "running"; }
    },
  });
  Object.defineProperty(globalThis, "navigator", {configurable:true, value:{mediaDevices:{getUserMedia() {}}}});
  const {renderDevices} = await import("./devices.js");
  const {state} = await import("./state.js");
  const d = {device_id:"echoear-cake", character:"Cake", ws_connected:true};
  state.app = {devices:[d]};
  state.view = {id:d.device_id, device:d};
  renderDevices();
  const row = els.get("physical").children[0].querySelector("button");
  assert.equal(row.attrs["aria-pressed"], "true");
  assert.equal(row.children[1].textContent, "Cake");
  assert.equal(row.children[2].textContent, "Live WS");
  assert.doesNotThrow(() => row.onclick());
  d.caps = {call:true};
  const sent = [];
  state.view = {id:"phone-1234", virtual:true, mode:"idle"};
  state.rev = 1;
  state.ws = {readyState:1, send:raw => {
    const event = JSON.parse(raw);
    activation.push(event.ev);
    sent.push(event);
  }};
  renderDevices();
  const call = els.get("physical").children[0].children[1];
  assert.equal(call.hidden, false);
  assert.equal(call.disabled, false);
  assert.equal(call.attrs["aria-label"], "Call Cake");
  call.onclick();
  assert.deepEqual(sent, [{ev:"call", target:d.device_id}]);
  assert.deepEqual(activation, ["resume", "call"]);

  const stage = els.get("stage").listeners;
  const gesture = {pointerType:"touch", button:0, pointerId:1, clientX:180, clientY:250};
  state.view.mode = "calling";
  audio.state = "suspended";
  stage.pointerdown(gesture);
  stage.pointerup(gesture);
  assert.deepEqual(activation, ["resume", "call", "resume", "up", "tap"]);
});
