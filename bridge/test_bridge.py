#!/usr/bin/env python3
"""Live bridge test. Flite (or --wav) supplies speech; no model stubs."""

import argparse
from array import array
import ctypes
import hashlib
import json
import math
import os
import queue
import re
import secrets
import shlex
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import wave
from pathlib import Path
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from server import DATA, Bridge, SESSION_ID, Sessions, json_bytes, split_screen, upstream
from tts import make_intercom_chime_pcm, normalize_voice_pcm, split_sentences

DEVICE = f"bridge-selftest-{secrets.token_hex(8)}"
SPEECH = b"What is the capital city of France? Answer with only the city name."
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def settings():
    result = {}
    config_home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    env_file = config_home / "echoear/bridge.env"
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            key, _, value = line.partition("=")
            if key in ("ECHOEAR_BRIDGE_TOKEN", "ECHOEAR_BRIDGE_PORT"):
                fields = shlex.split(value, comments=True)
                if len(fields) == 1:
                    result[key] = fields[0]
    return {**result, **os.environ}


def generated_wav(path):
    try:
        flite = ctypes.CDLL("libflite.so.1")
        voice = ctypes.CDLL("libflite_cmu_us_kal16.so.2.2")
    except OSError:
        raise SystemExit("Flite not available; pass --wav with the spoken test prompt") from None
    flite.flite_init()
    voice.register_cmu_us_kal16.argtypes = [ctypes.c_char_p]
    voice.register_cmu_us_kal16.restype = ctypes.c_void_p
    flite.flite_text_to_speech.argtypes = [ctypes.c_char_p, ctypes.c_void_p, ctypes.c_char_p]
    flite.flite_text_to_speech.restype = ctypes.c_float
    assert flite.flite_text_to_speech(SPEECH, voice.register_cmu_us_kal16(None), os.fsencode(path)) > 0


def send(url, path, *, body=None, content_type=None, token=None, device=None, headers=None):
    headers = dict(headers or {})
    if token:
        headers["X-EchoEar-Token"] = token
    if device:
        headers["X-Device-Id"] = device
    if isinstance(body, dict):
        body = json.dumps(body).encode()
        content_type = "application/json"
    if content_type:
        headers["Content-Type"] = content_type
    with HTTP.open(urllib.request.Request(url + path, data=body, headers=headers), timeout=170) as response:
        payload = response.read()
        if response.headers.get_content_type() == "application/json":
            payload = json.loads(payload)
        return payload, response.headers


def rejects(status, url, path, **request):
    try:
        send(url, path, **request)
    except urllib.error.HTTPError as error:
        with error:
            assert error.code == status, f"{path}: expected HTTP {status}, got {error.code}"
    else:
        raise AssertionError(f"{path}: expected HTTP {status}")


def test_ota(root):
    ota = root / "ota"
    ota.mkdir()
    with Bridge(("127.0.0.1", 0), "test-only", ota, Sessions(root / "sessions.json", None, None)) as bridge:
        worker = threading.Thread(target=bridge.serve_forever)
        worker.start()
        try:
            url = f"http://127.0.0.1:{bridge.server_port}"
            auth = {"token": "test-only"}
            assert send(url, "/ota/version.json", **auth)[0] == {
                "exists": False, "size": 0, "sha256": "", "mtime": 0
            }
            rejects(404, url, "/ota/latest.bin", **auth)
            firmware = b"EchoEar OTA test payload" * 4000
            sha256 = hashlib.sha256(firmware).hexdigest()
            (ota / "echoear.bin").write_bytes(firmware)
            info, _ = send(url, "/ota/version.json", **auth)
            assert info["exists"] and info["size"] == len(firmware) and info["sha256"] == sha256
            assert type(info["mtime"]) is int and info["mtime"] > 0
            payload, headers = send(url, "/ota/latest.bin", **auth)
            assert payload == firmware and int(headers["Content-Length"]) == len(firmware)
            assert headers["X-Firmware-Sha256"] == sha256
        finally:
            bridge.shutdown()
            worker.join()
    print("PASS OTA missing image, manifest, hash, and streamed download", flush=True)


