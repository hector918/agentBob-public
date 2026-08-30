"""PP-OCRv6 sidecar — serves the agentbob model pool's KindOCR backend.

The wire contract the bob-side rapidocr provider already speaks
(leaf/model/providers/36-rapidocr.go):

  POST /ocr  multipart: image (+ optional lang / bbox / enhance form fields)
             → {"lines": [{"bbox": [...], "text": "...", "conf": 0.9x}, ...], "text": "..."}
  GET  /healthz → 200 if the process is up.

The engine runs **PP-OCRv6** det+rec ONNX models
(https://huggingface.co/collections/PaddlePaddle/pp-ocrv6) via RapidOCR's ONNX
pipeline (the same DB-detection + CTC-recognition glue), instead of the bundled
PP-OCRv4. Model files are NOT vendored here — drop the v6 ONNX into ./models and
point the env vars at them (see README). Stays onnxruntime-only (no paddlepaddle),
so it's CPU-only and light (~300MB working set), independent of the VLM/GPU box —
which is the point: if the VLM goes offline, this keeps OCR alive.

The `enhance=gray` form field carries over the deep-color-screen treatment the
VLM path used (medical/ultrasound screens with coloured text on a dark
background): the image is desaturated before detection/recognition. Off by
default; bob passes it for the dyed force-OCR rounds.

Concurrency: the OCR pipeline is blocking CPU work, so /ocr does it in a worker
thread (run_in_threadpool) and OCR_ENGINE_POOL engine instances (default 2) cap
how many run at once — see ENGINE_POOL_SIZE / borrow_engine. Serving it inline
on the event loop, as this once did, meant one in-flight OCR froze
/healthz and every other request for its full ~8s.

Memory shape: ~80MB import baseline, ~300MB per engine instance (built lazily,
so a container that only ever sees one request at a time only ever builds one),
self-exit after IDLE_EXIT_SECONDS idle (docker restarts on demand; the shipped
compose disables it).
"""

from __future__ import annotations

import io
import logging
import os
import queue
import threading
import time
from contextlib import asynccontextmanager, contextmanager
from typing import Any, Optional

from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.concurrency import run_in_threadpool
from PIL import Image

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("ppocrv6-sidecar")

# Idle exit window. OCR_IDLE_EXIT_SECONDS=0 disables the self-exit (dev/debug).
IDLE_EXIT_SECONDS = int(os.environ.get("OCR_IDLE_EXIT_SECONDS", "300"))

# PP-OCRv6 model paths. The v6 ONNX det/rec models + the rec charset (keys) file
# are mounted/copied into the image; point these at them. Empty → RapidOCR falls
# back to its bundled (v4) models, which lets the container boot before the v6
# files are in place but is NOT what you want in production — check /healthz's
# "v6_models" flag.
DET_MODEL_PATH = os.environ.get("DET_MODEL_PATH", "")
REC_MODEL_PATH = os.environ.get("REC_MODEL_PATH", "")
REC_KEYS_PATH = os.environ.get("REC_KEYS_PATH", "")

# How many OCR calls may run at once = how many engine instances exist. Each
# instance is ~250-300MB of ONNX session + models, so this is the real memory
# knob (mem_limit must cover POOL × ~300MB + the ~80MB baseline).
#
# 2 (hector): measured on this box a 1200×800 screenshot takes ~8s,
# and ORT already spreads one request over the cores — a second worker is NOT
# there to double throughput (it barely does), it is there so a second caller
# stops waiting a full 8s behind the first, and so /healthz keeps answering
# while OCR runs.
ENGINE_POOL_SIZE = max(1, int(os.environ.get("OCR_ENGINE_POOL", "2")))

# How long a call waits for a free engine before giving up with 503. Must be
# comfortably longer than one OCR (~8s for a full screenshot here) so ordinary
# queueing still succeeds, and short enough that a WEDGED engine surfaces as a
# busy backend instead of parking callers forever: an unbounded wait pins a
# threadpool worker per caller, and the ASGI threadpool (40) is shared with
# every other route. 503 is the right answer because bob classifies it as
# "busy" (leaf/model/15-errors.go::isTransientBackendErr) and queues + retries
# with backoff rather than declaring OCR dead.
ENGINE_WAIT_SECONDS = float(os.environ.get("OCR_QUEUE_TIMEOUT", "30"))

# Idle engines waiting to be borrowed. Instances are created lazily (first N
# concurrent requests each build one) so an idle container keeps the ~80MB
# import baseline; once built they stay for the process lifetime.
_engine_pool: "queue.Queue[Any]" = queue.Queue()
_engine_lock = threading.Lock()
_engines_created = 0

_last_request_at = time.monotonic()
_last_request_lock = threading.Lock()


