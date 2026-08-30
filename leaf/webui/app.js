// bob webui — canvas frontend (trunk-rebuild).
//
// STRUCTURE (docs/webui-trunk.md §五) — fixed, closed, no exceptions:
//   • TWO top pages: intro (the three-body door) and home (the base graph).
//   • On home there are EXACTLY two DOM elements: one canvas + the dock (the
//     全场唯一输入框). No other DOM exists.
//   • On the canvas there are THREE — and only three — display forms, peers with
//     no hierarchy, freely composable (any form may open any other):
//       layer — a module's main body; persists in the spiral once rendered.
//       model — editable content; input borrows the dock; vanishes on exit.
//       glance — read-only content; vanishes on exit.
//   • The field vocabulary (stat/status/table/text read-only · code/action
//     writable) ALL renders inside these three forms — there is NO fourth form.
//     openForm() rejects any kind outside {layer,model,glance}; the contract has
//     no bespoke-render escape. That closure is the point.
//
// The rendering MECHANISM (DPR canvas, golden-angle spiral, breath+sparks,
// withLayerFrame, the three-body intro sim) is ported verbatim from the skeleton
// (docs/webui.md §5). The DATA layer is generic: the home graph draws one node
// per module-described panel from /api/state; a form renders that panel's fields
// by kind — the frontend knows no subsystem.
(function () {
  'use strict';

  // ===========================================================================
  // §CONFIG — palette + constants
  // ===========================================================================
  const BG0 = '#070605', INK = '#c3bbac', HEAD = '#f1eadd', ACCENT = '#c89b52', DIM = '#6f7c93';
  const EDGE = '#2a2520';
  const STATUS_COL = { ok: '#5fb87a', warn: '#e0a93e', down: '#e0606a', bridge: '#7c8cff', disabled: '#9a5a62', '': '#cfd8e6' };

  const LAYER_HARD_CAP = 20, IDLE_GC_MS = 5 * 60 * 1000, IDLE_GC_TARGET = 10;
  const STALE_MS = 12000;
  const motionOK = !(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);

  // FORM_KINDS is the CLOSED set of canvas display forms. openForm() rejects
  // anything outside it and paintLayer() throws on an unknown kind — there is no
  // fourth form and no bespoke-render escape (the contract carries no Render
  // field). 夹死:闭集,无第4种.
  const FORM_KINDS = { layer: 1, model: 1, glance: 1 };

  // ===========================================================================
  // §CANVAS — DPR-aware sizing
  // ===========================================================================
  const canvas = document.getElementById('graph');
  const ctx = canvas.getContext('2d');
  const dock = document.getElementById('dock'); // the 全场唯一输入框 (command + model editor)
  const effectiveDPR = Math.min(window.devicePixelRatio || 1, 2);
  const UI_SCALE = 1.35; // global zoom: every size is `px * s`, so this bumps the whole UI (fonts + spacing); raised 1.15→1.35 for larger type

  function fit() {
    canvas.width = canvas.clientWidth * effectiveDPR;
    canvas.height = canvas.clientHeight * effectiveDPR;
    const portrait = canvas.clientHeight > canvas.clientWidth;
    dock.classList.toggle('bottom', portrait);
  }
  let fitScheduled = false;
  function scheduleFit() {
    if (fitScheduled) return;
    fitScheduled = true;
    requestAnimationFrame(function () { fitScheduled = false; fit(); });
  }
  window.addEventListener('resize', scheduleFit);

  function devicePos(e) {
    const r = canvas.getBoundingClientRect();
    return { x: (e.clientX - r.left) * effectiveDPR, y: (e.clientY - r.top) * effectiveDPR };
  }

  // ===========================================================================
  // §STATE
  // ===========================================================================
  const ui = {
    t0: performance.now(),
    stack: ['home'],          // recency stack; 'home' is the base (idx 0)
    depthF: { home: 0 },      // eased stack-index per layer
    spiralIndex: { home: 0 }, // golden-angle slot per layer (home = centre)
    layerVitals: {},
    layerPan: {},
    layerBottom: {},          // fg layer's content bottom in screen px, last frame — bounds layerPan.ty (see layerMaxScroll)
    hover: { rowKey: null },
    nodeHits: [],             // home-layer node click targets (device px)
    entityHitRects: [],       // current fg glance-layer hit targets (pre-transform px)
    entityLayerXform: { scale: 1, ox: 0, oy: 0 },
    lastInteractAt: performance.now(),
    lastPollOk: 0,
    admin: false,
    coverage: '',
    result: { line: '', text: '', pending: false, status: '' },  // glance 'result' content (last /api/run output); status = X-Run-Status (ok|error)
    copiedAt: -9,     // elapsed-secs of the last clipboard copy (drives the "✓ copied" flash)
    lastElapsed: 0,   // latest frame's elapsed secs (so click handlers can stamp copiedAt)
    forms: {},                                        // form id → { kind, ... } for every non-home canvas entry
    model: { id: '', panelID: '', setting: '', label: '', lang: '', body: '', orig: '', status: '', statusKind: '', loading: false },
    pick: { id: '', template: '', items: [], filter: '' }, // active picker (dock = filter)
    // dial: the agora org turntable (a "graph" field with layout:"radial"). rot eases
    // toward target; drag spins, click-a-company turns it to centre.
    dial: { rot: 0, target: 0, active: false, selected: '', bases: {}, cx: 0, maxR: 300, drag: { down: false, lastX: 0, moved: 0 } },
    pageOffset: {},   // "panel:field" → current table offset
    tagFilter: {},    // "panel:field" → array of active tags (a TagFilter table's filter-bar selection)
    pageCache: {},    // "panel:field:offset" → TablePage
    pageFetching: {}, // in-flight page fetch guard
    pageNav: null,    // the foreground frame's paged table {key,offset,pageSize,total} → ←/→ keys page it
    takeover: { sid: null, lastSid: null, es: null, img: null, rect: null, down: false, clicks: [], mode: 'watch', btn: null, saveBtn: null, helpBtn: null, qBtn: null, quality: (function () { try { return localStorage.getItem('bobTakeoverQuality') || 'med'; } catch (e) { return 'med'; } })(), connErr: false, evicted: false, effTier: '', help: false, holdTimer: null, leaseFail: false }, // live browser handoff: stream + last frame + painted rect + mouse-down latch + watch/control mode + the toggle/save/help/画质 button rects + requested bandwidth tier + help overlay flag
    routedCov: '',    // last coverage auto-routed to its takeover (so a scoped session lands once)
    viewCache: {},    // "panel:view" → StateField[] (lazy GraphAction.Open views); null = failed
    viewFetching: {}, // in-flight view fetch guard
    // §INTRO splash state — an opaque boot splash painted above everything until
    // dismissed (not a stack layer). alpha/alphaT = fade spring (dismiss only).
    intro: {
      active: true, alpha: 1, alphaT: 1,
      blocks: null, orbs: null, rain: null, fit: null, geo: null, _lastT: 0,
      drag: { orb: null, gux: 0, guy: 0, tux: 0, tuy: 0, sx: 0, sy: 0, down: false, moved: 0 },
    },
  };
  function bumpInteract() { ui.lastInteractAt = performance.now(); }

  // ===========================================================================
  // §UTIL — draw helpers (skeleton §UTIL)
  // ===========================================================================
  function roundRect(x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  }
  function pulse(cx, cy, rad, rgb, peak) {
    if (rad <= 0) return;
    const g = ctx.createRadialGradient(cx, cy, 0, cx, cy, rad);
    g.addColorStop(0, 'rgba(' + rgb + ',' + peak + ')');
    g.addColorStop(1, 'rgba(' + rgb + ',0)');
    ctx.fillStyle = g;
    ctx.beginPath(); ctx.arc(cx, cy, rad, 0, Math.PI * 2); ctx.fill();
  }
  function clip(str, n) {
    const arr = Array.from(str == null ? '' : String(str));
    return arr.length > n ? arr.slice(0, n - 1).join('') + '…' : String(str == null ? '' : str);
  }
  // clipPx truncates to a PIXEL budget using the live ctx.font — so CJK / emoji
  // (which are far wider than a Latin char) don't overrun their column. Caller must
  // have set ctx.font. Returns the string (with a trailing … when cut).
  function clipPx(str, maxPx) {
    str = str == null ? '' : String(str);
    if (maxPx <= 0 || ctx.measureText(str).width <= maxPx) return str;
    const arr = Array.from(str);
    let lo = 0, hi = arr.length;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (ctx.measureText(arr.slice(0, mid).join('') + '…').width <= maxPx) lo = mid; else hi = mid - 1;
    }
    return arr.slice(0, lo).join('') + '…';
  }
  function floatOf(seed, t) {
    return { dx: Math.sin(t * 0.21 + seed) * 4, dy: Math.cos(t * 0.17 + seed * 1.3) * 4 };
  }
  // bezPt evaluates a cubic Bézier at t (for riding a pulse along a curved edge).
  function bezPt(x0, y0, x1, y1, x2, y2, x3, y3, t) {
    const u = 1 - t, a = u * u * u, b = 3 * u * u * t, c = 3 * u * t * t, d = t * t * t;
    return { x: a * x0 + b * x1 + c * x2 + d * x3, y: a * y0 + b * y1 + c * y2 + d * y3 };
  }
  function statusColor(s) { return STATUS_COL[s] || STATUS_COL['']; }
  // wrap hard-wraps a string to <=cols chars per line (by code point, so a split
  // never lands mid-surrogate). Command output is mostly monospace already.
  function wrap(str, cols) {
    const out = [];
    for (const para of String(str == null ? '' : str).split('\n')) {
      const arr = Array.from(para);
      if (arr.length <= cols) { out.push(para); continue; }
      for (let i = 0; i < arr.length; i += cols) out.push(arr.slice(i, i + cols).join(''));
    }
    return out;
  }

  // ===========================================================================
  // §POLL — /api/state + /api/whoami
  // ===========================================================================
  let state = null;
  function poll() {
    fetch('/api/state', { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (s) { state = s; ui.lastPollOk = performance.now(); })
      .catch(function () { /* keep last good state */ });
  }
  function pollWhoami() {
    fetch('/api/whoami', { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (w) {
        ui.coverage = w.coverage || ''; ui.admin = ui.coverage === '*';
        // a scoped capability session (coverage = a sid) lands straight on its
        // takeover, once — the only thing such a session can do.
        if (ui.coverage && ui.coverage !== '*' && ui.routedCov !== ui.coverage) {
          ui.routedCov = ui.coverage; openTakeover(ui.coverage);
        }
      })
      .catch(function () { ui.coverage = ''; ui.admin = false; });
  }
  function connStale() { return performance.now() - ui.lastPollOk > STALE_MS; }
  function panels() { return (state && state.Panels) || []; }
  function panelByID(id) { return panels().filter(function (p) { return p.ID === id; })[0]; }
  // mapNodeActions converts a panel graph node's raw Actions into the glance's action
  // shape. liveGraphNode finds the CURRENT raw node by id in the latest polled state,
  // so a node glance re-derives its detail+buttons live (a pause→resume flips in place
  // after the next poll instead of needing the glance reopened).
  function mapNodeActions(arr) {
    return (arr || []).map(function (a) {
      return { label: a.Label, command: a.Command, pick: (a.Pick || []).map(function (p) { return { value: p.Value, label: p.Label, sub: p.Sub }; }), edit: a.Edit ? { panel: a.Edit.Panel, setting: a.Edit.Setting, lang: a.Edit.Lang } : null, open: a.Open ? { panel: a.Open.Panel, node: a.Open.Node, view: a.Open.View, kind: a.Open.Kind, title: a.Open.Title } : null };
    });
  }
  function liveGraphNode(panelID, nodeID) {
    const p = panelByID(panelID); if (!p) return null;
    for (const fld of (p.State || [])) if (fld.Kind === 'graph') for (const n of (fld.Nodes || [])) if (n.ID === nodeID) return n;
    return null;
  }

  // ===========================================================================
  // §VITALS — breath + sparks (skeleton §VITALS, verbatim)
  // ===========================================================================
  function initVitals(id) {
    if (ui.layerVitals[id]) return;
    ui.layerVitals[id] = { breathPhase: Math.random(), nextSparkAt: 0, sparks: [] };
  }
  function breath(t, phase) {
    const period = 14;
    const s = Math.sin((t / period + phase) * 2 * Math.PI);
    return { dScale: 0.003 * s, dY: 2.5 * s };
  }
  function scheduleNextSpark(id, t, d) {
    const mean = 12 + 8 * Math.min(d, 2);
    ui.layerVitals[id].nextSparkAt = t + mean * (0.5 + Math.random());
  }
  function makeSpark(t, d) {
    return { u: Math.random(), v: Math.random(), bornAt: t, ttl: 0.6, peak: 0.20 - 0.04 * Math.min(d, 2) };
  }
  function tickVitals(elapsed) {
    if (!motionOK) return;
    for (const id of ui.stack) {
      initVitals(id);
      const d = ui.depthF[id];
      if (d == null || d < 0.3) continue;
      const v = ui.layerVitals[id];
      v.sparks = v.sparks.filter(function (sp) { return elapsed - sp.bornAt < sp.ttl; });
      if (v.nextSparkAt === 0) { scheduleNextSpark(id, elapsed, d); continue; }
      if (elapsed >= v.nextSparkAt) { v.sparks.push(makeSpark(elapsed, d)); scheduleNextSpark(id, elapsed, d); }
    }
  }
  function drawSparks(id, fx, fy, fw, fh, elapsed, s, layerAlpha) {
    const v = ui.layerVitals[id];
    if (!v || v.sparks.length === 0) return;
    const prev = ctx.globalAlpha;
    ctx.globalAlpha = Math.min(1, layerAlpha * 1.6);
    for (const sp of v.sparks) {
      const age = (elapsed - sp.bornAt) / sp.ttl;
      if (age < 0 || age > 1) continue;
      const env = age < 0.2 ? age / 0.2 : 1 - (age - 0.2) / 0.8;
      pulse(fx + sp.u * fw, fy + sp.v * fh, 38 * s, '180,200,255', sp.peak * env);
    }
    ctx.globalAlpha = prev;
  }

  // ===========================================================================
  // §LAYERS — stack, spiral, focusTreat, withLayerFrame (skeleton §LAYERS)
  // ===========================================================================
  function showLayer(id) {
    const i = ui.stack.indexOf(id);
    if (i === 0) return;
    if (i < 0) {
      while (ui.stack.length >= LAYER_HARD_CAP) { if (!evictDeepest()) break; }
      if (ui.depthF[id] == null) ui.depthF[id] = ui.stack.length;
      if (ui.spiralIndex[id] == null) ui.spiralIndex[id] = nextFreeSpiralIdx();
      ui.stack.unshift(id);
    } else {
      ui.stack.splice(i, 1);
      ui.stack.unshift(id);
    }
  }
  function popLayer() {
    if (ui.stack.length <= 1) return;
    ui.stack.push(ui.stack.shift());
  }
  // openForm brings a canvas form to the foreground, creating it on first sight.
  // kind MUST be in FORM_KINDS — anything else throws (夹死:无第4种). Opening any
  // form discards an active model (the dock can host only one editor; navigating
  // away from a model = exit it). Forms are peers: any form's content may call
  // this to open any other.
  function openForm(id, meta) {
    if (!FORM_KINDS[meta.kind]) throw new Error('webui: unknown form kind "' + meta.kind + '" — only layer/model/glance exist');
    if (ui.model.id && ui.model.id !== id) exitModel();
    if (ui.pick.id && ui.pick.id !== id) exitPick();
    ui.forms[id] = meta;
    if (meta.kind === 'model') enterModel(id, meta);
    else if (meta.view === 'picker') enterPick(id, meta);
    else if (meta.view === 'fields') {
      // refetch + reset paging on each open: an ephemeral glance closes via removeForm,
      // so a reopen must start at the newest page, not a stale offset left in pageOffset
      // from the previous visit (the view's page 0 is the fresh f.Rows).
      const pre = meta.panelID + ':';
      for (const k in ui.pageOffset) if (k.indexOf(pre) === 0) delete ui.pageOffset[k];
      delete ui.viewCache[meta.panelID + ':' + meta.viewID];
      fetchView(meta.panelID, meta.viewID);
    }
    showLayer(id);
  }
  // openTarget descends into a ViewTarget (GraphAction.Open or Cell.Open): a node id
  // opens that node's detail glance (the drill-down — same form a graph-node click
  // makes); otherwise a viewID opens the panel's parameterized fields. One path for
  // both the top action-buttons and the clickable table cells.
  function openTarget(panel, node, view, kind, title) {
    if (node) openForm('glance:node:' + node, { kind: 'glance', view: 'text', nodeID: node, panelID: panel, title: title });
    else openForm('view:' + panel + ':' + view, { kind: FORM_KINDS[kind] ? kind : 'glance', view: 'fields', panelID: panel, viewID: view, title: title });
  }
  // ===========================================================================
  // §TAKEOVER — live browser handoff (docs/wip-takeover-port.md). A `layer` with
  // view:'takeover' paints the screencast frames on the canvas; mouse rides the
  // canvas, text rides the dock. The SSE is foreground-bound (decision 5): started
  // when the takeover layer is foreground, stopped on recede, resumed on return.
  function openTakeover(sid) {
    if (!sid) return;
    openForm('takeover:' + sid, { kind: 'layer', view: 'takeover', sid: sid, title: 'takeover · ' + sid });
  }
  function isTakeoverFg() { const f = ui.forms[ui.stack[0]]; return !!(f && f.view === 'takeover'); }
  // syncTakeover (called each frame) starts/stops the screencast to match the
  // foreground form. The last frame is kept across a stop so a return doesn't flash.
  function syncTakeover() {
    const fg = ui.forms[ui.stack[0]];
    const want = (fg && fg.view === 'takeover') ? fg.sid : null;
    if (ui.takeover.sid === want) return;
    // Leaving the current takeover (recede / switch sid): release any held control
    // (→ bob resumes) and stop the heartbeat, while ui.takeover.sid is still the OLD sid.
    stopHoldBeat(ui.takeover.mode === 'control');
    ui.takeover.mode = 'watch'; ui.takeover.leaseFail = false; ui.takeover.btn = null;
    ui.takeover.sid = want;
    if (!want) { if (ui.takeover.es) { ui.takeover.es.close(); ui.takeover.es = null; } if (dock.value && document.activeElement === dock) { dock.value = ''; dock.blur(); } return; } // receded → stop (last frame kept for a same-sid resume)
    if (want !== ui.takeover.lastSid) { ui.takeover.img = null; ui.takeover.rect = null; } // a DIFFERENT session → never paint/click the prior session's frame
    ui.takeover.lastSid = want;
    openScreencast(want); // owns the close-then-open teardown (single point); recede above closes explicitly
  }
  // openScreencast (re)opens the SSE for sid at the current bandwidth tier
  // (ui.takeover.quality → ?q=). Factored out so the 画质 button can reconnect at a
  // new tier without a sid change. The tier is a REQUEST — the backend clamps it
  // down under load and may also refuse outright when at capacity (503 → onerror).
  function openScreencast(sid) {
    if (ui.takeover.es) { ui.takeover.es.close(); ui.takeover.es = null; }
    ui.takeover.connErr = false; // fresh attempt
    ui.takeover.evicted = false; // re-opening clears a prior "taken over elsewhere"
    ui.takeover.effTier = ''; // unknown until the backend reports the EFFECTIVE tier
    const q = ui.takeover.quality || 'med';
    const es = new EventSource('/api/browser/screencast?sid=' + encodeURIComponent(sid) + '&q=' + encodeURIComponent(q));
    es.onmessage = function (e) {
      // In-band control lines (a base64 JPEG frame can never start with '~', so these are
      // unambiguous and proxy-transparent):
      //  ~tier:<t> — the EFFECTIVE tier (post load-clamp); the 画质 button shows what's
      //              actually running, which may be below the requested tier when busy.
      //  ~evicted  — a newer viewer took over this browser; stop and do NOT auto-reconnect
      //              (reopening would ping-pong against the new viewer).
      if (e.data && e.data.indexOf('~tier:') === 0) { ui.takeover.effTier = e.data.slice(6); return; }
      if (e.data === '~evicted') {
        // We lost this browser to a newer viewer: drop to watch + stop the hold heartbeat
        // (without POSTing release — the NEW viewer owns the lease now, releasing would
        // clear theirs), so we stop sending input/hold-renewals to a browser we don't own,
        // and the 🎮/green-border chrome stops contradicting the "taken over" overlay.
        ui.takeover.evicted = true;
        stopHoldBeat(false);
        ui.takeover.mode = 'watch'; ui.takeover.leaseFail = false;
        if (ui.takeover.es) { ui.takeover.es.close(); ui.takeover.es = null; }
        return;
      }
      ui.takeover.connErr = false; // frames flowing → healthy (clears a prior failure)
      const img = new Image();
      img.onload = function () { if (ui.takeover.sid === sid) ui.takeover.img = img; };
      img.src = 'data:image/jpeg;base64,' + e.data;
    };
    es.onerror = function () {
      // A non-200 OPEN (e.g. the 503 admission cap "capacity reached") is a PERMANENT
      // failure: EventSource sets readyState=CLOSED and does NOT auto-reconnect. A
      // transient drop AFTER a successful open instead goes CONNECTING and self-heals,
      // so only the CLOSED case is surfaced as a hard error (else keep painting/last frame).
      if (es.readyState === 2 /* CLOSED */) ui.takeover.connErr = true;
    };
    ui.takeover.es = es;
  }
  // TAKEOVER_TIERS / cycleTakeoverQuality — the quality button cycles Hi→Md→Lo→5s→Hi,
  // persisting the choice (so a bad-link user stays low) and reconnecting the live
  // stream at the new tier. Quality (JPEG) is held constant server-side; Hi/Md/Lo trade
  // resolution + frame rate (text stays legible). "slow" (5s) is a manual-only mode:
  // med resolution but ~1 frame / 5s — the backend never auto-selects it.
  const TAKEOVER_TIERS = ['high', 'med', 'low', 'slow'];
  function cycleTakeoverQuality() {
    const i = TAKEOVER_TIERS.indexOf(ui.takeover.quality);
    ui.takeover.quality = TAKEOVER_TIERS[(i + 1) % TAKEOVER_TIERS.length] || 'med';
    try { localStorage.setItem('bobTakeoverQuality', ui.takeover.quality); } catch (e) { }
    if (ui.takeover.sid) openScreencast(ui.takeover.sid); // reconnect at the new tier
  }
  // takeoverPoint maps a pointer event to browser-viewport coords inside the painted
  // frame, or null when outside it. Inverts the layer transform like the hit-rects.
  // localPos inverts the foreground layer transform: device coords → this layer's local
  // space (the space the frame + chip rects are recorded in). Shared by takeoverPoint /
  // takeoverPointClamped so they never drift.
  function localPos(e) {
    const p = devicePos(e), t = ui.entityLayerXform;
    return { lx: (p.x - t.ox) / t.scale, ly: (p.y - t.oy) / t.scale };
  }
  function takeoverPoint(e) {
    const r = ui.takeover.rect; if (!r) return null;
    const lp = localPos(e);
    if (lp.lx < r.dx || lp.lx > r.dx + r.dw || lp.ly < r.dy || lp.ly > r.dy + r.dh) return null;
    return { x: (lp.lx - r.dx) / r.dw * r.nw, y: (lp.ly - r.dy) / r.dh * r.nh };
  }
  // takeoverPointClamped is takeoverPoint but clamped to the frame edge instead of
  // returning null outside it — so a drag that RELEASES off the frame still sends a
  // release (no stuck mouse-button in the remote browser).
  function takeoverPointClamped(e) {
    const r = ui.takeover.rect; if (!r) return null;
    const lp = localPos(e);
    const lx = Math.max(r.dx, Math.min(r.dx + r.dw, lp.lx));
    const ly = Math.max(r.dy, Math.min(r.dy + r.dh, lp.ly));
    return { x: (lx - r.dx) / r.dw * r.nw, y: (ly - r.dy) / r.dh * r.nh };
  }
  // postBrowserInput posts one event regardless of mode. sendInput gates it on control
  // mode; the release of an in-flight press uses postBrowserInput directly so a press
  // begun in control always completes — even if control was dropped mid-drag (otherwise
  // the remote browser keeps the button held).
  function postBrowserInput(ev) {
    if (!ui.takeover.sid) return;
    fetch('/api/browser/input?sid=' + encodeURIComponent(ui.takeover.sid), { method: 'POST', body: JSON.stringify(ev) }).catch(function () { });
  }
  let lastMoveSentAt = 0; // throttle clock for the mousemove flood (move only — see sendInput)
  function sendInput(ev) {
    if (ui.takeover.mode !== 'control') return; // only control mode drives the browser
    // Throttle ONLY mousemove (~25Hz): a drag fires a pointermove per pixel, hundreds/sec,
    // which floods browserd AND queues a following click behind stale moves in the browser's
    // ~6-connection-per-origin pool (so the click feels "stuck"). press/wheel/text are
    // low-rate and pass through untouched; release never reaches here (posts raw) so a held
    // button always lifts. A dropped trailing move only leaves the cursor a pixel or two behind.
    if (ev.kind === 'mouse' && ev.mouseType === 'move') {
      const now = performance.now();
      if (now - lastMoveSentAt < 40) return;
      lastMoveSentAt = now;
    }
    postBrowserInput(ev);
  }
  // Watch ⇄ control. Control drives the human-control hold on bob's side: a 10s
  // heartbeat (POST on:true) pauses bob's browser for this sid; switching back to watch
  // (or leaving) releases it (POST on:false) which, for an auto worker, wakes it to
  // continue. The lease self-expires server-side, so a dead tab never wedges bob.
  function setTakeoverMode(mode) {
    if (!ui.takeover.sid || ui.takeover.mode === mode) return;
    ui.takeover.mode = mode;
    if (mode === 'control') { ui.takeover.leaseFail = false; postHold(true); startHoldBeat(); }
    else stopHoldBeat(true); // hand back → release (bob resumes)
  }
  function postHold(on) {
    const sid = ui.takeover.sid; if (!sid) return;
    fetch('/api/browser/hold?sid=' + encodeURIComponent(sid), {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ on: on }), keepalive: true,
    }).then(function (r) {
      // Renewal refused while controlling (session expired / bob restarted) → bob will
      // resume in ~30s; drop to watch loudly so human and bob don't fight the page.
      if (on && !r.ok && ui.takeover.mode === 'control') { stopHoldBeat(false); ui.takeover.mode = 'watch'; ui.takeover.leaseFail = true; }
    }).catch(function () { });
  }
  function startHoldBeat() { if (!ui.takeover.holdTimer) ui.takeover.holdTimer = setInterval(function () { postHold(true); }, 10000); }
  function stopHoldBeat(release) {
    if (ui.takeover.holdTimer) { clearInterval(ui.takeover.holdTimer); ui.takeover.holdTimer = null; }
    if (release) postHold(false);
  }
  // inTakeoverButton — is the pointer over the screen-fixed mode toggle (drawn beside the
  // dock by drawTakeoverButton). Screen coords (devicePos), NOT layer-inverted: the button
  // lives outside the takeover layer's transform, pinned to the dock.
  function inTakeoverButton(e) {
    const b = ui.takeover.btn; if (!b) return false;
    const p = devicePos(e);
    return p.x >= b.x && p.x <= b.x + b.w && p.y >= b.y && p.y <= b.y + b.h;
  }
  // inSaveButton — pointer over the 💾 保存登陆 button (right of the toggle). Screen coords.
  function inSaveButton(e) {
    const b = ui.takeover.saveBtn; if (!b) return false;
    const p = devicePos(e);
    return p.x >= b.x && p.x <= b.x + b.w && p.y >= b.y && p.y <= b.y + b.h;
  }
  // inHelpButton — pointer over the ❓ help button (right of 💾). Screen coords.
  function inHelpButton(e) {
    const b = ui.takeover.helpBtn; if (!b) return false;
    const p = devicePos(e);
    return p.x >= b.x && p.x <= b.x + b.w && p.y >= b.y && p.y <= b.y + b.h;
  }
  // inQualityButton — pointer over the Hi/Md/Lo/5s quality button (right of ❓). Screen coords.
  function inQualityButton(e) {
    const b = ui.takeover.qBtn; if (!b) return false;
    const p = devicePos(e);
    return p.x >= b.x && p.x <= b.x + b.w && p.y >= b.y && p.y <= b.y + b.h;
  }
  // inLogoutButton — pointer over the ⎋ logout chip (right of the dock, admin only). Screen coords.
  function inLogoutButton(e) {
    const b = ui.logoutBtn; if (!b) return false;
    const p = devicePos(e);
    return p.x >= b.x && p.x <= b.x + b.w && p.y >= b.y && p.y <= b.y + b.h;
  }
  // doLogout (F102) — end the management session server-side, then drop this client to
  // read-only regardless of the result (a network error still clears local admin so a
  // half-online session can't linger). pollWhoami re-syncs coverage from the server.
  function doLogout() {
    const done = function () {
      dock.value = ''; dock.blur();
      dock.placeholder = 'paste a /webui token to unlock management';
      ui.admin = false; ui.coverage = '';
      pollWhoami(); poll();
    };
    fetch('/api/logout', { method: 'POST' }).then(done, done);
  }
  // doSaveLogin — 保存登陆: capture THIS copy's live login into its member master (cookies
  // incl session cookies, no close), release control + resume the worker on the SAME live,
  // logged-in copy, then recede. On the server side /save captures FIRST, then clears the hold
  // + resumes. Stop the heartbeat BEFORE the POST so no stray Set re-asserts the hold after the
  // server clears it. On failure the login was NOT saved — keep control + flag it for retry.
  function doSaveLogin() {
    const sid = ui.takeover.sid; if (!sid) return;
    const wasControl = ui.takeover.mode === 'control';
    stopHoldBeat(false);
    fetch('/api/browser/save?sid=' + encodeURIComponent(sid), { method: 'POST' }).then(function (r) {
      if (r.ok) {
        ui.takeover.mode = 'watch';
        if (ui.forms[ui.stack[0]] && ui.forms[ui.stack[0]].kind === 'layer') popLayer(); // syncTakeover closes the stream
        return;
      }
      if (wasControl) { postHold(true); startHoldBeat(); } // save failed → keep control
      ui.takeover.leaseFail = true;                        // ⚠️ surfaces it; human retries 💾
    }).catch(function () { if (wasControl) { postHold(true); startHoldBeat(); } ui.takeover.leaseFail = true; });
  }
  // paintTakeover draws the cached frame letterboxed into the layer frame and records
  // the painted rect (for input coord mapping). "connecting" until the first frame.
  function paintTakeover(api, f, s) {
    const img = ui.takeover.img;
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    if (!img || !img.naturalWidth) {
      ui.takeover.rect = null;
      ctx.fillStyle = DIM; ctx.font = 13 * s + 'px ui-monospace, Menlo, monospace';
      ctx.textAlign = 'center';
      const msg = ui.takeover.evicted ? 'This browser was taken over elsewhere.'
        : ui.takeover.connErr ? 'Can’t connect — busy or unavailable. Retry shortly.'
          : ('… connecting to ' + (f.sid || '') + ' …');
      ctx.fillText(msg, api.cx, api.cy);
      ctx.textAlign = 'left';
      return;
    }
    // Maximize the screencast: fill the whole canvas MINUS the dock strip (the dock sits
    // at the top in landscape, bottom in portrait — read its real rect so the frame uses
    // every other pixel), letterboxed to keep the browser aspect. The mode toggle lives
    // beside the dock (drawTakeoverButton), not on the frame, so nothing overlays the page.
    const dr = dock.getBoundingClientRect(), dpr = effectiveDPR, M = 14 * s, gap = 12 * s;
    const portrait = canvas.height > canvas.width;
    const by = portrait ? gap : dr.bottom * dpr + gap;
    const botB = portrait ? dr.top * dpr - gap : api.cy * 2 - gap;
    const bx = M, bw = api.cx * 2 - 2 * M, bh = botB - by;
    const sc = Math.min(bw / img.naturalWidth, bh / img.naturalHeight);
    const dw = img.naturalWidth * sc, dh = img.naturalHeight * sc;
    const dx = bx + (bw - dw) / 2, dy = by + (bh - dh) / 2;
    ctx.drawImage(img, dx, dy, dw, dh);
    ui.takeover.rect = { dx: dx, dy: dy, dw: dw, dh: dh, nw: img.naturalWidth, nh: img.naturalHeight };
    // Mode border on the frame (control = green/live, watch = dim) — a subtle "you're live"
    // cue; the toggle itself is the dock-side button.
    const ctrl = ui.takeover.mode === 'control';
    ctx.strokeStyle = ctrl ? ACCENT : '#3a4250'; ctx.lineWidth = (ctrl ? 2 : 1) * s;
    ctx.strokeRect(dx, dy, dw, dh);

    // Click ripples — a short expanding ring per press (see the pointerdown handler).
    // Local + immediate, so a trailing screencast still acknowledges the click. ACCENT
    // (#c89b52) = the live/control accent, so it reads as "you're driving".
    if (ui.takeover.clicks.length) {
      const tnow = performance.now(), RIPPLE_MS = 350;
      for (const c of ui.takeover.clicks) {
        const age = tnow - c.t; if (age < 0 || age > RIPPLE_MS) continue;
        const k = age / RIPPLE_MS; // 0→1 over the ring's life
        ctx.beginPath();
        ctx.arc(c.x, c.y, (5 + 16 * k) * s, 0, Math.PI * 2);
        ctx.strokeStyle = 'rgba(200,155,82,' + (0.85 * (1 - k)).toFixed(3) + ')';
        ctx.lineWidth = 2 * s; ctx.stroke();
      }
      ui.takeover.clicks = ui.takeover.clicks.filter(function (c) { return tnow - c.t <= RIPPLE_MS; });
    }

    // Stream interrupted → dim the now-stale frame so it's obviously not live. Keyed on
    // the EventSource connection state, NOT frame timing: the 15s server heartbeat keeps
    // a live-but-static stream OPEN, so this fires only on a real drop / reconnect.
    if (!ui.takeover.es || ui.takeover.es.readyState !== 1 /* EventSource.OPEN */) {
      ctx.fillStyle = 'rgba(0,0,0,0.55)';
      ctx.fillRect(dx, dy, dw, dh);
      ctx.fillStyle = '#cfd8e6'; ctx.font = 'bold ' + 13 * s + 'px ui-monospace, Menlo, monospace';
      ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
      const lost = ui.takeover.evicted ? 'This browser was taken over elsewhere.'
        : ui.takeover.connErr ? 'Connection closed — busy or unavailable. Retry shortly.'
          : 'Signal lost — reconnecting…';
      ctx.fillText(lost, dx + dw / 2, dy + dh / 2);
      ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    }
  }

  // removeForm drops a form entirely (the ephemeral path): out of the stack, out
  // of the spiral bookkeeping, out of the registry.
  function removeForm(id) {
    const i = ui.stack.indexOf(id);
    if (i >= 0) ui.stack.splice(i, 1);
    removeLayer(id); // also deletes ui.forms[id]
  }
  // closeForm handles Esc / click-away on the foreground form, by kind: a layer
  // recedes into the spiral (persists), a model/glance vanishes (ephemeral).
  function closeForm() {
    const id = ui.stack[0];
    if (id === 'home') return;
    const f = ui.forms[id];
    if (f && f.kind === 'layer') { popLayer(); return; } // persists in the spiral
    if (f && f.kind === 'model') exitModel();
    if (f && f.view === 'picker') exitPick();
    removeForm(id);                                       // model / glance: gone on exit
  }
  // navForward is closeForm's inverse for navigation: it brings the most-recently
  // receded entry back to the front. Esc steps BACK through the layers (skeleton:
  // sequential, no direct jump, no clicking faint background cards — those get
  // lost in the spiral); Tab steps FORWARD (same side as Esc, one-handed). Layers
  // persist (only GC/cap remove them). Both ignored while the dock has focus.
  function navForward() {
    if (ui.stack.length <= 1) return;
    ui.stack.unshift(ui.stack.pop());
  }
  function nextFreeSpiralIdx() {
    const used = new Set(Object.values(ui.spiralIndex));
    for (let i = 1; i < LAYER_HARD_CAP + 1; i++) if (!used.has(i)) return i;
    return LAYER_HARD_CAP;
  }
  function removeLayer(id) {
    delete ui.depthF[id]; delete ui.layerVitals[id]; delete ui.layerPan[id]; delete ui.layerBottom[id]; delete ui.spiralIndex[id];
    delete ui.forms[id]; // also drop the form record so cap/idle eviction can't leak ui.forms
  }
  function evictDeepest() {
    for (let i = ui.stack.length - 1; i >= 1; i--) {
      const id = ui.stack[i];
      if (id === 'home') continue;
      ui.stack.splice(i, 1);
      removeLayer(id);
      return id;
    }
    return null;
  }
  function gcLayers(now) {
    if (now - ui.lastInteractAt < IDLE_GC_MS) return;
    while (ui.stack.length > IDLE_GC_TARGET) if (!evictDeepest()) break;
  }
  function spiralOffset(idx, d) {
    if (idx === 0) return { dx: 0, dy: 0 };
    const angle = idx * 2.39996;
    const homeR = 300 * Math.sqrt(idx);
    const f = Math.min(1, d);
    return { dx: homeR * f * Math.cos(angle), dy: homeR * f * Math.sin(angle) };
  }
  function focusTreat(id, t) {
    const d = ui.depthF[id] != null ? ui.depthF[id] : 1;
    const idx = ui.spiralIndex[id] || 0;
    let dScale = 0, dY = 0;
    const targetIdx = ui.stack.indexOf(id);
    const settledness = targetIdx < 0 ? 0 : Math.max(0, 1 - Math.abs(d - targetIdx) * 5);
    const ampLinear = Math.min(1, Math.max(0, (d - 0.05) / 0.90));
    const vitalsAmp = settledness * ampLinear * ampLinear * (3 - 2 * ampLinear);
    if (motionOK && t != null && vitalsAmp > 0 && ui.layerVitals[id]) {
      const b = breath(t, ui.layerVitals[id].breathPhase);
      dScale = b.dScale * vitalsAmp;
      dY = b.dY * vitalsAmp;
    }
    const sp = spiralOffset(idx, d);
    const baseY = idx === 0 ? -14 * d : 0;
    return {
      alpha: Math.max(0.06, 1 - 0.74 * d),
      scale: Math.max(0.84, 1 - 0.06 * d) + dScale,
      yOffset: baseY + dY + sp.dy,
      xOffset: sp.dx,
    };
  }
  function withLayerFrame(id, s, elapsed, body) {
    const tr = focusTreat(id, elapsed);
    const isFg = ui.stack[0] === id;
    if (isFg) { ui.entityHitRects = []; ui.pageNav = null; } // a paged table re-arms pageNav below if one is on this frame
    ctx.save();
    const cx = canvas.width / 2, cy = canvas.height / 2;
    const pan = ui.layerPan[id] || { tx: 0, ty: 0 };
    // Re-clamp before painting: content can SHRINK between frames (rows went away, a
    // table paged back, the body stopped opting in), which would otherwise strand ty
    // below the new floor and park the layer in blank space with no way back.
    clampLayerPan(id, pan);
    ctx.translate(cx + tr.xOffset * s + pan.tx, cy + tr.yOffset * s + pan.ty);
    ctx.scale(tr.scale, tr.scale);
    ctx.translate(-cx, -cy);
    ctx.globalAlpha = tr.alpha;
    if (isFg) {
      ui.entityLayerXform.scale = tr.scale;
      ui.entityLayerXform.ox = cx + tr.xOffset * s + pan.tx - tr.scale * cx;
      ui.entityLayerXform.oy = cy + tr.yOffset * s + pan.ty - tr.scale * cy;
    }
    const W = Math.min(760 * s, canvas.width - 80 * s);
    const endY = body({ cx: cx, cy: cy, alpha: tr.alpha, isFg: isFg, W: W, s: s, leftX: cx - W / 2, topY: cy - 270 * s, bottomHintY: cy + 280 * s });
    // A body that reports where it stopped opts INTO scrolling; one that returns
    // nothing (takeover, viewport-anchored graphs) keeps the fixed frame. Reuse the
    // xform just written above rather than re-deriving it — that pair is what click
    // and hover invert, so a second hand-rolled copy could drift and put the scroll
    // bounds somewhere the hit-rects aren't. Clearing it is also what re-zeroes ty,
    // via the clamp at the top of the next frame.
    if (isFg) {
      ui.layerBottom[id] = typeof endY === 'number'
        ? ui.entityLayerXform.oy + ui.entityLayerXform.scale * endY - pan.ty
        : undefined;
    }
    drawSparks(id, 0, 0, canvas.width, canvas.height, elapsed, s, tr.alpha);
    ctx.restore();
    ctx.globalAlpha = 1;
  }

  // layerMaxScroll — how far this layer may scroll UP, in screen px (0 = it fits).
  // The 28 is `* s` like every other size in this file: layerBottom and canvas.height
  // are device px, so an unscaled pad would shrink to ~10px on a 2× display.
  function layerMaxScroll(id) {
    const b = ui.layerBottom[id];
    if (b == null) return 0;
    return Math.max(0, b + 28 * (effectiveDPR * UI_SCALE) - canvas.height);
  }
  // clampLayerPan — the single owner of the scroll range. Both the paint path and the
  // wheel handler go through here so the bounds can only ever be changed in one place.
  function clampLayerPan(id, pan) {
    const p = pan || ui.layerPan[id];
    if (!p) return 0;
    const maxSc = layerMaxScroll(id);
    p.ty = Math.max(-maxSc, Math.min(0, p.ty));
    return maxSc;
  }

  // ===========================================================================
  // §UI-PRIMS — paintButton / paintSectionHeader
  // ===========================================================================
  function paintButton(x, y, w, h, label, o) {
    o = o || {};
    const s = o.s != null ? o.s : 1;
    const hovered = o.hoverKey && ui.hover.rowKey === o.hoverKey;
    ctx.fillStyle = hovered ? '#2b3445' : (o.fill || '#1f2937');
    ctx.strokeStyle = hovered ? ACCENT : (o.stroke || '#4a5666');
    ctx.lineWidth = 1 * s;
    roundRect(x, y, w, h, 4 * s); ctx.fill(); ctx.stroke();
    ctx.fillStyle = o.textColor || '#d6d9dd';
    ctx.font = 'bold ' + (o.fontSize || 12) * s + 'px ui-monospace, Menlo, monospace';
    ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
    ctx.fillText(label, x + w / 2, y + h / 2);
    if (o.hitRects && o.action) o.hitRects.push({ x: x, y: y, w: w, h: h, action: o.action, hoverKey: o.hoverKey });
  }
  // copyToClipboard writes text to the clipboard on a click gesture (so the write is
  // allowed); navigator.clipboard first, a hidden-textarea+execCommand fallback for
  // non-secure-context origins (http LAN). Sets copiedAt → the "✓ copied" flash.
  function copyToClipboard(text) {
    const done = function () { ui.copiedAt = ui.lastElapsed; };
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () { if (fallbackCopy(text)) done(); });
        return;
      }
    } catch (_) { }
    if (fallbackCopy(text)) done();
  }
  function fallbackCopy(text) {
    try {
      const ta = document.createElement('textarea');
      ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.focus(); ta.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(ta);
      return ok;
    } catch (_) { return false; }
  }
  function paintSectionHeader(x, y, w, title, o) {
    o = o || {};
    const s = o.s != null ? o.s : 1;
    ctx.fillStyle = '#a4b6d4';
    ctx.font = 'bold ' + 13 * s + 'px ui-monospace, Menlo, monospace';
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    ctx.fillText(title, x, y);
    return y + 18 * s;
  }

  // ===========================================================================
  // §PANEL-RENDER — generic: home node ring + the three canvas forms
  // (layer/model/glance) + per-kind field renderers
  // ===========================================================================

  // headline returns the node's one-line summary: the first stat/status field.
  function headline(p) {
    const fs = (p.State) || [];
    for (const f of fs) {
      if (f.Kind === 'stat') return f.Label + ': ' + f.Value + (f.Unit ? ' ' + f.Unit : '');
      if (f.Kind === 'status') return f.Label + ': ' + f.Status;
    }
    return '';
  }
  // worstStatus returns the most-severe status color among a panel's fields.
  function worstStatus(p) {
    let rank = 0; // 0 ok/none, 1 warn, 2 down
    const bump = function (st) { if (st === 'down') rank = Math.max(rank, 2); else if (st === 'warn') rank = Math.max(rank, 1); };
    for (const f of (p.State || [])) {
      if (f.Kind === 'status' || f.Kind === 'stat') bump(f.Status);
      if (f.Kind === 'table') for (const row of (f.Rows || [])) for (const c of (row || [])) bump(c.Status);
    }
    return rank === 2 ? 'down' : rank === 1 ? 'warn' : '';
  }

  // hasContent reports whether a panel has anything worth a layer. A module that
  // leaves State AND Settings empty is a presence-only node ("我在这里,里面没东西")
  // — drawn but not clickable; anything non-empty opens a layer. The gate is the
  // module's own choice (it self-describes), computed on the auth-redacted state
  // the frontend holds, so a /webui token naturally reveals more on the next poll.
  function hasContent(p) {
    return !!((p.State && p.State.length) || (p.Settings && p.Settings.length));
  }

  // drawNode paints one home-graph node box (shared by the bob root + tier nodes).
  // A content-less / root node renders dimmer; the title stays bright for bob.
  function drawNode(b, w, h, s, title, sub, wc, open, hovered, isRoot) {
    ctx.fillStyle = hovered ? '#171b25' : '#101218';
    ctx.strokeStyle = wc ? statusColor(wc) : (open || isRoot) ? (hovered ? ACCENT : '#33405a') : '#23272f';
    ctx.lineWidth = 1.5 * s;
    roundRect(b.x, b.y, w, h, 6 * s); ctx.fill(); ctx.stroke();
    ctx.fillStyle = (open || isRoot) ? HEAD : DIM;
    ctx.font = 'bold ' + 12.5 * s + 'px ui-monospace, Menlo, monospace';
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    ctx.fillText(clip(title, 20), b.x + 11 * s, b.y + 9 * s);
    ctx.fillStyle = wc ? statusColor(wc) : DIM;
    ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
    ctx.fillText(clip(sub, 22), b.x + 11 * s, b.y + 28 * s);
  }

  // drawGraph renders a generic banded node/edge graph — the reusable geometry
  // behind the home module graph and (later) agora's org graph. Nodes are laid in
  // horizontal bands (top→bottom); edges are cubic-Bézier curves routed through the
  // channel just ABOVE the lower endpoint's band, so they arc over node rows rather
  // than through them; nodes are content-driven (interactive ones get a hit-rect).
  // Purely data-driven — no per-caller branches. spec:
  //   bands: [{key, label, emptyHint}]                       (ordered top→bottom)
  //   nodes: [{id, band, title, sub, status, interactive, root, onClick}]
  //   edges: [{from, to, label, weight}]
  // hits: array to push interactive node hit-rects into ({x,y,w,h,id,action}) —
  // null to draw without registering clicks (e.g. a backgrounded layer).
  function drawGraph(api, spec, s, elapsed, hits, top, bottom) {
    const W = canvas.width, cx = api.cx, cy = api.cy;
    const bands = spec.bands || [], nodes = spec.nodes || [], edges = spec.edges || [];
    const nodeW = Math.min(168 * s, Math.max(104 * s, (W - 48 * s) / 5));
    const nodeH = 52 * s, gap = 14 * s, rowGap = 20 * s;

    // band centre Y + index. top/bottom are CONTENT bounds; inset the band centres
    // by nodeH/2 so nodes (which extend ±nodeH/2) never bleed past [top, bottom]
    // (else the top band overlaps the section header and the bottom band overlaps
    // the settings buttons below the graph).
    const inTop = top + nodeH / 2, inBot = bottom - nodeH / 2;
    const bandY = {}, bandIdx = {};
    for (let i = 0; i < bands.length; i++) {
      bandY[bands[i].key] = bands.length <= 1 ? (inTop + inBot) / 2 : inTop + (inBot - inTop) * (i / (bands.length - 1));
      bandIdx[bands[i].key] = i;
    }

    // group nodes by band, then place each band's nodes in centered, wrapping rows.
    const byBand = {}, box = {}, nodeBand = {};
    for (const b of bands) byBand[b.key] = [];
    for (const n of nodes) { (byBand[n.band] || (byBand[n.band] = [])).push(n); nodeBand[n.id] = n.band; }
    for (const b of bands) {
      const list = byBand[b.key], yc = bandY[b.key];
      const perRow = Math.max(1, Math.floor((W - 56 * s) / (nodeW + gap)));
      const rows = Math.max(1, Math.ceil(list.length / perRow));
      const yTop = yc - ((rows - 1) * (nodeH + rowGap) + nodeH) / 2;
      for (let i = 0; i < list.length; i++) {
        const n = list[i], row = Math.floor(i / perRow);
        const inRow = Math.min(perRow, list.length - row * perRow);
        const totalW = inRow * nodeW + (inRow - 1) * gap, idx = i % perRow;
        const fl = floatOf((n.id.charCodeAt(0) || 0) + i, elapsed);
        const ccx = cx - totalW / 2 + idx * (nodeW + gap) + nodeW / 2 + fl.dx * s;
        const ccy = yTop + row * (nodeH + rowGap) + nodeH / 2 + fl.dy * s;
        box[n.id] = { cx: ccx, cy: ccy, x: ccx - nodeW / 2, y: ccy - nodeH / 2 };
      }
    }

    // band labels (left margin) + empty-band hints.
    ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
    for (const b of bands) {
      ctx.textAlign = 'left'; ctx.fillStyle = '#4a5666';
      ctx.fillText(b.label, 16 * s, bandY[b.key] - nodeH / 2 - 4 * s);
      if (!byBand[b.key].length && b.emptyHint) {
        ctx.textAlign = 'center'; ctx.fillStyle = '#3a4150';
        ctx.fillText(b.emptyHint, cx, bandY[b.key] - 6 * s);
      }
    }

    // edges: route each through the channel just ABOVE the lower endpoint's band,
    // attaching each end on the side facing the rail — the curve leaves vertically,
    // arcs across the channel, descends into the target, passing OVER node rows.
    ctx.lineWidth = 1.2 * s;
    let ei = 0;
    for (const e of edges) {
      const a = box[e.from], b = box[e.to];
      if (!a || !b) continue;
      const loIdx = Math.max(bandIdx[nodeBand[e.from]] || 0, bandIdx[nodeBand[e.to]] || 0);
      const railBase = loIdx > 0
        ? (bandY[bands[loIdx - 1].key] + bandY[bands[loIdx].key]) / 2
        : bandY[bands[0].key] - (nodeH / 2 + 16 * s);
      const rail = railBase - (ei % 4) * 6 * s; ei++;
      const ax = a.cx, bx = b.cx;
      const ay = rail < a.cy ? a.y : a.y + nodeH;
      const by = rail < b.cy ? b.y : b.y + nodeH;
      ctx.strokeStyle = '#2f4063';
      ctx.beginPath(); ctx.moveTo(ax, ay); ctx.bezierCurveTo(ax, rail, bx, rail, bx, by); ctx.stroke();
      const peak = 0.16 * (e.weight ? Math.min(1 + e.weight * 0.3, 2) : 1);
      const pt = bezPt(ax, ay, ax, rail, bx, rail, bx, by, (elapsed * 0.4) % 1);
      pulse(pt.x, pt.y, 13 * s, '120,170,255', peak);
      if (e.label) {
        ctx.textAlign = 'center'; ctx.fillStyle = '#56627a'; ctx.font = 9.5 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText(clip(e.label, 14), (ax + bx) / 2, rail - 7 * s);
        ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
      }
    }

    // nodes.
    for (const n of nodes) {
      const b = box[n.id]; if (!b) continue;
      const interactive = !!n.interactive;
      const hovered = interactive && ui.hover.rowKey === 'node:' + n.id;
      drawNode(b, nodeW, nodeH, s, n.title, n.sub || '', n.status || '', interactive, hovered, n.root);
      if (hits && interactive) hits.push({ x: b.x, y: b.y, w: nodeW, h: nodeH, id: n.id, action: n.onClick });
    }
  }

  // drawOrgRadial renders a "graph" field with layout:"radial" as a TURNTABLE: origin
  // near top, rings fanning down (company inner / members mid / inboxes outer; bridge
  // pinned near centre). Each company is a FIXED ~40° slot; the whole wheel rotates
  // (ui.dial) — drag spins it, clicking a company turns that slot to dead-centre.
  // Off-screen slots are culled. Reuses statusColor / the hit-rect+onClick convention.
  function drawOrgRadial(api, spec, s, elapsed, hits, top, bottom) {
    const cx = api.cx;
    const bands = spec.bands || [], nodes = spec.nodes || [], edges = spec.edges || [];
    if (bands.length < 1 || !nodes.length) return bottom;
    const innerKey = bands[0].key, outerKey = bands[bands.length - 1].key, midKey = bands[1] ? bands[1].key : 'member';
    const byId = {}; for (const n of nodes) byId[n.id] = n;
    const kids = {}; for (const e of edges) (kids[e.to] || (kids[e.to] = [])).push(e.from);
    const childNodes = function (id, band) { return (kids[id] || []).map(function (c) { return byId[c]; }).filter(function (n) { return n && n.band === band; }); };

    // Origin near the top; the wheel fans down. Each company is a FIXED ~40° slot;
    // the whole wheel ROTATES (ui.dial.rot eases toward target) — drag spins it,
    // clicking a company turns that slot to dead-centre (clicking the centred one
    // opens its glance). Off-screen slots (rotated past the visible arc) are culled.
    const Oy = top + 8 * s;
    const maxR = Math.max(80 * s, Math.min(api.W / 2 - 16 * s, (bottom - Oy) - 16 * s));
    const r0 = maxR * 0.16, r1 = maxR * 0.36, r2 = maxR * 0.64, r3 = maxR - 16 * s;
    const companies = nodes.filter(function (n) { return n.band === innerKey; });
    const N = Math.max(1, companies.length), slot = 0.7; // ≈40° per company
    const norm = function (a) { let v = a; while (v > Math.PI) v -= 2 * Math.PI; while (v < -Math.PI) v += 2 * Math.PI; return v; };
    const pos = function (r, a) { return { x: cx + r * Math.sin(a), y: Oy + r * Math.cos(a) }; };
    const VIS = 1.62; // ~93°: cull slots rotated past the visible bottom arc

    const D = ui.dial;
    D.rot += norm(D.target - D.rot) * 0.22;                 // ease toward target
    D.active = !!api.isFg; D.cx = cx; D.maxR = maxR; D.bases = {};

    const place = {}, ang = {};
    let selected = '', selBest = 99;
    for (let i = 0; i < companies.length; i++) {
      const co = companies[i], base = (i - (N - 1) / 2) * slot; // group centred on the bottom
      D.bases[co.id] = base;
      const ac = base + D.rot, acn = norm(ac);
      if (Math.abs(acn) < selBest) { selBest = Math.abs(acn); selected = co.id; }
      place[co.id] = pos(r1, ac); ang[co.id] = acn;
      const bridges = childNodes(co.id, outerKey);
      for (let b = 0; b < bridges.length; b++) { const ba = ac + (b - (bridges.length - 1) / 2) * 0.05; place[bridges[b].id] = pos(r0, ba); ang[bridges[b].id] = norm(ba); }
      const members = childNodes(co.id, midKey), m = members.length, usable = slot * 0.86;
      for (let j = 0; j < m; j++) {
        const ma = ac - usable / 2 + (j + 0.5) / m * usable;
        place[members[j].id] = pos(r2, ma); ang[members[j].id] = norm(ma);
        const ibs = childNodes(members[j].id, outerKey), sub = usable / m;
        for (let k = 0; k < ibs.length; k++) { const ia = ma - sub / 2 + (k + 0.5) / ibs.length * sub; place[ibs[k].id] = pos(r3, ia); ang[ibs[k].id] = norm(ia); }
      }
    }
    D.selected = selected;

    // spokes under nodes (cull when either end is off-screen).
    ctx.lineWidth = 1.1 * s; ctx.strokeStyle = '#2f4063';
    for (const e of edges) {
      const a = place[e.from], b = place[e.to];
      if (!a || !b || Math.abs(ang[e.from]) > VIS || Math.abs(ang[e.to]) > VIS) continue;
      ctx.beginPath(); ctx.moveTo(b.x, b.y); ctx.lineTo(a.x, a.y); ctx.stroke();
    }

    // nodes: dot marker + horizontal label below (company bold, member, inbox small).
    for (const n of nodes) {
      const p = place[n.id], a = ang[n.id]; if (!p || Math.abs(a) > VIS) continue;
      const hovered = ui.hover.rowKey === 'node:' + n.id;
      const isCo = n.band === innerKey, isIb = n.band === outerKey;
      const dotR = (isCo ? 8 : isIb ? 6 : 7) * s;
      let fill = statusColor(n.status || '');
      // company dots are structural (slate/selected-gold), not status-colored —
      // EXCEPT a disabled company, which gets the muted "disabled" tone so a
      // deliberately-off company reads distinctly from an active one.
      if (isCo) fill = n.id === selected ? ACCENT : (n.status === 'disabled' ? statusColor('disabled') : '#46506a');
      ctx.beginPath(); ctx.arc(p.x, p.y, dotR, 0, 2 * Math.PI); ctx.fillStyle = fill; ctx.fill();
      // a disabled company also dims its label + tags it "·停用" — the label signal
      // survives selection (the dot turns gold when selected, the label stays dim).
      const off = isCo && n.status === 'disabled';
      let lbl = clip(n.title || '', isCo ? 14 : isIb ? 8 : 10);
      if (off) lbl += '·停用';
      ctx.font = (isCo ? 'bold ' : '') + (isCo ? 12 : isIb ? 9.5 : 11) * s + 'px ui-monospace, Menlo, monospace';
      ctx.textAlign = 'center'; ctx.textBaseline = 'top'; ctx.fillStyle = hovered ? ACCENT : (off ? DIM : (isCo ? HEAD : DIM));
      ctx.fillText(lbl, p.x, p.y + dotR + 3 * s);
      if (hits) {
        const ll = Math.max(ctx.measureText(lbl).width, 32 * s);
        const act = isCo
          ? (function (cn) { return function () { if (cn.id === ui.dial.selected) cn.onClick(); else ui.dial.target = ui.dial.rot - norm((ui.dial.bases[cn.id] || 0) + ui.dial.rot); }; })(n)
          : n.onClick;
        hits.push({ x: p.x - ll / 2 - 9 * s, y: p.y - dotR - 8 * s, w: ll + 18 * s, h: dotR + 28 * s, id: n.id, action: act });
      }
    }
    return bottom;
  }

  // homeLayer draws the base graph AS A LAYER (recedes into the spiral when a form
  // is foreground). It builds a graph spec from the panels — bob/trunk root, leaf
  // modules (mechanism), flows (strategy), each self-reporting its tier via
  // Panel.Tier — and hands it to drawGraph. Edges are the dependency DAG (each
  // panel's Upstreams): position shows the tree, edges show the wiring. Hit-rects
  // are in LOCAL space (click/hover invert ui.entityLayerXform); only content-ful
  // nodes are interactive.
  function homeLayer(s, elapsed) {
    withLayerFrame('home', s, elapsed, function (api) {
      const ps = panels(), cx = api.cx;
      ctx.fillStyle = DIM; ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
      ctx.textAlign = 'center'; ctx.textBaseline = 'top';
      const up = state ? (state.UptimeSec || 0) : 0;
      ctx.fillText((connStale() ? '· stale ·  ' : '') + 'uptime ' + up + 's' + (ui.admin ? '  · admin' : ''), cx, 12 * s);

      const nodes = [{ id: '__bob', band: 'trunk', title: 'bob', sub: 'trunk', root: true }];
      const edges = [];
      for (const p of ps) {
        if (p.Tier === 'none') continue; // hidden sub-panel: addressable for View/Page, but not a top-level node (reached by a parent's drill-down)
        const open = hasContent(p);
        nodes.push({
          id: p.ID, band: p.Tier === 'flow' ? 'flow' : 'leaf',
          title: p.Title || p.ID, sub: open ? headline(p) : '—', status: worstStatus(p),
          interactive: open,
          onClick: (function (id) { return function () { openForm('layer:' + id, { kind: 'layer', panelID: id }); }; })(p.ID),
        });
        for (const upId of (p.Upstreams || [])) edges.push({ from: upId, to: p.ID });
      }
      const spec = {
        bands: [
          { key: 'trunk', label: 'trunk' },
          { key: 'leaf', label: 'leaf · mechanism', emptyHint: '—' },
          { key: 'flow', label: 'flow · strategy', emptyHint: ui.admin ? '—' : '· paste a /webui token to unlock ·' },
        ],
        nodes: nodes, edges: edges,
      };
      const H = canvas.height;
      drawGraph(api, spec, s, elapsed, api.isFg ? (ui.nodeHits = []) : null, api.cy - H * 0.30, api.cy + H * 0.30);
    });
  }

  // ── the three canvas forms (peers; no hierarchy) ───────────────────────────
  // Each draws inside withLayerFrame (spiral placement + breath). The home graph
  // is NOT a form — it is the base page (homeLayer).

  // renderLayer — a module's main body. The setting launch buttons render at the
  // TOP (under the title) so they never collide with the bottom back-hint or a
  // full-area field (e.g. agora's graph). A layer persists in the spiral.
  // paintFieldsView draws a parameterized read-only view (GraphAction.Open): fields
  // fetched lazily from /api/panel/<panel>/view/<vid> into ui.viewCache, rendered via
  // renderField (paged tables inside page through the normal fetchPage path against
  // the same panelID). Shared by both the glance and layer form kinds (ViewTarget.Kind).
  function paintFieldsView(api, f, s, elapsed) {
    let y = formTitle(api, s, f.title || f.panelID);
    const x = api.leftX;
    const cached = ui.viewCache[f.panelID + ':' + f.viewID];
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    if (cached === undefined) {
      ctx.fillStyle = DIM; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText('… loading …', x, y);
    } else if (cached === null) {
      ctx.fillStyle = DIM; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText('(unavailable)', x, y);
    } else {
      // Same viewport-anchored test as renderLayer/renderGlance — a fields view is
      // reached through both of them, and without this its tables would still cut at
      // 18 rows inside a frame the wheel can scroll (i.e. scroll into blank space).
      const graph = cached.some(function (fl) { return fl.Kind === 'graph'; });
      api.flow = !graph;
      for (const fld of cached) y = renderField(api, fld, x, y, f.panelID, elapsed);
      if (graph) return undefined;
    }
    return y;
  }

  function renderLayer(id, f, s, elapsed) {
    const p = panelByID(f.panelID);
    withLayerFrame(id, s, elapsed, function (api) {
      if (f.view === 'fields') { const yb = paintFieldsView(api, f, s, elapsed); backHint(api, s); return yb; }
      if (f.view === 'takeover') { paintTakeover(api, f, s); backHint(api, s); return; } // live browser handoff layer (fixed viewport — never scrolls)
      let y = formTitle(api, s, p ? (p.Title || p.ID) : f.panelID);
      const x = api.leftX;
      // A graph field sizes itself to the VIEWPORT and returns bottomHintY rather than
      // a content bottom (renderField's graph branch does this for both layouts), and a
      // radial panel additionally pins its actions button there. Feeding that constant
      // into layerBottom would report phantom overflow on a short window and let the
      // wheel drag a viewport-anchored panel off-screen — so such a panel opts out.
      const graph = !!p && (p.State || []).some(function (fl) { return fl.Kind === 'graph'; });
      const radial = !!p && (p.State || []).some(function (fl) { return fl.Kind === 'graph' && fl.Layout === 'radial'; });
      api.flow = !graph; // frame scrolls ⇒ renderField must not truncate its content
      if (!p) {
        ctx.fillStyle = DIM; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText('(panel gone)', x, y);
      } else {
        // a turntable (radial graph) tucks its settings into a corner "actions" glance
        // so they don't crowd the wheel; other panels keep the top settings strip.
        if (api.isFg && !radial && (p.Settings || []).length) y = renderSettings(api, p, x, y) + 8 * s;
        for (const fld of (p.State || [])) y = renderField(api, fld, x, y, f.panelID, elapsed);
        if (api.isFg && radial && (p.Settings || []).length) {
          const bh = 24 * s;
          paintButton(x, api.bottomHintY - bh - 4 * s, 100 * s, bh, '≡ actions', { s: s, hitRects: ui.entityHitRects, action: (function (pid) { return function () { openForm('glance:acts:' + pid, { kind: 'glance', view: 'settings', panelID: pid }); }; })(p.ID) });
        }
      }
      backHint(api, s);
      return graph ? undefined : y;
    });
  }

  // renderGlance — read-only content, ephemeral. Either a command's output
  // (view==='result') or a read-only snapshot of a panel's fields.
  function renderGlance(id, f, s, elapsed) {
    withLayerFrame(id, s, elapsed, function (api) {
      if (f.view === 'result') {
        drawResultBody(api, s);
        // success/failure mark (top-right) once the command returned: green ✓ / red ✗,
        // from X-Run-Status — a glance tells the operator it worked without reading.
        if (api.isFg && !ui.result.pending && ui.result.status) {
          const ok = ui.result.status === 'ok';
          ctx.textAlign = 'right'; ctx.textBaseline = 'top';
          ctx.fillStyle = ok ? STATUS_COL.ok : STATUS_COL.down;
          ctx.font = 'bold ' + 22 * s + 'px ui-monospace, Menlo, monospace';
          ctx.fillText(ok ? '✓' : '✗', api.leftX + api.W, api.topY);
          ctx.textAlign = 'left';
        }
        // copy button (bottom-left): copies the result (a 32-hex token if present,
        // e.g. a wire token, else the whole text). Flashes "✓ copied" for ~1.6s.
        if (api.isFg && ui.result.text && !ui.result.pending) {
          const recent = (elapsed - ui.copiedAt) < 1.6;
          const bh = 24 * s;
          paintButton(api.leftX, api.bottomHintY - bh - 4 * s, 104 * s, bh, recent ? '✓ copied' : 'copy', {
            s: s,
            fill: recent ? '#15301f' : '#1f2937',
            stroke: recent ? ACCENT : '#4a5666',
            textColor: recent ? ACCENT : '#d6d9dd',
            hitRects: ui.entityHitRects,
            action: function () {
              const m = (ui.result.text || '').match(/[0-9a-f]{32}/);
              copyToClipboard(m ? m[0] : (ui.result.text || ''));
            },
          });
        }
        backHint(api, s); return;
      }
      if (f.view === 'settings') { // a panel's actions, opened from its corner "≡ actions" button
        const p = panelByID(f.panelID);
        let y = api.topY; const x = api.leftX;
        ctx.textAlign = 'left'; ctx.textBaseline = 'top';
        ctx.fillStyle = HEAD; ctx.font = 'bold ' + 16 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText((p ? (p.Title || p.ID) : f.panelID) + ' · actions', x, y); y += 28 * s;
        if (p) renderSettings(api, p, x, y);
        backHint(api, s); return;
      }
      if (f.view === 'picker') {
        const x = api.leftX; let y = api.topY;
        ctx.textAlign = 'left'; ctx.textBaseline = 'top';
        ctx.fillStyle = HEAD; ctx.font = 'bold ' + 16 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText(clip(f.title || 'pick', 40), x, y); y += 26 * s;
        ctx.fillStyle = DIM; ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText('filter in the dock · click a name', x, y); y += 22 * s;
        const filt = (ui.pick.filter || '').toLowerCase();
        for (const it of (ui.pick.items || [])) {
          if (filt && (it.label || '').toLowerCase().indexOf(filt) < 0) continue;
          if (y > api.bottomHintY - 20 * s) { ctx.fillText('…', x, y); break; }
          const hovered = ui.hover.rowKey === 'pick:' + it.value;
          if (hovered) { ctx.fillStyle = '#22293a'; ctx.fillRect(x - 4 * s, y - 3 * s, api.W, 18 * s); }
          ctx.fillStyle = HEAD; ctx.font = 'bold ' + 12 * s + 'px ui-monospace, Menlo, monospace';
          ctx.fillText(clip(it.label || '', 28), x, y);
          ctx.fillStyle = DIM; ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
          ctx.fillText(clip(it.sub || '', 44), x + 220 * s, y);
          if (api.isFg) ui.entityHitRects.push({ x: x - 4 * s, y: y - 3 * s, w: api.W, h: 18 * s, pickKey: it.value, action: (function (v) { return function () { const cmd = (ui.pick.template || '').replace('%s', v); closeForm(); prefillDock(cmd); }; })(it.value) });
          y += 19 * s;
        }
        backHint(api, s); return;
      }
      if (f.view === 'text') {
        // re-derive title/detail/actions from the LIVE node each frame (so a
        // pause→resume etc. flips in place after poll); fall back to the captured
        // snapshot if the node is gone.
        let title = f.title, text = f.text, actions = f.actions, ln = null;
        if (f.nodeID) {
          ln = liveGraphNode(f.panelID, f.nodeID);
          if (ln) { title = ln.Title; text = ln.Detail || '(no detail)'; actions = mapNodeActions(ln.Actions); }
        }
        const x = api.leftX; let y = api.topY;
        ctx.textAlign = 'left'; ctx.textBaseline = 'top';
        ctx.fillStyle = DIM; ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText('› ' + clip(title || 'detail', 96), x, y); y += 22 * s;
        // per-entity action buttons (prefill the dock) — at the top, admin only.
        if (api.isFg && ui.admin && (actions || []).length) {
          let bx = x;
          for (const a of actions) {
            ctx.font = 'bold ' + 12 * s + 'px ui-monospace, Menlo, monospace';
            const w = Math.max(80 * s, ctx.measureText(a.label).width + 24 * s);
            if (bx + w > x + api.W) { bx = x; y += 30 * s; }
            paintButton(bx, y, w, 24 * s, a.label, {
              s: s, hitRects: ui.entityHitRects, hoverKey: 'gbtn:' + a.label, action: (function (act) {
                return function () {
                  if (act.open) openTarget(act.open.panel, act.open.node, act.open.view, act.open.kind, act.open.title || act.label);
                  else if (act.edit) openForm('model:' + act.edit.panel + ':' + act.edit.setting, { kind: 'model', panelID: act.edit.panel, settingID: act.edit.setting, label: act.label, lang: act.edit.lang });
                  else if (act.pick && act.pick.length) openForm('glance:pick', { kind: 'glance', view: 'picker', title: act.label, items: act.pick, template: act.command });
                  else prefillDock(act.command.replace('%s', '')); // no picker items → don't leak the %s placeholder
                };
              })(a),
            });
            bx += w + 10 * s;
          }
          y += 34 * s;
        }
        // reset align/baseline — paintButton left them at center/middle.
        ctx.textAlign = 'left'; ctx.textBaseline = 'top';
        ctx.fillStyle = INK; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
        const cols = Math.max(20, Math.floor(api.W / (7 * s)));
        for (const line of wrap(text || '(no detail)', cols)) { // stale/removed node → graceful placeholder, not a blank body
          if (y > api.bottomHintY - 24 * s) { ctx.fillText('…', x, y); break; }
          ctx.fillText(line, x, y); y += 16 * s;
        }
        // node-attached fields (the module's own self-description — e.g. a company
        // node's employee table with per-row actions). Rendered generically via
        // renderField; the webui holds NO subsystem knowledge of what they are.
        const nodeFields = ln ? ln.Fields : f.fields;
        if ((nodeFields || []).length) {
          y += 8 * s;
          for (const fld of nodeFields) y = renderField(api, fld, x, y, f.panelID, elapsed);
        }
        backHint(api, s); return;
      }
      if (f.view === 'fields') { const yb = paintFieldsView(api, f, s, elapsed); backHint(api, s); return yb; }
      const p = panelByID(f.panelID);
      let y = formTitle(api, s, p ? (p.Title || p.ID) : f.panelID);
      const x = api.leftX;
      // Same viewport-anchored rule as renderLayer: a glance over the identical fields
      // must scroll identically. Whether a view opens as a layer or a glance is decided
      // by the module's ViewTarget.Kind (openTarget), which says nothing about overflow.
      const graph = !!p && (p.State || []).some(function (fl) { return fl.Kind === 'graph'; });
      api.flow = !graph;
      if (!p) {
        ctx.fillStyle = DIM; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText('(panel gone)', x, y);
      } else {
        for (const fld of (p.State || [])) y = renderField(api, fld, x, y, f.panelID, elapsed);
      }
      backHint(api, s);
      return graph ? undefined : y;
    });
  }

  // renderModel — editable content, ephemeral. The dock IS the editor (a textarea
  // grown to model mode); this paints the heading, a live preview of the edit
  // buffer, and the save/cancel hint. Input never lives on the canvas.
  function renderModel(id, f, s, elapsed) {
    withLayerFrame(id, s, elapsed, function (api) {
      const x = api.leftX;
      // The expanded dock covers ~60vh on ONE edge (top in landscape, bottom in
      // portrait). Draw the form's title/status/buttons on the OPPOSITE, free edge.
      const portrait = canvas.height > canvas.width;
      let y = portrait ? api.topY : api.bottomHintY - 92 * s;
      ctx.textAlign = 'left'; ctx.textBaseline = 'top';
      ctx.fillStyle = HEAD; ctx.font = 'bold ' + 16 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText((f.label || f.settingID) + (f.lang ? '  ·  ' + f.lang : ''), x, y); y += 26 * s;
      ctx.fillStyle = ui.model.statusKind === 'err' ? STATUS_COL.down : ui.model.statusKind === 'ok' ? STATUS_COL.ok : DIM;
      ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText(ui.model.loading ? '…loading…' : (ui.model.status || 'edit in the dock · ⌘/Ctrl+Enter saves · Esc cancels'), x, y); y += 24 * s;
      // Save / Reset buttons (canvas — no DOM). Hit-rects ride ui.entityHitRects;
      // the foreground-form click handler inverts the layer transform to test them.
      if (api.isFg && !ui.model.loading) {
        const bw = 96 * s, bh = 28 * s, gap = 12 * s;
        paintButton(x, y, bw, bh, 'Save', { s: s, fill: '#2d4f3a', stroke: '#4a7a5e', hitRects: ui.entityHitRects, action: saveModel });
        paintButton(x + bw + gap, y, bw, bh, 'Reset', { s: s, hitRects: ui.entityHitRects, action: resetModel });
      }
    });
  }
  // resetModel reverts the edit buffer (and the dock) to the value last loaded.
  function resetModel() {
    if (!ui.model.id || ui.model.loading) return;
    ui.model.body = ui.model.orig;
    dock.value = ui.model.orig;
    ui.model.status = 'reset to saved'; ui.model.statusKind = '';
    dock.focus();
  }

  // formTitle paints a form heading and returns the next y; backHint paints the
  // shared dismiss line. Both are reused by all three forms.
  function formTitle(api, s, text) {
    const x = api.leftX, y = api.topY;
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    ctx.fillStyle = HEAD; ctx.font = 'bold ' + 18 * s + 'px ui-monospace, Menlo, monospace';
    ctx.fillText(text, x, y);
    return y + 34 * s;
  }
  function backHint(_api, _s) {
    // Intentionally blank: the Esc / click-away gesture still works on every layer; the
    // label was dropped from all layers by design. Kept as a no-op so call sites stand.
  }

  // renderField draws one StateField and returns the next y. panelID is needed
  // to key a paged table's offset/cache.
  function renderField(api, f, x, y, panelID, elapsed) {
    const s = api.s;
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    if (f.Kind === 'graph') {
      y = paintSectionHeader(x, y, api.W, f.Label || 'graph', { s: s });
      const spec = {
        bands: (f.Bands || []).map(function (k) { return { key: k, label: k }; }),
        nodes: (f.Nodes || []).map(function (n) {
          return {
            id: n.ID, band: n.Band, title: n.Title, sub: n.Sub, status: n.Status, interactive: true,
            onClick: (function (nn) {
              return function () {
                openForm('glance:node:' + nn.ID, {
                  kind: 'glance', view: 'text', title: nn.Title, text: nn.Detail || '(no detail)',
                  nodeID: nn.ID, panelID: panelID, // → renderGlance re-derives live each frame
                  actions: mapNodeActions(nn.Actions), fields: nn.Fields, // snapshot fallback if the live node is gone
                });
              };
            })(n),
          };
        }),
        edges: (f.Edges || []).map(function (e) { return { from: e.From, to: e.To, label: e.Label, weight: e.Weight }; }),
        layout: f.Layout || '',
      };
      // content bounds [y+6, just above the back-hint]. Layout selects geometry:
      // "radial" → the half-disk sunburst (agora org); else horizontal bands (home).
      const gHits = api.isFg ? ui.entityHitRects : null;
      if (spec.layout === 'radial') drawOrgRadial(api, spec, s, elapsed || 0, gHits, y + 6 * s, api.bottomHintY - 12 * s);
      else drawGraph(api, spec, s, elapsed || 0, gHits, y + 6 * s, api.bottomHintY - 12 * s);
      return api.bottomHintY - 12 * s;
    }
    if (f.Kind === 'stat') {
      ctx.fillStyle = DIM; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText(clip(f.Label, 28), x, y);
      ctx.fillStyle = statusColor(f.Status);
      ctx.font = 'bold ' + 12 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText(f.Value + (f.Unit ? ' ' + f.Unit : ''), x + 180 * s, y);
      return y + 19 * s;
    }
    if (f.Kind === 'status') {
      ctx.fillStyle = DIM; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText(clip(f.Label, 28), x, y);
      ctx.fillStyle = statusColor(f.Status);
      ctx.font = 'bold ' + 12 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText(f.Status || '—', x + 180 * s, y);
      return y + 19 * s;
    }
    if (f.Kind === 'text') {
      if (f.Label) { y = paintSectionHeader(x, y, api.W, f.Label, { s: s }); }
      ctx.fillStyle = INK; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
      for (const line of String(f.Text || '').split('\n')) {
        ctx.fillText(clip(line, 90), x, y); y += 16 * s;
      }
      return y + 4 * s;
    }
    if (f.Kind === 'table') {
      y = paintSectionHeader(x, y, api.W, f.Label || 'table', { s: s });
      const cols = f.Columns || [];

      // Resolve which page of rows to show. Unpaged: f.Rows. Paged: page 0 is
      // embedded in f.Rows; other offsets come from the cache (fetched on nav).
      let rows = f.Rows || [], offset = 0;
      const paged = !!f.Paged, total = paged ? (f.Total || 0) : (f.Rows || []).length;
      const pageSize = f.PageSize || 50;
      if (paged) {
        const key = panelID + ':' + f.ID;
        offset = ui.pageOffset[key] || 0;
        if (offset !== 0) {
          const cached = ui.pageCache[panelID + ':' + f.ID + ':' + offset];
          if (cached) rows = cached.Rows || [];
          else { rows = null; fetchPage(panelID, f.ID, offset, pageSize); }
        }
      }

      // Tag-filter bar (f.TagFilter): a clickable row of every tag the entries
      // carry; click to toggle, the table then shows only rows whose Cell.Tags
      // include EVERY active tag. Frontend-only state (ui.tagFilter), so the
      // selection survives the 1s state poll. Reusable by any TagFilter table.
      if (f.TagFilter) {
        const fkey = panelID + ':' + (f.ID || f.Label || '');
        const active = ui.tagFilter[fkey] || (ui.tagFilter[fkey] = []);
        const seen = {};
        for (const r of (f.Rows || [])) for (const c of (r || [])) if (c && c.Tags) for (const t of c.Tags) seen[t] = true;
        const tags = Object.keys(seen).sort();
        // Drop any active tag that no longer exists in the pool (e.g. a model was
        // removed from models.yaml) — else it filters to zero rows with no pill left
        // to un-toggle it. Mutates the persisted selection in place.
        for (let i = active.length - 1; i >= 0; i--) if (!seen[active[i]]) active.splice(i, 1);
        if (tags.length) {
          ctx.font = 10 * s + 'px ui-monospace, Menlo, monospace';
          ctx.fillStyle = DIM; ctx.fillText('tags', x, y);
          let tx = x + 34 * s;
          for (const t of tags) {
            const on = active.indexOf(t) >= 0;
            const k = 'tagf:' + fkey + ':' + t;
            const hov = ui.hover.rowKey === k;
            const tw = ctx.measureText(t).width + 10 * s;
            if (tx + tw > x + api.W) { tx = x + 34 * s; y += 19 * s; }
            // box + hit-rect use the top-baseline convention (textBaseline='top' here),
            // so they sit OVER the text (y..y+~14s), not above it — the whole pill is
            // clickable. Hover paints a faint box + bright text (like the row buttons).
            if (on || hov) { ctx.fillStyle = on ? '#2b3445' : '#222a38'; ctx.fillRect(tx, y - 2 * s, tw, 16 * s); }
            ctx.fillStyle = on ? ACCENT : (hov ? HEAD : DIM);
            ctx.fillText(t, tx + 5 * s, y);
            if (api.isFg) ui.entityHitRects.push({ x: tx, y: y - 2 * s, w: tw, h: 16 * s, hoverKey: k, action: (function (tag) { return function () { const a = ui.tagFilter[fkey], i = a.indexOf(tag); if (i >= 0) a.splice(i, 1); else a.push(tag); }; })(t) });
            tx += tw + 6 * s;
          }
          y += 22 * s;
        }
        if (active.length && rows !== null) { // rows===null = a paged table mid-fetch; leave the "… loading …" path intact
          rows = rows.filter(function (r) { return active.every(function (t) { return (r || []).some(function (c) { return c && c.Tags && c.Tags.indexOf(t) >= 0; }); }); });
        }
      }

      // Content-aware column widths, PIXEL-measured (so CJK / emoji don't overrun):
      // each non-last column is the width of its widest cell (capped at 40% so one
      // can't hog the row), and the LAST column absorbs the rest — short "who" sits
      // tight, long "message" fills the frame. A table whose column labels are all
      // blank skips the header row (e.g. a bare list).
      const wrows = rows || f.Rows || [], pad = 12 * s, minCol = 36 * s;
      ctx.font = 10.5 * s + 'px ui-monospace, Menlo, monospace';
      const colXs = [], colPx = [];
      let accX = x;
      for (let c = 0; c < cols.length; c++) {
        let w = ctx.measureText(String(cols[c] || '')).width;
        for (const r of wrows) { const t = String((r[c] && r[c].Text) || '').split('\n')[0]; const tw = ctx.measureText(t).width; if (tw > w) w = tw; }
        let px;
        if (c < cols.length - 1) {
          // a non-last column can't grow so much that the columns after it lack room —
          // keeps a wide multi-column table (e.g. the 7-col model pool) inside the frame.
          const roomForRest = (x + api.W) - accX - (cols.length - 1 - c) * minCol;
          px = Math.max(minCol, Math.min(w + pad, api.W * 0.4, roomForRest));
        } else {
          px = Math.max(minCol, x + api.W - accX); // last column absorbs the remainder
        }
        colXs[c] = accX; colPx[c] = px; accX += px;
      }
      if (cols.some(function (h) { return String(h || '') !== ''; })) {
        ctx.font = 'bold ' + 10.5 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillStyle = '#b6914d';
        for (let c = 0; c < cols.length; c++) ctx.fillText(clipPx(cols[c], colPx[c] - 4 * s), colXs[c], y);
        y += 16 * s;
      }

      if (rows === null) {
        ctx.fillStyle = DIM; ctx.font = 10.5 * s + 'px ui-monospace, Menlo, monospace';
        ctx.fillText('… loading …', x, y); y += 15 * s;
      } else {
        // 18 is the cut a FIXED frame needed — there was nowhere to put row 19. A
        // scrolling frame (api.flow) has somewhere, and capping there would hide rows
        // the wheel can now reach. Paged tables keep pageSize: their cut is the
        // server's LIMIT, not a drawing limit, and the footer navigates it.
        const cap = paged ? pageSize : (api.flow ? rows.length : 18);
        const lineH = 13 * s;
        for (let ri = 0; ri < Math.min(rows.length, cap); ri++) {
          const row = rows[ri] || [];
          // a faint divider between entries (each row, except the first) so multi-line
          // rows (e.g. the model pool's name + "kind tags model" subline) read as
          // distinct items, not a run-on block.
          if (ri > 0) { ctx.fillStyle = '#454d5c'; ctx.fillRect(x - 4 * s, y - 4 * s, api.W + 8 * s, Math.max(1, s)); }
          ctx.font = 10.5 * s + 'px ui-monospace, Menlo, monospace';
          let maxLines = 1, detail = '', act = '', actLabel = '', copy = '', rowActions = [], edit = null;
          // RowCopy (e.g. chat log): the whole row hover-highlights + copies on click.
          // Pre-scan the row height so the highlight sits behind the text.
          let rowKey = null;
          if (f.RowCopy) {
            for (let c = 0; c < cols.length; c++) { const n = String((row[c] && row[c].Text) || '').split('\n').length; if (n > maxLines) maxLines = n; }
            rowKey = 'rowcopy:' + panelID + ':' + (f.ID || '') + ':' + ri;
            if (ui.hover.rowKey === rowKey) { ctx.fillStyle = '#1b2230'; ctx.fillRect(x - 4 * s, y - 2 * s, api.W + 8 * s, maxLines * lineH + 2 * s); }
          }
          for (let c = 0; c < cols.length; c++) {
            const cell = row[c] || { Text: '' };
            if (cell.Detail) detail = cell.Detail;
            if (cell.Action) { act = cell.Action; actLabel = cell.ActionLabel || 'run'; }
            if (cell.Actions && cell.Actions.length) rowActions = rowActions.concat(cell.Actions);
            if (cell.Copy) copy = cell.Copy;
            if (cell.Edit) edit = cell.Edit;
            // multi-line cell: first line at the column x, continuations indented.
            const lines = String(cell.Text || '').split('\n');
            if (lines.length > maxLines) maxLines = lines.length;
            // a cell with Open is a drill-down link (the company→member→inbox path):
            // colored + clickable on its text, brightening on hover, descending into
            // the target's glance.
            const cellKey = cell.Open ? 'cellopen:' + (cell.Open.Node || cell.Open.View) : null;
            const cellHov = cellKey && ui.hover.rowKey === cellKey;
            ctx.fillStyle = cell.Open ? (cellHov ? ACCENT : '#7aa8c4') : (cell.Status ? statusColor(cell.Status) : INK);
            for (let li = 0; li < lines.length; li++) {
              // First line clips to its column. A continuation line (the model pool's
              // "kind · model" subline) spans to the ROW END instead — the other columns
              // are blank on that line, so it needs no truncation to fit the column.
              const indent = li ? 10 * s : 0;
              const budget = li ? (x + api.W - colXs[c] - indent - 4 * s) : (colPx[c] - 4 * s);
              ctx.fillText(clipPx(lines[li], budget), colXs[c] + indent, y + li * lineH);
            }
            if (cell.Open && api.isFg) {
              // bound the link hit-rect to the TEXT width (not the column) so it can't
              // overlap the right-aligned row action buttons on a narrow viewport.
              const lw = ctx.measureText(clipPx(lines[0] || '', colPx[c] - 4 * s)).width;
              ui.entityHitRects.push({ x: colXs[c] - 3 * s, y: y - 3 * s, w: lw + 6 * s, h: lineH + 4 * s, hoverKey: cellKey, action: (function (t) { return function () { openTarget(t.Panel, t.Node, t.View, t.Kind, t.Title); }; })(cell.Open) });
            }
          }
          // Row-end affordances (right-aligned): an action button (prefills the
          // dock, admin) then a "why" (read-only detail → glance).
          let rx = x + api.W;
          if (detail) {
            rx -= 34 * s;
            ctx.fillStyle = '#7aa8c4'; ctx.fillText('why', rx, y);
            if (api.isFg) ui.entityHitRects.push({ x: rx - 4 * s, y: y - 3 * s, w: 38 * s, h: 16 * s, action: (function (d, t) { return function () { openForm('glance:why', { kind: 'glance', view: 'text', title: t, text: d }); }; })(detail, ((row[0] && row[0].Text) || 'detail').split('\n')[0] + ' — why') });
          }
          if (act && ui.admin) {
            const lw = ctx.measureText(actLabel).width + 8 * s, k = 'cellact:' + act;
            rx -= lw + 8 * s;
            ctx.fillStyle = ui.hover.rowKey === k ? ACCENT : '#b6914d'; ctx.fillText(actLabel, rx, y);
            if (api.isFg) ui.entityHitRects.push({ x: rx - 4 * s, y: y - 3 * s, w: lw + 6 * s, h: 16 * s, hoverKey: k, action: (function (cmd) { return function () { prefillDock(cmd); }; })(act) });
          }
          // additional per-row action buttons (Cell.Actions) — same prefill mechanism,
          // right-aligned in order, admin only.
          if (ui.admin) for (const ca of rowActions) {
            const lab = ca.Label || 'run', lw = ctx.measureText(lab).width + 8 * s, k = 'cellact:' + ca.Command;
            rx -= lw + 8 * s;
            ctx.fillStyle = ui.hover.rowKey === k ? ACCENT : '#b6914d'; ctx.fillText(lab, rx, y);
            if (api.isFg) ui.entityHitRects.push({ x: rx - 4 * s, y: y - 3 * s, w: lw + 6 * s, h: 16 * s, hoverKey: k, action: (function (cmd) { return function () { prefillDock(cmd); }; })(ca.Command) });
          }
          if (copy) {
            const lw = ctx.measureText('copy').width + 8 * s;
            rx -= lw + 8 * s;
            ctx.fillStyle = '#7aa8c4'; ctx.fillText('copy', rx, y);
            if (api.isFg) ui.entityHitRects.push({ x: rx - 4 * s, y: y - 3 * s, w: lw + 6 * s, h: 16 * s, action: (function (t) { return function () { copyToClipboard(t); }; })(copy) });
          }
          // per-row Edit button (Cell.Edit) → opens the markdown model editor on the
          // panel's Read/Apply(setting), same path as a graph node's edit action. Admin
          // only (the setting endpoints are admin-gated).
          if (edit && ui.admin) {
            const lab = 'edit', lw = ctx.measureText(lab).width + 8 * s, k = 'celledit:' + edit.Panel + ':' + edit.Setting;
            rx -= lw + 8 * s;
            ctx.fillStyle = ui.hover.rowKey === k ? ACCENT : '#b6914d'; ctx.fillText(lab, rx, y);
            if (api.isFg) ui.entityHitRects.push({ x: rx - 4 * s, y: y - 3 * s, w: lw + 6 * s, h: 16 * s, hoverKey: k, action: (function (e, ttl) { return function () { openForm('model:' + e.Panel + ':' + e.Setting, { kind: 'model', panelID: e.Panel, settingID: e.Setting, label: ttl, lang: e.Lang }); }; })(edit, ((row[0] && row[0].Text) || 'edit').split('\n')[0]) });
          }
          // whole-row copy hit-rect — pushed LAST so any cell link/button takes
          // precedence; copies the row's joined cell text.
          if (f.RowCopy && api.isFg) {
            const txt = row.map(function (c) { return (c && c.Text) || ''; }).filter(Boolean).join('  ');
            ui.entityHitRects.push({ x: x - 4 * s, y: y - 2 * s, w: api.W + 8 * s, h: maxLines * lineH + 2 * s, hoverKey: rowKey, action: (function (t) { return function () { copyToClipboard(t); }; })(txt) });
          }
          y += maxLines * lineH + 3 * s;
        }
        // The 18-row cut exists because a fixed frame had nowhere to put row 19. On a
        // frame that scrolls (api.flow) it would hide rows the wheel can now reach —
        // the reader would scroll into blank space below a "… N more" that never opens.
        if (!paged && !api.flow && rows.length > 18) {
          ctx.fillStyle = DIM; ctx.font = 10 * s + 'px ui-monospace, Menlo, monospace';
          ctx.fillText('… ' + (rows.length - 18) + ' more', x, y); y += 15 * s;
        }
      }

      // Paging footer: ‹ prev   page X / Y   next ›
      if (paged && total > pageSize) {
        const key = panelID + ':' + f.ID;
        if (api.isFg) ui.pageNav = { key: key, offset: offset, pageSize: pageSize, total: total }; // ←/→ keys page this
        const pageNum = Math.floor(offset / pageSize) + 1;
        const pageCount = Math.max(1, Math.ceil(total / pageSize));
        const hasPrev = offset > 0, hasNext = offset + pageSize < total;
        y += 4 * s;
        ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
        ctx.textAlign = 'left'; ctx.textBaseline = 'top';
        ctx.fillStyle = hasPrev ? '#7aa8c4' : '#3a4150';
        ctx.fillText('‹ prev', x, y);
        if (api.isFg && hasPrev) ui.entityHitRects.push({ x: x - 4 * s, y: y - 4 * s, w: 60 * s, h: 20 * s, action: (function (k, o, ps) { return function () { ui.pageOffset[k] = Math.max(0, o - ps); }; })(key, offset, pageSize) });
        ctx.fillStyle = DIM; ctx.textAlign = 'center';
        ctx.fillText('page ' + pageNum + ' / ' + pageCount, x + api.W / 2, y);
        ctx.textAlign = 'right';
        ctx.fillStyle = hasNext ? '#7aa8c4' : '#3a4150';
        ctx.fillText('next ›', x + api.W, y);
        if (api.isFg && hasNext) ui.entityHitRects.push({ x: x + api.W - 56 * s, y: y - 4 * s, w: 60 * s, h: 20 * s, action: (function (k, o, ps) { return function () { ui.pageOffset[k] = o + ps; }; })(key, offset, pageSize) });
        ctx.textAlign = 'left';
        y += 20 * s;
      }
      return y + 6 * s;
    }
    return y;
  }

  // renderSettings draws the panel's settings as launch buttons: a code setting
  // opens a model form (the dock becomes its editor); an action setting prefills
  // its command into the dock for the admin to review + Enter. Admin-only — for a
  // non-admin it draws only the unlock hint.
  function renderSettings(api, p, x, y) {
    const s = api.s;
    if (!ui.admin) {
      ctx.fillStyle = DIM; ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
      ctx.fillText('settings — unlock with a /webui token', x, y);
      return y + 18 * s;
    }
    y = paintSectionHeader(x, y, api.W, 'settings', { s: s });
    let bx = x, by = y;
    for (const st of (p.Settings || [])) {
      const label = (st.Kind === 'code' ? '✎ ' : '▸ ') + (st.Label || st.ID);
      ctx.font = 'bold ' + 12 * s + 'px ui-monospace, Menlo, monospace';
      const w = Math.max(90 * s, ctx.measureText(label).width + 28 * s);
      if (bx + w > x + api.W) { bx = x; by += 32 * s; } // wrap to next row
      const action = st.Kind === 'code'
        ? (function (pid, sid, lab, lang) { return function () { openForm('model:' + pid + ':' + sid, { kind: 'model', panelID: pid, settingID: sid, label: lab, lang: lang }); }; })(p.ID, st.ID, st.Label || st.ID, st.Lang)
        : (function (cmd) { return function () { prefillDock(cmd || ''); }; })(st.Command);
      paintButton(bx, by, w, 24 * s, label, { s: s, hitRects: ui.entityHitRects, action: action });
      bx += w + 10 * s;
    }
    return by + 32 * s;
  }

  // drawResultBody paints the last /api/run output inside a glance form (the form
  // owns the frame + back hint; this only draws the body).
  // drawTextBody paints a titled, wrapped text body inside a glance form (shared by
  // the command result and the "why" error detail).
  function drawTextBody(api, s, title, text, pending) {
    let y = api.topY; const x = api.leftX;
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    ctx.fillStyle = DIM; ctx.font = 11 * s + 'px ui-monospace, Menlo, monospace';
    ctx.fillText('› ' + clip(title, 96), x, y); y += 22 * s;
    ctx.fillStyle = INK; ctx.font = 12 * s + 'px ui-monospace, Menlo, monospace';
    const cols = Math.max(20, Math.floor(api.W / (7 * s)));
    for (const line of wrap(pending ? '…running…' : text, cols)) {
      if (y > api.bottomHintY - 24 * s) { ctx.fillText('…', x, y); break; }
      ctx.fillText(line, x, y); y += 16 * s;
    }
  }
  function drawResultBody(api, s) { drawTextBody(api, s, ui.result.line, ui.result.text, ui.result.pending); }

  // ===========================================================================
  // §RUN — dock command runner (output shows as a glance form)
  // ===========================================================================
  function runCommand(line) {
    ui.result = { line: line, text: '', pending: true, status: '' };
    openForm('glance:result', { kind: 'glance', view: 'result' });
    fetch('/api/run', { method: 'POST', body: line })
      .then(function (r) { if (r.status === 401) { ui.admin = false; ui.coverage = ''; } ui.result.status = r.headers.get('X-Run-Status') || (r.ok ? '' : 'error'); return r.text(); })
      .then(function (t) { ui.result.text = t; ui.result.pending = false; poll(); })
      .catch(function () { ui.result.text = '(network error — command may not have run)'; ui.result.pending = false; ui.result.status = 'error'; });
  }
  // prefillDock loads a setting's slash template into the dock (the §5.3 confirm
  // mechanism: the admin reviews + Enter runs it).
  function prefillDock(cmd) {
    if (!ui.admin || !cmd) return;
    dock.value = cmd; dock.focus();
    try { dock.setSelectionRange(cmd.length, cmd.length); } catch (_) { }
  }

  // fetchPage loads a paged table's later page into the cache (offset/limit,
  // skeleton convention). Guarded so one offset fetches once at a time.
  function fetchPage(panelID, fieldID, offset, pageSize) {
    const k = panelID + ':' + fieldID + ':' + offset;
    if (ui.pageFetching[k]) return;
    ui.pageFetching[k] = true;
    fetch('/api/panel/' + encodeURIComponent(panelID) + '/table/' + encodeURIComponent(fieldID) +
      '?limit=' + pageSize + '&offset=' + offset, { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (tp) { ui.pageCache[k] = tp; })
      .catch(function () { })
      .then(function () { delete ui.pageFetching[k]; });
  }

  // fetchView loads a parameterized read-only view's fields (GraphAction.Open) into
  // ui.viewCache. null on failure (rendered as "(unavailable)"). Guarded in-flight.
  function fetchView(panelID, viewID) {
    const k = panelID + ':' + viewID;
    if (ui.viewFetching[k]) return;
    ui.viewFetching[k] = true;
    fetch('/api/panel/' + encodeURIComponent(panelID) + '/view/' + encodeURIComponent(viewID), { cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (fields) { ui.viewCache[k] = fields || []; })
      .catch(function () { ui.viewCache[k] = null; })
      .then(function () { delete ui.viewFetching[k]; });
  }

  // ===========================================================================
  // §MODEL — editable form bound to the dock. There is no separate editor DOM:
  // the dock (a textarea) grows to model mode and IS the editor. enterModel loads
  // the setting into it; saveModel persists; exitModel restores command mode.
  // ===========================================================================
  function enterModel(id, meta) {
    ui.model = { id: id, panelID: meta.panelID, setting: meta.settingID, label: meta.label || meta.settingID, lang: meta.lang || '', body: '', orig: '', status: '', statusKind: '', loading: true };
    dock.classList.add('model');
    dock.value = '';
    fetch('/api/panel/' + encodeURIComponent(meta.panelID) + '/setting/' + encodeURIComponent(meta.settingID), { cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw r.status; return r.text(); })
      .then(function (t) {
        if (ui.model.id !== id) return;                 // navigated away while loading
        ui.model.body = t; ui.model.orig = t; ui.model.loading = false;
        dock.value = t; dock.focus();
        try { dock.setSelectionRange(t.length, t.length); } catch (_) { }
      })
      .catch(function (e) {
        if (ui.model.id !== id) return;
        ui.model.loading = false; ui.model.status = 'load failed (' + e + ')'; ui.model.statusKind = 'err';
      });
  }
  function exitModel() {
    ui.model = { id: '', panelID: '', setting: '', label: '', lang: '', body: '', orig: '', status: '', statusKind: '', loading: false };
    dock.classList.remove('model');
    dock.value = '';
    // Release focus from the dock — otherwise the window keydown guard
    // (e.target === dock → return) would swallow Esc/Tab nav after leaving the
    // editor, and subsequent keys would be lost.
    dock.blur();
  }

  // enterPick/exitPick drive a picker glance: the dock becomes a single-line filter
  // over ui.pick.items; clicking an item fills ui.pick.template's "%s" and prefills
  // that command (see renderGlance's 'picker' view).
  function enterPick(id, meta) {
    ui.pick = { id: id, template: meta.template || '', items: meta.items || [], filter: '' };
    dock.value = ''; dock.placeholder = 'filter…'; dock.focus();
  }
  function exitPick() {
    ui.pick = { id: '', template: '', items: [], filter: '' };
    dock.value = '';
    // restore the command-mode placeholder (enterPick set it to "filter…").
    dock.placeholder = ui.admin ? 'unlocked — management mode' : 'paste a /webui token to unlock management first';
  }
  function saveModel() {
    if (!ui.model.id || ui.model.loading) return;
    const id = ui.model.id, panelID = ui.model.panelID, setting = ui.model.setting;
    ui.model.status = 'saving…'; ui.model.statusKind = '';
    fetch('/api/panel/' + encodeURIComponent(panelID) + '/setting/' + encodeURIComponent(setting),
      { method: 'POST', body: ui.model.body })
      .then(function (r) { if (r.status === 401) { ui.admin = false; return null; } return r.json(); })
      .then(function (res) {
        if (ui.model.id !== id) return;                 // navigated away mid-save
        if (!res) { closeForm(); return; }              // 401: lost admin → drop the form
        if (res.Success) { poll(); if (ui.stack[0] === id) closeForm(); else { removeForm(id); exitModel(); } }
        else { ui.model.status = (res.Phase ? res.Phase + ': ' : '') + (res.Error || 'failed'); ui.model.statusKind = 'err'; }
      })
      .catch(function () { if (ui.model.id === id) { ui.model.status = 'network error'; ui.model.statusKind = 'err'; } });
  }

  // ===========================================================================
  // §INTRO — editorial three-body splash (skeleton §INTRO, ported verbatim)
  // ===========================================================================
  // A one-shot, full-viewport editorial engine painted above everything on load:
  // a near-black warm ground, a dominating serif headline + amber kicker, body
  // poured into auto-fit justified columns flowing around three dark-glass
  // planets — a real Chenciner–Montgomery figure-eight three-body sim, kept
  // presentable by a TOTAL-energy (KE+PE) thermostat. Drag an orb to perturb;
  // tap empty space / Esc / Enter dismisses (fade). Copy = /api/intro markdown.
  const INTRO_SERIF =
    '"Iowan Old Style","Palatino Linotype","Book Antiqua",Palatino,' +
    '"Songti SC","Source Han Serif SC","Noto Serif CJK SC",serif';
  const INTRO_SANS = '"Helvetica Neue",Helvetica,Arial,sans-serif';
  const INTRO_PAL = {
    bg0: '#171310', bg1: '#070605', head: '#f1eadd', accent: '#c89b52',
    body: '#c3bbac', en: '#b6914d', hr: 'rgba(241,234,221,0.14)',
    hint: 'rgba(195,187,172,0.5)',
  };
  function introLS(px) { try { ctx.letterSpacing = px + 'px'; } catch (_) { } }

  function parseIntro(md) {
    const blocks = [];
    let zh = null, en = null;
    const flush = () => {
      if (zh != null) { blocks.push({ type: 'zh', text: zh }); zh = null; }
      if (en != null) { blocks.push({ type: 'en', text: en }); en = null; }
    };
    for (const raw of String(md).replace(/\r/g, '').split('\n')) {
      const line = raw.trim();
      if (/^#\s+/.test(line)) { flush(); blocks.push({ type: 'h1', text: line.replace(/^#\s+/, '') }); continue; }
      if (/^##\s+/.test(line)) { flush(); blocks.push({ type: 'kicker', text: line.replace(/^##\s+/, '') }); continue; }
      if (/^(---+|\*\*\*+)$/.test(line)) { flush(); blocks.push({ type: 'hr' }); continue; }
      if (/^>\s?/.test(line)) {
        if (zh != null) { blocks.push({ type: 'zh', text: zh }); zh = null; }
        const t = line.replace(/^>\s?/, '');
        en = en == null ? t : en + ' ' + t;
        continue;
      }
      if (line === '') { flush(); continue; }
      if (en != null) { blocks.push({ type: 'en', text: en }); en = null; }
      zh = zh == null ? line : zh + line;
    }
    flush();
    return blocks;
  }

  const introIsCJK = c => /[　-鿿豈-﫿＀-￯]/.test(c);
  function introTokens(text) {
    const out = [], s = String(text);
    for (let i = 0; i < s.length;) {
      const c = s[i];
      if (c === ' ' || c === '\t') { out.push({ t: ' ', sp: true }); i++; }
      else if (introIsCJK(c)) { out.push({ t: c, sp: false }); i++; }
      else { let j = i; while (j < s.length && s[j] !== ' ' && s[j] !== '\t' && !introIsCJK(s[j])) j++; out.push({ t: s.slice(i, j), sp: false }); i = j; }
    }
    return out;
  }

  function introBuildParas(blocks) {
    const paras = [];
    for (const b of blocks) {
      if (b.type === 'zh' || b.type === 'en') paras.push({ style: b.type, text: b.text });
    }
    let capChar = '';
    const firstZh = paras.find(p => p.style === 'zh');
    if (firstZh) {
      const arr = [...firstZh.text];
      capChar = arr[0] || '';
      firstZh.text = arr.slice(1).join('');
      firstZh.cap = true;
    }
    for (const p of paras) p.tokens = introTokens(p.text);
    return { paras, capChar };
  }

  function introFreeSegs(left, right, yMid, orbs) {
    if (right <= left) return [];
    const blocked = [];
    for (const o of orbs) {
      const dy = yMid - o._y, rr = o._rEx;
      if (Math.abs(dy) < rr) {
        const hc = Math.sqrt(rr * rr - dy * dy) + o._pad;
        blocked.push([o._x - hc, o._x + hc]);
      }
    }
    if (!blocked.length) return [[left, right]];
    blocked.sort((a, b) => a[0] - b[0]);
    const segs = [];
    let cur = left;
    for (const [a, b] of blocked) {
      if (b <= cur) continue;
      if (a > cur) segs.push([cur, Math.min(a, right)]);
      cur = Math.max(cur, b);
      if (cur >= right) break;
    }
    if (cur < right) segs.push([cur, right]);
    return segs.filter(sg => sg[1] - sg[0] > 1);
  }

  function introPaintRun(acc, sx, segW, y, justify, fill) {
    ctx.fillStyle = fill;
    const hasSpace = acc.some(t => t.sp);
    const elastic = [];
    for (let i = 0; i < acc.length - 1; i++) if (hasSpace ? acc[i].sp : true) elastic.push(i);
    let natural = 0; for (const t of acc) natural += t.w;
    let per = 0;
    if (justify && elastic.length) per = Math.min((segW - natural) / elastic.length, 1.4 * acc[0].w || 0);
    if (!(per > 0)) per = 0;
    let x = sx, ei = 0;
    for (let i = 0; i < acc.length; i++) {
      ctx.fillText(acc[i].t, x, y);
      x += acc[i].w;
      if (ei < elastic.length && elastic[ei] === i) { x += per; ei++; }
    }
  }

  function dismissIntro() { if (ui.intro.active) ui.intro.alphaT = 0; }

  function teardownIntro() {
    const it = ui.intro;
    it.active = false; it.alpha = 0; it.alphaT = 0;
    it.rain = null; it.orbs = null; it.blocks = null; it.fit = null;
    it.geo = null; it._lastT = 0;
    dock.style.display = '';
  }

  function introFlow(paras, cols, yTop, yBot, fs, lh, orbs, paint, P, capChar) {
    let ci = 0, y = yTop, capDone = false;
    const capFs = fs * 2.6, capGap = 0.45 * fs;
    for (let pi = 0; pi < paras.length; pi++) {
      const para = paras[pi], toks = para.tokens;
      if (!toks.length) continue;
      const font = para.style === 'en'
        ? 'italic 400 ' + fs + 'px ' + INTRO_SERIF
        : '400 ' + fs + 'px ' + INTRO_SERIF;
      const fill = para.style === 'en' ? P.en : P.body;
      const capActive = !capDone && !!para.cap;
      let ti = 0, paraLine = 0;
      let carry = null;
      while (ti < toks.length || carry != null) {
        if (y + lh > yBot) { ci++; y = yTop; if (ci >= cols.length) return { overflow: true }; }
        y += lh; paraLine++;
        const c = cols[ci];
        let capW = 0;
        if (capActive && ci === 0 && paraLine <= 2) {
          ctx.font = '700 ' + capFs + 'px ' + INTRO_SERIF;
          capW = ctx.measureText(capChar).width + capGap;
        }
        ctx.font = font;
        const hyphW = ctx.measureText('-').width;
        const yMid = y - lh * 0.32;
        const colLeft = c.x + capW, colRight = c.x + c.w;
        const segs = introFreeSegs(colLeft, colRight, yMid, orbs);
        for (let si = 0; si < segs.length && (ti < toks.length || carry != null); si++) {
          const sx = segs[si][0], segW = segs[si][1] - segs[si][0];
          if (segW < fs * 1.1) continue;
          const acc = []; let accW = 0;
          while (ti < toks.length || carry != null) {
            const fromCarry = carry != null;
            const tk = fromCarry ? { t: carry, sp: false } : toks[ti];
            if (tk.sp && !acc.length) { ti++; continue; }
            const w = ctx.measureText(tk.t).width;
            if (accW + w <= segW) {
              acc.push({ t: tk.t, sp: tk.sp, w }); accW += w;
              if (fromCarry) carry = null; else ti++;
            } else if (!acc.length) {
              const arr = [...tk.t];
              let cut = 0, pw = 0;
              for (let k = 0; k < arr.length; k++) {
                const cw = ctx.measureText(arr[k]).width;
                if (pw + cw + hyphW > segW) break;
                pw += cw; cut = k + 1;
              }
              if (cut >= arr.length) {
                acc.push({ t: tk.t, sp: false, w });
                if (fromCarry) carry = null; else ti++;
              } else if (cut >= 1) {
                const head = arr.slice(0, cut).join('') + '-';
                acc.push({ t: head, sp: false, w: ctx.measureText(head).width });
                carry = arr.slice(cut).join('');
                if (!fromCarry) ti++;
              }
              break;
            } else break;
          }
          while (acc.length && acc[acc.length - 1].sp) { accW -= acc[acc.length - 1].w; acc.pop(); }
          if (acc.length && paint) introPaintRun(acc, sx, segW, y, (ti < toks.length || carry != null), fill);
        }
        if (paint && capActive && ci === 0 && paraLine === 1) {
          ctx.font = '700 ' + capFs + 'px ' + INTRO_SERIF;
          ctx.fillStyle = P.accent;
          ctx.fillText(capChar, c.x, y + 1.18 * fs);
        }
      }
      if (capActive) capDone = true;
      y += lh * 0.55;
    }
    return { overflow: false };
  }

  function introFit(paras, cols, yTop, yBot, s, capChar, orbs) {
    let lo = 8.5 * s, hi = 21 * s, best = lo;
    for (let i = 0; i < 11; i++) {
      const fs = (lo + hi) / 2, lh = fs * 1.62;
      const res = introFlow(paras, cols, yTop, yBot - lh * 2.2, fs, lh, orbs, false, INTRO_PAL, capChar);
      if (res.overflow) hi = fs; else { best = fs; lo = fs; }
    }
    return best;
  }

  function introOrbs() {
    if (!ui.intro.orbs) {
      const X = 0.97000436, Y = -0.24308753;
      const VX = 0.93240737, VY = 0.86473146;
      ui.intro.orbs = [
        { ux: X, uy: Y, uvx: VX / 2, uvy: VY / 2, rk: 0.135, seed: 0.0, hue: '96,130,228' },
        { ux: -X, uy: -Y, uvx: VX / 2, uvy: VY / 2, rk: 0.11, seed: 2.3, hue: '74,178,122' },
        { ux: 0, uy: 0, uvx: -VX, uvy: -VY, rk: 0.145, seed: 4.1, hue: '212,84,98' },
      ];
    }
    return ui.intro.orbs;
  }

  function introRain() {
    let r = ui.intro.rain;
    if (r) return r;
    const blocks = ui.intro.blocks || [];
    let s = '';
    for (let i = blocks.length - 1; i >= 0; i--) { const t = blocks[i].text; if (t) s += t; }
    const chars = [];
    for (const ch of s) { if (!/\s/.test(ch)) chars.push(ch); }
    if (!chars.length) return { chars: ['的', '是', '在', '不', '了', '事', '物', '行'], discs: null };
    r = { chars, discs: null };
    ui.intro.rain = r;
    return r;
  }

  function introDrawRainDisc(o, rain, s) {
    const R = o._r, x = o._x, y = o._y;
    if (R <= 0) return;
    const hue = o.hue;
    const COLS = 9;
    const glyph = (2 * R) / COLS;
    const colW = glyph;
    const fontPx = glyph * 0.92;
    if (!rain.discs) rain.discs = {};
    const di = o.seed;
    const bucket = Math.round(glyph);
    let st = rain.discs[di];
    if (!st || st.bucket !== bucket) {
      const cols = [];
      const n = rain.chars.length;
      for (let i = 0; i < COLS; i++) {
        cols.push({
          off: Math.floor(Math.random() * n),
          head: Math.random() * (2 * R),
          speed: glyph * (0.06 + Math.random() * 0.07),
          len: 6 + Math.floor(Math.random() * 8),
        });
      }
      st = rain.discs[di] = { bucket, cols };
    }

    const halo = ctx.createRadialGradient(x, y, R * 0.5, x, y, R * 1.65);
    halo.addColorStop(0, 'rgba(' + hue + ',0.22)');
    halo.addColorStop(0.55, 'rgba(' + hue + ',0.08)');
    halo.addColorStop(1, 'rgba(' + hue + ',0)');
    ctx.fillStyle = halo;
    ctx.beginPath(); ctx.arc(x, y, R * 1.65, 0, Math.PI * 2); ctx.fill();

    const lx = x - R * 0.38, ly = y - R * 0.38;
    const body = ctx.createRadialGradient(lx, ly, R * 0.05, x, y, R);
    body.addColorStop(0, '#272019');
    body.addColorStop(0.5, '#13100d');
    body.addColorStop(1, '#060505');
    ctx.fillStyle = body;
    ctx.beginPath(); ctx.arc(x, y, R, 0, Math.PI * 2); ctx.fill();

    ctx.save();
    ctx.beginPath(); ctx.arc(x, y, R, 0, Math.PI * 2); ctx.clip();

    const tint = ctx.createRadialGradient(lx, ly, 0, lx, ly, R * 1.25);
    tint.addColorStop(0, 'rgba(' + hue + ',0.32)');
    tint.addColorStop(0.6, 'rgba(' + hue + ',0.10)');
    tint.addColorStop(1, 'rgba(' + hue + ',0)');
    ctx.fillStyle = tint;
    ctx.fillRect(x - R, y - R, 2 * R, 2 * R);

    const bx = x + R * 0.72, by = y + R * 0.72;
    const rim = ctx.createRadialGradient(bx, by, R * 0.2, bx, by, R * 1.05);
    rim.addColorStop(0, 'rgba(' + hue + ',0.20)');
    rim.addColorStop(1, 'rgba(' + hue + ',0)');
    ctx.fillStyle = rim;
    ctx.fillRect(x - R, y - R, 2 * R, 2 * R);

    ctx.font = '700 ' + fontPx + 'px ' + INTRO_SANS;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    const top = y - R, left = x - R;
    const wrap = 2 * R + glyph * 16;
    const n = rain.chars.length;
    for (let ci = 0; ci < st.cols.length; ci++) {
      const col = st.cols[ci];
      col.head += col.speed;
      if (col.head > wrap) col.head -= wrap;
      const cx = left + (ci + 0.5) * colW;
      for (let k = 0; k < col.len; k++) {
        const gy = top + col.head - k * glyph;
        if (gy < top - glyph || gy > top + 2 * R + glyph) continue;
        const d = Math.hypot(cx - x, gy - y) / R;
        const fall = d >= 1 ? 0 : (1 - d * d);
        if (fall <= 0.01) continue;
        const ch = rain.chars[(col.off + ci * 3 + k) % n];
        if (k === 0) {
          ctx.fillStyle = 'rgba(' + hue + ',' + (0.45 * fall).toFixed(3) + ')';
        } else {
          const a = Math.max(0, 1 - k / col.len) * 0.22 * fall;
          ctx.fillStyle = 'rgba(' + hue + ',' + a.toFixed(3) + ')';
        }
        ctx.fillText(ch, cx, gy);
      }
    }
    ctx.restore();

    const sh = ctx.createRadialGradient(lx, ly, 0, lx, ly, R * 0.6);
    sh.addColorStop(0, 'rgba(246,240,228,0.14)');
    sh.addColorStop(1, 'rgba(246,240,228,0)');
    ctx.fillStyle = sh;
    ctx.beginPath(); ctx.arc(x, y, R, 0, Math.PI * 2); ctx.fill();
  }

  function drawIntro(s, elapsed) {
    const it = ui.intro;
    if (!it.active) return;
    const P = INTRO_PAL;

    it.alpha = springStep(it.alpha, it.alphaT, 0.16);
    if (it.alphaT === 0 && it.alpha < 0.02) { teardownIntro(); return; }

    const W = canvas.width, H = canvas.height;
    const portrait = H > W;
    const blocks = (it.blocks && it.blocks.length) ? it.blocks : [{ type: 'h1', text: 'bob' }];

    ctx.save();
    ctx.globalAlpha = it.alpha;
    ctx.textAlign = 'left';
    ctx.textBaseline = 'alphabetic';

    const g = ctx.createRadialGradient(W / 2, H * 0.3, 0, W / 2, H * 0.3, Math.max(W, H) * 1.05);
    g.addColorStop(0, P.bg0);
    g.addColorStop(1, P.bg1);
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, W, H);

    const orbs = introOrbs();
    const orbR = Math.min(W, H);
    const rain = introRain();

    const TAU = 0.5;
    const SOFT2 = 0.05 * 0.05;
    const CW = 0.15;
    const CD = 0.55;
    const WALL = 40;
    const E_HI = -0.85, E_LO = -1.75;
    const E_SC = 0.9;
    const DRATE = 0.6, BRATE = 0.25;
    const V_CAP = 5;

    const geo = { cx: W / 2, cy: H / 2, L: Math.max(1, Math.min(0.25 * W, 0.34 * H)) };
    it.geo = geo;

    let dt = elapsed - (it._lastT || elapsed);
    it._lastT = elapsed;
    if (dt > 0.05) dt = 0.05;

    for (const o of orbs) {
      o._r = o.rk * orbR;
      o._rEx = o._r + 6 * s;
      o._pad = 7 * s;
    }

    const held = (it.drag && it.drag.down) ? it.drag.orb : null;

    const accel = () => {
      for (const o of orbs) { o._ax = 0; o._ay = 0; }
      for (let i = 0; i < orbs.length; i++) {
        for (let j = i + 1; j < orbs.length; j++) {
          const a = orbs[i], b = orbs[j];
          const dx = b.ux - a.ux, dy = b.uy - a.uy;
          const r2 = dx * dx + dy * dy + SOFT2;
          const inv = 1 / (r2 * Math.sqrt(r2));
          a._ax += dx * inv; a._ay += dy * inv;
          b._ax -= dx * inv; b._ay -= dy * inv;
        }
      }
      let cmx = 0, cmy = 0, cvx = 0, cvy = 0;
      for (const o of orbs) { cmx += o.ux; cmy += o.uy; cvx += o.uvx; cvy += o.uvy; }
      cmx /= orbs.length; cmy /= orbs.length; cvx /= orbs.length; cvy /= orbs.length;
      const rcx = -CW * cmx - CD * cvx, rcy = -CW * cmy - CD * cvy;
      for (const o of orbs) {
        o._ax += rcx; o._ay += rcy;
        const ex = (W / 2 - o._r) / geo.L, ey = (H / 2 - o._r) / geo.L;
        if (o.ux < -ex) o._ax += WALL * (-ex - o.ux);
        if (o.ux > ex) o._ax -= WALL * (o.ux - ex);
        if (o.uy < -ey) o._ay += WALL * (-ey - o.uy);
        if (o.uy > ey) o._ay -= WALL * (o.uy - ey);
        if (o === held) {
          const om = (10 / TAU) * (0.15 / o.rk) * (0.15 / o.rk);
          o._ax += om * om * (it.drag.tux - o.ux) - 2 * 0.9 * om * o.uvx;
          o._ay += om * om * (it.drag.tuy - o.uy) - 2 * 0.9 * om * o.uvy;
        }
      }
    };

    if (dt > 0 && orbR > 0) {
      const dts = dt * TAU;
      const steps = Math.max(1, Math.ceil(dts / 0.01));
      const h = dts / steps;
      for (let stp = 0; stp < steps; stp++) {
        accel();
        for (const o of orbs) {
          o.ux += (o.uvx + 0.5 * o._ax * h) * h;
          o.uy += (o.uvy + 0.5 * o._ay * h) * h;
          o.uvx += 0.5 * o._ax * h; o.uvy += 0.5 * o._ay * h;
        }
        accel();
        for (const o of orbs) {
          o.uvx += 0.5 * o._ax * h; o.uvy += 0.5 * o._ay * h;
        }
      }
      let ke = 0, pe = 0;
      for (const o of orbs) {
        if (o === held) continue;
        ke += 0.5 * (o.uvx * o.uvx + o.uvy * o.uvy);
      }
      for (let i = 0; i < orbs.length; i++) {
        for (let j = i + 1; j < orbs.length; j++) {
          const dx = orbs[j].ux - orbs[i].ux, dy = orbs[j].uy - orbs[i].uy;
          pe -= 1 / Math.sqrt(dx * dx + dy * dy + SOFT2);
        }
      }
      const E = ke + pe;
      let kf = 1;
      if (E > E_HI) kf = Math.exp(-DRATE * dts * Math.min((E - E_HI) / E_SC, 2));
      else if (E < E_LO) kf = Math.exp(BRATE * dts * Math.min((E_LO - E) / E_SC, 2));
      for (const o of orbs) {
        if (o === held) continue;
        o.uvx *= kf; o.uvy *= kf;
        const sp = Math.hypot(o.uvx, o.uvy);
        if (sp > V_CAP) { o.uvx *= V_CAP / sp; o.uvy *= V_CAP / sp; }
      }
    }

    for (const o of orbs) {
      o._x = geo.cx + o.ux * geo.L;
      o._y = geo.cy + o.uy * geo.L;
      introDrawRainDisc(o, rain, s);
    }

    const marginX = Math.max(46 * s, W * 0.055);
    const fullW = W - 2 * marginX;
    const topPad = (portrait ? 0.07 : 0.09) * H;
    let hy = topPad;
    const h1 = blocks.find(b => b.type === 'h1');
    const kicker = blocks.find(b => b.type === 'kicker');
    if (h1) {
      const fs = Math.min(portrait ? 56 * s : 104 * s, fullW / Math.max(4, [...h1.text].length) * 2.4);
      ctx.font = '700 ' + fs + 'px ' + INTRO_SERIF;
      ctx.fillStyle = P.head;
      introLS(-1.2 * s);
      hy += fs;
      ctx.fillText(h1.text, marginX, hy);
      introLS(0);
      hy += fs * 0.16;
    }
    if (kicker) {
      const fs = (portrait ? 11 : 12.5) * s;
      ctx.font = '400 ' + fs + 'px ' + INTRO_SANS;
      ctx.fillStyle = P.accent;
      introLS(2.5 * s);
      hy += fs * 1.5;
      ctx.fillText(kicker.text.toUpperCase(), marginX, hy);
      introLS(0);
    }
    hy += (portrait ? 14 : 18) * s;
    ctx.strokeStyle = P.hr;
    ctx.lineWidth = 1 * s;
    ctx.beginPath(); ctx.moveTo(marginX, hy); ctx.lineTo(marginX + fullW, hy); ctx.stroke();

    const yTop = hy + (portrait ? 14 : 20) * s;
    const yBot = H - (portrait ? 0.085 : 0.09) * H;
    const N = Math.max(1, Math.min(5, Math.round(fullW / (250 * s))));
    const gutter = 26 * s;
    const colW = (fullW - (N - 1) * gutter) / N;
    const cols = [];
    for (let i = 0; i < N; i++) cols.push({ x: marginX + i * (colW + gutter), w: colW });

    const built = introBuildParas(blocks);
    const paras = built.paras, capChar = built.capChar;
    if (!it.fit || it.fit.W !== W || it.fit.H !== H || it.fit.N !== N || it.fit.src !== it.blocks) {
      const fs = introFit(paras, cols, yTop, yBot, s, capChar, orbs);
      it.fit = { W, H, N, src: it.blocks, fs, lh: fs * 1.62 };
    }
    introFlow(paras, cols, yTop, yBot, it.fit.fs, it.fit.lh, orbs, true, P, capChar);

    const hfs = (portrait ? 11 : 12.5) * s;
    ctx.font = '400 ' + hfs + 'px ' + INTRO_SANS;
    ctx.fillStyle = P.hint;
    ctx.textAlign = 'center';
    introLS(2 * s);
    ctx.fillText('drag the orbs  ·  touch elsewhere to enter', W / 2, H - (portrait ? 22 : 30) * s);
    introLS(0);

    ctx.restore();
    ctx.globalAlpha = 1;
  }

  // While the intro is up, hide the HTML dock (it sits above the canvas in DOM);
  // teardownIntro restores it.
  dock.style.display = 'none';

  // Fetch the editable intro copy once per load (the endpoint always 200s at
  // least the embedded default; .catch only guards a hard network failure).
  fetch('/api/intro', { cache: 'no-store' })
    .then(r => (r.ok ? r.text() : Promise.reject(r.status)))
    .then(md => { ui.intro.blocks = parseIntro(md); })
    .catch(() => { ui.intro.blocks = parseIntro('# bob'); });

  // ===========================================================================
  // §INTERACTIONS — hover / click / keydown / dock
  // ===========================================================================
  canvas.addEventListener('pointermove', function (e) {
    bumpInteract();
    if (ui.intro.active) {
      const d = ui.intro.drag;
      if (d.down) {
        const dp = devicePos(e);
        d.moved = Math.max(d.moved, Math.hypot(dp.x - d.sx, dp.y - d.sy));
        if (d.orb && ui.intro.geo) {
          const g = ui.intro.geo;
          d.tux = (dp.x - g.cx) / g.L - d.gux;
          d.tuy = (dp.y - g.cy) / g.L - d.guy;
        }
      }
      return;
    }
    const p = devicePos(e);
    if (isTakeoverFg()) {
      if (inTakeoverButton(e) || inSaveButton(e) || inHelpButton(e) || inQualityButton(e)) { canvas.style.cursor = 'pointer'; return; } // the dock-side toggle + 💾 save are clickable
      const pt = takeoverPoint(e); if (pt) sendInput({ kind: 'mouse', mouseType: 'move', x: pt.x, y: pt.y }); // sendInput no-ops in watch mode
      canvas.style.cursor = ui.takeover.mode === 'control' ? 'default' : 'not-allowed'; return;
    }
    // dial spin: horizontal drag over the agora turntable layer rotates it.
    if (ui.dial.active && ui.dial.drag.down) {
      const dx = p.x - ui.dial.drag.lastX; ui.dial.drag.lastX = p.x; ui.dial.drag.moved += Math.abs(dx);
      const dA = dx / (ui.dial.maxR || 300); ui.dial.rot += dA; ui.dial.target += dA;
      canvas.style.cursor = 'grabbing'; return;
    }
    let key = null;
    if (ui.stack[0] === 'home') {
      const t = ui.entityLayerXform, lx = (p.x - t.ox) / t.scale, ly = (p.y - t.oy) / t.scale;
      for (const n of ui.nodeHits) if (hit({ x: lx, y: ly }, n)) { key = 'node:' + n.id; break; }
    } else { // a form layer is foreground: hover its hit-rects (nodes by id; buttons /
      // links carry a hoverKey; any actionable rect at least flips the cursor).
      const x = ui.entityLayerXform, lx = (p.x - x.ox) / x.scale, ly = (p.y - x.oy) / x.scale;
      for (const hr of ui.entityHitRects) {
        if (lx >= hr.x && lx <= hr.x + hr.w && ly >= hr.y && ly <= hr.y + hr.h) {
          key = hr.id ? 'node:' + hr.id : (hr.hoverKey || (hr.action ? 'hit' : null));
          if (key) break;
        }
      }
    }
    ui.hover.rowKey = key;
    canvas.style.cursor = key ? 'pointer' : (ui.dial.active ? 'grab' : 'default');
  });

  // Orb grab/release (intro splash only).
  canvas.addEventListener('pointerdown', function (e) {
    if (!ui.intro.active) {
      if (ui.dial.active) { // begin a drag-spin on the turntable layer
        const dp = devicePos(e);
        ui.dial.drag.down = true; ui.dial.drag.lastX = dp.x; ui.dial.drag.moved = 0;
        try { canvas.setPointerCapture(e.pointerId); } catch (_) { }
      }
      return;
    }
    bumpInteract();
    const dp = devicePos(e), d = ui.intro.drag;
    d.down = true; d.moved = 0; d.sx = dp.x; d.sy = dp.y; d.orb = null;
    const g = ui.intro.geo;
    for (const o of (g ? ui.intro.orbs || [] : [])) {
      if (o._x != null && Math.hypot(dp.x - o._x, dp.y - o._y) <= o._r) {
        d.orb = o;
        d.gux = (dp.x - g.cx) / g.L - o.ux;
        d.guy = (dp.y - g.cy) / g.L - o.uy;
        d.tux = o.ux; d.tuy = o.uy;
        break;
      }
    }
    try { canvas.setPointerCapture(e.pointerId); } catch (_) { }
  });
  function endDrag(e) {
    if (ui.dial.drag.down) { // turntable: drag leaves the wheel where you put it (no snap-back)
      ui.dial.drag.down = false;
      if (e && e.pointerId != null) { try { canvas.releasePointerCapture(e.pointerId); } catch (_) { } }
      return;
    }
    if (!ui.intro.active) return;
    ui.intro.drag.down = false; // leave drag.orb set so click can tell orb-release from a bare tap
    if (e && e.pointerId != null) { try { canvas.releasePointerCapture(e.pointerId); } catch (_) { } }
  }
  canvas.addEventListener('pointerup', endDrag);
  canvas.addEventListener('pointercancel', endDrag);

  // Takeover mouse: press / release / wheel sent to the live browser (move is in the
  // pointermove handler). Additive listeners — they no-op unless a takeover layer is
  // foreground (and the existing handlers no-op for takeover: dial isn't active).
  canvas.addEventListener('pointerdown', function (e) {
    if (ui.intro.active || !isTakeoverFg()) return;
    if (ui.takeover.mode !== 'control' || inTakeoverButton(e) || inSaveButton(e) || inHelpButton(e) || inQualityButton(e)) return; // watch mode / the toggle button: no press
    const pt = takeoverPoint(e);
    if (!pt) return; // press only starts inside the frame
    ui.takeover.down = true;
    try { canvas.setPointerCapture(e.pointerId); } catch (_) { } // so a release outside the canvas still reaches us
    sendInput({ kind: 'mouse', mouseType: 'press', x: pt.x, y: pt.y, button: 'left', clickCount: 1 });
    // Local press feedback: paint a brief ring at the click so a laggy remote frame still
    // confirms "your click landed" — this is what stops the user re-mashing (and the
    // re-mash burst) when the stream trails behind. Stored in device coords for the draw loop.
    const dp = devicePos(e);
    ui.takeover.clicks.push({ x: dp.x, y: dp.y, t: performance.now() });
    if (ui.takeover.clicks.length > 6) ui.takeover.clicks.shift();
  });
  canvas.addEventListener('pointerup', function (e) {
    if (!ui.takeover.down) return; // only release a press we started
    ui.takeover.down = false;
    try { canvas.releasePointerCapture(e.pointerId); } catch (_) { }
    const pt = takeoverPointClamped(e); // clamp to the edge so an off-frame release still fires
    // raw post (not sendInput): a press begun in control MUST be released even if control
    // was dropped mid-drag (lease failure / hand-back), else the remote button stays held.
    if (pt) postBrowserInput({ kind: 'mouse', mouseType: 'release', x: pt.x, y: pt.y, button: 'left' });
  });
  canvas.addEventListener('wheel', function (e) {
    if (!isTakeoverFg()) {
      // Scroll the foreground layer. Only claim the wheel when the content actually
      // overflows — on a panel that fits, preventDefault would swallow the gesture and
      // make the page feel dead for no gain.
      const id = ui.stack[0];
      if (layerMaxScroll(id) <= 0) return;
      e.preventDefault();
      const pan = ui.layerPan[id] || (ui.layerPan[id] = { tx: 0, ty: 0 });
      pan.ty -= e.deltaY;
      clampLayerPan(id, pan); // momentum/trackpad fires faster than rAF — clamp per event, not per frame
      bumpInteract();
      return;
    }
    e.preventDefault(); // takeover owns the wheel (don't scroll the host) whether or not over the frame
    if (ui.takeover.mode !== 'control' || inTakeoverButton(e) || inSaveButton(e) || inHelpButton(e) || inQualityButton(e)) return; // watch mode / the toggle button: no scroll
    const pt = takeoverPoint(e);
    if (pt) sendInput({ kind: 'mouse', mouseType: 'wheel', x: pt.x, y: pt.y, deltaX: e.deltaX, deltaY: e.deltaY });
  }, { passive: false });

  canvas.addEventListener('click', function (e) {
    bumpInteract();
    // Intro swallows clicks; a tap on empty space (not an orb, not a drag) dismisses.
    if (ui.intro.active) {
      const d = ui.intro.drag;
      if (ui.intro.alphaT !== 0 && d.moved < 6 && !d.orb) dismissIntro();
      return;
    }
    if (isTakeoverFg()) { // the chip toggles watch⇄control; 💾 saves+hands back; ❓ help; frame clicks ride press/release
      if (inHelpButton(e)) { ui.takeover.help = !ui.takeover.help; return; }
      if (ui.takeover.help) { ui.takeover.help = false; return; } // any click dismisses the help overlay
      if (inSaveButton(e)) { doSaveLogin(); return; }
      if (inQualityButton(e)) { cycleTakeoverQuality(); return; }
      if (inTakeoverButton(e)) setTakeoverMode(ui.takeover.mode === 'control' ? 'watch' : 'control');
      return;
    }
    if (inLogoutButton(e)) { doLogout(); return; } // ⎋ chip: end the admin session (any layer)
    const p = devicePos(e);
    const fg = ui.stack[0];
    if (fg === 'home') {
      const t = ui.entityLayerXform, lx = (p.x - t.ox) / t.scale, ly = (p.y - t.oy) / t.scale;
      for (const n of ui.nodeHits) if (hit({ x: lx, y: ly }, n)) { n.action && n.action(); return; }
      return;
    }
    // a turntable spin (a drag, not a tap) must not fire a node action.
    if (ui.dial.active && ui.dial.drag.moved > 6) { ui.dial.drag.moved = 0; return; }
    // A form is foreground: test only ITS hit-rects (inverse the layer xform). A
    // miss stays in this layer — clicks never fall through to the layer behind;
    // dismiss/navigate with Esc / Tab.
    const x = ui.entityLayerXform, lx = (p.x - x.ox) / x.scale, ly = (p.y - x.oy) / x.scale;
    for (const hr of ui.entityHitRects) {
      if (lx >= hr.x && lx <= hr.x + hr.w && ly >= hr.y && ly <= hr.y + hr.h) { hr.action && hr.action(); return; }
    }
  });

  window.addEventListener('keydown', function (e) {
    if (ui.intro.active) {
      if (e.key === 'Escape' || e.key === 'Enter') { dismissIntro(); e.preventDefault(); }
      return;
    }
    if (e.target === dock) return;                  // focus in the dock → don't listen (it owns its keys)
    if (e.key === 'Escape') { bumpInteract(); closeForm(); }                          // back
    else if (e.key === 'Tab') { e.preventDefault(); bumpInteract(); navForward(); }   // forward (same side as Esc)
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {                        // page a foreground paged table (e.g. chat history)
      const pn = ui.pageNav;
      if (pn) {
        e.preventDefault(); bumpInteract();
        if (e.key === 'ArrowLeft') ui.pageOffset[pn.key] = Math.max(0, pn.offset - pn.pageSize);
        else if (pn.offset + pn.pageSize < pn.total) ui.pageOffset[pn.key] = pn.offset + pn.pageSize;
      }
    }
  });

  function hit(p, r) { return p.x >= r.x && p.x <= r.x + r.w && p.y >= r.y && p.y <= r.y + r.h; }

  // The dock is the 全场唯一输入框 with two modes:
  //   • model mode (a model form is active): the textarea IS the editor — its text
  //     is the edit buffer; ⌘/Ctrl+Enter saves, Esc cancels, Enter inserts a line.
  //   • command mode: one line — a pasted 32-hex token unlocks; otherwise an admin
  //     command runs via /api/run (output shown as a glance form).
  dock.addEventListener('input', function () {
    if (ui.model.id && !ui.model.loading) ui.model.body = dock.value;
    else if (ui.pick.id) ui.pick.filter = dock.value;
  });
  dock.addEventListener('keydown', function (e) {
    if (ui.model.id) {                              // model edit mode
      if (e.key === 'Escape') { e.preventDefault(); closeForm(); }
      else if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); saveModel(); }
      return;                                       // text + newlines handled by the textarea
    }
    if (ui.pick.id) {                              // picker filter mode
      if (e.key === 'Escape') { e.preventDefault(); closeForm(); }
      return;                                       // typing filters via the input listener
    }
    if (isTakeoverFg()) {                           // takeover text-input: Enter inserts the typed text (IME-safe)
      if (e.key === 'Escape') { e.preventDefault(); dock.blur(); return; }
      if (e.key !== 'Enter') return;
      e.preventDefault();
      // kind:'text' → CDP Input.insertText = whole-string insertion into the browser's
      // focused field; carries composed CJK/unicode correctly (a per-keystroke char event
      // never sees IME output). Only control mode types into the browser.
      if (dock.value && ui.takeover.mode === 'control') sendInput({ kind: 'text', text: dock.value });
      dock.value = '';
      return;
    }
    if (e.key === 'Escape') { e.preventDefault(); dock.blur(); return; } // release focus → canvas nav works
    if (e.key !== 'Enter') return;
    e.preventDefault();                             // command mode is single-line
    const v = dock.value.trim();
    if (v === '') return;
    if (/^[0-9a-f]{32}$/.test(v)) {
      fetch('/api/token', { method: 'POST', body: v })
        .then(function (r) {
          if (r.ok) { dock.value = ''; dock.placeholder = 'unlocked — management mode'; pollWhoami(); poll(); }
          else { dock.value = ''; dock.placeholder = 'invalid or expired token'; }
        })
        .catch(function () { dock.placeholder = 'network error'; });
      return;
    }
    if (ui.admin) { runCommand(v); dock.value = ''; }
    else { dock.value = ''; dock.placeholder = 'paste a /webui token to unlock management first'; }
  });

  // ===========================================================================
  // §MAIN-LOOP — rAF driver
  // ===========================================================================
  function springStep(cur, target, k) { return cur + (target - cur) * k; }

  function draw(now) {
    const elapsed = (now - ui.t0) / 1000;
    ui.lastElapsed = elapsed; // so click handlers can stamp ui.copiedAt
    const s = effectiveDPR * UI_SCALE; // logical px → device px (canvas 1:1 with viewport), × the global zoom

    for (const id of ui.stack) {
      if (ui.depthF[id] == null) ui.depthF[id] = ui.stack.indexOf(id);
      ui.depthF[id] = springStep(ui.depthF[id], ui.stack.indexOf(id), 0.12);
    }
    tickVitals(elapsed);
    gcLayers(now);
    syncTakeover(); // start/stop the screencast to match the foreground form
    ui.dial.active = false; // re-armed by drawOrgRadial only when the radial org layer is foreground

    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.fillStyle = BG0;
    ctx.fillRect(0, 0, canvas.width, canvas.height);

    // Repaint ONLY the front 3 stack entries (foreground + the two behind it).
    // Deeper layers stay in the stack for back/forward nav but are not drawn —
    // they stop updating, so a full stack (up to LAYER_HARD_CAP) costs a fixed 3
    // layers/frame instead of 20. (hector: "只重绘前3张，其它的暂停".)
    const drawn = Math.min(ui.stack.length, 3);
    for (let i = drawn - 1; i >= 0; i--) {
      try { paintLayer(ui.stack[i], s, elapsed); }
      catch (err) { /* a panel paint error must not kill the loop */ }
    }

    if (!ui.intro.active) { drawDockStatus(s); drawTakeoverButton(s); } // dot left of the dock; takeover toggle right of it

    // Intro splash — painted ABOVE everything until dismissed (opaque boot
    // splash, not a stack layer). Guarded so a sim error can't kill the loop.
    try { drawIntro(s, elapsed); } catch (err) { ui.intro.active = false; }

    requestAnimationFrame(draw);
  }
  // dockRectCanvas returns the dock's rect in CANVAS coords (its DOM rect is CSS px;
  // canvas coords = CSS px × effectiveDPR — the dock isn't UI-zoomed, so don't use s).
  function dockRectCanvas() {
    const r = dock.getBoundingClientRect(), d = effectiveDPR;
    return { left: r.left * d, right: r.right * d, top: r.top * d, midY: (r.top + r.height / 2) * d };
  }
  // drawDockStatus paints the persistent connection dot just LEFT of the dock — the only
  // status it carries now (lock/form/nav text was removed). Canvas-drawn, not a DOM node;
  // skipped while a model owns (expands) the dock.
  function drawDockStatus(s) {
    if (ui.model.id) { ui.logoutBtn = null; return; }
    const dr = dockRectCanvas(), dotR = 4 * s, gap = 10 * s;
    ctx.save();
    ctx.fillStyle = connStale() ? STATUS_COL.warn : STATUS_COL.ok;
    ctx.beginPath(); ctx.arc(dr.left - gap - dotR, dr.midY, dotR, 0, Math.PI * 2); ctx.fill();
    ctx.restore();
    // Logout chip (F102): pinned RIGHT of the dock, ONLY for a live admin session and
    // only when takeover isn't foreground (takeover owns that strip). ⎋ POSTs /api/logout
    // to end the management session; the UI falls back to read-only. Re-minting a /webui
    // token already voids the old session, but a plain logout is still worth having.
    if (ui.admin && !isTakeoverFg()) {
      const h = 30 * s, w = 40 * s, bx = dr.right + gap, by = dr.midY - h / 2;
      ctx.save();
      ctx.font = 16 * s + 'px ui-monospace, Menlo, monospace';
      ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
      ctx.fillStyle = 'rgba(15,13,11,0.82)'; ctx.fillRect(bx, by, w, h);
      ctx.strokeStyle = '#3a342c'; ctx.lineWidth = 1 * s; ctx.strokeRect(bx, by, w, h);
      ctx.fillStyle = '#cfd8e6'; ctx.fillText('⎋', bx + w / 2, dr.midY);
      ui.logoutBtn = { x: bx, y: by, w: w, h: h };
      ctx.restore();
    } else {
      ui.logoutBtn = null;
    }
  }
  // drawTakeoverButton paints the watch/control toggle just RIGHT of the dock, ONLY while
  // a takeover layer is foreground — takeover's own screen-fixed chrome (self-gated, not a
  // branch in the generic dock status). Records its screen rect for inTakeoverButton.
  // 👁 watch · 🎮 control · ⚠️ lease dropped.
  function drawTakeoverButton(s) {
    if (ui.model.id || !isTakeoverFg()) { ui.takeover.btn = null; ui.takeover.saveBtn = null; ui.takeover.helpBtn = null; ui.takeover.qBtn = null; ui.takeover.help = false; return; }
    const dr = dockRectCanvas(), gap = 10 * s;
    const h = 30 * s, w = 40 * s, bx = dr.right + gap, by = dr.midY - h / 2;
    const ctrl = ui.takeover.mode === 'control';
    const emoji = ctrl ? '🎮' : (ui.takeover.leaseFail ? '⚠️' : '👁');
    const cell = function (x, fill, stroke, fg, glyph) {
      ctx.fillStyle = fill; ctx.fillRect(x, by, w, h);
      ctx.strokeStyle = stroke; ctx.lineWidth = 1 * s; ctx.strokeRect(x, by, w, h);
      ctx.fillStyle = fg; ctx.fillText(glyph, x + w / 2, dr.midY);
    };
    ctx.save();
    ctx.font = 18 * s + 'px ui-monospace, Menlo, monospace';
    ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
    // watch/control toggle
    cell(bx, ctrl ? 'rgba(20,48,31,0.9)' : 'rgba(15,13,11,0.82)', ctrl ? ACCENT : '#3a342c', ctrl ? ACCENT : '#cfd8e6', emoji);
    ui.takeover.btn = { x: bx, y: by, w: w, h: h };
    // 保存登陆 (save the login back to the master + hand the browser back to bob): 💾
    const sx = bx + w + gap;
    cell(sx, 'rgba(15,13,11,0.82)', '#3a342c', '#cfd8e6', '💾');
    ui.takeover.saveBtn = { x: sx, y: by, w: w, h: h };
    // help: ❓ (toggles the English help overlay)
    const hx = sx + w + gap;
    cell(hx, 'rgba(15,13,11,0.82)', ui.takeover.help ? ACCENT : '#3a342c', ui.takeover.help ? ACCENT : '#cfd8e6', '❓');
    ui.takeover.helpBtn = { x: hx, y: by, w: w, h: h };
    // quality tier (Hi/Md/Lo/5s): the glyph shows the EFFECTIVE tier the backend reports
    // (ui.takeover.effTier), falling back to the requested tier until it arrives. When the
    // backend clamped below the request (busy box), the glyph goes amber so the user sees
    // it's system-limited, not a broken button. Click cycles the REQUEST. "slow" (5s) is
    // manual-only and never clamped, so it never shows amber.
    const qx = hx + w + gap;
    const rank = { slow: 1, low: 1, med: 2, high: 3 };
    const shown = ui.takeover.effTier || ui.takeover.quality;
    const clamped = ui.takeover.effTier && ui.takeover.effTier !== 'slow' && rank[ui.takeover.effTier] < rank[ui.takeover.quality];
    const qglyph = ({ high: 'Hi', med: 'Md', low: 'Lo', slow: '5s' })[shown] || 'Md';
    cell(qx, 'rgba(15,13,11,0.82)', clamped ? '#d8a657' : '#3a342c', clamped ? '#d8a657' : '#cfd8e6', qglyph);
    ui.takeover.qBtn = { x: qx, y: by, w: w, h: h };
    ctx.restore();
    if (ui.takeover.help) drawTakeoverHelp(s);
  }

  // drawTakeoverHelp overlays a concise English explanation of the takeover controls,
  // toggled by the ❓ button. Centered card; click ❓ again (or the card) to dismiss.
  function drawTakeoverHelp(s) {
    const lines = [
      ['Browser takeover', 'h'],
      ['', ''],
      ['👁  Watch', 'b'],
      ['Bob keeps driving; you only watch the live page.', 'd'],
      ['', ''],
      ['🎮  Control', 'b'],
      ['You take over — bob pauses. Your mouse & keyboard', 'd'],
      ['drive the browser. Log in / clear the wall here.', 'd'],
      ['', ''],
      ['💾  Save & hand back', 'b'],
      ['Saves the login to this member’s master profile', 'd'],
      ['and returns the browser to bob — it resumes with', 'd'],
      ['the login. Future tasks reuse it (no re-login).', 'd'],
      ['', ''],
      ['Hi / Md / Lo / 5s  Quality', 'b'],
      ['Cycle stream quality on a slow link. Text stays', 'd'],
      ['sharp at every tier (only size & frame rate drop).', 'd'],
      ['The server may auto-lower it when many are active.', 'd'],
      ['5s = 1 frame / 5 s (med size) — manual, for bad links.', 'd'],
      ['', ''],
      ['If you just close the tab, bob still resumes, but', 'd'],
      ['the login is only saved on an idle timeout.', 'd'],
    ];
    const fs = 13 * s, lh = fs * 1.5, pad = 18 * s;
    let maxw = 0;
    ctx.font = fs + 'px ui-monospace, Menlo, monospace';
    for (const [t] of lines) maxw = Math.max(maxw, ctx.measureText(t).width);
    const cw = maxw + pad * 2, ch = lines.length * lh + pad * 2;
    const cx = canvas.width / 2 - cw / 2, cy = canvas.height / 2 - ch / 2;
    ctx.save();
    ctx.fillStyle = 'rgba(8,7,6,0.94)'; ctx.fillRect(cx, cy, cw, ch);
    ctx.strokeStyle = ACCENT; ctx.lineWidth = 1 * s; ctx.strokeRect(cx, cy, cw, ch);
    ctx.textAlign = 'left'; ctx.textBaseline = 'top';
    let y = cy + pad;
    for (const [t, kind] of lines) {
      if (kind === 'h') { ctx.fillStyle = ACCENT; ctx.font = 'bold ' + fs + 'px ui-monospace, Menlo, monospace'; }
      else if (kind === 'b') { ctx.fillStyle = '#e6ddcf'; ctx.font = 'bold ' + fs + 'px ui-monospace, Menlo, monospace'; }
      else { ctx.fillStyle = '#9aa4b2'; ctx.font = fs + 'px ui-monospace, Menlo, monospace'; }
      ctx.fillText(t, cx + pad, y);
      y += lh;
    }
    ctx.restore();
    ui.takeover.helpCard = { x: cx, y: cy, w: cw, h: ch };
  }

  // paintLayer draws one stack entry. 'home' is the base page; every other entry
  // is a form, dispatched by its kind. An unknown kind throws — there is no fourth
  // form (夹死). The draw loop's try/catch keeps one bad paint from killing rAF.
  function paintLayer(id, s, elapsed) {
    if (id === 'home') return homeLayer(s, elapsed);
    const f = ui.forms[id];
    if (!f) return;                                 // stale entry mid-removal
    if (f.kind === 'layer') return renderLayer(id, f, s, elapsed);
    if (f.kind === 'glance') return renderGlance(id, f, s, elapsed);
    if (f.kind === 'model') return renderModel(id, f, s, elapsed);
    throw new Error('webui: unknown form kind "' + f.kind + '"');
  }

  // ===========================================================================
  // kickoff
  // ===========================================================================
  fit();
  poll(); setInterval(poll, 3000);
  pollWhoami(); setInterval(pollWhoami, 15000);
  requestAnimationFrame(draw);
})();
