import {$, say} from "./dom.js";
import {send, state} from "./state.js";

let audio, gain, playAt = 0, volume = 55, endedAudio = null, muted = false;
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
  return audio?.state === "running" && canBufferPCM();
}
function canBufferPCM() {
  return audio && audio.state !== "closed" && !muted && volume > 0;
}
function syncSpeaker() {
  speakerUI();
  // A suspended context retains its clock and scheduled PCM until it resumes.
  if (!canBufferPCM()) {
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
  if (!canBufferPCM() || mic) return;
  const wire = new DataView(raw,1), count = wire.byteLength / 2;
  const buffer = audio.createBuffer(1,count,16000), samples = buffer.getChannelData(0);
  for (let i = 0; i < count; i++) samples[i] = wire.getInt16(i*2,true)/32768;
  const source = audio.createBufferSource();
  source.buffer = buffer;
  source.connect(gain);
  const now = audio.currentTime;
  if (playAt <= now) playAt = now + .035;
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
function handleTTS(msg) {
  if (msg.state === "start") endedAudio = null;
  if (msg.state === "end") {
    endedAudio = msg.audio ?? 0;
    ackIfDone();
  }
  if (msg.state === "abort") stopSound();
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
let recordingChanged;
function onRecordingChanged(handler) { recordingChanged = handler; }
function micStopping() { return !!mic?.stopping; }
function recordUI(active) {
  $("talk").setAttribute("aria-pressed", String(active));
  $("talk").hidden = active;
  $("mic-send").hidden = !active;
  $("mic-send").disabled = true;
  $("mic-send").textContent = "Send now";
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
    if (!await audioReady() || !state.view?.virtual || mic !== rec || state.ws?.readyState !== WebSocket.OPEN) throw new Error();
    rec.stream = await navigator.mediaDevices.getUserMedia({audio:{channelCount:1,echoCancellation:true,noiseSuppression:false,autoGainControl:false}});
    if (mic !== rec) { rec.stream.getTracks().forEach(t=>t.stop()); return; }
    workletLoaded ||= audio.audioWorklet.addModule("/app/mic.js").catch(err => { workletLoaded = null; throw err; });
    await workletLoaded;
    if (mic !== rec) return;
    rec.source = audio.createMediaStreamSource(rec.stream);
    rec.node = new AudioWorkletNode(audio,"echo-mic",{channelCount:1,channelCountMode:"explicit"});
    rec.node.port.onmessage = event => {
      if (mic !== rec) return;
      if (event.data === "end" || event.data === "auto_end") {
        endMic(false, true, true);
      } else if (state.ws?.readyState === WebSocket.OPEN) {
        if (state.ws.bufferedAmount > 256*1024) {
          endMic(true);
          say("chat-note", "Recording cancelled: the connection is too slow.");
        } else state.ws.send(event.data);
      }
    };
    send({ev:"rec_start",target});
    rec.source.connect(rec.node).connect(audio.destination);
    $("mic-send").disabled = false;
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
      $("mic-send").disabled = true;
      $("mic-send").textContent = "Sending…";
      rec.node.port.postMessage("flush");
    }
    return;
  }
  mic = null;
  rec.stream?.getTracks().forEach(t => t.stop());
  rec.source?.disconnect();
  rec.node?.disconnect();
  rec.node?.port.close();
  recordUI(false);
  recordingChanged?.();
  if (tell) {
    cancelled ||= !rec.node;
    send({ev:"rec_end",cancelled,target:rec.target});
    say("chat-note", cancelled ? "Recording cancelled." : "Message sent to the bridge.");
  }
}
$("talk").onclick = () => startMic($("target").value);
$("mic-send").onclick = () => endMic(false);
$("cancel").onclick = () => endMic(true);

export {audioReady, chime, endMic, handleTTS, micStopping, onRecordingChanged, playPCM, setVolume, startMic, stopSound, volume};
