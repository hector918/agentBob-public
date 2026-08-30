# 功能详解

本文讲三大功能的**实现原理**, 让你知道结果是怎么来的、什么时候会失效、怎么优化。
集成契约见 [`INTEGRATION.md`](INTEGRATION.md)。

> 离线话题(BERTopic / 话题演化)已在 agentbob 集成中移除。话题/时段叙事由
> 调用方(bob)拿 `time_window` / `entity_mentions` 的结果做 LLM 现场综述。

## 功能 1: entity_mentions (对象相关)

### 它在做什么

回答 **"X 这个人/公司/项目相关的所有信息"** 这类问题。

X 可以是:
- 一个内部用户 ID / 不可变 handle (`who_id=alice`)
- 一个字面名字 (`who_text="凤凰项目"`)
- 两者都有 (`who_id=alice, who_text="Alice Wang"`)

### 工作原理

按 `mode` 启用最多三种召回, 然后合并去重 + rerank:

```
输入: who_id="hans", who_text="Hans", mode="both"
   │
   ├── 召回 A (author, 需 who_id): SELECT * WHERE user_id = 'hans'
   │   (Hans 自己发的消息, 按 timestamp DESC)
   │
   ├── 召回 B (literal, 需 who_text): SELECT * WHERE content ILIKE '%Hans%'
   │   (内容里字面包含 "Hans" 的消息)
   │   * 用 pg_trgm GIN 索引加速 *
   │
   └── 召回 C (semantic, 需 who_text): SELECT * ORDER BY embedding <=> embed("Hans")
       (跨语言/变体语义召回, e.g. "汉斯"/"老 H"; 相似度 < 0.3 丢弃)
       * 用 pgvector HNSW 索引 *
   ↓
   合并去重 (按 message_id), 每条标注 match_type (author / literal / semantic / 组合)
   ↓
   Reranker 精排 (query=who_text)  —— reranker 关闭/挂了则走 _fallback_sort 加权合分
   ↓
   返回 top-N
```

`mode` = `author` | `mention` | `both`(默认)。`mode=author` 必须给 `who_id`,
`mode=mention` 必须给 `who_text`,否则**响亮 400**(不静默返空,免得调用方分不清
"没数据"和"查询本身是 no-op")。

### 为什么这样设计

| 单用一种方式 | 缺陷 |
|---|---|
| 只字面 ILIKE | 跨语言不行 (中文查不到英文消息) |
| 只向量召回 | 召回不精, 会拉很多"语义近但不是同人"的消息 |
| 只 user_id | 找不到"别人提到 Hans"的 |

**多路并行** = 各自补充。`match.type` 字段告诉你这条结果是哪路命中的, 可以用来做信任度判断。

### 跨语言效果举例

输入 `who_text="凤凰项目"`, 同一个项目的消息:

- 字面命中: `"凤凰项目本周延期"` ✅ (literal)
- 向量召回: `"Phoenix project delayed"` ✅ (semantic, 跨语言)
- 向量召回: `"凤凰 项目"` (带空格) ✅ (semantic, 变体)
- 字面命中: `"PRJ-Phoenix 状态更新"` ✅ (literal, 因为含 Phoenix)

### 限制

- **没有实体消歧**: "Hans" 可能命中德国同事 Hans 和荷兰同事 Hans-jürgen (rerank 能稍微补救)
- **依赖向量召回的语言能力**: Qwen3-Embedding 对 100+ 主流语言效果好, 极冷门语言可能不行
- **候选越多 rerank 越慢**: 大量假阳性时延迟上升

---

## 功能 2: time_window (时段总览)

### 它在做什么

回答 **"X 时段发生了什么"** 这类问题:
- "这周 phoenix 频道聊了什么"
- "11 月公司发生了什么"
- "上季度关于 API 的讨论"

`time_range` 必填(放 `filters` 或 `params` 里都行)。形状不对响亮 400。

### 两种工作模式

**模式 A: 无 query_text(拿时段总览)**

```
SELECT ... WHERE <time_range + filters>
  ORDER BY timestamp DESC
  LIMIT <limit>
→ 返回该时段最近 N 条代表消息
```

**模式 B: 有 query_text(拿相关切片)**

```
embedding(query_text) → pgvector 检索
  WHERE embedding IS NOT NULL AND <time_range + filters>
  ORDER BY 距离 LIMIT default_candidates
→ Rerank → 返回 top-N
```

