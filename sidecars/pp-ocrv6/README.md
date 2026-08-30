# pp-ocrv6 sidecar

PP-OCRv6 det+rec (ONNX) behind FastAPI, serving the agentbob model pool's
**KindOCR** backend. It speaks the wire contract the bob-side rapidocr provider
already talks, so **bob needs zero code change** — it's a drop-in OCR provider
you point a `models.yaml` entry at.

Built to be the **resilient OCR workhorse**: onnxruntime-only (no paddlepaddle),
CPU-only, ~300MB, independent of the VLM/GPU box. If the VLM goes offline, this
keeps OCR (incl. ultrasound) alive.

## Contract (what `leaf/model/providers/36-rapidocr.go` speaks)

```
POST /ocr   multipart:
    image    (file, required)
    lang     (form, optional; accepted for parity, v6 model set is fixed)
    bbox     (form, optional "x1,y1,x2,y2" — server-side crop)
    enhance  (form, optional; "gray" = desaturate before OCR, see below)
  → {"lines": [{"bbox": [...], "text": "...", "conf": 0.9}, ...], "text": "joined\n..."}

GET /healthz → {"ok": true, "engine_loaded": bool, "engines_built": int,
                "engines_busy": int, "pool_size": int, "v6_models": bool}

503 from POST /ocr = every engine was busy for OCR_QUEUE_TIMEOUT — a BUSY
backend, not a broken one. bob classifies it as such and retries with backoff.
```

`enhance=gray` carries over the deep-color-screen treatment (coloured text on dark
medical/ultrasound backgrounds). bob passes it on the dyed force-OCR rounds; the
server desaturates the image to 3-channel grayscale before detection/recognition.

## Models — auto-downloaded at build

`fetch-models.sh` downloads the PP-OCRv6 **medium** det+rec ONNX and extracts the
rec charset, and the Dockerfile runs it during `docker build` — so a plain build
produces a ready image, no manual step. The files land at:

```
/models/det.onnx   # PaddlePaddle/PP-OCRv6_medium_det_onnx : inference.onnx
/models/rec.onnx   # PaddlePaddle/PP-OCRv6_medium_rec_onnx : inference.onnx
/models/keys.txt   # rec charset extracted from that repo's inference.yml
```

(Source: <https://huggingface.co/collections/PaddlePaddle/pp-ocrv6>. The build
needs network access to huggingface.co.)

**To change size** (latency vs accuracy): edit the three `medium` → `small`/`tiny`
URLs in `fetch-models.sh`. **To pin your own files**: drop `det.onnx` / `rec.onnx`
/ `keys.txt` into `./models/` before building — the script keeps anything already
present (idempotent). All three paths are env-overridable
(`DET_MODEL_PATH` / `REC_MODEL_PATH` / `REC_KEYS_PATH`).

## Build & wire (runs on the SIDECAR server, not with bob)

This sidecar lives on the separate sidecar/models box; bob reaches it over the LAN.
A standalone `docker-compose.yml` ships in this directory:

```sh
cd sidecars/pp-ocrv6            # on the sidecar server
docker compose up -d --build    # builds + downloads v6 models + runs on :11500
```

Then add a `models.yaml` provider (on the **bob** host) pointing at the sidecar's
LAN address — same shape as the existing rapidocr OCR entry: `provider: rapidocr`,
`kind: ocr`, `base_url: http://<sidecar-host>:11500`. Give it a higher `priority`
than the VLM OCR entry to make v6 **primary**; the pool fails over down the chain,
so the VLM entries stay as automatic fallbacks. Full steps +
verify checklist in **DEPLOY.md**.

## Verify on deploy (do NOT skip — untested-here paths)

1. `GET /healthz` → **`"v6_models": true`**. If `false`, the model files weren't
   found and it silently fell back to bundled v4 — fix the paths.
2. RapidOCR loads the v6 models without error on the first `/ocr` (watch the log
   for the engine-load line, not a stack trace). RapidOCR's pipeline (DB
   detection + CTC recognition) is expected to fit v6's det/rec heads, but this
   is the one thing to confirm live — if v6's model I/O doesn't match RapidOCR's
   pre/post-processing, switch the engine to a v6-native ONNX pipeline (e.g.
   `OnnxOCR`) keeping this same server/contract.
3. Run your real **ultrasound** images (with `enhance=gray`) and compare text +
   numbers against the VLM output. Confirm the deep-color readings are right and
   the on-screen UI noise is tolerable before promoting v6 above the VLM.

## Notes

- `OCR_ENGINE_POOL` (default 2): concurrent OCR calls = engine instances, one
  borrowed per call (`borrow_engine`). Each instance costs ~300MB, so this and
  `mem_limit` move together. It is NOT a throughput knob — onnxruntime already
  spreads a single request across the cores, and a 1200×800 screenshot takes
  ~8s either way; it exists so a second caller doesn't wait out the first in
  full, and so `/healthz` keeps answering during OCR. Set bob's matching
  `concurrency:` on the `kind: ocr` entry in `models.yaml` (the pool then also
  queues 2× that many waiters before reporting the queue full).
- `OCR_QUEUE_TIMEOUT` (default 30s): how long a call waits for a free engine
  before answering **503**. Longer than one OCR (~8s) so ordinary queueing still
  succeeds; bounded so a wedged engine surfaces as "busy" instead of parking
  callers forever (each waiter pins one of the 40 ASGI threadpool workers).
- `OCR_IDLE_EXIT_SECONDS` (image default **0** = engine resident): when set
  >0 the process self-exits after that many idle seconds and
  `restart: unless-stopped` brings it back on the next request. Turned off —
  that restart window is seconds of `502` from the fronting proxy,
  not worth the ~400MB of saved RSS.
- Runs as uid 1000, no home dir — same container hardening as bob itself.
