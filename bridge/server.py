#!/usr/bin/env python3
"""Bridge EchoEar devices to the local Hex and OpenCode services."""

import base64
import hashlib
import hmac
import http.client
import io
import json
import math
import os
import re
import secrets
import stat
import sys
import tempfile
import threading
import time
import wave
from collections import OrderedDict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlencode, urlsplit

from tts import MAX_TEXT, SAMPLE_RATE, Speech, plain_text

DATA = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share")
STATE = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state")
PROJECT = Path(__file__).resolve().parent
DEVICE_ID = re.compile(r"[A-Za-z0-9_.-]{1,64}")
SESSION_ID = re.compile(r"ses[A-Za-z0-9_-]{1,128}")
MAX_AUDIO = 2 * 1024 * 1024
MAX_JSON = 16 * 1024
MAX_SCREEN_BYTES = 6000
COLOR = re.compile(r"#[0-9A-Fa-f]{6}")
SCREEN_FENCE = re.compile(
    r"^[ \t]*```echoear-screen[ \t]*(?:\r?\n|\Z)(.*?)(^[ \t]*```[ \t]*\r?$|\Z)",
    re.MULTILINE | re.DOTALL | re.IGNORECASE,
)
SHAPE_FIELDS = {
    "circle": (("x", "y", "r"), ("w",)),
    "rect": (("x", "y", "w", "h"), ("r",)),
    "line": (("x1", "y1", "x2", "y2"), ("w",)),
    "arc": (("x", "y", "r", "a1", "a2"), ("w",)),
    "text": (("x", "y"), ()),
    "btn": (("x", "y", "w", "h"), ("r",)),
}


