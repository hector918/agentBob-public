// Package browserd is the control-plane HTTP service that hosts bob's
// browser subsystem out-of-process (docs/browserd.md, P1). It wraps the
// existing tools/browser core (Pool / actions / tabs / custodian) in thin
// JSON endpoints; chromium's RAM/CPU then lives in the browserd container
// and can never OOM the main agent.
//
// Trust model: browserd does NO permission work. It is a private-network
// trusted executor — bob runs the gate (browser.ProfileIdentity) and ships
// the resolved routing facts ({scope, profile_key}) with every request.
// The listen address must never be exposed beyond the private network.
package browserd

import (
	"encoding/json"

	"agentbob/sidecars/browser/tools/browser"
)

// Common carries the routing fields every action request includes.
//
//   - Scope is the conversation's session_scope — the live-session axis
//     (pool key on the legacy path, custodian owner on the profile path).
//   - ProfileKey is the acting identity whose persistent browser profile to
//     use ("" = legacy per-scope ephemeral browser). browserd derives the
//     pool key (browser.ProfileName) and vault dir (browser.ProfileDir)
//     from it; bob has already gated it.
type Common struct {
	Scope      string `json:"scope"`
	ProfileKey string `json:"profile_key,omitempty"`
	// Mode routes the profile (browser profile identity Phase 2): "" = in-place on the
	// profile's vault dir (the login session a human takes over, the master's single
	// writer; also the legacy path); "copy" = a read-only per-session worker copy seeded
	// from that vault dir (own instance, no custodian checkout → concurrent, discarded on
	// close). Ignored when ProfileKey is empty (legacy per-scope ephemeral browser).
	Mode string `json:"mode,omitempty"`
}

// --- per-endpoint requests -------------------------------------------------

type NavigateReq struct {
	Common
	URL string `json:"url"`
}

type SnapshotReq struct {
	Common
	Format   string `json:"format,omitempty"`
	Selector string `json:"selector,omitempty"`
	// (ScreenshotDir is gone: format=image PNG bytes travel
	// back in the response and bob writes its own sandbox — the shared-
	// mount same-host deploy invariant no longer exists. Same two-step
	// shape as /vision.)
}

type ClickReq struct {
	Common
	Ref      string `json:"ref,omitempty"`
	Selector string `json:"selector,omitempty"`
}

type TypeReq struct {
	Common
	Ref      string `json:"ref,omitempty"`
	Selector string `json:"selector,omitempty"`
	Text     string `json:"text"`
	Replace  bool   `json:"replace,omitempty"`
}

type PressReq struct {
	Common
	Key      string `json:"key"`
	Ref      string `json:"ref,omitempty"`
	Selector string `json:"selector,omitempty"`
}

type ScrollReq struct {
	Common
	Direction string `json:"direction,omitempty"`
	Pixels    int    `json:"pixels,omitempty"`
	Selector  string `json:"selector,omitempty"`
}

type BackReq struct {
	Common
}

type DialogReq struct {
	Common
	Accept     bool   `json:"accept"`
	PromptText string `json:"prompt_text,omitempty"`
}

type ConsoleReq struct {
	Common
	Level      string `json:"level,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
}

type GetImagesReq struct {
	Common
	Selector  string `json:"selector,omitempty"`
	MaxImages int    `json:"max_images,omitempty"`
}

// VisionReq asks for a screenshot only. The vision MODEL call stays in bob
// (the model pool is bob's; docs/browserd.md keeps browserd model-free) —
// bob's browser_vision shell posts here, gets PNG bytes back, then runs its
// own vision-model round.
type VisionReq struct {
	Common
}

type CDPReq struct {
	Common
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type TabReq struct {
	Common
	Action string `json:"action,omitempty"`
	Index  int    `json:"index,omitempty"`
}

// ProfilesReq lists the live profile checkouts (custodian snapshot). It is
// the control-plane feed for bob's admin takeover picker (docs/browserd.md
// P2) — in remote mode the custodian lives here, so bob has to ask.
type ProfilesReq struct{}

// PurgeReq wipes the scope's browser state (instance + legacy data dir +
// profile checkouts released) — the remote face of Pool.PurgeSession, wired
// to bob's /new and /close-session.
type PurgeReq struct {
	Scope string `json:"scope"`
}

// SaveReq (保存登陆) writes the worker COPY at scope back to the member master it seeded
// from — the human-driven login becomes what future copies seed. The one master writer.
type SaveReq struct {
	Scope string `json:"scope"`
}

// LiveReq asks whether scope has a LIVE chromium instance a human could take
// over — the remote face of Pool.LiveForScope, backing bob's /takeover
// no-live-browser guard (docs/remote-browser-handoff.md P1b). Read-only;
// never spawns chromium.
type LiveReq struct {
	Scope string `json:"scope"`
}

// --- responses ---------------------------------------------------------------

// Response is the uniform envelope every endpoint returns.
//
//   - transport-level problems (bad method / undecodable body) → 4xx with
//     OK=false and Error set;
//   - action-level failures (navigate timed out, selector missing, profile
//     busy) → HTTP 200 with OK=false and Error set — these are normal
//     model-facing errors, not service faults;
//   - success → HTTP 200, OK=true, Data holding the endpoint's payload.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// HealthData is /healthz's payload — the config browserd actually resolved, so
// a deploy can spot drift from bob's own config.yaml (separate files per
// process). PageTimeoutSeconds is the effective per-action page timeout; a
// mismatch with bob's makes slow actions look like transport errors on bob's
// side.
type HealthData struct {
	PageTimeoutSeconds int `json:"page_timeout_seconds"`
}

// VisionData is /vision's payload: the current page as PNG, base64-encoded
// for the JSON wire. ByteCount is the raw PNG size (pre-base64).
type VisionData struct {
	ImageB64  string `json:"image_b64"`
	ByteCount int    `json:"byte_count"`
}

// ConsoleData is /console's payload.
type ConsoleData struct {
	Entries []browser.ConsoleEntry `json:"entries"`
}

// ImagesData is /get-images' payload.
type ImagesData struct {
	Images []browser.ImageInfo `json:"images"`
}

// ProfileInfo is one live profile checkout in /profiles' payload. Owner (the
// session_scope) is the takeover handle; Key is the profile pool key, kept as
// a human label only (mirrors the webui picker contract).
type ProfileInfo struct {
	Key    string `json:"key"`
	Owner  string `json:"owner"`
	Pinned bool   `json:"pinned"`
}

// ProfilesData is /profiles' payload.
type ProfilesData struct {
	Profiles []ProfileInfo `json:"profiles"`
}

// LiveData is /live's payload: whether the requested scope has a live,
// take-over-able chromium instance right now.
type LiveData struct {
	Live bool `json:"live"`
}

// VaultProfile is one PERSISTED browser profile in GET /profiles' payload
// (docs/browserd.md P3) — a dir under browserd's profile vault, present
// whether or not any chromium is live. Key is the vault dir segment (the
// sanitized identity) and is the handle the export/import endpoints take.
// CheckedOut mirrors the custodian: true means some scope holds the profile
// right now (chromium may be live → its SQLite is inconsistent → export is
// refused with 409 until it's released).
type VaultProfile struct {
	Key        string `json:"key"`
	CheckedOut bool   `json:"checked_out"`
}

// VaultProfilesData is GET /profiles' payload (the persisted-profile listing;
// POST /profiles keeps returning the LIVE checkout snapshot in ProfilesData).
type VaultProfilesData struct {
	Profiles []VaultProfile `json:"profiles"`
}
