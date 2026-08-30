"""
query_type=entity_mentions
功能 1: 查"某个人/公司/对象"相关的所有信息

支持两种模式 (可同时):
  - author_mode:  who_id 指定的用户发出的消息
  - mention_mode: 任何人提到 "Alice"/"OpenAI" 的消息

字面匹配 + 向量语义召回 + 可选 rerank
"""
import logging
from clients.embedder import embedder
from clients.reranker import reranker
from clients.db import get_pool, format_vector
from config import settings
from .utils import build_filter_sql

logger = logging.getLogger(__name__)


async def handle(params: dict, filters: dict | None, limit: int) -> dict:
    # 三种输入方式 (互斥优先级: who_id > who_text)
    who_id = params.get("who_id")       # 内部 ID, 比如 user_id
    who_text = params.get("who_text")   # 字面名字, 比如 "Alice" / "凤凰项目"

    if not (who_id or who_text):
        return {"error": "must provide who_id or who_text"}

    mode = params.get("mode", "both")  # "author" | "mention" | "both"
    # mode 和输入不配套时,三个分支会全部跳过,返回空结果 —— 调用方的 LLM
    # 没法区分"没数据"和"查询本身是 no-op"。响亮拒绝。
    if mode not in ("author", "mention", "both"):
        return {"error": f"unknown mode: {mode!r} (valid: author / mention / both)"}
    if mode == "author" and not who_id:
        return {"error": "mode=author requires who_id"}
    if mode == "mention" and not who_text:
        return {"error": "mode=mention requires who_text"}

    candidates_n = params.get("candidates", settings.default_candidates * 2)

    # 语义召回的查询向量在拿池连接**之前**算 —— embed 是外部 HTTP(超时 120s),
    # 拿着池连接干等会把池抽干(semantic.py 同款顺序)。
    qvec = None
    if mode in ("mention", "both") and who_text:
        try:
            qvecs = await embedder.embed(who_text, is_query=True)
            qvec = format_vector(qvecs[0])
        except Exception as e:
            logger.warning(f"semantic recall failed: {e}")

    # 累积候选 (按 message_id 去重)
    candidates: dict[str, dict] = {}

    pool = get_pool()
    async with pool.acquire() as conn:

        # --- 模式 A: author ---
        if mode in ("author", "both") and who_id:
            where, sql_params, _ = build_filter_sql(filters, starting_param_idx=2)
            sql = f"""
                SELECT
                    message_id, company_id, session_id, user_id, worker_id,
                    timestamp, received_at, role, content, channel_id, lang,
                    1.0 as score_author
                FROM messages
                WHERE user_id = $1 AND ({where})
                ORDER BY timestamp DESC
                LIMIT {int(candidates_n)}
            """
            rows = await conn.fetch(sql, who_id, *sql_params)
            for r in rows:
                candidates.setdefault(r["message_id"], dict(r))["match_type"] = "author"

        # --- 模式 B: mention 字面匹配 ---
        if mode in ("mention", "both") and who_text:
            where, sql_params, next_idx = build_filter_sql(filters, starting_param_idx=2)
            # ILIKE 模糊匹配 (前后加 %)
            pattern = f"%{who_text}%"
            sql = f"""
                SELECT
                    message_id, company_id, session_id, user_id, worker_id,
                    timestamp, received_at, role, content, channel_id, lang,
                    1.0 as score_literal
                FROM messages
                WHERE content ILIKE $1 AND ({where})
                ORDER BY timestamp DESC
                LIMIT {int(candidates_n)}
            """
            try:
                rows = await conn.fetch(sql, pattern, *sql_params)
                for r in rows:
                    if r["message_id"] in candidates:
                        candidates[r["message_id"]]["match_type"] += "+literal"
                        # 不带这行,author+literal 的行 literal_hit 返 None,
                        # 且 _fallback_sort 把最强的候选(本人发的+字面命中)
                        # 漏算 0.8 分。
                        candidates[r["message_id"]]["score_literal"] = r["score_literal"]
                    else:
                        d = dict(r)
                        d["match_type"] = "literal"
                        candidates[r["message_id"]] = d
            except Exception as e:
                logger.warning(f"literal match failed: {e}")

        # --- 模式 C: mention 语义召回 (跨语言、变体) ---
        if qvec is not None:
            try:
                where, sql_params, _ = build_filter_sql(filters, starting_param_idx=2)
                sql = f"""
                    SELECT
                        message_id, company_id, session_id, user_id, worker_id,
                        timestamp, received_at, role, content, channel_id, lang,
                        1 - (embedding <=> $1::vector) as score_semantic
                    FROM messages
                    WHERE embedding IS NOT NULL AND ({where})
                    ORDER BY embedding <=> $1::vector
                    LIMIT {int(candidates_n)}
                """
                rows = await conn.fetch(sql, qvec, *sql_params)
                for r in rows:
                    # 阈值: 语义相似度 > 0.3 才纳入 (避免完全不相关)
                    if r["score_semantic"] < 0.3:
                        continue
                    if r["message_id"] in candidates:
                        if "semantic" not in candidates[r["message_id"]]["match_type"]:
                            candidates[r["message_id"]]["match_type"] += "+semantic"
                            candidates[r["message_id"]]["score_semantic"] = r["score_semantic"]
                    else:
                        d = dict(r)
                        d["match_type"] = "semantic"
                        candidates[r["message_id"]] = d
            except Exception as e:
                logger.warning(f"semantic recall failed: {e}")

    if not candidates:
        return {
            "query_type": "entity_mentions",
            "context": [],
            "metadata": {"result_count": 0, "who_id": who_id, "who_text": who_text},
        }

    cand_list = list(candidates.values())

    # --- 用 reranker 精排 (如果开启且 who_text 存在) ---
    if settings.reranker_enabled and who_text and len(cand_list) > 1:
        try:
            rerank_result = await reranker.rerank(
                query=who_text,
                documents=[c["content"] for c in cand_list],
                top_n=limit,
            )
            ordered = [(cand_list[r["index"]], r["score"]) for r in rerank_result]
        except Exception as e:
            logger.warning(f"rerank failed, falling back: {e}")
            ordered = _fallback_sort(cand_list, limit)
    else:
        ordered = _fallback_sort(cand_list, limit)

    # --- 拼结果 ---
    context = []
    for cand, final_score in ordered:
        context.append({
            "type": "message",
            "content": cand["content"],
            "metadata": {
                "message_id": cand["message_id"],
                "company_id": cand["company_id"],
                "session_id": cand["session_id"],
                "user_id": cand["user_id"],
                "worker_id": cand["worker_id"],
                "timestamp": cand["timestamp"].isoformat(),
                "received_at": cand["received_at"].isoformat() if cand["received_at"] else None,
                "role": cand["role"],
                "channel_id": cand["channel_id"],
                "lang": cand["lang"],
            },
            "match": {
                "type": cand["match_type"],  # author / literal / semantic / 组合
                "final_score": float(final_score),
                "literal_hit": cand.get("score_literal"),
                "semantic_score": cand.get("score_semantic"),
                "author_hit": cand.get("score_author"),
            },
        })

    return {
        "query_type": "entity_mentions",
        "context": context,
        "metadata": {
            "result_count": len(context),
            "candidates_pooled": len(cand_list),
            "who_id": who_id,
            "who_text": who_text,
            "mode": mode,
        },
    }


def _fallback_sort(cands: list[dict], limit: int) -> list[tuple[dict, float]]:
    """没有 reranker 时的兜底排序: 综合各种 score"""
    def score(c):
        s = 0.0
        s += c.get("score_author") or 0  # 自己发的最重要
        s += (c.get("score_literal") or 0) * 0.8
        s += (c.get("score_semantic") or 0) * 0.6
        return s
    sorted_cands = sorted(cands, key=lambda c: -score(c))
    return [(c, score(c)) for c in sorted_cands[:limit]]
