# 部署指南

集成契约与部署前自检见 [`INTEGRATION.md`](INTEGRATION.md)。

## 快速路径:`./up.sh`

Ubuntu 上的唯一入口是 [`up.sh`](../up.sh)(见 [README](../README.md) 的"快速开始")。
它把下面的"模型 → 配置 → 起 app → 自检"全串好:

```bash
./up.sh                  # 新部署交互问三个地址(库 / embedder / reranker);已部署直接跑
./up.sh --db postgres://u:p@host:49179/chatdata \
        --embedder http://gpu:8080/v1 --reranker http://gpu:8081   # 非交互/脚本化
```

**内置 vs 外置 = 看地址**:DSN host `postgres` → 起内置 postgres;embedder/reranker host
是 `host.docker.internal`/`localhost` → 本机起 llama-server(自动 wget GGUF);其它 host
→ 外部,只连不起。配置写进 repo 之外的 `~/.chat-retrieval/.env`(重 clone 不丢);pg 数据
和模型权重是 docker 具名卷(`chat_pg_data` / `chat_models`)。

下面是同一套步骤的手动展开(理解每步在做什么、或单独排查时用)。

## 部署架构

```
┌────────────────────────────┐         ┌──────────────────────────────┐
│  应用机                     │ ──HTTP─>│  模型机                      │
│  ┌──────────────────────┐  │         │  ┌──────────────────────┐    │
│  │ PostgreSQL           │  │         │  │ Embedder (llama.cpp) │    │
│  │ 主体 API (FastAPI)   │  │         │  │ Reranker (llama.cpp) │    │
│  └──────────────────────┘  │         │  └──────────────────────┘    │
└────────────────────────────┘         └──────────────────────────────┘
```

`gpu-server/docker-compose.yml` 用 **llama.cpp llama-server**(`ghcr.io/ggml-org/llama.cpp:server`,
默认 CPU)起两个 GGUF:Qwen3-Embedding-0.6B Q8_0(`--embeddings`)+ Qwen3-Reranker-0.6B Q8_0
(`--reranking`)。无 GPU 也能跑;有 GPU 想更快:把 image 换 `:server-cuda` 并加
`deploy.resources.reservations.devices`。两台机器最理想;只有一台时把两个 compose 都在一台
跑也行(走 `host.docker.internal`)。

## 系统要求

### 模型机(跑 embedder + reranker)

- 0.6B Q8_0 模型很小(embedder ~1.2GB / reranker ~1.1GB 权重),普通 4 核 + 8GB 内存即可
- GPU 版(可选,更快):显存几 GB 足够(两个 0.6B 模型都很小)
- Docker;第一次启动会 wget GGUF 模型(~2.3GB 合计),需要网络;无需 HuggingFace 账户

### 应用机(主体 + 数据库)

- CPU 4 核+、内存 8GB+、磁盘 50GB+(PostgreSQL 数据)
- Docker + Docker Compose,不需要 GPU

## 第 1 步: 部署模型服务(模型机)

```bash
cd gpu-server
docker compose up -d              # fetch-embedder/fetch-reranker 先 wget GGUF,再起两个 llama-server
docker compose logs -f embedder   # 看到 "server is listening" 即就绪
```

> ⚠ gpu-server compose **故意不设 healthcheck** —— 首次启动要先下模型,期间不响应,任何
> 健康检查都会误判并触发重启循环。llama.cpp 加载本身很快;唯一的首跑等待是模型 **下载**
> (~1.2GB + ~1.1GB,wget ggml-org 的公开 `resolve/` 直链,无需 HF 账户),`up.sh` 会轮询
> `/v1/embeddings` 和 `/rerank` 判定就绪。

验证(注意:embedder base_url 带 `/v1`,reranker 不带):

```bash
# 测 embedding (OpenAI 兼容 /v1/embeddings —— 主体应用就走这个)
curl http://localhost:8080/v1/embeddings -H "Content-Type: application/json" \
  -d '{"input": ["hello world"], "model": "qwen3-embedding-0.6b-q8_0.gguf"}'
# → {"data":[{"index":0,"embedding":[...]}], ...}

# 测 reranker (llama.cpp /rerank)
curl http://localhost:8081/rerank -H "Content-Type: application/json" \
  -d '{"model": "qwen3-reranker-0.6b-q8_0.gguf", "query": "凤凰项目进展", "documents": ["Phoenix project status update", "今天天气真好"]}'
# → {"results":[{"index":0,"relevance_score":...}, ...]}
```

## 第 2 步: 配置主体应用(应用机)

