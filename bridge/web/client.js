"use strict";

const $ = id => document.getElementById(id);
const screen = $("screen"), nodes = $("nodes"), svgNS = "http://www.w3.org/2000/svg";
const getLocal = key => { try { return localStorage.getItem(key) || ""; } catch { return ""; } };
const setLocal = (key, val) => { try { localStorage.setItem(key, val); } catch {} };
const params = new URL(location.href);
let token = params.searchParams.get("token") || getLocal("echoear-token");
if (params.searchParams.has("token")) {
  params.searchParams.delete("token");
  history.replaceState(null, "", params.pathname + params.search + params.hash);
  setLocal("echoear-token", token);
}
let phone = getLocal("echoear-phone");
if (!/^phone-[0-9a-f]{4}$/.test(phone)) {
  phone = "phone-" + crypto.getRandomValues(new Uint16Array(1))[0].toString(16).padStart(4, "0");
  setLocal("echoear-phone", phone);
}
let ws, asPhone = false, selected = "", devices = [], reconnect = 0, timer;
let rev = 0;
const keyed = new Map();
const send = o => { if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(o)); };
const updateStatus = text => { $("screen-status").textContent = text; $("screen-description").textContent = text; };

function settings(show) {
  $("credentials").hidden = !show;
  $("settings").setAttribute("aria-expanded", String(show));
  if (show) $("token").focus();
}
$("settings").onclick = () => settings($("credentials").hidden);
$("credentials").onsubmit = e => {
  e.preventDefault();
  token = $("token").value.trim();
  $("token").value = "";
  if (token) {
    setLocal("echoear-token", token);
    settings(false);
    connect();
  }
};

function clearScene() {
  rev = 0;
  keyed.clear();
  nodes.replaceChildren();
  $("canvas-bg").setAttribute("fill", "#251C3B");
  delete screen.dataset.dim;
}
const chosen = () => devices.find(d => d.device_id === selected);
function controls() {
  const ready = ws?.readyState === WebSocket.OPEN && (asPhone || chosen()?.ws);
  $("controls").hidden = !ready;
  $("phone-controls").hidden = !asPhone;
  $("mic-help").hidden = !asPhone || isSecureContext && !!navigator.mediaDevices?.getUserMedia && !!window.AudioWorkletNode;
  $("talk").disabled = !$("mic-help").hidden;
  $("join").textContent = asPhone ? "Switch to mirror" : "Join as Phone Cat";
  $("join").setAttribute("aria-pressed", String(asPhone));
}
function watch(id) {
  selected = id;
  clearScene();
  const d = chosen();
  send({ev:"watch", device_id:d?.ws ? id : ""});
  updateStatus(!d ? "Select a nearby cat." : d.ws ? `Mirroring ${d.character}…` :
    `${d.character} is using HTTP firmware. Live mirroring requires a WebSocket connection.`);
  peers();
}
function peers() {
  const nav = $("peers"), list = devices.filter(d => d.online && d.device_id !== phone);
  nav.replaceChildren();
  for (const d of list) {
    const b = document.createElement("button");
    b.type = "button";
    b.setAttribute("aria-pressed", String(!asPhone && d.device_id === selected));
    b.textContent = d.character;
    const badge = document.createElement("span");
    badge.className = "peer-state";
    badge.textContent = d.ws ? " · live" : " · HTTP";
    b.append(badge);
    b.onclick = () => {
      if (asPhone) { asPhone = false; selected = d.device_id; connect(); }
      else watch(d.device_id);
    };
    nav.append(b);
  }
  if (!list.length) {
    const empty = document.createElement("span");
    empty.className = "small";
    empty.textContent = "No other cats online.";
    nav.append(empty);
  }
  const target = $("target"), prev = target.value;
  target.replaceChildren(new Option("Ask my cat", ""), ...list.map(d => new Option(`Intercom: ${d.character}`, d.device_id)));
  target.value = list.some(d => d.device_id === prev) ? prev : "";
  controls();
}
$("join").onclick = () => {
  if (!token) { settings(true); return; }
  asPhone = !asPhone;
  if (asPhone) audioReady();
  connect();
};

