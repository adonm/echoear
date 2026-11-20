"use strict";

const $ = id => document.getElementById(id);
const screen = $("stage"), nodes = $("nodes"), svgNS = "http://www.w3.org/2000/svg";
const getLocal = key => { try { return localStorage.getItem(key) || ""; } catch { return ""; } };
const setLocal = (key, val) => { try { localStorage.setItem(key, val); } catch {} };
const params = new URL(location.href);
const initialToken = params.searchParams.has("token") ? params.searchParams.get("token") : getLocal("echoear-token");
if (params.searchParams.has("token")) {
  params.searchParams.delete("token");
  history.replaceState(null, "", params.pathname + params.search + params.hash);
  setLocal("echoear-token", initialToken);
}
let phone = getLocal("echoear-phone");
if (!/^phone-[0-9a-f]{4}$/.test(phone)) {
  phone = "phone-" + crypto.getRandomValues(new Uint16Array(1))[0].toString(16).padStart(4, "0");
  setLocal("echoear-phone", phone);
}
const wide = matchMedia("(min-width: 921px)");
const themeOf = d => ["cake", "mint", "plum", "sunny"].includes(d?.theme) ? d.theme : "cake";
const isLive = d => !!(d?.ws_connected ?? d?.ws);
const persisted = d => /^virt-[0-9a-f]{8}$/.test(d?.device_id || "");
const node = (tag, cls, text) => {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (text != null) element.textContent = text;
  return element;
};
const say = (id, text) => {
  const element = $(id);
  if (element.textContent !== text) element.textContent = text;
};
let app, view, ws, watched, retry, poll, rev = 0, pointer;
const keyed = new Map();
function send(event) {
  if (ws?.readyState !== WebSocket.OPEN) return false;
  ws.send(JSON.stringify(event));
  return true;
}
function updateStatus(text) {
  say("screen-status", text);
  say("screen-description", text);
}

function credentials(note = "") {
  say("auth-note", note);
  $("token-cancel").hidden = !app;
  if (!$("credentials").open) $("credentials").showModal();
}

function request(token, path, {method = "GET", body, signal} = {}) {
  const headers = {"X-EchoEar-Token": token};
  if (body) headers["Content-Type"] = "application/json";
  return fetch(path, {method, headers, body: body && JSON.stringify(body), signal, cache:"no-store"});
}

async function api(current, path, options) {
  const res = await request(current.token, path, options);
  if (res.status === 401 && app === current) {
    stop();
    setLocal("echoear-token", "");
    credentials("Your token is no longer accepted.");
  }
  if (!res.ok) throw new Error(`Request failed (${res.status})`);
  return res.status === 204 ? null : res.json();
}

function stop() {
  clearInterval(poll);
  view?.loading?.abort();
  view = app = null;
  disconnect();
  $("workspace").hidden = true;
  $("sidebar-toggle").hidden = true;
  say("link-state", "Not connected");
}

async function authorize(token) {
  $("connect").disabled = true;
  say("auth-note", "Checking token…");
  try {
    const res = await request(token, "/api/scope");
    if (res.status === 401 || res.status === 403) {
      if (!app) setLocal("echoear-token", "");
      credentials("That token wasn’t accepted. Check it and try again.");
      return;
    }
    const profile = res.ok ? await res.json() : null;
    if (res.status !== 404 && (typeof profile?.scoped !== "boolean" || profile.scoped && !profile.device_id)) {
      throw new Error("Unexpected scope response");
    }
    const scope = profile?.scoped ? profile : null;
    stop();
    setLocal("echoear-token", token);
    app = {token, scope, devices:[], deviceVersion:0, sidebar:!scope && wide.matches, tunnel:null};
    $("workspace").dataset.app = scope ? "scoped" : "lan";
    $("workspace").hidden = false;
    $("sidebar-toggle").hidden = !!scope;
    $("credentials").close();
    $("token").value = "";
    renderSidebar();
    if (scope) {
      $("share-url").value = "";
      $("open-link").removeAttribute("href");
      select({...scope, kind:"virtual"});
    } else {
      say("link-state", "Looking for cats…");
      renderDevices();
      select(null);
      refreshDevices();
      refreshTunnel();
      poll = setInterval(() => { refreshDevices(); refreshTunnel(); }, 5000);
    }
  } catch {
    credentials("Could not reach the bridge. Check the connection and try again.");
  } finally {
    $("connect").disabled = false;
  }
}

