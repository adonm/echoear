import {api} from "./api.js";
import {audioReady, setVolume, volume} from "./audio.js";
import {loadHistory} from "./chat.js";
import {$, colors, getLocal, isLive, node, persisted, say, setLocal, wide} from "./dom.js";
import {loadSavedCats} from "./library.js";
import {renderCurrent, select} from "./live.js";
import {state} from "./state.js";

let phone = getLocal("echoear-phone");
if (!/^phone-[0-9a-f]{4}$/.test(phone)) {
  phone = "phone-" + crypto.getRandomValues(new Uint16Array(1))[0].toString(16).padStart(4, "0");
  setLocal("echoear-phone", phone);
}

function renderSidebar() {
  $("sidebar").hidden = !state.app?.sidebar;
  $("workspace").dataset.sidebar = state.app?.sidebar ? "open" : "closed";
  $("sidebar-toggle").setAttribute("aria-expanded", String(!!state.app?.sidebar));
}

function renderReassign() {
  const select = $("reassign-target");
  const choices = state.view?.id ? state.app.devices.filter(d => d.device_id !== state.view.id) : [];
  const version = JSON.stringify(choices.map(d => [d.device_id, d.character]));
  if (select.dataset.choices !== version) {
    const previous = select.value;
    select.replaceChildren(new Option("Choose a device…", ""),
      ...choices.map(d => new Option(`${d.character} · ${d.device_id}`, d.device_id)));
    select.value = choices.some(d => d.device_id === previous) ? previous : "";
    select.dataset.choices = version;
  }
  select.disabled = !!state.app.reassigning || !choices.length;
  $("swap-cat").disabled = $("move-cat").disabled = select.disabled || !select.value;
}

async function reassignCat(mode) {
  const current = state.app, selected = state.view, to = $("reassign-target").value;
  if (!current || current.scope || current.reassigning || !selected?.id || !to) return;
  current.reassigning = true;
  renderReassign();
  say("reassign-note", mode === "swap" ? "Swapping cats…" : "Moving cat…");
  try {
    await api(current, "/api/cats/reassign", {method:"POST",
      body:{from_device_id:selected.id, to_device_id:to, mode}});
    if (state.app !== current) return;
    refreshDevices();
    loadSavedCats();
    if (state.view === selected) {
      say("reassign-note", mode === "swap" ? "Cats swapped." : "Cat moved. This device now has a blank starter.");
      loadHistory();
    }
  } catch {
    if (state.app === current && state.view === selected) say("reassign-note", "Could not reassign cats.");
  } finally {
    current.reassigning = false;
    if (state.app === current) renderReassign();
  }
}
$("reassign-target").onchange = renderReassign;
$("swap-cat").onclick = () => reassignCat("swap");
$("move-cat").onclick = () => reassignCat("move");

$("sidebar-toggle").onclick = () => { state.app.sidebar = !state.app.sidebar; renderSidebar(); };
wide.addEventListener("change", e => {
  if (state.app && !state.app.scope) { state.app.sidebar = e.matches; renderSidebar(); }
});

const rowParts = new WeakMap();
function deviceRow(d) {
  const button = node("button", "device");
  button.type = "button";
  button.dataset.id = d.device_id;
  const name = node("span", "device-name"), status = node("span", "pill device-state");
  const meta = node("span", "device-meta"), badge = node("span", "pill tunnel-badge", "HTTPS Tunnel");
  const dot = node("span", "cat-dot");
  dot.setAttribute("aria-hidden", "true");
  button.append(dot, name, status, meta, badge);
  rowParts.set(button, {name, status, meta, badge});
  button.onclick = () => {
    const selected = state.app.devices.find(cat => cat.device_id === button.dataset.id);
    if (selected) {
      select(selected);
      if (selected.kind !== "virtual" && !wide.matches) {
        state.app.sidebar = false;
        renderSidebar();
        $("sidebar-toggle").focus();
      }
    }
  };
  return button;
}

