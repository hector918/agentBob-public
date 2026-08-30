package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
)

// isAgoraCaller reports whether the turn's principal is an agora worker — a
// "member:" / "company:" / "agora:" stamped identity (vs the normal flow's
// admin/user). The single source of truth for this auth/delivery-routing fork,
// shared by send_message (agora-routed vs raw emit) and escalate_to_coo (bridge vs
// ride-the-reply) so the prefix set can't drift between two security-relevant
// branches. (L-S2)
func isAgoraCaller(ctx context.Context) bool {
	id, _ := contract.IdentityFrom(ctx)
	return strings.HasPrefix(id, "member:") || strings.HasPrefix(id, "company:") || strings.HasPrefix(id, "agora:")
}

// sendMessage emits a message to a target session scope through the internal
// bridge source: the message re-enters the gateway's inbound stream and wakes a
// turn at that scope, exactly as a wire event would.
//
// STAGE A (no agora, docs/agora-port.md §6.1): `to` is an explicit session scope
// — there is NO target name-resolution and NO send authorization. The tool
// degrades to a plain emit; warrant gates who may use it (tool:use:send_message).
//
// STAGE B (agora present, §5.2/§6.1): when contract.AgoraSend is wired AND the turn
// carries a scope (tc.Scope) that resolves, `to` may be a Member NAME or a scope:
// AgoraSend.ResolveTarget maps it to a destination scope, CheckSend authorizes
// the send (鉴权 = 通讯录), and `as` may name a role-granted credential. Anything
// that does not resolve as an agora target falls back to Stage A raw emit.
//
// Loop / fan-out protection is NOT done here: anti-cycle is an org-structure concern
// (don't grant a subordinate role tool:use:send_message → it can only auto-return, not
// dispatch back up), and concurrency is bounded by the model pool's per-entry semaphore
// + per-session turn serialization. The old runtime caller-chain guard was removed.
type sendMessage struct {
	gw    func() contract.Gateway   // lazy — the gateway may start after tools
	agora func() contract.AgoraSend // lazy — agora is optional; nil → Stage A
}

