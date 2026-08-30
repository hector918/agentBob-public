package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentbob/contract"
	"agentbob/leaf/tools/browserremote"
)

// escalateToCoo escalates a stuck task to a HUMAN — the company's operating human
// ("COO") — and ENDS the turn (exit door ②). It is the single "summon a human" door:
// it mints whatever capability token the escalation KIND needs, then delivers it down
// the channel that actually reaches a person. Today the one kind is browser_takeover:
// a login wall / captcha / 2FA the worker can't clear itself — it mints a scope-locked
// webui takeover token for the worker's live browser.
//
// Delivery (kind-agnostic — the reusable half):
//   - human-facing turn (not an agora worker): the token + message ride the turn's
//     final reply; the partner pastes the token into the dashboard, clears the wall,
//     hits 保存登陆, and the worker resumes on re-call.
//   - unattended agora worker (member: principal): there is no human on THIS turn, so
//     the escalation is pushed to the company BRIDGE (the company's to-human bus) via
//     the same internal-emit path send_message rides; agora forwards it to the
//     operator's real chat. Loud failure when there is no deliverable bridge — never a
//     silent "escalated" while nobody can receive the token. NoteResume records this
//     worker so the human's hand-back resumes the right session.
//
// New escalation kinds (approval / 2FA relay / …) add a branch to the mint switch + a
// `kind` enum value; the human-reaching half below is shared, written once.
type escalateToCoo struct {
	tk    contract.BrowserTakeover
	mint  func() contract.TakeoverMinter
	holds contract.BrowserControlHold
	gw    func() contract.Gateway   // internal-source emitter for the bridge path
	agora func() contract.AgoraSend // bridge resolution (worker path); nil → no bridge escalation
}