class Error(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status


class UpstreamStatus(Error):
    def __init__(self, service, status):
        super().__init__(502, f"{service} returned HTTP {status}")
        self.upstream_status = status


def upstream(service, method, path, body=None, content_type="application/json", timeout=5):
    """Re-read discovery on every call so local service restarts rotate credentials."""
    try:
        if service == "Hex":
            registration = json.loads((DATA / "hex/local-api.json").read_text())
            port = registration["port"]
            authorization = "Bearer " + registration["token"]
        else:
            registration = json.loads((STATE / "opencode/service.json").read_text())
            address = urlsplit(registration["url"])
            if address.scheme != "http" or address.hostname not in (
                "0.0.0.0", "::", "127.0.0.1", "::1", "localhost"
            ):
                raise ValueError("not a local OpenCode URL")
            port = address.port
            credentials = f"opencode:{registration['password']}".encode()
            authorization = "Basic " + base64.b64encode(credentials).decode("ascii")
        if type(port) is not int or not 1 <= port <= 65535:
            raise ValueError("invalid service port")
        host = "::1" if service == "OpenCode" and address.hostname in ("::", "::1") else "127.0.0.1"
        connection = http.client.HTTPConnection(host, port, timeout=timeout)
        try:
            headers = {"Authorization": authorization}
            if body is not None:
                headers["Content-Type"] = content_type
                if content_type == "application/json":
                    body = json.dumps(body).encode()
            connection.request(method, path, body, headers)
            response = connection.getresponse()
            if not 200 <= response.status < 300:
                raise UpstreamStatus(service, response.status)
            payload = response.read(4 * 1024 * 1024 + 1)
            if len(payload) > 4 * 1024 * 1024:
                raise ValueError("oversized service response")
            return json.loads(payload) if payload else None
        finally:
            connection.close()
    except TimeoutError:
        raise Error(504, f"{service} timed out") from None
    except (OSError, http.client.HTTPException, KeyError, TypeError, ValueError):
        raise Error(502, f"{service} unavailable or returned invalid data") from None


def reachable(service, path):
    try:
        upstream(service, "GET", path, timeout=1.5)
        return True
    except Error:
        return False


def elapsed_ms(start):
    return (time.monotonic_ns() - start) // 1_000_000


def pcm_wav(pcm):
    if not pcm or len(pcm) % 2:
        raise Error(400, "PCM must contain whole 16-bit samples")
    output = io.BytesIO()
    with wave.open(output, "wb") as wav:
        wav.setparams((1, 2, 16000, 0, "NONE", ""))
        wav.writeframes(pcm)
    return output.getvalue()


def transcribe(wav):
    try:
        settings = json.loads((DATA / "hex/settings.json").read_text())
        selection = settings["transcription"]
        model = selection["model"]
        if not isinstance(model, str) or not model:
            raise ValueError("missing model")
    except (OSError, KeyError, TypeError, ValueError):
        raise Error(502, "Hex selected transcription model unavailable") from None
    query = urlencode({"model": model, "language": selection.get("language", "en")})
    result = upstream("Hex", "POST", "/transcriptions?" + query, wav, "audio/wav", timeout=60)
    if not isinstance(result, dict) or not isinstance(result.get("transcript"), str):
        raise Error(502, "Hex returned an invalid transcript")
    return result["transcript"].strip()


def final_reply(messages, prompt_id):
    # Descending timeline: idle, the final answer, then the admitted user input.
    reply = None
    for message in messages:
        if message["id"] == prompt_id:
            if reply:
                return reply
            break
        if message["type"] == "idle" and message["outcome"] != "succeeded":
            status = (message.get("error") or {}).get("status")
            if status == 429:
                raise Error(503, "OpenCode model usage limit reached")
            raise Error(502, "OpenCode turn did not complete successfully")
        if message["type"] == "assistant" and reply is None and message.get("finish") in ("stop", "length"):
            reply = "\n".join(part["text"] for part in message["content"] if part["type"] == "text").strip()
    raise Error(502, "OpenCode did not return a final assistant reply")


def json_bytes(value):
    return json.dumps(value, separators=(",", ":")).encode()


def screen_text(value, limit):
    if not isinstance(value, str):
        return ""
    return "".join(char for char in value if char.isprintable()).strip()[:limit]


def screen_color(value):
    return value.upper() if isinstance(value, str) and COLOR.fullmatch(value) else None


def screen_number(value):
    if type(value) is int or (type(value) is float and math.isfinite(value)):
        return int(min(360, max(0, value)))
    return None


def screen_shape(value):
    if not isinstance(value, dict):
        return None
    kind = value.get("t")
    color = screen_color(value.get("c"))
    if not isinstance(kind, str) or kind not in SHAPE_FIELDS or not color:
        return None
    result = {"t": kind, "c": color}
    required, optional = SHAPE_FIELDS[kind]
    for key in required + optional:
        number = screen_number(value.get(key))
        if number is not None:
            result[key] = number
        elif key in required:
            return None
    if kind in ("text", "btn"):
        label = screen_text(value.get("s"), 32)
        if not label:
            return None
        result["s"] = label
    if kind in ("circle", "rect") and type(value.get("fill")) is bool:
        result["fill"] = value["fill"]
    for key in ("tap", "ask"):
        if action := screen_text(plain_text(value.get(key)), 120):
            result[key] = action
    return result


def sanitize_screen(raw):
    if len(raw) > 64 * 1024:
        return None
    try:
        value = json.loads(raw)
    except (ValueError, RecursionError):
        return None
    if not isinstance(value, dict) or not isinstance(value.get("shapes", []), list):
        return None
    screen = {}
    if bg := screen_color(value.get("bg")):
        screen["bg"] = bg
    for key, limit in (("title", 48), ("subtitle", 80)):
        if label := screen_text(value.get(key), limit):
            screen[key] = label
    shapes = [shape for item in value.get("shapes", [])[:24] if (shape := screen_shape(item))]
    if not screen and not shapes:
        return None
    screen.setdefault("bg", "#0B1020")
    screen["shapes"] = shapes
    while len(json_bytes(screen)) >= MAX_SCREEN_BYTES:
        shapes.pop()
    return screen


def split_screen(reply):
    screen = None

    def strip_block(match):
        nonlocal screen
        if screen is None and match[2]:
            screen = sanitize_screen(match[1])
        return ""

    reply = SCREEN_FENCE.sub(strip_block, reply).strip()
    return reply, screen


class Sessions:
    def __init__(self, path, agent, model):
        self.path = path
        self.agent = agent
        self.model = model
        self.permissions = [{"action": "*", "resource": "*", "effect": "deny"}]
        if agent == "echoear":
            self.permissions.append({"action": "webfetch", "resource": "*", "effect": "allow"})
        self.lock = threading.Lock()
        # Serialize turns and resets per device without retaining unbounded locks.
        self.turns = [threading.Lock() for _ in range(64)]
        try:
            self.ids = json.loads(path.read_text())
        except FileNotFoundError:
            self.ids = {}
        if not isinstance(self.ids, dict):
            raise ValueError("sessions.json must contain a device-to-session map")

    def get(self, device):
        with self.lock:
            sid = self.ids.get(device)
        return sid if isinstance(sid, str) and SESSION_ID.fullmatch(sid) else ""

    def save(self, device, sid):
        with self.lock:
            new = dict(self.ids)
            if sid:
                new[device] = sid
            elif device in new:
                del new[device]
            else:
                return
            self.path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            with tempfile.NamedTemporaryFile(mode="w", dir=self.path.parent, delete=False) as tmp:
                try:
                    json.dump(new, tmp)
                    tmp.close()
                    os.replace(tmp.name, self.path)
                finally:
                    Path(tmp.name).unlink(missing_ok=True)
            self.ids = new

    def reset(self, device):
        with self.turns[hash(device) % len(self.turns)]:
            self.save(device, "")

    def ask(self, device, text):
        with self.turns[hash(device) % len(self.turns)]:
            sid = self.get(device)
            session = None
            if sid:
                try:
                    session = upstream("OpenCode", "GET", f"/api/session/{sid}")["data"]
                except UpstreamStatus as error:
                    if error.upstream_status != 404:
                        raise
            if session is None:
                create = {
                    "title": f"EchoEar: {device}",
                    "location": {"directory": str(PROJECT)},
                    "agent": self.agent,
                    "model": self.model,
                    "permissions": self.permissions,
                }
                sid = upstream("OpenCode", "POST", "/api/session", create)["data"]["id"]
                if not SESSION_ID.fullmatch(sid):
                    raise Error(502, "OpenCode returned an invalid session ID")
                self.save(device, sid)
            else:
                # Agent and model are independent session settings; keep device history.
                if session.get("agent") != self.agent:
                    upstream("OpenCode", "POST", f"/api/session/{sid}/agent", {"agent": self.agent})
                if session.get("model") != self.model:
                    upstream("OpenCode", "POST", f"/api/session/{sid}/model", {"model": self.model})
                if session.get("permissions") != self.permissions:
                    upstream("OpenCode", "PATCH", f"/api/session/{sid}", {"permissions": self.permissions})

            prompt = upstream("OpenCode", "POST", f"/api/session/{sid}/prompt", {"text": text})
            upstream("OpenCode", "POST", f"/api/experimental/session/{sid}/wait", timeout=90)
            messages = upstream("OpenCode", "GET", f"/api/session/{sid}/message?order=desc&limit=32")
            return final_reply(messages["data"], prompt["data"]["id"]), sid


class Bridge(ThreadingHTTPServer):
    def __init__(self, address, token, ota_dir, sessions):
        self.token = token.encode()
        self.firmware = ota_dir / "echoear.bin"
        self.sessions = sessions
        self.speech = None
        self.audio = OrderedDict()
        self.audio_lock = threading.Lock()
        super().__init__(address, Handler)

    def cache_pcm(self, pcm):
        audio_id = secrets.token_urlsafe(18)
        with self.audio_lock:
            self.audio[audio_id] = pcm
            if len(self.audio) > 16:
                self.audio.popitem(last=False)
        return f"/api/audio/{audio_id}"

    def cached_pcm(self, audio_id):
        with self.audio_lock:
            pcm = self.audio.get(audio_id)
            if pcm is not None:
                self.audio.move_to_end(audio_id)
            return pcm


class Handler(BaseHTTPRequestHandler):
    timeout = 15

    def log_message(self, *_):
        # Never log user-supplied URLs, authorization headers, or transcript text.
        pass

    def send_json(self, status, result):
        self.send_bytes(status, json_bytes(result), "application/json")

    def send_bytes(self, status, body, content_type):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def read_body(self, limit):
        lengths = self.headers.get_all("Content-Length", [])
        if self.headers.get("Transfer-Encoding") or len(lengths) > 1:
            raise Error(400, "Request requires a single Content-Length")
        if not lengths:
            raise Error(411, "Content-Length required")
        if not re.fullmatch(r"[0-9]{1,12}", lengths[0]):
            raise Error(400, "Invalid Content-Length")
        size = int(lengths[0])
        if size > limit:
            raise Error(413, "Request body too large")
        try:
            body = self.rfile.read(size)
        except TimeoutError:
            raise Error(408, "Request body timed out") from None
        if len(body) != size:
            raise Error(400, "Incomplete request body")
        return body

    def json_body(self):
        if self.headers.get_content_type() != "application/json":
            raise Error(415, "Expected application/json")
        try:
            body = json.loads(self.read_body(MAX_JSON))
        except (ValueError, RecursionError):
            raise Error(400, "Invalid JSON") from None
        if not isinstance(body, dict):
            raise Error(400, "Expected a JSON object")
        return body

    @staticmethod
    def device(value):
        if not isinstance(value, str) or not DEVICE_ID.fullmatch(value):
            raise Error(400, "Invalid device ID")
        return value

    def voice(self):
        device = self.device(self.headers.get("X-Device-Id", "echoear"))
        # X-Session-Id is advisory; only the persisted device mapping can select a session.
        kind = self.headers.get_content_type()
        if kind not in ("audio/pcm", "audio/wav"):
            raise Error(415, "Expected audio/pcm or audio/wav")
        audio = self.read_body(MAX_AUDIO)
        if kind == "audio/pcm":
            audio = pcm_wav(audio)
        elif len(audio) < 12 or audio[:4] != b"RIFF" or audio[8:12] != b"WAVE":
            raise Error(400, "Expected RIFF/WAVE audio")
        start = time.monotonic_ns()
        transcript = transcribe(audio)
        result = {"transcript": transcript, "hex_ms": elapsed_ms(start)}
        if not transcript:
            return dict(result, reply="", screen=None, session_id=self.server.sessions.get(device),
                        opencode_ms=0, **self.audio_fields(""))
        return dict(result, **self.text(device, transcript))

    def text(self, device, text):
        if not isinstance(text, str) or not text.strip():
            raise Error(400, "Expected nonempty text")
        start = time.monotonic_ns()
        try:
            reply, sid = self.server.sessions.ask(device, text.strip())
        except (KeyError, TypeError, ValueError):
            raise Error(502, "OpenCode returned an invalid response") from None
        reply, screen = split_screen(reply)
        return dict(reply=reply, screen=screen, session_id=sid, opencode_ms=elapsed_ms(start),
                    **self.audio_fields(reply))

    def speech_pcm(self, text):
        clean = plain_text(text)
        if len(clean) > MAX_TEXT or not any(c.isalnum() for c in clean):
            raise Error(400, f"Expected spoken text of 1–{MAX_TEXT} characters")
        if self.server.speech is None:
            raise Error(503, "TTS unavailable")
        try:
            return self.server.speech.synthesize(clean)
        except Exception as error:
            print(f"EchoEar TTS synthesis failed: {type(error).__name__}", file=sys.stderr, flush=True)
            raise Error(502, "TTS synthesis failed") from None

    def audio_fields(self, reply):
        result = dict(audio_url=None, audio_bytes=0, sample_rate=SAMPLE_RATE, tts_ms=0)
        if reply:
            start = time.monotonic_ns()
            try:
                pcm = self.speech_pcm(reply)
                result.update(audio_url=self.server.cache_pcm(pcm), audio_bytes=len(pcm))
            except Error as error:
                if error.status != 502:  # Inference failures are already logged.
                    print(f"EchoEar could not attach TTS audio: {error}", file=sys.stderr, flush=True)
            result["tts_ms"] = elapsed_ms(start)
        return result

    def ota(self, manifest):
        try:
            firmware = self.server.firmware.open("rb")
        except FileNotFoundError:
            if manifest:
                self.send_json(200, {"exists": False, "size": 0, "sha256": "", "mtime": 0})
                return
            raise Error(404, "Firmware not found") from None
        with firmware:
            info = os.fstat(firmware.fileno())
            if not stat.S_ISREG(info.st_mode):
                raise Error(404, "Firmware not found")
            digest = hashlib.file_digest(firmware, "sha256").hexdigest()
            if manifest:
                self.send_json(200, dict(exists=True, size=info.st_size, sha256=digest, mtime=int(info.st_mtime)))
                return
            firmware.seek(0)
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(info.st_size))
            self.send_header("X-Firmware-Sha256", digest)
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            remaining = info.st_size
            while remaining and (chunk := firmware.read(min(remaining, 64 * 1024))):
                self.wfile.write(chunk)
                remaining -= len(chunk)

    def dispatch(self):
        path = self.path.partition("?")[0]
        try:
            if path.startswith(("/api/", "/ota/")):
                provided = self.headers.get_all("X-EchoEar-Token", [])
                if len(provided) != 1 or not hmac.compare_digest(provided[0].encode("latin-1"), self.server.token):
                    raise Error(401, "Unauthorized")
            if self.command == "GET":
                if path == "/health":
                    speech = self.server.speech
                    return self.send_json(200, dict(
                        ok=True, hex=reachable("Hex", "/health"), opencode=reachable("OpenCode", "/api/info"),
                        tts=speech is not None, tts_engine=speech.engine if speech else "unavailable",
                    ))
                if path in ("/ota/version.json", "/ota/latest.bin"):
                    return self.ota(path.endswith(".json"))
                if path.startswith("/api/audio/"):
                    pcm = self.server.cached_pcm(path.removeprefix("/api/audio/"))
                    if pcm is None:
                        raise Error(404, "Audio not found")
                    return self.send_bytes(200, pcm, "audio/pcm")
            if self.command == "POST":
                if path == "/api/voice":
                    return self.send_json(200, self.voice())
                if path == "/api/tts":
                    pcm = self.speech_pcm(self.json_body().get("text"))
                    return self.send_bytes(200, pcm, "audio/pcm")
                if path in ("/api/text", "/api/reset"):
                    body = self.json_body()
                    device = self.device(body.get("device_id", self.headers.get("X-Device-Id", "echoear")))
                    if path == "/api/text":
                        return self.send_json(200, self.text(device, body.get("text")))
                    self.server.sessions.reset(device)
                    return self.send_json(200, {"ok": True})
            raise Error(404, "Not found")
        except Error as error:
            self.send_json(error.status, {"error": str(error)})
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            pass

    do_GET = dispatch
    do_POST = dispatch


