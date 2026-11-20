import {api} from "./api.js";
import {audioReady} from "./audio.js";
import {$, node, say} from "./dom.js";
import {renderCurrent} from "./live.js";
import {state} from "./state.js";

const dayFormat = new Intl.DateTimeFormat(undefined, {dateStyle:"medium"});
const timeFormat = new Intl.DateTimeFormat(undefined, {hour:"2-digit", minute:"2-digit", hourCycle:"h23"});
function bubble(entry) {
  if (entry.role === "card" || entry.role === "system") {
    const item = node("li", "pill chat-divider", entry.text);
    item.dataset.entry = entry.id;
    return item;
  }
  const role = ["user", "intercom"].includes(entry.role) ? entry.role : "cat";
  const item = node("li", `bubble ${role}`);
  item.dataset.entry = entry.id;
  const catName = state.view.device.character;
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
  return item;
}

function renderTranscript(toBottom = false) {
  const list = $("messages"), scroll = $("transcript"), top = scroll.scrollTop;
  const follow = toBottom || scroll.scrollHeight - scroll.clientHeight - top <= 80;
  const anchor = follow ? null : [...list.children].find(el => el.dataset.entry && el.offsetTop + el.offsetHeight > top);
  const offset = anchor?.offsetTop;
  const entries = [...state.view.entries.values()].sort((a, b) => new Date(a.timestamp) - new Date(b.timestamp));
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
  say("empty-chat", !state.view.id ? "Choose a cat to open its chat." : state.view.loading ? "Loading chat…" :
    state.view.historyError ? "Could not load chat. Try Refresh." : "No messages yet — say hi or tap the mic!");
  if (follow) scroll.scrollTop = scroll.scrollHeight;
  else {
    const replacement = anchor && [...list.children].find(el => el.dataset.entry === anchor.dataset.entry);
    scroll.scrollTop = replacement ? top + replacement.offsetTop - offset : top;
  }
}

async function loadHistory({replace = true} = {}) {
  const cat = state.view, current = state.app;
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
    if (state.view !== cat || cat.loading !== loading) return;
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
    if (state.view === cat && cat.loading === loading) {
      cat.loading = cat.liveDuringLoad = null;
      renderCurrent();
      renderTranscript();
    }
  }
}

$("refresh").onclick = () => loadHistory();
$("reset").onclick = async () => {
  const cat = state.view, current = state.app;
  if (!current || current.scope || !cat?.id) return;
  cat.loading?.abort();
  cat.loading = cat.liveDuringLoad = null;
  cat.resetting = true;
  renderCurrent();
  say("chat-note", "Starting a new chat…");
  try {
    await api(current, "/api/reset", {method:"POST", body:{device_id:cat.id}});
    if (state.view !== cat) return;
    cat.resetting = false;
    say("chat-note", "New chat started.");
    loadHistory();
  } catch {
    if (state.view === cat) say("chat-note", "Could not start a new chat.");
  } finally {
    cat.resetting = false;
    if (state.view === cat) renderCurrent();
  }
};

$("prompt").onsubmit = async e => {
  e.preventDefault();
  const cat = state.view, current = state.app, input = $("text"), text = input.value.trim();
  if (!text || !cat?.id || $("send").disabled) return;
  if (cat.virtual) audioReady();
  cat.sending = true;
  renderCurrent();
  say("chat-note", "Sending…");
  try {
    const result = await api(current, "/api/chat", {method:"POST", body:{device_id:cat.id, text}});
    if (state.view !== cat) return;
    if (input.value.trim() === text) input.value = "";
    say("chat-note", "");
    if (!state.rev || result.reply) loadHistory({replace:false});
  } catch {
    if (state.view === cat) say("chat-note", "Could not send. Your message is still in the composer.");
  } finally {
    cat.sending = false;
    if (state.view === cat) renderCurrent();
  }
};

$("share-photo").onclick = () => $("photo-file").click();
$("photo-file").onchange = async e => {
  const file = e.target.files[0], cat = state.view, current = state.app;
  e.target.value = "";
  if (!file || !cat?.id || $("share-photo").disabled) return;
  if (file.size > 2 * 1024 * 1024) {
    say("chat-note", "Choose a photo under 2 MiB.");
    return;
  }
  const body = new FormData();
  body.append("photo", file);
  cat.sharingPhoto = true;
  renderCurrent();
  say("chat-note", "Sending photo…");
  try {
    const {displayed} = await api(current, `/api/devices/${encodeURIComponent(cat.id)}/photo`, {method:"POST", body});
    if (state.view !== cat) return;
    say("chat-note", displayed ? "Photo sent." : "Photo saved. This cat is offline.");
    if (!displayed || !state.rev) loadHistory({replace:false});
  } catch {
    if (state.view === cat) say("chat-note", "Could not send the photo. Use a PNG, JPEG or WebP.");
  } finally {
    cat.sharingPhoto = false;
    if (state.view === cat) renderCurrent();
  }
};

export {loadHistory, renderTranscript};