$("settings").onclick = () => credentials();
$("token-cancel").onclick = () => $("credentials").close();
$("credentials").oncancel = e => { if (!app || $("connect").disabled) e.preventDefault(); };
$("token-form").onsubmit = e => {
  e.preventDefault();
  const token = $("token").value.trim();
  if (token) authorize(token);
};

function renderSidebar() {
  $("sidebar").hidden = !app?.sidebar;
  $("workspace").dataset.sidebar = app?.sidebar ? "open" : "closed";
  $("sidebar-toggle").setAttribute("aria-expanded", String(!!app?.sidebar));
}
$("sidebar-toggle").onclick = () => { app.sidebar = !app.sidebar; renderSidebar(); };
wide.addEventListener("change", e => {
  if (app && !app.scope) { app.sidebar = e.matches; renderSidebar(); }
});

const rowParts = new WeakMap();
function deviceRow(d) {
  const button = node("button", "device");
  button.type = "button";
  button.dataset.id = d.device_id;
  const name = node("span", "device-name"), state = node("span", "pill device-state");
  const meta = node("span", "device-meta"), badge = node("span", "pill tunnel-badge", "HTTPS Tunnel");
  const dot = node("span", "theme-dot");
  dot.setAttribute("aria-hidden", "true");
  button.append(dot, name, state, meta, badge);
  rowParts.set(button, {name, state, meta, badge});
  button.onclick = () => {
    const selected = app.devices.find(cat => cat.device_id === button.dataset.id);
    if (selected) {
      select(selected);
      if (selected.kind !== "virtual" && !wide.matches) {
        app.sidebar = false;
        renderSidebar();
        $("sidebar-toggle").focus();
      }
    }
  };
  return button;
}

function renderDevices() {
  if (!app || app.scope) return;
  for (const [id, virtual] of [["physical", false], ["virtual", true]]) {
    const nav = $(id);
    const list = app.devices.filter(d => virtual ? persisted(d) : d.kind !== "virtual");
    const rows = new Map([...nav.querySelectorAll("button")].map(b => [b.dataset.id, b]));
    nav.querySelector("p")?.remove();
    for (const d of list) {
      const button = rows.get(d.device_id) ?? deviceRow(d);
      rows.delete(d.device_id);
      button.dataset.theme = themeOf(d);
      button.setAttribute("aria-pressed", String(view?.id === d.device_id));
      const {name, state, meta, badge} = rowParts.get(button);
      if (name.textContent !== d.character) name.textContent = d.character;
      state.dataset.status = isLive(d) ? "live" : d.online ? "http" : "off";
      state.textContent = isLive(d) ? "Live WS" : d.online ? "HTTP" : "Offline";
      meta.textContent = virtual ? "" : [
        isLive(d) ? d.mode : "",
        Number.isFinite(d.battery_pct) ? `Battery ${d.battery_pct}%` : "",
        Number.isFinite(d.volume) ? `Vol ${d.volume}%` : "",
      ].filter(Boolean).join(" · ");
      meta.hidden = !meta.textContent;
      badge.hidden = !d.tunnel?.active;
      if (!button.isConnected) nav.append(button);
    }
    for (const stale of rows.values()) stale.remove();
    if (!list.length) nav.append(node("p", "small", virtual ? "No saved Virtual Cats." : "No EchoEars found."));
  }
  renderTargets(app.devices);
  $("join").textContent = view?.quick ? "Switch to mirror" : "Join as Phone Cat";
  $("join").setAttribute("aria-pressed", String(!!view?.quick));
  renderTunnel();
}

function renderTargets(devices) {
  const target = $("target");
  const choices = devices.filter(d => d.online && d.device_id !== view?.id);
  const values = choices.map(d => `${d.device_id}:${d.character}`).join("\n");
  if (target.dataset.choices !== values) {
    const prior = target.value;
    target.replaceChildren(new Option("Ask my cat", ""), ...choices.map(d => new Option(`Intercom: ${d.character}`, d.device_id)));
    target.value = choices.some(d => d.device_id === prior) ? prior : "";
    target.dataset.choices = values;
  }
}

const preferredCat = devices => devices.find(d => d.kind !== "virtual" && isLive(d))
  || devices.find(d => d.kind !== "virtual") || devices.find(persisted);
