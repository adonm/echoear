#!/usr/bin/env python3
import hashlib
import hmac
import io
import ipaddress
import json
import logging
import math
import os
import re
import secrets
import stat
import threading
import time
import wave
from collections import OrderedDict
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPException
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from textwrap import wrap
from urllib.error import HTTPError

from backend import DATA, JSON, LOG, PROJECT, Error, Sessions, _http_request, json_bytes, reachable, transcribe
from state import BROADCAST, CHARACTER, DEVICE_ID, Peers, StateError, lan_ip
from tts import MAX_PCM, MAX_TEXT, SAMPLE_RATE, Speech, make_intercom_chime_pcm, normalize_voice_pcm, plain_text, short_speech

MAX_JSON, MAX_SCREEN, MAX_MESSAGE, MAX_PEER_PCM = 16 * 1024, 6000, 400, 1536 * 1024
PCM, WAV = "audio/pcm", "audio/wav"
TTS_WAIT = 20
TTS_ERRORS = (ValueError, RuntimeError, OSError)
FENCE = re.compile(r"^[ \t]*```echoear-(screen|message)[ \t]*(?:\r?\n|\Z)(.*?)(^[ \t]*```[ \t]*\r?$|\Z)",
                   re.MULTILINE | re.DOTALL | re.IGNORECASE)
SHAPES = {
    "circle": ("x y r", "w"), "rect": ("x y w h", "r"), "line": ("x1 y1 x2 y2", "w"),
    "arc": ("x y r a1 a2", "w"), "text": ("x y", ""), "btn": ("x y w h", "r"),
}


def elapsed_ms(start):
    return (time.monotonic_ns() - start) // 1_000_000


def pcm_wav(pcm):
    if not pcm or len(pcm) % 2:
        raise Error(400, "PCM must contain whole 16-bit samples")
    output = io.BytesIO()
    with wave.open(output, "wb") as file:
        file.setparams((1, 2, SAMPLE_RATE, 0, "NONE", ""))
        file.writeframes(pcm)
    return output.getvalue()


def wav_pcm(wav):
    try:
        with wave.open(io.BytesIO(wav), "rb") as file:
            if (file.getframerate(), file.getnchannels(), file.getsampwidth()) == (SAMPLE_RATE, 1, 2):
                pcm = file.readframes(file.getnframes())
                if pcm and len(pcm) == file.getnframes() * 2:
                    return pcm
    except (wave.Error, EOFError):
        pass
    raise Error(400, "Intercom requires 16 kHz mono 16-bit PCM")


def object_json(raw, limit):
    if len(raw) <= limit:
        try:
            data = json.loads(raw)
            return data if isinstance(data, dict) else None
        except (ValueError, RecursionError):
            pass


def short_text(value, limit, plain=False):
    value = plain_text(value) if plain else value
    return "".join(c for c in value if c.isprintable()).strip()[:limit] if isinstance(value, str) else ""


def color(value):
    return value.upper() if isinstance(value, str) and re.fullmatch(r"#[0-9A-Fa-f]{6}", value) else None


def shape(value):
    if not isinstance(value, dict):
        return None
    kind, tint = value.get("t"), color(value.get("c"))
    if not isinstance(kind, str) or kind not in SHAPES or not tint:
        return None
    required, optional = (fields.split() for fields in SHAPES[kind])
    coords = {k: int(min(360 if k in ("a1", "a2") else 359, max(0, n))) for k in required + optional if
              type(n := value.get(k)) is int or (type(n) is float and math.isfinite(n))}
    if not all(k in coords for k in required):
        return None
    result = dict(t=kind, c=tint, **coords)
    if kind in ("text", "btn"):
        if not (label := short_text(value.get("s"), 32)):
            return None
        result["s"] = label
    if kind in ("circle", "rect") and type(value.get("fill")) is bool:
        result["fill"] = value["fill"]
    actions = {"tap": 120, "ask": 120, **({"reply_to": 40} if kind == "btn" else {})}
    result.update({k: s for k, limit in actions.items() if (s := short_text(value.get(k), limit, plain=True))})
    return result


def _sanitize_screen(raw):
    data = object_json(raw, 64 * 1024)
    if data is None or not isinstance(items := data.get("shapes", []), list):
        return None
    screen = {k: s for k, limit in (("title", 48), ("subtitle", 80)) if (s := short_text(data.get(k), limit))}
    if bg := color(data.get("bg")):
        screen["bg"] = bg
    shapes = [s for item in items[:24] if (s := shape(item))]
    if not screen and not shapes:
        return None
    screen.setdefault("bg", "#0B1020")
    screen["shapes"] = shapes
    while len(json_bytes(screen)) >= MAX_SCREEN:
        shapes.pop()
    return screen


