// Package store is the arrangement module's postgres persistence: the arrangement
// DEFINITIONS (bob_arrangement_defs — validated, frozen-once-started) and the live
// work ITEMS (bob_arrangement_items — the per-role priority buckets). Two tables:
// the def is immutable while running (no concurrency), the items move (per-item
// atomic claim), so neither needs blob-level optimistic locking.
//
// Speaks ONLY contract.DB for SQL; uses contract.ErrNoRows for not-found.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"agentbob/contract"
)

// PG is the arrangement store over a shared contract.DB pool.
type PG struct{ db contract.DB }

// NewPG returns an arrangement store. Call Migrate first.
func NewPG(db contract.DB) *PG { return &PG{db: db} }

func newID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + hex.EncodeToString(b[:])
}