def test_screen_parser():
    raw = json.dumps({
        "bg": "oops",
        "injected": {"ignore": "me"},
        "shapes": [
            {"t": "circle", "x": -1, "y": 999, "r": 1.8, "c": "#ab00ef", "z": 100,
             "tap": "**Fun fact!**\nTap again.\0", "ask": 42},
            {"t": "line", "x1": 1, "y1": 2, "x2": 3, "y2": 4, "c": "bad"},
            {"t": "btn", "x": 120, "y": 230, "w": 110, "h": 50, "s": "Explore",
             "c": "#C084FC", "r": 22, "ask": "Why? " + "a" * 130, "reply_to": "**Mint**\0"},
        ] + [{"t": "text", "x": 1, "y": 2, "s": "🔥" * 80, "c": "#FFFFFF"}] * 40,
    })
    reply, screen = split_screen(f"Short answer.\r\n\r\n```echoear-screen\r\n{raw}\r\n```\r\n")
    assert reply == "Short answer."
    assert set(screen) == {"bg", "shapes"} and screen["bg"] == "#0B1020"
    assert screen["shapes"][0] == {
        "t": "circle", "x": 0, "y": 360, "r": 1, "c": "#AB00EF", "tap": "Fun fact! Tap again."
    }
    button = screen["shapes"][1]
    assert button["t"] == "btn" and button["s"] == "Explore" and button["r"] == 22
    assert button["ask"] == "Why? " + "a" * 115
    assert button["reply_to"] == "Mint"
    assert all(shape["t"] == "text" for shape in screen["shapes"][2:])
    assert 2 < len(screen["shapes"]) <= 24 and len(json_bytes(screen)) < 6000
    for block in ("```echoear-screen\n{bad}\n```", '```echoear-screen\n{"title":"Unclosed"}'):
        assert split_screen("Short answer.\n" + block) == ("Short answer.", None)
    print("PASS screen sanitization preserves btn, tap, and ask within size limits", flush=True)


def test_sentence_split():
    assert split_sentences(
        "**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! Where do spiral shells get their shape?"
    ) == [
        "Whoa! Dr. Lee found a 3.2-inch shell.",
        "Ooh, awesome question! Where do spiral shells get their shape?",
    ]
    assert split_sentences('She asked, "Why is the Moon so bright?" The answer is reflected sunlight.') == [
        'She asked, "Why is the Moon so bright?"', "The answer is reflected sunlight.",
    ]
    chained = "Whoa! That is pretty neat! Sunlight takes several minutes to reach us."
    assert split_sentences(chained) == [chained]
    assert split_sentences("Paris.") == ["Paris."] and split_sentences("Wow!") == ["Wow!"]
    print("PASS sentence boundaries, abbreviations, decimals, and merged exclamations", flush=True)


def check_pcm(payload, headers, expected=None):
    assert headers.get_content_type() == "audio/pcm"
    if headers.get("Transfer-Encoding") == "chunked":
        assert expected is None and "Content-Length" not in headers
    else:
        assert len(payload) == int(headers["Content-Length"])
    if expected is not None:
        assert len(payload) == expected
    assert len(payload) > 8000
    assert len(payload) % 2 == 0
    samples = array("h", payload)
    if sys.byteorder == "big":
        samples.byteswap()
    rms = math.sqrt(sum(sample * sample for sample in samples) / len(samples))
    assert rms > 100 and max(map(abs, samples)) <= 29204
    return len(samples) / 16000, round(rms)


def audio_path(result):
    path = result["audio_url"]
    assert re.fullmatch(r"/api/audio/[A-Za-z0-9_-]{24}", path or "")
    assert result["sample_rate"] == 16000
    assert isinstance(result["tts_first_ms"], int) and result["tts_first_ms"] == result["tts_ms"] >= 0
    assert type(result["audio_streaming"]) is bool
    assert (result["audio_bytes"] == 0) == result["audio_streaming"]
    return path