async function refreshDevices() {
  const current = app;
  if (!current || current.scope) return;
  if (current.deviceRequest) { current.devicesDirty = true; return; }
  const version = current.deviceVersion;
  current.deviceRequest = true;
  try {
    const {devices} = await api(current, "/api/devices");
    if (app !== current || version !== current.deviceVersion) return;
    current.devices = devices;
    const updated = devices.find(d => d.device_id === view?.id);
    if (!view?.id || !updated && view.virtual && !view.quick && !current.tunnelBusy) {
      select(preferredCat(devices));
    } else if (updated) {
      view.device = updated;
      renderCurrent();
    }
    renderDevices();
    if ($("device-note").textContent === "Could not load devices.") say("device-note", "");
  } catch {
    if (app === current) say("device-note", "Could not load devices.");
  } finally {
    current.deviceRequest = false;
    if (app === current && current.devicesDirty) {
      current.devicesDirty = false;
      refreshDevices();
    }
  }
}

async function refreshTunnel() {
  const current = app;
  if (!current || current.scope || current.tunnelRequest) return;
  current.tunnelRequest = true;
  try {
    const {installed, service_running} = await api(current, "/api/tunnel/status");
    current.tunnel = {installed, service_running};
  } catch {
    current.tunnel = null;
  } finally {
    current.tunnelRequest = false;
    if (app === current) renderTunnel();
  }
}

function renderTunnel() {
  const status = app.tunnel;
  say("tunnel-status", !status ? "Service status unavailable." : !status.installed ? "CLI not installed." :
    status.service_running ? "Service running." : "Service not running.");
  $("install-note").hidden = status?.installed !== false;
  const d = view?.device, show = persisted(d), lock = !!app.tunnelBusy;
  $("tunnel-controls").hidden = !show;
  $("tunnel-hint").hidden = show;
  const tunnel = show ? d.tunnel : null;
  const share = tunnel?.active ? safeShareURL(tunnel.share_url) : "";
  if ($("share-url").value !== share) {
    $("share-url").value = share;
    say("copy", "Copy Share Link");
    if (share) $("open-link").href = share;
    else $("open-link").removeAttribute("href");
  }
  if (!show) return;
  say("tunnel-cat", d.character);
  $("share").hidden = !share;
  $("tunnel-create").hidden = !!tunnel?.active;
  $("tunnel-create").disabled = lock || (!status?.installed && !tunnel);
  say("tunnel-create", tunnel ? "Restore Tunnel" : "Create Tunnel");
  $("tunnel-close").hidden = !tunnel;
  $("tunnel-close").disabled = lock;
  $("delete-cat").disabled = lock;
  $("delete-cancel").hidden = !view.confirmDelete;
  $("delete-cancel").disabled = lock;
  say("delete-cat", view.confirmDelete ? "Confirm delete" : "Delete");
}

function safeShareURL(link) {
  try {
    const url = new URL(link);
    return url.protocol === "https:" && !url.username && !url.password ? url.href : "";
  } catch { return ""; }
}

$("create-cat").onsubmit = async e => {
  e.preventDefault();
  const current = app, form = e.currentTarget, button = form.querySelector("button");
  const {character, theme} = Object.fromEntries(new FormData(form));
  button.disabled = true;
  say("device-note", "Creating cat…");
  try {
    const cat = await api(current, "/api/virtual-clients", {method:"POST", body:{character:character.trim(), theme}});
    if (app !== current) return;
    const d = {...cat, device_id:cat.id, kind:"virtual"};
    current.deviceVersion++;
    current.devices.push(d);
    form.reset();
    $("new-cat").open = false;
    say("device-note", "Virtual Cat created.");
    select(d);
    refreshDevices();
  } catch {
    if (app === current) say("device-note", "Could not create that cat. Use 1–40 letters, numbers, spaces, or basic punctuation.");
  } finally {
    button.disabled = false;
  }
};

