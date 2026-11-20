#!/usr/bin/env python3
import argparse
import ctypes
import hashlib
import json
import os
import queue
import secrets
import shlex
import subprocess
import tempfile
import threading
import time
import wave
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from http.client import IncompleteRead
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from struct import iter_unpack
from unittest.mock import patch
from urllib.error import HTTPError
from urllib.parse import urlencode

from backend import DATA, Error, Sessions, _http_request, json_bytes, opencode_data, upstream
from server import AudioStream, Bridge, extract_payloads, message_card
from state import SESSION_ID, Peers, write_map
from tts import make_intercom_chime_pcm, normalize_voice_pcm, split_sentences

SPEECH = (b"What is a right triangle? Please explain in four complete sentences and draw a simple "
          b"diagram with a button I can tap to learn more.")
MODEL = {"providerID": "opencode", "id": "gemini-4-neon", "variant": "high"}


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def expect(kind, operation):
    try:
        operation()
    except kind as error:
        return error
    raise AssertionError(f"Expected {kind.__name__}")


class _BridgeClient:
    def __init__(self, url, token):
        self.url, self.token = url.rstrip("/"), token

    def open(self, path, body=None, *, pcm=False, auth=True, **headers):
        if auth:
            headers["X-EchoEar-Token"] = self.token
        if body is not None:
            headers["Content-Type"] = "audio/pcm" if pcm else "application/json"
        return _http_request(self.url + path, "GET" if body is None else "POST", body, headers, 120)

    def call(self, path, body=None, **kwargs):
        with self.open(path, body, **kwargs) as response:
            data = response.read()
            return (json.loads(data) if response.headers.get_content_type() == "application/json" else data,
                    response.headers)

    def json(self, path, body=None, **kwargs):
        return self.call(path, body, **kwargs)[0]

    def rejects(self, status, path, body=None, **kwargs):
        with expect(HTTPError, lambda: self.call(path, body, **kwargs)) as error:
            check(error.code == status, f"{path}: expected {status}, got {error.code}")


@contextmanager
def running(server):
    with server:
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        thread.start()
        try:
            yield server.server_port
        finally:
            server.shutdown()
            thread.join()


def test_gated_stream(root):
    with running(bridge := Bridge(("127.0.0.1", 0), "unit", root, Sessions(root / "sessions.json", None, None))) as port:
        client = _BridgeClient(f"http://127.0.0.1:{port}", "unit")
        first, last = b"\x01\x00" * 32, b"\x02\x00" * 32
        for fail in (False, True):
            tail, stream = threading.Event(), AudioStream()

            class Gated:
                def synthesize_sentences(self, _):
                    yield first
                    if not tail.wait(2) or fail:
                        raise RuntimeError("tail failed")
                    yield last

            with ThreadPoolExecutor(max_workers=2) as workers:
                waiting = workers.submit(stream.chunk, 0)
                workers.submit(stream.produce, Gated(), "unit")
                try:
                    check(waiting.result(1) == first and not stream.done, "first chunk did not wake before tail")
                    path = bridge.cache_audio(stream)
                    with client.open(path) as response:
                        check(response.headers["Transfer-Encoding"] == "chunked", "missing chunked framing")
                        check(response.read(len(first)) == first, "wrong first chunk")
                        tail.set()
                        if fail:
                            expect(IncompleteRead, response.read)
                        else:
                            check(response.read() == last, "wrong tail")
                finally:
                    tail.set()
            if fail:
                client.rejects(502, path)
            else:
                pcm, headers = client.call(path)
                check(pcm == first + last and int(headers["Content-Length"]) == len(pcm), "bad cached replay")
        stalled = AudioStream()
        with patch("server.TTS_WAIT", 0.01):
            expect(Error, lambda: stalled.chunk(0))
        client.rejects(502, bridge.cache_audio(stalled))