def check_audio(url, token, result):
    path = audio_path(result)
    rejects(401, url, path)
    pcm, headers = send(url, path, token=token)
    return check_pcm(pcm, headers, result["audio_bytes"] or None)


def check_stream(url, token, result, posted_at):
    path = audio_path(result)
    assert result["audio_streaming"] and result["audio_bytes"] == 0
    request = urllib.request.Request(url + path, headers={"X-EchoEar-Token": token})
    with HTTP.open(request, timeout=30) as response:
        assert response.version == 11 and response.headers["Transfer-Encoding"] == "chunked"
        assert response.headers["Connection"] == "close"
        first = response.read1()
        first_received = time.monotonic_ns()
        rest = response.read()
        completed = time.monotonic_ns()
        assert first and rest
        pcm = first + rest
        duration, rms = check_pcm(pcm, response.headers)
    replay, headers = send(url, path, token=token)
    assert "Transfer-Encoding" not in headers and headers["Content-Length"] == str(len(pcm))
    assert replay == pcm
    first_ms = result["tts_first_ms"]
    total_ms = first_ms + (completed - posted_at) // 1_000_000
    assert first_ms < 0.8 * total_ms, (first_ms, total_ms)
    assert (first_received - posted_at) * 2 < completed - posted_at
    print(f"PASS progressive 16 kHz PCM and byte-identical replay: "
          f"{first_ms} ms first sentence, {total_ms} ms full stream, {duration:.2f} s audio (RMS {rms})", flush=True)


def test_visual(url, token, sid):
    result, _ = send(url, "/api/text", token=token, body={
        "device_id": DEVICE,
        "text": ("What is a right triangle? Compare it to something at home and draw a diagram. "
                 "Add an Explore button with a question and a tappable detail with a fun fact. "
                 "Explain it to a child in four complete spoken sentences."),
    })
    posted_at = time.monotonic_ns()
    assert result["session_id"] == sid
    reply = result["reply"]
    assert 20 <= len(reply.split()) <= 75 and not re.search(r"[*_`#]|https?://|echoear-screen", reply)
    assert 2 <= len(re.findall(r"[.!?](?:\s|$)", reply)) <= 4
    assert not re.search(r"(?m)^\s*(?:- |\d+[.)] )", reply)
    screen = result["screen"]
    assert isinstance(screen, dict) and 0 < len(screen["shapes"]) <= 24 and len(json_bytes(screen)) < 6000
    assert all(re.fullmatch(r"#[0-9A-F]{6}", s["c"]) for s in screen["shapes"])
    assert any(s["t"] == "line" for s in screen["shapes"])
    assert any(s["t"] == "btn" and s.get("ask") for s in screen["shapes"])
    assert any(s.get("tap") for s in screen["shapes"])
    assert len(split_sentences(reply)) >= 2
    check_stream(url, token, result, posted_at)
    model = {"providerID": "opencode", "id": "gemini-4-neon", "variant": "high"}
    messages = upstream("OpenCode", "GET", f"/api/session/{sid}/message?order=desc&limit=1&type=assistant")
    latest = messages["data"][0]
    assert latest["agent"] == "echoear" and latest["model"] == model, (latest["agent"], latest["model"])
    print("PASS interactive right-triangle diagram; assistant used echoear / Gemini 4 Neon high", flush=True)
    print(f"Sample: {reply}", flush=True)


def check_health(url):
    health, _ = send(url, "/health")
    assert all(health.get(key) is True for key in ("ok", "hex", "opencode", "tts")), health
    assert health["tts_engine"] == "kokoro-onnx"


