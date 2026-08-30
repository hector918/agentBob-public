# agentbob moss-asr sidecar — OpenMOSS MOSS-Transcribe-Diarize via the ggml C++ port
# (moss-transcribe.cpp), behind an OpenAI-compat FastAPI shim on port 11503. Serves the
# model pool's KindASR backend; bob needs ZERO code change (a models.yaml entry points
# an openai-compat provider at this host's /v1 — the same contract the fun-asr sidecar
# it replaces already speaks).
#
# CPU-first by design. The target box is an E5-2630L v4 (2016 Broadwell, AVX2 only, no
# AVX-512, no AMX), where the reference PyTorch path is memory-bandwidth bound at ~3.6
# GB per generated token in fp32 — measured RTF ≈ 1.0, i.e. a 40-minute recording takes
# 40 minutes. The ggml port at q5_k is 2-3x better and drops the weights from 3.4 GB to
# 619 MB, while staying BYTE-IDENTICAL to the reference through q5_0.
#
# Two stages: build the C++ binary, then ship it next to the weights on a slim runtime
# with no compiler, no torch, no transformers, no HF cache.

# ---------- stage 1: build the ggml CLI ----------
FROM debian:bookworm-slim AS build

# Pin the source. An unpinned clone makes the image irreproducible, and this port is a
# young repo (bus factor 1) — bump deliberately, after re-running the verify checklist.
ARG MT_REF=master

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential cmake git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
# --recursive: ggml is vendored as a submodule at third_party/ggml.
RUN git clone --recursive https://github.com/mudler/moss-transcribe.cpp . \
    && git checkout ${MT_REF} \
    && git submodule update --init --recursive
# BUILD_SHARED_LIBS=OFF links ggml INTO the binary. Shared by default, it produced a
# libggml.so.0 that lived only in this stage — the runtime image then had a binary that
# died with "error while loading shared libraries" on its first request.
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release -DMT_BUILD_CLI=ON -DBUILD_SHARED_LIBS=OFF \
    && cmake --build build -j "$(nproc)"

# Stage the binary plus any .so the build still emitted, so the runtime copy is one
# known path whether the static link took or not (with a real static build lib/ is
# simply empty).
RUN mkdir -p /out/lib \
    && cp build/moss-transcribe /out/ \
    && find build -name '*.so*' -exec cp -a {} /out/lib/ \; || true

# ---------- stage 2: runtime ----------
FROM python:3.12-slim

# Bake MORE THAN ONE quantization and pick at RUNTIME (MOSS_QUANT env, or a per-request
# override). Comparing q4_k against q5_k then costs a container restart at most, instead
# of a rebuild plus a 600 MB re-download — which is the difference between "we measured
# it" and "we assumed it".
#
# q5_k is upstream's recommendation: byte-identical to the reference at ~1/6 the size.
# q4_k is ~15% faster and only WORD-identical (timestamps drift 0.02-0.07 s).
ARG MOSS_QUANTS="q5_k q4_k"

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HOME=/tmp

# ffmpeg is NOT optional: bob hands us canonical 16 kHz mono FLAC and the CLI takes
# WAV, so every request transcodes. curl fetches the weights below. libgomp1 is ggml's
# OpenMP runtime — absent from the slim image, and its absence looks exactly like the
# libggml miss did: a 127 exit on the first request, naming a library nobody expected.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg curl ca-certificates libgomp1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install -r requirements.txt

COPY --from=build /out/ /app/
ENV LD_LIBRARY_PATH=/app/lib

# Assertion 1 — libraries resolve, checked in the RUNTIME image, after the copy.
#
# The first cut of this file checked the binary in the BUILDER stage, where ggml's .so
# was still on disk, so it passed — and the miss surfaced on the first voice message as
# `exited 127: libggml.so.0: cannot open shared object file`. A build-time check that
# runs in the wrong stage is not a build-time check. ldd here resolves against THIS
# image's libraries, which is the only question that matters.
#
# It runs BEFORE the 620 MB download on purpose: a binary the loader cannot satisfy
# should not cost a weights fetch to discover.
#
# Note kept OUT of the RUN because a `#` line inside a continued instruction is handled
# inconsistently across builders and can swallow the command after it: ldd exits 1 on a
# fully static binary ("not a dynamic executable"), which is a PASS — its status is
# ignored on purpose and only its TEXT is judged. Nothing is EXECUTED here: this CLI
# exits 2 on any non-subcommand invocation (`usage: moss-transcribe <subcommand>`), so
# a bare run proves nothing that ldd hasn't already; assertion 2 does the real thing.
RUN set -e; \
    ldd /app/moss-transcribe > /tmp/ldd.txt 2>&1 || true; cat /tmp/ldd.txt; \
    if grep -q "not found" /tmp/ldd.txt; then \
        echo "FATAL: unresolved shared libraries listed above"; exit 1; \
    fi; \
    echo "shared libraries all resolve"

# Pre-converted GGUF straight from the port's own HF repo — no python conversion step,
# no HF client in the image, no sidecar metadata files (the converter embeds dims, mel
# filterbank and tokenizer into the GGUF itself).
RUN mkdir -p /models \
    && for q in ${MOSS_QUANTS}; do \
         echo "fetching ${q}"; \
         curl -fL --retry 3 -o "/models/moss-transcribe-${q}.gguf" \
           "https://huggingface.co/mudler/moss-transcribe.cpp-gguf/resolve/main/moss-transcribe-${q}.gguf"; \
       done \
    && ls -l /models/

# Assertion 2 — the binary actually loads THESE weights. `info` is the CLI's own
# metadata subcommand: it parses the GGUF end to end without needing audio, so this one
# check covers everything a first request would otherwise discover the hard way — a
# truncated or HTML-error download, a quant filename that 404'd into a 9-byte file, a
# GGUF the binary's version cannot read. Cheap, and it is the difference between a
# broken image failing at `docker compose build` and failing at the first voice message.
RUN set -e; \
    for f in /models/*.gguf; do \
        echo "== info $f"; \
        /app/moss-transcribe info "$f"; \
    done; \
    echo "binary loads every baked GGUF OK"

COPY server.py .

RUN groupadd --gid 1000 asr \
    && useradd --uid 1000 --gid 1000 --no-create-home asr
USER asr

EXPOSE 11503

ENV MOSS_MODEL=OpenMOSS-Team/MOSS-Transcribe-Diarize \
    MOSS_BINARY=/app/moss-transcribe \
    MOSS_MODELS_DIR=/models \
    MOSS_QUANT=q5_k \
    MOSS_MAX_NEW=8192 \
    MOSS_RUN_TIMEOUT=1800 \
    MTD_THREADS=8 \
    PORT=11503

CMD ["python", "server.py"]
