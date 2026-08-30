package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// Credentials: central broker for every external secret the agent uses
// (model API keys / IM platform tokens / store middleware DSNs / future
// skill third-party tokens). Design: docs/credentials-design.md.
//
// Two sentinel errors + the public types live here; the dispatch entry
// points + baseline impl live in the credentials/ package (always
// linked), and the rich yaml-backed impl lives in credentials-vault/
// (optional plugin, deletable, registers itself at startup).
//
// Mirrors the permissions baseline+rich split — same "broker 删掉后
// caller 回退到 baseline" reducibility property.

// ErrCredentialNotFound is the sentinel returned when a target /
// credential name isn't registered with the broker. Callers MUST
// errors.Is-check this (don't string-match) so the rich plugin can
// wrap the message with more context without breaking the contract.
var ErrCredentialNotFound = errors.New("credentials: not found")

// ErrCredentialDenied is the sentinel returned by the rich vault's
// permission gate when a credential target exists (or might) but the
// ctx-identified caller lacks the credential:use:<target> grant. It is
// DISTINCT from ErrCredentialNotFound so audit classification can tell a
// security-relevant permission refusal apart from a routine routing miss
// (decisionFromErr maps it to decision="deny", not "miss").
//
// The denial error returned by checkPermission wraps BOTH this sentinel
// AND ErrCredentialNotFound, so the long-standing caller contract — a
// single errors.Is(err, ErrCredentialNotFound) catches "no file" AND "no
// permission" (pgresolve env-DSN fall-through, agora, model wake) — keeps
// working unchanged, while the audit classifier matches ErrCredentialDenied
// FIRST to emit "deny".
var ErrCredentialDenied = errors.New("credentials: denied by permissions")

// ErrCredentialReadOnly is returned by Set / Delete on the baseline
// (envvar-only) impl. The baseline can't write back to .env from a
// running process — admin must edit .env + restart, or install the
// rich vault plugin which writes ~/.bob/credentials/<name>.yaml.
var ErrCredentialReadOnly = errors.New("credentials: baseline is read-only (install credentials-vault to write)")

// OutgoingTarget is the non-secret outbound configuration for a
// credential — used by source-telegram outbound + agora bridge inbox to
// pick which chat / channel to deliver into, WITHOUT touching the
// secret. Separate from HTTPClient/PGConn because the consumer is the
// destination router (which doesn't need auth — the source already has
// its own bot client) rather than the auth layer.
//
// Today carries chat-id-pool style data. Future outbound kinds (email
// distribution list, slack channel id, webhook URL) get their own
// typed fields here rather than a generic data bag — keeps the
// destination router's branching explicit at the type level.
type OutgoingTarget struct {
	// Name is the credential name this target belongs to (e.g.
	// "telegram_marketing_bot"). Same string the broker uses for
	// HTTPClient lookups.
	Name string

	// ChatIDPool is the round-robin pool of destination chat ids. First
	// non-empty entry wins for now; M5+ adds round-robin / health-aware
	// selection.
	ChatIDPool []string

	// DefaultChat is the single fallback chat id when ChatIDPool is empty.
	// Either-or shape: callers should consult ChatIDPool first, then fall
	// back to DefaultChat.
	DefaultChat string
}

// CredentialMeta is what callers get from List() — name / kind /
// timestamps only. The secret bytes are NEVER part of this struct;
// the broker contract is "caller asks for a configured client, broker
// returns a client, raw secrets never cross the boundary".
type CredentialMeta struct {
	// Name is the credential's stable identifier (also the yaml file
	// stem under ~/.bob/credentials/, and the action segment under
	// `credential:use:<name>` once permissions integration ships in C5).
	Name string

	// Kind is a free-form classifier ("api_key" / "dsn" / "token" /
	// "browser_data" / ...). Mostly for admin display + future routing
	// (e.g. "kind=dsn → call PGConn instead of HTTPClient"). Empty is
	// allowed; baseline guesses from envvar shape.
	Kind string

	// UpdatedAt is the unix-second timestamp of the last write. For
	// the baseline (envvar) impl, this is the process-start time —
	// envvars are immutable per-process so "last updated" defaults to
	// "first seen by this process". The rich impl populates from yaml
	// mtime.
	UpdatedAt int64

	// Owner is a coarse source-shape marker — "env" for envvar / .env
	// file backed credentials, "yaml" for the legacy yaml-shape, and
	// "bootstrap" for the baseline migration. NOT the real creator of
	// the credential — that information is captured at credential-set
	// time via the permission-judge.log (admin's identity is in the
	// permission decision record for the credential:set request); the
	// vault deliberately doesn't persist a separate creator-attribution
	// field (DECISION 3.2: dropped the misleading Set(owner)
	// parameter rather than fake-persisting it).
	Owner string

	// Description is the optional `description=` field from the .env
	// file — a one-line human-readable label (e.g. "小米服务器") used
	// by prompt sources that surface credential names to the model.
	// Empty when the credential's file omits it.
	Description string
}

// KindFactory builds a typed client from a flat key=value credential
// map. Each builder knows its own kind's required fields and returns
// the appropriate typed client (e.g. *http.Client for `bearer`,
// *smtp.Client for `smtp`, Verifier for `hmac`). Callers of
// Broker.Build then type-assert the returned `any` to the type they
// expect.
//
// B2: introduced for the broker v2 generic-factory path.
// Vault.RegisterKind stores these in a mutex-protected map; vault.Build
// dispatches by the credential file's `kind=` field.
type KindFactory func(data map[string]string) (any, error)