def test_live(url, token, pcm):
    check_health(url)
    print("PASS LAN health, Hex, OpenCode and Kokoro TTS ready", flush=True)
    for path in ("/api/voice", "/api/text", "/api/reset", "/api/tts", "/api/register", "/api/intercom"):
        rejects(401, url, path, body=b"")
    for path in ("/ota/version.json", "/ota/latest.bin"):
        rejects(401, url, path)
    rejects(401, url, "/api/audio/unknown")
    rejects(401, url, "/api/devices")
    rejects(404, url, "/api/audio/unknown", token=token)
    print("PASS API and OTA reject requests without a token", flush=True)
    pcm_direct, headers = send(url, "/api/tts", token=token, body={
        "text": "**Look up!** The Moon shines because it reflects sunlight."
    })
    check_pcm(pcm_direct, headers, len(pcm_direct))
    print("PASS direct TTS returns normalized, non-silent PCM", flush=True)

    reset = {"body": {}, "device": DEVICE, "token": token}
    voice = {"content_type": "audio/pcm", "device": DEVICE, "token": token}
    assert send(url, "/api/reset", **reset)[0] == {"ok": True}
    sid = None
    try:
        result, _ = send(url, "/api/voice", body=pcm, **voice)
        sid = result["session_id"]
        assert SESSION_ID.fullmatch(sid)
        assert "capital" in result["transcript"].lower() and "france" in result["transcript"].lower()
        assert re.fullmatch(r"paris[.!]?", result["reply"].strip(), re.IGNORECASE)
        assert result["screen"] is None
        check_audio(url, token, result)
        assert all(type(result[key]) is int and result[key] >= 0 for key in ("hex_ms", "opencode_ms"))
        print(f"PASS spoken PCM -> Hex -> OpenCode ({result['hex_ms']} ms / {result['opencode_ms']} ms)", flush=True)
        followup, _ = send(url, "/api/text", token=token, body={
            "device_id": DEVICE,
            "text": "What city did you just answer with? Reply with only the city name.",
        })
        assert followup["session_id"] == sid
        assert re.fullmatch(r"paris[.!]?", followup["reply"].strip(), re.IGNORECASE)
        check_audio(url, token, followup)
        print("PASS text turn reuses the voice session", flush=True)
        test_visual(url, token, sid)
    finally:
        assert send(url, "/api/reset", **reset)[0] == {"ok": True}
        if sid and SESSION_ID.fullmatch(sid):
            path = f"/api/session/{sid}"
            if upstream("OpenCode", "GET", path)["data"].get("title") == f"EchoEar: {DEVICE}":
                upstream("OpenCode", "DELETE", path)
    silence, _ = send(url, "/api/voice", body=bytes(2 * 16000 * 2), **voice)
    assert silence["transcript"] == silence["reply"] == silence["session_id"] == ""
    assert silence["opencode_ms"] == 0 and silence["screen"] is None
    assert silence["audio_url"] is None and silence["audio_streaming"] is False
    assert silence["audio_bytes"] == silence["tts_first_ms"] == silence["tts_ms"] == 0
    print("PASS reset and silent PCM skip OpenCode", flush=True)


@contextmanager
def isolated_bridge(root, token):
    """Use real Hex, OpenCode and Kokoro without touching the deployed peer registry."""
    data = root / "data"
    echoear = data / "echoear"
    echoear.mkdir(parents=True)
    (data / "hex").symlink_to(DATA / "hex", target_is_directory=True)
    (echoear / "tts-models").symlink_to(DATA / "echoear/tts-models", target_is_directory=True)
    env = dict(os.environ, XDG_DATA_HOME=str(data), ECHOEAR_BRIDGE_HOST="127.0.0.1",
               ECHOEAR_BRIDGE_PORT="0", ECHOEAR_BRIDGE_TOKEN=token, ECHOEAR_OTA_DIR=str(root / "ota"),
               ECHOEAR_OPENCODE_AGENT="echoear", ECHOEAR_OPENCODE_MODEL="opencode/gemini-4-neon#high")
    python = DATA / "echoear/tts-venv/bin/python"
    with subprocess.Popen([str(python), "-B", str(Path(__file__).with_name("server.py"))],
                          env=env, stdout=subprocess.PIPE, text=True) as service:
        try:
            for line in service.stdout:
                if line.startswith("EchoEar bridge listening on 127.0.0.1:"):
                    yield f"http://127.0.0.1:{int(line.rsplit(':', 1)[1])}", echoear / "devices.json"
                    break
            else:
                raise AssertionError("Isolated bridge did not start")
        finally:
            service.terminate()
            try:
                service.wait(timeout=5)
            except subprocess.TimeoutExpired:
                service.kill()


