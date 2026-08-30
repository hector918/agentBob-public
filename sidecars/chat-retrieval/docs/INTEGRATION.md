# agentbob 集成调整说明 (2026-06)

本文记录为接入 agentbob (作为它的"冷记忆叶子")对原草稿做的设计改动,
是集成契约的事实源。`README` / `FEATURES.md` / `USAGE.md` / `SETUP.md` /
`ARCHITECTURE.md` 已对齐这些改动(离线话题/话题演化已移除,`TOPIC_WORKER.md` 已删)。

## ★ 部署前必验

**一条命令搞定**:打 **`GET /selftest`**(或 bob 侧 `bob debug retrieval`)—— 一次请求把下面
1–3 全验了,逐项 ok/detail,有 fail 返 503。**接口符合性已做进端点,请求测试时顺便就验了。**

各项含义(/selftest 自动覆盖):

1. **embedder 真返目标维度**(★最易让首跑全挂):`dimensions` 是请求里"要求"的,服务端
   **不一定认**(忽略就返原生 2560 维)。/selftest 探服务端**原生**维度并判定:==期望=认;
   >期望=被忽略、靠客户端截断(`clients/embedder.py` 已兜底,MRL 模型 OK、非 MRL 会错);
   <期望=配错模型(fail)。
2. **pgvector ≥ 0.8**:低于 0.8 没有 `hnsw.iterative_scan`(db.py 容忍降级,但召回差)。
3. **reranker 可达**(若 `RERANKER_ENABLED=true`)。
4. **网络隔离**(/selftest 不查,自己确认):本服务**不鉴权**,`/selftest`、`/messages/{id}`、
   `/retrieve` 等只对 bob 应用机内网开放(跨公司隔离靠 bob 夹 filter + company 分区)。

## 定位

bob 通过 HTTP 把它当一个**叶子**调用:服务不在,bob 照常运行(fail-open)。
本系统只做检索,不调生成式 LLM;话题/聚合叙事由 bob 拿检索结果自己的 LLM 现场综述。

写入侧由 bob 在 sweep(蒸馏)点驱动:原始消息逐条经 outbox 灌入 `/messages/batch`;
per-message 派生标签(sentiment / is_commitment / intent)在 bob 那次批量蒸馏调用里
一并抽出,随消息一起送来。存量不 backfill,语料从空开始向前积累。

## 改了什么 (vs 原草稿)

| # | 改动 | 为什么 |
|---|---|---|
| 1 | `messages` 按 **company_id LIST 分区**(预建 `__personal__`,其余按需建) | 解决"中等选择性元数据过滤 + 全局 HNSW 欠召回";跨公司隔离变成结构性保证。公司数预期 ≤ ~100,分区数安全 |
| 2 | 新增列 `company_id`(分区键)、`worker_id`、`received_at`、`sentiment`、`is_commitment`、`intent` | 分公司/分 worker 切分;权威收信时间;sweep 一并抽的派生标签 |
| 3 | `user_id` 语义改为**不可变 source_handle** | account 会被认领/合并,存 account id 会让同一人历史裂开。语料只存 handle,bob 查询时把 account 展开成 `user_ids` handle 列表 |
| 4 | 双时间戳:`timestamp`=source 声明时间;`received_at`=bob pg 校准时钟 | "时间作事实证明";email 等 source 的声明时间可伪造,以 received_at 兜底 |
| 5 | PK = `(company_id, message_id)`;ON CONFLICT 同步改 | 分区表主键必须含分区键 |
| 6 | HNSW 索引**按分区建**(不建在父表);普通索引建在父表自动传播 | 规避部分版本"父表 HNSW 分区索引"的不确定性;分区裁剪后命中单分区 HNSW |
| 7 | 连接初始化 `SET hnsw.iterative_scan` / `ef_search`(pgvector 0.8+,缺则容忍降级) | 公司内子过滤(account/worker/时间)仍可能中等选择性,iterative scan 兜召回 |
| 8 | **移除离线话题**:删 `topic_snapshots` / `message_topics` 表、`topic_evolution` query_type、`topic_worker/` 容器;`time_window` 去掉话题段(只剩消息切片 + 聚合) | bob 有自己的 LLM,客户/时段主题用"检索 + 现场综述"(用时抽,小 N)即可,不养又重又脆的 BERTopic 流水线 |
| 9 | append-only:不提供 delete/edit | 撤回/编辑不回退;进库前由 bob `scrub`,所以不会有密钥落进不可删的语料 |

## 当前 query_type

- `entity_mentions` — 对象相关(author / literal / semantic 三路 + rerank)
- `time_window` — 时段消息切片 + 聚合统计(无话题段)
- `semantic` — 通用语义兜底

## 测试通路探针(bob 碰不到本系统 DB,靠这几个验证端到端)

- `GET /selftest` — **接口符合性自检**:一次请求跑 db / pgvector≥0.8 / **embedder 维度** /
  reranker,逐项 ok+detail,有 fail 返 **503**。只读 + 探针字符串,不写真数据。


- `GET /health` — 探活,**带 DB ping**(`SELECT 1`,廉价 —— docker healthcheck 每 30s 打):
  `{"status":"ok","db":"ok"}`;DB 不通返 **503** `{"status":"degraded",...}`,让调用方区分"进程在但库挂了"。
- `GET /messages/{message_id}?company_id=…` — 按 (company_id, id) 取回单条,验证某条 ingest
  真落库(往返)。**company_id 必填**:命中单分区 + 限单公司,避免按 id 跨公司读;不存在 → **404**。

bob 侧对应 `bob debug retrieval`:health → 幂等探针 ingest(固定 id 在 `__probe__`
公司,不累积)→ get-back → 可选 retrieve。

## bob 侧要保证的(写入契约)

每条消息 POST 时必带:`message_id`、`company_id`(无公司用 `__personal__`)、
`session_id`、`user_id`(= source_handle)、`timestamp`(source 时间)、`role`、
`content`(已 scrub,非空)。
建议带:`received_at`、`channel_id`(inbox)、`worker_id`、`sentiment`/`is_commitment`/`intent`、`lang`。

**ingest 过滤**(bob 侧):只送 user/assistant 文本 + 内容型工具结果(截断)+ OCR 文本;
跳过工具调用管线、系统/控制事件。

## 鉴权(可选)

`API_KEY` env 设置后,`/messages*` 与 `/retrieve` 要求请求头 `X-API-Key` 一致
(401 拒);`/health`、`/selftest` 始终开放。bob 侧 = `retrieval.api_key`。
两边都空 = 不鉴权(网络层信任)。

`/retrieve` 的 `filters` **必须**带 `company_id` 或 `company_ids`(无公司数据用
`__personal__`),否则 400 —— 叶子侧 fail-closed,防调用方丢 filters 时静默跨
公司返回。同公司内全量搜索 = 只给 company_id。

## scope 安全

服务故意不鉴权,跨公司不泄露**全靠 bob 每次下 `company_id` filter**。bob 的 recall
工具必须 **fail-closed**:取不到当前公司 scope 时拒绝查询,不退化成无 filter 全库查。
分区在结构上是第二道防线。
