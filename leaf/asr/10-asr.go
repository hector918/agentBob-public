// Package asr is the voice-preprocess step: it transcribes an inbound event's
// voice/audio attachments to TEXT at ingestion (session submit), BEFORE the user row is
// persisted — so the transcript lives in the message text and every downstream reader
// (the prompt, stored history, the recall feed) treats spoken content as ordinary text,
// tagged only so the model knows it came from a voice note. This replaces the older
// per-turn compose pass that split a transcript across ev.Text (bare note) vs a separate
// att.Transcript "[语音转写]" section — one text path now.
//
// Transcription routes through the model pool's KindASR backend (ffmpeg → flac → ASR),
// exactly as before. Images, by contrast, are read on demand by the model via the ocr
// tool, not pre-extracted.
package asr

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/prompt"
)

const (
	// transcribeTimeout bounds ONE clip's transcode + backend call. Sized against the
	// ingestion cap below and the deployed backend's measured cost (~12 s fixed + ~0.65x
	// realtime), so a clip at the cap finishes with slack rather than dying at the wire.
	transcribeTimeout    = 90 * time.Second
	transcodeConcurrency = 4
	// transcribeMaxBytes caps the audio file size before transcode (25 MB).
	transcribeMaxBytes int64 = 25 * 1024 * 1024
	// transcribeMaxSeconds is the INGESTION line: past it a clip is not transcribed here
	// at all — it is announced instead, and the model pulls the words on demand.
	//
	// It is a LATENCY budget, not a capability one. The backend handles far longer audio
	// in one pass; ingestion is what cannot afford it, because it runs synchronously with
	// a human waiting for the turn to start. At the deployed backend's measured cost a
	// 30 s clip lands around 31 s of wall clock — already a long pause, and the point
	// past which "just transcribe it" stops being the kind thing to do.
	transcribeMaxSeconds = 30.0
	// voiceTag prefixes a transcript that IS the message — the sender's own words, in the
	// instruction position, marked only so the model knows they were spoken not typed.
	voiceTag = "[语音]"
	// materialTag opens a transcript that is CONTENT rather than instruction. It names a
	// source, deliberately unlike voiceTag: the whole point of the material position is
	// that these words belong to someone or something other than the person asking.
	materialTag = "音频内容"
	// transcribeFailNote keeps a voice-only turn from going empty when transcription
	// fails — the model still gets something to respond to. Instruction position ONLY.
	transcribeFailNote = "（语音转写失败，请用户用文字重发或稍后再试）"
)

// transcodeSem bounds in-flight ffmpeg transcodes process-wide so a burst of voice
// messages can't spawn an unbounded number of subprocesses.
var transcodeSem = make(chan struct{}, transcodeConcurrency)