def _model_kwargs() -> dict:
    """RapidOCR model-path overrides for the v6 files that are actually present.
    Only set keys are passed, so a missing file degrades to the bundled model
    (and is surfaced via /healthz) instead of crashing the import."""
    kw: dict[str, str] = {}
    if DET_MODEL_PATH and os.path.exists(DET_MODEL_PATH):
        kw["det_model_path"] = DET_MODEL_PATH
    if REC_MODEL_PATH and os.path.exists(REC_MODEL_PATH):
        kw["rec_model_path"] = REC_MODEL_PATH
    if REC_KEYS_PATH and os.path.exists(REC_KEYS_PATH):
        kw["rec_keys_path"] = REC_KEYS_PATH
    return kw


def _new_engine():
    """Build one RapidOCR engine wired to the v6 models. Imports inside the
    function so module import stays at the ~80MB baseline; the ONNX runtime +
    models (~300MB per instance) load on first use."""
    from rapidocr_onnxruntime import RapidOCR  # deferred import

    kw = _model_kwargs()
    if not kw:
        log.warning(
            "PP-OCRv6 model paths unset/missing — falling back to RapidOCR's "
            "bundled (v4) models. Set DET_MODEL_PATH/REC_MODEL_PATH/REC_KEYS_PATH."
        )
    else:
        log.info("loading RapidOCR with PP-OCRv6 models: %s", kw)
    return RapidOCR(**kw)


@contextmanager
def borrow_engine():
    """Hand out one engine for the duration of a call, then return it to the
    pool. This IS the concurrency gate: at most ENGINE_POOL_SIZE calls hold an
    engine, and a caller past that waits here — up to ENGINE_WAIT_SECONDS, then
    503 (a busy backend, which the caller can retry) rather than waiting out a
    wedged engine forever.

    One instance per concurrent call rather than one shared instance: RapidOCR's
    wrapper makes no thread-safety promise (only onnxruntime's session.Run
    does), and an engine is cheap enough to own outright.
    """
    global _engines_created
    try:
        engine = _engine_pool.get_nowait()
    except queue.Empty:
        with _engine_lock:
            build = _engines_created < ENGINE_POOL_SIZE
            if build:
                _engines_created += 1
        if build:
            try:
                engine = _new_engine()
            except BaseException:
                with _engine_lock:
                    _engines_created -= 1
                raise
        else:
            # Pool is fully built and every engine is busy — wait for one.
            try:
                engine = _engine_pool.get(timeout=ENGINE_WAIT_SECONDS)
            except queue.Empty:
                log.warning(
                    "all %d engine(s) busy for %gs — refusing with 503",
                    ENGINE_POOL_SIZE,
                    ENGINE_WAIT_SECONDS,
                )
                raise HTTPException(
                    status_code=503,
                    detail=f"ocr busy: all {ENGINE_POOL_SIZE} engine(s) in use for "
                    f"{ENGINE_WAIT_SECONDS:g}s — retry shortly",
                ) from None
    try:
        yield engine
    finally:
        _engine_pool.put(engine)


def using_v6() -> bool:
    """True only when the FULL v6 set is present (det + rec + the rec charset).
    A partial config — e.g. a v6 rec model without its matching keys.txt — would
    silently decode against RapidOCR's default charset and emit garbage, so it
    must read as a red flag (healthz "v6_models": false), not a half-true."""
    return all(p and os.path.exists(p) for p in (DET_MODEL_PATH, REC_MODEL_PATH, REC_KEYS_PATH))


def touch_request() -> None:
    global _last_request_at
    with _last_request_lock:
        _last_request_at = time.monotonic()


