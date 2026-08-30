package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	"gopkg.in/yaml.v3"

	"agentbob/contract"
)

// ErrInboxGone is returned by AddInboxSource / UpsertInboxSource when the target inbox
// no longer exists — the INSERT trips the inbox_id foreign key (SQLSTATE 23503). The
// only FK on that INSERT is inbox_id, so a 23503 there unambiguously means the inbox was
// deleted (e.g. /agora company delete cascaded it away). A distinct sentinel lets the
// agora-wire redeemer classify a token whose inbox is gone as TERMINAL — burn it —
// instead of endlessly telling the operator to retry a doomed code until TTL (F117).
var ErrInboxGone = errors.New("agora: target inbox no longer exists")

// isInboxFKViolation reports whether err is a Postgres foreign-key violation (23503).
// pgpool passes non-lock SQLSTATEs through untouched, so the underlying *pgconn.PgError
// is still reachable via errors.As here.
func isInboxFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// Inboxes + InboxSources CRUD over bob_agora_inboxes / bob_agora_inbox_sources.

const inboxSelect = `
SELECT id, kind, COALESCE(member_id, ''),
       COALESCE(owner_company_id, ''), scope, name, status,
       created_at, COALESCE(archived_at, 0), COALESCE(bridge_default_target_inbox_id, ''),
       validation_default, idle_timeout_seconds, finalize_grace_seconds, context,
       visibility_yaml
  FROM bob_agora_inboxes`

// CreateInbox inserts a new Inbox after validating its kind invariants. A member
// inbox requires a MemberID (it belongs to that member); a bridge inbox requires
// an OwnerCompanyID and must not carry a MemberID.
func (s *PG) CreateInbox(ctx context.Context, ib Inbox, nowUnix int64) (Inbox, error) {
	if err := validateInboxKind(ib); err != nil {
		return Inbox{}, err
	}
	ib.ID = newID("ib")
	ib.CreatedAt = nowUnix
	if ib.Kind == "" {
		ib.Kind = "member"
	}
	if ib.Status == "" {
		ib.Status = "active"
	}
	if ib.ValidationDefault == "" {
		ib.ValidationDefault = "auto"
	}
	if ib.IdleTimeoutSeconds == 0 {
		ib.IdleTimeoutSeconds = 86400
	}
	if ib.FinalizeGraceSeconds == 0 {
		ib.FinalizeGraceSeconds = 300
	}
	vis, err := marshalInboxVisibility(ib.Visibility)
	if err != nil {
		return Inbox{}, err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO bob_agora_inboxes
    (id, kind, member_id, owner_company_id, scope, name, status,
     created_at, bridge_default_target_inbox_id,
     validation_default, idle_timeout_seconds, finalize_grace_seconds, context,
     visibility_yaml)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		ib.ID, ib.Kind, nullStr(ib.MemberID),
		nullStr(ib.OwnerCompanyID), ib.Scope, ib.Name, ib.Status,
		ib.CreatedAt, nullStr(ib.BridgeDefaultTargetInboxID),
		ib.ValidationDefault, ib.IdleTimeoutSeconds, ib.FinalizeGraceSeconds, ib.Context,
		vis,
	)
	if err != nil {
		return Inbox{}, fmt.Errorf("agora: create inbox: %w", err)
	}
	return ib, nil
}

// GetInbox returns one Inbox by id (contract.ErrNoRows when absent).
func (s *PG) GetInbox(ctx context.Context, id string) (Inbox, error) {
	return scanInbox(s.db.QueryRow(ctx, inboxSelect+` WHERE id = $1`, id))
}

// GetInboxByScope returns one Inbox by scope (contract.ErrNoRows when absent).
func (s *PG) GetInboxByScope(ctx context.Context, scope string) (Inbox, error) {
	return scanInbox(s.db.QueryRow(ctx, inboxSelect+` WHERE scope = $1`, scope))
}

