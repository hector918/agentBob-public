"""MOSS-Transcribe-Diarize sidecar — serves the agentbob model pool's KindASR backend.

Drop-in for the fun-asr sidecar: it speaks the SAME wire contract, so switching bob
to it is a one-line `base_url` + `model` change in models.yaml and NO bob code change.

  POST /v1/chat/completions       (what bob actually calls)
    in : {messages:[{role,content:[{type:"audio_url",
                       audio_url:{url:"data:audio/flac;base64,..."}}, ...]}], ...}
    out: {choices:[{message:{role:"assistant",content:"<transcript>"}}], ...}

  POST /v1/audio/transcriptions   (the STANDARD OpenAI transcription endpoint,
                                   multipart file=@clip; not used by bob today — here
                                   so a future bob-side provider can move to it
                                   without this sidecar needing another revision)
  GET  /v1/models                 stub list (the pool's Ping wants a 2xx)
  GET  /healthz                   liveness

Behind it: moss-transcribe.cpp — a ggml C++17 port of OpenMOSS MOSS-Transcribe-Diarize
(Whisper-Medium encoder + Qwen3-0.6B decoder; transcription + speaker diarization +
timestamps in one pass, 50+ languages).

WHY THE ggml PORT AND NOT PyTorch. This box is an E5-2630L v4 — 2016 Broadwell, AVX2
only, no AVX-512, no AMX. The decoder is autoregressive, so on CPU it is memory-
bandwidth bound at ~3.6 GB/token in fp32. Measured RTF for the PyTorch path on
comparable hardware is ~1.0 (i.e. a 40-minute recording takes ~40 minutes); the ggml
port at q5_k is ~2-3x better AND the weights drop from 3.4 GB to 619 MB. The port
gates every component numerically against tensors dumped from the reference model
(cosine 1.0) and is byte-identical end-to-end through q5_0 — so the accuracy question
that usually kills a third-party port is already answered.

WHAT IT COSTS: the port is CLI-only (an HTTP API is on its roadmap), so this shim
shells out per request. The model is therefore loaded per call — ~600 MB off page
cache, well under a second warm, and irrelevant at this box's request volume.
"""

from __future__ import annotations

import base64
import logging
import os
import re
import shutil
import subprocess
import tempfile
import time
from contextlib import asynccontextmanager
from typing import Optional

from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import JSONResponse

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("moss-asr")

MODEL_ID = os.environ.get("MOSS_MODEL", "OpenMOSS-Team/MOSS-Transcribe-Diarize")
BINARY = os.environ.get("MOSS_BINARY", "/app/moss-transcribe")
# Several quantizations are baked in and CHOSEN AT RUNTIME, so trying q4_k against q5_k
# is an env change (or a per-request override, below) rather than a rebuild + a 600 MB
# re-download. MOSS_GGUF still wins if set — an explicit path beats the naming scheme.
MODELS_DIR = os.environ.get("MOSS_MODELS_DIR", "/models")
DEFAULT_QUANT = os.environ.get("MOSS_QUANT", "q5_k")
GGUF_OVERRIDE = os.environ.get("MOSS_GGUF", "")
DEFAULT_THREADS = os.environ.get("MTD_THREADS", "")
# Short clips get MORE threads than long ones — deliberately backwards from a fairness
# policy, and measured. A clip's cost is dominated by a FIXED encoder window (Whisper
# pads to 30 s), so a 4-second voice note pays ~14 s no matter what; and it pays it
# SYNCHRONOUSLY, with a human waiting on the turn to start. Long recordings run
# on-demand where nobody is watching, so they stay polite to the other services on this
# box. Measured on a 4.1 s clip at q5_k: 8 threads 16.0 s → 10 threads 14.1 s (-12%),
# and past the 10 physical cores it flattens (20 threads only reaches 13.3 s).
SHORT_SECONDS = float(os.environ.get("MOSS_SHORT_SECONDS", "15"))
SHORT_THREADS = os.environ.get("MTD_THREADS_SHORT", "10")
MAX_NEW = os.environ.get("MOSS_MAX_NEW", "8192")
# Wall-clock ceiling for one CLI run. Deliberately above every caller's own deadline —
# ingestion's transcribeTimeout (90 s, for clips capped at 30 s of audio) and the audio
# tool's per-call budget (90 s + 1.2x window, so ~450 s at its 300 s cap). Whoever asked
# should give up first: their timeout is a policy about how long a human waits, this one
# only stops a wedged subprocess pinning a worker forever.
RUN_TIMEOUT = int(os.environ.get("MOSS_RUN_TIMEOUT", "1800"))
SPEAKER_LABEL = os.environ.get("MOSS_SPEAKER_LABEL", "说话人")
PORT = int(os.environ.get("PORT", "11503"))

