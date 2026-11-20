#!/usr/bin/env python3
"""Warm Kokoro worker. File descriptor 1 belongs exclusively to the protocol."""
import json
import os
import struct
import sys
from pathlib import Path


class UnknownVoice(ValueError):
    pass


def voice_request(raw, available):
    # The original first field was a bare voice ID. Keep its framing and
    # default to US English when a legacy caller doesn't specify a language.
    request = json.loads(raw) if raw.startswith(b"{") else {"voice": raw.decode("utf-8")}
    voice = request["voice"]
    if not isinstance(voice, str) or voice not in available:
        raise UnknownVoice()
    return voice, request.get("lang") or "en-us"


def pin_physical_cores():
    allowed = sorted(os.sched_getaffinity(0))
    physical = {}
    try:
        for cpu in allowed:
            topology = Path(f"/sys/devices/system/cpu/cpu{cpu}/topology")
            key = tuple((topology / field).read_text().strip()
                        for field in ("physical_package_id", "core_id"))
            physical.setdefault(key, cpu)
        cpus = list(physical.values())[:8]
        os.sched_setaffinity(0, cpus)
        return len(cpus)
    except OSError:
        return min(8, len(allowed))


def main(model, voices):
    out = os.fdopen(os.dup(1), "wb")
    os.dup2(2, 1)
    threads = pin_physical_cores()

    import numpy as np
    import onnxruntime as ort
    from kokoro_onnx import Kokoro

    options = ort.SessionOptions()
    options.intra_op_num_threads = threads
    options.inter_op_num_threads = 1
    options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    options.add_session_config_entry("session.intra_op.allow_spinning", "0")
    session = ort.InferenceSession(model, sess_options=options,
                                   providers=["CPUExecutionProvider"])
    kokoro = Kokoro.from_session(session, voices)
    available = set(kokoro.get_voices())

    def speak(voice, lang, text):
        samples, rate = kokoro.create(text, voice=voice, speed=1.02, lang=lang)
        if samples.ndim != 1 or not 0 < samples.size <= 1_572_864:
            raise ValueError("Invalid synthesized audio length")
        return rate, memoryview(np.ascontiguousarray(samples, dtype="<f4")).cast("B")

    source = sys.stdin.buffer

    def field(header, maximum):
        size, = struct.unpack("<I", header)
        if size > maximum:
            raise ValueError("Oversized request field")
        value = source.read(size)
        if len(value) != size:
            raise EOFError("Incomplete request field")
        return value

    while header := source.read(4):
        metadata = field(header, 128)
        text = field(source.read(4), 4096)
        status = 0
        try:
            voice, lang = voice_request(metadata, available)
            rate, payload = speak(voice, lang, text.decode("utf-8") if text else "Hello!")
            if not text:  # Startup probe warms the same catalog voice as Go.
                rate, payload = 0, b""
        except Exception as exc:
            rate, payload, status = 0, type(exc).__name__.encode("ascii"), 1
        out.write(struct.pack("<BII", status, rate, len(payload)))
        out.write(payload)
        out.flush()


if __name__ == "__main__":
    try:
        main(*sys.argv[1:])
    except Exception as exc:
        print(f"Kokoro worker: {type(exc).__name__}", file=sys.stderr)
        sys.exit(1)
