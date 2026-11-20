import base64
import json
import logging
import os
import threading
from http.client import HTTPException
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from state import SESSION_ID, Store

DATA = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share")
STATE = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state")
PROJECT = Path(__file__).resolve().parent
JSON = "application/json"
LOG = logging.getLogger("echoear")


class Error(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status


class UpstreamStatus(Error):
    def __init__(self, service, status):
        super().__init__(502, f"{service} returned HTTP {status}")
        self.upstream_status = status


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *_):
        return None


HTTP = build_opener(ProxyHandler({}), NoRedirect())


def json_bytes(value):
    return json.dumps(value, separators=(",", ":")).encode()


def _http_request(url, method, body, headers, timeout):
    data = json_bytes(body) if isinstance(body, dict) else body
    return HTTP.open(Request(url, method=method, data=data, headers=headers), timeout=timeout)


def service_endpoint(service):
    host = "127.0.0.1"
    if service == "Hex":
        info = json.loads((DATA / "hex/local-api.json").read_text())
        port, auth = info["port"], "Bearer " + info["token"]
    else:
        info = json.loads((STATE / "opencode/service.json").read_text())
        url = urlsplit(info["url"])
        if url.scheme != "http" or url.hostname not in ("0.0.0.0", "127.0.0.1", "::", "::1", "localhost"):
            raise ValueError("Not a local OpenCode URL")
        port = url.port
        host = "[::1]" if url.hostname in ("::", "::1") else host
        auth = "Basic " + base64.b64encode(f"opencode:{info['password']}".encode()).decode("ascii")
    if type(port) is not int or not 1 <= port <= 65535:
        raise ValueError("Invalid service port")
    return f"http://{host}:{port}", auth


def invalid_response(service, context):
    LOG.warning("%s: invalid response (%s)", service, context)
    return Error(502, f"{service} returned invalid data")


def upstream(service, method, path, body=None, content_type=JSON, timeout=5):
    try:
        url, auth = service_endpoint(service)
        headers = {"Authorization": auth}
        if body is not None:
            headers["Content-Type"] = content_type
        limit = 4 * 1024 * 1024
        with _http_request(url + path, method, body, headers, timeout) as response:
            payload = response.read(limit + 1)
        if len(payload) > limit:
            raise ValueError("Oversized response")
        return json.loads(payload) if payload else None
    except HTTPError as error:
        error.close()
        raise UpstreamStatus(service, error.code) from None
    except (OSError, HTTPException, KeyError, TypeError, ValueError, RecursionError) as error:
        reason = error.reason if isinstance(error, URLError) else error
        LOG.warning("%s %s: %s", service, method, type(reason).__name__)
        if isinstance(reason, TimeoutError):
            raise Error(504, f"{service} timed out") from None
        raise Error(502, f"{service} unavailable or returned invalid data") from None


def reachable(service, path):
    try:
        upstream(service, "GET", path, timeout=1.5)
        return True
    except Error:
        return False


def transcribe(wav):
    try:
        selected = json.loads((DATA / "hex/settings.json").read_text())["transcription"]
        if not isinstance(selected["model"], str) or not selected["model"]:
            raise ValueError("Missing model")
        query = urlencode({"model": selected["model"], "language": selected.get("language", "en")})
    except (OSError, KeyError, TypeError, ValueError, RecursionError):
        raise invalid_response("Hex", "selected transcription model") from None
    result = upstream("Hex", "POST", "/transcriptions?" + query, wav, "audio/wav", timeout=60)
    if not isinstance(result, dict) or not isinstance(result.get("transcript"), str):
        raise invalid_response("Hex", "transcript")
    return result["transcript"].strip()


def opencode_data(response, expected=dict):
    if isinstance(response, dict) and isinstance(response.get("data"), expected):
        return response["data"]
    raise invalid_response("OpenCode", "data envelope")


def final_reply(messages, prompt_id):
    reply = None
    for message in messages:
        if not isinstance(message, dict) or not isinstance(message.get("id"), str) or not isinstance(message.get("type"), str):
            raise invalid_response("OpenCode", "message")
        kind = message["type"]
        if message["id"] == prompt_id:
            if reply:
                return reply
            break
        if kind == "idle" and message.get("outcome") != "succeeded":
            error = message.get("error")
            if isinstance(error, dict) and error.get("status") == 429:
                raise Error(503, "OpenCode model usage limit reached")
            raise Error(502, "OpenCode turn did not complete successfully")
        if kind == "assistant" and reply is None and message.get("finish") in ("stop", "length"):
            parts = message.get("content")
            if not isinstance(parts, list) or any(not isinstance(p, dict) or (
                p.get("type") == "text" and not isinstance(p.get("text"), str)) for p in parts):
                raise invalid_response("OpenCode", "assistant content")
            reply = "\n".join(p["text"] for p in parts if p.get("type") == "text").strip()
    raise Error(502, "OpenCode did not return a final assistant reply")


class Sessions:
    def __init__(self, path, agent, model):
        self.store = Store(path, lambda _, sid: isinstance(sid, str) and SESSION_ID.fullmatch(sid))
        self.turns = [threading.Lock() for _ in range(64)]
        self.settings = dict(agent=agent, model=model, permissions=[dict(action="*", resource="*", effect="deny")])
        if agent == "echoear":
            self.settings["permissions"].append(dict(action="webfetch", resource="*", effect="allow"))

    def get(self, device):
        with self.store.lock:
            return self.store.entries.get(device, "")

    def save(self, device, sid):
        with self.store.lock:
            entries = dict(self.store.entries)
            if sid:
                entries[device] = sid
            elif device in entries:
                del entries[device]
            else:
                return
            self.store.replace(entries)

    def reset(self, device):
        with self.turns[hash(device) % len(self.turns)]:
            self.save(device, "")

    def ask(self, device, text):
        def call(method, path, body=None, timeout=5):
            return upstream("OpenCode", method, path, body, timeout=timeout)

        with self.turns[hash(device) % len(self.turns)]:
            sid, session = self.get(device), None
            if sid:
                try:
                    session = opencode_data(call("GET", f"/api/session/{sid}"))
                    if session.get("id") != sid:
                        raise invalid_response("OpenCode", "session identity")
                except UpstreamStatus as error:
                    if error.upstream_status != 404:
                        raise
            if session is None:
                created = call("POST", "/api/session", dict(
                    title=f"EchoEar: {device}", location={"directory": str(PROJECT)}, **self.settings))
                sid = opencode_data(created).get("id")
                if not isinstance(sid, str) or not SESSION_ID.fullmatch(sid):
                    raise invalid_response("OpenCode", "new session ID")
                self.save(device, sid)
            path = f"/api/session/{sid}"
            if session is not None:
                for key, value in self.settings.items():
                    if session.get(key) == value:
                        continue
                    method, endpoint = ("PATCH", path) if key == "permissions" else ("POST", f"{path}/{key}")
                    call(method, endpoint, {key: value})
            prompt = opencode_data(call("POST", path + "/prompt", {"text": text})).get("id")
            if not isinstance(prompt, str) or not prompt:
                raise invalid_response("OpenCode", "prompt ID")
            call("POST", f"/api/experimental/session/{sid}/wait", timeout=90)
            messages = opencode_data(call("GET", path + "/message?order=desc&limit=32"), list)
            return final_reply(messages, prompt), sid
