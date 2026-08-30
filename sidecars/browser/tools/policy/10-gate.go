package policy

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agentbob/sidecars/browser/core"
)

// approvalWaitTimeout caps how long a dynamic policy gate waits for an
// admin /approve before auto-denying. 30 s matches the static-tier consent
// flow's timeout — same UX, same operator expectations.
const approvalWaitTimeout = 30 * time.Second

// EnforcePath runs the path policy and, on Approval, blocks on the admin
// /approve channel (or auto-refuses on banned). Returns (ok=true) when
// the caller may proceed, or (ok=false, refusal) for a refusal / denied /
// timed-out approval — the caller returns that structured result to the
// model (A4: core.ErrResult, sealed by the dispatcher).
//
// `actx` provides Approvals + Sink. Tool is the calling tool's name
// (shown in the approval prompt). What is a one-line preview of *what*
// the tool is about to touch (e.g. the path, or the truncated command).
func EnforcePath(ctx context.Context, p *Policy, actx core.AgentCtx, tool, what, path string) (ok bool, refusal core.ToolResult) {
	r := p.CheckPath(path)
	return enforceResult(ctx, r, actx, tool, what)
}

// EnforceCommand — same shape as EnforcePath, but evaluates a shell
// command / code blob against banned/approval command patterns.
func EnforceCommand(ctx context.Context, p *Policy, actx core.AgentCtx, tool, what, text string) (ok bool, refusal core.ToolResult) {
	r := p.CheckCommand(text)
	return enforceResult(ctx, r, actx, tool, what)
}

// enforceResult is the shared finish line. Banned → fast refuse (no
// approval prompt — "if not allowed, don't even ask"). Approval → emit
// prompt + block. Allow → proceed.
func enforceResult(ctx context.Context, r Result, actx core.AgentCtx, tool, what string) (bool, core.ToolResult) {
	switch r.Decision {
	case DecisionAllow:
		return true, core.ToolResult{}
	case DecisionRefuse:
		// The detailed reason names the matched banned segment / root — i.e.
		// $BOB_HOME + the credential/secret/memory tree layout — which must NOT
		// reach the model (prompt-info-hygiene, docs/prompt-module-design.md
		// §0.7). Keep it in slog/admin only; hand the model a generic refusal.
		slog.Warn("policy: refused tool target", "tool", tool, "reason", r.Reason)
		return false, core.ErrResult(tool + " refused: this target is off-limits by policy")
	case DecisionApproval:
		if actx.Approvals == nil {
			return false, core.ErrResult(fmt.Sprintf("%s requires approval (%s) but no approval manager is wired", tool, r.Reason))
		}
		id, ch := actx.Approvals.Register(actx.SessionScope, "", tool, core.TierAdminApproval)
		defer actx.Approvals.Unregister(id)
		if actx.Sink != nil {
			// ContentDelta (not TraceDelta) — approval prompts are
			// user-actionable and must show even when trace=off.
			actx.Sink.ContentDelta(fmt.Sprintf(
				"🔐 approval needed (admin): %s wants to touch %q (policy: %s) — admin /approve %s or /deny %s (30s)\n",
				tool, what, r.Reason, id, id,
			))
		}
		select {
		case dec := <-ch:
			return approvalEnvelope(tool, dec)
		case <-time.After(approvalWaitTimeout):
			// R21 drain mirrors agent-orchestrator/40-dispatch.go's
			// awaitApproval — Resolve may have sent on the buffered(1)
			// channel just before our timeout fired. Without the drain
			// the admin sees "ok" to their /approve while this caller
			// reports timeout, confusing both sides.
			select {
			case dec := <-ch:
				return approvalEnvelope(tool, dec)
			default:
				return false, core.ErrResult(fmt.Sprintf("%s: approval timeout (30s)", tool))
			}
		case <-ctx.Done():
			select {
			case dec := <-ch:
				return approvalEnvelope(tool, dec)
			default:
				return false, core.ErrResult(fmt.Sprintf("%s: approval cancelled (%v)", tool, ctx.Err()))
			}
		}
	}
	// Fail-closed default. The switch above is exhaustive over today's
	// Decision values; if a new one is ever added without a case here, deny
	// rather than silently grant (a gate must never fall open).
	return false, core.ErrResult(fmt.Sprintf("%s: internal error — unhandled policy decision", tool))
}

// approvalEnvelope folds an ApprovalDecision into the (ok, refusal)
// pair the caller expects. Centralised so the timeout / cancel drain
// arms share the success-path mapping.
func approvalEnvelope(tool string, dec core.ApprovalDecision) (bool, core.ToolResult) {
	if dec.Approved {
		return true, core.ToolResult{}
	}
	msg := fmt.Sprintf("%s: approval %s", tool, dec.Reason)
	if dec.By != "" {
		msg += " by " + dec.By
	}
	return false, core.ErrResult(msg)
}
