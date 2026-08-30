package browser

import "agentbob/sidecars/browser/core"

// SpecForSession (core.SessionAwareTool) gates the interactive
// browser_* tools — they only appear in the model's tool list once a
// browser_navigate has succeeded for this session_scope (chromedp data
// dir on disk).
//
// Why: pre-the model could see browser_snapshot /
// browser_click / browser_get_images / browser_vision without any
// prior navigate, and a recurring bug was the model trying to use
// them on user-attached telegram images (because browser_get_images
// "looks relevant" when a user just sent an image). Hiding them until
// the session has demonstrably been used as a browser kills the false
// affordance.
//
// browser_navigate is NOT gated — it's the entry point that produces
// the disk state. Always advertised so users can ask "open this URL"
// even on a fresh session. Once it succeeds, the other 11 tools wake
// up via Pool.HasSession() returning true on the next turn.
//
// gateOnSession is the single decision point. baseSpec is taken by
// value so we can address &spec without leaking the receiver.

func gateOnSession(pool *Pool, actx core.AgentCtx, baseSpec core.ToolSpec) *core.ToolSpec {
	if pool == nil || !pool.HasSession(actx.SessionScope) {
		return nil
	}
	return &baseSpec
}

func (b browserSnapshot) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserClick) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserType) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserPress) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserScroll) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserBack) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserDialog) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserConsole) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserGetImages) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserVision) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserCDP) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

func (b browserTab) SpecForSession(actx core.AgentCtx) *core.ToolSpec {
	return gateOnSession(b.pool, actx, b.Spec())
}

// Compile-time check: a representative gated tool satisfies the
// interface so renames / signature changes break the build instead of
// silently un-gating tools.
var _ core.SessionAwareTool = browserSnapshot{}
