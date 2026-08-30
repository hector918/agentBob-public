# syntax=docker/dockerfile:1.7
#
# agentbob — multi-stage build.
#
# Stage 1: golang:alpine builds the static binary (CGO_ENABLED=0 thanks to
#          modernc.org/sqlite — no libsqlite3 needed).
# Stage 2: alpine runtime with python3 (for the future execute_code tool),
#          ca-certificates (HTTPS to OpenAI / Anthropic / etc.), and tini
#          (so SIGTERM cleanly stops `bob gateway` instead of being swallowed
#          by a default PID-1 shell).
#
# Runs as a non-root user. State lives at /data/.bob (mount it from the host
# to persist sessions / memory / skills / logs).
#
# Default entrypoint: `bob gateway`. Override CMD to run `bob doctor`,
# `bob model list`, etc.

# ---- build stage --------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# BOB_VERSION is stamped into the binary (main.version) so the running build is
# identifiable ($BOB_HOME/VERSION + the startup log). The deploy passes the host's
# git short SHA: `docker build --build-arg BOB_VERSION=$(git rev-parse --short HEAD)`
# (or set it in docker-compose). Defaults to "dev" when unset.
ARG BOB_VERSION=dev

# Build. CGO off → static binary; -s -w trims the binary; -trimpath removes
# build-host paths from stack traces. vendor/ is checked in, so the build
# runs in -mod=vendor mode (Go's default when vendor/ exists) — fully
# offline, no module-download layer or proxy reachability needed. The main
# package is cmd/bob.
COPY . .
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${BOB_VERSION}" \
        -o /out/bob ./cmd/bob