async function changeTunnel(action) {
  const current = app, cat = view, id = encodeURIComponent(cat.id);
  if (current.tunnelBusy) return;
  current.tunnelBusy = true;
  renderTunnel();
  say("tunnel-note", "Working…");
  try {
    const create = action === "create";
    const result = await api(current, create ? "/api/tunnels" :
      action === "delete" ? `/api/virtual-clients/${id}` : `/api/tunnels/${id}`,
    create ? {method:"POST", body:{device_id:cat.id}} : {method:"DELETE"});
    if (app !== current) return;
    current.deviceVersion++;
    const d = current.devices.find(d => d.device_id === cat.id);
    if (action === "delete") {
      current.devices = current.devices.filter(d => d.device_id !== cat.id);
      if (view === cat) select(preferredCat(current.devices));
      say("device-note", "Virtual Cat deleted.");
    } else if (d) {
      d.tunnel = result;
      if (view === cat) view.device = d;
    }
    if (view === cat) say("tunnel-note", create ? "Tunnel created." : "Tunnel closed.");
    refreshDevices();
    refreshTunnel();
  } catch {
    if (app === current && view === cat) say("tunnel-note", `Could not ${action} ${action === "delete" ? "the cat" : "the tunnel"}.`);
  } finally {
    current.tunnelBusy = false;
    if (app === current) renderDevices();
  }
}

$("tunnel-create").onclick = () => changeTunnel("create");
$("tunnel-close").onclick = () => changeTunnel("close");
$("delete-cat").onclick = () => {
  if (view.confirmDelete) changeTunnel("delete");
  else {
    view.confirmDelete = true;
    say("tunnel-note", "Delete this cat? Its share link will stop working.");
    renderTunnel();
  }
};
$("delete-cancel").onclick = () => {
  view.confirmDelete = false;
  say("tunnel-note", "");
  renderTunnel();
  $("delete-cat").focus();
};
$("copy").onclick = async () => {
  const field = $("share-url"), value = field.value;
  let copied = false;
  try {
    await navigator.clipboard.writeText(value);
    copied = true;
  } catch {
    field.focus();
    field.select();
    try { copied = document.execCommand("copy"); } catch {}
  }
  if (field.value !== value) return;
  say("copy", copied ? "Copied!" : "Copy Share Link");
  say("tunnel-note", copied ? "Share link copied." : "Copy the selected link with your keyboard.");
  if (copied) setTimeout(() => { if (field.value === value) say("copy", "Copy Share Link"); }, 2000);
};

$("join").onclick = () => {
  if (view?.quick) select(preferredCat(app.devices));
  else {
    audioReady();
    select({device_id:phone, theme:"plum", character:"Plum", kind:"virtual"}, true);
  }
};

function clearScene() {
  rev = 0;
  pointer = null;
  keyed.clear();
  nodes.replaceChildren();
  $("canvas-bg").setAttribute("fill", "#202432");
  $("screen-placeholder").removeAttribute("display");
  delete screen.dataset.dim;
  delete screen.dataset.interactive;
}

function select(d, quick = false) {
  const id = d?.device_id || "";
  if (id === view?.id) return;
  view?.loading?.abort();
  // A fresh socket keeps unlabelled scene/patch frames from crossing selections.
  disconnect();
  view = {id, device:d, virtual:d?.kind === "virtual", quick, entries:new Map()};
  $("text").value = "";
  $("target").value = "";
  say("chat-note", "");
  say("tunnel-note", "");
  say("voice-note", "");
  renderCurrent();
  renderDevices();
  renderTranscript(true);
  if (id) {
    if (!quick) loadHistory();
    connect();
  } else say("link-state", "No cat selected");
}

function renderCurrent() {
  const d = view?.device, ready = ws?.readyState === WebSocket.OPEN && rev > 0;
  const active = ready && view?.virtual;
  const mode = d?.mode && !["offline", "http"].includes(d.mode) ? d.mode : "";
  $("workspace").dataset.theme = themeOf(d);
  say("chat-name", d?.character || "Choose a cat");
  $("chat-mode").hidden = !d;
  say("chat-mode", !d ? "" : view.virtual ? active ? (modeLabel[view.mode] || "Idle") : "Connecting" :
    isLive(d) ? mode || "Live WS" : d.online ? "HTTP" : "Offline");
  $("screen-controls").hidden = !ready;
  renderVoice(d, view?.voicePending);
  $("phone-controls").hidden = !view?.virtual;
  const micAvailable = window.isSecureContext && !!navigator.mediaDevices?.getUserMedia && !!window.AudioWorkletNode;
  $("talk").disabled = !active || !micAvailable || !!mic?.stopping;
  $("sound").disabled = !active;
  $("mic-help").hidden = !view?.virtual || micAvailable;
  say("mic-help", window.isSecureContext ? "This browser can’t capture microphone audio." :
    "Mic needs localhost or an HTTPS opentunnel.xyz link");
  $("text").disabled = !d || view.virtual && !active;
  $("send").disabled = $("text").disabled || !!view?.sending || !!view?.resetting;
  $("reset").disabled = !d || view.virtual && !active || !!view?.resetting || !!view?.sending;
  $("refresh").disabled = !d || view.quick && !active || !!view?.loading || !!view?.resetting;
}

