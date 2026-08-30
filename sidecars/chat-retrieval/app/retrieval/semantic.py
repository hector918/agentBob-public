"""query_type=semantic: 语义检索 (向量 + rerank)"""
import logging
from clients.embedder import embedder
from clients.reranker import reranker
from clients.db import get_pool, format_vector
from config import settings
from .utils import build_filter_sql

logger = logging.getLogger(__name__)


async def handle(params: dict, filters: dict | None, limit: int) -> dict:
    query_text = params.get("query_text")
    if not query_text:
        return {"error": "query_text required"}

    candidates_n = params.get("candidates", settings.default_candidates)

    # 1. 查询向量
    query_vecs = await embedder.embed(query_text, is_query=True)
    qvec = format_vector(query_vecs[0])

    # 2. 构造 SQL
    where, sql_params, next_idx = build_filter_sql(filters, starting_param_idx=2)
    sql = f"""
        SELECT
            message_id, company_id, session_id, user_id, worker_id,
            timestamp, received_at, role, content, channel_id, lang,
            user_attributes, session_attributes,
            1 - (embedding <=> $1::vector) as vector_score
        FROM messages
        WHERE embedding IS NOT NULL AND ({where})
        ORDER BY embedding <=> $1::vector
        LIMIT {int(candidates_n)}
    """

    pool = get_pool()
    async with pool.acquire() as conn:
        rows = await conn.fetch(sql, qvec, *sql_params)

    if not rows:
        return {
            "query_type": "semantic",
            "context": [],
            "metadata": {"result_count": 0, "candidates_scanned": 0},
        }

    # 3. rerank
    if settings.reranker_enabled and len(rows) > 1:
        try:
            reranked = await reranker.rerank(
                query=query_text,
                documents=[r["content"] for r in rows],
                top_n=limit,
            )
            ordered = [(rows[r["index"]], r["score"]) for r in reranked]
        except Exception as e:
            # reranker 挂了, 退回向量分排序 —— 必须可见:这是质量降级,
            # 不打日志的话 rerank 永久失效也没人知道。
            logger.warning(f"rerank failed, falling back to vector order: {e}")
            ordered = [(r, r["vector_score"]) for r in rows[:limit]]
    else:
        ordered = [(r, r["vector_score"]) for r in rows[:limit]]

    # 4. 拼结果
    context = []
    for row, score in ordered:
        context.append({
            "type": "message",
            "content": row["content"],
            "metadata": {
                "message_id": row["message_id"],
                "company_id": row["company_id"],
                "session_id": row["session_id"],
                "user_id": row["user_id"],
                "worker_id": row["worker_id"],
                "timestamp": row["timestamp"].isoformat(),
                "received_at": row["received_at"].isoformat() if row["received_at"] else None,
                "role": row["role"],
                "channel_id": row["channel_id"],
                "lang": row["lang"],
                "user_attributes": row["user_attributes"],
                "session_attributes": row["session_attributes"],
            },
            "scores": {
                "vector": float(row["vector_score"]),
                "final": float(score),
            },
        })

    return {
        "query_type": "semantic",
        "context": context,
        "metadata": {
            "result_count": len(context),
            "candidates_scanned": len(rows),
        },
    }