// ListInboxes returns every Inbox (any status), ordered by scope.
func (s *PG) ListInboxes(ctx context.Context) ([]Inbox, error) {
	rows, err := s.db.Query(ctx, inboxSelect+` ORDER BY scope ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInboxes(rows)
}

// ListInboxesByMember returns a Member's inboxes.
func (s *PG) ListInboxesByMember(ctx context.Context, memberID string) ([]Inbox, error) {
	rows, err := s.db.Query(ctx, inboxSelect+` WHERE member_id = $1 ORDER BY name ASC`, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInboxes(rows)
}

// ListInboxesByCompany returns a Company's owned (bridge) inboxes.
func (s *PG) ListInboxesByCompany(ctx context.Context, companyID string) ([]Inbox, error) {
	rows, err := s.db.Query(ctx, inboxSelect+` WHERE owner_company_id = $1 ORDER BY name ASC`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInboxes(rows)
}

// ArchiveInbox terminally archives an Inbox (stamps archived_at). Returns
// contract.ErrNoRows when id has no row.
func (s *PG) ArchiveInbox(ctx context.Context, id string, nowUnix int64) error {
	return s.affectOne(ctx, `
UPDATE bob_agora_inboxes
   SET status = 'archived', archived_at = $1
 WHERE id = $2`, nowUnix, id)
}

// UpdateInboxStatus flips an Inbox between "active" and "paused" (the operator
// kill switch for a single inbox — archived is terminal via ArchiveInbox).
// Returns contract.ErrNoRows when inboxID has no row.
func (s *PG) UpdateInboxStatus(ctx context.Context, inboxID, status string) error {
	switch status {
	case "active", "paused":
	default:
		return fmt.Errorf("agora: UpdateInboxStatus only supports active|paused, got %q", status)
	}
	if inboxID == "" {
		return fmt.Errorf("agora: UpdateInboxStatus requires non-empty inboxID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_inboxes SET status = $1 WHERE id = $2`, status, inboxID)
}