def idle_watcher() -> None:
    """Background thread: hard-exit after IDLE_EXIT_SECONDS of no requests
    (docker restarts on demand); the shipped image/compose disable it."""
    if IDLE_EXIT_SECONDS <= 0:
        log.info("idle exit disabled (OCR_IDLE_EXIT_SECONDS=0)")
        return
    sleep_step = max(IDLE_EXIT_SECONDS // 4, 10)
    while True:
        time.sleep(sleep_step)
        with _last_request_lock:
            elapsed = time.monotonic() - _last_request_at
        if elapsed > IDLE_EXIT_SECONDS:
            log.info("idle for %ds (> %ds) — exiting", int(elapsed), IDLE_EXIT_SECONDS)
            os._exit(0)


@asynccontextmanager
async def lifespan(app: FastAPI):
    t = threading.Thread(target=idle_watcher, daemon=True, name="idle-watcher")
    t.start()
    log.info(
        "pp-ocrv6 sidecar started; idle_exit=%ds; port=%s; v6_models=%s; engine_pool=%d",
        IDLE_EXIT_SECONDS,
        os.environ.get("PORT", "11500"),
        using_v6(),
        ENGINE_POOL_SIZE,
    )
    yield


app = FastAPI(title="agentbob pp-ocrv6 sidecar", lifespan=lifespan)


@app.get("/healthz")
async def healthz() -> dict:
    """Liveness. v6_models=false means the v6 files weren't found and the engine
    will fall back to bundled v4 — a deploy red flag worth alerting on.
    engines_busy tells an operator whether the pool is the thing throttling.

    async on purpose: a sync `def` route runs in the SAME anyio threadpool the
    /ocr workers occupy, so once enough callers were queued on borrow_engine
    (the threadpool caps at 40) liveness would start blocking again — the exact
    lie we just removed. Everything here is non-blocking (a counter read under a
    lock that is never held across work, and a qsize), so the event loop is the
    right place for it."""
    with _engine_lock:
        built = _engines_created
    return {
        "ok": True,
        "engine_loaded": built > 0,
        "engines_built": built,
        "engines_busy": max(0, built - _engine_pool.qsize()),
        "pool_size": ENGINE_POOL_SIZE,
        "v6_models": using_v6(),
    }


def _maybe_crop(raw: bytes, bbox_str: Optional[str]) -> bytes:
    """Crop to [x1,y1,x2,y2] when bbox is given; passthrough otherwise."""
    if not bbox_str:
        return raw
    try:
        parts = [int(s.strip()) for s in bbox_str.split(",")]
    except ValueError:
        raise HTTPException(
            status_code=400,
            detail=f"bbox must be 4 comma-separated ints (got {bbox_str!r})",
        )
    if len(parts) != 4:
        raise HTTPException(
            status_code=400,
            detail=f"bbox must have exactly 4 ints (got {len(parts)})",
        )
    try:
        img = Image.open(io.BytesIO(raw))
        cropped = img.crop(tuple(parts))
        buf = io.BytesIO()
        cropped.save(buf, format=img.format or "PNG")
        return buf.getvalue()
    except HTTPException:
        raise
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"crop failed: {e}") from e


def _maybe_enhance(raw: bytes, enhance: str) -> bytes:
    """enhance=gray → desaturate to a 3-channel grayscale image, the deep-color
    screen treatment (coloured text on dark medical/ultrasound backgrounds reads
    far better once colour is removed). Unknown/empty enhance → passthrough."""
    mode = (enhance or "").strip().lower()
    if mode in ("", "none"):
        return raw
    if mode != "gray":
        raise HTTPException(status_code=400, detail=f"unknown enhance {enhance!r} (want 'gray')")
    try:
        img = Image.open(io.BytesIO(raw)).convert("L").convert("RGB")
        buf = io.BytesIO()
        img.save(buf, format="PNG")
        return buf.getvalue()
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"enhance failed: {e}") from e


@app.post("/ocr")
async def ocr(
    image: UploadFile = File(...),
    lang: str = Form("auto"),
    bbox: Optional[str] = Form(None),
    enhance: str = Form(""),
) -> dict:
    """Run PP-OCRv6 on the uploaded image → the {lines, text} envelope the
    bob-side rapidocr provider expects. lang is accepted for contract parity but
    the v6 model set is fixed (logged if a non-default lang is requested)."""
    raw = await image.read()
    if not raw:
        raise HTTPException(status_code=400, detail="empty image")

    # Everything below is blocking CPU work (PIL decode/crop + the ONNX
    # pipeline, ~8s for a full screenshot on this box), so it runs in a worker
    # thread. Doing it inline in this `async def` would sit ON the event loop:
    # measured, one in-flight OCR made /healthz take 7.6s instead of
    # its usual sub-second, and a second /ocr waited out the first in full
    # before it even started. Real parallelism is capped by borrow_engine, not
    # by the threadpool.
    return await run_in_threadpool(_run_ocr, raw, bbox, enhance, lang)


def _run_ocr(raw: bytes, bbox: Optional[str], enhance: str, lang: str) -> dict:
    """The blocking half of /ocr — always called on a worker thread."""
    raw = _maybe_crop(raw, bbox)
    raw = _maybe_enhance(raw, enhance)
    # touch AFTER input validation so malformed requests don't extend the idle window.
    touch_request()

    if lang and lang.lower() not in ("", "auto", "ch", "en"):
        log.info("lang=%s requested; v6 model set is fixed — proceeding with it", lang)

    try:
        with borrow_engine() as engine:
            result, _elapse = engine(raw)
    except HTTPException:
        raise  # borrow_engine's 503 (all engines busy) is not an engine fault
    except Exception as e:  # noqa: BLE001
        log.exception("ocr failed")
        raise HTTPException(status_code=500, detail=f"ocr failed: {e}") from e

    if not result:
        log.warning(
            "pp-ocrv6: engine returned empty for non-empty input — possible model "
            "misconfig (image_size=%d lang=%s v6=%s)",
            len(raw),
            lang,
            using_v6(),
        )

    lines = []
    joined: list[str] = []
    if result:
        for entry in result:
            bbox_pts, text, conf = entry
            lines.append({"bbox": bbox_pts, "text": text, "conf": conf})
            joined.append(text)
    return {"lines": lines, "text": "\n".join(joined)}


if __name__ == "__main__":
    import uvicorn

    port = int(os.environ.get("PORT", "11500"))
    uvicorn.run(app, host="0.0.0.0", port=port, log_level="info")
