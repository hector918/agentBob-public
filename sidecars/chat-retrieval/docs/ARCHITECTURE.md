# 架构说明

集成契约见 [`INTEGRATION.md`](INTEGRATION.md)。

## 设计哲学

1. **检索和生成解耦**: 本系统只做检索, 不调 LLM, 返回结构化数据
2. **模型独立部署**: 主体应用不绑定 GPU, 通过 HTTP 调外部模型
3. **公司分区即隔离**: `messages` 按 `company_id` LIST 分区, 跨公司隔离是结构性保证
4. **多语言原生**: 所有组件支持 100+ 语言, 不靠翻译
5. **写入同步、幂等**: 2xx = 已落库; 失败返 5xx 让调用方留 outbox 重推

## 组件拓扑

```
                     ┌──────────────────────┐
                     │         bob          │
                     └──────┬───────────────┘
                            │ HTTP
                            ▼
┌───────────────────────────────────────────────────────────┐
│                       主体 (应用机)                          │
│  ┌─────────────────────────────────────────────────────┐  │
│  │  FastAPI App                                          │  │
│  │    POST /messages, /messages/batch -> ingestion       │  │
│  │    POST /retrieve       -> retrieval router           │  │
│  │      ├── entity_mentions handler                      │  │
│  │      ├── time_window handler                          │  │
│  │      └── semantic handler                             │  │
│  │    GET /health, /selftest -> meta                     │  │
│  └────┬──────────────────────────────┬──────────────────┘  │
│       │                              │                      │
│       ▼                              ▼                      │
│  ┌────────────┐               ┌──────────────────┐         │
│  │ PostgreSQL │               │  HTTP clients     │         │
│  │ + pgvector │               │  - embedder       │         │
│  │ (按公司分区)│               │  - reranker       │         │
│  └────────────┘               └────────┬─────────┘         │
└──────────────────────────────────────────┼────────────────┘
                                            │ HTTP
                                            ▼
                              ┌──────────────────────────────┐
                              │   模型服务 (gpu-server)        │
                              │  ┌────────────────────────┐  │
                              │  │ llama.cpp: Embedder    │  │
                              │  │ Qwen3-Embedding-0.6B   │  │
                              │  │ Q8_0 (--embeddings)    │  │
                              │  └────────────────────────┘  │
                              │  ┌────────────────────────┐  │
                              │  │ llama.cpp: Reranker    │  │
                              │  │ Qwen3-Reranker-0.6B    │  │
                              │  │ Q8_0 (--reranking)     │  │
                              │  └────────────────────────┘  │
                              └──────────────────────────────┘
```

## 数据流

### 写入流(同步)

```
bob
   │ POST /messages 或 /messages/batch
   ▼
FastAPI ingestion handler
   │  1. 批量 embed (调外部 embedder API)
   │  2. ensure 公司分区 (按需建表 + HNSW 索引, advisory lock 串行化)
   │  3. 事务内批量 INSERT (ON CONFLICT DO NOTHING)
   │
   └── 全部成功 → 200 {"status":"stored"}
       任一失败  → 500 (调用方留 outbox 下轮重推; 幂等)
```

**为什么不用 202 异步**: 调用方(bob)的 drainer 把 2xx 当"已持久化"→ 删自己的
outbox 行;若后台处理在 202 之后失败,数据永久丢且不重试。同步 + 失败返 500 让
bob 留着行下轮重推(叶子幂等,重推安全)。

### 查询流(同步)

```
bob
   │ POST /retrieve {query_type, params, filters, limit}
   ▼
FastAPI retrieval router  (校验 filters.company_id 必填, fail-closed)
   │ 按 query_type 路由到 handler
   ▼
对应的 handler (e.g. entity_mentions)
   ├─ 调 embedder.embed(query) 算查询向量 (在拿池连接之前, 避免抽干连接池)
   ├─ 查 PostgreSQL (向量 / 字面 ILIKE / metadata 过滤; 分区裁剪命中单公司)
   ├─ 调 reranker 精排 (连接归还之后做; 挂了退回向量分 + WARN)
   └─ 拼结构化 context 返回
   ▼
bob 收到, 拼 prompt 给自己的 LLM
```

**总延迟**: 100-500ms, 取决于 query_type 和数据量。

> 离线话题流水线(BERTopic / topic worker)已移除 —— 话题/聚合叙事改由调用方
> "检索 + LLM 现场综述"完成。

## 关键技术选择

### 为什么 PostgreSQL + pgvector 而不是专用向量库

| | pgvector | Qdrant / Milvus / Weaviate |
|---|---|---|
| 性能 (1000 万级以下) | 持平 | 持平 |
| 性能 (1 亿级以上) | 弱 | 强 |
| metadata 过滤 | ✅ 强 (SQL) | 部分支持 |
| 事务 / ACID | ✅ | 弱 |
| 运维复杂度 | 极低 (1 个 Postgres) | 多一个组件 |
| 适合规模 | 单 user / 小项目, 千万级 | 亿级以上 |

中等规模 + 多用户用 pgvector 完胜。规模上去再换 Qdrant, 应用层改动很小
(`clients/db.py` 抽象了)。

