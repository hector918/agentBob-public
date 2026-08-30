package core

import (
	"context"
	"encoding/json"
	"errors"

	"agentbob/sidecars/browser/scrub"
)

// 冷记忆检索叶子的写入侧:bob 在 turn 末尾把消息 enqueue 进 outbox,一个
// drainer 再 fire-and-forget 喂给外部检索服务。设计见
// docs/cold-memory-retrieval.md。这里只定 store 契约 + 行类型;HTTP client
// 和 drainer 在 retrieval/ 包,enqueue 调用点在 turn 末尾(gated)。
//
// 用独立接口(不并进 SessionStore)是为了不强迫每个 SessionStore 实现者/测试
// 桩都实现这几个方法 —— 只有 drainer 和 enqueue 调用方按需取 RetrievalOutboxStore。

const (
	// RetrievalPersonalCompany 是 outer-bob(无 agora 公司)消息的 company 哨兵,
	// 对应叶子侧预建的 '__personal__' 分区。
	RetrievalPersonalCompany = "__personal__"
	// RetrievalBotHandle 是 assistant/tool 消息的 source_handle(发话方=bob 自己)。
	RetrievalBotHandle = "__bob__"
	// RetrievalContentCap 是单条消息进 outbox 前的内容上限(字节)。超长机械 blob
	// (大段 tool 结果/粘贴的文件)截到这个长度,避免污染召回 + 撑大语料。见
	// docs/cold-memory-retrieval.md §4.2「内容型工具结果(截断)/丢超长机械 blob」。
	RetrievalContentCap = 8000
)

// RetrievalOutboxCap 是 outbox 的硬行数上限(安全界,非调优旋钮)。叶子持续 down
// 时 enqueue 只进不出会无界堆积(每行带完整 payload),撑爆主库比丢冷记忆更糟
// ——『宁可丢最旧的冷记忆也别撑爆主库』。enqueue 在同 tx 内把超出上限的最旧行删掉
// (FIFO 丢头)。登记在 docs/accumulation-inventory.md。是 var 而非 const 仅为让
// store 层的回归测试能调小后验证裁剪 SQL —— 生产恒为这个值,不暴露成配置。
var RetrievalOutboxCap = 50000

// ErrRetrievalPermanent marks a leaf rejection that retrying will never fix
// (a permanent 4xx such as 400/422 — malformed/over-long/illegal-encoding
// content), as opposed to a transient outage (5xx / 408 / 429 / transport).
// The drainer drops (dead-letters) a head batch that hits this so one poison
// batch can't block the FIFO outbox forever. Transient errors leave rows for
// the next tick. The HTTP client wraps it; callers test with errors.Is.
var ErrRetrievalPermanent = errors.New("retrieval: leaf permanently rejected batch")

// RetrievalIngestContent 套用 §4.2 的 ingest 过滤到一条消息的 (role, content):
// 返回 (cleaned, ok)。ok=false 表示该消息不进 outbox(系统/控制事件)。ok=true
// 时 cleaned 已过 scrub.Scrub(密钥绝不出境)并按 RetrievalContentCap 截断。
//
// 放 core 而非各 store impl,是为了 sqlite/postgres 两侧的过滤策略不漂移。
func RetrievalIngestContent(role, content string) (string, bool) {
	switch role {
	case "user", "assistant", "tool":
		// 收:user/assistant 文本 + 内容型 tool 结果(截断)。
	default:
		// 丢:system 压缩摘要 / 控制事件等。
		return "", false
	}
	content = scrub.Scrub(content)
	// Rune-aware: a byte slice could cut a multi-byte rune at the cap and
	// the invalid UTF-8 tail would make pg reject the whole enqueue tx,
	// wedging the session's outbox cursor forever.
	content = TruncateRunes(content, RetrievalContentCap)
	return content, true
}