# ---- runtime stage ------------------------------------------------------
#
# Switched alpine → debian-slim. Reason: Scrapling 0.2.x+
# transitively requires `playwright>=1.49`, and playwright doesn't ship
# wheels for alpine-arm64 (musl + arm64) — pip silently downgrades to
# scrapling 0.1.2 which has no `extract` CLI, breaking the tool. Debian
# (glibc) has full arm64 wheel coverage. Image grows ~80 MB but the
# scrapling tool actually works.
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
# Tell playwright (and scrapling, which uses it) where to put the
# browser binaries — system path so the non-root `bob` user at runtime
# can read what was installed by root at build time. Without this,
# `playwright install` defaults to ~/.cache/ms-playwright, which gets
# written to /root/ during build and is unreadable by bob at runtime.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        python3 \
        python3-pip \
        tini \
        # ffmpeg — the gateway transcodes inbound audio to a canonical
        # format before sending it to the ASR model (voice transcription).
        ffmpeg \
        # Build deps for native Python wheels we install via pip
        # (curl_cffi / lxml / msgspec). Removed at the end of the
        # RUN step to keep the image lean.
        gcc \
        g++ \
        libffi-dev \
        libssl-dev \
        libxml2-dev \
        libxslt1-dev \
        # Slice 5 (browser_* tools): Chromium for the browser_* tools.
        # chromedp's default exec-allocator finds /usr/bin/chromium
        # automatically. Adds ~250 MB. Set tools.browser.enabled:false
        # in config.yaml if you don't want it.
        #
        # REMOVABLE once the deploy runs the external browserd service
        # (docs/browserd.md): with tools.browser.browserd_url set, bob's
        # browser tools are thin HTTP shells and this process spawns NO
        # chromium — this package + the two font lines below can then be
        # dropped from THIS image (sidecars/browser/Dockerfile carries its own).
        # Kept for now: the default in-process mode (browserd_url empty)
        # still needs it; remove only after the deploy side confirms
        # remote mode. (The playwright chromium below is separate — it
        # belongs to scrapling, not the browser_* tools.)
        chromium \
        fonts-liberation \
        fonts-noto-cjk \
    # Install Scrapling 0.4.x + playwright chromium. The browser is required
    # by scrapling's fetch / stealthy-fetch modes (DynamicFetcher +
    # StealthyFetcher). The simpler `extract get` uses curl_cffi (no browser)
    # but fetch/stealthy launch chromium via playwright — without the binary
    # they fail with "Executable doesn't exist at .../chromium-...".
    #
    # Note: we keep BOTH browsers in the image:
    #   - /usr/bin/chromium (apt) — used by chromedp for browser_* tools
    #   - $PLAYWRIGHT_BROWSERS_PATH/chromium-* — used by scrapling fetch modes
    # They can't share: playwright pins a specific chromium build by SHA
    # and validates it on launch. Measured cost of the playwright half: ~630 MB
    # (full chromium ~370 MB + the headless shell ~260 MB that `playwright
    # install chromium` always brings along; scrapling only uses the full one,
    # and playwright has no flag to skip the shell).
    #
    # outage — the browser rungs were dead in production for days.
    # `scrapling extract fetch` / `extract stealthy-fetch` exited 1 on EVERY
    # call with `ValueError: No headers based on this input can be generated`,
    # killing the render + stealth rungs of the web tool outright (engines
    # bing/baidu/google, and every url fetch the model retried at a higher
    # rung). `extract get` kept working, which is why it looked like flaky
    # anti-bot rather than a broken install.
    #
    # Mechanism, in the order it actually bites:
    #   - the fingerprint DATA lives in `apify-fingerprint-datapoints`, a
    #     separate package from the `browserforge` generator that reads it;
    #   - scrapling 0.4.8 asks for exactly one Chrome major (147) pinned
    #     min==max, and only when generating headers for a BROWSER fetch
    #     (`browser_mode`), which is why the plain-HTTP path was unaffected;
    #   - datapoints 0.14.0 shipped a dataset whose chrome entries collapsed to
    #     140-141, so that exact-version ask had no candidate → ValueError.
    #     0.12.0 covered 140-147 and 0.15.0 covers 140-149: the SAME Dockerfile
    #     built a working image before that release and a broken one after.
    # So the root cause is an unpinned data package, not the pinned code:
    # both fingerprint packages are pinned below for that reason. Re-pin them
    # deliberately when refreshing fingerprints — the dataset ages, and a stale
    # one costs stealth quality (0.4.14 falls back to unconstrained headers
    # rather than crashing, so a future mismatch degrades quietly).
    #
    # scrapling 0.4.14 is the code half of the fix: 0.4.13 dropped the max
    # pin and wrapped the generator in a relax-and-retry fallback, so a
    # dataset that lacks the wanted major no longer raises. It also moves the
    # playwright/patchright floors to 1.61 (it now rewrites the reported
    # Chrome version to match the browser build actually installed). Take
    # 0.4.14, not 0.4.13: 0.4.13 required a curl_cffi prerelease.
    && pip install --no-cache-dir --break-system-packages \
        'scrapling==0.4.14' \
        'playwright==1.61.0' \
        'patchright==1.61.2' \
        click \
        'curl_cffi>=0.16.0' \
        'markdownify>=1.2.0' \
        msgspec \
        anyio \
        protego \
        'browserforge==1.2.4' \
        'apify-fingerprint-datapoints==0.15.0' \
    # Both fetch rungs need the chromium build: `fetch` drives it through
    # playwright, `stealthy-fetch` through patchright. Today they pin the same
    # revision so the second install is a no-op, but they are independent
    # packages — when their revisions diverge, only installing playwright's
    # leaves stealthy-fetch failing with "Executable doesn't exist".
    && playwright install chromium \
    && patchright install chromium \
    && chmod -R a+rX /opt/ms-playwright \
    # Trim build deps to keep the image lean.
    && apt-get purge -y --auto-remove gcc g++ libffi-dev libssl-dev libxml2-dev libxslt1-dev \
    && rm -rf /var/lib/apt/lists/* \
    # Pin uid/gid to 1000 (was --system, which picked the first free
    # UID in 100-999 — could be 997 or whatever, host-dependent). 1000
    # is the conventional first-user UID on Debian/Ubuntu, so
    # bind-mounted host directories created by a regular user usually
    # match without manual chown. Avoids the "permission denied" boot
    # loop when first deploying to a fresh box.
    && groupadd --gid 1000 bob \
    && useradd --uid 1000 --gid 1000 --create-home --home-dir /home/bob bob \
    && mkdir -p /data/.bob \
    && chown -R bob:bob /data /home/bob

USER bob
WORKDIR /home/bob
ENV BOB_HOME=/data/.bob

COPY --from=build /out/bob /usr/local/bin/bob

# tini reaps zombies and forwards SIGTERM to bob → graceful gateway shutdown.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/bob"]
CMD ["gateway"]
