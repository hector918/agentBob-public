# Deployment

agentbob ships as a Docker image. You supply three things it will not bring for
you: a **Postgres database**, at least one **OpenAI-compatible model endpoint**,
and credentials for whichever chat platforms you want to connect.

---

## Platform support

Tested on:

- **macOS arm64** (M-series, Docker Desktop)
- **Linux amd64** (Intel/AMD Ubuntu / Debian, native Docker)
- **Linux arm64** (Raspberry Pi 5, AWS Graviton, and similar)

The Dockerfile is architecture-portable: a `debian-bookworm-slim` base, and all
Python dependencies (`scrapling`, `playwright`, `patchright`, `curl_cffi`) have
both amd64 and arm64 wheels. Chromium ships in both apt and playwright variants
per architecture.

> **Images are architecture-specific.** `docker compose build` produces an image
> tied to the build host's CPU. Do not `docker save | scp` an arm64 image onto an
> amd64 host or vice versa — it falls back to QEMU emulation, which is slow and
> flaky.

---

## Build and run on the same machine

```bash
git clone <this-repo> agentbob
cd agentbob
docker compose build           # ~5-8 min first time (downloads chromium)
docker compose up -d
docker compose logs -f bob
```

## Build and run on different machines

**Option A — rebuild on the target (recommended, simplest).** Clone and
`docker compose build` on the target host; it builds natively for that CPU.

**Option B — multi-arch image via a registry**, when you cannot build on the
target:

```bash
# one-time, on your dev machine
docker buildx create --use --name multiarch
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t <registry>/<your-name>/bob:latest \
  --push .

# on every target host
docker pull <registry>/<your-name>/bob:latest
docker tag  <registry>/<your-name>/bob:latest bob:latest
docker compose up -d           # uses bob:latest from local cache
```

---

## From scratch on a fresh Linux server

A greenfield Ubuntu or Debian box with root SSH, end to end.

### 1. Docker engine and compose plugin

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
```

### 2. Lay out the directories

Two locations: where the repo is cloned, and where persistent state lives
(bind-mounted into the container). The paths below are a convention — use
whatever suits your host.

```bash
mkdir -p /srv/agentbob-dev /srv/agentbob-state/bob-home
```

### 3. Clone

```bash
cd /srv/agentbob-dev
git clone <this-repo> agentbob
cd agentbob
```

### 4. Point `${HOME}/.bob` at your state directory

`docker-compose.yml` mounts `${HOME}/.bob`. Symlink it so the mount lands in the
right place — or edit the compose file to use an absolute path instead.

```bash
ln -snf /srv/agentbob-state/bob-home /root/.bob
```

### 5. Configuration files

Three files live in `$BOB_HOME`. Only the first is mandatory.

**`.env` — secrets and the database.** bob refuses to boot without
`BOB_POSTGRES_DSN`; compose does **not** bundle a database, so supply your own
reachable Postgres.

```bash
cat > /srv/agentbob-state/bob-home/.env <<'EOF'
BOB_POSTGRES_DSN=postgres://user:pass@<db-host>:5432/bob?sslmode=disable
TELEGRAM_BOT_TOKEN=<token-from-BotFather>
EOF
```

**`models.yaml` — where your models live.** Use a LAN IP; `host.docker.internal`
does not work on Linux.

```bash
cat > /srv/agentbob-state/bob-home/models.yaml <<'EOF'
entries:
  - name: smart
    provider: llama-cpp
    model: <your-model-id>
    base_url: http://<model-host>:11434/v1
    tags: [smart, local, toolcall]
  - name: small
    provider: llama-cpp
    model: <your-small-model-id>
    base_url: http://<model-host>:11433/v1
    tags: [small, fast, local, vision]
EOF
```

**`config.yaml` — optional.** bob runs on defaults. Add it only to tune things
like tool execution, attachment retention, or filesystem read roots.

### 6. Ownership

UID 1000 matches the in-container `bob` user, so the container can write logs,
state, sandbox, and memory. The Dockerfile pins 1000 regardless of your host
UID — the host owner name does not matter to Docker.

```bash
chown -R 1000:1000 /srv/agentbob-state/bob-home
```

### 7. Build and start

```bash
docker compose build
docker compose up -d
docker exec bob tail -20 /data/.bob/logs/agent.log
```

Expected on a healthy first boot: `bob starting version=…`, `bob serving
sources=…`, and `seeded sources/telegram.yaml (closed-default)`.

---

## Authorize yourself to chat

`sources/telegram.yaml` is seeded **closed by default** — every inbound message
is dropped until you open it. This is deliberate: an agent with shell access,
reachable by anyone who finds the bot handle, is an open shell.

Either open it to everyone (fine for a single-user development box):

```bash
sed -i 's/^allow_all: false$/allow_all: true/' \
  /srv/agentbob-state/bob-home/sources/telegram.yaml
```

Or admit specific user IDs only (look yours up with `@userinfobot` in Telegram):

```bash
cat >> /srv/agentbob-state/bob-home/sources/telegram.yaml <<'EOF'
allowlist:
  - "<your-telegram-user-id>"
admins:
  - "<your-telegram-user-id>"
EOF
```

Then `docker restart bob`. If you skip this, every inbound logs:

```
WARN telegram: drop — user not authorized user_id=... hint=edit $BOB_HOME/sources/telegram.yaml ...
```

---

## Check the models are reachable

```bash
# from the server itself — should return 200
curl -s -o /dev/null -w '%{http_code}\n' http://<model-host>:11434/v1/models

# from inside the container — proves the container can reach them too, which
# matters when the model server sits on a separate host or VLAN
docker exec bob curl -s -o /dev/null -w '%{http_code}\n' http://<model-host>:11434/v1/models 2>&1 || \
  echo "no curl in container — bob will surface 'connection refused' in agent.log when first used"
```

Docker bridge networking reaches LAN IPs by default. You only need a
`network_mode: host` override when the model server binds `127.0.0.1` on the
same host as the container.

---

## Moving hosts

State lives in two places: **Postgres** holds sessions and memory; `~/.bob/`
holds everything else (attachments, skills, browser profiles, logs, sandbox).
Carry both and the target architecture does not matter.

```bash
# on the old host
docker compose down
tar -czf bob-state.tgz -C ~ .bob
pg_dump <your-bob-database> > bob-db.sql

# on the new host (a different CPU architecture is fine)
tar -xzf bob-state.tgz -C ~
# restore the database, then point BOB_POSTGRES_DSN at it
docker compose up -d
```

Sessions, memory, and logged-in browser profiles come back with it.