def available_quants() -> list:
    """The quantizations actually baked into this image, newest-first by name."""
    out = []
    try:
        for n in sorted(os.listdir(MODELS_DIR)):
            m = re.fullmatch(r"moss-transcribe-(.+)\.gguf", n)
            if m:
                out.append(m.group(1))
    except OSError:
        pass
    return out


def gguf_path(quant: Optional[str] = None) -> str:
    """Resolve a quant name to a baked GGUF path.

    Whitelisted against what is on disk rather than string-formatted blindly: the name
    can arrive from a request body, and a path built from unvalidated input is how a
    benchmarking convenience turns into an arbitrary-file-read.
    """
    if GGUF_OVERRIDE:
        return GGUF_OVERRIDE
    q = (quant or DEFAULT_QUANT).strip()
    avail = available_quants()
    if q not in avail:
        raise ValueError(f"unknown quant {q!r}; this image has {avail or 'none'}")
    return os.path.join(MODELS_DIR, f"moss-transcribe-{q}.gguf")


# MOSS emits `[start][Sxx]text[end]` per segment, e.g.
#   [0.48][S01]Welcome everyone[1.66][12.26][S02]The pipeline is ready[13.81]
# Parsed HERE from the CLI's default raw stream rather than from `--format json`:
# the bracket format is the MODEL's own output contract (documented identically by
# the upstream model card and the port), while the JSON schema is the port's and
# could shift under us. Fewer moving parts, and the fallbacks below mean a format
# surprise degrades to "plain text" instead of to "empty transcript".
SEG_RE = re.compile(r"\[(\d+(?:\.\d+)?)\]\[(S\d+)\]([^\[]*)\[(\d+(?:\.\d+)?)\]")
SEG_NOSPK_RE = re.compile(r"\[(\d+(?:\.\d+)?)\]([^\[]+)\[(\d+(?:\.\d+)?)\]")
BRACKET_RE = re.compile(r"\[[^\]]*\]")


def parse_transcript(raw: str) -> list:
    """Raw CLI stdout → segments [{start,end,speaker,text}], in timeline order.

    Three tiers, each a fallback for the one above, because losing a transcript to a
    formatting surprise is a far worse failure than losing the speaker labels:
      1. full `[t][Sxx]text[t]` segments;
      2. `[t]text[t]` when a build emits no speaker labels;
      3. everything with bracket groups stripped — no structure, but the words survive.
    Non-matching lines (CLI progress/log noise) are ignored by construction.
    """
    segs = []
    for m in SEG_RE.finditer(raw):
        text = m.group(3).strip()
        if text:
            segs.append({"start": float(m.group(1)), "end": float(m.group(4)), "speaker": m.group(2), "text": text})
    if segs:
        return segs
    for m in SEG_NOSPK_RE.finditer(raw):
        text = m.group(2).strip()
        if text:
            segs.append({"start": float(m.group(1)), "end": float(m.group(3)), "speaker": None, "text": text})
    if segs:
        return segs
    leftover = BRACKET_RE.sub(" ", raw).strip()
    if leftover:
        log.warning("transcript matched no segment pattern; falling back to bare text (%d chars)", len(leftover))
        return [{"start": None, "end": None, "speaker": None, "text": leftover}]
    return []


