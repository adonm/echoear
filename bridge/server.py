#!/usr/bin/env python3
"""Bridge EchoEar devices to the local Hex and OpenCode services."""

import base64
import errno
import hashlib
import hmac
import http.client
import io
import ipaddress
import json
import math
import os
import re
import secrets
import socket
import stat
import sys
import tempfile
import textwrap
import threading
import time
import wave
from collections import OrderedDict
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlencode, urlsplit

from tts import (
    MAX_PCM, MAX_TEXT, SAMPLE_RATE, Speech, make_intercom_chime_pcm,
    normalize_voice_pcm, plain_text, short_speech,
)

DATA = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share")
STATE = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state")
PROJECT = Path(__file__).resolve().parent
DEVICE_ID = re.compile(r"[A-Za-z0-9_.-]{1,64}")
CHARACTER = re.compile(r"[A-Za-z0-9][A-Za-z0-9 ._'’-]{0,39}")
SESSION_ID = re.compile(r"ses[A-Za-z0-9_-]{1,128}")
MAX_AUDIO = 2 * 1024 * 1024
MAX_JSON = 16 * 1024
MAX_SCREEN_BYTES = 6000
MAX_MESSAGE = 400
MAX_PEER_PCM = 1536 * 1024
PEER_TTL = 180
BROADCAST = ("all", "everyone", "everybody")
LAN_NETWORKS = tuple(map(ipaddress.ip_network, (
    "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "fc00::/7",
)))
DIRECT_PREFIX = re.compile(r"^(?:hey\s+[\w-]+,?\s+)?(?:tell|message|intercom|walkie|say\s+to|send)\s+", re.I)
COMPOSE_REQUEST = re.compile(
    r"^(?:about\b|(?:(?:an?|the|some)\s+(?:[\w'-]+\s+){0,5})?(?:joke|story|fact|riddle|quiz)\b)",
    re.I,
)
COLOR = re.compile(r"#[0-9A-Fa-f]{6}")
def payload_fence(tag):
    return re.compile(
        rf"^[ \t]*```{tag}[ \t]*(?:\r?\n|\Z)(.*?)(^[ \t]*```[ \t]*\r?$|\Z)",
        re.MULTILINE | re.DOTALL | re.IGNORECASE,
    )


SCREEN_FENCE = payload_fence("echoear-screen")
MESSAGE_FENCE = payload_fence("echoear-message")
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


def persist_json(path, value):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as tmp:
        try:
            json.dump(value, tmp)
            tmp.close()
            os.replace(tmp.name, path)
        finally:
            Path(tmp.name).unlink(missing_ok=True)


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
    if kind == "btn" and (reply_to := screen_text(plain_text(value.get("reply_to")), 40)):
        result["reply_to"] = reply_to
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


def strip_payload(reply, fence, parse):
    payload = None

    def strip_block(match):
        nonlocal payload
        if payload is None and match[2]:
            payload = parse(match[1])
        return ""

    return fence.sub(strip_block, reply).strip(), payload


def split_screen(reply):
    return strip_payload(reply, SCREEN_FENCE, sanitize_screen)


def parse_message(raw):
    if len(raw) > MAX_JSON:
        return None
    try:
        data = json.loads(raw)
    except (ValueError, RecursionError):
        return None
    if isinstance(data, dict):
        target = data.get("to")
        text = screen_text(plain_text(data.get("text")), MAX_MESSAGE)
        if isinstance(target, str) and re.fullmatch(r"[\w .'’-]{1,64}", target.strip()) and text:
            return target.strip(), text


def local_ip(value, allow_loopback):
    if not isinstance(value, str):
        raise Error(400, "Expected a numeric LAN IP address")
    try:
        ip = ipaddress.ip_address(value)
    except ValueError:
        raise Error(400, "Expected a numeric LAN IP address") from None
    if (ip.is_loopback and allow_loopback) or any(ip in network for network in LAN_NETWORKS):
        return str(ip)
    raise Error(400, "Peer address must be on the LAN")


def is_host_ip(ip):
    address = ipaddress.ip_address(ip)
    if address.is_loopback:
        return True
    family = socket.AF_INET if address.version == 4 else socket.AF_INET6
    # A bind checks all local interfaces, even when the bridge listens on a wildcard.
    with socket.socket(family, socket.SOCK_DGRAM) as probe:
        try:
            probe.bind((ip, 0))
        except OSError as error:
            if error.errno == errno.EADDRNOTAVAIL:
                return False
            raise
    return True


