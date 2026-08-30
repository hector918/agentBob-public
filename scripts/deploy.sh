#!/usr/bin/env sh
# Deploy bob: pull, then build + restart with the running build's version stamped
# as "<branch>@<short-sha>" (lands in $BOB_HOME/VERSION + the startup log, so you can
# confirm what's live). Run from anywhere — it cd's to the repo root.
#
#   ./scripts/deploy.sh
#
# (.git is excluded from the Docker build context, so the version is derived HERE on
# the host and passed in via the BOB_VERSION build arg.)
set -eu

cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

git pull --ff-only

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
SHA="$(git rev-parse --short HEAD)"

# Namespace EVERYTHING by branch so this deploy never touches another bob's state
# (e.g. the skeleton's ~/.bob). State dir, container, and image all follow the
# branch; the running build's version is "<branch>@<sha>".
export BOB_VERSION="${BRANCH}@${SHA}"
export BOB_STATE_DIR="${HOME}/.bob-${BRANCH}"
export BOB_CONTAINER="bob-${BRANCH}"
export BOB_IMAGE="bob:${BRANCH}"
# NOTE: to run alongside another bob, also set BOB_WEBUI_PORT (default 80) and point
# BOB_DSN at a SEPARATE Postgres database — the rebuild's schema is incompatible with
# the skeleton's, so they must never share a DB.

echo "==> deploying ${BOB_VERSION}  (state ${BOB_STATE_DIR}, container ${BOB_CONTAINER})"
mkdir -p "${BOB_STATE_DIR}"

docker compose up -d --build bob

echo "==> done — ${BOB_STATE_DIR}/VERSION now reports ${BOB_VERSION}"