// Transcribe turns an event's soundtracks into text, SPLIT BY WHOSE WORDS THEY ARE.
//
//	instruction — the sender's own captionless voice note. The transcript IS the request;
//	  submit puts it in the message text as if typed. Never sanitised: those are the
//	  user's own words in the user's own slot, and their typed text is not sanitised
//	  either.
//	material    — a captioned clip, a clip carried in from a REPLIED-TO message, or a
//	  video's soundtrack. Third-party content, so it is framed with its source and run
//	  through the same hygiene the quoted-reply line uses.
//
// The split is the point. Before it, a transcript was appended to the caption and landed
// in the user role unframed, unattributed and unbounded — the ONE path in bob that put
// somebody else's words into the instruction channel without a tool boundary (tool
// results are role:tool; quoted replies go through prompt.ReplyLine + SanitizeQuoted).
//
// Best-effort throughout: a nil pool / unreadable file / backend error logs and degrades.
// Transcription never fails the turn.
func Transcribe(ctx context.Context, pool contract.ModelPool, ev contract.MessageEvent) (instruction, material string) {
	if pool == nil {
		return "", ""
	}
	// Captionless is decided HERE and not by the caller: it is half of "is this clip the
	// message or is it an attachment", and splitting that judgement across two files is
	// how the two halves drift apart. Submit has not stamped the stand-in yet at this
	// point, but a source may have (email does), so both spellings of "no caption" count.
	cur := strings.TrimSpace(ev.Text)
	captionless := cur == "" || cur == contract.NoCaptionStandIn

	var spoken, mats []string
	hadOwnSpeech := false
	for i := range ev.Attachments {
		// Taken by POINTER so the Transcribed mark below lands on the caller's slice —
		// ev is a copy, but its Attachments share a backing array, which is the same way
		// placement mutates them one step later.
		a := &ev.Attachments[i]
		if !isAudioAttachment(*a) {
			continue
		}
		// Does this clip speak FOR the sender? Only their own voice note, on a message
		// that carries no words of its own. A forwarded-in (replied-to) clip fails this
		// even with no caption — that case is precisely how somebody else's recording
		// used to end up occupying the user's instruction slot.
		ownVoice := captionless && !a.FromReply && a.IsAudioContent()

		if dur, err := probeDuration(ctx, a.LocalPath); err == nil && dur > transcribeMaxSeconds {
			// Declined, not failed. The two must not blur: the fallback note below says
			// transcription BROKE and asks the user to retype, which for a clip we simply
			// chose not to attempt is false AND steers the model away from the tool that
			// can still fetch the words. So hadOwnSpeech is armed only past this gate.
			//
			// Announced for AUDIO only. A long video gets nothing: its soundtrack usually
			// carries no intelligible speech (measured — a 65 s disaster clip yielded one
			// shouted sentence), so advertising retrievable words in every shared video
			// would buy a wasted tool round per clip. The file is visible in the
			// attachment list and the audio tool accepts video, so the words stay
			// reachable; they are just not advertised.
			if a.IsAudioContent() {
				slog.Info("asr: clip too long for ingestion, announcing instead",
					"file", a.FileName, "kind", a.Kind, "seconds", int(dur))
				mats = append(mats, longClipNote(*a, dur))
			} else {
				slog.Info("asr: clip too long for ingestion, skipping silently",
					"file", a.FileName, "kind", a.Kind, "seconds", int(dur))
			}
			continue
		}
		if ownVoice {
			hadOwnSpeech = true
		}

		text, err := transcribeAudio(ctx, pool, a.LocalPath)
		if err != nil {
			slog.Warn("asr: transcription failed (skipping audio)", "file", a.FileName, "kind", a.Kind, "err", err)
			continue
		}
		if text = strings.TrimSpace(text); text == "" {
			slog.Warn("asr: transcription returned empty text", "file", a.FileName, "kind", a.Kind)
			continue
		}
		// The words are now in the message text — say so on the attachment, or the prompt's
		// list shows an untouched-looking file and invites a redundant fetch.
		a.Transcribed = true
		if ownVoice {
			spoken = append(spoken, voiceTag+"\n"+text)
			continue
		}
		mats = append(mats, materialBlock(*a, text))
	}

	instruction = strings.Join(spoken, "\n\n")
	// The fallback note exists so a VOICE-ONLY turn never goes empty, so it belongs to
	// the instruction position alone. A video that yields nothing is the normal case (its
	// content is the picture), and a message with a caption already has words.
	if instruction == "" && hadOwnSpeech {
		instruction = transcribeFailNote
	}
	return instruction, strings.Join(mats, "\n\n")
}

// materialBlock frames one transcript as attributed, sanitised CONTENT.
//
// Sanitised with the same primitives the quoted-reply line uses: a transcript is model
// output over third-party audio, so it can contain brackets and newlines — exactly the
// characters prompt.SanitizeSpeaker / SanitizeQuoted exist to keep out of the "[name]: "
// and "[In reply to …]" meta structure. It is NOT length-capped, unlike a quote: this
// text is the thing the user wants processed, and clipping it would defeat the point.
func materialBlock(a contract.Attachment, text string) string {
	head := materialTag
	if name := prompt.SanitizeSpeaker(a.FileName); name != "" {
		head += " · " + name
	}
	return "[" + head + "]\n" + sanitizeTranscript(text)
}

// longClipNote is what the model sees INSTEAD of a transcript when a clip is past the
// ingestion budget. It states a fact and stops: what to do about it belongs to the tool
// catalogue, the same division NoCaptionStandIn keeps.
func longClipNote(a contract.Attachment, seconds float64) string {
	head := materialTag
	if name := prompt.SanitizeSpeaker(a.FileName); name != "" {
		head += " · " + name
	}
	return fmt.Sprintf("[%s]\n（时长 %s，未自动转写）", head, humanDuration(seconds))
}

// humanDuration renders seconds as "4 分 12 秒" / "48 秒" — for a human-facing note, not
// for parsing.
func humanDuration(seconds float64) string {
	s := int(seconds + 0.5)
	if s < 60 {
		return fmt.Sprintf("%d 秒", s)
	}
	return fmt.Sprintf("%d 分 %d 秒", s/60, s%60)
}