func (sendMessage) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "send_message",
		Description: "给某个会话发一条消息：消息会进入对方会话、触发它处理一轮。" +
			"to 可以填对方的成员名（agora 通讯录里的人）或目标会话的 scope；" +
			"body 是消息正文（UTF-8 纯文本）。",
		SideEffect: true, // a failed send the model may still claim as delivered (§6.3.4)
		// NOTE: the `as=` (send under a named credential) param is intentionally NOT
		// advertised — the ④b dispatcher that stamps the credential isn't wired, so Run
		// rejects any as= (below). Advertising it would just make the model fill a param
		// that always errors (a wasted round). Re-add here when the dispatcher lands.
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "to":   {"type": "string", "description": "对方的成员名，或目标会话的 scope"},
    "body": {"type": "string", "description": "消息正文（UTF-8 纯文本）"},
    "force_new_session": {"type": "boolean", "description": "开始一个全新任务时设 true（对方会另起一个全新会话来处理，不带旧任务的上下文）；追问、纠正、接着之前的任务时不要设（默认接着同一会话）"}
  },
  "required": ["to", "body"]
}`),
	}
}

func (sendMessage) Serialize() bool { return false }

func (s sendMessage) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var a struct {
		To              string `json:"to"`
		Body            string `json:"body"`
		As              string `json:"as"`
		ForceNewSession bool   `json:"force_new_session"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("send_message: 参数解析失败：" + err.Error())
	}
	a.To, a.Body, a.As = strings.TrimSpace(a.To), strings.TrimSpace(a.Body), strings.TrimSpace(a.As)
	if a.To == "" || a.Body == "" {
		return contract.ErrResult("send_message: to 和 body 都不能为空")
	}

	gw := s.gw()
	if gw == nil {
		return contract.ErrResult("send_message: 消息总线不可用")
	}
	emitter, ok := gw.SourceByName(contract.SourceNameInternal).(contract.MessageEmitter)
	if !ok {
		return contract.ErrResult("send_message: 内部投递通道未接线")
	}

	// `as=` (send under a named credential) is NOT carriable yet — the credential is
	// stamped on the dispatched event by the ④b dispatcher, which isn't wired. Reject
	// it rather than silently ignore a requested sender identity (which would mislead
	// the caller into thinking it sent as that account).
	if a.As != "" {
		return contract.ErrResult("send_message: as= 暂不支持（凭证投递随 dispatcher 落地）")
	}

	// An AGORA caller — the agora flow stamped a "member:<name>" (or a no-grants
	// "agora:..." sentinel; legacy "company:<co>/role:<role>") principal — MUST route
	// through agora auth: name-resolution + send check are MANDATORY, and a resolution
	// failure DENIES. It never falls through to the unchecked raw-emit path, so send
	// authorization can't be evaded by passing an unresolvable `to`. A non-agora caller
	// (the normal flow's admin/user) takes Stage A: a raw emit to the given scope.
	target := a.To
	if isAgoraCaller(ctx) {
		as := s.agoraSeam()
		if as == nil {
			return contract.ErrResult("send_message: agora 发送解析不可用")
		}
		resolved, _, rerr := as.ResolveTarget(ctx, tc.Scope, a.To)
		if rerr != nil {
			return contract.ErrResult("send_message: 无法解析目标 " + strconv.Quote(a.To) + "：" + rerr.Error())
		}
		target = resolved
		deliver, deny, cerr := as.CheckSend(ctx, tc.Scope, target)
		if cerr != nil {
			return contract.ErrResult("send_message: 发送鉴权检查失败：" + cerr.Error())
		}
		if deny != "" {
			return contract.ErrResult("send_message: " + deny)
		}
		// F87: emit to the RESOLVED inbox's own scope (a bridge target's own inbox scope,
		// not the verbatim external chat scope) — keeps the worker's internal event off the
		// operator's external-chat session so routeBridge's direction stays unambiguous.
		if deliver != "" {
			target = deliver
		}
	}

	now := time.Now()
	ev := contract.MessageEvent{
		ChatID:     target,
		MessageID:  strconv.FormatInt(now.UnixNano(), 10),
		Text:       a.Body,
		ReceivedAt: now,
		// Dispatch frame: send_message IS the dispatch tool, so every emit carries one.
		// CallerSessionID = the emitting turn's session — the caller this dispatch
		// returns to. Set on BOTH the Stage A (raw scope) and Stage B (agora-resolved)
		// paths, because the emitting turn IS the caller regardless of how `to` resolved.
		// "" when the tool runs outside a routed turn (tc.Sid unset). The woken worker
		// session records this as its caller_session_id (leaf/session), so its reply
		// routes back here. ForceNewSession opens a fresh session at the target instead
		// of resuming (an agent starting a new task). Loop prevention is org-structural
		// (see the file-level doc), not a runtime dispatch guard.
		Dispatch: &contract.MessageEventDispatchMeta{
			CallerSessionID: tc.Sid,
			ForceNewSession: a.ForceNewSession,
		},
	}
	if err := emitter.Emit(ev); err != nil {
		if errors.Is(err, contract.ErrBridgeBusy) {
			return contract.ErrResult("send_message: 投递通道忙，稍后重试").WithHint("稍等片刻再发同一条")
		}
		return contract.ErrResult("send_message: 投递失败：" + err.Error())
	}
	return contract.OKResult("已投递到 " + target)
}

// agoraSeam resolves the lazy AgoraSend thunk (nil-safe — the thunk may itself be
// nil when send_message was constructed Stage-A-only, e.g. in a unit test).
func (s sendMessage) agoraSeam() contract.AgoraSend {
	if s.agora == nil {
		return nil
	}
	return s.agora()
}