function renderVoice(device, pending) {
  $("voice-controls").hidden = !device;
  const voice = pending || device?.voice || "af_heart";
  for (const [id, option] of [["voice-heart", "af_heart"], ["voice-fable", "bm_fable"]]) {
    $(id).disabled = !!pending;
    $(id).setAttribute("aria-pressed", String(voice === option));
  }
  $("voice-select").disabled = !!pending;
  $("voice-select").value = voice;
}

async function setVoice(voice) {
  const cat = view, current = app;
  if (!cat?.id || cat.voicePending || cat.device.voice === voice) return;
  cat.voicePending = voice;
  say("voice-note", "");
  renderVoice(cat.device, voice);
  try {
    const result = await api(current, `/api/devices/${encodeURIComponent(cat.id)}/voice`, {method:"POST", body:{voice}});
    cat.device.voice = result.voice;
    current.deviceVersion++;
    const device = current.devices.find(d => d.device_id === cat.id);
    if (device) device.voice = result.voice;
  } catch {
    if (view === cat) say("voice-note", "Could not save voice.");
  } finally {
    cat.voicePending = null;
    if (view === cat) renderVoice(cat.device);
  }
}
$("voice-heart").onclick = () => setVoice("af_heart");
$("voice-fable").onclick = () => setVoice("bm_fable");
$("voice-select").onchange = e => setVoice(e.target.value);

function disconnect() {
  clearTimeout(retry);
  const old = ws;
  ws = null;
  old?.close();
  endMic(true, false);
  stopSound();
  clearScene();
  watched = null;
}

