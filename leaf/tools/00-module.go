package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"agentbob/contract"
	"agentbob/leaf/tools/browserremote"
	"agentbob/leaf/tools/epub"
	"agentbob/leaf/tools/imagecreate"
	"agentbob/leaf/tools/promptlib"
	"agentbob/leaf/tools/wordpress"
	"agentbob/trunk"
)

const (
	// scraplingSandboxIdleTTL: a per-session scrapling sandbox dir untouched for this
	// long is swept. scraplingSweepPeriod is how often the sweep runs.
	scraplingSandboxIdleTTL = 7 * 24 * time.Hour
	scraplingSweepPeriod    = 6 * time.Hour
)

// Module is the tools catalog as a trunk module: it builds the registered tool
// set and provides contract.ToolCatalog. The flow Requires it (optionally),
// projects + authorizes into the per-turn ToolSet, and hands that to the turn.
type Module struct {
	home      string // $BOB_HOME — roots the per-session sandbox the scrapling tool writes under
	cat       *catalog
	pool      *pool
	fail      *toolFailureStore // per-tool failure records (learn raw material); nil → no persistence
	imgCreate *imagecreate.Tool // held past construction so its WAL sweep can be registered on the Housekeeper
	store     *insightStore     // learned-notes persistence (INSIGHT.md); nil → in-memory
	state     atomic.Int32      // trunk.State
}

// New returns a tools module. home is $BOB_HOME; the scrapling tool writes its
// scratch output under $home/sandbox keyed by the turn's sid.
func New(home string) *Module { return &Module{home: home} }

func (m *Module) Name() string { return "tools" }

func (m *Module) Provides() []reflect.Type {
	return []reflect.Type{
		trunk.TypeOf[contract.ToolCatalog](),
		trunk.TypeOf[contract.ChannelPool](),
		trunk.TypeOf[contract.BrowserControlHold](),
	}
}

// Needs the webui PanelRegistry so it can register its dashboard panel at Start.
// The registry is provided by the critical webui module (always present, even when
// the http server is off), so this hard Need can't skip-trap an Optional module.
func (m *Module) Needs() []reflect.Type {
	return []reflect.Type{trunk.TypeOf[contract.PanelRegistry]()}
}

// Optional is true: without a tool catalog the flow runs the turn text-only — the
// pipeline still works, the model just has no tools.
func (m *Module) Optional() bool { return true }

func (m *Module) Health() trunk.State { return trunk.State(m.state.Load()) }