class Peers:
    def __init__(self, path):
        self.path = path
        self.lock = threading.Lock()
        try:
            self.entries = json.loads(path.read_text())
        except FileNotFoundError:
            self.entries = {}
        if not isinstance(self.entries, dict):
            raise ValueError("devices.json must contain a device map")

    def _save(self, device, peer):
        if self.entries.get(device) != peer:
            entries = {**self.entries, device: peer}
            persist_json(self.path, entries)
            self.entries = entries

    def register(self, device, ip, character, port):
        with self.lock:
            self._save(device, dict(device_id=device, character=character, ip=ip, port=port,
                                    last_seen=int(time.time())))

    def touch(self, device, client_ip, character=None):
        with self.lock:
            old = self.entries.get(device, {})
            peer = dict(old, device_id=device, character=character or old.get("character", device[:40]),
                        last_seen=int(time.time()))
            if not peer.get("ip") and not is_host_ip(client_ip):
                peer.update(ip=client_ip, port=old.get("port", 80))
            if old or peer.get("ip"):
                self._save(device, peer)
            return peer

    def others(self, sender=None):
        with self.lock:
            return [p for key, p in sorted(self.entries.items())
                    if p.get("ip") and key.casefold() != (sender or "").casefold()]

    def public(self, sender=None):
        now = time.time()
        return [dict(device_id=p["device_id"], character=p["character"], ip=p["ip"],
                     online=now - p["last_seen"] <= PEER_TTL) for p in self.others(sender)]

    def targets(self, sender, target):
        target = target.casefold()
        peers = self.others(sender)
        if target in BROADCAST:
            return peers
        return ([p for p in peers if p["device_id"].casefold() == target]
                or [p for p in peers if p["character"].casefold() == target])

    def direct(self, sender, text):
        prefix = DIRECT_PREFIX.match(text.strip())
        if not prefix:
            return None
        names = {*BROADCAST}
        for peer in self.others(sender):
            names.update((peer["character"], peer["device_id"]))
        # Match longer names first and keep a literal boundary after the addressee.
        choices = "|".join(map(re.escape, sorted(names, key=len, reverse=True)))
        match = re.fullmatch(rf"({choices})(?:[\s,:]+|[-–—]\s+)(.*\S)\s*",
                             prefix.string[prefix.end():], re.I | re.S)
        if match:
            target, message = match.groups()
            message = message.lstrip(" \t\r\n,:-–—")
            if message and not COMPOSE_REQUEST.match(message):
                return target, message

    def prompt(self, sender, text):
        peers = self.others(sender["device_id"])
        names = [f"{p['character']} ({p['device_id']})" for p in peers[:16]]
        others = ", ".join(names) or "none"
        return f"[EchoEar: you are {sender['character']} ({sender['device_id']}); other EchoEars: {others}]\n{text}"


def message_card(character, text):
    lines = textwrap.wrap(text or "Voice message", width=27, max_lines=3, placeholder="…")
    shapes = [dict(t="text", x=180, y=124 + i * 25, c="#FFF1C8", s=line) for i, line in enumerate(lines)]
    shapes.append(dict(t="btn", x=85, y=218, w=190, h=46, r=23, c="#34D399",
                       s=f"Reply to {character}"[:32], reply_to=character))
    return dict(bg="#1E1B3A", title=f"From {character}!", shapes=shapes)


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
            persist_json(self.path, new)
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


class AudioStream:
    def __init__(self, pcm=b""):
        self.chunks: list[bytes] = [pcm] if pcm else []
        self.done = bool(pcm)
        self.error: str | None = None
        self.cond = threading.Condition()
        self.total_bytes = len(pcm)

    def produce(self, speech, text):
        error = None
        try:
            for _, pcm in speech.synthesize_sentences(text):
                with self.cond:
                    if not pcm or len(pcm) % 2 or self.total_bytes + len(pcm) > MAX_PCM:
                        raise ValueError("Invalid sentence size or audio stream exceeds 2 MiB")
                    self.chunks.append(pcm)
                    self.total_bytes += len(pcm)
                    self.cond.notify_all()
        except Exception as exc:
            error = type(exc).__name__
            print(f"EchoEar TTS streaming failed: {error}", file=sys.stderr, flush=True)
        finally:
            with self.cond:
                self.error = error
                self.done = True
                self.cond.notify_all()


