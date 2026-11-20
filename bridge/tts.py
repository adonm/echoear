"""Local Kokoro speech as mono signed 16-bit little-endian PCM."""

import math
import os
import threading

SAMPLE_RATE = 16000
MAX_PCM = 2 * 1024 * 1024
MAX_TEXT = 640
MARKDOWN = str.maketrans("", "", "*_`#")


def plain_text(value):
    if not isinstance(value, str):
        return ""
    return " ".join(value.translate(MARKDOWN).split())


class Speech:
    engine = "kokoro-onnx"
    voice = "af_heart"

    def __init__(self, models):
        import numpy
        import onnxruntime
        from kokoro_onnx import Kokoro
        from scipy.signal import resample_poly

        options = onnxruntime.SessionOptions()
        options.intra_op_num_threads = min(8, len(os.sched_getaffinity(0)))
        options.add_session_config_entry("session.intra_op.allow_spinning", "0")
        session = onnxruntime.InferenceSession(
            str(models / "kokoro-v1.0.onnx"), sess_options=options,
            providers=["CPUExecutionProvider"],
        )
        self.kokoro = Kokoro.from_session(session, str(models / "voices-v1.0.bin"))
        self.numpy = numpy
        self.resample = resample_poly
        self.lock = threading.Lock()

    def synthesize(self, text):
        text = plain_text(text)
        if len(text) > MAX_TEXT or not any(c.isalnum() for c in text):
            raise ValueError("Expected short spoken text")
        with self.lock:
            wave, rate = self.kokoro.create(text, voice=self.voice, lang="en-us")
        if rate != SAMPLE_RATE:
            divisor = math.gcd(SAMPLE_RATE, rate)
            wave = self.resample(wave, SAMPLE_RATE // divisor, rate // divisor)
        if wave.ndim != 1 or not 0 < wave.size <= MAX_PCM // 2:
            raise ValueError("Invalid synthesized audio length")
        np = self.numpy
        peak = float(np.max(np.abs(wave)))
        if not math.isfinite(peak) or peak < 1e-5:
            raise ValueError("Synthesized audio is silent or invalid")
        target = 32767 * 10 ** (-1 / 20)
        return np.rint(wave * (target / peak)).astype("<i2").tobytes()