func (m *Module) Start(ctx context.Context, reg *trunk.Registry) error {
	// urlLib is the OPTIONAL URL-memory library (leaf/urllib provides it). It is
	// resolved LAZILY through this thunk, not at Start: urllib hard-needs the DB so
	// the trunk may start it AFTER tools (tools needs nothing), but the registry
	// persists, so a Run-time first-use lookup always sees it once started. The web
	// tools (web_search / scrapling / browser) Record successful fetches and Recall
	// through it; nil thunk result → they keep their no-library behaviour.
	urlLib := lazyURLLibrary(reg)

	// modelPool is the OPTIONAL model pool, resolved LAZILY (same reason as urlLib:
	// the pool module may start after tools in the trunk order). The image tool holds
	// a Kind-bound handle over it — it can ONLY reach the OCR backend (KindOCR → the
	// OCR sidecar), never the general LLM. Absent pool / no kind=ocr entry → the tool
	// reports the backend is unavailable rather than erroring at Start.
	modelPool := lazyModelPool(reg)

	// The catalog wraps each tool's Run in a learn observer that persists a shape-only
	// record of each FAILED call (docs/learn.md §6) so accreted tool experience survives
	// a restart, symmetric with the skill/member failure stores. Needs a writable home;
	// without one the observer no-ops (tools don't learn). Built before the catalog so
	// the observer can write to it.
	if m.home != "" {
		m.fail = &toolFailureStore{base: filepath.Join(m.home, "insights", "tools", ".failures")}
	}
	registered := []contract.Tool{
		clarify{}, staySilent{}, now{}, fsTool{}, terminalTool{}, skillView{}, delegate{}, deliverFile{},
		// web_search_scrapling — merged search+fetch tool (docs/web-fleet.md §11). ALWAYS
		// registered: plain-engine search needs no CLI; render/stealth engines + url-fetch
		// modes report "scrapling CLI not found" when absent rather than hiding.
		newWebTool(filepath.Join(m.home, "sandbox"), urlLib, pageExtractFn(modelPool), pageExtractCapFn(modelPool)),
		sendMessage{gw: lazyGateway(reg), agora: lazyAgoraSend(reg)},
		// vision — the single surface for everything done with a picture or a clip the
		// user sent: task=read transcribes it through the Kind-bound OCR sidecar,
		// task=answer and task=reverse_prompt hand it to a vision-tagged llm
		// (Requires:["vision"]), a video reaching task=answer as sampled frames.
		// Merged from the former `ocr` + `vision` tools and renamed from
		// `image` when video joined; see visionTool for why one surface rather than
		// three. Both legs are lazy + fail-closed.
		visionTool{
			ocr:  kindChat{pool: modelPool, kind: contract.KindOCR},
			pool: modelPool,
		},
		// audio — on-demand transcription of a clip's soundtrack (kind=asr). The PULL half
		// of spoken content: ingestion (leaf/asr) already put short voice notes in the
		// message text, so this exists for what ingestion declined — anything past its
		// latency budget, announced there as 「（时长 …，未自动转写）」 — and for reaching a
		// chosen slice of a long recording.
		audioTool{pool: modelPool},
		epub.New(translateFunc(kindChat{pool: modelPool, kind: contract.KindTranslate})),
		recallTool{client: lazyRetrievalClient(reg)},
		// arrangement tools — thin shells over contract.Arrangements (leaf/arrangement).
		// Lazy: arrangement is Optional and may start after tools; absent → the tools
		// report the feature is off (docs/arrangement.md).
		arrangementDefine{a: lazyArrangements(reg)},
		arrangementInject{a: lazyArrangements(reg)},
		arrangementPull{a: lazyArrangements(reg)},
		arrangementSubmit{a: lazyArrangements(reg)},
	}
	// WordPress / WooCommerce REST passthrough (docs/wordpress.md). TWO
	// namespace-locked tools (the tool-split IS the warrant boundary:
	// tool:use:woocommerce vs tool:use:wordpress_content). Per-endpoint field
	// knowledge lives in the woocommerce_operation / wordpress_content SKILLs, not
	// here. ALWAYS registered so the capability stays visible; the SITE is a vault
	// credential (kind=wordpress) resolved per identity at use time — each tool
	// asks warrant for the kind=wordpress credential the caller is authorized for
	// (the factory is registered with the broker by the wiring layer). No
	// credential authorized → Run reports it rather than vanishing from the catalog.
	registered = append(registered, wordpress.NewWooTool(), wordpress.NewWPTool())
	// corpus_import: authenticated import into the promptlib(hygpo) 素材库 (docs —
	// leaf/tools/promptlib). The import key is a bob-process secret that must never
	// reach the model's whitelisted shell, so the authenticated POST is made bob-side
	// here. ALWAYS registered so the capability stays visible; config is process env
	// ($PROMPTLIB_IMPORT_KEY / $PROMPTLIB_BASE) read at use time — unset key → Run
	// reports "未配置" rather than vanishing from the catalog (same contract as the
	// wordpress/browser tools above). Per-endpoint整理规则 lives in the corpus-import SKILL.
	registered = append(registered, promptlib.New())
	// draw: image GENERATION (and editing). The outbound counterpart to `image` —
	// the boundary is DIRECTION, not medium, so everything whose product is a
	// picture lives there including img2img (docs/image-create-tool.md §2.2). ALWAYS
	// registered: with no kind=image entry it reports "没有可用的生图后端" rather
	// than vanishing, the same contract as the wordpress/browser tools. Holds the
	// gateway lazily for its write-ahead log, which must deliver to a chat AFTER
	// the turn that asked is gone (§9.5).
	m.imgCreate = imagecreate.New(modelPool, lazyImageCatalog(reg), lazyGateway(reg), m.home)
	registered = append(registered, m.imgCreate)
	// The single `browser` tool is an HTTP thin client to an out-of-process
	// browserd service (docs/browserd.md): one action-dispatched tool that fans out
	// to browserd's endpoints (was 13 separate browser_* tools). ALWAYS registered so
	// the capability stays visible; the URL comes from $BROWSERD_URL (trunk has no
	// tools.browser config section yet). Unset/down → the tool's Run reports the
	// backend unavailable rather than vanishing from the catalog (a missing backend
	// shouldn't hide the capability — only make calling it fail honestly).
	browserURL := os.Getenv("BROWSERD_URL")
	bclient := browserremote.New(browserURL, os.Getenv("BROWSERD_API_KEY"))
	// The human control-hold gate (browser takeover yield): the browser tool checks it
	// before each action; leaf/webui drives Set/Clear from the control-mode toggle.
	// Provided always (the browser tool is always registered).
	holds := newControlHolds()
	trunk.Provide[contract.BrowserControlHold](reg, holds)
	// browser_vision's model leg: browserd only screenshots (runs no model), so the
	// vision answer runs bob-side over bob's own pool — pick a vision-tagged llm
	// (Requires:["vision"]) and answer over the screenshot. Lazy pool / no vision entry
	// → the tool degrades to saving the PNG. Kept a narrow func so browserremote stays
	// decoupled from the model pool.
	visionFn := func(ctx context.Context, question string, png []byte, mime string) (string, error) {
		p := modelPool()
		if p == nil {
			return "", fmt.Errorf("model pool not available")
		}
		resp, err := p.Chat(ctx, contract.ModelRequest{Requires: []string{"vision"}}, []contract.Message{{
			Role:    "user",
			Content: question,
			Images:  []contract.ImageRef{{Data: png, MIME: mime}},
		}})
		if err != nil {
			return "", err
		}
		// FAIL-CLOSED guard (mirrors askVision in 97-vision.go): a config-driven vision→smart
		// fallback rule can serve this request on a non-vision entry that never saw
		// the PNG. Error out instead of returning a confident hallucination —
		// browserremote treats a visionFn error as "degrade to saving the PNG".
		if !entryHasTag(p, resp.Model, "vision") {
			return "", fmt.Errorf("answered by non-vision entry %q, refusing", resp.Model)
		}
		return resp.Content, nil
	}
	registered = append(registered, browserremote.NewTool(bclient, urlLib, holds, visionFn))
	// escalate_to_coo hands a stuck browser (login wall / captcha) to a human: it
	// guards on a live browser (bclient) and mints a takeover token via the webui
	// (lazy). For an unattended agora worker it pushes the token to the company
	// bridge — the SAME gw + agora seams send_message uses (lazy: both Optional / may
	// start after tools). ALWAYS registered (not gated on BROWSERD_URL): a backend-less
	// boot must not let the config reconcile mirror prune its grant row (incl. an admin
	// toggle), which would silently zero the authorization once the env returns. Without
	// a backend its Run fails honestly ("接管功能未启用") — same "always register, report
	// unavailable" contract as the browser/wordpress tools above (F67).
	registered = append(registered, escalateToCoo{
		tk: bclient, mint: lazyTakeoverMinter(reg), holds: holds,
		gw: lazyGateway(reg), agora: lazyAgoraSend(reg),
	})
	// runtime_state: admin-only self-inspection (docs — 97-runtime.go). It renders the
	// SAME PanelRegistry the webui polls, so the model can read the live process state
	// in the admin conversation and debug agentbob itself. The registry is already a
	// hard Need of this module (for m.panel()); Require is idempotent. ALWAYS registered
	// so its capability row stays in the reconcile mirror (an admin-flipped grant must
	// not be pruned on a boot where the tool vanished); admin-only is the permissions
	// matrix's job, not this tool's — it carries no code gate.
	registered = append(registered, runtimeStateTool{panels: trunk.Require[contract.PanelRegistry](reg)})
	if browserURL != "" {
		// The same client serves the live takeover seam (screencast/input relay to
		// browserd's takeover face), consumed by leaf/webui. Only when a backend exists.
		trunk.Provide[contract.BrowserTakeover](reg, bclient)
		slog.Info("browser remote tool registered", "browserd_url", browserURL)
	} else {
		slog.Info("browser remote tool registered without a backend — runs report browserd unavailable (set BROWSERD_URL)")
	}
	m.cat = newCatalog(m.fail, registered...)

	// Persist learned usage notes as INSIGHT.md files (docs/learn.md §6) so accreted
	// experience survives a restart. Needs a writable home; without one the notes stay
	// in-memory (the catalog is unaffected).
	if m.home != "" {
		m.store = &insightStore{dir: filepath.Join(m.home, "insights", "tools")}
		// Boot-time orphan sweep: a tool renamed/deleted across builds must not keep
		// its INSIGHT.md + .failures/ dir forever (nor reload the stale addendum into
		// memory every boot). The tool set is STATIC per build — no reload path — so a
		// one-shot sweep here suffices; no Housekeeper task. Runs before all() so an
		// orphan note is never loaded. Mirrors skills' boot sweep (leaf/skills).
		live := map[string]bool{}
		for _, n := range m.cat.names() {
			live[safeToolName(n)] = true
		}
		if n := m.store.sweepOrphans(live); n > 0 {
			slog.Info("tools: swept insights of removed tools", "files", n)
		}
		if m.fail != nil {
			if n := m.fail.sweepOrphans(live); n > 0 {
				slog.Info("tools: swept failure records of removed tools", "dirs", n)
			}
		}
		// all() is per-file fail-open (unreadable files warned + skipped), so one bad
		// INSIGHT.md can't blank every tool's notes for the run. all() keys by the
		// safeToolName-sanitized filename, but every reader uses the REAL spec name —
		// so iterate the catalog's real names and look each up under its sanitized key,
		// else a note for any tool whose name isn't safeToolName-identity loads under a
		// key nobody reads (B17).
		all := m.store.all()
		for _, n := range m.cat.names() {
			if text, ok := all[safeToolName(n)]; ok {
				m.cat.setAddendum(n, text)
			}
		}
	}
	trunk.Provide[contract.ToolCatalog](reg, m.cat)

	// The channel pool keeps stateful channels (exec sessions; remote conns) alive
	// across calls; warrant Acquires through it. Runtime resource — own idle timer.
	m.pool = newPool()
	m.pool.start()
	trunk.Provide[contract.ChannelPool](reg, m.pool)

	// Self-describe to the webui (catalog + pool are now built). Hard-Need, so the
	// registry is guaranteed present.
	trunk.Require[contract.PanelRegistry](reg).RegisterPanel(m.panel())

	// Optional: register the tool learn source (one target per tool) into leaf/learn.
	// learn is registered before tools in cmd/bob, so it has started by now; absent →
	// tools simply don't learn.
	if lr, ok := trunk.TryRequire[contract.LearnRegistry](reg); ok {
		lr.AddSource(learnSource{cat: m.cat, fail: m.fail, store: m.store})
	}

	// Persistent-state sweep: scrapling creates one dir per distinct sid under
	// $home/sandbox/by_sessions_scope and only removes its per-call temp file, never
	// the dir — so without a sweep those dirs/inodes accumulate unboundedly. SWEEP
	// RULE: a persistent-state sweep belongs on the trunk Housekeeper, not a private
	// timer. (The files module's date-dir sweep matches YYYY-MM-DD names only, so it
	// never reaches these sid dirs.)
	if m.home != "" {
		if hk, ok := trunk.TryRequire[trunk.Housekeeper](reg); ok {
			home := m.home
			hk.Register(trunk.Task{
				Name:   "scrapling-sandbox-sweep",
				Period: scraplingSweepPeriod,
				Run: func(context.Context) error {
					return sweepScraplingSandboxes(home, scraplingSandboxIdleTTL)
				},
			})
			// image_create's write-ahead log: finish (or honestly abandon) generations
			// whose submitting process died. Same SWEEP RULE — persistent state, so the shared
			// scheduler rather than a private timer. It also has to run here rather than
			// at Start: delivery needs the gateway, which may start AFTER tools.
			if m.imgCreate != nil {
				hk.Register(m.imgCreate.HousekeepingTask())
			}
		}
	}

	m.state.Store(int32(trunk.StateReady))
	return nil
}

