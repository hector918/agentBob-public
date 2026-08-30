# agentbob fun-asr sidecar — FunASR Fun-ASR-Nano-2512 (0.8B end-to-end ASR)
# behind an OpenAI-compat FastAPI shim on port 11502. Serves the model pool's
# KindASR backend; bob needs zero code change (a models.yaml entry points an
# openai-compat provider at this host's /v1).
#
# CPU-only (torch from the CPU wheel index — no CUDA). The 0.8B weights are
# BAKED into the HF cache at build time so runtime is fully offline + the model
# id resolves exactly as the model card documents (model + hub="hf"). Build
# needs network; the cache + remote code are then frozen with HF_HUB_OFFLINE=1.
#
# Image is fat (torch + 0.8B weights ≈ a few GB) — this runs on its own box,
# NOT in bob's compose. See README.md.

FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HOME=/tmp \
    HF_HOME=/models/hf

# ffmpeg/libsndfile for audio decode (soundfile/torchaudio backends); git for
# any source-built FunASR deps.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        libsndfile1 \
        git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# CPU torch first so funasr's dep resolution finds it satisfied (and never pulls
# the CUDA build).
RUN pip install torch==2.5.1 torchaudio==2.5.1 --index-url https://download.pytorch.org/whl/cpu

COPY requirements.txt .
RUN pip install -r requirements.txt

# Fail the BUILD, not the first voice message. funasr's HuggingfaceTokenizer
# hides an unimportable transformers behind a bare `except: pass` and only trips
# over it at inference time, as an UnboundLocalError that names neither the
# module nor the real cause. Assert the HF stack imports while we can still see
# the error.
# Both failures this guards against were silent at build time and only surfaced
# on the first voice message, as errors naming neither the module nor the cause:
# a missing transformers became `UnboundLocalError: AutoTokenizer` (funasr's
# HuggingfaceTokenizer hides the ImportError behind a bare `except: pass`), and
# an unregistered model class became `FunASRNano is not registered` (funasr's
# package init swallows every submodule ImportError the same way). Assert both
# here, where the error is still visible.
# The FunASRNano assertion imports ONLY `funasr` on purpose — registration has to
# happen through funasr's own package init. Importing the submodule by hand here
# would paper over exactly the regression this guards (funasr <1.4.0 could not
# self-register the class at all).
RUN python -c "import torch, transformers; from transformers import AutoTokenizer; \
    import funasr; from funasr.register import tables; \
    assert 'FunASRNano' in tables.model_classes, 'FunASRNano failed to register'; \
    print('torch', torch.__version__, '| transformers', transformers.__version__, \
          '| funasr', funasr.__version__, '| FunASRNano registered')"

# Bake the model into the HF cache (needs network). Includes the custom model
# code that trust_remote_code loads — so runtime stays offline.
RUN huggingface-cli download FunAudioLLM/Fun-ASR-Nano-2512 >/dev/null

# Freeze offline AFTER the download so the runtime never reaches out.
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1

COPY server.py .

# /models (the HF cache) is root-owned + world-readable; run unprivileged.
RUN groupadd --gid 1000 asr \
    && useradd --uid 1000 --gid 1000 --no-create-home asr
USER asr

EXPOSE 11502

ENV ASR_MODEL=FunAudioLLM/Fun-ASR-Nano-2512 \
    ASR_DEVICE=cpu \
    ASR_LANGUAGE=中文 \
    ASR_ITN=true \
    PORT=11502

CMD ["python", "server.py"]
