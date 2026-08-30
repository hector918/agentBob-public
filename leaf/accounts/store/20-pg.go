package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/heartwood/clock"
)

// PG is the Postgres implementation of Store over the shared contract.DB pool.
type PG struct {
	db contract.DB
}

// NewPG returns an accounts store over db.
func NewPG(db contract.DB) *PG { return &PG{db: db} }

// newID mints an id: prefix + base36 nanos + 4 random bytes.
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

// CreateAccount inserts an account entitled to "normal". flow is written EXPLICITLY
// from the returned struct (single truth), not left to the schema column DEFAULT —
// the two could otherwise drift silently. Mirrors CreateBareAccount's explicit write.
func (s *PG) CreateAccount(ctx context.Context, displayName, note string, now int64) (Account, error) {
	a := Account{
		ID:          newID("ac"),
		DisplayName: displayName,
		Note:        note,
		Flow:        "normal",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_accounts (id, display_name, note, flow, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		a.ID, a.DisplayName, a.Note, a.Flow, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return Account{}, fmt.Errorf("CreateAccount: %w", err)
	}
	return a, nil
}

// CreateBareAccount inserts an account with flow=” (NO entitlement) + onboard_scope set.
// flow is set EXPLICITLY (not the column default 'normal') so a bare onboarding account
// has no access until an admin grants it.
func (s *PG) CreateBareAccount(ctx context.Context, displayName, onboardScope string, now int64) (Account, error) {
	a := Account{
		ID:           newID("ac"),
		DisplayName:  displayName,
		Flow:         "",
		OnboardScope: onboardScope,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_accounts (id, display_name, note, flow, onboard_scope, created_at, updated_at)
		 VALUES ($1, $2, '', '', $3, $4, $5)`,
		a.ID, a.DisplayName, a.OnboardScope, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return Account{}, fmt.Errorf("CreateBareAccount: %w", err)
	}
	return a, nil
}

// DeleteAccount removes an account row by id. Used to clean up an orphan when a
// bind fails right after CreateAccount (no handle exists yet, so the delete is
// clean). Deleting an absent id affects 0 rows and is not an error.
func (s *PG) DeleteAccount(ctx context.Context, id string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM bob_accounts WHERE id = $1`, id); err != nil {
		return fmt.Errorf("DeleteAccount: %w", err)
	}
	return nil
}

func (s *PG) GetAccount(ctx context.Context, id string) (Account, error) {
	var a Account
	err := s.db.QueryRow(ctx,
		`SELECT id, display_name, note, flow, status, onboard_scope, created_at, updated_at FROM bob_accounts WHERE id = $1`,
		id,
	).Scan(&a.ID, &a.DisplayName, &a.Note, &a.Flow, &a.Status, &a.OnboardScope, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, contract.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("GetAccount: %w", err)
	}
	return a, nil
}

func (s *PG) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, display_name, note, flow, status, onboard_scope, created_at, updated_at FROM bob_accounts ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListAccounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.Note, &a.Flow, &a.Status, &a.OnboardScope, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("ListAccounts scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListAccounts rows.Err: %w", err)
	}
	return out, nil
}

func (s *PG) AccountRosterPage(ctx context.Context, limit, offset int) ([]Account, int64, error) {
	if limit < 1 {
		limit = 1
	}
	if offset < 0 {
		offset = 0
	}
	var total int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM bob_accounts`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("AccountRosterPage count: %w", err)
	}
	rows, err := s.db.Query(ctx,
		`SELECT id, display_name, note, flow, status, onboard_scope, created_at, updated_at FROM bob_accounts
		 ORDER BY created_at DESC, id ASC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("AccountRosterPage: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.Note, &a.Flow, &a.Status, &a.OnboardScope, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("AccountRosterPage scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("AccountRosterPage rows.Err: %w", err)
	}
	return out, total, nil
}