def main():
    token = os.environ.get("ECHOEAR_BRIDGE_TOKEN", "")
    if not token or not token.isascii() or any(not 33 <= ord(char) <= 126 for char in token):
        raise SystemExit("Set ECHOEAR_BRIDGE_TOKEN to a nonempty ASCII token without whitespace")
    agent = os.environ.get("ECHOEAR_OPENCODE_AGENT", "echoear")
    model_spec = os.environ.get("ECHOEAR_OPENCODE_MODEL") or "opencode/gemini-4-neon#high"
    provider, slash, model_id = model_spec.partition("/")
    model_id, hash_mark, variant = model_id.partition("#")
    if not slash or not provider or not model_id or (hash_mark and not variant):
        raise SystemExit("ECHOEAR_OPENCODE_MODEL must be provider/model or provider/model#variant")
    model = {"providerID": provider, "id": model_id}
    if variant:
        model["variant"] = variant
    address = (os.environ.get("ECHOEAR_BRIDGE_HOST", "0.0.0.0"), int(os.environ.get("ECHOEAR_BRIDGE_PORT", "8765")))
    ota_dir = Path(os.environ.get("ECHOEAR_OTA_DIR", PROJECT.parent / "firmware/build"))
    with Bridge(address, token, ota_dir, Sessions(DATA / "echoear/sessions.json", agent, model)) as bridge:
        try:
            start = time.monotonic_ns()
            speech = Speech(DATA / "echoear/tts-models")
            speech.synthesize("Hello! Let's find something fun to learn.")
            bridge.speech = speech
            print(f"EchoEar TTS ready: {speech.engine}/{speech.voice} ({elapsed_ms(start)} ms startup)", flush=True)
        except Exception as error:
            print(f"EchoEar TTS unavailable: {type(error).__name__}", file=sys.stderr, flush=True)
        print(f"EchoEar bridge listening on {bridge.server_address[0]}:{bridge.server_address[1]}", flush=True)
        bridge.serve_forever()


if __name__ == "__main__":
    main()
