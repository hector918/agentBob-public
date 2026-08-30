package contract

import "context"

// This file is the webui presentation contract. The webui is a generic renderer:
// it knows nothing about any specific subsystem. Instead every module DESCRIBES
// ITSELF — what runtime state it wants shown and what settings it exposes — by
// registering a Panel. The webui collects the panels and renders them with a
// fixed vocabulary of field kinds; the frontend only needs renderers, not
// per-subsystem knowledge. See docs/webui-trunk.md.
//
// Discovery mirrors SlashRegistry (contract/88-slash.go): the webui module
// Provides a PanelRegistry; each panel-owning module Requires it and registers
// its panel at Start. Because the coupling is a struct of closures (like
// SlashCommand), the webui imports nothing but contract — each module carries
// its own state inside its closures.

// Panel is one module's self-described webui presence (= one canvas layer). A
// module may register 0..N panels. It mirrors the SlashCommand pattern: a struct
// of closures the owning module builds over its own state and registers.
type Panel struct {
	ID        string // stable id, e.g. "model-pool"; the webui keys layout/routing on it
	Title     string
	AdminOnly bool     // whole layer is sensitive → redacted from /api/state when unauthenticated
	Tier      string   // home-graph tier: "flow" for a flow module; "" (default) = leaf; "none" = hidden (addressable for View/Page but NOT drawn as a home node — a sub-view reached via a parent panel's GraphAction.Open). The trunk has no panel; the webui draws a synthetic root.
	Upstreams []string // panel IDs this module depends on — the home-graph edges (each module describes its own)
	Settings  []SettingSpec

	// State is polled by the webui each refresh tick; it returns the live
	// read-only fields to display.
	State func(ctx context.Context) []StateField
	// Read returns a setting's current value (e.g. a yaml body) for the editor.
	Read func(ctx context.Context, settingID string) (string, error)
	// Apply writes a setting (code settings only — actions go through the dock).
	Apply func(ctx context.Context, settingID, body string) WriteResult
	// Page fetches a later page of a paged table field (optional; offset/limit).
	Page func(ctx context.Context, fieldID string, limit, offset int) (TablePage, error)
	// View returns a parameterized read-only field set for a glance/layer opened by a
	// GraphAction.Open (mirrors Read, but yields fields not a string) — e.g. an
	// inbox's chat log. viewID is opaque (the module encodes its key, like Read's
	// settingID). Optional (nil → the panel exposes no views).
	View func(ctx context.Context, viewID string) ([]StateField, error)
}

// StateField is one runtime datum. The frontend dispatches on Kind to a renderer.
type StateField struct {
	Kind  string // "stat" | "status" | "table" | "text" | "graph"
	Label string
	ID    string // paged tables only: Page() addresses this field by it

	// stat / status
	Value  string // stat: a value the module already formatted (number / "X/Y" / "1.2M tok")
	Unit   string // stat: optional unit
	Status string // stat/status: "ok"|"warn"|"down"|"" → the frontend colors it

	// table
	Columns   []string
	Rows      [][]Cell // unpaged: all rows; paged: the first page (offset 0)
	Paged     bool     // true → server-side paging (offset/limit); later pages via Panel.Page
	Total     int64    // paged: total row count (frontend computes page X/Y)
	PageSize  int      // paged: rows per page (e.g. accounts 50, inbox chat 30)
	RowCopy   bool     // table: each row hover-highlights and copies its joined text on click (e.g. a chat log)
	TagFilter bool     // table: draw a tag-filter bar (the union of the rows' Cell.Tags) above it; clicking a tag toggles it and the table shows only rows whose Cell.Tags include every active tag. Frontend-only/stateful — the server just declares the dimension via Cell.Tags.

	// text
	Text string // multi-line; the frontend wraps it (error reason / explanation)

	// graph (Kind=="graph"): a node/edge graph banded by Bands (ordered inner→outer).
	// A node click opens a read-only glance with the node's Detail. Layout selects the
	// geometry: "" (default) = horizontal bands (the home dependency graph); "radial"
	// = a half-disk sunburst (Bands as concentric rings inner→outer, edges as a tree:
	// the agora org — companies inner, members mid, inboxes outer).
	Bands  []string
	Layout string
	Nodes  []GraphNode
	Edges  []GraphEdge
}

// GraphNode is one node of a "graph" StateField; Band names the tier it sits in.
type GraphNode struct {
	ID      string
	Band    string
	Title   string
	Sub     string
	Status  string        // ""|"ok"|"warn"|"down" → node color
	Detail  string        // node click opens a read-only glance with this text
	Actions []GraphAction // per-entity action buttons shown in that glance (prefill the dock)
	// Fields, when set, are read-only StateFields rendered in the node's detail glance
	// BELOW its Detail text — so a node self-describes richer content (e.g. a company
	// node's employee table with per-row actions) instead of the generic renderer
	// deriving it. Keeps subsystem logic in the module, never in the webui.
	Fields []StateField
}

// GraphAction is one button in a node's detail glance. A plain action prefills
// Command into the dock (§5.3). A PICKER action (Pick non-empty) opens a
// filterable list; clicking an item substitutes its Value into Command's "%s"
// and prefills that — e.g. a company's "hire" picker lists members. An EDIT action
// (Edit non-nil) opens a code editor (model form) on a panel setting — e.g. a
// company's permissions.yaml / playbook.
type GraphAction struct {
	Label   string
	Command string
	Pick    []PickItem
	Edit    *EditTarget
	Open    *ViewTarget
}

