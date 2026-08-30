package core

import "errors"

// Sentinel errors classify backend-native errors into impl-agnostic
// categories so callers (and FallbackStore) can react without knowing
// which backend produced the error.
//
// Each SessionStore impl wraps native errors with one of these via
// fmt.Errorf("%w: ...", core.ErrStoreXxx). Callers check with
// errors.Is(err, core.ErrStoreXxx).
//
// See docs/store-dual-backend.md §"Error sentinels" for the full
// design + D10 for the failover-trigger contract.
var (
	// ErrStoreConnLost — backend connection / process / network is
	// gone. The ONLY sentinel that triggers FallbackStore failover.
	// sqlite: rare (local file IO error). pg: connection_failure
	// (SQLSTATE 08006/08001) / dial timeout / TLS error / server
	// shutdown.
	ErrStoreConnLost = errors.New("store: connection lost")

	// ErrStoreLocked — transient SQL contention. Callers should
	// retry, NOT failover. sqlite: SQLITE_BUSY / "database is locked".
	// pg: serialization_failure (40001) / deadlock_detected (40P01).
	ErrStoreLocked = errors.New("store: lock/contention (transient)")

	// ErrStoreDiskFull — no space left on storage. Operator-fix
	// territory; no failover (pg full → sqlite likely also full).
	// sqlite: ENOSPC bubbled up. pg: disk_full (53100).
	ErrStoreDiskFull = errors.New("store: no space left on device")

	// ErrStoreReadOnly — backend in read-only state. Likely
	// misconfiguration / filesystem mode. Caller-visible error.
	ErrStoreReadOnly = errors.New("store: backend is read-only")

	// ErrStoreCorrupt — backend integrity broken (DB file damaged /
	// schema mismatch / FK violation that shouldn't be possible).
	// Fatal-class — operator must intervene; bob does NOT failover
	// (silently routing around corruption hides the root cause).
	ErrStoreCorrupt = errors.New("store: backend integrity issue")

	// ErrStoreUniqueViolation — duplicate PK / unique-index hit. Used
	// by callbacks.Manager.Register to retry id generation on collision
	// (3-byte id space => birthday-bound ~3×10⁻⁴ at 100 concurrent live
	// registrations). sqlite: "UNIQUE constraint failed" /
	// "constraint failed". pg: unique_violation (SQLSTATE 23505).
	ErrStoreUniqueViolation = errors.New("store: unique constraint violation")
)
