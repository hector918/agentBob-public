"""Fun-ASR sidecar — serves the agentbob model pool's KindASR backend.

bob's voice-preprocess pass (flow/compose/30-asr.go ::PreASR) transcodes each
voice/audio attachment to canonical 16 kHz mono FLAC, then asks the pool's
KindASR entry to transcribe it. That entry is a plain OpenAI-compat provider
(no bespoke bob client) — the wire layer (leaf/model/providers/30-wire.go)
encodes the clip as an OpenAI `audio_url` data-URI content block. So this
sidecar is a thin OpenAI-compat shim that:

  POST /v1/chat/completions
    in : {messages:[{role,content:[{type:"audio_url",
                       audio_url:{url:"data:audio/flac;base64,..."}}, ...]}], ...}
    out: {choices:[{message:{role:"assistant",content:"<transcript>"}}], ...}

  GET  /v1/models   → stub list (the pool's Ping probes /models; a 2xx is nicer
                      than the 404 it tolerates)
  GET  /healthz     → liveness

Behind it sits FunASR's Fun-ASR-Nano-2512 (0.8B end-to-end ASR; zh + dialects,
en, ja) on CPU. The model is lazy-loaded on the first transcription and held
resident (this is a dedicated standalone box — no idle-exit dance like the
lightweight ocr sidecar).

Language: Fun-ASR-Nano takes an explicit `language` (中文/英文/日文; no documented
auto-detect) and bob does not forward one, so the shim defaults via ASR_LANGUAGE
(中文) and lets a request override it with a top-level "language" field. itn
(inverse text normalization → digits/punctuation) defaults on via ASR_ITN.
"""

from __future__ import annotations

import base64
import logging
import os
import tempfile
import threading
import time
from contextlib import asynccontextmanager
from typing import Any, Optional

from fastapi import FastAPI, HTTPException
from fastapi.responses import JSONResponse

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("fun-asr-sidecar")

# --- config (env-overridable) ------------------------------------------------

MODEL_ID = os.environ.get("ASR_MODEL", "FunAudioLLM/Fun-ASR-Nano-2512")
DEVICE = os.environ.get("ASR_DEVICE", "cpu")
# Fun-ASR-Nano accepts 中文 / 英文 / 日文 (no documented auto). Default to the
# deploy's primary language; a per-request "language" field overrides this.
DEFAULT_LANGUAGE = os.environ.get("ASR_LANGUAGE", "中文")
DEFAULT_ITN = os.environ.get("ASR_ITN", "true").strip().lower() in ("1", "true", "yes", "on")
PORT = int(os.environ.get("PORT", "11502"))

# --- model (lazy, single resident instance, serialized) ----------------------

_model: Any = None
_model_lock = threading.Lock()  # guards both load and inference (FunASR generate
#                                 over one CPU model instance — serialize calls)


def get_model():
    """Lazy-load and cache the FunASR AutoModel. Imported inside the function so
    `python server.py --help`-style probes and the /healthz endpoint don't pull
    torch + the 0.8B graph until the first real transcription."""
    global _model
    if _model is not None:
        return _model
    with _model_lock:
        if _model is not None:
            return _model
        from funasr import AutoModel  # deferred: torch + funasr import is heavy

        log.info("loading FunASR model=%s device=%s ...", MODEL_ID, DEVICE)
        t0 = time.monotonic()
        _model = AutoModel(
            model=MODEL_ID,
            hub="hf",
            trust_remote_code=True,
            device=DEVICE,
            disable_update=True,
        )
        log.info("model loaded in %.1fs", time.monotonic() - t0)
        return _model


def transcribe_file(path: str, language: str, itn: bool, hotwords: Optional[list]) -> str:
    """Run one offline transcription. Serialized via _model_lock so concurrent
    requests don't trample the single CPU model instance."""
    model = get_model()
    with _model_lock:
        res = model.generate(
            input=[path],
            cache={},
            batch_size=1,
            language=language,
            itn=itn,
            hotwords=hotwords or [],
        )
    if not res:
        return ""
    first = res[0]
    if isinstance(first, dict):
        return str(first.get("text", "")).strip()
    return str(first).strip()


# --- request parsing ---------------------------------------------------------