def render_plain(segments: list, speakers: list) -> str:
    """Segments → the plain string bob folds into the message text.

    Timestamps are dropped: bob's transcript is ordinary message text, and a wall of
    `[12.26]` markers would cost prompt budget in every downstream reader for
    something no reader uses.

    Speaker prefixes appear ONLY at 2+ speakers. Same rule bob applies to group
    speaker prefixes — with one voice there is nobody to disambiguate from, and the
    prefix would be pure noise on the single-speaker voice notes that are, today,
    100% of the real traffic. This is what makes the fun-asr → moss swap invisible
    for everything bob currently sees.
    """
    if not segments:
        return ""
    if len(speakers) < 2:
        return " ".join(s["text"] for s in segments).strip()
    order = {spk: i + 1 for i, spk in enumerate(speakers)}
    lines, cur, buf = [], None, []
    for s in segments:
        spk = s["speaker"]
        if spk != cur and buf:
            lines.append(f"{SPEAKER_LABEL}{order.get(cur, '?')}：" + " ".join(buf))
            buf = []
        cur = spk
        buf.append(s["text"])
    if buf:
        lines.append(f"{SPEAKER_LABEL}{order.get(cur, '?')}：" + " ".join(buf))
    return "\n".join(lines)


def probe_duration(path: str) -> Optional[float]:
    """Audio length in seconds, or None if it cannot be read.

    None is a legitimate answer, not an error: the only caller uses it to pick a thread
    count, and "unknown" simply means keep the default.
    """
    try:
        proc = subprocess.run(
            ["ffprobe", "-v", "error", "-show_entries", "format=duration",
             "-of", "default=noprint_wrappers=1:nokey=1", path],
            capture_output=True, timeout=30,
        )
        if proc.returncode != 0:
            return None
        return float(proc.stdout.decode().strip())
    except Exception:  # noqa: BLE001
        return None


def to_wav16k(src: str) -> str:
    """Transcode anything ffmpeg reads into 16 kHz mono WAV — what the CLI wants.

    NOT optional: bob's ingestion hands us FLAC (leaf/asr transcodes voice notes to
    canonical 16 kHz mono flac before the call), and the CLI takes WAV. Skipping this
    is a first-request failure, not a slow path.
    """
    dst = src + ".16k.wav"
    proc = subprocess.run(
        ["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-ar", "16000", "-ac", "1", dst],
        capture_output=True,
    )
    if proc.returncode != 0 or not os.path.exists(dst):
        raise RuntimeError("ffmpeg decode failed: " + (proc.stderr.decode(errors="replace").strip() or "no output"))
    return dst


def transcribe_file(path: str, quant: Optional[str] = None, threads: Optional[str] = None) -> dict:
    """Run one audio file through the CLI. Returns {raw, segments, text, speakers, ...}.

    quant / threads default to the container's env and exist so a tuning sweep can run
    the whole matrix WITHOUT a restart per combination. bob never sends them.
    """
    gguf = gguf_path(quant)
    wav = to_wav16k(path)
    dur = probe_duration(wav)
    th = threads
    if not th:
        th = SHORT_THREADS if (dur is not None and dur < SHORT_SECONDS) else DEFAULT_THREADS
    env = dict(os.environ)
    if th:
        env["MTD_THREADS"] = str(th)
    try:
        t0 = time.time()
        proc = subprocess.run(
            [BINARY, "transcribe", gguf, wav, "--max-new", MAX_NEW],
            capture_output=True,
            timeout=RUN_TIMEOUT,
            env=env,
        )
        dt = time.time() - t0
    except subprocess.TimeoutExpired as e:
        raise RuntimeError(f"transcription exceeded {RUN_TIMEOUT}s") from e
    finally:
        try:
            os.remove(wav)
        except OSError:
            pass
    if proc.returncode != 0:
        err = proc.stderr.decode(errors="replace").strip() or proc.stdout.decode(errors="replace").strip()
        raise RuntimeError(f"moss-transcribe exited {proc.returncode}: {err[:500]}")
    raw = proc.stdout.decode(errors="replace")
    segments = parse_transcript(raw)
    speakers = sorted({s["speaker"] for s in segments if s["speaker"]})
    log.info("transcribed %.1fs of audio in %.1fs — quant=%s threads=%s, %d segment(s), %d speaker(s)",
             dur or -1, dt, os.path.basename(gguf), th or "default", len(segments), len(speakers))
    return {
        "raw": raw.strip(),
        "segments": segments,
        "text": render_plain(segments, speakers),
        "speakers": speakers,
        "quant": os.path.basename(gguf),
        "threads": th or "default",
        "audio_s": round(dur, 2) if dur is not None else None,
        "elapsed_s": round(dt, 2),
    }


