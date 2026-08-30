// Package store is the agora module's postgres persistence: the self-managed
// `bob_agora_*` entity tables (Members / Companies / Employments / Inboxes /
// InboxSources) and their CRUD.
//
// Per docs/agora-port.md §3 + §4 boundary A this package owns the agora data
// model — the entity structs (20-entities.go) live here, not in contract. It
// speaks ONLY contract.DB for SQL (no database/sql import; the "it is Postgres"
// fact stops at pgpool + this file) and uses contract.ErrNoRows for not-found,
// mirroring leaf/session/store and leaf/session/messages.
//
// Trunk pg is mandatory (pgpool is non-optional), so the skeleton's
// "sqlite→agora absent" backend-type gating is gone: agora is a plain module
// that self-manages pg tables. Schema is owned by 10-migrate.go (wipe-on-bump).
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"agentbob/contract"
)

// PG is the agora entity store over a shared contract.DB pool.
type PG struct{ db contract.DB }

// NewPG returns an agora store over db. Call Migrate first to ensure the tables
// exist.
func NewPG(db contract.DB) *PG { return &PG{db: db} }

// newID generates a prefixed unique id (same shape as the other stores' ids).
func newID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + hex.EncodeToString(b[:])
}

// nullStr maps "" → SQL NULL so nullable TEXT columns stay NULL rather than
// storing an empty string (lets COALESCE / NULL checks behave).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// marshalAnyMap JSON-encodes a map[string]any with nil→`{}` normalisation.
func marshalAnyMap(v map[string]any) (string, error) {
	if v == nil {
		v = map[string]any{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("agora: marshal map: %w", err)
	}
	return string(b), nil
}
