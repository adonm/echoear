#!/usr/bin/env python3
"""Live bridge test. Flite (or --wav) supplies speech; no model stubs."""

import argparse
import ctypes
import hashlib
import json
import os
import re
import shlex
import tempfile
import threading
import urllib.error
import urllib.request
import wave
from pathlib import Path

from server import Bridge, SESSION_ID, Sessions, json_bytes, split_screen, upstream

DEVICE = "bridge-selftest"
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


def send(url, path, *, body=None, content_type=None, token=None, device=None):
    headers = {}
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
            {"t": "circle", "x": -1, "y": 999, "r": 1.8, "c": "#ab00ef", "z": 100},
            {"t": "line", "x1": 1, "y1": 2, "x2": 3, "y2": 4, "c": "bad"},
        ] + [{"t": "text", "x": 1, "y": 2, "s": "🔥" * 80, "c": "#FFFFFF"}] * 40,
    })
    reply, screen = split_screen(f"Short answer.\r\n\r\n```echoear-screen\r\n{raw}\r\n```\r\n")
    assert reply == "Short answer."
    assert set(screen) == {"bg", "shapes"} and screen["bg"] == "#0B1020"
    assert screen["shapes"][0] == {"t": "circle", "x": 0, "y": 360, "r": 1, "c": "#AB00EF"}
    assert all(shape["t"] == "text" for shape in screen["shapes"][1:])
    assert 1 < len(screen["shapes"]) <= 24 and len(json_bytes(screen)) < 4000
    for block in ("```echoear-screen\n{bad}\n```", '```echoear-screen\n{"title":"Unclosed"}'):
        assert split_screen("Short answer.\n" + block) == ("Short answer.", None)
    print("PASS screen extraction, validation, clamping, and size limit", flush=True)


def test_visual(url, token, sid):
    result, _ = send(url, "/api/text", token=token, body={
        "device_id": DEVICE,
        "text": "What is a right triangle? Please draw a simple diagram on the screen.",
    })
    assert result["session_id"] == sid
    reply = result["reply"]
    assert 0 < len(reply) <= 260 and "```" not in reply and "echoear-screen" not in reply
    assert len(re.findall(r"[.!?](?:\s|$)", reply)) <= 3 and not re.search(r"(?m)^\s*(?:#|[*-] )", reply)
    screen = result["screen"]
    assert isinstance(screen, dict) and 0 < len(screen["shapes"]) <= 24 and len(json_bytes(screen)) < 4000
    assert all(re.fullmatch(r"#[0-9A-F]{6}", s["c"]) for s in screen["shapes"])
    assert any(s["t"] == "line" for s in screen["shapes"])
    model = {"providerID": "opencode", "id": "gemini-4-neon", "variant": "high"}
    messages = upstream("OpenCode", "GET", f"/api/session/{sid}/message?order=desc&limit=1&type=assistant")
    latest = messages["data"][0]
    assert latest["agent"] == "echoear" and latest["model"] == model, (latest["agent"], latest["model"])
    print("PASS right-triangle diagram with plain prose; assistant used echoear / Gemini 4 Neon high", flush=True)


def test_live(url, token, pcm):
    health, _ = send(url, "/health")
    assert health == {"ok": True, "hex": True, "opencode": True}, f"Unexpected health: {health}"
    print("PASS LAN health, Hex and OpenCode reachable", flush=True)
    for path in ("/api/voice", "/api/text", "/api/reset"):
        rejects(401, url, path, body=b"")
    for path in ("/ota/version.json", "/ota/latest.bin"):
        rejects(401, url, path)
    print("PASS API and OTA reject requests without a token", flush=True)

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
        assert all(type(result[key]) is int and result[key] >= 0 for key in ("hex_ms", "opencode_ms"))
        print(f"PASS spoken PCM -> Hex -> OpenCode ({result['hex_ms']} ms / {result['opencode_ms']} ms)", flush=True)
        followup, _ = send(url, "/api/text", token=token, body={
            "device_id": DEVICE,
            "text": "What city did you just answer with? Reply with only the city name.",
        })
        assert followup["session_id"] == sid
        assert re.fullmatch(r"paris[.!]?", followup["reply"].strip(), re.IGNORECASE)
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
    print("PASS reset and silent PCM skip OpenCode", flush=True)


def main():
    config = settings()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=f"http://127.0.0.1:{config.get('ECHOEAR_BRIDGE_PORT', '8765')}")
    parser.add_argument("--wav", type=Path, help=f"16 kHz signed 16-bit mono WAV saying: {SPEECH.decode()}")
    args = parser.parse_args()
    token = config.get("ECHOEAR_BRIDGE_TOKEN")
    if not token:
        parser.error("Missing ECHOEAR_BRIDGE_TOKEN")
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
        test_live(args.url.rstrip("/"), token, pcm)


if __name__ == "__main__":
    main()