function connect() {
  clearTimeout(timer);
  if (!token) { settings(true); return; }
  const old = ws;
  ws = null;
  old?.close();
  endMic(true, false);
  stopSound(false);
  clearScene();
  devices = [];
  peers();
  $("link-state").textContent = "Connecting to the bridge…";
  updateStatus(asPhone ? "Joining as Plum…" : "Looking for nearby cats…");
  const url = new URL("/ws", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  url.searchParams.set("token", token);
  const socket = new WebSocket(url);
  ws = socket;
  socket.binaryType = "arraybuffer";
  let opened = false;
  socket.onopen = () => {
    if (ws !== socket) return;
    opened = true;
    reconnect = 0;
    $("link-state").textContent = asPhone ? "Connected · Phone Cat" : "Connected · Remote control";
    send(asPhone ? {ev:"hello",role:"web",device_id:phone,character:"Plum",firmware:"web",volume:+$("volume").value}
      : {ev:"hello",role:"web"});
  };
  socket.onmessage = e => {
    if (ws !== socket) return;
    if (typeof e.data === "string") {
      try { command(JSON.parse(e.data)); } catch { updateStatus("Invalid screen update."); }
    } else if (asPhone) playPCM(e.data);
  };
  socket.onclose = () => {
    if (ws !== socket) return;
    ws = null;
    endMic(true, false);
    stopSound(false);
    controls();
    $("link-state").textContent = opened ? "Disconnected · reconnecting…" : "Could not connect. Check the bridge token and URL.";
    if (opened || reconnect > 0) {
      reconnect++;
      timer = setTimeout(connect, Math.min(5000, 500 * reconnect));
    }
  };
}

function command(msg) {
  switch (msg.op) {
    case "peers": {
      const wasLive = chosen()?.ws;
      devices = msg.devices;
      if (!asPhone) {
        if (!selected) selected = devices.find(d => d.online && d.ws)?.device_id || "";
        if (wasLive !== chosen()?.ws || (!rev && selected)) { watch(selected); break; }
        if (!selected) updateStatus("Select a nearby cat.");
      }
      peers();
      break;
    }
    case "scene":
      keyed.clear();
      nodes.replaceChildren();
      rev = msg.rev;
      $("canvas-bg").setAttribute("fill", msg.bg);
      for (const n of msg.nodes) put(n);
      updateStatus(asPhone ? "Plum · Phone Cat" : `Mirroring ${chosen()?.character || "EchoEar"}`);
      controls();
      break;
    case "patch":
      if (msg.rev !== rev + 1) {
        if (!asPhone) send({ev:"watch", device_id:selected});
        else connect();
        break;
      }
      rev = msg.rev;
      if (msg.bg) $("canvas-bg").setAttribute("fill", msg.bg);
      for (const id of msg.del) { keyed.get(id)?.el.remove(); keyed.delete(id); }
      for (const n of msg.add) put(n);
      for (const [id, fields] of Object.entries(msg.set)) {
        const old = keyed.get(id);
        if (old) put({...old.node, ...fields, id});
      }
      break;
    case "power":
      screen.dataset.dim = String(msg.brightness < 20);
      break;
    case "volume":
      setVolume(msg.level);
      break;
    case "chime":
      if (asPhone) chime(msg.kind);
      break;
    case "rec":
      if (asPhone) {
        if (msg.action === "start") startMic(msg.target || "");
        else if (msg.action === "stop") endMic(false);
      }
      break;
    case "tts":
      if (asPhone) {
        if (msg.state === "start") ttsEnded = false;
        if (msg.state === "end") { ttsEnded = true; ackIfDone(); }
      }
      break;
  }
}

function el(tag, attrs) {
  const element = document.createElementNS(svgNS, tag);
  for (const [key, val] of Object.entries(attrs)) if (val != null) element.setAttribute(key, val);
  return element;
}
function arcPath(n) {
  const rad = a => a * Math.PI / 180;
  const p = a => [n.x + n.r * Math.cos(rad(a)), n.y + n.r * Math.sin(rad(a))];
  const span = (n.a2 - n.a1 + 360) % 360;
  if (Math.abs(n.a2 - n.a1) >= 360) {
    const start = p(n.a1), end = p(n.a1 + 180);
    return `M ${start} A ${n.r} ${n.r} 0 0 1 ${end} A ${n.r} ${n.r} 0 0 1 ${start}`;
  }
  return `M ${p(n.a1)} A ${n.r} ${n.r} 0 ${span > 180 ? 1 : 0} 1 ${p(n.a2)}`;
}
function textMark(n, x, y) {
  const t = el("text", {x,y,fill:n.tc || n.c,"text-anchor":"middle","dominant-baseline":"central","font-size":[0,15,22,28][n.fs || 1]});
  t.textContent = n.s;
  return t;
}
function put(n) {
  const prev = keyed.get(n.id), group = prev?.el || el("g", {"data-id":n.id});
  if (!prev) nodes.append(group);
  keyed.set(n.id, {node:n, el:group});
  group.setAttribute("display", n.vis === false ? "none" : "inline");
  if (n.on) {
    group.dataset.on = n.on;
    if (n.on === "tap") {
      group.setAttribute("role", "button");
      group.setAttribute("tabindex", "0");
      group.setAttribute("aria-label", n.s || (n.id === "cat-ring" ? "Talk" : "Use " + n.t));
    }
  } else {
    delete group.dataset.on;
    for (const attr of ["role","tabindex","aria-label"]) group.removeAttribute(attr);
  }
  const stroke = {stroke:n.bc || n.c, "stroke-width":n.sw || 1, "stroke-linecap":"round"};
  const paint = {fill:n.fill || n.t === "btn" ? n.c : "none", ...(n.bc || !n.fill && n.t !== "btn" ? stroke : {})};
  let marks;
  switch (n.t) {
    case "circle": marks = [el("circle",{cx:n.x,cy:n.y,r:n.r,...paint})]; break;
    case "rect":
    case "btn":
      marks = [el("rect",{x:n.x,y:n.y,width:n.w,height:n.h,rx:n.r || 0,...paint})];
      if (n.t === "btn") marks.push(textMark(n,n.x+n.w/2,n.y+n.h/2));
      break;
    case "line": marks = [el("line",{x1:n.x1,y1:n.y1,x2:n.x2,y2:n.y2,...stroke})]; break;
    case "arc": marks = [el("path",{d:arcPath(n),fill:"none",...stroke})]; break;
    case "text": marks = [textMark(n,n.x,n.y)]; break;
    default: return;
  }
  group.replaceChildren(...marks);
}

let pointer = null;
function xy(e) {
  const p = new DOMPoint(e.clientX,e.clientY).matrixTransform(screen.getScreenCTM().inverse());
  const int = v => Math.max(0,Math.min(359,Math.round(v)));
  return {x:int(p.x), y:int(p.y)};
}
screen.addEventListener("pointerdown", e => {
  if (e.button !== 0 || pointer || !rev) return;
  const g = e.target.closest("[data-on]");
  pointer = {pointer:e.pointerId,id:g?.dataset.id || "",drag:g?.dataset.on === "drag",last:0};
  screen.setPointerCapture(e.pointerId);
  if (pointer.drag) send({ev:"drag",id:pointer.id,...xy(e)});
});
screen.addEventListener("pointermove", e => {
  if (pointer?.drag && pointer.pointer === e.pointerId && e.timeStamp - pointer.last >= 32) {
    pointer.last = e.timeStamp;
    send({ev:"drag",id:pointer.id,...xy(e)});
  }
});
function pointerEnd(e, cancelled) {
  if (!pointer || pointer.pointer !== e.pointerId) return;
  if (asPhone && !cancelled) audioReady();
  if (!cancelled && !pointer.drag) send({ev:"tap",id:pointer.id,...xy(e)});
  send({ev:"up",id:pointer.id,...xy(e)});
  pointer = null;
}
screen.addEventListener("pointerup", e => pointerEnd(e, false));
screen.addEventListener("pointercancel", e => pointerEnd(e, true));
screen.addEventListener("keydown", e => {
  const g = e.target.closest("[data-on=tap]");
  if (!g || (e.key !== " " && e.key !== "Enter")) return;
  e.preventDefault();
  if (asPhone) audioReady();
  send({ev:"tap",id:g.dataset.id,x:180,y:180});
});
$("prompt").onsubmit = e => {
  e.preventDefault();
  const input = $("text"), text = input.value.trim();
  if (text) {
    if (asPhone) audioReady();
    send({ev:"text",device_id:asPhone ? phone : selected,text});
    input.value = "";
  }
};
$("volume").oninput = e => { $("volume-value").value = e.target.value + "%"; };
$("volume").onchange = e => {
  const level = +e.target.value, old = volume;
  setVolume(level);
  send({ev:"head",action:level < old ? "vol_down" : "vol_up",volume:level});
};

let audio, gain, playAt = 0, volume = 80, ttsEnded = false;
const speaking = new Set();
function setVolume(n) {
  volume = n;
  $("volume").value = n;
  $("volume-value").value = n + "%";
  if (gain) gain.gain.value = n / 100;
}
async function audioReady() {
  try {
    if (!audio) {
      audio = new AudioContext();
      gain = audio.createGain();
      gain.gain.value = volume / 100;
      gain.connect(audio.destination);
    }
    if (audio.state !== "running") await audio.resume();
    if (audio.state === "running") $("sound").hidden = true;
    return audio.state === "running";
  } catch {
    return false;
  }
}
$("sound").onclick = audioReady;
function playPCM(raw) {
  if (!(raw instanceof ArrayBuffer) || raw.byteLength < 3 || raw.byteLength % 2 !== 1 || new Uint8Array(raw)[0] !== 2) return;
  if (audio?.state !== "running") { $("sound").hidden = false; return; }
  const wire = new DataView(raw,1), count = wire.byteLength / 2;
  const buffer = audio.createBuffer(1,count,16000), samples = buffer.getChannelData(0);
  for (let i = 0; i < count; i++) samples[i] = wire.getInt16(i*2,true)/32768;
  const source = audio.createBufferSource();
  source.buffer = buffer;
  source.connect(gain);
  if (playAt < audio.currentTime) playAt = audio.currentTime + .035;
  source.start(playAt);
  playAt += buffer.duration;
  speaking.add(source);
  source.onended = () => {
    speaking.delete(source);
    source.disconnect();
    ackIfDone();
  };
}
function ackIfDone() {
  if (ttsEnded && speaking.size === 0) {
    ttsEnded = false;
    send({ev:"tts_ack", state:"done"});
  }
}
function stopSound(ack) {
  const had = speaking.size > 0;
  ttsEnded = false;
  for (const source of speaking) {
    source.onended = null;
    source.stop();
    source.disconnect();
  }
  speaking.clear();
  playAt = 0;
  if (ack && had) send({ev:"tts_ack",state:"interrupted"});
}
function chime(kind) {
  if (kind === "cancel") stopSound(false);
  if (audio?.state !== "running") return;
  const tones = {tap:[540],think:[440],intercom:[660,880],cancel:[280]}[kind] || [];
  for (const [i, hz] of tones.entries()) {
    const at = audio.currentTime+.025+i*.10, osc = audio.createOscillator(), env = audio.createGain();
    osc.frequency.value = hz;
    env.gain.setValueAtTime(.001,at);
    env.gain.exponentialRampToValueAtTime(.10,at+.012);
    env.gain.exponentialRampToValueAtTime(.001,at+.085);
    osc.connect(env).connect(gain);
    osc.start(at);
    osc.stop(at+.09);
  }
}

let mic, workletLoaded;
function recordUI(active) {
  $("talk").setAttribute("aria-pressed", String(active));
  $("talk").textContent = active ? "Stop & send" : "Talk";
  $("cancel").hidden = !active;
  $("target").disabled = active;
}
async function startMic(target) {
  if (mic) return;
  if (!isSecureContext || !navigator.mediaDevices?.getUserMedia || !window.AudioWorkletNode) {
    $("mic-help").hidden = false;
    send({ev:"rec_end",cancelled:true,target});
    return;
  }
  stopSound(true);
  const rec = {target,stream:null,node:null,source:null,stopping:false};
  mic = rec;
  recordUI(true);
  try {
    if (!await audioReady() || !asPhone || mic !== rec || ws?.readyState !== WebSocket.OPEN) throw new Error();
    rec.stream = await navigator.mediaDevices.getUserMedia({audio:{channelCount:1,echoCancellation:true,noiseSuppression:true}});
    if (mic !== rec) { rec.stream.getTracks().forEach(t=>t.stop()); return; }
    workletLoaded ||= audio.audioWorklet.addModule("/app/mic.js").catch(err => { workletLoaded = null; throw err; });
    await workletLoaded;
    if (mic !== rec) return;
    rec.source = audio.createMediaStreamSource(rec.stream);
    rec.node = new AudioWorkletNode(audio,"echo-mic",{channelCount:1,channelCountMode:"explicit"});
    rec.node.port.onmessage = event => {
      if (mic !== rec) return;
      if (event.data === "end") {
        endMic(false, true, true);
      } else if (ws?.readyState === WebSocket.OPEN) {
        if (ws.bufferedAmount > 256*1024) {
          endMic(true);
          updateStatus("Recording cancelled: the connection is too slow.");
        } else ws.send(event.data);
      }
    };
    send({ev:"rec_start",target});
    rec.source.connect(rec.node).connect(audio.destination);
    updateStatus(target ? "Recording intercom…" : "Listening…");
  } catch {
    if (mic === rec) {
      endMic(true);
      updateStatus("Could not open the microphone. Check browser permissions.");
    }
  }
}
function endMic(cancelled, tell = true, flushed = false) {
  if (!mic) return;
  const rec = mic;
  if (rec.node && !cancelled && !flushed) {
    if (!rec.stopping) { rec.stopping = true; rec.node.port.postMessage("flush"); }
    return;
  }
  mic = null;
  rec.stream?.getTracks().forEach(t => t.stop());
  rec.source?.disconnect();
  rec.node?.disconnect();
  rec.node?.port.close();
  recordUI(false);
  if (tell) {
    send({ev:"rec_end",cancelled,target:rec.target});
    updateStatus(cancelled ? "Recording cancelled." : "Message sent to the bridge.");
  }
}
$("talk").onclick = () => { if (mic) endMic(false); else startMic($("target").value); };
$("cancel").onclick = () => endMic(true);
if (token) connect(); else settings(true);