// sweepScraplingSandboxes removes per-session scrapling sandbox dirs whose mtime is
// older than idleTTL (a dir's mtime advances on each call's temp-file create/remove,
// so an idle dir is one with no recent activity). Idempotent: a missing root or an
// already-gone dir is a no-op. Errors on individual removes are collected, not fatal.
func sweepScraplingSandboxes(home string, idleTTL time.Duration) error {
	base := filepath.Join(home, "sandbox", "by_sessions_scope")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing created yet
		}
		return err
	}
	cutoff := time.Now().Add(-idleTTL)
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *Module) Stop(context.Context) error {
	if m.pool != nil {
		m.pool.stop()
	}
	m.state.Store(int32(trunk.StateStopped))
	return nil
}

// lazyURLLibrary returns a thunk resolving the OPTIONAL contract.URLLibrary on
// first use. Lazy because urllib (which hard-needs the DB) may start AFTER tools
// in the trunk's topo order; the registry persists, so the first Run-time lookup
// after startup finds it. Absent → returns nil (the web tools stay nil-safe).
func lazyURLLibrary(reg *trunk.Registry) func() contract.URLLibrary {
	var once sync.Once
	var lib contract.URLLibrary
	return func() contract.URLLibrary {
		once.Do(func() { lib, _ = trunk.TryRequire[contract.URLLibrary](reg) })
		return lib
	}
}

