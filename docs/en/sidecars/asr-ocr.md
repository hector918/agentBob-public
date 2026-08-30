# Speech-transcription and OCR sidecars

Three small services: two speech-to-text backends (`fun-asr` / `moss-asr`) and one OCR backend (`pp-ocrv6`).
They share one wiring style — **no code change in the main process, just one more entry in the model pool**.

---

## Where they sit

The main process's model pool routes by `kind`: `kind: asr` entries serve transcription, `kind: ocr` entries serve image-to-text. A sidecar that impersonates the protocol its kind expects is, to the pool, an ordinary provider.

| kind | Wire protocol | Main-process caller |
|---|---|---|
| `asr` | OpenAI-compatible `POST /v1/chat/completions`; audio goes in as an `audio_url` data-URI content block, the transcript comes back as the assistant message | `leaf/asr/` (ingestion), `leaf/tools/99-audio.go` (on demand) |
| `ocr` | `POST /ocr` multipart (`image` plus optional `lang` / `bbox` / `enhance`), returning per-line boxes and a joined text | `leaf/tools/96-ocr.go` (`vision task=read`), with the provider in `leaf/model/providers/36-rapidocr.go` |

Two main-process paths are worth stating first, because much of what the sidecars do is forced by them.

### Speech has two positions

`leaf/asr/10-asr.go` transcribes at **ingestion** — before the message row is persisted. The transcript goes straight into the message text, and every downstream reader (prompt, stored history, corpus feed) treats it as ordinary text, tagged only so the model knows it was spoken rather than typed.

It splits by *whose words these are*:

- **Instruction position** — the sender's own, captionless voice note. The transcript *is* the request, so it enters the message text as if typed, and is never sanitised: those are the user's own words in the user's own slot, and their typed text is not sanitised either.
- **Material position** — a captioned clip, a clip carried in from a replied-to message, a video's soundtrack. Third-party content, so it is framed with its source and run through the same hygiene as a quoted reply.

This split is the point of the whole design. Before it, a transcript was appended to the caption and landed in the user role unframed and unbounded — the **one** path in the system that put somebody else's words into the instruction channel with no tool boundary (tool results carry the tool role; quoted replies go through their own line treatment).

### Ingestion has a duration line

A clip longer than about thirty seconds is not transcribed here; it is **announced**, and the model pulls the words on demand with the `audio` tool.

That line is a **latency budget, not a capability limit**: ingestion is synchronous, with a human waiting for the turn to start. The backend swallows far longer audio in one pass; ingestion is what cannot afford the wall clock.

"Declined" and "failed" must not blur here: a transcription failure hands the model a fallback note asking the user to retype, and saying that about a clip we simply *chose* not to attempt is both false and steers the model away from the tool that can still fetch the words.

Only **audio** is announced; a long video is not. A video's soundtrack usually carries no intelligible speech, so advertising retrievable words on every shared clip would buy a wasted tool round each time. The file is still in the attachment list and the `audio` tool accepts video — the words stay reachable, they are simply not advertised.

### The on-demand leg carries its own gate

`leaf/tools/99-audio.go` transcribes at most five minutes per call and **reports the remaining range**.

The segmentation strategy is to make the model the segmenter: it calls again with the next range if it still needs more, and stops when it has what it came for. A chunk-and-stitch engine was deliberately not written — such an engine always transcribes everything, including the fifty minutes nobody asked about.

It also carries **its own** deadline, which is damage control rather than politeness: the provider's hard timeout is **counted against entry health**, so two slow calls would be enough to cool the single `kind: asr` entry and take ingestion's voice transcription down with it. A local context deadline expires as a plain context error and costs only that one call.

### Images are not pre-extracted

Pictures are not OCR'd at ingestion; the model reads them on demand through the `vision` tool.

`readImageText` in `96-ocr.go` and the vision leg in `97-vlm.go` share a `kindChat` — a model-pool handle **bound to a single kind**. A media tool can only ever reach its own backend, never the general LLM. The read leg accepts images only: a per-frame transcript of a moving scene is not what "transcribe this" means, and quietly sampling one frame would answer a question nobody asked.

---

## fun-asr

**Role**: the routine `kind: asr` backend and the workhorse for short voice notes. A 0.8B end-to-end speech-recognition model (Chinese including dialects, English, Japanese) on CPU, wrapped in an OpenAI-compatible FastAPI shim.

**Interface**:

| Method | Path | Who uses it |
|---|---|---|
| POST | `/v1/chat/completions` | the main process (audio in, text out) |
| GET | `/v1/models` | the pool's reachability ping — it tolerates a 404, but a 2xx is cleaner |
| GET | `/healthz` | liveness plus whether the model is resident |

**Internals**: one `server.py`.

It pulls every `audio_url` data-URI clip out of the messages in order (the main process sends one per call, but N are handled defensively), decodes each into a temp file, runs the model, and packs the text back into the chat-completion shape.