func (escalateToCoo) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "escalate_to_coo",
		Description: "把只有真人能处理的关卡升级给本公司运营负责人（COO）接管，并结束本轮。仅当你确实无法自行通过时使用。" +
			"kind=browser_takeover：浏览器卡在登录 / 验证码 / 二次验证——会生成接管令牌，真人在面板接管这个浏览器、登录后交还，你再继续。" +
			"message=一句话说明卡在哪里、需要对方做什么。",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"kind":{"type":"string","enum":["browser_takeover"],"description":"升级类型。browser_takeover=浏览器登录/验证码/二次验证需真人接管"},` +
			`"message":{"type":"string","description":"给真人的一句话说明：卡在哪、要对方做什么"}` +
			`},"required":["kind","message"]}`),
		EndsTurn: true, // process exit (hands off to a human) — excluded from channel-less sub-turns
	}
}

func (escalateToCoo) Serialize() bool { return false }

func (h escalateToCoo) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var p struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(args, &p)
	kind := strings.TrimSpace(p.Kind)
	msg := strings.TrimSpace(p.Message)

	// 1. Mint the kind-specific token + build the human-facing body.
	var body string
	switch kind {
	case "browser_takeover":
		b, res, ok := h.mintBrowserTakeover(ctx, tc, msg)
		if !ok {
			return res // already a shaped ErrResult
		}
		body = b
	default:
		return contract.ErrResult("escalate_to_coo: 未知的升级类型 " + strconv.Quote(kind) + "（目前仅支持 browser_takeover）")
	}

	// 2. Deliver to a human. An unattended agora worker (member:/company:/agora: principal)
	// has no human on this turn → push to the company bridge; otherwise the partner is on
	// this turn → ride the reply.
	if isAgoraCaller(ctx) {
		return h.deliverToBridge(ctx, tc, body)
	}
	return contract.ToolResult{OK: true, Data: "escalated", ExitRequest: &contract.ToolExit{Reply: body}}
}

// mintBrowserTakeover runs the browser_takeover guards (backend + live browser),
// records the resume note, and returns the human-facing body (token + scope, or an
// "already in progress" note when a takeover is live). ok=false → res is the ErrResult.
func (h escalateToCoo) mintBrowserTakeover(ctx context.Context, tc contract.ToolContext, msg string) (body string, res contract.ToolResult, ok bool) {
	minter := h.mint()
	if h.tk == nil || minter == nil {
		return "", contract.ErrResult("接管功能未启用（需要浏览器后端 + 面板同时在线）。"), false
	}
	// browserd's takeover is scope-keyed: the human takes over THIS worker's copy at the
	// wall it's stuck on, logs in, then 保存登陆 writes it back to the member master.
	key := browserremote.CopyScope(tc)
	if key == "" {
		return "", contract.ErrResult("当前没有会话上下文，无法移交浏览器。"), false
	}
	if live, _ := h.tk.LiveForSid(ctx, key); !live {
		return "", contract.ErrResult("当前没有活动的浏览器可以移交（先用 browser 打开页面再升级）。"), false
	}
	if msg == "" {
		msg = "浏览器卡在需要真人处理的登录/验证，请接管。"
	}
	// Always mint a FRESH token on every (re-)escalation. MintTakeover hard-resets this
	// coverage, so the newest token simply supersedes any prior one — and the browser copy
	// (keyed by scope, untouched by minting) stays live, so a human just pastes the latest
	// token and lands back on the same browser. No suppression: an earlier pending token that
	// was missed / never delivered must never leave a re-escalation tokenless.
	tok := minter.MintTakeover(key)
	if tok == "" {
		return "", contract.ErrResult("暂时无法生成接管令牌，请稍后再试。"), false
	}
	// Record this worker so the hand-back (Clear of the scope-keyed hold) resumes THIS
	// session — AFTER a successful mint, so a mint failure never leaves a stale resume
	// note that a later proactive /takeover would act on for an escalation that never
	// actually delivered a token.
	if h.holds != nil && tc.Sid != "" {
		h.holds.NoteResume(key, tc.Sid)
	}
	return msg + fmt.Sprintf("\n\n浏览器接管令牌（5 分钟内有效，单次使用）：\n%s\n\n打开面板把令牌粘贴进去即可接管这个浏览器（scope：%s）；登录 / 清验证后点「保存登陆」，我会接着用。", tok, key), contract.ToolResult{}, true
}

// deliverToBridge pushes body to the worker's company bridge (the to-human bus) and
// ends the turn with NO return reply (the escalation went sideways to the bridge, not
// back up the caller chain). Loud failure when the bridge is missing / undeliverable.
func (h escalateToCoo) deliverToBridge(ctx context.Context, tc contract.ToolContext, body string) contract.ToolResult {
	as, gw := h.agoraSeam(), h.gwSeam()
	if as == nil || gw == nil {
		return contract.ErrResult("escalate_to_coo: 升级通道未接线（agora / 消息总线不可用）。")
	}
	bridgeScope, deliverable, err := as.ResolveBridge(ctx, tc.Scope)
	if err != nil {
		return contract.ErrResult("escalate_to_coo: 解析公司 bridge 失败：" + err.Error())
	}
	if bridgeScope == "" {
		return contract.ErrResult("escalate_to_coo: 本公司没有对人 bridge，无法升级给真人（需管理员配置 bridge）。")
	}
	if !deliverable {
		return contract.ErrResult("escalate_to_coo: 公司 bridge 未绑定真人聊天，消息发不出去（需管理员 wire bridge）。")
	}
	emitter, ok := gw.SourceByName(contract.SourceNameInternal).(contract.MessageEmitter)
	if !ok {
		return contract.ErrResult("escalate_to_coo: 内部投递通道未接线。")
	}
	now := time.Now()
	ev := contract.MessageEvent{
		ChatID:     bridgeScope,
		MessageID:  strconv.FormatInt(now.UnixNano(), 10),
		Text:       body,
		ReceivedAt: now,
		// CallerSessionID = this worker, so a bridge reply (if any) routes back here.
		Dispatch: &contract.MessageEventDispatchMeta{CallerSessionID: tc.Sid},
	}
	if err := emitter.Emit(ev); err != nil {
		return contract.ErrResult("escalate_to_coo: 推送到公司 bridge 失败：" + err.Error())
	}
	// Empty-reply exit: the worker goes idle (no return up the caller chain), resumes
	// when the human finishes the login and hands back.
	return contract.ToolResult{OK: true, Data: "escalated_to_bridge", ExitRequest: &contract.ToolExit{}}
}

func (h escalateToCoo) agoraSeam() contract.AgoraSend {
	if h.agora == nil {
		return nil
	}
	return h.agora()
}

func (h escalateToCoo) gwSeam() contract.Gateway {
	if h.gw == nil {
		return nil
	}
	return h.gw()
}