// lazyRetrievalClient resolves the OPTIONAL contract.RetrievalClient (cold-memory
// leaf) on first use — nil when retrieval is disabled, so recall_memory reports
// the feature is off rather than vanishing from the catalog.
func lazyRetrievalClient(reg *trunk.Registry) func() contract.RetrievalClient {
	var once sync.Once
	var cl contract.RetrievalClient
	return func() contract.RetrievalClient {
		once.Do(func() { cl, _ = trunk.TryRequire[contract.RetrievalClient](reg) })
		return cl
	}
}

// lazyModelPool returns a thunk resolving the OPTIONAL contract.ModelPool on first
// use. Lazy for the same reason as lazyURLLibrary — the pool may start after tools
// in the trunk order. Absent → nil (the image tool reports the backend is absent). A
// SOFT dependency (TryRequire, not Needs): tools stays Optional and order-free.
func lazyModelPool(reg *trunk.Registry) func() contract.ModelPool {
	var once sync.Once
	var pool contract.ModelPool
	return func() contract.ModelPool {
		once.Do(func() { pool, _ = trunk.TryRequire[contract.ModelPool](reg) })
		return pool
	}
}

// lazyImageCatalog returns a thunk resolving the OPTIONAL contract.ImageCatalog on
// first use — same reason as the pool: the model module may start after tools.
// Absent → image_create reports no backends rather than erroring at Start.
func lazyImageCatalog(reg *trunk.Registry) func() contract.ImageCatalog {
	var once sync.Once
	var cat contract.ImageCatalog
	return func() contract.ImageCatalog {
		once.Do(func() { cat, _ = trunk.TryRequire[contract.ImageCatalog](reg) })
		return cat
	}
}

