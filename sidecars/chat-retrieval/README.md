# Chat Retrieval System

agentbob 的**冷记忆叶子**(sidecar):为 bob 提供"对话记忆"检索能力。
多语言、按公司分区、只检索不调 LLM。

集成契约与运维事实源见 [`docs/INTEGRATION.md`](docs/INTEGRATION.md)。

## 它做什么

提供检索功能,把**结构化结果**返回给 bob;bob 拿结果拼 prompt、用自己的
LLM 现场综述。

```
┌──────────────┐     ┌────────────┐     ┌──────────┐
│     bob      │ ──> │  本系统     │ ──> │  bob 的   │
│              │     │ (检索/聚合) │     │  LLM      │
└──────────────┘     └────────────┘     └──────────┘
                          ↑
                     结构化数据
                  (消息 / 聚合统计)
```

**本系统不调 LLM**。生成式调用主权完全在 bob。服务不在时 bob 照常运行(fail-open)。

## 核心功能

| 功能 | query_type | 描述 |
|---|---|---|
| **对象相关** | `entity_mentions` | 查"某人/公司/项目"说过什么、被提到的所有消息(author + 字面 + 语义三路 + rerank) |
| **时段总览** | `time_window` | 某段时间发生了什么(消息切片 + 聚合统计:总量/独立用户/按天/活跃用户) |
| **语义兜底** | `semantic` | 通用语义检索 |

> 离线话题(BERTopic / 话题演化)**已在 agentbob 集成中移除** —— 话题/时段叙事
> 由 bob 拿 `time_window` / `entity_mentions` 的结果做 LLM 现场综述。详见
> [`docs/INTEGRATION.md`](docs/INTEGRATION.md)。

## 技术栈

- **存储**: PostgreSQL 16 + pgvector(0.8+,用到 `hnsw.iterative_scan`);`messages` 按 `company_id` LIST 分区
- **Embedding**: Qwen3-Embedding-0.6B Q8_0(1024 维,100+ 语言),llama.cpp GGUF(OpenAI `/v1/embeddings`)
- **Reranker**: Qwen3-Reranker-0.6B Q8_0,llama.cpp GGUF(Cohere/Jina 风格 `/rerank`)
- **应用框架**: FastAPI + asyncpg

## 文档导航

- [`docs/INTEGRATION.md`](docs/INTEGRATION.md) - **agentbob 集成契约 + 部署前自检(事实源)**
- [`docs/SETUP.md`](docs/SETUP.md) - 部署指南(从零到跑通)
- [`docs/USAGE.md`](docs/USAGE.md) - 使用文档(API 怎么调、参数说明)
- [`docs/FEATURES.md`](docs/FEATURES.md) - 功能详解(三大功能的实现原理)
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) - 架构说明

## 目录结构

```
chat-retrieval/
├── docker-compose.yml          # 主体 (Postgres + App)
├── schema.sql                  # 数据库 schema (按 company_id 分区)
├── app/
│   ├── Dockerfile              # 主体应用镜像 (~200MB, 不带模型)
│   ├── requirements.txt
│   ├── .env.example
│   ├── main.py                 # FastAPI 入口 (/health, /selftest, 鉴权)
│   ├── config.py               # 配置 (全从环境变量读)
│   ├── clients/
│   │   ├── db.py               # PostgreSQL 连接池 + 分区/GUC
│   │   ├── embedder.py         # 调 Embedding API (OpenAI /v1/embeddings)
│   │   └── reranker.py         # 调 Reranker API (llama.cpp /rerank)
│   ├── ingestion/
│   │   └── handler.py          # POST /messages, /messages/batch (同步落库)
│   └── retrieval/
│       ├── api.py              # POST /retrieve 路由
│       ├── entity_mentions.py  # 功能 1
│       ├── time_window.py      # 功能 2
│       ├── semantic.py         # 功能 3 (通用兜底)
│       └── utils.py            # filters → SQL
├── gpu-server/
│   └── docker-compose.yml      # 模型服务 (llama.cpp GGUF: embedder + reranker, 可独立部署)
├── up.sh                       # Ubuntu 一键拉起 (新部署交互问三个地址, 内置/外置由地址判定)
├── scripts/
│   ├── load_sample_data.py     # 灌测试数据
│   └── test_queries.sh         # 测试各 query_type
└── docs/
    ├── INTEGRATION.md
    ├── SETUP.md
    ├── USAGE.md
    ├── FEATURES.md
    └── ARCHITECTURE.md
```

## 快速开始

**Ubuntu 一键拉起** `./up.sh`。代码跟 repo 走(常 git pull / 重 clone),**部署状态放
repo 之外的独立空间** `~/.chat-retrieval`(重 clone 不丢):
- `~/.chat-retrieval/.env` —— 三个重点 + 配置(库 DSN / embedding URL / reranker URL / API_KEY…)
- pg 数据 / 模型权重 —— docker 具名卷(`chat_pg_data` / `chat_models`),本就独立于目录

**新部署**(`~/.chat-retrieval/.env` 不存在)且有交互终端 → 逐个问三个服务,**每个 internal
还是 external**(回车=external 并输入地址;输 `i`=内置由脚本起):

```
== 配置三个服务 ==
[1/3] 数据库(内置 postgres) internal / external? [i/E]: e
  DATABASE_URL: postgres://u:p@<db-host>:5432/chatdata
[2/3] Embedding(内置 llama-server GGUF) internal / external? [i/E]: e
  EMBEDDER_BASE_URL: http://<inference-host>:8080/v1
[3/3] Reranker(内置 llama-server GGUF) internal / external? [i/E]: e
  RERANKER_BASE_URL: http://<inference-host>:8081
```

**已部署**(`.env` 已在)→ 不问、不理 flag,按 `.env` 跑。**无终端**(cron/管道)→ 用
`--db/--embedder/--reranker`(或同名大写环境变量),缺的取内置默认。

```bash
./up.sh                                          # 交互问三个(或已部署直接跑)
./up.sh --db postgres://u:p@host:49179/chatdata \
        --embedder http://gpu:8080/v1 --reranker http://gpu:8081   # 非交互/脚本化
./up.sh --rebuild        # 重建 app 镜像
./up.sh --down [--purge] # 停(--purge 连具名卷一起删;.env 永远保留)
```

- **内置 vs 外置 = 看地址**(app 永远只是 HTTP 客户端,不分内外):DSN host `postgres`
  → 起内置 postgres;embedder/reranker host 是 `host.docker.internal`/`localhost` → 本机
  起 llama-server(自动 wget ggml-org 的 GGUF,无需 HF 账户);其它 host → 外部,只连不起。
- **外部库**:`up.sh` 每次**幂等灌 `schema.sql`**(全 `CREATE … IF NOT EXISTS`)。
- 三个地址也能用 flag / 同名大写环境变量;改地址 = 编辑 `~/.chat-retrieval/.env` 或重跑带新值。
- 独立空间路径:`--state-dir /path` 或 `STATE_DIR=/path`。端口:`EMBEDDER_PORT/RERANKER_PORT/APP_PORT`。
- llama-server 内置模型参数在 `gpu-server/docker-compose.yml`(`-c 16384/8192 --parallel 2`)。

详细步骤见 [docs/SETUP.md](docs/SETUP.md)。
