import errno
import ipaddress
import json
import os
import re
import socket
import tempfile
import threading
import time
from pathlib import Path

BROADCAST = ("all", "everyone", "everybody")
DEVICE_ID = re.compile(r"[A-Za-z0-9_.-]{1,64}")
SESSION_ID = re.compile(r"ses[A-Za-z0-9_-]{1,128}")
CHARACTER = re.compile(r"[A-Za-z0-9][A-Za-z0-9 ._'’-]{0,39}")
LAN_NETWORKS = tuple(map(ipaddress.ip_network, (
    "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "fc00::/7",
)))
DIRECT = re.compile(r"^(?:hey\s+[\w-]+,?\s+)?(?:tell|message|intercom|walkie|say\s+to|send)\s+", re.I)
COMPOSE = re.compile(
    r"^(?:about\b|(?:(?:an?|the|some)\s+(?:[\w'-]+\s+){0,5})?(?:joke|story|fact|riddle|quiz)\b)", re.I)


class StateError(RuntimeError):
    pass


class Store:
    def __init__(self, path, valid):
        self.path, self.lock = path, threading.Lock()
        try:
            entries = json.loads(path.read_text())
            self._entries = entries if isinstance(entries, dict) and all(
                DEVICE_ID.fullmatch(key) and valid(key, value) for key, value in entries.items()) else None
        except FileNotFoundError:
            self._entries = {}
        except (OSError, ValueError, RecursionError):
            self._entries = None

    @property
    def entries(self):
        if self._entries is None:
            raise StateError(f"Invalid {self.path.name}")
        return self._entries

    def replace(self, entries):
        try:
            write_map(self.path, entries)
        except OSError as error:
            raise StateError(f"Cannot save {self.path.name}: {type(error).__name__}") from None
        self._entries = entries


def write_map(path, value):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as file:
        temp = Path(file.name)
        try:
            os.fchmod(file.fileno(), 0o600)
            json.dump(value, file)
            file.flush()
            os.fsync(file.fileno())
            file.close()
            os.replace(temp, path)
        finally:
            temp.unlink(missing_ok=True)


def lan_ip(value, allow_loopback=False):
    if not isinstance(value, str):
        raise ValueError("Expected a numeric LAN IP address")
    try:
        ip = ipaddress.ip_address(value)
    except ValueError:
        raise ValueError("Expected a numeric LAN IP address") from None
    if (ip.is_loopback and allow_loopback) or any(ip in net for net in LAN_NETWORKS):
        return str(ip)
    raise ValueError("Peer address must be on the LAN")


def is_host_ip(ip):
    address = ipaddress.ip_address(ip)
    if address.is_loopback:
        return True
    with socket.socket(socket.AF_INET if address.version == 4 else socket.AF_INET6, socket.SOCK_DGRAM) as sock:
        try:
            sock.bind((ip, 0))
        except OSError as error:
            if error.errno == errno.EADDRNOTAVAIL:
                return False
            raise
    return True


def valid_peer(device, peer):
    if not isinstance(peer, dict) or peer.get("device_id") != device:
        return False
    try:
        lan_ip(peer.get("ip"), allow_loopback=True)
    except ValueError:
        return False
    character, port, seen = (peer.get(key) for key in ("character", "port", "last_seen"))
    return (isinstance(character, str) and (CHARACTER.fullmatch(character) or character == device[:40])
            and type(port) is int and 1 <= port <= 65535 and type(seen) is int and seen >= 0)


class Peers:
    def __init__(self, path):
        self.store = Store(path, valid_peer)

    def _save(self, device, peer):
        if self.store.entries.get(device) != peer:
            self.store.replace({**self.store.entries, device: peer})

    def register(self, device, ip, character, port):
        with self.store.lock:
            self._save(device, dict(device_id=device, character=character, ip=ip, port=port,
                                    last_seen=int(time.time())))

    def touch(self, device, client_ip, character=None):
        with self.store.lock:
            old = self.store.entries.get(device, {})
            peer = dict(old, device_id=device, character=character or old.get("character", device[:40]),
                        last_seen=int(time.time()))
            # Only explicit registration replaces an address. Local bridge clients never become peers.
            if not peer.get("ip") and not is_host_ip(client_ip):
                peer.update(ip=client_ip, port=old.get("port", 80))
            if old or peer.get("ip"):
                self._save(device, peer)
            return peer

    def others(self, sender=None):
        with self.store.lock:
            return [p for key, p in sorted(self.store.entries.items())
                    if p.get("ip") and key.casefold() != (sender or "").casefold()]

    def public(self, sender=None):
        now = time.time()
        return [dict(device_id=p["device_id"], character=p["character"], ip=p["ip"],
                     online=now - p["last_seen"] <= 180) for p in self.others(sender)]

    def targets(self, sender, target):
        target, peers = target.casefold(), self.others(sender)
        if target in BROADCAST:
            return peers
        return ([p for p in peers if p["device_id"].casefold() == target]
                or [p for p in peers if p["character"].casefold() == target])

    def direct(self, sender, text):
        if not (prefix := DIRECT.match(text.strip())):
            return None
        names = {*BROADCAST}
        for peer in self.others(sender):
            names.update((peer["character"], peer["device_id"]))
        choices = "|".join(map(re.escape, sorted(names, key=len, reverse=True)))
        match = re.fullmatch(rf"({choices})(?:[\s,:]+|[-–—]\s+)(.*\S)\s*", prefix.string[prefix.end():], re.I | re.S)
        if match:
            target, message = match.groups()
            message = message.lstrip(" \t\r\n,:-–—")
            if message and not COMPOSE.match(message):
                return target, message

    def prompt(self, sender, text):
        names = [f"{p['character']} ({p['device_id']})" for p in self.others(sender["device_id"])[:16]]
        return (f"[EchoEar: you are {sender['character']} ({sender['device_id']}); "
                f"other EchoEars: {', '.join(names) or 'none'}]\n{text}")