// lazyGateway returns a thunk resolving the OPTIONAL contract.Gateway on first
// use — the seam send_message emits through (it resolves the internal source by
// name + type-asserts to contract.MessageEmitter). Lazy for the same reason as
// lazyURLLibrary (the gateway may start after tools); a SOFT dependency
// (TryRequire, not Needs) so tools stays Optional and order-free. Absent → the
// tool reports the message bus is unavailable rather than erroring at Start.
func lazyGateway(reg *trunk.Registry) func() contract.Gateway {
	var once sync.Once
	var gw contract.Gateway
	return func() contract.Gateway {
		once.Do(func() { gw, _ = trunk.TryRequire[contract.Gateway](reg) })
		return gw
	}
}

// lazyAgoraSend returns a thunk resolving the OPTIONAL contract.AgoraSend on first
// use — the send_message Stage B seam (target name-resolution + channel auth). Lazy
// for the same reason as lazyGateway (agora is Optional and may start after tools); a
// SOFT dependency (TryRequire, not Needs) so tools stays Optional and order-free.
// Absent → send_message degrades to Stage A (raw scope emit, no name-resolution / no
// channel check).
func lazyAgoraSend(reg *trunk.Registry) func() contract.AgoraSend {
	var once sync.Once
	var as contract.AgoraSend
	return func() contract.AgoraSend {
		once.Do(func() { as, _ = trunk.TryRequire[contract.AgoraSend](reg) })
		return as
	}
}

// lazyArrangements returns a thunk resolving the OPTIONAL contract.Arrangements (the
// arrangement mechanism) on first use — the seam the arrangement_* tools reach. Lazy
// because arrangement is Optional and may start after tools; a SOFT dependency
// (TryRequire, not Needs) so tools stays Optional and order-free. Absent → the tools
// report the feature is off.
func lazyArrangements(reg *trunk.Registry) func() contract.Arrangements {
	var once sync.Once
	var a contract.Arrangements
	return func() contract.Arrangements {
		once.Do(func() { a, _ = trunk.TryRequire[contract.Arrangements](reg) })
		return a
	}
}

// lazyTakeoverMinter returns a thunk resolving the OPTIONAL contract.TakeoverMinter
// (the webui auth) on first use — the escalate_to_coo seam. Lazy because webui may
// Start after tools; the registry persists, so the first Run-time lookup finds it.
// Absent (no webui) → escalate_to_coo reports the capability unavailable.
func lazyTakeoverMinter(reg *trunk.Registry) func() contract.TakeoverMinter {
	var once sync.Once
	var tm contract.TakeoverMinter
	return func() contract.TakeoverMinter {
		once.Do(func() { tm, _ = trunk.TryRequire[contract.TakeoverMinter](reg) })
		return tm
	}
}

// translateFunc adapts a KindTranslate-bound kindChat into the epub tool's
// narrow ChatFunc (system + user text → translated text), so the epub package
// stays decoupled from the pool — it only ever sees "text in, text out".
func translateFunc(kc kindChat) epub.ChatFunc {
	return func(ctx context.Context, system, user string) (string, error) {
		resp, err := kc.chat(ctx, []contract.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

var _ trunk.Module = (*Module)(nil)
