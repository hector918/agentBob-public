# fun-asr-sidecar

The ASR backend for the bob model pool's `kind: asr` entries. An OpenAI-compat
FastAPI shim around [FunASR](https://github.com/FunAudioLLM/FunASR)'s
**Fun-ASR-Nano-2512** (0.8B end-to-end speech-to-text; Chinese incl. dialects,
English, Japanese; Apache-2.0).

Runs on its **own box** (CPU-only), not in bob's compose — the image is fat
(torch + 0.8B weights). bob reaches it over the LAN and needs **zero code
change**: the voice-preprocess pass already transcodes clips and calls the pool's
KindASR entry as a plain openai-compat provider.

## What lives where

- `server.py` — the FastAPI shim: `POST /v1/chat/completions` + `GET /v1/models`
  + `GET /healthz`
- `Dockerfile` — `python:3.11-slim` + CPU torch + funasr; bakes the model into
  the HF cache at build, then freezes `HF_HUB_OFFLINE=1`
- `docker-compose.yml` — standalone, `restart: unless-stopped`, port 11502
- `requirements.txt` — pinned versions (torch/torchaudio installed separately
  from the CPU wheel index)

## Why an OpenAI-compat shim (not a `/asr` endpoint)

bob's `flow/compose/30-asr.go` (`PreASR`) transcodes each voice/audio attachment
to **16 kHz mono FLAC** via ffmpeg, then sends it to the pool's `KindASR` entry.
That entry is a normal openai-compat provider — the wire layer
(`leaf/model/providers/30-wire.go`) encodes the clip as an OpenAI `audio_url`
data-URI content block (the shape "whisper-style ASR shims accept"). So the
sidecar only has to speak `/v1/chat/completions`.

## API

### `POST /v1/chat/completions`

Request (the relevant subset bob sends):

```json
{
  "messages": [
    {"role": "user", "content": [
      {"type": "audio_url",
       "audio_url": {"url": "data:audio/flac;base64,<...>"}}
    ]}
  ]
}
```

Response:

```json
{
  "choices": [
    {"index": 0,
     "message": {"role": "assistant", "content": "<transcript>"},
     "finish_reason": "stop"}
  ],
  "model": "FunAudioLLM/Fun-ASR-Nano-2512",
  "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
}
```

Errors: 400 (no `audio_url` clip / bad `messages`), 500 (FunASR error).

Optional top-level overrides (bob doesn't send these — handy for `curl`):
`"language"` (中文/英文/日文), `"itn"` (bool), `"hotwords"` (string array).

### `GET /v1/models`

Stub list — the pool's reachability Ping probes `/models`; a 2xx is nicer than
the 404 it tolerates.

### `GET /healthz`

`{"ok": true, "model": "...", "model_loaded": <bool>}`. No auth — the sidecar
sits on the LAN, not exposed externally.

## Language

Fun-ASR-Nano takes an explicit `language` (中文 / 英文 / 日文 — **no documented
auto-detect**), and bob does not forward one. The shim defaults via
`ASR_LANGUAGE` (`中文`) and a request may override per-call. For an English- or
Japanese-primary deploy, set `ASR_LANGUAGE` in the compose env. `itn` (inverse
text normalization → digits/punctuation) defaults on via `ASR_ITN`.

## bob wiring (models.yaml on the deploy box)

```yaml
- name: funasr-nano
  kind: asr
  provider: openai                       # default openai-compat client
  base_url: http://<this-host>:11502/v1
  model: fun-asr-nano                     # label only; the shim ignores it
```

No other bob change — `PreASR` picks up any live `kind: asr` entry.

## Memory / startup

- Build needs network (bakes ~a couple GB of weights into the HF cache).
- The model **lazy-loads on the first transcription** (torch + 0.8B graph; tens
  of seconds on CPU) and stays resident — no idle-exit (dedicated box).
- Working set a few GB; `mem_limit: 4g` in the compose. Inference is serialized
  over the single CPU model instance.

## Local smoke

**`/healthz` alone is not a smoke test.** It answers `ok: true` without ever
touching the model — the graph lazy-loads on the first transcription, so a build
that can't load it at all still probes green (as does `/v1/models`, which is what
bob's pool pings). Always run the transcribe call below, and check that
`model_loaded` has flipped to `true` afterwards.

```bash
docker compose up -d --build
curl http://localhost:11502/healthz          # model_loaded: false — expected, nothing loaded yet

# transcribe a clip (bob sends 16k mono flac; any ffmpeg-decodable file works here):
b64=$(base64 -w0 sample.flac)
curl -s http://localhost:11502/v1/chat/completions \
  -H 'content-type: application/json' \
  -d "{\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"audio_url\",\"audio_url\":{\"url\":\"data:audio/flac;base64,$b64\"}}]}]}" \
  | jq -r '.choices[0].message.content'

curl http://localhost:11502/healthz          # model_loaded must now be true
```