function connect(attempt = 0) {
  if (!app || !view?.id) return;
  disconnect();
  const cat = view;
  cat.mode = "";
  say("link-state", "Connecting…");
  updateStatus(cat.virtual ? `Joining as ${cat.device.character}…` : `Connecting to ${cat.device.character}…`);
  renderCurrent();
  const url = new URL("/ws", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  url.searchParams.set("token", app.token);
  const socket = new WebSocket(url);
  ws = socket;
  socket.binaryType = "arraybuffer";
  socket.onopen = () => {
    if (ws !== socket) return;
    send(cat.virtual ? {ev:"hello",role:"web",device_id:cat.id,character:cat.device.character,firmware:"web",volume}
      : {ev:"hello",role:"web"});
  };
  socket.onmessage = e => {
    if (ws !== socket) return;
    if (typeof e.data === "string") {
      try { command(JSON.parse(e.data)); } catch { updateStatus("Invalid screen update."); }
    } else if (cat.virtual) playPCM(e.data);
  };
  socket.onclose = () => {
    if (ws !== socket) return;
    if (rev) attempt = 0;
    disconnect();
    renderCurrent();
    say("link-state", "Reconnecting…");
    updateStatus("Live screen disconnected.");
    retry = setTimeout(() => connect(attempt + 1), Math.min(5000, 500 * 2 ** Math.min(attempt, 4)));
  };
}

function syncWatch(live) {
  if (view.virtual || ws?.readyState !== WebSocket.OPEN) return;
  const id = live ? view.id : "";
  if (watched !== id) {
    watched = id;
    clearScene();
    // Always subscribe by ID: the bridge may have reconnected since the peer snapshot.
    send({ev:"watch", device_id:view.id});
    const d = view.device;
    updateStatus(id ? `Opening ${d.character}’s live screen…` : d.online ?
      `${d.character} is online over HTTP. The live screen needs a WebSocket.` : `${d.character} is offline.`);
  }
}

const modeLabel = {idle:"Idle", thinking:"Thinking", listening:"Listening", speaking:"Speaking", sleepy:"Sleepy"};

function command(msg) {
  if (msg.type === "chat") {
    if (msg.device_id === view.id && msg.entry?.id) {
      view.entries.set(msg.entry.id, msg.entry);
      view.liveDuringLoad?.set(msg.entry.id, msg.entry);
      renderTranscript();
    }
    return;
  }
  switch (msg.op) {
    case "peers": {
      if (app.scope) {
        renderTargets(msg.devices);
      } else {
        const peer = msg.devices.find(d => d.device_id === view.id);
        if (!view.virtual) {
          say("link-state", "LAN dashboard");
          syncWatch(isLive(peer));
        }
        refreshDevices();
      }
      break;
    }
    case "scene": {
      const first = !rev;
      keyed.clear();
      nodes.replaceChildren();
      rev = msg.rev;
      $("canvas-bg").setAttribute("fill", msg.bg);
      $("screen-placeholder").setAttribute("display", "none");
      screen.dataset.interactive = "true";
      for (const n of msg.nodes) put(n);
      if (first) {
        say("link-state", app.scope ? "Private live cat" : "LAN dashboard");
        updateStatus(view.virtual ? `${view.device.character} · Live Virtual Cat` : `Mirroring ${view.device.character}`);
        loadHistory({replace:false});
      }
      renderCurrent();
      break;
    }
    case "patch":
      if (msg.rev !== rev + 1) {
        if (!view.virtual || app.scope) send({ev:"watch", device_id:view.id});
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
    case "mode":
      if (view.virtual) {
        view.mode = msg.mode;
        renderCurrent();
      }
      break;
    case "volume":
      setVolume(msg.level);
      break;
    case "chime":
      if (view.virtual) chime(msg.kind);
      break;
    case "rec":
      if (view.virtual) {
        if (msg.action === "start") startMic(msg.target || "");
        else if (msg.action === "stop") endMic(false);
      }
      break;
    case "tts":
      if (view.virtual) {
        if (msg.state === "start") endedAudio = null;
        if (msg.state === "end") { endedAudio = msg.audio ?? 0; ackIfDone(); }
      }
      break;
    case "error":
      say("chat-note", "The bridge couldn’t complete the live action.");
      if (msg.status === 404 && view.virtual && !app.scope) refreshDevices();
      break;
  }
}

const dayFormat = new Intl.DateTimeFormat(undefined, {dateStyle:"medium"});
const timeFormat = new Intl.DateTimeFormat(undefined, {hour:"2-digit", minute:"2-digit", hourCycle:"h23"});
function bubble(entry) {
  if (entry.role === "system") {
    const item = node("li", "pill chat-divider", entry.text);
    item.dataset.entry = entry.id;
    return item;
  }
  const role = ["assistant", "user", "intercom"].includes(entry.role) ? entry.role : "assistant";
  const item = node("li", `bubble ${role}`);
  item.dataset.entry = entry.id;
  const catName = view.device.character;
  const sender = entry.sender || (role === "user" ? "You" : catName);
  const recipient = entry.recipient || (sender !== catName ? catName : "");
  const label = role === "intercom" ? recipient ? `${sender} → ${recipient}` : `From ${sender}` : sender;
  const header = node("div", "row wrap bubble-head");
  header.append(node("strong", "", label));
  if (role === "intercom") header.append(node("span", "pill", "Intercom"));
  else if (role === "user") header.append(node("span", "pill", {voice:"Voice", tap:"Tap"}[entry.source] || "Text"));
  const when = new Date(entry.timestamp);
  if (Number.isFinite(when.valueOf())) {
    const time = node("time", "", timeFormat.format(when));
    time.dateTime = when.toISOString();
    header.append(time);
  }
  item.append(header, node("p", "bubble-text", entry.text));
  if (entry.scene_title || entry.tool_summary) {
    const meta = node("div", "row wrap bubble-meta");
    if (entry.scene_title) meta.append(node("span", "pill", `Scene: ${entry.scene_title}`));
    if (entry.tool_summary) meta.append(node("span", "pill", `Tool: ${entry.tool_summary}`));
    item.append(meta);
  }
  return item;
}

function renderTranscript(toBottom = false) {
  const list = $("messages"), scroll = $("transcript"), top = scroll.scrollTop;
  const follow = toBottom || scroll.scrollHeight - scroll.clientHeight - top <= 80;
  const anchor = follow ? null : [...list.children].find(el => el.dataset.entry && el.offsetTop + el.offsetHeight > top);
  const offset = anchor?.offsetTop;
  const entries = [...view.entries.values()].sort((a, b) => new Date(a.timestamp) - new Date(b.timestamp));
  const content = document.createDocumentFragment();
  let lastDay = "";
  for (const entry of entries) {
    const when = new Date(entry.timestamp);
    const day = Number.isFinite(when.valueOf()) ? dayFormat.format(when) : "";
    if (day && day !== lastDay) content.append(node("li", "day", day));
    lastDay = day;
    content.append(bubble(entry));
  }
  list.replaceChildren(content);
  $("empty-chat").hidden = entries.length !== 0;
  say("empty-chat", !view.id ? "Choose a cat to open its chat." : view.loading ? "Loading chat…" :
    view.historyError ? "Could not load chat. Try Refresh." : "No messages yet — say hi or tap the mic!");
  if (follow) scroll.scrollTop = scroll.scrollHeight;
  else {
    const replacement = anchor && [...list.children].find(el => el.dataset.entry === anchor.dataset.entry);
    scroll.scrollTop = replacement ? top + replacement.offsetTop - offset : top;
  }
}

async function loadHistory({replace = true} = {}) {
  const cat = view, current = app;
  if (!cat?.id || cat.resetting) return;
  cat.loading?.abort();
  const loading = new AbortController(), live = new Map();
  cat.loading = loading;
  cat.liveDuringLoad = live;
  cat.historyError = false;
  renderCurrent();
  if (!cat.entries.size) renderTranscript();
  try {
    const {messages} = await api(current, `/api/devices/${encodeURIComponent(cat.id)}/messages`, {signal:loading.signal});
    if (view !== cat || cat.loading !== loading) return;
    const merged = new Map();
    for (const entry of messages) if (entry.id) merged.set(entry.id, entry);
    if (!replace) {
      for (const [id, entry] of cat.entries) if (!merged.has(id)) merged.set(id, entry);
    }
    for (const [id, entry] of live) merged.set(id, entry);
    cat.entries = merged;
  } catch {
    if (!loading.signal.aborted) cat.historyError = true;
  } finally {
    if (view === cat && cat.loading === loading) {
      cat.loading = cat.liveDuringLoad = null;
      renderCurrent();
      renderTranscript();
    }
  }
}

$("refresh").onclick = () => loadHistory();
$("reset").onclick = async () => {
  const cat = view, current = app;
  cat.loading?.abort();
  cat.loading = cat.liveDuringLoad = null;
  cat.resetting = true;
  renderCurrent();
  say("chat-note", "Starting a new chat…");
  try {
    await api(current, "/api/reset", {method:"POST", body:{device_id:cat.id}});
    if (view !== cat) return;
    cat.resetting = false;
    say("chat-note", "New chat started.");
    loadHistory();
  } catch {
    if (view === cat) say("chat-note", "Could not start a new chat.");
  } finally {
    cat.resetting = false;
    if (view === cat) renderCurrent();
  }
};

$("prompt").onsubmit = async e => {
  e.preventDefault();
  const cat = view, current = app, input = $("text"), text = input.value.trim();
  if (!text || !cat?.id || $("send").disabled) return;
  if (cat.virtual) audioReady();
  cat.sending = true;
  renderCurrent();
  say("chat-note", "Sending…");
  try {
    const result = await api(current, "/api/chat", {method:"POST", body:{device_id:cat.id, text}});
    if (view !== cat) return;
    if (input.value.trim() === text) input.value = "";
    say("chat-note", "");
    if (!rev || result.reply) loadHistory({replace:false});
  } catch {
    if (view === cat) say("chat-note", "Could not send. Your message is still in the composer.");
  } finally {
    cat.sending = false;
    if (view === cat) renderCurrent();
  }
};

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
    case "tri": {
      const filled = n.fill !== false, color = n.c || "#F8FAFC";
      marks = [el("polygon",{points:`${n.x1 ?? 0},${n.y1 ?? 0} ${n.x2 ?? 0},${n.y2 ?? 0} ${n.x3 ?? 0},${n.y3 ?? 0}`,
        fill:filled ? color : "none", stroke:filled ? "none" : color, "stroke-width":n.sw ?? 2,
        "stroke-linejoin":"round"})];
      break;
    }
    case "arc": marks = [el("path",{d:arcPath(n),fill:"none",...stroke})]; break;
    case "text": marks = [textMark(n,n.x,n.y)]; break;
    default: return;
  }
  group.replaceChildren(...marks);
}

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
  if (view.virtual && !cancelled) audioReady();
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
  if (view.virtual) audioReady();
  send({ev:"tap",id:g.dataset.id,x:180,y:180});
});
$("volume").oninput = e => { $("volume-value").value = e.target.value + "%"; };
$("volume").onchange = e => {
  const level = +e.target.value, old = volume;
  setVolume(level);
  send({ev:"head",action:level < old ? "vol_down" : "vol_up",volume:level});
};

