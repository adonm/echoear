const $ = id => document.getElementById(id);
const note = text => { $("parent-note").textContent = text; };
const auth = $("parent-auth"), controls = $("parent-controls"), picker = $("parent-device");
let access, devices = [], pending = false;

try { $("bridge-token").value = localStorage.getItem("echoear-token") || ""; } catch {}

async function request(path, body) {
  const {bridge, parent} = access;
  const headers = {"X-EchoEar-Token": bridge, "X-EchoEar-Parent-Token": parent};
  const options = {headers, cache:"no-store"};
  if (body) {
    headers["Content-Type"] = "application/json";
    Object.assign(options, {method:"POST", body:JSON.stringify(body)});
  }
  const res = await fetch(path, options);
  const result = await res.json();
  if (!res.ok) throw new Error(result.error || "The bridge could not complete the request.");
  return result;
}

function showDevice() {
  const cat = devices.find(d => d.device_id === picker.value);
  $("parent-selected").hidden = !cat;
  $("no-devices").hidden = !!devices.length;
  picker.disabled = !cat;
  if (!cat) return;
  const s = cat.settings, secs = cat.used_seconds;
  const labels = {quiet_hours:"Quiet hours", daily_limit:"Daily time used", paused:"Paused", unavailable:"Controls unavailable"};
  $("parent-status").textContent = labels[cat.reason] || "Available";
  $("parent-usage").textContent = `${Math.floor(secs / 60)} min ${secs % 60} sec used${s.daily_minutes ? ` of ${s.daily_minutes} min` : ""} · ${cat.day} (${s.timezone})`;
  const recent = Object.entries({"Talk turns":"talk_turns", "App launches":"app_launches", "Calls":"calls", "Intercom notes":"intercom", "Timer cards":"timer_cards"});
  $("parent-activity").replaceChildren(...recent.flatMap(([label, key]) => {
    const term = document.createElement("dt"), count = document.createElement("dd");
    term.textContent = label;
    count.textContent = cat.recent[key];
    return [term, count];
  }));
  $("parent-history").href = `/app?device_id=${encodeURIComponent(cat.device_id)}#chat`;
  $("parent-timezone").value = s.timezone;
  $("parent-start").value = s.quiet_start;
  $("parent-end").value = s.quiet_end;
  $("parent-budget").value = s.daily_minutes;
  const paused = s.pause_until_ms > Date.now();
  const until = paused ? s.pause_until_ms : s.override_until_ms;
  $("parent-until").textContent = until > Date.now() ?
    `${paused ? "Paused" : "Overridden"} until ${new Date(until).toLocaleString([], {timeZone:s.timezone})} (${s.timezone}).` : "";
}

async function refresh() {
  const previous = picker.value;
  ({devices} = await request("/api/parent"));
  picker.replaceChildren(...devices.map(d => new Option(`${d.character} (${d.device_id})`, d.device_id)));
  if (devices.some(d => d.device_id === previous)) picker.value = previous;
  showDevice();
}

async function work(f, success) {
  if (pending) return;
  pending = true;
  note("");
  try {
    await f();
    note(success);
  } catch (err) {
    note(err.message || "Cannot reach the bridge.");
  } finally {
    pending = false;
  }
}

auth.onsubmit = e => {
  e.preventDefault();
  work(async () => {
    access = {bridge:$("bridge-token").value, parent:$("parent-token").value};
    $("parent-token").value = "";
    try { await refresh(); } catch (err) { access = null; throw err; }
    auth.hidden = true;
    controls.hidden = false;
    (picker.disabled ? $("parent-refresh") : picker).focus();
  }, "Parent controls opened.");
};

picker.onchange = showDevice;
$("parent-refresh").onclick = () => work(refresh, "Usage refreshed.");
$("parent-signout").onclick = () => {
  if (pending) return;
  access = null;
  devices = [];
  picker.replaceChildren();
  controls.hidden = true;
  auth.hidden = false;
  note("Signed out of parent controls.");
  $("parent-token").focus();
};

function save(body, success) {
  const id = picker.value;
  if (!id) return;
  work(async () => {
    const updated = await request(`/api/devices/${encodeURIComponent(id)}/parent`, body);
    const cat = devices.find(d => d.device_id === id);
    Object.assign(cat, updated);
    showDevice();
  }, success);
}

$("parent-settings").onsubmit = e => {
  e.preventDefault();
  save({action:"settings", timezone:$("parent-timezone").value.trim(), quiet_start:$("parent-start").value,
    quiet_end:$("parent-end").value, daily_minutes:$("parent-budget").valueAsNumber}, "Rules saved.");
};
$("parent-actions").onsubmit = e => {
  e.preventDefault();
  const action = e.submitter?.value;
  if (!action) return;
  save(action === "end_override" ? {action} : {action, minutes:$("parent-minutes").valueAsNumber},
    {end_override:"Override ended.", pause:"Cat paused.", unlock:"Cat unlocked."}[action]);
};