def test_unit(root):
    button = dict(t="btn", x=-1, y=360, w=180, h=40, c="#abcdef", s="Explore", tap="**Hi**",
                  ask="x" * 130, reply_to="**mint-id**")
    shapes = [button, dict(button, c="bad")] + [dict(button, ask="🔥" * 120)] * 30
    text, screen, message = extract_payloads("Hi!\n```echoear-message\n"
        '{"to":"Mint","text":"**Hi**"}\n```\n```echoear-screen\n' + json.dumps({"shapes": shapes}) + "\n```")
    check(text == "Hi!" and message == ("Mint", "Hi"), "message extraction")
    check(0 < len(screen["shapes"]) <= 24 and len(json_bytes(screen)) < 6000, "screen limits")
    check(screen["shapes"][0] == dict(button, x=0, y=359, c="#ABCDEF", tap="Hi", ask="x" * 120,
                                     reply_to="mint-id"), "button sanitizer")
    for raw in ('```echoear-screen\n{"title":"unclosed"}', "```echoear-message\n{bad}\n```"):
        check(extract_payloads("Hi!\n" + raw) == ("Hi!", None, None), "invalid fence")
    card = message_card({"device_id": "cake-id", "character": "Cake"}, "Hi")
    check(card["shapes"][-1]["reply_to"] == "cake-id", "reply not routed by device ID")
    check(split_sentences("**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral?")
          == ["Whoa! Dr. Lee found a 3.2-inch shell.", "Ooh, awesome question! What made it spiral?"], "sentence split")
    loud = (16000).to_bytes(2, "little", signed=True) * 60
    normalized = normalize_voice_pcm(loud)
    check(25_000 < int.from_bytes(normalized[:2], "little", signed=True) < 26_100
          and normalize_voice_pcm(bytes(80)) == bytes(80), "PCM normalization")
    peers = Peers(root / "devices.json")
    old_ip, new_ip = "192.0.2.20", "192.0.2.21"
    peers.register("mint", old_ip, "Mint", 8080)
    with patch("state.is_host_ip", return_value=True):
        peers.touch("mint", "127.0.0.1")
        peers.touch("local", "192.0.2.1")
    peers.touch("mint", "192.0.2.99")
    check([p["device_id"] for p in peers.targets("other", "EVERYONE")] == ["mint"], "host incorrectly registered")
    saved = json.loads((root / "devices.json").read_text())["mint"]
    check((saved["ip"], saved["port"]) == (old_ip, 8080), "traffic overwrote address")
    check(peers.direct("cake", "Hey EchoEar, tell MINT: dinner is ready!") == ("MINT", "dinner is ready!"), "direct relay")
    for phrase in ("a joke", "a short dinosaur story", "a space fact", "about the Moon"):
        check(peers.direct("cake", "Send Mint " + phrase) is None, "composition used fast path")
    peers.register("mint", new_ip, "Mint", 80)
    check(peers.public()[0]["ip"] == new_ip, "explicit re-registration did not replace address")
    path = root / "state.json"
    write_map(path, {"old": 1})
    with patch("state.os.replace", side_effect=OSError):
        expect(OSError, lambda: write_map(path, {"new": 2}))
    check(json.loads(path.read_text()) == {"old": 1} and path.stat().st_mode & 0o777 == 0o600,
          "failed write lost data or permissions")
    check(len(list(root.iterdir())) == 2, "temporary file leaked")
    for bad in ({"data": []}, ["bad envelope"]):
        check(expect(Error, lambda: opencode_data(bad)).status == 502, "malformed upstream did not map to 502")
    test_gated_stream(root)
    (root / "devices.json").write_text('{"mint":[]}')
    (root / "sessions.json").write_text("[" * 2000 + "]" * 2000)
    with running(Bridge(("127.0.0.1", 0), "unit", root, Sessions(root / "sessions.json", None, None))) as port:
        client = _BridgeClient(f"http://127.0.0.1:{port}", "unit")
        client.rejects(502, "/api/devices")
        client.rejects(502, "/api/reset", {"device_id": "mint"})
    print("PASS offline: payloads, peers, PCM, atomic state, malformed JSON and gated streaming")


def recording(path, generate):
    if generate:
        try:
            flite, voice = ctypes.CDLL("libflite.so.1"), ctypes.CDLL("libflite_cmu_us_kal16.so.2.2")
        except OSError:
            raise SystemExit("Flite unavailable; supply --wav") from None
        flite.flite_init()
        voice.register_cmu_us_kal16.argtypes = [ctypes.c_char_p]
        voice.register_cmu_us_kal16.restype = ctypes.c_void_p
        flite.flite_text_to_speech.argtypes = [ctypes.c_char_p, ctypes.c_void_p, ctypes.c_char_p]
        flite.flite_text_to_speech.restype = ctypes.c_float
        length = flite.flite_text_to_speech(SPEECH, voice.register_cmu_us_kal16(None), os.fsencode(path))
        check(length > 0, "speech generation failed")
    with wave.open(str(path), "rb") as wav:
        check((wav.getnchannels(), wav.getsampwidth(), wav.getframerate()) == (1, 2, 16000), "invalid test WAV")
        return wav.readframes(wav.getnframes())


