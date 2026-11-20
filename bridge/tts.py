import math
import os
import re
import sys
import threading
from array import array
from functools import lru_cache

SAMPLE_RATE = 16000
MAX_PCM = 2 * 1024 * 1024
MAX_TEXT = 640
MARKDOWN = str.maketrans("", "", "*_`#")
SENTENCE_END = re.compile(r"""[.!?]+["'”’)\]]*(?=\s|$)""")
ABBREVIATION = re.compile(r"\b(?:Mr|Mrs|Ms|Dr|Prof|St|vs|etc|e\.g|i\.e)\.$", re.IGNORECASE)
INITIALS = re.compile(r"\b(?:[A-Z]\.)+$")


def plain_text(value):
    return " ".join(value.translate(MARKDOWN).split()) if isinstance(value, str) else ""


def split_sentences(text: str) -> list[str]:
    text = plain_text(text)
    sentences = []
    clip_start = sentence_start = 0
    for end in SENTENCE_END.finditer(text):
        sentence = text[sentence_start:end.end()].strip()
        if end.group().startswith(".") and (ABBREVIATION.search(sentence) or INITIALS.search(sentence)):
            continue
        sentence_start = end.end()
        # Count letters so "Ooh, awesome question!" stays with the explanation.
        if end.group().startswith(("!", "?")) and sum(c.isalnum() for c in sentence) < 20:
            continue
        sentences.append(text[clip_start:sentence_start].strip())
        clip_start = sentence_start
    if tail := text[clip_start:].strip():
        sentences.append(tail)
    return sentences


def pcm_bytes(samples):
    if sys.byteorder != "little":
        samples.byteswap()
    return samples.tobytes()


@lru_cache(maxsize=1)
def make_intercom_chime_pcm() -> bytes:
    samples = array("h")
    length = SAMPLE_RATE // 10
    fade = SAMPLE_RATE // 80
    for tone in (660, 880):
        if samples:
            samples.extend([0] * (SAMPLE_RATE // 50))
        for i in range(length):
            envelope = min(1, i / fade, (length - 1 - i) / fade)
            samples.append(round(4600 * envelope * math.sin(math.tau * tone * i / SAMPLE_RATE)))
    return pcm_bytes(samples)


def normalize_voice_pcm(raw_pcm: bytes) -> bytes:
    if not raw_pcm:
        return raw_pcm
    if len(raw_pcm) % 2:
        raise ValueError("PCM requires whole 16-bit samples")
    samples = array("h", raw_pcm)
    if sys.byteorder != "little":
        samples.byteswap()
    peak = max(map(abs, samples))
    rms = math.sqrt(sum(value * value for value in samples) / len(samples))
    if peak < 1100 or rms < 400:
        return raw_pcm
    gain = min(3, 32767 * 10 ** (-2 / 20) / peak)
    return pcm_bytes(array("h", (round(value * gain) for value in samples)))


@lru_cache(maxsize=32)
def short_speech(speech, text: str) -> bytes:
    return speech.synthesize_pcm(text)


class Speech:
    engine = "kokoro-onnx"
    voice = "af_heart"

    def __init__(self, models):
        import numpy as np
        import onnxruntime as ort
        from kokoro_onnx import Kokoro
        from onnxruntime.capi import onnxruntime_pybind11_state as errors
        from scipy.signal import resample_poly

        self.errors = (errors.Fail, errors.RuntimeException, errors.InvalidArgument, errors.EPFail, ValueError, OSError)
        options = ort.SessionOptions()
        options.intra_op_num_threads = min(8, len(os.sched_getaffinity(0)))
        options.add_session_config_entry("session.intra_op.allow_spinning", "0")
        try:
            session = ort.InferenceSession(str(models / "kokoro-v1.0.onnx"), sess_options=options,
                                           providers=["CPUExecutionProvider"])
            self.kokoro = Kokoro.from_session(session, str(models / "voices-v1.0.bin"))
        except (*self.errors, errors.NoSuchFile, errors.InvalidGraph, errors.InvalidProtobuf, errors.NoModel) as error:
            raise RuntimeError(f"Kokoro load: {type(error).__name__}") from None
        self.np, self.resample, self.lock = np, resample_poly, threading.Lock()

    @staticmethod
    def spoken_text(text):
        text = plain_text(text)
        if len(text) > MAX_TEXT or not any(c.isalnum() for c in text):
            raise ValueError("Expected short spoken text")
        return text

    def synthesize_pcm(self, text: str) -> bytes:
        return self._pcm(self.spoken_text(text))

    def synthesize_sentences(self, text: str):
        sentences = split_sentences(self.spoken_text(text))
        for i, sentence in enumerate(sentences):
            # Restore the inter-sentence pause trimmed by Kokoro.
            pause = bytes(SAMPLE_RATE // 5) if i + 1 < len(sentences) else b""
            yield self._pcm(sentence) + pause

    def _pcm(self, text):
        try:
            with self.lock:
                wave, rate = self.kokoro.create(text, voice=self.voice, lang="en-us")
        except self.errors as error:
            raise RuntimeError(f"Kokoro inference: {type(error).__name__}") from None
        if rate != SAMPLE_RATE:
            divisor = math.gcd(SAMPLE_RATE, rate)
            wave = self.resample(wave, SAMPLE_RATE // divisor, rate // divisor)
        if wave.ndim != 1 or not 0 < wave.size <= MAX_PCM // 2:
            raise ValueError("Invalid synthesized audio length")
        np, scaled = self.np, wave * 1.8
        magnitude = np.abs(scaled)
        peak = float(np.max(magnitude))
        if not math.isfinite(peak) or peak < 1e-5:
            raise ValueError("Synthesized audio is silent or invalid")
        # Fixed voice gain preserves relative loudness. A soft knee limits peaks to -1 dBFS.
        knee, headroom = 0.75, 10 ** (-1 / 20) - 0.75
        hot = magnitude > knee
        magnitude[hot] = knee + headroom * np.tanh((magnitude[hot] - knee) / headroom)
        return np.rint(np.copysign(magnitude, scaled) * 32767).astype("<i2").tobytes()