def message_payload(raw):
    if data := object_json(raw, MAX_JSON):
        target, text = data.get("to"), short_text(data.get("text"), MAX_MESSAGE, plain=True)
        if isinstance(target, str) and re.fullmatch(r"[\w .'’-]{1,64}", target.strip()) and text:
            return target.strip(), text


def extract_payloads(reply):
    payloads = {}

    def strip(match):
        kind, raw, closed = match.groups()
        kind = kind.lower()
        if closed and kind not in payloads:
            parse = _sanitize_screen if kind == "screen" else message_payload
            if payload := parse(raw):
                payloads[kind] = payload
        return ""

    return FENCE.sub(strip, reply).strip(), payloads.get("screen"), payloads.get("message")


def message_card(sender, text):
    character = sender["character"]
    lines = wrap(text or "Voice message", width=27, max_lines=3, placeholder="…")
    shapes = [dict(t="text", x=180, y=124 + i * 25, c="#FFF1C8", s=line) for i, line in enumerate(lines)]
    shapes.append(dict(t="btn", x=85, y=218, w=190, h=46, r=23, c="#34D399",
                       s=f"Reply to {character}"[:32], reply_to=sender.get("device_id") or character))
    return dict(bg="#1E1B3A", title=f"From {character}!", shapes=shapes)


class AudioStream:
    def __init__(self, pcm=b""):
        self.chunks, self.done = ([pcm] if pcm else []), bool(pcm)
        self.error, self.cond, self.total_bytes = None, threading.Condition(), len(pcm)

    def finish(self, error=None):
        with self.cond:
            if not self.done:
                self.error, self.done = error, True
                self.cond.notify_all()

    def chunk(self, index):
        with self.cond:
            if not self.cond.wait_for(lambda: index < len(self.chunks) or self.done, timeout=TTS_WAIT):
                self.finish("TTS stream timed out")
                LOG.warning("TTS stream timed out")
            if self.error:
                raise Error(502, self.error)
            return self.chunks[index] if index < len(self.chunks) else None

    def produce(self, speech, text):
        failed = True
        try:
            for pcm in speech.synthesize_sentences(text):
                with self.cond:
                    if self.done:
                        return
                    if not pcm or len(pcm) % 2 or self.total_bytes + len(pcm) > MAX_PCM:
                        raise ValueError("Invalid PCM stream size")
                    self.chunks.append(pcm)
                    self.total_bytes += len(pcm)
                    self.cond.notify_all()
            failed = False
        except TTS_ERRORS as error:
            LOG.warning("TTS sentence stream: %s", type(error).__name__)
        finally:
            self.finish("TTS stream failed" if failed else None)


class Bridge(ThreadingHTTPServer):
    def __init__(self, address, token, ota_dir, sessions):
        self.token, self.firmware, self.sessions = token.encode(), ota_dir / "echoear.bin", sessions
        self.peers, self.speech = Peers(sessions.store.path.with_name("devices.json")), None
        self.audio, self.audio_lock = OrderedDict(), threading.Lock()
        super().__init__(address, BridgeHandler)

    def cache_audio(self, stream):
        key = secrets.token_urlsafe(18)
        with self.audio_lock:
            self.audio[key] = stream
            if len(self.audio) > 16:
                self.audio.popitem(last=False)
        return f"/api/audio/{key}"

    def cached_audio(self, key):
        with self.audio_lock:
            if key in self.audio:
                self.audio.move_to_end(key)
                return self.audio[key]
        raise Error(404, "Audio not found")

    def post_peer(self, peer, path, content_type, body):
        try:
            address = lan_ip(peer["ip"], allow_loopback=True)
            host = f"[{address}]" if ":" in address else address
            headers = {"X-EchoEar-Token": self.token.decode("ascii"), "Content-Type": content_type}
            with _http_request(f"http://{host}:{peer['port']}{path}", "POST", body, headers, 5) as response:
                return 200 <= response.status < 300
        except (OSError, HTTPException, ValueError) as error:
            if isinstance(error, HTTPError):
                error.close()
            LOG.warning("Peer %s %s: %s", peer["device_id"], path, type(error).__name__)
            return False

    def deliver(self, sender, peers, text, raw_pcm=None):
        result = dict(delivered_to=[], sent_to=[], failed_to=[])
        if not peers:
            return dict(result, error="peer_not_found")
        text = short_text(text, MAX_MESSAGE, plain=True)
        display = message_card(sender, text)
        try:
            speech, character = self.speech, sender["character"]
            if speech is None:
                raise RuntimeError("TTS unavailable")
            intro = short_speech(speech, f"{'From' if raw_pcm else 'Message from'} {character}!")
            message = normalize_voice_pcm(raw_pcm) if raw_pcm else speech.synthesize_pcm(text)
            audio = make_intercom_chime_pcm() + intro + message
        except TTS_ERRORS as error:
            LOG.warning("Intercom audio: %s", type(error).__name__)
            return dict(result, error="audio_unavailable", failed_to=[p["device_id"] for p in peers])
        if len(audio) > MAX_PEER_PCM:
            return dict(result, error="message_too_long", failed_to=[p["device_id"] for p in peers])

        def send(peer):
            display_ok = self.post_peer(peer, "/display", JSON, display)
            audio_ok = self.post_peer(peer, "/speak", PCM, audio)
            return display_ok and audio_ok

        with ThreadPoolExecutor(max_workers=min(len(peers), 8)) as workers:
            for peer, ok in zip(peers, workers.map(send, peers)):
                result["sent_to" if ok else "failed_to"].append(peer["device_id"])
                if ok:
                    result["delivered_to"].append(peer["character"])
        if not result["sent_to"]:
            result["error"] = "delivery_failed"
        return result


