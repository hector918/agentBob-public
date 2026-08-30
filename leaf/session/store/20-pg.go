package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// PG is the Postgres implementation of Store over the shared contract.DB pool.
type PG struct {
	db contract.DB
}

// NewPG returns a session store over db.
func NewPG(db contract.DB) *PG { return &PG{db: db} }

// now returns epoch-float seconds on the DB-calibrated clock, the time unit every
// session timestamp uses. (newID keeps a raw local nanos for entropy — not a
// recorded instant, so it needs no calibration.)
func now() float64 { return clock.UnixSeconds() }

// newID mints a session id: prefix + base36 nanos + 4 random bytes.
func newID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + hex.EncodeToString(b[:])
}

// Ping verifies the store is reachable.
func (s *PG) Ping(ctx context.Context) error {
	var x int
	return s.db.QueryRow(ctx, "SELECT 1").Scan(&x)
}

var _ Store = (*PG)(nil)