// SetInboxVisibility sets (vis non-nil) or clears (vis nil) the per-Inbox tool/skill
// HIDE denylist. nil writes SQL NULL ("nothing hidden"); a non-nil filter writes a
// yaml body. Returns contract.ErrNoRows when inboxID has no row. (The panel editor
// passes nil for an emptied editor, so the non-nil-but-empty case isn't produced.)
func (s *PG) SetInboxVisibility(ctx context.Context, inboxID string, vis *InboxVisibility) error {
	if inboxID == "" {
		return fmt.Errorf("agora: SetInboxVisibility requires non-empty inboxID")
	}
	body, err := marshalInboxVisibility(vis)
	if err != nil {
		return err
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_inboxes SET visibility_yaml = $1 WHERE id = $2`, body, inboxID)
}

// SetBridgeDefaultTarget points a bridge inbox at its default downstream inbox
// ("" clears it). Returns contract.ErrNoRows when bridgeInboxID has no row.
func (s *PG) SetBridgeDefaultTarget(ctx context.Context, bridgeInboxID, targetInboxID string) error {
	if bridgeInboxID == "" {
		return fmt.Errorf("agora: SetBridgeDefaultTarget requires non-empty bridgeInboxID")
	}
	return s.affectOne(ctx,
		`UPDATE bob_agora_inboxes SET bridge_default_target_inbox_id = $1 WHERE id = $2`,
		nullStr(targetInboxID), bridgeInboxID)
}

// AddInboxSource binds an external chat (source + chat type + chat id + optional
// thread) onto an Inbox. The UNIQUE (source_id, chat_type, chat_id, thread_id)
// index rejects duplicate wirings.
func (s *PG) AddInboxSource(ctx context.Context, src InboxSource, nowUnix int64) (InboxSource, error) {
	if src.InboxID == "" || src.SourceID == "" {
		return InboxSource{}, fmt.Errorf("agora: InboxSource.InboxID + SourceID are required")
	}
	src.ID = newID("is")
	src.CreatedAt = nowUnix
	_, err := s.db.Exec(ctx, `
INSERT INTO bob_agora_inbox_sources
    (id, inbox_id, source_id, chat_type, chat_id, thread_id, outgoing_credential_ref, is_default, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		src.ID, src.InboxID, src.SourceID, string(src.ChatType), src.ChatID, src.ThreadID,
		nullStr(src.OutgoingCredentialRef), src.IsDefault, src.CreatedAt,
	)
	if err != nil {
		if isInboxFKViolation(err) {
			return InboxSource{}, ErrInboxGone
		}
		return InboxSource{}, fmt.Errorf("agora: add inbox source: %w", err)
	}
	return src, nil
}

// UpsertInboxSource is AddInboxSource but re-points an existing (source_id,
// chat_type, chat_id, thread_id) row to a new inbox instead of failing on the
// UNIQUE constraint. On a re-point the returned row carries the PRE-EXISTING id
// + created_at (via RETURNING) so the caller sees the stable row.
func (s *PG) UpsertInboxSource(ctx context.Context, src InboxSource, nowUnix int64) (InboxSource, error) {
	if src.InboxID == "" || src.SourceID == "" {
		return InboxSource{}, fmt.Errorf("agora: InboxSource.InboxID + SourceID are required")
	}
	src.ID = newID("is")
	src.CreatedAt = nowUnix
	// DM = one chat → one inbox: re-point on the DM partial unique (preserve credential).
	// GROUP = a chat may bind MANY member inboxes: idempotent per (inbox, chat) on the
	// per-binding unique (re-wiring the SAME member to the SAME group is a no-op update).
	const credUpd = `outgoing_credential_ref = COALESCE(EXCLUDED.outgoing_credential_ref, bob_agora_inbox_sources.outgoing_credential_ref), is_default = EXCLUDED.is_default`
	conflict := `(inbox_id, source_id, chat_type, chat_id, thread_id) DO UPDATE SET ` + credUpd
	if src.ChatType == contract.ChatDM {
		conflict = `(source_id, chat_id, thread_id) WHERE chat_type = 'dm' DO UPDATE SET inbox_id = EXCLUDED.inbox_id, ` + credUpd
	}
	var chatType string
	err := s.db.QueryRow(ctx, `
INSERT INTO bob_agora_inbox_sources
    (id, inbox_id, source_id, chat_type, chat_id, thread_id, outgoing_credential_ref, is_default, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT `+conflict+`
RETURNING id, inbox_id, source_id, chat_type, chat_id, thread_id, is_default, created_at`,
		src.ID, src.InboxID, src.SourceID, string(src.ChatType), src.ChatID, src.ThreadID,
		nullStr(src.OutgoingCredentialRef), src.IsDefault, src.CreatedAt,
	).Scan(&src.ID, &src.InboxID, &src.SourceID, &chatType, &src.ChatID, &src.ThreadID, &src.IsDefault, &src.CreatedAt)
	if err != nil {
		if isInboxFKViolation(err) {
			return InboxSource{}, ErrInboxGone
		}
		return InboxSource{}, fmt.Errorf("agora: upsert inbox source: %w", err)
	}
	src.ChatType = contract.ChatType(chatType)
	return src, nil
}

// RemoveInboxSource deletes an inbox source by id. Returns contract.ErrNoRows
// when no row matched.
func (s *PG) RemoveInboxSource(ctx context.Context, id string) error {
	return s.affectOne(ctx, `DELETE FROM bob_agora_inbox_sources WHERE id = $1`, id)
}

// ListInboxSources returns the sources for one inbox (inboxID="" → all).
func (s *PG) ListInboxSources(ctx context.Context, inboxID string) ([]InboxSource, error) {
	const sel = `
SELECT id, inbox_id, source_id, chat_type, chat_id, thread_id, COALESCE(outgoing_credential_ref, ''), is_default, created_at
  FROM bob_agora_inbox_sources`
	var (
		rows contract.Rows
		err  error
	)
	if inboxID != "" {
		rows, err = s.db.Query(ctx, sel+` WHERE inbox_id = $1 ORDER BY source_id ASC`, inboxID)
	} else {
		rows, err = s.db.Query(ctx, sel+` ORDER BY inbox_id ASC, source_id ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxSource
	for rows.Next() {
		var (
			r        InboxSource
			chatType string
		)
		if err := rows.Scan(&r.ID, &r.InboxID, &r.SourceID, &chatType, &r.ChatID, &r.ThreadID, &r.OutgoingCredentialRef, &r.IsDefault, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("agora: scan inbox source: %w", err)
		}
		r.ChatType = contract.ChatType(chatType)
		out = append(out, r)
	}
	return out, rows.Err()
}

func validateInboxKind(ib Inbox) error {
	kind := ib.Kind
	if kind == "" {
		kind = "member"
	}
	switch kind {
	case "member":
		// a member inbox belongs to a Member (MemberID set); it persists across
		// the member's companies.
		if ib.MemberID == "" {
			return fmt.Errorf("agora: inbox kind='member' requires MemberID")
		}
	case "bridge":
		if ib.OwnerCompanyID == "" {
			return fmt.Errorf("agora: inbox kind='bridge' requires OwnerCompanyID")
		}
		if ib.MemberID != "" {
			return fmt.Errorf("agora: inbox kind='bridge' must not have MemberID")
		}
	default:
		return fmt.Errorf("agora: inbox kind must be 'member' or 'bridge', got %q", kind)
	}
	return nil
}

func scanInbox(r contract.Row) (Inbox, error) {
	var (
		out   Inbox
		visNl *string // NULL → nil (no filter); non-NULL → yaml body
	)
	err := r.Scan(&out.ID, &out.Kind, &out.MemberID, &out.OwnerCompanyID,
		&out.Scope, &out.Name, &out.Status,
		&out.CreatedAt, &out.ArchivedAt, &out.BridgeDefaultTargetInboxID,
		&out.ValidationDefault, &out.IdleTimeoutSeconds, &out.FinalizeGraceSeconds, &out.Context,
		&visNl)
	if errors.Is(err, contract.ErrNoRows) {
		return Inbox{}, err
	}
	if err != nil {
		return Inbox{}, fmt.Errorf("agora: scan inbox: %w", err)
	}
	if visNl != nil {
		vis, perr := unmarshalInboxVisibility(*visNl)
		if perr != nil {
			// Malformed operator-edited yaml: surface loudly but degrade this row to
			// an EMPTY denylist (nothing hidden) rather than failing the whole scan (a
			// single bad inbox would otherwise strand OrgCache.Load). Visibility only
			// NARROWS on top of grants and the store can't enumerate "hide all" here, so
			// a corrupt hide-filter degrades to no narrowing — the GRANT gate still
			// protects; the operator fixes the yaml after the WARN.
			slog.Warn("agora: unparseable inbox.visibility_yaml — degrading to empty denylist (nothing hidden; grants still gate)",
				"inbox_id", out.ID, "err", perr)
			out.Visibility = &InboxVisibility{}
		} else {
			out.Visibility = vis
		}
	}
	return out, nil
}

func scanInboxes(rows contract.Rows) ([]Inbox, error) {
	var out []Inbox
	for rows.Next() {
		ib, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ib)
	}
	return out, rows.Err()
}

// marshalInboxVisibility yaml-encodes vis (the HIDE denylist) for the
// visibility_yaml column. nil → SQL NULL ("nothing hidden"); non-nil → marshalled
// body. With a denylist, NULL and an empty body both mean "nothing hidden" — the
// distinction no longer carries a security semantic (it's just NULL-vs-"{}").
func marshalInboxVisibility(vis *InboxVisibility) (any, error) {
	if vis == nil {
		return nil, nil
	}
	body, err := yaml.Marshal(vis)
	if err != nil {
		return nil, fmt.Errorf("agora: marshal inbox visibility: %w", err)
	}
	return string(body), nil
}

// unmarshalInboxVisibility decodes a visibility_yaml body into a non-nil
// *InboxVisibility. Empty body parses to a zero-value (nil HiddenTools/HiddenSkills)
// = nothing hidden (a denylist; empty hides nothing).
func unmarshalInboxVisibility(body string) (*InboxVisibility, error) {
	out := &InboxVisibility{}
	if body == "" {
		return out, nil
	}
	if err := yaml.Unmarshal([]byte(body), out); err != nil {
		return nil, err
	}
	return out, nil
}
