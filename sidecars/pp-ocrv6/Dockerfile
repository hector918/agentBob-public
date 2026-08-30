# agentbob pp-ocrv6 sidecar — PP-OCRv6 det+rec (ONNX) behind FastAPI on port
# 11500, via RapidOCR's ONNX pipeline. Serves the model pool's KindOCR backend
# with the /ocr contract the bob-side rapidocr provider speaks (bob needs zero
# change to point at it).
#
# Model files are NOT vendored. Drop the PP-OCRv6 ONNX into ./models before build
# (see README.md) — the COPY below bakes them in; the env vars point the server
# at them. Stays onnxruntime-only (no paddlepaddle) → CPU-only, ~300MB working
# set, independent of the VLM/GPU box.
#
# Image size after slim: ~280-320MB + the v6 model files (tens of MB).

FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

ENV HOME=/tmp

# Runtime libs RapidOCR's ONNX backend + opencv-python-headless need; wget for
# the build-time model fetch.
RUN apt-get update && apt-get install -y --no-install-recommends \
        libgl1 \
        libglib2.0-0 \
        wget \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install -r requirements.txt

# PP-OCRv6 models. fetch-models.sh downloads the medium det+rec ONNX and extracts
# the rec charset → /models at build (needs network). It's idempotent: anything
# pre-placed in ./models/ (copied first) is kept, so you can override with a
# different size by dropping your own det.onnx/rec.onnx/keys.txt before building.
COPY models/ /models/
COPY fetch-models.sh .
RUN sh fetch-models.sh /models
ENV DET_MODEL_PATH=/models/det.onnx \
    REC_MODEL_PATH=/models/rec.onnx \
    REC_KEYS_PATH=/models/keys.txt

COPY server.py .

RUN groupadd --gid 1000 ocr \
    && useradd --uid 1000 --gid 1000 --no-create-home ocr
USER ocr

EXPOSE 11500

# Runtime defaults, chosen so a bare `docker run` of this image behaves like the
# shipped compose:
#   * no idle self-exit — the restart window it bought was seconds of 502 from
#     whatever proxy fronts this, not worth the ~400MB of saved RSS
#   * 2 engine instances = 2 concurrent OCR calls (~300MB each); match bob's
#     `concurrency:` on the kind:ocr entry
#   * a caller waits 30s for a free engine, then gets 503 (busy → retryable)
ENV OCR_IDLE_EXIT_SECONDS=0 \
    OCR_ENGINE_POOL=2 \
    OCR_QUEUE_TIMEOUT=30

CMD ["python", "server.py"]