### 两种模式都返回聚合统计

```
total_messages   时段内消息总量
unique_users     独立用户数 (count distinct user_id)
unique_sessions  独立会话数
unique_channels  独立频道数
by_day           按天分布 (date_trunc('day', timestamp))
by_user          最活跃用户 top 10
```

聚合是纯 SQL,不依赖任何离线流水线 —— 随查随算。

### 返回数据怎么用

把 `context.messages` + `aggregations` 喂给调用方自己的 LLM 写时段报告:

```python
prompt = f"""
请基于以下数据写一份"{channel_id} {period} 活动总结":

代表消息:
{json.dumps(data['context']['messages'], ensure_ascii=False)}

整体统计:
{json.dumps(data['aggregations'], ensure_ascii=False)}

要求: 提炼主要话题, 引用代表消息附 timestamp, 提及主要参与者。
"""
```

"主要话题"由调用方的 LLM 当场从消息里提炼(小 N、用时抽),本系统不养离线话题模型。

---

## 功能 3: semantic (通用语义检索)

### 它在做什么

兜底用。简单的"找相关消息":给 `query_text`,做向量召回 + rerank。

```
embedding(query_text, is_query=True)   # 加 Qwen3 instruction 前缀
  → SELECT ... ORDER BY embedding <=> qvec LIMIT candidates
  → Rerank (query=query_text) → top-N
```

`params.candidates` 控制向量召回多少候选(默认 `default_candidates`),`limit`
控制 rerank 后返回多少。reranker 关闭/挂了则退回向量分排序(打 WARN 日志)。

---

## 通用: filters

所有 query_type 都共用一组 filters。**`company_id`(或 `company_ids`)必填** ——
缺了直接 400(fail-closed,防跨公司分区泄露)。其余 filters 强烈建议尽量加:
减小候选集 → 向量检索更快 → rerank 更准。

| filter | 类型 | 索引情况 | 推荐 |
|---|---|---|---|
| `company_id` / `company_ids` | str / list | ✅ LIST 分区裁剪 | **必加** |
| `user_id` / `user_ids` | str / list (= source_handle) | ✅ B-tree | 强 |
| `worker_id` / `worker_ids` | str / list (agora worker) | ✅ B-tree | 中 |
| `channel_id` / `channel_ids` | str / list (agora inbox) | ✅ B-tree | 强 |
| `session_id` | str | ✅ B-tree | 强 |
| `time_range` | [start, end] | ✅ B-tree | **强烈建议** |
| `sentiment` / `is_commitment` / `intent` | 派生标签 | ❌ | 中 |
| `lang` / `role` | str | ❌ | 弱 |
| `user_attributes` | dict | ✅ GIN (JSONB @>) | 中 |

> `*_ids` 列表为**空** ≠ 缺省:空列表语义是"一个都不匹配"(fail-closed,生成
> `FALSE` 子句),不会反转成"匹配全部"。

---

## 系统行为速查

| 场景 | 行为 |
|---|---|
| 模型服务挂了(embedder) | 写入返 **500**(同步,不丢数据),调用方留 outbox 重推;查询若需 embed 也失败 |
| Reranker 挂了 | 查询正常, 自动退回向量分排序(打 WARN) |
| 同 message_id 重复写 | `ON CONFLICT (company_id, message_id) DO NOTHING` 静默忽略(幂等) |
| filters 缺 company_id | 400(fail-closed) |
| 查询形状错误(mode/time_range 不对) | 400(让调用方修查询,而非 500 触发其 fail-open) |
| 用户问的语言跟数据语言不同 | 向量召回自动跨语言, ILIKE 不能跨语言 |
| 极冷门关键词查不到 | 字面匹配可能 0 召回, 但向量召回会兜底 |

---

## 后续可扩展功能

如果某个 query_type 不够用, 可以加(每个 = 在 `app/retrieval/` 加一个文件 + 注册到
`api.py` 的 `HANDLERS` dict):

| 想要的 | 该加什么 |
|---|---|
| 跨 user/部门对比 | 新 query_type: `cross_segment` |
| 用户兴趣画像 | 新 query_type: `user_profile` |
| 实时热点 (最近 24h 增长最快的) | 新 query_type: `trending` |
| 找"分歧"消息 (同话题观点对立) | 新 query_type: `controversies`, 用 embedding 距离 |