```bash
cp app/.env.example app/.env
nano app/.env
```

**关键改动**:

```ini
# 把模型机的内网地址填进来
EMBEDDER_BASE_URL=http://10.0.1.50:8080/v1
RERANKER_BASE_URL=http://10.0.1.50:8081

# 数据库密码换强的(同步改 docker-compose.yml 的 POSTGRES_PASSWORD)
DATABASE_URL=postgres://chat:你的强密码@chat-postgres:5432/chatdata

# 数据面鉴权(可选): 设了之后 /messages* + /retrieve 要求 X-API-Key 一致
# 调用方(bob)的 retrieval.api_key 要填同一个值。空 = 网络层信任。
API_KEY=
```

> 单机部署:`EMBEDDER_BASE_URL` / `RERANKER_BASE_URL` 用 `http://host.docker.internal:8080/v1`
> 等,并确保 `docker-compose.yml` 的 `app` 服务带 `extra_hosts: ["host.docker.internal:host-gateway"]`
> (compose 里已带)。

## 第 3 步: 启动主体

```bash
docker compose up -d
docker compose logs -f app

# 应该看到:
# starting up...
#   embedder: http://10.0.1.50:8080/v1 (qwen3-embedding-0.6b-q8_0.gguf)
#   reranker: http://10.0.1.50:8081 (qwen3-reranker-0.6b-q8_0.gguf)
# ready.
```

## 第 4 步: 部署前自检(一次请求验全栈)

```bash
curl http://localhost:8000/selftest
```

`/selftest` 一次性验 **db 连通 / pgvector≥0.8 / embedder 维度符合 schema / reranker 可达**,
逐项 ok+detail,有任一 fail 返 503。最易踩的坑是 embedder 维度:服务端不一定认请求里的
`dimensions`,/selftest 会探原生维度并判定。bob 侧对应 `bob debug retrieval`。

```bash
# 轻量探活(带 DB ping)
curl http://localhost:8000/health
# {"status":"ok","db":"ok"}
```

## 第 5 步: 灌点测试数据(可选)

```bash
pip install httpx
python scripts/load_sample_data.py
```

## 网络拓扑建议

### 单机部署(开发/小规模)

```
docker compose up -d                     # 主体
cd gpu-server && docker compose up -d     # 模型
# .env: EMBEDDER_BASE_URL=http://host.docker.internal:8080/v1
```

### 双机部署(推荐生产)

```
应用机 (10.0.1.10):  docker compose up -d
模型机 (10.0.1.50):  cd gpu-server && docker compose up -d
                     防火墙: 只允许 10.0.1.10 访问 8080, 8081
# .env: EMBEDDER_BASE_URL=http://10.0.1.50:8080/v1
```

主体 API 与模型服务都**只对内网开放**;跨公网必须 HTTPS + 鉴权,不要裸暴露。

## 常见问题

### Q: 模型服务很慢 / 一开始不响应

首跑要先 wget GGUF(~2.3GB 合计),期间不响应;下完 llama.cpp 加载很快。模型权重缓存在
具名卷 `chat_models`,重启不再下。`up.sh` 用每模型独立的 fetch 服务,只起 embedder 时不会
顺带下 reranker 的 ~1GB(反之亦然)。

### Q: 主体启动报 "connection refused" 连不上模型服务

- 模型机防火墙是否放行 8080/8081
- 网络是否通:`docker exec -it chat-app curl http://10.0.1.50:8080/v1/embeddings -H 'Content-Type: application/json' -d '{"input":"ping","model":"x"}'`
- `.env` 里地址/端口是否正确(embedder 带 `/v1`,reranker 不带)

### Q: /selftest 报 embedder_dim fail / pgvector<0.8

- `embedder_dim` < 期望 = 配错模型;> 期望 = 服务端忽略 dimensions 靠客户端截断(MRL 模型 OK)
- `pgvector<0.8` = 没有 `hnsw.iterative_scan`,带 filter 的向量查询会欠召回,升级 pgvector

### Q: PostgreSQL 数据怎么备份

```bash
docker exec chat-postgres pg_dump -U chat chatdata > backup.sql
cat backup.sql | docker exec -i chat-postgres psql -U chat chatdata
```

### Q: 应用升级怎么做

```bash
docker compose build app && docker compose up -d app   # 数据库零影响
```

## 监控建议

最少要监控:`GET /health`(主体)、`GET http://<模型机>:8080/health`(embedder)、
PostgreSQL 连接数 / 慢查询、Embedding/Rerank 调用延迟。llama-server 和 FastAPI 都有现成 metrics。