let audio, gain, playAt = 0, volume = 80, endedAudio = null, muted = false;
const speaking = new Set();
function speakerUI() {
  say("sound", audio?.state !== "running" ? "Enable speaker" : muted ? "Unmute speaker" : "Mute speaker");
}
function setVolume(n) {
  volume = n;
  $("volume").value = n;
  $("volume-value").value = n + "%";
  if (gain) gain.gain.value = muted ? 0 : n / 100;
  syncSpeaker();
}
function canPlay() {
  return audio?.state === "running" && !muted && volume > 0;
}
function syncSpeaker() {
  speakerUI();
  if (!canPlay()) {
    clearPCM();
    ackIfDone();
  }
}
async function audioReady() {
  try {
    if (!audio) {
      audio = new AudioContext();
      gain = audio.createGain();
      gain.gain.value = muted ? 0 : volume / 100;
      gain.connect(audio.destination);
      audio.onstatechange = syncSpeaker;
    }
    if (audio.state !== "running") await audio.resume();
    speakerUI();
    return audio.state === "running";
  } catch {
    return false;
  }
}
$("sound").onclick = async () => {
  if (audio?.state === "running") {
    muted = !muted;
    setVolume(volume);
  } else if (!await audioReady()) {
    say("chat-note", "The browser couldn’t enable audio.");
  }
};
function playPCM(raw) {
  if (!(raw instanceof ArrayBuffer) || raw.byteLength < 3 || raw.byteLength % 2 !== 1 || new Uint8Array(raw)[0] !== 2) return;
  if (!canPlay() || mic) return;
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
  if (endedAudio !== null && speaking.size === 0) {
    send({ev:"tts_ack", state:"done", audio:endedAudio});
    endedAudio = null;
  }
}
function clearPCM() {
  for (const source of speaking) {
    source.onended = null;
    source.stop();
    source.disconnect();
  }
  speaking.clear();
  playAt = 0;
}
function stopSound() {
  endedAudio = null;
  clearPCM();
}
function chime(kind) {
  if (kind === "cancel") stopSound();
  if (!canPlay()) return;
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
  $("talk").textContent = active ? "Stop & send" : "Mic · Talk";
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
  clearPCM();
  ackIfDone();
  const rec = {target,stream:null,node:null,source:null,stopping:false};
  mic = rec;
  recordUI(true);
  try {
    if (!await audioReady() || !view?.virtual || mic !== rec || ws?.readyState !== WebSocket.OPEN) throw new Error();
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
          say("chat-note", "Recording cancelled: the connection is too slow.");
        } else ws.send(event.data);
      }
    };
    send({ev:"rec_start",target});
    rec.source.connect(rec.node).connect(audio.destination);
    say("chat-note", target ? "Recording intercom…" : "Listening…");
  } catch {
    if (mic === rec) {
      endMic(true);
      say("chat-note", "Could not open the microphone. Check browser permissions.");
    }
  }
}
function endMic(cancelled, tell = true, flushed = false) {
  if (!mic) return;
  const rec = mic;
  if (rec.node && !cancelled && !flushed) {
    if (!rec.stopping) {
      rec.stopping = true;
      $("talk").disabled = true;
      $("talk").textContent = "Finishing…";
      rec.flushTimer = setTimeout(() => {
        if (mic === rec) rec.node.port.postMessage("flush");
      }, 200);
    }
    return;
  }
  clearTimeout(rec.flushTimer);
  mic = null;
  rec.stream?.getTracks().forEach(t => t.stop());
  rec.source?.disconnect();
  rec.node?.disconnect();
  rec.node?.port.close();
  recordUI(false);
  renderCurrent();
  if (tell) {
    cancelled ||= !rec.node;
    send({ev:"rec_end",cancelled,target:rec.target});
    say("chat-note", cancelled ? "Recording cancelled." : "Message sent to the bridge.");
  }
}
$("talk").onclick = () => { if (mic) endMic(false); else startMic($("target").value); };
$("cancel").onclick = () => endMic(true);
credentials();
if (initialToken) authorize(initialToken);