def _extract_audio_clips(messages: list) -> list:
    """Pull every `audio_url` data-URI clip out of the messages, in order.

    Same extraction the fun-asr shim does — bob's wire layer
    (leaf/model/providers/30-wire.go) is what produces this shape, and both sidecars
    must read it identically or a swap changes behaviour silently.
    """
    clips = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        content = m.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "audio_url":
                continue
            url = ((block.get("audio_url") or {}).get("url") or "").strip()
            raw = _decode_data_uri(url)
            if raw:
                clips.append(raw)
    return clips


def _decode_data_uri(url: str) -> Optional[bytes]:
    if not url.startswith("data:"):
        return None
    _, _, payload = url.partition(",")
    if not payload:
        return None
    try:
        return base64.b64decode(payload)
    except Exception:  # noqa: BLE001
        log.warning("undecodable data-URI payload (%d chars)", len(payload))
        return None


def _run_bytes(raw: bytes, suffix: str, quant=None, threads=None) -> dict:
    tmp = tempfile.NamedTemporaryFile(suffix=suffix, delete=False)
    try:
        tmp.write(raw)
        tmp.close()
        return transcribe_file(tmp.name, quant, threads)
    finally:
        try:
            os.remove(tmp.name)
        except OSError:
            pass


@asynccontextmanager
async def lifespan(app: FastAPI):
    # Fail loudly at STARTUP if the binary or weights are missing, rather than on the
    # first voice message as a 500 that names neither.
    if not os.path.exists(BINARY):
        log.error("missing binary at %s — transcription will fail", BINARY)
    if not available_quants() and not GGUF_OVERRIDE:
        log.error("no GGUF found under %s — transcription will fail", MODELS_DIR)
    if not shutil.which("ffmpeg"):
        log.error("ffmpeg not on PATH — every request will fail at decode")
    log.info("moss-asr up on :%d binary=%s quants=%s default=%s threads=%s",
             PORT, BINARY, available_quants(), DEFAULT_QUANT, DEFAULT_THREADS or "default")
    _prewarm()
    yield


def _prewarm() -> None:
    """Pull EVERY baked GGUF into the page cache at startup.

    The port is CLI-only, so the model is (re)loaded per request — but ggml mmaps the
    file, so once its pages are cached the "load" is a page-table walk, not disk I/O.
    On a 32 GB box a 619 MB file simply stays there, which makes the per-call reload a
    non-issue without needing a resident process at all. (A genuinely in-process model
    would need the port's flat C-API, which is still on its roadmap.)

    ALL of them, not just the default: a tuning sweep that compares quants would
    otherwise charge the first run of each one a cold-cache penalty and report it as
    that quant being slower. On a 32 GB box the whole set is ~1 GB and simply stays.

    Best-effort by design: a failure here costs one slow first request, nothing more.
    """
    paths = [GGUF_OVERRIDE] if GGUF_OVERRIDE else [
        os.path.join(MODELS_DIR, f"moss-transcribe-{q}.gguf") for q in available_quants()
    ]
    total, t0 = 0, time.time()
    for path in paths:
        if not os.path.exists(path):
            continue
        try:
            with open(path, "rb", buffering=0) as f:
                while True:
                    chunk = f.read(8 << 20)
                    if not chunk:
                        break
                    total += len(chunk)
        except OSError as e:
            log.warning("prewarm skipped %s: %s", os.path.basename(path), e)
    if total:
        log.info("prewarmed %d MB across %d quant(s) in %.1fs", total >> 20, len(paths), time.time() - t0)