class Bridge(ThreadingHTTPServer):
    def __init__(self, address, token, ota_dir, sessions):
        self.token = token.encode()
        self.firmware = ota_dir / "echoear.bin"
        self.sessions = sessions
        self.peers = Peers(sessions.path.with_name("devices.json"))
        self.speech = None
        self.audio = OrderedDict()
        self.audio_lock = threading.Lock()
        super().__init__(address, Handler)

    def cache_audio(self, stream):
        audio_id = secrets.token_urlsafe(18)
        with self.audio_lock:
            self.audio[audio_id] = stream
            if len(self.audio) > 16:
                self.audio.popitem(last=False)
        return f"/api/audio/{audio_id}"

    def cached_audio(self, audio_id):
        with self.audio_lock:
            stream = self.audio.get(audio_id)
            if stream is not None:
                self.audio.move_to_end(audio_id)
            return stream

    def post_peer(self, peer, path, content_type, body):
        connection = None
        try:
            address = local_ip(peer["ip"], allow_loopback=True)
            connection = http.client.HTTPConnection(address, peer["port"], timeout=5)
            connection.request("POST", path, body, {
                "Content-Type": content_type, "X-EchoEar-Token": self.token.decode("ascii"),
            })
            return 200 <= connection.getresponse().status < 300
        except (Error, OSError, http.client.HTTPException, KeyError, ValueError, TypeError):
            return False
        finally:
            if connection is not None:
                connection.close()

    def deliver_intercom(self, sender_char, sender_id, target_name, message_text, raw_pcm=None):
        peers = self.peers.targets(sender_id, target_name)
        result = dict(delivered_to=[], sent_to=[], failed_to=[])
        if not peers:
            return dict(result, error="peer_not_found")
        text = screen_text(plain_text(message_text), MAX_MESSAGE)
        display = json_bytes(message_card(sender_char, text))
        try:
            if self.speech is None:
                raise RuntimeError("TTS unavailable")
            if raw_pcm:
                intro = short_speech(self.speech, f"From {sender_char}!")
                message = normalize_voice_pcm(raw_pcm)
            else:
                intro = short_speech(self.speech, f"Message from {sender_char}!")
                message = self.speech.synthesize_pcm(text)
            audio = make_intercom_chime_pcm() + intro + message
        except Exception as exc:
            print(f"EchoEar intercom audio failed: {type(exc).__name__}", file=sys.stderr, flush=True)
            return dict(result, error="audio_unavailable", failed_to=[p["device_id"] for p in peers])
        if len(audio) > MAX_PEER_PCM:
            return dict(result, error="message_too_long", failed_to=[p["device_id"] for p in peers])

        def send(peer):
            display_ok = self.post_peer(peer, "/display", "application/json", display)
            audio_ok = self.post_peer(peer, "/speak", "audio/pcm", audio)
            return peer, display_ok and audio_ok

        with ThreadPoolExecutor(max_workers=min(len(peers), 8)) as workers:
            for peer, ok in workers.map(send, peers):
                if ok:
                    result["delivered_to"].append(peer["character"])
                    result["sent_to"].append(peer["device_id"])
                else:
                    result["failed_to"].append(peer["device_id"])
        if not result["delivered_to"]:
            result["error"] = "delivery_failed"
        return result


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    timeout = 15

    def log_message(self, *_):
        # Never log user-supplied URLs, authorization headers, or transcript text.
        pass

    def send_json(self, status, result):
        self.send_bytes(status, json_bytes(result), "application/json")

    def payload_headers(self, status, content_type, length):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        if length is None:
            self.send_header("Transfer-Encoding", "chunked")
        else:
            self.send_header("Content-Length", str(length))
        self.send_header("Connection", "close")
        self.send_header("Cache-Control", "no-store")

    def send_bytes(self, status, body, content_type):
        self.payload_headers(status, content_type, len(body))
        self.end_headers()
        self.wfile.write(body)

    def send_audio(self, stream):
        with stream.cond:
            length = stream.total_bytes if stream.done else None
        self.payload_headers(200, "audio/pcm", length)
        self.end_headers()
        if length is not None:
            # Once done, chunks never change; no join or lock is needed to replay them.
            for pcm in stream.chunks:
                self.wfile.write(pcm)
            return

        index = 0
        while True:
            with stream.cond:
                stream.cond.wait_for(lambda: index < len(stream.chunks) or stream.done)
                if index == len(stream.chunks):
                    break
                pcm = stream.chunks[index]
            self.wfile.write(f"{len(pcm):X}\r\n".encode("ascii"))
            self.wfile.write(pcm)
            self.wfile.write(b"\r\n")
            self.wfile.flush()
            index += 1
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

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

    @staticmethod
    def character(value):
        if not isinstance(value, str) or not CHARACTER.fullmatch(value.strip()):
            raise Error(400, "Invalid character name")
        return value.strip()

    def peer_ip(self, value):
        return local_ip(value, ipaddress.ip_address(self.client_address[0]).is_loopback)

    def sender(self, device):
        character = self.headers.get("X-Character")
        return self.server.peers.touch(
            device, self.peer_ip(self.client_address[0]), self.character(character) if character is not None else None,
        )

    def register(self):
        body = self.json_body()
        device = self.device(body.get("device_id"))
        character = self.character(body.get("character"))
        ip = self.peer_ip(body.get("ip", self.client_address[0]))
        port = body.get("port", 80)
        if type(port) is not int or not 1 <= port <= 65535:
            raise Error(400, "Invalid peer port")
        self.server.peers.register(device, ip, character, port)
        return dict(ok=True, peers=self.server.peers.public(device))

    def recording(self):
        kind = self.headers.get_content_type()
        if kind not in ("audio/pcm", "audio/wav"):
            raise Error(415, "Expected audio/pcm or audio/wav")
        audio = self.read_body(MAX_AUDIO)
        if kind == "audio/pcm":
            return pcm_wav(audio)
        if len(audio) < 12 or audio[:4] != b"RIFF" or audio[8:12] != b"WAVE":
            raise Error(400, "Expected RIFF/WAVE audio")
        return audio

    def voice(self):
        wav = self.recording()
        device = self.device(self.headers.get("X-Device-Id", "echoear"))
        # X-Session-Id is advisory; only the persisted device mapping can select a session.
        sender = self.sender(device)
        start = time.monotonic_ns()
        transcript = transcribe(wav)
        result = {"transcript": transcript, "hex_ms": elapsed_ms(start)}
        if not transcript:
            return dict(result, reply="", screen=None, session_id=self.server.sessions.get(device),
                        opencode_ms=0, **self.audio_fields(""))
        return dict(result, **self.text(sender, transcript, wav=wav))

    def relay(self, sender, target, message, pcm=None):
        device = sender["device_id"]
        delivery = self.server.deliver_intercom(sender["character"], device, target, message, pcm)
        peers = self.server.peers.targets(device, target)
        recipient = "everyone" if target.casefold() in BROADCAST else peers[0]["character"] if peers else target
        delivered = delivery["delivered_to"]
        if not delivered:
            reply = f"I couldn't reach {recipient} right now."
        elif delivery["failed_to"]:
            reply = "I sent your message, but couldn't reach everyone."
        else:
            reply = f"Sent to {recipient}!"
        return dict(ok=bool(delivered), reply=reply, screen=None,
                    session_id=self.server.sessions.get(device), opencode_ms=0,
                    **delivery, **self.audio_fields(reply, cached=True))

    def intercom(self):
        target = self.headers.get("X-Target", "").strip()
        if not target or len(target) > 64:
            raise Error(400, "Expected X-Target peer name")
        wav = self.recording()
        pcm = wav_pcm(wav)
        sender = self.sender(self.device(self.headers.get("X-Device-Id", "echoear")))
        start = time.monotonic_ns()
        transcript = transcribe(wav)
        result = dict(transcript=transcript, hex_ms=elapsed_ms(start))
        return dict(result, **self.relay(sender, target, transcript, pcm))

    def text(self, sender, text, wav=None):
        if not isinstance(text, str) or not text.strip():
            raise Error(400, "Expected nonempty text")
        device = sender["device_id"]
        if direct := self.server.peers.direct(device, text):
            return self.relay(sender, *direct, pcm=wav_pcm(wav) if wav is not None else None)
        start = time.monotonic_ns()
        try:
            reply, sid = self.server.sessions.ask(device, self.server.peers.prompt(sender, text.strip()))
        except (KeyError, TypeError, ValueError):
            raise Error(502, "OpenCode returned an invalid response") from None
        opencode_ms = elapsed_ms(start)
        reply, screen = split_screen(reply)
        reply, message = strip_payload(reply, MESSAGE_FENCE, parse_message)
        delivery = {"sent_to": []}
        if message:
            target, text = message
            delivery = self.server.deliver_intercom(sender["character"], device, target, text)
        return dict(reply=reply, screen=screen, session_id=sid, opencode_ms=opencode_ms,
                    **delivery, **self.audio_fields(reply))

    def speech_input(self, text):
        try:
            clean = Speech.spoken_text(text)
        except ValueError:
            raise Error(400, f"Expected spoken text of 1–{MAX_TEXT} characters") from None
        if self.server.speech is None:
            raise Error(503, "TTS unavailable")
        return self.server.speech, clean

    def speech_pcm(self, text):
        speech, clean = self.speech_input(text)
        try:
            return speech.synthesize_pcm(clean)
        except Exception as error:
            print(f"EchoEar TTS synthesis failed: {type(error).__name__}", file=sys.stderr, flush=True)
            raise Error(502, "TTS synthesis failed") from None

    def audio_fields(self, reply, cached=False):
        result = dict(audio_url=None, audio_bytes=0, sample_rate=SAMPLE_RATE,
                      audio_streaming=False, tts_first_ms=0, tts_ms=0)
        if not reply:
            return result
        start = time.monotonic_ns()
        try:
            speech, clean = self.speech_input(reply)
            stream = AudioStream(short_speech(speech, clean) if cached else b"")
        except Exception as error:
            print(f"EchoEar could not attach TTS audio: {type(error).__name__}", file=sys.stderr, flush=True)
        else:
            if not cached:
                threading.Thread(target=stream.produce, args=(speech, clean), daemon=True).start()
            with stream.cond:
                stream.cond.wait_for(lambda: stream.chunks or stream.done)
                if stream.chunks:
                    result.update(audio_url=self.server.cache_audio(stream),
                                  audio_bytes=stream.total_bytes if stream.done else 0,
                                  audio_streaming=not stream.done)
        result["tts_first_ms"] = result["tts_ms"] = elapsed_ms(start)
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
            self.payload_headers(200, "application/octet-stream", info.st_size)
            self.send_header("X-Firmware-Sha256", digest)
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
                if path == "/api/devices":
                    return self.send_json(200, dict(devices=self.server.peers.public()))
                if path in ("/ota/version.json", "/ota/latest.bin"):
                    return self.ota(path.endswith(".json"))
                if path.startswith("/api/audio/"):
                    stream = self.server.cached_audio(path.removeprefix("/api/audio/"))
                    if stream is None:
                        raise Error(404, "Audio not found")
                    return self.send_audio(stream)
            if self.command == "POST":
                if path == "/api/voice":
                    return self.send_json(200, self.voice())
                if path == "/api/register":
                    return self.send_json(200, self.register())
                if path == "/api/intercom":
                    return self.send_json(200, self.intercom())
                if path == "/api/tts":
                    pcm = self.speech_pcm(self.json_body().get("text"))
                    return self.send_bytes(200, pcm, "audio/pcm")
                if path in ("/api/text", "/api/reset"):
                    body = self.json_body()
                    device = self.device(body.get("device_id", self.headers.get("X-Device-Id", "echoear")))
                    if path == "/api/text":
                        return self.send_json(200, self.text(self.sender(device), body.get("text")))
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
            speech.synthesize_pcm("Hello! Let's find something fun to learn.")
            bridge.speech = speech
            print(f"EchoEar TTS ready: {speech.engine}/{speech.voice} ({elapsed_ms(start)} ms startup)", flush=True)
        except Exception as error:
            print(f"EchoEar TTS unavailable: {type(error).__name__}", file=sys.stderr, flush=True)
        print(f"EchoEar bridge listening on {bridge.server_address[0]}:{bridge.server_address[1]}", flush=True)
        bridge.serve_forever()


if __name__ == "__main__":
    main()
