# 使用文档

完整 API 文档在 `http://localhost:8000/docs` (Swagger)。本文重点讲怎么用、参数怎么传。
集成契约见 [`INTEGRATION.md`](INTEGRATION.md)。

## API 总览

| Method | Path | 用途 |
|---|---|---|
| POST | `/messages` | 写入单条消息(同步) |
| POST | `/messages/batch` | 批量写入(推荐, 减少 embedding 调用) |
| GET | `/messages/{message_id}?company_id=…` | 取回单条(验证落库往返) |
| POST | `/retrieve` | 统一检索入口 |
| GET | `/health` | 探活(带 DB ping) |
| GET | `/selftest` | 接口符合性自检(db / pgvector≥0.8 / embedder 维度 / reranker) |

> 设置了 `API_KEY` 时,`/messages*` 与 `/retrieve` 必须带请求头 `X-API-Key`;
> `/health`、`/selftest` 始终开放。

## 写入消息

写入(单条和批量)都是**同步**的:`200` = embed + 落库**全部完成**。失败返 `500`,
调用方留着数据下轮重推(幂等,重复 `message_id` 被 `ON CONFLICT DO NOTHING` 忽略)。
**不返 202**:202 异步会让调用方把"已收下"误当"已持久化",后台失败就永久丢数据。

### 单条

```bash
curl -X POST http://localhost:8000/messages \
  -H "Content-Type: application/json" \
  -d '{
    "message_id": "msg_001",
    "company_id": "__personal__",
    "session_id": "sess_alice_20241115",
    "user_id": "alice",
    "timestamp": "2024-11-15T10:30:00Z",
    "role": "user",
    "content": "凤凰项目最近怎么样, Hans 给我推荐过什么书来着",
    "lang": "zh",
    "channel_id": "ch_phoenix",
    "user_attributes": {"department": "engineering", "title": "senior_engineer"}
  }'

# 返回: {"status":"stored","message_id":"msg_001"}  (200 = 已落库)
```

### 批量(推荐)

```bash
curl -X POST http://localhost:8000/messages/batch \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"message_id":"m1", "company_id":"__personal__", "session_id":"s1", "user_id":"u1", "timestamp":"2024-11-15T10:00:00Z", "role":"user", "content":"..."},
      {"message_id":"m2", "company_id":"__personal__", "session_id":"s1", "user_id":"u1", "timestamp":"2024-11-15T10:01:00Z", "role":"assistant", "content":"..."}
    ]
  }'
```

最多 200 条一批。批量调用让 embedding 服务一次算多条, 比逐条快得多。

### 字段说明

| 字段 | 必填 | 说明 |
|---|---|---|
| `message_id` | ✅ | 全局唯一 ID, 业务侧给(重复会被忽略) |
| `company_id` | ✅ | 公司 scope / 分区键, 无公司用 `__personal__` |
| `session_id` | ✅ | 会话 ID |
| `user_id` | ✅ | 不可变 source_handle(account 由调用方查询时解析展开) |
| `timestamp` | ✅ | source 声明时间, UTC ISO 8601(可被客户端伪造) |
| `role` | ✅ | `user` / `assistant` / `system` |
| `content` | ✅ | 消息文本(已 scrub, 非空; 空串 422) |
| `received_at` | ❌ | 调用方权威收到时间(pg 校准时钟) |
| `lang` | ❌ | 语言代码(zh/en/de/...), 不填则存 NULL, 无推测 |
| `channel_id` | ❌ | agora inbox(会话室) |
| `worker_id` | ❌ | 接手的 agora worker; 空 = generic bob / 非 agora |
| `thread_id` | ❌ | 所在 thread ID |
| `sentiment` / `is_commitment` / `intent` | ❌ | per-message 派生标签(sweep 蒸馏时一并抽) |
| `user_attributes` | ❌ | JSONB, 比如 `{"department":"eng"}` |
| `session_attributes` | ❌ | JSONB, session 级 metadata |
| `extra` | ❌ | 任意自定义字段 |

---

## 检索: 三大功能

> `filters` **必须**带 `company_id` 或 `company_ids`(无公司数据用 `__personal__`),
> 否则 400。下面的示例为省篇幅多数省略了它,实际调用要加。
> 设置了 `API_KEY` 时还要带请求头 `X-API-Key`。