def _extract_audio_clips(messages: list) -> list[bytes]:
    """Pull every `audio_url` data-URI clip out of the messages (in order).
    bob sends one clip per call, but we handle N defensively (joined on return).
    Tolerates content as a plain string (no audio) or a content-block array."""
    clips: list[bytes] = []
    for m in messages or []:
        content = m.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "audio_url":
                continue
            url = ((block.get("audio_url") or {}).get("url") or "").strip()
            data = _decode_data_uri(url)
            if data is not None:
                clips.append(data)
    return clips


def _decode_data_uri(url: str) -> Optional[bytes]:
    """Decode a `data:<mime>;base64,<payload>` URI to raw bytes. Returns None for
    a non-data / non-base64 URL (the shim only accepts inlined audio)."""
    if not url.startswith("data:"):
        return None
    try:
        header, payload = url.split(",", 1)
    except ValueError:
        return None
    if "base64" not in header:
        return None
    try:
        return base64.b64decode(payload)
    except Exception:  # noqa: BLE001
        return None


def _suffix_for_data_uri(url: str) -> str:
    """Map the data-URI mime to a temp-file extension (cosmetic — FunASR sniffs
    the container; bob always sends FLAC)."""
    mime = url[5:].split(";", 1)[0].lower() if url.startswith("data:") else ""
    return {
        "audio/flac": ".flac",
        "audio/wav": ".wav",
        "audio/x-wav": ".wav",
        "audio/ogg": ".ogg",
        "audio/mpeg": ".mp3",
        "audio/mp3": ".mp3",
    }.get(mime, ".flac")


# --- app ---------------------------------------------------------------------


@asynccontextmanager
async def lifespan(app: FastAPI):
    log.info(
        "fun-asr sidecar started; model=%s device=%s language=%s itn=%s port=%d "
        "(model lazy-loads on first /v1/chat/completions)",
        MODEL_ID, DEVICE, DEFAULT_LANGUAGE, DEFAULT_ITN, PORT,
    )
    yield


app = FastAPI(title="agentbob fun-asr sidecar", lifespan=lifespan)


@app.get("/healthz")
def healthz() -> dict:
    """Liveness. model_loaded reflects whether the graph is resident yet."""
    return {"ok": True, "model": MODEL_ID, "model_loaded": _model is not None}


@app.get("/v1/models")
def list_models() -> dict:
    """Minimal OpenAI-compat /models so the pool's reachability Ping gets a 2xx."""
    return {
        "object": "list",
        "data": [{"id": MODEL_ID, "object": "model", "owned_by": "funasr"}],
    }


@app.post("/v1/chat/completions")
async def chat_completions(body: dict) -> JSONResponse:
    """OpenAI-compat transcription shim: decode the audio_url clip(s), run FunASR,
    return the transcript as the assistant message content."""
    messages = body.get("messages")
    if not isinstance(messages, list):
        raise HTTPException(status_code=400, detail="messages must be an array")

    clips = _extract_audio_clips(messages)
    if not clips:
        raise HTTPException(status_code=400, detail="no audio_url clip in messages")

    # Per-request overrides (bob doesn't send these today; they keep manual
    # curl / future callers flexible without a bob change).
    language = str(body.get("language") or DEFAULT_LANGUAGE)
    itn = bool(body.get("itn")) if "itn" in body else DEFAULT_ITN
    hotwords = body.get("hotwords") if isinstance(body.get("hotwords"), list) else None

    texts: list[str] = []
    for raw in clips:
        tmp = tempfile.NamedTemporaryFile(suffix=".flac", delete=False)
        try:
            tmp.write(raw)
            tmp.close()
            try:
                text = transcribe_file(tmp.name, language, itn, hotwords)
            except Exception as e:  # noqa: BLE001
                log.exception("transcription failed")
                raise HTTPException(status_code=500, detail=f"asr failed: {e}") from e
        finally:
            try:
                os.remove(tmp.name)
            except OSError:
                pass
        texts.append(text)

    transcript = "\n".join(t for t in texts if t)
    if not transcript:
        log.warning("empty transcript for %d clip(s) language=%s", len(clips), language)

    return JSONResponse(
        {
            "id": "chatcmpl-asr",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": MODEL_ID,
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": transcript},
                    "finish_reason": "stop",
                }
            ],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
        }
    )


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=PORT, log_level="info")