@contextmanager
def isolated_bridge(root, token):
    data = root / "data"
    (data / "echoear").mkdir(parents=True)
    for name in ("hex", "echoear/tts-models"):
        (data / name).symlink_to(DATA / name, target_is_directory=True)
    env = dict(os.environ, XDG_DATA_HOME=str(data), ECHOEAR_BRIDGE_HOST="127.0.0.1", ECHOEAR_BRIDGE_PORT="0",
               ECHOEAR_BRIDGE_TOKEN=token, ECHOEAR_OTA_DIR=str(root / "ota"), ECHOEAR_OPENCODE_AGENT="echoear",
               ECHOEAR_OPENCODE_MODEL="opencode/gemini-4-neon#high")
    python = DATA / "echoear/tts-venv/bin/python"
    with subprocess.Popen([str(python), "-B", str(Path(__file__).with_name("server.py"))],
                          stdout=subprocess.PIPE, text=True, env=env) as child:
        try:
            for line in child.stdout:
                if line.startswith("EchoEar bridge listening on 127.0.0.1:"):
                    yield _BridgeClient(f"http://127.0.0.1:{int(line.rsplit(':', 1)[1])}", token)
                    return
            raise AssertionError("isolated bridge did not start")
        finally:
            child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()


def opencode(method, path):
    return upstream("OpenCode", method, path)


def test_actor(sid, kind="assistant"):
    check(isinstance(sid, str) and SESSION_ID.fullmatch(sid), "missing OpenCode session")
    result = opencode("GET", f"/api/session/{sid}/message?order=desc&limit=1&type={kind}")["data"][0]
    if kind == "assistant":
        check(result["agent"] == "echoear" and result["model"] == MODEL, "wrong OpenCode agent or model")
    return result


def pcm_ok(pcm):
    samples = [n for n, in iter_unpack("<h", pcm)]
    return len(pcm) > 8000 and sum(n * n for n in samples) > 100**2 * len(samples)


def test_voice(client, pcm, device):
    headers = {"X-Device-Id": device}
    result = client.json("/api/voice", pcm, pcm=True, **headers)
    posted = time.monotonic_ns()
    check("right triangle" in result["transcript"].lower() and result["screen"], "Hex transcript or screen missing")
    check(result["audio_streaming"] and result["audio_bytes"] == 0 and result["sample_rate"] == 16000
          and result["tts_first_ms"] == result["tts_ms"], "audio metadata")
    path = result["audio_url"]
    with client.open(path) as response:
        check(response.version == 11 and response.headers["Transfer-Encoding"] == "chunked", "not chunked HTTP/1.1")
        first, rest = response.read1(), response.read()
    full = result["tts_first_ms"] + (time.monotonic_ns() - posted) // 1_000_000
    cached, headers_reply = client.call(path)
    check(first and rest and cached == first + rest and int(headers_reply["Content-Length"]) == len(cached)
          and pcm_ok(cached), "stream/replay mismatch or silent audio")
    sid = result["session_id"]
    test_actor(sid)
    followup = client.json("/api/text", {"device_id": device, "text": "Reply with only the word Paris."})
    check(followup["session_id"] == sid, "text did not reuse voice session")
    client.json("/api/reset", {"device_id": device})
    silence = client.json("/api/voice", bytes(64000), pcm=True, **headers)
    check(not silence["session_id"] and not silence["reply"] and silence["audio_url"] is None, "silence opened a session")
    print(f"PASS Hex → OpenCode → Kokoro chunked PCM and identical replay ({result['tts_first_ms']}/{full} ms)")


def test_intercom(client, pcm, run):
    received = queue.Queue()
    sender, recipient = f"{run}-cake", f"{run}-mock"

    class MockPeer(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers["Content-Length"]))
            received.put((self.path, body))
            self.send_response(204 if self.headers.get("X-EchoEar-Token") == client.token else 401)
            self.end_headers()

    def delivery():
        display, speak = received.get(timeout=2), received.get(timeout=2)
        check((display[0], speak[0]) == ("/display", "/speak"), "missing peer request")
        card = json.loads(display[1])
        button = card["shapes"][-1]
        check(card["title"] == "From Cake!" and button["s"] == "Reply to Cake" and button["reply_to"] == sender,
              "intercom message card")
        check(speak[1].startswith(make_intercom_chime_pcm()), "missing chime")
        return speak[1]

    with running(ThreadingHTTPServer(("127.0.0.1", 0), MockPeer)) as port:
        client.json("/api/register", dict(device_id=recipient, character="MockPeer", port=port))
        registered = client.json("/api/register", dict(device_id=sender, character="Cake", ip="127.0.0.2"))
        check([p["device_id"] for p in registered["peers"]] == [recipient], "peer discovery failed")
        direct = client.json("/api/intercom", pcm, pcm=True, **{"X-Device-Id": sender, "X-Target": "mockpeer"})
        audio = delivery()
        check(direct["ok"] and direct["opencode_ms"] == 0 and direct["delivered_to"] == ["MockPeer"]
              and audio.endswith(normalize_voice_pcm(pcm)), "voice intercom failed")
        started = time.monotonic_ns()
        direct = client.json("/api/text", dict(device_id=sender, text="Tell MockPeer dinner is ready!"))
        check(direct["sent_to"] == [recipient] and not direct["session_id"] and direct["opencode_ms"] == 0,
              "direct relay called OpenCode or did not deliver")
        delivery()
        elapsed = (time.monotonic_ns() - started) // 1_000_000
        message = client.json("/api/text", dict(device_id=sender, text="Send MockPeer a short dinosaur joke!"))
        audio = delivery()
        check(message["sent_to"] == [recipient] and "```" not in message["reply"] and pcm_ok(audio),
              "composed intercom message failed")
        test_actor(message["session_id"])
        user = test_actor(message["session_id"], "user")
        check(user["text"].startswith(f"[EchoEar: you are Cake ({sender}); other EchoEars: MockPeer ({recipient})]"),
              "peer context missing")
        devices = client.json("/api/devices")["devices"]
        check(any(p["device_id"] == sender and p["ip"] == "127.0.0.2" for p in devices), "IP guard regression")
        print(f"PASS discovery and voice intercom; direct relay {elapsed} ms; AI dinosaur joke delivered")


