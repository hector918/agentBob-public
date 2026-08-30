# Deploying the pp-ocrv6 sidecar (standalone, on the sidecar server)

This sidecar runs on the **separate sidecar/models server**, NOT alongside bob.
bob reaches it over the LAN at `http://<sidecar-host>:<port>` (same as the existing
OCR/VLM backends). Making it the **primary** OCR needs **no bob code change** —
just a `models.yaml` entry.

Prereqs: the build host can reach `huggingface.co` (the image downloads ~140MB of
v6 models at build).

---

## 1. Copy this directory to the sidecar server

It's self-contained (Dockerfile + the standalone `docker-compose.yml` + the
model-fetch script all live here):

```sh
scp -r sidecars/pp-ocrv6 <sidecar-host>:~/pp-ocrv6
```

## 2. Build + run — one command, in that directory

```sh
cd ~/pp-ocrv6
docker compose up -d --build      # build downloads the v6 models (~140MB, needs network)
```

That's it — the bundled `docker-compose.yml` builds the image, downloads the
medium det+rec ONNX + extracts the charset, and runs the container with
`-p 11500:11500` (LAN-published) and `restart: unless-stopped`.

- **Different host port** if 11500 is taken: edit `ports:` in `docker-compose.yml`
  to e.g. `"11600:11500"` → bob then uses `http://<sidecar-host>:11600`.
- **The engine stays resident** — `OCR_IDLE_EXIT_SECONDS: "0"` is set in
  `docker-compose.yml`. The old idle self-exit saved ~400MB of RSS
  at the cost of a restart window every 5 idle minutes, and nginx-lb answers
  **502** for the seconds the container is down. Set it back to `300` only on a
  box that is genuinely short of memory.
- No-network build box? Run `sh fetch-models.sh ./models` where there IS network
  (or drop your own `det.onnx`/`rec.onnx`/`keys.txt` into `./models/`) first — the
  build keeps pre-placed files.
- Security: the published port has no auth (same as the other LAN OCR/model
  backends) — keep it on the trusted LAN, never the public internet.

## 4. Wire it into bob's models.yaml as the PRIMARY OCR

In bob's `models.yaml` (on the **bob** host), add an OCR provider pointing at the
sidecar's **LAN address**, with a higher `priority` than the VLM OCR entry. Shape —
mirror your current OCR provider, new `base_url` + higher priority:

```yaml
  - kind: ocr
    provider: rapidocr                       # bob-side /ocr client (leaf/.../36-rapidocr.go)
    base_url: http://<sidecar-host>:11500     # the sidecar server's LAN IP:port
    priority: 30                             # ABOVE the VLM OCR entry → v6 serves first
    concurrency: 2                           # MUST match the sidecar's OCR_ENGINE_POOL —
                                             # bob also sizes the kind's wait queue at
                                             # 2× this before reporting "queue full"
    # (leave the VLM/older entries lower — the pool fails over down the chain
    #  when the top one errors/cools)
```

Reload the pool:

```sh
/model reload         # from a bob admin chat / the webui dock
# (or restart bob)
```

## 5. Verify (do NOT skip)

1. **Health + models present** — from bob's host (proves the LAN path works too):
   ```sh
   curl http://<sidecar-host>:11500/healthz
   # expect: {"ok":true,"engine_loaded":...,"engines_built":N,"engines_busy":N,
   #          "pool_size":2,"v6_models":true}
   ```
   `"v6_models": false` ⇒ det/rec/keys aren't all in place (it fell back to bundled
   v4) — re-check the build/model paths.

2. **Engine loads v6 on first OCR** — trigger one OCR, watch the container log:
   ```sh
   docker logs -f pp-ocrv6
   ```
   Expect `loading RapidOCR with PP-OCRv6 models`, NOT a stack trace. If RapidOCR
   rejects the v6 model I/O, switch the engine to a v6-native ONNX pipeline (e.g.
   OnnxOCR) — same server/contract, only `_new_engine()` changes.

   Concurrency check while you are here: fire two `/ocr` calls at once and hit
   `/healthz` in between. Both OCRs should overlap (`engines_busy: 2`) and
   healthz should answer immediately. If healthz blocks for the length of an
   OCR, the request is running on the event loop again — `/ocr` must hand the
   blocking work to `run_in_threadpool`.

3. **Real ultrasound check** — run your actual ultrasound screenshots through bob's
   OCR (with `enhance=gray`) and compare numbers/text vs the VLM. Confirm
   deep-color readings are right and on-screen UI noise is tolerable before trusting
   v6 as primary.

## 6. Rollback

Instant, bob-side only: lower v6's `priority` below the VLM entry in `models.yaml`
(or remove it) and `/model reload` — the VLM resumes as primary. The sidecar
container can stay up (idle it costs ~400MB of RSS and no CPU).

---

### Notes
- CPU-only, independent of the GPU/VLM box — that independence is the point: with
  v6 primary, OCR (incl. ultrasound) survives the VLM going offline.
- Accuracy vs latency: rebuild with `small`/`tiny` models (edit the URLs in
  `fetch-models.sh`).
- This sidecar does NOT go in bob's `docker-compose.yml` — it lives on the sidecar
  server, reached over the LAN.
