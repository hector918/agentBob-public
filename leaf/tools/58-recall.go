package tools

import (
	"context"
	"encoding/json"
	"log/slog"

	"agentbob/contract"
)

// recallTool exposes the cold-memory leaf to the model as `recall_memory`. The
// model supplies the query; bob CLAMPS the scope from the flow-resolved identity
// (contract.ToolContext.Caller) so a turn can never read outside its bounds — a
// DM caller only its own history, a group the shared room. The tool itself holds
// NO identity logic beyond reading Caller. Lazily resolves the OPTIONAL retrieval
// client (nil when retrieval is disabled → the tool reports unavailable).
// See docs/cold-memory-trunk.md §4.
type recallTool struct {
	client func() contract.RetrievalClient
}

const recallParams = `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "要回忆的内容（自然语言）。例：\"上次关于定价的结论\"、\"这个客户抱怨过什么\"。"
    },
    "who": {
      "type": "string",
      "description": "可选：聚焦某个人/公司/项目的名字（跨语言、变体也能召回，如 \"凤凰项目\" 也命中 \"Phoenix\"）。给了就按对象检索。"
    },
    "mode": {
      "type": "string",
      "enum": ["mention", "both"],
      "description": "可选，仅当给了 who：mention=只要提到此人/物的；both=提到的+语义相关的（默认 both）。"
    },
    "time_range": {
      "type": "array",
      "items": {"type": "string"},
      "description": "可选 [起, 止]（ISO 日期/时间），把回忆限定在这个时段。"
    },
    "limit": {
      "type": "integer",
      "description": "可选，返回多少条（默认 10，上限 50）。"
    }
  }
}`

func (recallTool) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name: "recall_memory",
		Description: "从长期对话记忆里回忆过去说过/发生过的事，返回带精确时间戳的原话作依据。" +
			"检索范围已自动按当前会话夹定（私聊只回忆你自己说过的，群里回忆本群的）——你只管给 query 或 who。",
		Parameters:     json.RawMessage(recallParams),
		NoAutoCompress: true, // 召回的是原话 + 时间证据，别 summarize 掉
		SelectionHint: &contract.SelectionHint{
			When: `用户想回忆、想起以前的事，或提到"之前 / 刚才 / 上次 / 还记得 / 我们聊过 / 你说过"——要找的是这个对话、这个群里出现过的人、事、结论`,
			Then: `用 recall_memory 从长期对话记忆里捞原话（query 写要回忆的内容；针对某个人 / 公司 / 项目填 who），返回带精确时间戳的依据`,
		},
	}
}

func (recallTool) Serialize() bool { return false }

func (t recallTool) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var p struct {
		Query     string   `json:"query"`
		Who       string   `json:"who"`
		Mode      string   `json:"mode"`
		TimeRange []string `json:"time_range"`
		Limit     int      `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return contract.ErrResult("recall_memory: 参数不是合法 JSON")
	}
	if p.Query == "" && p.Who == "" {
		return contract.ErrResult("recall_memory: 需要 query 或 who 至少给一个")
	}

	var client contract.RetrievalClient
	if t.client != nil {
		client = t.client()
	}
	if client == nil {
		return contract.ErrResult("recall_memory: 长期记忆未启用")
	}

	// --- scope clamp: the security boundary the model CANNOT widen ---
	caller := tc.Caller
	company := caller.Company
	personal := company == ""
	if personal {
		company = contract.RetrievalPersonalCompany
	}
	filters := map[string]any{"company_id": company}
	switch {
	case caller.Room != "":
		// Group: recall the shared room (everyone in it) — members already see all.
		filters["channel_id"] = caller.Room
	case personal:
		// Private DM: the __personal__ partition holds EVERY user's history, so we
		// MUST clamp to the caller's own handles. No identity → fail-closed.
		if len(caller.Handles) == 0 {
			return contract.ErrResult("recall_memory: 无法确定你的身份，拒绝检索（以免读到别人的记忆）")
		}
		filters["user_ids"] = caller.Handles
	default:
		// company scope (agora): worker sees the whole company — no user clamp.
	}
	if len(p.TimeRange) == 2 && (p.TimeRange[0] != "" || p.TimeRange[1] != "") {
		filters["time_range"] = p.TimeRange
	}

	limit := p.Limit
	switch {
	case limit <= 0:
		limit = 10
	case limit > 50:
		limit = 50
	}

	q := contract.RetrievalQuery{Filters: filters, Limit: limit}
	if p.Who != "" {
		mode := p.Mode
		if mode == "" {
			mode = "both"
		}
		q.QueryType = "entity_mentions"
		q.Params = map[string]any{"who_text": p.Who, "mode": mode}
	} else {
		q.QueryType = "semantic"
		q.Params = map[string]any{"query_text": p.Query}
	}

	raw, err := client.Retrieve(ctx, q)
	if err != nil {
		// fail-open: a down leaf must not fail the turn.
		slog.Warn("recall_memory: leaf unavailable", "err", err)
		return contract.OKResult("记忆服务暂不可用，本次回忆跳过（不影响其余回答）。")
	}
	return contract.OKResult(shapeRecall(raw))
}

// --- leaf response shaping (kept local so the tool imports only contract) ---

type recallItem struct {
	When    string `json:"when"`
	Role    string `json:"role"` // user | assistant — speaker name (in groups) rides in content's [Name]: prefix
	Content string `json:"content"`
}

// shapeRecall compacts the leaf's {"context":[…]} response into a terse list for
// the model, dropping scores/embeddings/ids. Falls back to the raw body if the
// shape is unexpected (e.g. an error body, or a time_window object).
func shapeRecall(raw json.RawMessage) string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return string(raw)
	}
	ctxRaw, ok := probe["context"]
	if !ok {
		return string(raw)
	}
	var list []struct {
		Content  string `json:"content"`
		Metadata struct {
			Role      string `json:"role"`
			Timestamp string `json:"timestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(ctxRaw, &list); err != nil {
		return string(raw)
	}
	out := struct {
		Count   int          `json:"count"`
		Results []recallItem `json:"results"`
	}{}
	for _, c := range list {
		out.Results = append(out.Results, recallItem{
			When:    c.Metadata.Timestamp,
			Role:    c.Metadata.Role,
			Content: c.Content,
		})
	}
	out.Count = len(out.Results)
	if out.Count == 0 {
		return "（没有召回到相关记忆。）"
	}
	b, err := json.Marshal(out)
	if err != nil {
		return string(raw)
	}
	return string(b)
}
