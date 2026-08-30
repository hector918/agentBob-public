"""
query_type=time_window
功能 2: 某段时间发生了什么

返回:
  - 代表性消息 (有 query_text → 语义切片; 无 → 该时段最近 N 条)
  - 聚合统计 (总量 / 独立用户 / 按天 / 活跃用户)

话题分布/主题聚合不在本系统做 —— 调用方拿这里的 messages + aggregations,
按需让自己的 LLM 现场综述。

参数:
  - time_range: 必须 (filters 或 params 里给)
  - query_text: 可选 (有就做语义检索, 没有就返时段总览)
"""
import logging
from clients.embedder import embedder
from clients.reranker import reranker
from clients.db import get_pool, format_vector
from config import settings
from .utils import build_filter_sql

logger = logging.getLogger(__name__)

# 返回行里要带的列 (统一一处, 别在多个 SQL 里漂移)
_COLS = (
    "message_id, company_id, session_id, user_id, worker_id, "
    "timestamp, received_at, role, content, channel_id, lang"
)


def _row_meta(row) -> dict:
    return {
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
    }


async def handle(params: dict, filters: dict | None, limit: int) -> dict:
    tr = (filters or {}).get("time_range") or params.get("time_range")
    if not tr:
        return {"error": "time_range required"}

    # 形状不对以前直接 unpack/.get 炸成 500;这是调用方的查询错误,响亮 400。
    if isinstance(tr, (list, tuple)) and len(tr) == 2:
        start, end = tr
    elif isinstance(tr, dict):
        start, end = tr.get("start"), tr.get("end")
    else:
        return {"error": "time_range must be [start, end] or {start, end}"}
    if not (start and end):
        return {"error": "time_range needs both start and end"}

    # 确保 filters 里有 time_range (供 build_filter_sql 使用)
    filters = dict(filters or {})
    filters["time_range"] = [start, end]

    query_text = params.get("query_text")  # 可选: 这时段里"关于 X"的事

    # 查询向量在拿池连接**之前**算 —— embed 是外部 HTTP(超时 120s),拿着
    # 池连接干等会把池抽干(semantic.py 同款顺序)。
    qvec = None
    if query_text:
        qvecs = await embedder.embed(query_text, is_query=True)
        qvec = format_vector(qvecs[0])

    pool = get_pool()
    async with pool.acquire() as conn:

        # === 1. 拿代表性消息(这里只取行;rerank 也是外部 HTTP,挪到连接
        #        归还之后做) ===
        rows = []

        if qvec is not None:
            where, sql_params, _ = build_filter_sql(filters, starting_param_idx=2)
            candidates_n = settings.default_candidates
            sql = f"""
                SELECT {_COLS},
                    1 - (embedding <=> $1::vector) as vector_score
                FROM messages
                WHERE embedding IS NOT NULL AND ({where})
                ORDER BY embedding <=> $1::vector
                LIMIT {int(candidates_n)}
            """
            rows = await conn.fetch(sql, qvec, *sql_params)
        else:
            # 没有 query: 返回该时段最近的 N 条
            where, sql_params, _ = build_filter_sql(filters, starting_param_idx=1)
            sql = f"""
                SELECT {_COLS}
                FROM messages
                WHERE {where}
                ORDER BY timestamp DESC
                LIMIT {int(limit)}
            """
            rows = await conn.fetch(sql, *sql_params)

        # === 2. 聚合统计 ===
        where, sql_params, _ = build_filter_sql(filters, starting_param_idx=1)
        agg_sql = f"""
            SELECT
                count(*) as total_messages,
                count(distinct user_id) as unique_users,
                count(distinct session_id) as unique_sessions,
                count(distinct channel_id) as unique_channels
            FROM messages
            WHERE {where}
        """
        agg = await conn.fetchrow(agg_sql, *sql_params)

        # 按天分布
        by_day_sql = f"""
            SELECT
                date_trunc('day', timestamp) as day,
                count(*) as count
            FROM messages
            WHERE {where}
            GROUP BY day
            ORDER BY day
        """
        by_day = await conn.fetch(by_day_sql, *sql_params)

        # 最活跃用户
        by_user_sql = f"""
            SELECT user_id, count(*) as count
            FROM messages
            WHERE {where}
            GROUP BY user_id
            ORDER BY count DESC
            LIMIT 10
        """
        by_user = await conn.fetch(by_user_sql, *sql_params)

    # === 3. rerank + 拼消息(连接已归还,外部 HTTP 不占池) ===
    messages_context = []
    if qvec is not None:
        if settings.reranker_enabled and len(rows) > 1:
            try:
                rr = await reranker.rerank(
                    query=query_text,
                    documents=[r["content"] for r in rows],
                    top_n=limit,
                )
                ordered = [(rows[r["index"]], r["score"]) for r in rr]
            except Exception as e:
                logger.warning(f"rerank failed, falling back to vector order: {e}")
                ordered = [(r, r["vector_score"]) for r in rows[:limit]]
        else:
            ordered = [(r, r["vector_score"]) for r in rows[:limit]]

        for row, score in ordered:
            messages_context.append({
                "type": "message",
                "content": row["content"],
                "metadata": _row_meta(row),
                "scores": {
                    "vector": float(row["vector_score"]),
                    "final": float(score),
                },
            })
    else:
        for row in rows:
            messages_context.append({
                "type": "message",
                "content": row["content"],
                "metadata": _row_meta(row),
            })

    return {
        "query_type": "time_window",
        "context": {
            "messages": messages_context,
        },
        "aggregations": {
            "total_messages": agg["total_messages"] if agg else 0,
            "unique_users": agg["unique_users"] if agg else 0,
            "unique_sessions": agg["unique_sessions"] if agg else 0,
            "unique_channels": agg["unique_channels"] if agg else 0,
            "by_day": [
                {"day": r["day"].isoformat(), "count": r["count"]}
                for r in by_day
            ],
            "by_user": [
                {"user_id": r["user_id"], "count": r["count"]}
                for r in by_user
            ],
        },
        "metadata": {
            "time_range": {"start": start, "end": end},
        },
    }