app = FastAPI(lifespan=lifespan)


@app.get("/healthz")
def healthz() -> dict:
    quants = available_quants()
    return {
        "ok": True,
        "model": MODEL_ID,
        "binary": os.path.exists(BINARY),
        "gguf": bool(quants) or bool(GGUF_OVERRIDE),
        "ffmpeg": bool(shutil.which("ffmpeg")),
        "quants": quants,
        "default_quant": DEFAULT_QUANT,
        "threads": DEFAULT_THREADS or "default",
        "short_threads": SHORT_THREADS,
        "short_seconds": SHORT_SECONDS,
    }


@app.get("/v1/models")
def list_models() -> dict:
    return {"object": "list", "data": [{"id": MODEL_ID, "object": "model", "owned_by": "openmoss"}]}


@app.post("/v1/chat/completions")
async def chat_completions(body: dict) -> JSONResponse:
    """The endpoint bob calls. Identical envelope to the fun-asr shim."""
    messages = body.get("messages")
    if not isinstance(messages, list):
        raise HTTPException(status_code=400, detail="messages must be an array")
    clips = _extract_audio_clips(messages)
    if not clips:
        raise HTTPException(status_code=400, detail="no audio_url clip in messages")

    verbose = body.get("response_format") == "verbose_json"
    # Per-request tuning overrides. bob never sends these; they exist so a quant/thread
    # sweep runs as one script instead of one container restart per combination.
    tune = body.get("moss") if isinstance(body.get("moss"), dict) else {}
    quant, threads = tune.get("quant"), tune.get("threads")

    outs = []
    for raw in clips:
        try:
            outs.append(_run_bytes(raw, ".flac", quant, threads))
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e)) from e
        except Exception as e:  # noqa: BLE001
            log.exception("transcription failed")
            raise HTTPException(status_code=500, detail=f"asr failed: {e}") from e

    transcript = "\n".join(o["text"] for o in outs if o["text"])
    if not transcript:
        log.warning("empty transcript for %d clip(s)", len(clips))

    payload = {
        "id": "chatcmpl-asr",
        "object": "chat.completion",
        "created": int(time.time()),
        "model": MODEL_ID,
        "choices": [{"index": 0, "message": {"role": "assistant", "content": transcript}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
    }
    if verbose:
        payload["moss"] = [
            {k: o[k] for k in ("raw", "segments", "speakers", "quant", "threads", "audio_s", "elapsed_s")}
            for o in outs
        ]
    return JSONResponse(payload)


@app.post("/v1/audio/transcriptions")
async def transcriptions(
    file: UploadFile = File(...),
    model: str = Form(default=MODEL_ID),
    response_format: str = Form(default="json"),
    temperature: str = Form(default="0"),
) -> JSONResponse:
    """The standard OpenAI transcription endpoint — not used by bob today.

    Here so a future bob-side provider can speak the STANDARD endpoint (and then any
    ASR serving it drops in with a base_url change) without another sidecar revision.
    """
    raw = await file.read()
    if not raw:
        raise HTTPException(status_code=400, detail="empty file")
    suffix = os.path.splitext(file.filename or "")[1] or ".wav"
    try:
        out = _run_bytes(raw, suffix)
    except Exception as e:  # noqa: BLE001
        log.exception("transcription failed")
        raise HTTPException(status_code=500, detail=f"asr failed: {e}") from e
    if response_format == "verbose_json":
        return JSONResponse({"text": out["text"], "raw": out["raw"], "segments": out["segments"], "speakers": out["speakers"]})
    return JSONResponse({"text": out["text"]})


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=PORT, log_level="info")