// sanitizeTranscript strips what could forge the prompt's meta structure — brackets and
// control characters, newlines included — while leaving the words themselves untouched.
func sanitizeTranscript(text string) string {
	text = strings.NewReplacer("[", "(", "]", ")", "\n", " ", "\r", " ").Replace(text)
	return strings.TrimSpace(prompt.StripControl(text))
}

// probeDuration reads a media file's length in seconds via ffprobe.
//
// Takes the SAME semaphore the transcode does. It is a second ffmpeg-family subprocess
// per inbound clip, and the semaphore's whole purpose is that a burst of messages cannot
// spawn an unbounded number of them — a probe that runs outside the gate reintroduces
// exactly the fan-out the gate exists to prevent, just earlier in the function.
func probeDuration(ctx context.Context, path string) (float64, error) {
	if path == "" {
		return 0, fmt.Errorf("asr: no local path")
	}
	select {
	case transcodeSem <- struct{}{}:
		defer func() { <-transcodeSem }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(pctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, fmt.Errorf("asr: ffprobe: %w", err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("asr: unreadable duration")
	}
	return d, nil
}

// isAudioAttachment reports whether an attachment is downloaded audio bob can transcribe:
// a voice note, an audio file, an audio/* document — or a VIDEO, whose soundtrack is
// audio like any other. Requires LocalPath — the staged file must exist on disk (this
// runs pre-placement).
//
// Video qualifies because transcodeAudio's ffmpeg call already picks the audio stream
// out of a container and drops the video (`-f flac` with no video mapping), so a clip
// needs no separate path. It is worth little on its own — a real 65 s disaster-scene
// clip yielded one shouted sentence, and most footage yields nothing — which is exactly
// why it belongs HERE rather than in the vision tool: free when there is speech, silent
// when there is not, and never something the picture answer waits on.
func isAudioAttachment(a contract.Attachment) bool {
	if a.LocalPath == "" {
		return false
	}
	switch a.Kind {
	case "voice", "audio", "video", "animation":
		return true
	case "document":
		mime := strings.ToLower(a.MIME)
		return strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/")
	}
	return false
}

// transcribeAudio transcodes one saved audio file to canonical flac and asks the pool's
// KindASR backend to transcribe it. Returns the parsed transcript text.
func transcribeAudio(ctx context.Context, pool contract.ModelPool, path string) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("asr: model pool not available")
	}
	if path == "" {
		return "", fmt.Errorf("asr: attachment was not downloaded (no local path)")
	}
	if st, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("asr: stat: %w", err)
	} else if st.Size() > transcribeMaxBytes {
		return "", fmt.Errorf("asr: audio too large (%d bytes > cap %d)", st.Size(), transcribeMaxBytes)
	}

	tctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()

	audio, err := transcodeAudio(tctx, path)
	if err != nil {
		return "", err
	}
	msgs := []contract.Message{{
		Role:  "user",
		Audio: []contract.AudioRef{{Data: audio, MIME: "audio/flac"}},
	}}
	resp, err := pool.Chat(tctx, contract.ModelRequest{Kind: contract.KindASR}, msgs)
	if err != nil {
		return "", fmt.Errorf("asr: chat: %w", err)
	}
	return parseASRTranscript(resp.Content), nil
}

// transcodeAudio decodes the source clip (ogg/opus/mp3/…) to canonical 16 kHz mono flac
// via ffmpeg (already in the image). Bounded by transcodeSem.
func transcodeAudio(ctx context.Context, srcPath string) ([]byte, error) {
	select {
	case transcodeSem <- struct{}{}:
		defer func() { <-transcodeSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out, err := os.CreateTemp("", "bob-asr-*.flac")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg transcode: temp file: %w", err)
	}
	outPath := out.Name()
	out.Close()
	defer os.Remove(outPath)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", srcPath,
		"-ar", "16000", "-ac", "1",
		"-f", "flac", outPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg transcode: %s", msg)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg transcode: read output: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("ffmpeg transcode: produced no output")
	}
	return data, nil
}

// parseASRTranscript extracts the transcript from an ASR reply. Some ASR models wrap
// their output as "…<asr_text><transcript></asr_text>"; strip the markers and trim.
func parseASRTranscript(raw string) string {
	const marker = "<asr_text>"
	if i := strings.Index(raw, marker); i >= 0 {
		raw = raw[i+len(marker):]
	}
	raw = strings.TrimSuffix(strings.TrimSpace(raw), "</asr_text>")
	return strings.TrimSpace(raw)
}