// Verifier is the credential-broker handle for webhook-signature
// validation (GitHub / Stripe / Slack-style). The broker holds the
// raw signing secret internally; callers only pass payload + signature
// and get a bool. Implementations MUST use constant-time compare to
// prevent timing-oracle attacks on the secret.
//
// Signature is the RAW bytes the caller extracted from the header
// (e.g. hex-decode the `X-Hub-Signature-256` value before calling).
// The broker is intentionally unaware of header format — webhook
// receivers do header parsing; this interface only handles the
// HMAC compare.
//
// See docs/credentials-design.md §"Webhook signing 校验".
type Verifier interface {
	// Verify reports whether `signature` is a valid signature for
	// `payload` under this credential's secret. Implementation MUST
	// use constant-time compare to prevent timing oracle.
	Verify(payload, signature []byte) bool
}

// Broker is the credential gateway. Callers point at a target
// (e.g. "openai" / "telegram" / "notion") and get back a *configured*
// client — never the raw secret bytes. This closes the "caller leaks
// key via log/panic" hole that pervades the pre-broker code (every
// os.Getenv("X_KEY") site is a potential leak).
//
// Failure modes return wrapped errors so the caller can
// errors.Is(ErrCredentialNotFound) / errors.Is(ErrCredentialReadOnly).
//
// Two impls:
//   - credentials/ baseline: os.Getenv fallback. Always linked.
//     Build / RegisterKind / Verifier are no-ops on baseline — the
//     vault rich plugin is required for the generic-factory path.
//   - credentials-vault/ rich: .env-backed (yaml read-only compat),
//     plus permissions Judge integration. Optional plugin.
type Broker interface {
	// HTTPClient returns a *http.Client pre-configured for `target` —
	// the transport injects the Authorization / x-api-key header so
	// callers do NOT see the raw secret. Returns
	// ErrCredentialNotFound (wrapped) when target isn't registered.
	HTTPClient(ctx context.Context, target string) (*http.Client, error)

	// PGConn returns a *sql.DB opened against the DSN registered for
	// `target` (e.g. "postgres-main"). Callers do NOT see the DSN.
	// Returns ErrCredentialNotFound (wrapped) when target isn't
	// registered.
	PGConn(ctx context.Context, target string) (*sql.DB, error)

	// Build dispatches on the credential file's `kind=` field to a
	// previously-registered KindFactory. Used for non-HTTP, non-PG
	// kinds (SMTP / SSH / S3 / browser cookies / etc.) so the Broker
	// interface stays small while admin extensions plug in new
	// shapes via RegisterKind.
	//
	// Returns ErrCredentialNotFound (wrapped) when:
	//   - the target has no credential file
	//   - no factory is registered for the file's kind
	//
	// Baseline impl always returns ErrCredentialNotFound — the rich
	// vault plugin is required for Build dispatch.
	Build(ctx context.Context, target string) (any, error)

	// RegisterKind installs a KindFactory under `kind`. The vault calls
	// this for built-in kinds (`bearer` / `hmac` / `smtp` / ...) at
	// init; admin extensions can register custom kinds (browser_session,
	// s3, ssh, etc.) before Install().
	//
	// Baseline impl is a no-op (Build always fails on baseline anyway).
	RegisterKind(kind string, factory KindFactory)

	// Verifier returns a Verifier handle for `target`. The credential
	// file's kind must be a verifier-shaped kind (currently `hmac`;
	// future: rsa-sha256, ed25519). Returns ErrCredentialNotFound
	// (wrapped) when target is unknown OR its kind isn't verifier-shaped.
	//
	// The raw signing secret is held entirely inside the returned
	// Verifier — callers only see payload + signature → bool.
	//
	// Baseline impl returns ErrCredentialNotFound (no HMAC in baseline).
	Verifier(ctx context.Context, target string) (Verifier, error)

	// Has reports whether `target` has a credential registered. Cheap
	// existence check; never returns the secret, never logs, never
	// fails (returns false on any error condition).
	Has(ctx context.Context, target string) bool

	// List returns metadata for every registered credential — no
	// secret data in the response. Used by admin /credential list
	// slashes + the future credentials-doctor health check.
	List(ctx context.Context) ([]CredentialMeta, error)

	// Set creates or updates a credential. `kind` is the dispatch
	// classifier (`bearer` / `api_key_header` / `dsn` / `hmac` / ...);
	// `data` is the field map (e.g. {"api_key": "sk-..."}).
	//
	// `data` is map[string]string by contract — every credential field
	// in the .env-shape persists as a key=value string pair, so the
	// stronger type at the broker boundary keeps callers from accidentally
	// stuffing in non-string values that would have to be Sprint'd
	// downstream (R8-26, tightened from map[string]any).
	//
	// DECISION 3.2: the prior signature took an `owner
	// string` parameter that was slog'd but never persisted; audit
	// attribution now flows through the upstream permission-judge.log
	// (admin identity captured per Judge decision on credential:set)
	// rather than a half-persisted broker field.
	//
	// Baseline impl returns ErrCredentialReadOnly — admin must edit
	// .env + restart, or install the rich vault plugin.
	Set(ctx context.Context, name, kind string, data map[string]string) error

	// Delete removes a credential. Idempotent: deleting a
	// non-existent name returns nil.
	//
	// Baseline impl returns ErrCredentialReadOnly.
	Delete(ctx context.Context, name string) error

	// OutgoingTarget returns the non-secret outbound configuration for
	// `target` (chat-id-pool / default-chat / future kinds). For agora
	// bridge outbound + source-telegram outbound selection; the caller
	// already holds its own bot client and just needs to pick WHERE to
	// deliver. Returns ErrCredentialNotFound (wrapped) when target has
	// no credential file / no outbound shape configured.
	OutgoingTarget(ctx context.Context, target string) (OutgoingTarget, error)
}