class BridgeHandler(BaseHTTPRequestHandler):
    protocol_version, timeout = "HTTP/1.1", 15

    def log_message(self, *_):
        pass

    def payload_headers(self, status, content_type, length):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        if length is None:
            self.send_header("Transfer-Encoding", "chunked")
        else:
            self.send_header("Content-Length", str(length))
        self.send_header("Connection", "close")
        self.send_header("Cache-Control", "no-store")

    def send_bytes(self, body, kind=JSON, status=200):
        self.payload_headers(status, kind, len(body))
        self.end_headers()
        self.wfile.write(body)

    def send_audio(self, stream):
        with stream.cond:
            if stream.error:
                raise Error(502, stream.error)
            length = stream.total_bytes if stream.done else None
        self.payload_headers(200, PCM, length)
        self.end_headers()
        if length is not None:
            self.wfile.writelines(stream.chunks)
            return
        index = 0
        try:
            while chunk := stream.chunk(index):
                self.wfile.write(f"{len(chunk):X}\r\n".encode() + chunk + b"\r\n")
                self.wfile.flush()
                index += 1
        except Error:
            return  # No terminating chunk: an interrupted stream must not look complete.
        self.wfile.write(b"0\r\n\r\n")

    def body(self, audio=False):
        kind, lengths = self.headers.get_content_type(), self.headers.get_all("Content-Length", [])
        if kind not in ((PCM, WAV) if audio else (JSON,)):
            raise Error(415, "Expected audio/pcm or audio/wav" if audio else "Expected application/json")
        if self.headers.get("Transfer-Encoding") or len(lengths) > 1:
            raise Error(400, "Request requires a single Content-Length")
        if not lengths:
            raise Error(411, "Content-Length required")
        if not re.fullmatch(r"[0-9]{1,12}", lengths[0]):
            raise Error(400, "Invalid Content-Length")
        size = int(lengths[0])
        if size > (MAX_PCM if audio else MAX_JSON):
            raise Error(413, "Request body too large")
        try:
            data = self.rfile.read(size)
        except TimeoutError:
            raise Error(408, "Request body timed out") from None
        if len(data) != size:
            raise Error(400, "Incomplete request body")
        if kind == PCM:
            return pcm_wav(data)
        if kind == WAV:
            if len(data) < 12 or data[:4] != b"RIFF" or data[8:12] != b"WAVE":
                raise Error(400, "Expected RIFF/WAVE audio")
            return data
        try:
            value = json.loads(data)
        except (ValueError, RecursionError):
            raise Error(400, "Invalid JSON") from None
        if not isinstance(value, dict):
            raise Error(400, "Expected a JSON object")
        return value

    def device(self, body, required=False):
        default = None if required else self.headers.get("X-Device-Id", "echoear")
        device = body.get("device_id", default)
        if not isinstance(device, str) or not DEVICE_ID.fullmatch(device):
            raise Error(400, "Invalid device ID")
        return device

    def identity(self, body, register=False):
        device = self.device(body, required=register)
        source = self.client_address[0]
        character = body.get("character") if register else self.headers.get("X-Character")
        if register or character is not None:
            if not isinstance(character, str) or not CHARACTER.fullmatch(character.strip()):
                raise Error(400, "Invalid character name")
            character = character.strip()
        try:
            ip = lan_ip(body.get("ip", source) if register else source, ipaddress.ip_address(source).is_loopback)
        except ValueError as error:
            raise Error(400, str(error)) from None
        if not register:
            return self.server.peers.touch(device, ip, character)
        port = body.get("port", 80)
        if type(port) is not int or not 1 <= port <= 65535:
            raise Error(400, "Invalid peer port")
        self.server.peers.register(device, ip, character, port)
        return dict(ok=True, peers=self.server.peers.public(device))

    def speech(self, text):
        try:
            clean = Speech.spoken_text(text)
        except ValueError:
            raise Error(400, f"Expected spoken text of 1–{MAX_TEXT} characters") from None
        if self.server.speech is None:
            raise Error(503, "TTS unavailable")
        return self.server.speech, clean

    def answer(self, reply, sid, screen=None, opencode_ms=0, *, cached=False, **extra):
        result = dict(reply=reply, session_id=sid, screen=screen, opencode_ms=opencode_ms, **extra,
                      audio_url=None, audio_bytes=0, sample_rate=SAMPLE_RATE, audio_streaming=False,
                      tts_first_ms=0, tts_ms=0)
        if reply:
            start = time.monotonic_ns()
            try:
                speech, clean = self.speech(reply)
                stream = AudioStream(short_speech(speech, clean) if cached else b"")
                if not cached:
                    threading.Thread(target=stream.produce, args=(speech, clean), daemon=True).start()
                if stream.chunk(0):
                    with stream.cond:
                        if not stream.error:
                            result.update(audio_url=self.server.cache_audio(stream),
                                          audio_bytes=stream.total_bytes if stream.done else 0, audio_streaming=not stream.done)
            except (Error, *TTS_ERRORS) as error:
                LOG.warning("Reply audio: %s", type(error).__name__)
            result["tts_first_ms"] = result["tts_ms"] = elapsed_ms(start)
        return result

    def relay(self, sender, target, message, pcm=None):
        device = sender["device_id"]
        peers = self.server.peers.targets(device, target)
        result = self.server.deliver(sender, peers, message, pcm)
        recipient = "everyone" if target.casefold() in BROADCAST else peers[0]["character"] if peers else target
        delivered = bool(result["delivered_to"])
        if not delivered:
            reply = f"I couldn't reach {recipient} right now."
        elif result["failed_to"]:
            reply = "I sent your message, but couldn't reach everyone."
        else:
            reply = f"Sent to {recipient}!"
        return self.answer(reply, self.server.sessions.get(device), cached=True, ok=delivered, **result)

    def turn(self, sender, text, wav=None):
        if not isinstance(text, str) or not text.strip():
            raise Error(400, "Expected nonempty text")
        device = sender["device_id"]
        if direct := self.server.peers.direct(device, text):
            return self.relay(sender, *direct, pcm=wav_pcm(wav) if wav is not None else None)
        start = time.monotonic_ns()
        reply, sid = self.server.sessions.ask(device, self.server.peers.prompt(sender, text.strip()))
        opencode_ms = elapsed_ms(start)
        reply, screen, message = extract_payloads(reply)
        result = {"sent_to": []}
        if message:
            target, text = message
            result = self.server.deliver(sender, self.server.peers.targets(device, target), text)
        return self.answer(reply, sid, screen, opencode_ms, **result)

    def voice(self, intercom=False):
        target = self.headers.get("X-Target", "").strip()
        if intercom and (not target or len(target) > 64):
            raise Error(400, "Expected X-Target peer name")
        wav = self.body(audio=True)
        pcm = wav_pcm(wav) if intercom else None
        sender = self.identity({})
        start = time.monotonic_ns()
        transcript = transcribe(wav)
        hex_ms = elapsed_ms(start)
        if intercom:
            answer = self.relay(sender, target, transcript, pcm)
        elif transcript:
            answer = self.turn(sender, transcript, wav)
        else:
            answer = self.answer("", self.server.sessions.get(sender["device_id"]))
        return dict(transcript=transcript, hex_ms=hex_ms, **answer)

    def ota(self, manifest):
        try:
            file = self.server.firmware.open("rb")
        except FileNotFoundError:
            if manifest:
                return dict(exists=False, size=0, sha256="", mtime=0)
            raise Error(404, "Firmware not found") from None
        with file:
            info = os.fstat(file.fileno())
            if not stat.S_ISREG(info.st_mode):
                raise Error(404, "Firmware not found")
            digest = hashlib.file_digest(file, "sha256").hexdigest()
            if manifest:
                return dict(exists=True, size=info.st_size, sha256=digest, mtime=int(info.st_mtime))
            file.seek(0)
            self.payload_headers(200, "application/octet-stream", info.st_size)
            self.send_header("X-Firmware-Sha256", digest)
            self.end_headers()
            remaining = info.st_size
            while remaining and (chunk := file.read(min(remaining, 64 * 1024))):
                self.wfile.write(chunk)
                remaining -= len(chunk)

    def route(self, path):
        match self.command, path:
            case "GET", "/health":
                speech = self.server.speech
                return dict(ok=True, hex=reachable("Hex", "/health"), opencode=reachable("OpenCode", "/api/info"),
                            tts=speech is not None, tts_engine=speech.engine if speech else "unavailable")
            case "GET", "/api/devices":
                return dict(devices=self.server.peers.public())
            case "GET", "/ota/version.json" | "/ota/latest.bin":
                return self.ota(path.endswith(".json"))
            case "GET", _ if path.startswith("/api/audio/"):
                return self.send_audio(self.server.cached_audio(path.removeprefix("/api/audio/")))
            case "POST", "/api/voice" | "/api/intercom":
                return self.voice(path == "/api/intercom")
            case "POST", "/api/register":
                return self.identity(self.body(), register=True)
            case "POST", "/api/text":
                body = self.body()
                return self.turn(self.identity(body), body.get("text"))
            case "POST", "/api/reset":
                self.server.sessions.reset(self.device(self.body()))
                return {"ok": True}
            case "POST", "/api/tts":
                speech, text = self.speech(self.body().get("text"))
                try:
                    pcm = speech.synthesize_pcm(text)
                except TTS_ERRORS as error:
                    LOG.warning("Direct TTS: %s", type(error).__name__)
                    raise Error(502, "TTS synthesis failed") from None
                return self.send_bytes(pcm, PCM)
        raise Error(404, "Not found")

    def dispatch(self):
        path = self.path.partition("?")[0]
        try:
            if path.startswith(("/api/", "/ota/")):
                token = self.headers.get_all("X-EchoEar-Token", [])
                if len(token) != 1 or not hmac.compare_digest(token[0].encode("latin-1"), self.server.token):
                    raise Error(401, "Unauthorized")
            if (result := self.route(path)) is not None:
                self.send_bytes(json_bytes(result))
        except StateError as error:
            LOG.warning("%s", error)
            self.send_bytes(json_bytes({"error": str(error)}), status=502)
        except Error as error:
            self.send_bytes(json_bytes({"error": str(error)}), status=error.status)
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            pass

    do_GET = do_POST = dispatch