### 功能 1: 查"对象"相关 (entity_mentions)

**用途**: 查某个人/公司/项目/产品 相关的所有信息

**两种查法**:
- `who_id`: 内部 ID / 不可变 handle, 严格匹配
- `who_text`: 字面名字, 字面 + 语义模糊匹配(跨语言、变体)

**三种 mode**: `author`(只查这人发出的) / `mention`(只查提到这名字的) / `both`(默认)。
`author` 需 `who_id`,`mention` 需 `who_text`,否则 400。

```bash
# 例 1: 查 alice 自己发的所有消息
curl -X POST http://localhost:8000/retrieve -H "Content-Type: application/json" -d '{
  "query_type": "entity_mentions",
  "params": {"who_id": "alice", "mode": "author"},
  "filters": {"company_id": "__personal__"},
  "limit": 20
}'

# 例 2: 查所有提到 "凤凰项目" 的消息(跨语言: 也会命中 "Phoenix Project")
curl -X POST http://localhost:8000/retrieve -H "Content-Type: application/json" -d '{
  "query_type": "entity_mentions",
  "params": {"who_text": "凤凰项目", "mode": "mention"},
  "filters": {"company_id": "__personal__", "time_range": ["2024-11-01", "2024-12-01"]},
  "limit": 30
}'

# 例 3: 查 Hans 自己说的 + 别人提到 Hans 的所有消息
curl -X POST http://localhost:8000/retrieve -H "Content-Type: application/json" -d '{
  "query_type": "entity_mentions",
  "params": {"who_id": "hans", "who_text": "Hans", "mode": "both"},
  "filters": {"company_id": "__personal__"},
  "limit": 30
}'
```

**返回结构**:

```json
{
  "query_type": "entity_mentions",
  "context": [
    {
      "type": "message",
      "content": "Hans 推荐了 Designing Data-Intensive Applications",
      "metadata": {
        "message_id": "msg_xxx",
        "company_id": "__personal__",
        "session_id": "...",
        "user_id": "alice",
        "worker_id": null,
        "timestamp": "2024-11-12T15:20:00Z",
        "received_at": "2024-11-12T15:20:01Z",
        "role": "user",
        "channel_id": "ch_phoenix",
        "lang": "zh"
      },
      "match": {
        "type": "literal+semantic",
        "final_score": 0.92,
        "literal_hit": 1.0,
        "semantic_score": 0.87,
        "author_hit": null
      }
    }
  ],
  "metadata": {
    "result_count": 20,
    "candidates_pooled": 47,
    "who_id": "hans",
    "who_text": "Hans",
    "mode": "both"
  }
}
```

`match.type`: `author`(是这人发的)/ `literal`(字面命中)/ `semantic`(跨语言/变体)/
组合(`author+literal`、`literal+semantic` 等)。

---

### 功能 2: 查"某时段发生了什么" (time_window)

**用途**: 时段总览, 适合"这周聊了什么"/"上个月的活动"

**两种模式**:
- **不传 `query_text`**: 拿该时段**最近 N 条代表消息 + 聚合统计**
- **传 `query_text`**: 拿该时段里**关于这个查询**的相关消息 + 聚合统计

`time_range` 必填(放 `filters` 或 `params` 都行)。

```bash
# 例 1: 11 月份这个频道发生了什么(无 query)
curl -X POST http://localhost:8000/retrieve -H "Content-Type: application/json" -d '{
  "query_type": "time_window",
  "params": {},
  "filters": {"company_id": "__personal__", "channel_id": "ch_phoenix", "time_range": ["2024-11-01", "2024-12-01"]},
  "limit": 20
}'

# 例 2: 11 月份这个频道关于 "API 设计" 的讨论
curl -X POST http://localhost:8000/retrieve -H "Content-Type: application/json" -d '{
  "query_type": "time_window",
  "params": {"query_text": "API 设计"},
  "filters": {"company_id": "__personal__", "channel_id": "ch_phoenix", "time_range": ["2024-11-01", "2024-12-01"]},
  "limit": 15
}'
```

**返回结构**:

```json
{
  "query_type": "time_window",
  "context": {
    "messages": [
      // 传了 query_text: 召回的相关消息(带 scores)
      // 否则: 该时段最近的 N 条消息
    ]
  },
  "aggregations": {
    "total_messages": 234,
    "unique_users": 12,
    "unique_sessions": 45,
    "unique_channels": 3,
    "by_day": [{"day": "2024-11-01", "count": 8}, {"day": "2024-11-02", "count": 12}],
    "by_user": [{"user_id": "alice", "count": 78}]
  },
  "metadata": {"time_range": {"start": "2024-11-01", "end": "2024-12-01"}}
}
```

> 话题分布**不在返回里** —— 由调用方拿 `messages` + `aggregations` 让自己的 LLM 现场综述。

---

### 功能 3: 通用语义检索 (semantic)

兜底用。简单的"找相关消息":

```bash
curl -X POST http://localhost:8000/retrieve -H "Content-Type: application/json" -d '{
  "query_type": "semantic",
  "params": {"query_text": "怎么处理 API 接口的版本兼容性"},
  "filters": {"company_id": "__personal__", "channel_id": "ch_phoenix"},
  "limit": 10
}'
```

返回 `context` 是消息数组,每条带 `scores: {vector, final}`。

---

## 通用 filters

所有 query_type 都可以用 `filters`(`company_id` 必填):

```json
{
  "filters": {
    "company_id": "__personal__",
    "user_id": "alice",
    "user_ids": ["alice", "bob"],
    "worker_id": "w_1",
    "channel_id": "ch_phoenix",
    "channel_ids": ["ch_a", "ch_b"],
    "session_id": "sess_001",
    "lang": "zh",
    "role": "user",
    "sentiment": "pos",
    "is_commitment": true,
    "intent": "ask",
    "time_range": ["2024-11-01", "2024-12-01"],
    "user_attributes": {"department": "engineering"}
  }
}
```

---

## 把结果喂给 LLM

返回的 `context` 设计成可以直接拼 prompt。示例:

```python
import httpx, json

result = await client.post("http://localhost:8000/retrieve", json={
    "query_type": "entity_mentions",
    "params": {"who_text": user_query},
    "filters": {"company_id": company_id},
    "limit": 10,
})
data = result.json()

prompt = f"""
基于以下消息回答用户问题。每条消息标注了来源(message_id 和 timestamp)。

用户问题: {user_query}

相关消息:
{json.dumps(data['context'], ensure_ascii=False, indent=2)}

要求:
- 用消息里的实际信息回答, 不要编造
- 引用消息时附 timestamp 让用户知道时效
- 多语言混杂时, 用用户提问的语言回答
"""

answer = await your_llm.chat(prompt)
```

---

## 错误处理

| 状态码 | 含义 | 处理 |
|---|---|---|
| 200 | 成功(写入端点 = 已同步落库) | - |
| 400 | 查询形状/缺 company_id | 看 detail 改请求 |
| 401 | X-API-Key 缺失/不对 | 对一下两边的 key |
| 404 | `GET /messages/{id}` 该公司下不存在 | - |
| 422 | 请求体校验失败(如 content 空串) | 改请求, 重试同一批没用 |
| 500 | 内部错误(DB 不通 / embedder 挂了) | 重试(写入端点据此留 outbox 重推) |
| 503 | `/health` / `/selftest` 不健康 | 看 detail 排查 db / 模型 |

---

## 性能参考

| 操作 | 典型延迟(单条) |
|---|---|
| `POST /messages`(同步) | embed + 落库, 取决于 embedder(CPU ~2s/条) |
| `POST /messages/batch` | 一次 embed 多条, 单条均摊远低于逐条 |
| `POST /retrieve` `semantic` | 100-300ms(含 embedding + rerank) |
| `POST /retrieve` `entity_mentions`(both) | 200-500ms |
| `POST /retrieve` `time_window` | 50-300ms(含聚合统计) |

**优化建议**:
- 写入用 batch(50-200 条一批)
- 查询时尽量加 `filters`(尤其 `time_range` 和 `user_id`)
- 关闭 reranker(`RERANKER_ENABLED=false`)可省 50-150ms, 代价是精度降 10-20%