// EditTarget points a GraphAction at a panel setting to edit in a model form; the
// editor's Read/Apply address Panel.Read/Apply(ctx, Setting) on the named panel.
type EditTarget struct {
	Panel   string // panel ID owning Read/Apply
	Setting string // setting id (sid) passed to Read/Apply
	Lang    string // "yaml" | "markdown" | "text"
}

// ViewTarget is a read-only open: clicking it descends into either a graph NODE's
// detail glance (Node set — the company→member→inbox drill-down) OR a panel's
// parameterized View(viewID) (View set — e.g. an inbox's chat log). The read-only
// counterpart of EditTarget; like Edit it may target a FOREIGN panel. Kind picks the
// form ("glance" transient | "layer" persistent); Title labels it.
type ViewTarget struct {
	Panel string // panel ID owning the node / View
	Node  string // a graph nodeID on Panel → open that node's detail glance (like clicking it)
	View  string // OR a viewID passed to Panel.View → open its fields. Node wins if both set.
	Kind  string // form for a VIEW target: "glance" transient | "layer" persistent. A Node target is always a transient glance (Kind ignored).
	Title string
}

// PickItem is one row of a picker action: Label is shown, Sub is a secondary line
// (e.g. "hired by: acme"), Value is substituted into the action's Command "%s".
type PickItem struct {
	Value string
	Label string
	Sub   string
}

// GraphEdge is one directed edge; Label rides the curve, Weight scales its pulse.
type GraphEdge struct {
	From   string
	To     string
	Label  string
	Weight int
}

// Cell is one table cell; it may carry a status color (e.g. a model "state"
// column, an account "status" column).
type Cell struct {
	Text   string
	Status string // ""|"ok"|"warn"|"down"
	// Detail, when set, is revealed by a "why" affordance the table renderer draws
	// at the row's end — a read-only explanation opened as a glance (e.g. a model
	// entry's last error behind a "cooling" state). Multi-line Text (\n) renders as
	// a primary line + indented continuation lines.
	Detail string
	// Action, when set, draws a per-row button (labeled ActionLabel) at the row's
	// end that PREFILLS the dock with this slash command for the admin to review +
	// Enter (the §5.3 confirm-by-prefill mechanism) — e.g. "/accounts show ac_123"
	// for a usage glance. Admin-only (the dock prefill is gated). The command must be
	// in the webui run allowlist (60-run.go).
	Action      string
	ActionLabel string
	// Actions draws one button per entry at the row's end (each prefills its Command),
	// for rows needing MORE than the single Action above — e.g. an employee row's
	// [change role] [terminate]. Action (singular) stays the one-button shorthand; if
	// both are set the renderer draws the singular Action first, then these.
	Actions []CellAction
	// Copy, when set, makes the table renderer draw a per-row "copy" affordance that
	// copies this text to the clipboard (not a command prefill) — e.g. a chat scope
	// to paste into /agora inbox wire. A cell uses Action OR Copy.
	Copy string
	// Tags is the cell's filter dimension for a TagFilter table (see StateField):
	// the structured tags a row is filtered by, SEPARATE from Text (which is the
	// human display). The model pool sets it on the name cell from the entry's tags.
	Tags []string
	// Open, when set, makes this cell's TEXT a clickable link that descends into the
	// target (a node glance or a View) — the drill-down navigation, e.g. a company's
	// employee-name cell → the member's glance, a member's inbox-name cell → the inbox.
	Open *ViewTarget
	// Edit, when set, draws a per-row "edit" button at the row's end that opens a code
	// editor (model form) on the named panel setting — the table-cell counterpart of
	// GraphAction.Edit, addressing Panel.Read/Apply(ctx, Setting). Admin-only (the
	// setting read/write endpoints are admin-gated). Use for a per-row editable artifact,
	// e.g. a tool/skill's learned INSIGHT.md (Lang "markdown").
	Edit *EditTarget
}

// CellAction is one button in a table row (a Cell.Actions entry): Label is shown,
// Command is prefilled into the dock on click. Same gating + run-allowlist as the
// singular Cell.Action.
type CellAction struct {
	Label   string
	Command string
}

// TablePage is one page of a paged table. It mirrors the skeleton webui's
// AccountsPage: offset/limit plus a grand total.
type TablePage struct {
	Rows   [][]Cell
	Total  int64
	Limit  int
	Offset int
}

// SettingSpec is the static description of one editable item. The frontend
// dispatches on Kind to a control.
type SettingSpec struct {
	ID    string // stable; Read/Apply address it
	Kind  string // "code" | "action" ("line"|"toggle"|"select" added as needed)
	Label string

	Lang    string // code: "yaml"|"markdown"|"text"
	Command string // action: the slash template prefilled into the dock (the user reviews then Submits → /api/run)
}

// WriteResult is the uniform shape every Apply returns, so the frontend shares
// one render path. It mirrors the skeleton webui's YamlWriteResult.
type WriteResult struct {
	Success bool
	Phase   string // "validate"|"backup"|"write"|"reload" — which step failed
	Error   string
}

// PanelRegistry holds the registered panels. Provided by the webui module; each
// panel-owning module Requires it and registers at Start. Mirrors SlashRegistry.
type PanelRegistry interface {
	// RegisterPanel adds a panel. Panics on a duplicate ID or a nil State — both
	// are wiring bugs that must surface at startup, same posture as trunk.Provide.
	RegisterPanel(p Panel)
	// Panels returns every registered panel, sorted by ID.
	Panels() []Panel
}