// RetrievalEnqueueMeta 是一个 turn 的上下文,套用到该 turn 新写入的所有消息上。
// 来自 actx(InboxID/CompanyID)+ 触发该 turn 的 sender。
type RetrievalEnqueueMeta struct {
	CompanyID  string // "" → 存储层映射为 RetrievalPersonalCompany
	UserHandle string // user-role 消息的 source_handle(不可变);assistant/tool → RetrievalBotHandle
	InboxID    string // agora inbox;"" = 非 agora
	WorkerID   string // 当时接手的 agora worker;"" = generic bob
}

// RetrievalOutboxItem 是 outbox 里一行(drainer pop 出来喂给叶子)。MsgID =
// bob 的 messages.id,stringify 后作叶子的 message_id。标签字段 Phase 1 为空
// (Phase 1.5 的 tagger 回填)。
type RetrievalOutboxItem struct {
	ID           int64 // outbox 行 id(用于喂成功后 Delete)
	MsgID        int64 // bob messages.id
	CompanyID    string
	SessionID    string
	SourceHandle string
	InboxID      string // 空 = NULL
	WorkerID     string // 空 = NULL
	Role         string
	Content      string
	Ts           float64 // messages.timestamp(bob 落库时刻)
	Lang         string
	Sentiment    string
	IsCommitment *bool
	Intent       string
}

// RetrievalOutboxStore 是冷记忆 feed 的存储契约,sqlite + postgres 都实现。
type RetrievalOutboxStore interface {
	// EnqueueMessagesForRetrieval 在一个事务里:取本 session 游标之后的新消息
	// (id > hwm,覆盖 agora + personal)→ 按 role 定 source_handle → 插 outbox
	// → 推进游标。turn 末尾调,gated on retrieval.enabled。返回入队条数。
	EnqueueMessagesForRetrieval(ctx context.Context, sessionID string, meta RetrievalEnqueueMeta) (int, error)
	// PopRetrievalOutbox 读最旧的 limit 条(不删 —— 喂成功后由 DeleteRetrievalOutbox
	// 删;失败留队下轮重试)。limit<=0 → 100。
	PopRetrievalOutbox(ctx context.Context, limit int) ([]RetrievalOutboxItem, error)
	// DeleteRetrievalOutbox 按 outbox 行 id 删除(喂成功后调)。空 ids → no-op。
	DeleteRetrievalOutbox(ctx context.Context, ids []int64) error
	// SeedRetrievalCursor 把 session 的游标推进到当前 MAX(messages.id)(无消息
	// 则 no-op)。给「已喂过的内容换了新行 id」的迁移路径用——压缩切代把 recent
	// tail re-append 进新 sid(archive-restore 在 store 内部自己做同一件事):
	// 新 sid 没有游标行(hwm=0),不 seed 的话下一次 enqueue 会把整段 carry-over
	// 重喂给叶子(叶子按 message id 去重,id 变了 → 去重必 miss)。
	SeedRetrievalCursor(ctx context.Context, sessionID string) error
}

// RetrievalQuery 是 recall 工具发给检索叶子 /retrieve 的查询。Params/Filters
// 直接序列化进请求体;scope filters(company/account)由调用方夹定后放进 Filters。
type RetrievalQuery struct {
	QueryType string         // entity_mentions | time_window | semantic
	Params    map[string]any // query_type 相关参数
	Filters   map[string]any // 通用过滤(含夹定的 company_id / user_ids)
	Limit     int
}

// RetrievalClient 是 bob 调外部检索叶子(chat-retrieval-system)的 HTTP 契约。
// 两条路:Ingest(drainer 喂消息)/ Retrieve(recall 工具召回)。两者都 fail-open
// ——返错由调用方处理(drainer 留行重试;工具静默跳过)。impl 在 retrieval/ 包。
type RetrievalClient interface {
	// Ingest POST /messages/batch。叶子幂等(ON CONFLICT DO NOTHING),可安全重试。
	Ingest(ctx context.Context, items []RetrievalOutboxItem) error
	// Retrieve POST /retrieve,返回叶子的原始 JSON 响应(调用方按需解析)。
	Retrieve(ctx context.Context, q RetrievalQuery) (json.RawMessage, error)
}