function renderDevices() {
  if (!state.app || state.app.scope) return;
  for (const [id, virtual] of [["physical", false], ["virtual", true]]) {
    const nav = $(id);
    const list = state.app.devices.filter(d => virtual ? persisted(d) : d.kind !== "virtual");
    const rows = new Map([...nav.querySelectorAll("button")].map(b => [b.dataset.id, b]));
    nav.querySelector("p")?.remove();
    for (const d of list) {
      const button = rows.get(d.device_id) ?? deviceRow(d);
      rows.delete(d.device_id);
      colors(button, d.cat);
      button.setAttribute("aria-pressed", String(state.view?.id === d.device_id));
      const {name, status, meta, badge} = rowParts.get(button);
      if (name.textContent !== d.character) name.textContent = d.character;
      status.dataset.status = isLive(d) ? "live" : d.online ? "http" : "off";
      status.textContent = isLive(d) ? "Live WS" : "Offline";
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
  renderTargets(state.app.devices);
  $("join").textContent = state.view?.quick ? "Switch to mirror" : "Join as Phone Cat";
  $("join").setAttribute("aria-pressed", String(!!state.view?.quick));
  renderTunnel();
  renderReassign();
}

function renderTargets(devices) {
  const target = $("target");
  const choices = devices.filter(d => d.online && d.device_id !== state.view?.id);
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
  const current = state.app;
  if (!current || current.scope) return;
  if (current.deviceRequest) { current.devicesDirty = true; return; }
  const version = current.deviceVersion;
  current.deviceRequest = true;
  try {
    const {devices} = await api(current, "/api/devices");
    if (state.app !== current || version !== current.deviceVersion) return;
    current.devices = devices;
    const updated = devices.find(d => d.device_id === state.view?.id);
    if (!state.view?.id || !updated && state.view.virtual && !state.view.quick && !current.tunnelBusy) {
      select(preferredCat(devices));
    } else if (updated) {
      state.view.device = updated;
      renderCurrent();
    }
    renderDevices();
    if ($("device-note").textContent === "Could not load devices.") say("device-note", "");
  } catch {
    if (state.app === current) say("device-note", "Could not load devices.");
  } finally {
    current.deviceRequest = false;
    if (state.app === current && current.devicesDirty) {
      current.devicesDirty = false;
      refreshDevices();
    }
  }
}

async function refreshTunnel() {
  const current = state.app;
  if (!current || current.scope || current.tunnelRequest) return;
  current.tunnelRequest = true;
  try {
    const {installed, service_running} = await api(current, "/api/tunnel/status");
    current.tunnel = {installed, service_running};
  } catch {
    current.tunnel = null;
  } finally {
    current.tunnelRequest = false;
    if (state.app === current) renderTunnel();
  }
}

function renderTunnel() {
  const status = state.app.tunnel;
  say("tunnel-status", !status ? "Service status unavailable." : !status.installed ? "CLI not installed." :
    status.service_running ? "Service running." : "Service not running.");
  $("install-note").hidden = status?.installed !== false;
  const d = state.view?.device, show = persisted(d), lock = !!state.app.tunnelBusy;
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
  $("delete-cancel").hidden = !state.view.confirmDelete;
  $("delete-cancel").disabled = lock;
  say("delete-cat", state.view.confirmDelete ? "Confirm delete" : "Delete");
}

function safeShareURL(link) {
  try {
    const url = new URL(link);
    return url.protocol === "https:" && !url.username && !url.password ? url.href : "";
  } catch { return ""; }
}

$("create-cat").onclick = async e => {
  const current = state.app, button = e.currentTarget;
  button.disabled = true;
  say("device-note", "Creating cat…");
  try {
    const cat = await api(current, "/api/virtual-clients", {method:"POST", body:{}});
    if (state.app !== current) return;
    const d = {...cat, device_id:cat.id, kind:"virtual"};
    current.deviceVersion++;
    current.devices.push(d);
    say("device-note", "Say hi to design your new cat!");
    select(d);
    if (!wide.matches) {
      current.sidebar = false;
      renderSidebar();
      $("sidebar-toggle").focus();
    }
    refreshDevices();
  } catch {
    if (state.app === current) say("device-note", "Could not create a cat.");
  } finally {
    button.disabled = false;
  }
};

async function changeTunnel(action) {
  const current = state.app, cat = state.view, id = encodeURIComponent(cat.id);
  if (current.tunnelBusy) return;
  current.tunnelBusy = true;
  renderTunnel();
  say("tunnel-note", "Working…");
  try {
    const create = action === "create";
    const result = await api(current, create ? "/api/tunnels" :
      action === "delete" ? `/api/virtual-clients/${id}` : `/api/tunnels/${id}`,
    create ? {method:"POST", body:{device_id:cat.id}} : {method:"DELETE"});
    if (state.app !== current) return;
    current.deviceVersion++;
    const d = current.devices.find(d => d.device_id === cat.id);
    if (action === "delete") {
      current.devices = current.devices.filter(d => d.device_id !== cat.id);
      if (state.view === cat) select(preferredCat(current.devices));
      say("device-note", "Virtual Cat deleted.");
    } else if (d) {
      d.tunnel = result;
      if (state.view === cat) state.view.device = d;
    }
    if (state.view === cat) say("tunnel-note", create ? "Tunnel created." : "Tunnel closed.");
    refreshDevices();
    refreshTunnel();
  } catch {
    if (state.app === current && state.view === cat) say("tunnel-note", `Could not ${action} ${action === "delete" ? "the cat" : "the tunnel"}.`);
  } finally {
    current.tunnelBusy = false;
    if (state.app === current) renderDevices();
  }
}

$("tunnel-create").onclick = () => changeTunnel("create");
$("tunnel-close").onclick = () => changeTunnel("close");
$("delete-cat").onclick = () => {
  if (state.view.confirmDelete) changeTunnel("delete");
  else {
    state.view.confirmDelete = true;
    say("tunnel-note", "Delete this cat? Its share link will stop working.");
    renderTunnel();
  }
};
$("delete-cancel").onclick = () => {
  state.view.confirmDelete = false;
  say("tunnel-note", "");
  renderTunnel();
  $("delete-cat").focus();
};
async function copyField(field) {
  try {
    await navigator.clipboard.writeText(field.value);
    return true;
  } catch {
    field.focus();
    field.select();
    try { return document.execCommand("copy"); } catch { return false; }
  }
}

$("copy").onclick = async () => {
  const field = $("share-url"), value = field.value;
  const copied = await copyField(field);
  if (field.value !== value) return;
  say("copy", copied ? "Copied!" : "Copy Share Link");
  say("tunnel-note", copied ? "Share link copied." : "Copy the selected link with your keyboard.");
  if (copied) setTimeout(() => { if (field.value === value) say("copy", "Copy Share Link"); }, 2000);
};

$("join").onclick = () => {
  if (state.view?.quick) select(preferredCat(state.app.devices));
  else {
    audioReady();
    select({device_id:phone, character:"New Cat", kind:"virtual"}, true);
  }
};

const quickVoices = [$("voice-first"), $("voice-second")];
function loadVoiceCatalog(voices) {
  $("voice-select").replaceChildren(...voices.map(voice => new Option(voice.name, voice.id)));
  for (const [i, button] of quickVoices.entries()) {
    button.hidden = !voices[i];
    button.value = voices[i]?.id ?? "";
    button.textContent = voices[i]?.name ?? "";
    button.onclick = () => setVoice(button.value);
  }
}

function renderVoice(device, pending) {
  $("voice-controls").hidden = !device;
  const voice = pending || device?.voice || "";
  for (const button of quickVoices) {
    button.disabled = !!pending;
    button.setAttribute("aria-pressed", String(voice === button.value));
  }
  $("voice-select").disabled = !!pending;
  $("voice-select").value = voice;
}

async function setVoice(voice) {
  const cat = state.view, current = state.app;
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
    if (state.view === cat) say("voice-note", "Could not save voice.");
  } finally {
    cat.voicePending = null;
    if (state.view === cat) renderVoice(cat.device);
  }
}
$("voice-select").onchange = e => setVoice(e.target.value);

$("volume").oninput = e => { $("volume-value").value = e.target.value + "%"; };
$("volume").onchange = e => saveVolume({auto:false, volume:+e.target.value});
$("volume-auto").onclick = () => {
  if (state.view?.device?.auto_volume === false) saveVolume({auto:true});
};

function renderVolume() {
  $("volume-auto").setAttribute("aria-pressed", String(state.view?.device?.auto_volume !== false));
  $("volume-auto").disabled = $("volume").disabled = !!state.view?.volumePending;
}

function applyVolume(current, cat, level, auto_volume) {
  if (!cat?.device || !Number.isFinite(level)) return;
  const fields = {volume:level, ...(typeof auto_volume === "boolean" && {auto_volume})};
  Object.assign(cat.device, fields);
  current.deviceVersion++;
  const device = current.devices.find(d => d.device_id === cat.id);
  if (device) Object.assign(device, fields);
  if (state.view === cat) {
    setVolume(level);
    renderVolume();
  }
  if (state.app === current) renderDevices();
}

async function saveVolume(body) {
  const cat = state.view, current = state.app;
  if (!cat?.id || cat.volumePending) return;
  cat.volumePending = true;
  renderVolume();
  try {
    const result = await api(current, `/api/devices/${encodeURIComponent(cat.id)}/volume`, {method:"POST", body});
    applyVolume(current, cat, result.volume, result.auto_volume);
  } catch {
    if (state.view === cat) {
      setVolume(volume);
      say("chat-note", "Could not save volume.");
    }
  } finally {
    cat.volumePending = false;
    if (state.view === cat) renderVolume();
  }
}

export {applyVolume, copyField, loadVoiceCatalog, refreshDevices, refreshTunnel, renderDevices, renderSidebar, renderTargets, renderVoice, renderVolume};