- The model is **lazy-loaded**: the torch and weight imports are deferred until the first real transcription, so a health probe never drags them in;
- once loaded it stays **resident** (this is a dedicated box, so no idle-exit dance);
- one lock serialises both load and inference — a single CPU instance, where concurrent requests trampling each other buys nothing.

Language comes from `ASR_LANGUAGE` and inverse text normalization from `ASR_ITN`, both overridable per request. The model requires an explicit language (it has no documented auto-detection) and the main process does not send one, so the default is a deployment-side decision.

**Why an OpenAI-compatible shim rather than an `/asr` endpoint**: because that makes it an unremarkable provider in the pool's eyes, with no bespoke client to write on the main side. The whole integration is one configuration entry.

**One deployment lesson: `/healthz` is not a smoke test.** It answers ok without ever touching the model — the graph is lazy, so a build that cannot load the model at all still probes green (and so does `/v1/models`, which is exactly what the pool pings). A smoke test must send a real transcription and then confirm the health endpoint's "model loaded" flag has flipped.

---

## moss-asr

**Role**: a second `kind: asr` backend speaking the **identical wire protocol**, so switching is a one-line `base_url` + `model` change. Behind it is a model that produces transcription, speaker diarization and timestamps in a single pass (50+ languages, very long recordings in one go), running on CPU via a ggml C++ port.

### Why the ggml port rather than the official PyTorch path

The target box is an older CPU: AVX2 and none of the newer matrix instructions.

The model's decoder is autoregressive, hence **memory-bandwidth bound** on CPU — in fp32 every generated token reads several gigabytes of weights, for a measured real-time factor of about 1.0 on comparable hardware (a forty-minute recording takes forty minutes). The port is 2–3× faster under quantization and drops the weights from 3.4 GB to roughly 0.6 GB.

What makes a third-party port usable is its **correctness argument**: every component is gated numerically against tensors dumped from the reference model, and the end-to-end output is byte-identical at the higher precision tiers. Accuracy is what usually kills a port like this, and that question was answered first.

### Internals

Also one `server.py`, but shaped nothing like `fun-asr`:

- The port is CLI-only today, so the shim **forks a subprocess per request**. The cost is acceptable: ggml uses mmap, and once startup prewarming has pulled the weights into the page cache each "load" is just a walk of the page tables.
- The main process sends FLAC and the CLI only eats WAV, so every request is transcoded to 16 kHz mono with ffmpeg. **This is not an optional optimisation** — omit it and the very first request fails.
- Several quantization tiers are baked into the image and **chosen at runtime**, so trying a different one is an environment variable (or a per-request override) rather than a rebuild plus a several-hundred-megabyte download.
- The subprocess has a wall-clock ceiling deliberately set **above every caller's own deadline**: whoever asked should give up first. A caller's timeout is a policy about how long a human waits; this ceiling only stops a wedged subprocess from pinning a worker forever.

It also implements the standard OpenAI transcription endpoint (multipart upload), which nothing uses today — it is there so a future main-side provider can move to it without the sidecar needing another revision.

### Output shaping is the most important section here

The model's raw output carries timestamps and speaker labels.

**Clean text is returned by default and the timestamps are dropped.** That string is folded straight into the message body; timestamps would then consume budget in every subsequent prompt while no downstream reader would ever use them.

**A speaker prefix appears only when there really are two or more speakers**, following the same rule group messages already use (fewer than two, no prefix). So a single-speaker voice note renders **exactly** as the other backend's output does — for the overwhelming majority of real traffic, swapping backends is invisible in the output.