def main():
    logging.basicConfig(level=logging.WARNING, format="%(name)s: %(message)s")
    token = os.environ.get("ECHOEAR_BRIDGE_TOKEN", "")
    if not re.fullmatch(r"[!-~]+", token):
        raise SystemExit("Set ECHOEAR_BRIDGE_TOKEN to a nonempty ASCII token without whitespace")
    agent = os.environ.get("ECHOEAR_OPENCODE_AGENT", "echoear")
    spec = os.environ.get("ECHOEAR_OPENCODE_MODEL") or "opencode/gemini-4-neon#high"
    match = re.fullmatch(r"([^/]+)/([^#]+)(?:#(.+))?", spec)
    if not match:
        raise SystemExit("ECHOEAR_OPENCODE_MODEL must be provider/model or provider/model#variant")
    provider, model_id, variant = match.groups()
    model = dict(providerID=provider, id=model_id, **({"variant": variant} if variant else {}))
    address = os.environ.get("ECHOEAR_BRIDGE_HOST", "0.0.0.0"), int(os.environ.get("ECHOEAR_BRIDGE_PORT", "8765"))
    ota = Path(os.environ.get("ECHOEAR_OTA_DIR", PROJECT.parent / "firmware/build"))
    with Bridge(address, token, ota, Sessions(DATA / "echoear/sessions.json", agent, model)) as bridge:
        try:
            start = time.monotonic_ns()
            speech = Speech(DATA / "echoear/tts-models")
            speech.synthesize_pcm("Hello! Let's find something fun to learn.")
            bridge.speech = speech
            print(f"EchoEar TTS ready: {speech.engine}/{speech.voice} ({elapsed_ms(start)} ms startup)", flush=True)
        except (ImportError, *TTS_ERRORS) as error:
            LOG.warning("TTS startup: %s", type(error).__name__)
        print(f"EchoEar bridge listening on {bridge.server_address[0]}:{bridge.server_address[1]}", flush=True)
        bridge.serve_forever()


if __name__ == "__main__":
    main()