### 为什么按 company_id LIST 分区

- 解决"中等选择性元数据过滤 + 全局 HNSW 欠召回":带 `company_id` filter 的查询经
  分区裁剪只命中**单个公司分区**的 HNSW,召回正确。
- 跨公司隔离成为**结构性保证**(一次查询物理上只进自己公司的分区)。
- HNSW 索引按分区单独建(不建在父表);新公司分区由 ingest 路径 `ensure_partition`
  按需创建(advisory xact lock 串行化,防并发首批撞 duplicate_table)。
- 公司数预期 ≤ ~100,分区数安全。

### 为什么 Qwen3-Embedding + Qwen3-Reranker

| 维度 | 这套 | 说明 |
|---|---|---|
| 多语言支持 | 100+ 语言 | Qwen3-Embedding 系列 MTEB 多语言强 |
| MRL 可变维度 | ✅ | 维度冻结在 1024(换维度=历史向量作废重算) |
| 商业许可 | Apache 2.0 | - |
| CPU 可跑 | ✅ | 0.6B Q8_0 很小,两个模型都能在 CPU 上 llama.cpp 部署 |

> embedder / reranker 同一家族(Qwen3-Embedding-0.6B + Qwen3-Reranker-0.6B),Q8_0 量化
> 后权重各约 1GB。要更强的多语言精度可换更大的 Qwen3-Reranker GGUF —— 改 gpu-server
> compose 里 reranker 的 wget 直链 + `-m` 路径即可(详见下节)。

### 为什么用 llama.cpp GGUF 部署模型

| | llama.cpp | TEI | vLLM | 自己写 |
|---|---|---|---|---|
| Embedding 支持 | ✅ `--embeddings` | ✅ 原生 | ⚠️ 部分 | 麻烦 |
| Reranker 支持 | ✅ `--reranking` | ✅ 原生 | ❌ | 麻烦 |
| OpenAI 兼容 API | ✅ `/v1/embeddings` | ✅ | ✅ | - |
| 权重获取 | 公开 GGUF wget,无需 HF 账户 | 需 HF 拉取 | 同 | - |
| 部署难度 | 极低(单 binary) | 极低 | 低 | 高 |

llama.cpp 的 `llama-server` 是单 binary,同一镜像(`ggml-org/llama.cpp:server`)既能用
`--embeddings` 起 embedder、也能用 `--reranking` 起 reranker,直接吃本地 GGUF 文件;权重由
per-model 一次性 fetch 服务(fetch-embedder / fetch-reranker)从 ggml-org 的公开 `resolve/`
直链 wget 到 `chat_models` 卷,**无需 HF 账户**。单模型 server,请求里的 `model` 字段仅用于
日志对齐(真正路由靠 URL)。

**换 reranker 模型**:改 `gpu-server/docker-compose.yml` 里 `fetch-reranker` 的 wget URL
(指向新 GGUF 的 `resolve/` 直链)和 `reranker` 服务的 `-m /models/<新文件名>.gguf`。
不是改 HF repo id,也不是改 `RERANKER_MODEL`(那只是日志字段)。

## 横向扩容

### 主体应用(无状态)

```yaml
services:
  app:
    deploy:
      replicas: 3      # 前面挂 nginx / Traefik 做负载均衡
```

每个 app 实例都连同一个 PG + 同一组模型服务。

### 模型服务

```yaml
services:
  embedder:
    deploy:
      replicas: 2      # 多副本; 主体 EMBEDDER_BASE_URL 指向负载均衡器
```

### PostgreSQL

最简单的扩容: 升级机器(垂直扩展)。Postgres 16 单机扛千万 messages 没问题。
亿级以上: 读写分离(主从)/ 按 company 已天然分区,进一步可分库。

## 数据库 schema 演化

加字段(注意分区表):

```sql
ALTER TABLE messages ADD COLUMN reactions JSONB;   -- 父表加列自动传播到分区
CREATE INDEX idx_msg_xxx ON messages (...);        -- 普通索引建父表自动传播
```

embedding 维度想换:**代价巨大**(要重算所有历史向量)。一开始就选好维度;真要换
= 加新列 `embedding_v2`,双写 N 天再切。

## 安全

| 层 | 措施 |
|---|---|
| 网络 | 模型服务只对应用机内网开放;主体 API 只对 bob 应用机内网开放 |
| 数据面鉴权 | 可选 `API_KEY`:设置后 `/messages*`+`/retrieve` 要求 `X-API-Key` 一致;空 = 网络层信任 |
| scope 隔离 | `company_id` 是分区键不是身份 —— **跨公司不泄露全靠 bob 每次下 `company_id` filter**(fail-closed),分区是第二道防线 |
| 数据库 | 强密码, 不暴露公网 |
| append-only | 不删不改;进库前由 bob `scrub`(密钥不落进不可删的语料) |

## 不在本系统里的东西(调用方负责)

- 生成式 LLM 调用(bob 拿检索结果自己做)
- 用户鉴权 / ACL / account→handle 解析(bob 做; 语料只存不可变 handle)
- 话题建模 / 演化叙事(bob 用 LLM 现场综述)
- 数据 scrub(bob 在进库前做)