**Parsing targets the CLI's default raw output, not its JSON mode.** The bracketed format is the **model's own** output contract (the upstream model card and the porter's documentation agree on it), whereas a JSON schema belongs to the porter and may change.

Parsing degrades in three steps: full segments → no speaker labels → strip all markup and keep the words. **A format surprise degrades to plain text, never to an empty transcript** — which is the only thing that matters when choosing a degradation direction. Asking for `verbose_json` returns the raw string, per-segment start/end/speaker/text, and the speaker list alongside.

### Two structural findings, worth more than any single number

**① There is a fixed floor of roughly fifteen seconds.**
A one-second clip still costs that — the encoder aligns to a thirty-second window, and a short clip pays for the whole window. Its real-time factor therefore **improves as audio gets longer**, the opposite of intuition. Thread count tops out at the physical core count; hyper-threading does approximately nothing for autoregressive decoding. Even the most aggressive quantization at full threads cannot get under the floor — it is structural.

**② The other backend is not slow on long audio, it is broken.**
A recording of a few minutes comes back as a degenerate repeated string — something that looks like a transcript and is garbage. Ingestion's timeout hides it today, but the moment that timeout is relaxed for long audio, that garbage would be folded into a message body. This is not a latency problem.

**So the two backends are complementary, not alternatives:**

| Case | Which | Why |
|---|---|---|
| Short voice notes (ingestion, synchronous, someone waiting) | `fun-asr` | a few seconds versus a dozen-plus; the fifteen-second floor hurts most here |
| Long recordings (on-demand tool, nobody watching) | MOSS | the other backend emits garbage; MOSS throws in diarization |

That lands exactly on the existing push/pull split — **audio that *is* the message gets pushed; audio that is *material* gets pulled**.

**Routing uses mechanisms that already exist**: tag the MOSS entry as longform and keep the other at higher priority. Ingestion picks up `fun-asr` by kind, and the `audio` tool picks up MOSS by tag. No new mechanism required.

Automatically raising the thread count for short clips extends the same logic, and is **deliberately the opposite of a fairness policy**: a short clip's cost is dominated by that fixed encoder window and is paid synchronously, with someone waiting; a long recording runs where nobody is watching and can afford to be polite to the other services on the box.

---

## pp-ocrv6

**Role**: the `kind: ocr` backend — a detection and a recognition ONNX model behind FastAPI. Onnxruntime only, CPU only, light, and completely independent of the box that hosts the vision model. Which is the whole point: **when the vision model goes offline, OCR stays alive**.

**Contract**: `POST /ocr` multipart takes `image`, plus optional `lang` (accepted for parity), `bbox` (server-side crop) and `enhance`. It returns per-line boxes, text and confidence, plus a joined full text.

`enhance=gray` is the deep-colour-screen treatment: coloured text on a dark background (medical and ultrasound screens) is desaturated before detection and recognition. The main process passes it on the forced-OCR rounds — it is a **contract parameter**, not a server-side guess.

The main-side provider maps one Chat call onto several `/ocr` calls: one image per message, each with its own parameters, aggregated into one envelope. **Failure is collect-all** — one image's error populates that image's field without aborting the rest of the batch.

**Concurrency and backpressure**: OCR is blocking CPU work, so a request runs in a worker thread and the number of engine instances (`OCR_ENGINE_POOL`) is the concurrency ceiling, one borrowed per call.

This is **not a throughput knob** — onnxruntime already spreads a single request across the cores, and a second worker barely doubles anything. It does two things: it stops a second caller waiting out the first in full, and it keeps the health endpoint answering while OCR runs. Instances are built lazily, so a container that only ever sees one request at a time only ever builds one.

Waiting longer than `OCR_QUEUE_TIMEOUT` for a free engine answers **503**, and the meaning of that 503 is **busy, not broken**: the main process classifies it as a transient backend error and queues with backoff rather than declaring OCR dead. The timeout must be comfortably longer than one ordinary OCR (so normal queueing still succeeds) and still bounded (so a wedged engine surfaces as "busy" instead of parking callers forever — each waiter pins one of a shared threadpool's workers).

**Model files**: the detection/recognition ONNX and the recognition charset are fetched at build time, with all three paths overridable by environment variable; anything already present is kept (the script is idempotent), so pinning your own weights means dropping them in beforehand.

The health endpoint carries a flag for "are the newer models actually in use" — **check it after deploying**. False means the model files were not found and the service silently fell back to its bundled older models. Everything keeps working; it is simply less accurate than you believe. Silent degradation like that is exactly what a health endpoint is for.

---

## The shared rationale

**Heavy dependencies stay out of the main process.**
Torch plus weights is several gigabytes; onnxruntime plus models is hundreds of megabytes; the ggml port needs a C++ toolchain. The main process is a clean Go binary, and none of that belongs in its image. As sidecars they go wherever suits — the box with the compute, the box with spare CPU, a box that looks nothing like the main one — with zero change on the main side.

**One wire protocol means zero-code backend swaps.**
All three sidecars impersonate a protocol the pool **already speaks** instead of inventing an endpoint for the main process to adapt to. The cost is a translation layer inside the shim (transcoding audio, re-enveloping results); the benefit is that both integration and replacement are configuration: add an entry, change a `base_url`. Two ASR backends being online simultaneously and split by tag is a direct dividend of that choice.

**Busy and broken must be reported differently.**
503 means busy and retryable; 5xx means broken. Both ends have to honour it: the sidecar must answer 503 when its queue is full rather than stalling or returning 500, and the main process must classify that 503 as transient and retry rather than counting it against entry health. The failure is asymmetric — treating "busy" as "broken" takes a healthy backend offline under its own load.

**A health endpoint is not a smoke test.**
A lazily-loading service probes green having never touched its model. A health endpoint should answer "is the process and are its dependencies there, and what configuration did it actually resolve" — the OCR model-version flag, the retrieval side's dimensionality self-check. A smoke test has to run a real call.

**"We are not doing this" is also a conclusion — write it down.**
These sidecars carry several deliberate non-decisions: no script conversion inside the shim, no chunk-and-stitch transcription engine, no announcement for video soundtracks. None of them is an omission; each is a considered trade-off. Putting the reasoning in the code comments and the documentation is what stops the next person proposing the same "optimisation" again.