def test_intercom(url, token, pcm, registry):
    deliveries = queue.Queue()

    class Peer(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers["Content-Length"]))
            valid = self.headers.get("X-EchoEar-Token") == token
            deliveries.put((self.path, self.headers.get_content_type(), body))
            self.send_response(204 if valid else 401)
            self.end_headers()

    def received():
        display, audio = deliveries.get(timeout=2), deliveries.get(timeout=2)
        assert display[:2] == ("/display", "application/json") and audio[:2] == ("/speak", "audio/pcm")
        card = json.loads(display[2])
        assert card["bg"] == "#1E1B3A" and card["title"] == "From Cake!"
        button = next(s for s in card["shapes"] if s["t"] == "btn")
        assert button["s"] == "Reply to Cake" and button["reply_to"] == "Cake"
        lines = [s["s"] for s in card["shapes"] if s["t"] == "text"]
        assert 1 <= len(lines) <= 3 and all(len(line) <= 27 for line in lines)
        assert audio[2].startswith(make_intercom_chime_pcm()) and len(audio[2]) % 2 == 0
        return " ".join(lines), audio[2]

    sender, mock = "peer-selftest-cake", "peer-selftest-mock"
    auth = {"token": token}
    sid = None
    with ThreadingHTTPServer(("127.0.0.1", 0), Peer) as peer:
        worker = threading.Thread(target=peer.serve_forever)
        worker.start()
        try:
            registered, _ = send(url, "/api/register", **auth, body={
                "device_id": mock, "character": "MockPeer", "port": peer.server_port, "ip": "127.0.0.1",
            })
            assert registered == {"ok": True, "peers": []}
            sender_endpoint = {"ip": "127.0.0.2", "port": 8080}
            registered, _ = send(url, "/api/register", **auth, body={
                "device_id": sender, "character": "Placeholder", **sender_endpoint,
            })
            mock_listing = {"device_id": mock, "character": "MockPeer", "ip": "127.0.0.1", "online": True}
            assert registered == {"ok": True, "peers": [mock_listing]}
            devices, _ = send(url, "/api/devices", **auth)
            assert mock_listing in devices["devices"] and len(devices["devices"]) == 2
            saved = json.loads(registry.read_text())
            assert all(saved[sender][k] == v for k, v in sender_endpoint.items())
            assert saved[mock]["port"] == peer.server_port
            assert all(int(time.time()) - p["last_seen"] < 180 for p in saved.values())
            print("PASS authenticated peer registration, discovery, and persistence", flush=True)

            silent = {"body": bytes(2 * 16000 * 2), "content_type": "audio/pcm"}
            send(url, "/api/voice", **auth, **silent, device="unregistered-local")
            assert "unregistered-local" not in json.loads(registry.read_text())
            send(url, "/api/voice", **auth, **silent, device=sender)
            current = json.loads(registry.read_text())[sender]
            assert all(current[k] == v for k, v in sender_endpoint.items()), "/api/voice replaced registration"
            assert current["character"] == "Placeholder"

            result, _ = send(url, "/api/intercom", **auth, device=sender, body=pcm, content_type="audio/pcm",
                             headers={"X-Character": "Cake", "X-Target": "mOcKpEeR"})
            current = json.loads(registry.read_text())[sender]
            assert all(current[k] == v for k, v in sender_endpoint.items()), "/api/intercom replaced registration"
            assert current["character"] == "Cake"
            assert result["ok"] and result["delivered_to"] == ["MockPeer"] and result["sent_to"] == [mock]
            assert result["reply"] == "Sent to MockPeer!"
            assert result["opencode_ms"] == 0 and "capital" in result["transcript"].lower()
            check_audio(url, token, result)
            shown, audio = received()
            normalized = normalize_voice_pcm(pcm)
            assert "capital" in shown.lower() and audio.endswith(normalized)
            assert normalized != pcm and len(audio) > len(normalized) + len(make_intercom_chime_pcm())
            assert normalize_voice_pcm(bytes(3200)) == bytes(3200)
            print("PASS intercom delivers transcribed card, reply button, chime and normalized original voice", flush=True)

            started = time.monotonic_ns()
            result, _ = send(url, "/api/text", **auth, body={
                "device_id": sender, "text": "Tell MockPeer dinner is ready!",
            })
            current = json.loads(registry.read_text())[sender]
            assert all(current[k] == v for k, v in sender_endpoint.items()), "/api/text replaced registration"
            assert result["ok"] and result["delivered_to"] == ["MockPeer"] and result["sent_to"] == [mock]
            assert result["opencode_ms"] == 0
            assert result["session_id"] == ""
            shown, audio = received()
            assert shown == "dinner is ready!" and len(audio) > 8000
            print(f"PASS direct text relay bypasses OpenCode ({(time.monotonic_ns()-started)//1_000_000} ms)", flush=True)

            result, _ = send(url, "/api/text", **auth, body={
                "device_id": sender, "text": "Send MockPeer a short dinosaur joke!",
            })
            sid = result["session_id"]
            assert SESSION_ID.fullmatch(sid) and result["opencode_ms"] > 0 and result["sent_to"] == [mock]
            assert result["delivered_to"] == ["MockPeer"]
            assert "```" not in result["reply"] and "echoear-message" not in result["reply"]
            shown, audio = received()
            samples = array("h", audio)
            assert "?" in shown and len(audio) > 32_000 and max(map(abs, samples)) > 1000
            path = f"/api/session/{sid}/message?order=desc&limit=1&type="
            user = upstream("OpenCode", "GET", path + "user")["data"][0]
            assert user["text"].startswith(f"[EchoEar: you are Cake ({sender}); other EchoEars: MockPeer ({mock})]")
            assistant = upstream("OpenCode", "GET", path + "assistant")["data"][0]
            assert assistant["agent"] == "echoear" and assistant["model"] == {
                "providerID": "opencode", "id": "gemini-4-neon", "variant": "high",
            }
            print("PASS OpenCode composes dinosaur joke and delivers it as card and Kokoro PCM", flush=True)

            changed_endpoint = {"ip": "127.0.0.3", "port": 8081}
            send(url, "/api/register", **auth, body={
                "device_id": sender, "character": "Cake", **changed_endpoint,
            })
            current = json.loads(registry.read_text())[sender]
            assert all(current[k] == v for k, v in changed_endpoint.items())
            print("PASS traffic preserves the registered address and port; re-registration updates them", flush=True)
        finally:
            peer.shutdown()
            worker.join()
            if sid and SESSION_ID.fullmatch(sid):
                path = f"/api/session/{sid}"
                if upstream("OpenCode", "GET", path)["data"].get("title") == f"EchoEar: {sender}":
                    upstream("OpenCode", "DELETE", path)


def main():
    config = settings()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=f"http://127.0.0.1:{config.get('ECHOEAR_BRIDGE_PORT', '8765')}")
    parser.add_argument("--wav", type=Path, help=f"16 kHz signed 16-bit mono WAV saying: {SPEECH.decode()}")
    args = parser.parse_args()
    token = secrets.token_hex(32)
    check_health(args.url.rstrip("/"))
    test_sentence_split()
    test_screen_parser()
    with tempfile.TemporaryDirectory(prefix="echoear-bridge-test-") as temp:
        root = Path(temp)
        test_ota(root)
        wav_path = args.wav or root / "speech.wav"
        if not args.wav:
            generated_wav(wav_path)
        with wave.open(str(wav_path), "rb") as wav:
            assert (wav.getnchannels(), wav.getsampwidth(), wav.getframerate()) == (1, 2, 16000)
            pcm = wav.readframes(wav.getnframes())
        with isolated_bridge(root, token) as (url, registry):
            test_intercom(url, token, pcm, registry)
            test_live(url, token, pcm)


if __name__ == "__main__":
    main()
