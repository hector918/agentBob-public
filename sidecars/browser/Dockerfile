# syntax=docker/dockerfile:1.7
#
# browserd — bob's out-of-process browser service (docs/browserd.md).
#
# Stage 1: golang:alpine builds the static `browserd` binary (cmd/browserd).
# Stage 2: debian-slim runtime carrying ONLY what chromium needs — this is
#          the image that holds the browser; bob's own image can drop its
#          chromium once tools.browser.browserd_url points here (see the
#          note next to `chromium` in ./Dockerfile).
#
# State: $BOB_HOME (/data/.browserd) holds the profile vault
# (credentials/browser-profiles/<key>/ — the login-state MASTER copies) and
# an optional config.yaml (tools.browser.* tuning: timeouts / window size /
# max_concurrent_sessions). Mount it as a persistent volume; bob keeps
# tar.gz backups of the profiles via GET /profile/{key}/export.
#
# Ports (see docker-compose.yml for the wiring rules):
#   8377 — control plane, NO auth: private compose network ONLY.
#   8378 — takeover face, ticket-authed: reachable by bob ONLY (bob's webui
#          proxies it to the human — docs/browserd.md §4); never published.

# ---- build stage --------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# vendor/ is checked in → -mod=vendor build, fully offline (same as ./Dockerfile).
COPY . .
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/browserd ./cmd/browserd

# ---- runtime stage ------------------------------------------------------
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        tini \
        # chromedp's default exec-allocator finds /usr/bin/chromium
        # automatically — same package bob's image installs today.
        chromium \
        fonts-liberation \
        fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/* \
    # uid/gid 1000 mirrors bob's image (./Dockerfile) so bind-mounted host
    # dirs created by a regular user match without manual chown.
    && groupadd --gid 1000 browserd \
    && useradd --uid 1000 --gid 1000 --create-home --home-dir /home/browserd browserd \
    && mkdir -p /data/.browserd \
    && chown -R browserd:browserd /data /home/browserd

USER browserd
WORKDIR /home/browserd
ENV BOB_HOME=/data/.browserd

COPY --from=build /out/browserd /usr/local/bin/browserd

EXPOSE 8377 8378

# tini forwards SIGTERM → graceful shutdown (chromium closed, profiles
# preserved; the next boot resumes warm).
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/browserd"]
