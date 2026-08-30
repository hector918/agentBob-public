"""主体应用入口"""
import logging
import secrets
from contextlib import asynccontextmanager
from fastapi import Depends, FastAPI, Header, HTTPException
from fastapi.responses import JSONResponse

from config import settings
from clients.embedder import embedder
from clients.reranker import reranker
from clients.db import init_db_pool, close_db_pool, get_pool
from ingestion.handler import router as ingestion_router
from retrieval.api import router as retrieval_router


logging.basicConfig(
    level=settings.log_level.upper(),
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)
logger = logging.getLogger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    logger.info("starting up...")
    await init_db_pool()
    logger.info(f"  embedder: {settings.embedder_base_url} ({settings.embedder_model})")
    logger.info(f"  reranker: {settings.reranker_base_url} ({settings.reranker_model})")
    logger.info("ready.")
    yield
    logger.info("shutting down...")
    await close_db_pool()
    await embedder.close()
    await reranker.close()


app = FastAPI(
    title="Chat Retrieval API",
    description="多语言对话检索系统 - 对象检索 / 时段切片 / 语义兜底 (只检索, 不调 LLM)",
    version="1.0.0",
    lifespan=lifespan,
)


async def _require_api_key(x_api_key: str | None = Header(None, alias="X-API-Key")):
    """数据面鉴权:API_KEY 设置时,/messages* 和 /retrieve 必须带一致的
    X-API-Key(调用方 bob 的 retrieval.api_key)。叶子装着跨公司语料,
    company_id 只是分区键不是身份 —— 没有这道门,任何能到达端口的进程都能
    读任意公司分区/注入"记忆"。API_KEY 为空 = 维持网络层信任,不鉴权。"""
    if not settings.api_key:
        return
    if not (x_api_key and secrets.compare_digest(x_api_key, settings.api_key)):
        raise HTTPException(401, "invalid or missing X-API-Key")


app.include_router(ingestion_router, dependencies=[Depends(_require_api_key)])
app.include_router(retrieval_router, dependencies=[Depends(_require_api_key)])


@app.get("/health", tags=["meta"])
async def health():
    """探活: 确认进程 + DB 都通(调用方 bob 碰不到 DB,靠这个判断叶子是否真活)。
    DB 不通时返 503 + degraded,让 bob 的探针能区分"进程在但库挂了"。
    用 SELECT 1 而非 count(*) —— docker healthcheck 每 30s 打一次,在分区大表上
    全表 count 会越来越慢 + 周期性压库。"""
    try:
        pool = get_pool()
        async with pool.acquire() as conn:
            await conn.fetchval("SELECT 1")
        return {"status": "ok", "db": "ok"}
    except Exception as e:  # noqa: BLE001
        return JSONResponse(
            status_code=503,
            content={"status": "degraded", "db": "error", "error": str(e)},
        )


def _ver_ge(v: str | None, want: tuple[int, ...]) -> bool:
    """'0.8.0' >= (0,8) ?  解析失败 → False。"""
    if not v:
        return False
    parts = tuple(int(x) for x in v.split(".")[:3] if x.isdigit())
    return parts >= want


@app.get("/selftest", tags=["meta"])
async def selftest():
    """**接口符合性自检** —— 打这一个请求,顺便把"接口是否合要求"全验了,代替部署前
    手工 checklist。逐项 ok/detail;有任一 fail 返 503。只读 + 探针字符串,不写真数据。

    检查项:db 连通 / pgvector≥0.8 / **embedder 维度符合 schema(D-1 核心)** / reranker 可达。"""
    checks: list[dict] = []
    all_ok = True

    def add(name: str, passed: bool, detail: str = ""):
        nonlocal all_ok
        all_ok = all_ok and passed
        checks.append({"check": name, "ok": passed, "detail": detail})

    # 1. DB + pgvector 版本
    try:
        pool = get_pool()
        async with pool.acquire() as conn:
            ver = await conn.fetchval("SELECT extversion FROM pg_extension WHERE extname='vector'")
        add("db", True, "connected")
        add("pgvector>=0.8", _ver_ge(ver, (0, 8)), f"version={ver}（<0.8 则 iterative_scan 不可用，召回会差）")
    except Exception as e:  # noqa: BLE001
        add("db", False, str(e))

    # 2. embedder 维度符合 schema —— ★ 最易让首跑全挂的点
    try:
        raw = await embedder.probe_raw_dim()
        want = settings.embedder_dimensions
        if raw == want:
            add("embedder_dim", True, f"服务端原生返 {raw} == 期望 {want}（认 dimensions）")
        elif raw > want:
            add("embedder_dim", True,
                f"服务端原生返 {raw} > 期望 {want}：忽略 dimensions，靠客户端截断。"
                f"Qwen3 等 MRL 模型 OK；非 MRL 模型截断会出错，请确认模型")
        else:
            add("embedder_dim", False, f"服务端返 {raw} < 期望 {want} —— 模型/配置不对")
    except Exception as e:  # noqa: BLE001
        add("embedder", False, str(e))

    # 3. reranker（若启用）
    if settings.reranker_enabled:
        try:
            r = await reranker.rerank("probe", ["a", "b"], top_n=2)
            add("reranker", len(r) > 0, f"scored={len(r)}")
        except Exception as e:  # noqa: BLE001
            add("reranker", False, str(e))
    else:
        add("reranker", True, "disabled (RERANKER_ENABLED=false)")

    return JSONResponse(status_code=200 if all_ok else 503, content={"ok": all_ok, "checks": checks})


@app.get("/", tags=["meta"])
async def root():
    return {
        "name": "Chat Retrieval API",
        "version": "1.0.0",
        "docs": "/docs",
        "endpoints": {
            "ingest": "POST /messages, POST /messages/batch",
            "retrieve": "POST /retrieve",
        },
    }
