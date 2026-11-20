import assert from "node:assert/strict";
import {test} from "node:test";

test("renders the selected live device after the dashboard modules load", async () => {
  function element(tag) {
    return {
      tag, dataset:{}, style:{setProperty() {}}, attrs:{}, children:[],
      addEventListener() {},
      setAttribute(name, value) { this.attrs[name] = value; },
      removeAttribute(name) { delete this.attrs[name]; },
      replaceChildren(...children) { this.children = children; },
      querySelector(tag) { return this.children.find(child => child.tag === tag); },
      querySelectorAll(tag) { return this.children.filter(child => child.tag === tag); },
      append(...children) { this.children.push(...children); },
    };
  }
  const els = new Map();
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
  });
  const {renderDevices} = await import("./devices.js");
  const {state} = await import("./state.js");
  const d = {device_id:"echoear-cake", character:"Cake", ws_connected:true};
  state.app = {devices:[d]};
  state.view = {id:d.device_id, device:d};
  renderDevices();
  const row = els.get("physical").querySelector("button");
  assert.equal(row.attrs["aria-pressed"], "true");
  assert.equal(row.children[1].textContent, "Cake");
  assert.equal(row.children[2].textContent, "Live WS");
  assert.doesNotThrow(() => row.onclick());
});