def deployed_config():
    root = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    env = root / "echoear/bridge.env"
    fields = {}
    if env.exists():
        for line in env.read_text().splitlines():
            key, _, value = line.partition("=")
            if key in ("ECHOEAR_BRIDGE_PORT", "ECHOEAR_BRIDGE_TOKEN") and (parts := shlex.split(value, comments=True)):
                fields[key] = parts[0]
    return {**fields, **os.environ}


def owned_sessions(run):
    path = "/api/session?" + urlencode({"search": run, "limit": 50})
    return [s["id"] for s in opencode("GET", path)["data"] if s.get("title", "").startswith(f"EchoEar: {run}-")]


def smoke(client, root, wav):
    def real_ips():
        return {p["device_id"]: p["ip"] for p in client.json("/api/devices")["devices"] if p["character"] in ("Cake", "Mint")}

    health = client.json("/health")
    check(all(health.get(key) is True for key in ("ok", "hex", "opencode", "tts")), "deployed health check")
    before = real_ips()
    run = "bridge-selftest-" + secrets.token_hex(8)
    pcm = recording(wav or root / "speech.wav", wav is None)
    ota = root / "ota"
    ota.mkdir()
    try:
        with isolated_bridge(root, secrets.token_hex(32)) as test:
            test.rejects(401, "/api/devices", auth=False)
            test.rejects(401, "/api/intercom", {}, auth=False)
            test.rejects(401, "/ota/latest.bin", auth=False)
            test.rejects(404, "/ota/latest.bin")
            check(test.json("/ota/version.json")["exists"] is False, "missing manifest")
            data = b"EchoEar OTA fixture" * 4000
            (ota / "echoear.bin").write_bytes(data)
            manifest = test.json("/ota/version.json")
            image, headers = test.call("/ota/latest.bin")
            check(image == data and manifest["sha256"] == headers["X-Firmware-Sha256"] == hashlib.sha256(data).hexdigest()
                  and manifest["size"] == int(headers["Content-Length"]) == len(data), "OTA manifest or stream mismatch")
            sample, headers = test.call("/api/tts", {"text": "The Moon reflects sunlight."})
            check(headers.get_content_type() == "audio/pcm" and pcm_ok(sample), "direct TTS regression")
            test_intercom(test, pcm, run)
            test_voice(test, pcm, run + "-voice")
    finally:
        for sid in owned_sessions(run):
            opencode("DELETE", f"/api/session/{sid}")
        check(not owned_sessions(run), "test sessions leaked")
        check(real_ips() == before, "deployed Cake/Mint addresses changed")
    print(f"PASS health, auth, OTA, direct TTS; no test sessions left; {len(before)} real peer addresses intact")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--unit", action="store_true")
    parser.add_argument("--url", help="Deployed bridge URL (default: localhost)")
    parser.add_argument("--wav", type=Path, help=f"16 kHz PCM WAV saying: {SPEECH.decode()}")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="echoear-bridge-test-") as directory:
        root = Path(directory)
        unit = root / "unit"
        unit.mkdir()
        test_unit(unit)
        if not args.unit:
            config = deployed_config()
            url = args.url or f"http://127.0.0.1:{config.get('ECHOEAR_BRIDGE_PORT', '8765')}"
            token = config.get("ECHOEAR_BRIDGE_TOKEN")
            if not token:
                parser.error("Missing ECHOEAR_BRIDGE_TOKEN")
            smoke(_BridgeClient(url, token), root, args.wav)


if __name__ == "__main__":
    main()
