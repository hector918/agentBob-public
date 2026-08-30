# sidecars/

Out-of-process companion services bob talks to over the network — heavy or
specialized workloads kept OUT of the bob image so bob stays small and stateless.
Each subdirectory is one sidecar (its Dockerfile + run scripts + config).

| dir | service | bob reaches it via |
|---|---|---|
| `ocr/` | RapidOCR HTTP backend for the model pool's `kind: ocr` entries (`POST /ocr`, `GET /healthz`). | a `kind: ocr` entry in `models.yaml` pointing at its URL |
| `browser/` | browserd — the browser service (chromium + the browser tool core) in its own container. | `tools.browser.browserd_url` |
| `fun-asr/` | FunASR Fun-ASR-Nano-2512 的 OpenAI-compat shim（CPU），服务模型池的 `kind: asr`。 | 一条 `kind: asr` 条目指向它的 URL |
| `moss-asr/` | MOSS-Transcribe-Diarize 经 ggml 移植（moss-transcribe.cpp）的 OpenAI-compat shim —— 同一套线协议，可直接替换 `fun-asr`，额外带说话人分离与 90 分钟长音频。 | 同上（换 `base_url` + `model` 即可） |
| `chat-retrieval/` | Cold-memory retrieval (Postgres + pgvector; `POST /messages`, `POST /retrieve`, `GET /selftest`). Only retrieves — never calls an LLM; bob synthesizes from the structured results. | the retrieval leaf's configured base URL |

These build/run independently of bob. bob's own image is the root `Dockerfile`;
`docker-compose.yml` wires bob and (optionally) the sidecars together.
