#!/usr/bin/env bash
# Build AND (re)launch the browserd image — bob's out-of-process browser service
# (docs/browserd.md). One-shot deploy: build the fresh image, drop the old container, run the
# new one (plain `docker run`, no compose).
#
# The browser engine (cmd/browserd + browserd/ control plane + tools/browser chromedp core +
# its deps) lives RIGHT HERE, as a self-contained nested Go module (its own go.mod, module
# path `agentbob` so the engine's imports resolve in-tree; vendored for an offline build).
# bob's main module excludes this directory (a nested go.mod is a module boundary). Ported
# from the now-deprecated `skeleton` branch.
#
# Usage (run on the host that runs browserd — i.e. .174):
#   ./sidecars/browser/build.sh                           # build → rm → run (one shot)
#   BROWSERD_NO_UP=1 ./...build.sh                        # build only (skip rm + run)
#
# Persistence + secret are HOST-side and survive every image rebuild, so a bare
# ./build.sh "just works" each time — no per-build env to set:
#   - BROWSERD_VOLUME (default /home/browserd): a real host dir, bind-mounted to the
#     container's $BOB_HOME. Holds the login vault (credentials/browser-profiles/).
#     One-time: mkdir -p, then `chown -R 1000:1000` (the container runs as uid 1000).
#   - BROWSERD_ENV_FILE (default <vol>/.env): a host-side, gitignored env file this
#     script auto-sources for BROWSERD_API_KEY. Set the key ONCE there.
# Env knobs: BROWSERD_API_KEY (control-plane bearer; must match bob's — set it in the
# env file, NOT inline; there is NO baked-in default), BROWSERD_NAME (container name,
# default browserd), BROWSERD_PORTS_CTRL/TAKEOVER (default 8377/8378).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${BROWSERD_IMAGE:-browserd:latest}"
NAME="${BROWSERD_NAME:-browserd}"
VOL="${BROWSERD_VOLUME:-/home/browserd}"
PCTRL="${BROWSERD_PORT_CTRL:-8377}"
PTAKE="${BROWSERD_PORT_TAKEOVER:-8378}"

ENV_FILE="${BROWSERD_ENV_FILE:-$VOL/.env}"

# Self-seeding (idiot-proof): the FIRST run on a fresh host creates everything; later
# runs are no-ops. Only for a bind-mount path (absolute VOL) — a named volume is
# docker-managed and seeds itself. The container runs as uid 1000, so the host dir
# must be owned by 1000 or the in-container process can't write the vault.
case "$VOL" in
/*)
	if [ ! -d "$VOL" ]; then
		echo "→ seeding persistence dir $VOL (first run)…"
		mkdir -p "$VOL/credentials/browser-profiles"
		chown -R 1000:1000 "$VOL"
		chmod 700 "$VOL"
	fi
	if [ ! -f "$ENV_FILE" ]; then
		key="$(openssl rand -hex 32 2>/dev/null || true)"
		[ -n "$key" ] || key="$(od -vN32 -An -tx1 /dev/urandom | tr -d ' \n')"
		mkdir -p "$(dirname "$ENV_FILE")"
		printf 'BROWSERD_API_KEY=%s\n' "$key" >"$ENV_FILE"
		chmod 600 "$ENV_FILE"
		SEEDED_KEY="$key"
	fi
	;;
esac

# Auto-source the host-side env file so BROWSERD_API_KEY is picked up on every rebuild
# without re-supplying it. Lives beside the vault on the host, never in git. `set -a`
# exports what it defines for the docker run below.
if [ -f "$ENV_FILE" ]; then
	echo "→ sourcing $ENV_FILE"
	set -a; . "$ENV_FILE"; set +a
fi

# If we just minted a key, shout it: it MUST also go into bob's .env on the bob host.
if [ -n "${SEEDED_KEY:-}" ]; then
	echo "============================================================"
	echo "  Generated a NEW control-plane key (saved to $ENV_FILE):"
	echo
	echo "    BROWSERD_API_KEY=$SEEDED_KEY"
	echo
	echo "  → put that SAME line in bob's .env on the bob host, then:"
	echo "      docker compose up -d bob"
	echo "============================================================"
fi

echo "→ docker build $IMAGE (chromium runtime + cmd/browserd, offline vendored build)…"
docker build -t "$IMAGE" "$HERE"
echo "✓ built $IMAGE"

if [ -n "${BROWSERD_NO_UP:-}" ]; then
	echo "(BROWSERD_NO_UP set — skipping rm + run)"
	exit 0
fi

echo "→ dropping any old '$NAME' container…"
docker rm -f "$NAME" 2>/dev/null || true

# Control-plane bearer key — MUST match bob's BROWSERD_API_KEY. Comes from the env
# file sourced above; NEVER a baked-in default (a committed key is a public key, and
# the control plane has no other auth). Fail closed if unset rather than launch an
# unprotected /cdp + profile-export face. Generate one with: openssl rand -hex 32
KEY="${BROWSERD_API_KEY:?set BROWSERD_API_KEY in $ENV_FILE — refusing to launch with no control-plane key}"

echo "→ docker run $NAME ($IMAGE)…"
docker run -d --name "$NAME" --restart unless-stopped \
	-p "$PCTRL:8377" -p "$PTAKE:8378" \
	-v "$VOL:/data/.browserd" \
	-e BROWSERD_API_KEY="$KEY" \
	"$IMAGE"
echo "✓ $NAME up — verify: curl -s -H \"Authorization: Bearer $KEY\" localhost:$PCTRL/healthz"