func (s *PG) SetAccountFlow(ctx context.Context, accountID, flow string, now int64) error {
	res, err := s.db.Exec(ctx,
		`UPDATE bob_accounts SET flow = $1, updated_at = $2 WHERE id = $3`, flow, now, accountID)
	if err != nil {
		return fmt.Errorf("SetAccountFlow: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAccountNotFound
	}
	return nil
}

func (s *PG) SetAccountStatus(ctx context.Context, accountID, status string, now int64) error {
	res, err := s.db.Exec(ctx,
		`UPDATE bob_accounts SET status = $1, updated_at = $2 WHERE id = $3`, status, now, accountID)
	if err != nil {
		return fmt.Errorf("SetAccountStatus: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAccountNotFound
	}
	return nil
}

func (s *PG) BindHandle(ctx context.Context, h SourceHandle, now int64) (SourceHandle, error) {
	if h.AccountID == "" || h.Source == "" || h.PlatformUID == "" {
		return SourceHandle{}, fmt.Errorf("BindHandle: AccountID + Source + PlatformUID are required")
	}
	h.ID = newID("sh")
	h.BoundAt = now
	var out SourceHandle
	err := s.db.QueryRow(ctx,
		`INSERT INTO bob_account_handles (id, account_id, source, platform_uid, bound_source, access_uid, display_name, bound_at, bound_via)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (source, platform_uid) DO UPDATE SET
		     account_id   = EXCLUDED.account_id,
		     bound_source = EXCLUDED.bound_source,
		     access_uid   = EXCLUDED.access_uid,
		     display_name = EXCLUDED.display_name,
		     bound_at     = EXCLUDED.bound_at,
		     bound_via    = EXCLUDED.bound_via
		 RETURNING id, account_id, source, platform_uid, bound_source, access_uid, display_name, bound_at, bound_via`,
		h.ID, h.AccountID, h.Source, h.PlatformUID, h.BoundSource, h.AccessUID, h.DisplayName, h.BoundAt, h.BoundVia,
	).Scan(&out.ID, &out.AccountID, &out.Source, &out.PlatformUID, &out.BoundSource, &out.AccessUID, &out.DisplayName, &out.BoundAt, &out.BoundVia)
	if err != nil {
		return SourceHandle{}, fmt.Errorf("BindHandle: %w", err)
	}
	return out, nil
}

func (s *PG) HandlesForAccount(ctx context.Context, accountID string) ([]SourceHandle, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, account_id, source, platform_uid, bound_source, access_uid, display_name, bound_at, bound_via
		 FROM bob_account_handles WHERE account_id = $1 ORDER BY bound_at ASC, id ASC`,
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("HandlesForAccount: %w", err)
	}
	defer rows.Close()
	var out []SourceHandle
	for rows.Next() {
		var h SourceHandle
		if err := rows.Scan(&h.ID, &h.AccountID, &h.Source, &h.PlatformUID, &h.BoundSource, &h.AccessUID, &h.DisplayName, &h.BoundAt, &h.BoundVia); err != nil {
			return nil, fmt.Errorf("HandlesForAccount scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("HandlesForAccount rows.Err: %w", err)
	}
	return out, nil
}

func (s *PG) HandleCountsByAccount(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.Query(ctx,
		`SELECT account_id, count(*) FROM bob_account_handles GROUP BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("HandleCountsByAccount: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("HandleCountsByAccount scan: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("HandleCountsByAccount rows.Err: %w", err)
	}
	return out, nil
}

func (s *PG) HandleBySourceUID(ctx context.Context, source, platformUID string) (SourceHandle, bool, error) {
	var h SourceHandle
	err := s.db.QueryRow(ctx,
		`SELECT id, account_id, source, platform_uid, bound_source, access_uid, display_name, bound_at, bound_via
		 FROM bob_account_handles WHERE source = $1 AND platform_uid = $2`,
		source, platformUID,
	).Scan(&h.ID, &h.AccountID, &h.Source, &h.PlatformUID, &h.BoundSource, &h.AccessUID, &h.DisplayName, &h.BoundAt, &h.BoundVia)
	if errors.Is(err, contract.ErrNoRows) {
		return SourceHandle{}, false, nil
	}
	if err != nil {
		return SourceHandle{}, false, fmt.Errorf("HandleBySourceUID: %w", err)
	}
	return h, true, nil
}

func (s *PG) AddTurnUsage(ctx context.Context, source, platformUID string, turns, success int64, tokens map[string]KindTokens, native map[string]int64) error {
	var blob string
	err := s.db.QueryRow(ctx,
		`SELECT usage_json FROM bob_account_handle_usage WHERE source = $1 AND platform_uid = $2`,
		source, platformUID,
	).Scan(&blob)
	if err != nil && !errors.Is(err, contract.ErrNoRows) {
		return fmt.Errorf("AddTurnUsage read: %w", err)
	}
	next, berr := BumpTurnUsage(blob, turns, success, tokens, native, clock.Now())
	if berr != nil {
		return fmt.Errorf("AddTurnUsage bump: %w", berr)
	}
	if _, err := s.db.Exec(ctx,
		`INSERT INTO bob_account_handle_usage (source, platform_uid, usage_json) VALUES ($1, $2, $3)
		 ON CONFLICT (source, platform_uid) DO UPDATE SET usage_json = EXCLUDED.usage_json`,
		source, platformUID, next,
	); err != nil {
		return fmt.Errorf("AddTurnUsage write: %w", err)
	}
	return nil
}

// GetHandleLang returns the sender's remembered reply language, or "" when none.
func (s *PG) GetHandleLang(ctx context.Context, source, platformUID string) (string, error) {
	var lang string
	err := s.db.QueryRow(ctx,
		`SELECT lang FROM bob_account_handle_usage WHERE source = $1 AND platform_uid = $2`,
		source, platformUID).Scan(&lang)
	if errors.Is(err, contract.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetHandleLang %s/%s: %w", source, platformUID, err)
	}
	return lang, nil
}

// SetHandleLangIfEmpty seeds the sender's reply language only when none is set yet
// (the WHERE on the conflict update makes it seed-once → the seed stays stable).
func (s *PG) SetHandleLangIfEmpty(ctx context.Context, source, platformUID, lang string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_account_handle_usage (source, platform_uid, lang) VALUES ($1, $2, $3)
		 ON CONFLICT (source, platform_uid) DO UPDATE SET lang = EXCLUDED.lang
		   WHERE bob_account_handle_usage.lang = ''`,
		source, platformUID, lang)
	if err != nil {
		return fmt.Errorf("SetHandleLangIfEmpty %s/%s: %w", source, platformUID, err)
	}
	return nil
}

func (s *PG) AccountUsageBreakdown(ctx context.Context, accountID string) (AccountUsage, error) {
	u := AccountUsage{TokensByKind: map[string]KindTokens{}, Native: map[string]int64{}}
	rows, err := s.db.Query(ctx,
		`SELECT u.usage_json FROM bob_account_handle_usage u
		 JOIN bob_account_handles h ON h.source = u.source AND h.platform_uid = u.platform_uid
		 WHERE h.account_id = $1`,
		accountID,
	)
	if err != nil {
		return u, fmt.Errorf("AccountUsageBreakdown: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return u, fmt.Errorf("AccountUsageBreakdown scan: %w", err)
		}
		u.AddBlob(blob)
	}
	if err := rows.Err(); err != nil {
		return u, fmt.Errorf("AccountUsageBreakdown: %w", err)
	}
	return u, nil
}

// csvJoin renders a string slice into the CSV column form (blanks dropped).
func csvJoin(xs []string) string {
	kept := make([]string, 0, len(xs))
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			kept = append(kept, x)
		}
	}
	return strings.Join(kept, ",")
}

// csvSplit parses a CSV column back into a slice (blanks dropped). nil for "".
func csvSplit(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// CreateAPIKey inserts a key row (status 'active'). The plaintext token is the
// caller's to hold once — only secretHash lands here. k.Kinds/k.Models are
// serialized to their CSV columns.
func (s *PG) CreateAPIKey(ctx context.Context, k APIKey, secretHash string, now int64) (APIKey, error) {
	if k.AccountID == "" || secretHash == "" {
		return APIKey{}, fmt.Errorf("CreateAPIKey: AccountID + secretHash are required")
	}
	k.ID = newID("ak")
	k.Status = "active"
	k.CreatedAt = now
	k.LastUsedAt = 0
	// The dead pre-v8 columns (tags / prefer_tags / kind / max_inflight) are omitted
	// — they are NOT NULL DEFAULT '' / 0, so the row fills them itself.
	_, err := s.db.Exec(ctx,
		`INSERT INTO bob_account_apikeys (id, account_id, secret_hash, label, kinds, models, status, created_at, last_used_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, 0)`,
		k.ID, k.AccountID, secretHash, k.Label, csvJoin(k.Kinds), csvJoin(k.Models), k.CreatedAt,
	)
	if err != nil {
		return APIKey{}, fmt.Errorf("CreateAPIKey: %w", err)
	}
	return k, nil
}

// APIKeyByHash resolves an ACTIVE key whose owning account is ACTIVE. A revoked
// key, a paused account, or an unknown hash all return ok=false indistinguishably
// (a caller can't probe which). The account-status gate is a JOIN so a paused
// account instantly disables all its keys with no extra round-trip.
func (s *PG) APIKeyByHash(ctx context.Context, secretHash string) (APIKey, bool, error) {
	var k APIKey
	var kinds, models string
	err := s.db.QueryRow(ctx,
		`SELECT k.id, k.account_id, k.label, k.kinds, k.models, k.status, k.created_at, k.last_used_at
		 FROM bob_account_apikeys k
		 JOIN bob_accounts a ON a.id = k.account_id
		 WHERE k.secret_hash = $1 AND k.status = 'active' AND a.status <> 'paused'`,
		secretHash,
	).Scan(&k.ID, &k.AccountID, &k.Label, &kinds, &models, &k.Status, &k.CreatedAt, &k.LastUsedAt)
	if errors.Is(err, contract.ErrNoRows) {
		return APIKey{}, false, nil
	}
	if err != nil {
		return APIKey{}, false, fmt.Errorf("APIKeyByHash: %w", err)
	}
	k.Kinds, k.Models = csvSplit(kinds), csvSplit(models)
	return k, true, nil
}

// TouchAPIKey bumps last_used_at. Best-effort — the caller ignores the error.
func (s *PG) TouchAPIKey(ctx context.Context, id string, now int64) error {
	if _, err := s.db.Exec(ctx,
		`UPDATE bob_account_apikeys SET last_used_at = $1 WHERE id = $2`, now, id); err != nil {
		return fmt.Errorf("TouchAPIKey: %w", err)
	}
	return nil
}

// ListAPIKeys returns an account's keys newest-first (or ALL keys when accountID
// is empty — the admin roster). NEVER returns the secret hash.
func (s *PG) ListAPIKeys(ctx context.Context, accountID string) ([]APIKey, error) {
	q := `SELECT id, account_id, label, kinds, models, status, created_at, last_used_at
	      FROM bob_account_apikeys`
	var rows contract.Rows
	var err error
	if accountID == "" {
		rows, err = s.db.Query(ctx, q+` ORDER BY created_at DESC, id ASC`)
	} else {
		rows, err = s.db.Query(ctx, q+` WHERE account_id = $1 ORDER BY created_at DESC, id ASC`, accountID)
	}
	if err != nil {
		return nil, fmt.Errorf("ListAPIKeys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var kinds, models string
		if err := rows.Scan(&k.ID, &k.AccountID, &k.Label, &kinds, &models, &k.Status, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, fmt.Errorf("ListAPIKeys scan: %w", err)
		}
		k.Kinds, k.Models = csvSplit(kinds), csvSplit(models)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListAPIKeys rows.Err: %w", err)
	}
	return out, nil
}

// RevokeAPIKey flips a key to 'revoked' (the row stays — historical usage under
// its bound handle must remain readable, and the hash stays reserved). A no-op
// affecting 0 rows for an absent id returns ErrAPIKeyNotFound.
func (s *PG) RevokeAPIKey(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `UPDATE bob_account_apikeys SET status = 'revoked' WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("RevokeAPIKey: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAPIKeyNotFound
	}
	return nil
}

var _ Store = (*PG)(nil)
