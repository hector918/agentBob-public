"""PostgreSQL 异步连接池"""
import asyncpg
import hashlib
import json
import logging
from config import settings

logger = logging.getLogger(__name__)


db_pool: asyncpg.Pool | None = None


async def init_db_pool():
    """启动时创建连接池"""
    global db_pool
    db_pool = await asyncpg.create_pool(
        settings.database_url,
        min_size=settings.db_pool_min,
        max_size=settings.db_pool_max,
        command_timeout=60,
        init=_init_connection,
        setup=_setup_connection,
    )


async def _init_connection(conn: asyncpg.Connection):
    """每条物理连接建立时跑一次:注册 JSONB/JSON 自动转 dict(客户端 codec,
    不受会话 RESET 影响)。"""
    await conn.set_type_codec(
        "jsonb",
        encoder=json.dumps,
        decoder=json.loads,
        schema="pg_catalog",
    )
    await conn.set_type_codec(
        "json",
        encoder=json.dumps,
        decoder=json.loads,
        schema="pg_catalog",
    )

async def _setup_connection(conn: asyncpg.Connection):
    """每次从池里 acquire 都跑:asyncpg 在连接归还时执行 RESET ALL,会把
    SET 的 GUC 清掉 —— 放 init= 只对每条连接的第一次 checkout 生效,之后
    静默回到 pgvector 默认(iterative_scan=off / ef_search=40),带 filter
    的向量查询欠召回且无任何报错。所以 GUC 必须在 setup= 里每次重设。"""
    # HNSW iterative scan (pgvector 0.8+): 带 metadata filter 时持续扫描
    # 直到凑够 LIMIT,避免"全局近邻先取再过滤"的欠召回。pgvector < 0.8
    # 没有这些 GUC,SET 会报错 —— 容忍 (降级为普通 HNSW)。
    for stmt in (
        f"SET hnsw.iterative_scan = {settings.hnsw_iterative_scan}",
        f"SET hnsw.ef_search = {int(settings.hnsw_ef_search)}",
    ):
        try:
            await conn.execute(stmt)
        except Exception as e:  # noqa: BLE001
            logger.warning("skip '%s' (pgvector 可能 < 0.8): %s", stmt, e)


async def close_db_pool():
    """关闭连接池"""
    if db_pool is not None:
        await db_pool.close()


def get_pool() -> asyncpg.Pool:
    """获取连接池 (供 handler 使用)"""
    if db_pool is None:
        raise RuntimeError("DB pool not initialized")
    return db_pool


def _partition_name(company_id: str) -> str:
    """company_id → 稳定、合法的分区表名 (任意文本都安全)"""
    h = hashlib.md5(company_id.encode("utf-8")).hexdigest()[:16]
    return f"messages_p_{h}"


async def ensure_partition(conn: asyncpg.Connection, company_id: str):
    """
    按需为某 company 建分区 + 该分区的 HNSW 索引 (幂等)。

    '__personal__' 分区在 schema.sql 已预建,跳过。company_id 走字面
    单引号转义拼进 DDL —— asyncpg 不支持给 CREATE TABLE ... FOR VALUES
    的值绑参数。
    """
    if company_id == "__personal__":
        return
    part = _partition_name(company_id)
    safe = company_id.replace("'", "''")
    # IF NOT EXISTS 不防并发:同一新公司的两个并发首批会双双通过存在性检查,
    # 输家撞 duplicate_table → 整批 500。advisory xact lock 按 company 串行化
    # (锁随事务结束自动释放)。
    async with conn.transaction():
        await conn.execute(
            "SELECT pg_advisory_xact_lock(hashtext($1)::bigint)", company_id
        )
        await conn.execute(
            f"CREATE TABLE IF NOT EXISTS {part} "
            f"PARTITION OF messages FOR VALUES IN ('{safe}')"
        )
        await conn.execute(
            f"CREATE INDEX IF NOT EXISTS {part}_emb "
            f"ON {part} USING hnsw (embedding vector_cosine_ops)"
        )


def format_vector(vec: list[float]) -> str:
    """asyncpg + pgvector: 向量要转字符串"""
    return "[" + ",".join(f"{x:.8f}" for x in vec) + "]"
