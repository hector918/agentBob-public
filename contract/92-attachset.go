package contract

import "context"

// AttachmentSet is the turn's attachment bag, handed to tools as a capability (the
// sibling of ChannelOpener / CredentialOpener). The FLOW builds it — today over THIS
// turn's batch (with a space-inbox fallback for an earlier turn's file); later a
// session-backed variant over persisted history can drop in behind the same interface,
// with no tool change. It replaces the per-tool image / epub loaders: a tool supplies
// its APPETITE (a predicate) and the set does the resolution.
//
// The model is attachment-blind for actuation — it can only NAME a file (the path the
// prompt showed it). So the set is the model-blind anchor: it maps a model-supplied
// reference onto a REAL attachment's bytes, never a path the model invented.
type AttachmentSet interface {
	// Pick resolves which attachment(s) the model meant for a tool whose appetite is
	// `want`. hint is the model's reference (an image_path / file_name arg):
	//   - hint names a batch attachment of ANY kind → that one (so the caller can tell
	//     "named a non-image" from "named nothing"; the caller gates on want);
	//   - else a non-empty hint names an earlier-turn file in the space inbox → that one
	//     (name lookups run BEFORE any batch fallback: a hint that misses the batch but
	//     resolves in the inbox is honored, never silently swapped for this turn's file);
	//   - else the batch's want-matches: exactly one → that one; several → all of them
	//     (the caller asks the model to name one);
	//   - else nothing resolved → empty (the caller builds a "which did you mean" error
	//     from Suggest).
	// Returns 0 / 1 / many. A resolved attachment may carry Path=="" when the hint
	// named a batch attachment that was never downloaded/placed (oversized, or
	// capture disabled) — callers must gate on Path before Read and answer with a
	// "not ready" error, not assume a readable file (see leaf/tools/95-image.go).
	Pick(ctx context.Context, want func(Attachment) bool, hint string) []Attachment

	// Suggest lists the want-matching attachments the model COULD name — this turn's
	// batch plus recent earlier-turn files in the space inbox — for a "no match, did you
	// mean" error. It never resolves a subject; it only enumerates candidates.
	Suggest(ctx context.Context, want func(Attachment) bool) []Attachment

	// Read loads one attachment's bytes through the bound FileChannel, capped at max
	// bytes (0 = a sane default). a must be one Pick / Suggest returned.
	Read(ctx context.Context, a Attachment, max int64) ([]byte, error)
}
