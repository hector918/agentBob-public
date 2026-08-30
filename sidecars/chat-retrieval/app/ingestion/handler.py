"""POST /messages - 写入消息"""
import logging
from datetime import datetime
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from clients.embedder import embedder
from clients.db import get_pool, format_vector, ensure_partition

logger = logging.getLogger(__name__)
router = APIRouter()


class MessageIn(BaseModel):
    message_id: str = Field(..., description="消息唯一 ID")
    company_id: str = Field(..., description="公司 scope / 分区键; 无公司 (outer-bob) 用 '__personal__'")
    session_id: str = Field(..., description="所属会话 ID")
    user_id: str = Field(..., description="不可变 source_handle (account 由调用方查询时解析)")
    timestamp: datetime = Field(..., description="source 声明时间 (UTC, 可被客户端伪造)")
    received_at: datetime | None = Field(None, description="调用方权威收到时间 (pg 校准时钟)")
    role: str = Field(..., description="user / assistant / system")
    # min_length=1:空串会让整批 embed 被 TEI 拒,批量端点对同一批重试永远
    # 失败(毒丸)。422 在门口拒掉 —— 调用方(bob)对 422 走 dead-letter 不重试。
    content: str = Field(..., min_length=1, description="消息文本 (调用方已 scrub,非空)")
    lang: str | None = Field(None, description="语言标签,可选(不填则存 NULL,不做推测)")

    # agora / 灵活 metadata
    channel_id: str | None = Field(None, description="agora inbox id")
    worker_id: str | None = Field(None, description="接手的 agora worker; 空 = generic bob / 非 agora")
    thread_id: str | None = None

    # per-message 派生标签 (sweep 蒸馏时一并抽)
    sentiment: str | None = None
    is_commitment: bool | None = None
    intent: str | None = None

    user_attributes: dict | None = Field(None, description="部门/职位/地区 等用户属性")
    session_attributes: dict | None = Field(None, description="频道名/参与者 等会话属性")
    extra: dict | None = None


class BatchIn(BaseModel):
    messages: list[MessageIn]


@router.post("/messages", tags=["ingestion"])
async def ingest_message(msg: MessageIn):
    """单条入库 —— **同步**,= /messages/batch 的 N=1。理由同 batch 的
    docstring:202+后台任务是静默丢数据(embed/DB 一失败消息就没了,调用方
    却已拿到 2xx);单条没有任何需要异步的延迟。"""
    try:
        await _process_batch([msg])
    except Exception as e:  # noqa: BLE001
        logger.exception("single ingest failed")
        raise HTTPException(500, f"ingest failed: {e}")
    return {"status": "stored", "message_id": msg.message_id}


@router.post("/messages/batch", tags=["ingestion"])
async def ingest_batch(batch: BatchIn):
    """批量入库 —— **同步**: embed + 落库都成功才返回 200。

    不能用 202 异步:调用方(bob)的 drainer 把 2xx 当"已持久化"→删自己的
    outbox 行;若后台处理在 202 之后失败,数据永久丢且不重试。同步 + 失败返
    500 让 bob 留着行下轮重推(叶子幂等,重推安全)。"""
    if not batch.messages:
        raise HTTPException(400, "empty batch")
    if len(batch.messages) > 200:
        raise HTTPException(400, "batch too large (max 200)")
    try:
        await _process_batch(batch.messages)
    except Exception as e:  # noqa: BLE001
        logger.exception("batch ingest failed")
        raise HTTPException(500, f"ingest failed: {e}")
    return {"status": "stored", "count": len(batch.messages)}


@router.get("/messages/{message_id}", tags=["ingestion"])
async def get_message(message_id: str, company_id: str):
    """按 (company_id, message_id) 取回单条 —— 调用方(bob)碰不到 DB,用这个
    验证某条 ingest 是否真落库(ingest→persist→read 往返)。

    **company_id 必填**:命中单分区(分区裁剪)+ 限定单公司,避免"按 id 跨公司
    读"成为绕过 scope 隔离的口子。不存在 → 404。"""
    pool = get_pool()
    async with pool.acquire() as conn:
        row = await conn.fetchrow(
            """
            SELECT message_id, company_id, session_id, user_id, role, content,
                   timestamp, received_at, channel_id, worker_id, lang
              FROM messages WHERE company_id = $1 AND message_id = $2
            """,
            company_id, message_id,
        )
    if row is None:
        raise HTTPException(404, f"message {message_id!r} not found in company {company_id!r}")
    d = dict(row)
    for k in ("timestamp", "received_at"):
        if d.get(k) is not None:
            d[k] = d[k].isoformat()
    return d


async def _process_batch(msgs: list[MessageIn]):
    """embed → ensure 分区 → 事务内批量 INSERT。**失败抛出**(不吞)—— 让同步的
    /messages/batch 据此返 500,调用方留 outbox 重试。"""
    # 1. 批量算 embedding (一次调用搞定多条)
    embeddings = await embedder.embed(
        [m.content for m in msgs],
        is_query=False,
    )

    pool = get_pool()
    async with pool.acquire() as conn:
        # 2. 确保涉及的公司分区都已建好 (DDL, 在事务外做, 避免与
        #    insert 一起在父表上长锁)
        for company_id in {m.company_id for m in msgs}:
            await ensure_partition(conn, company_id)

        # 3. 批量写入
        async with conn.transaction():
            # strict: embedder 已校验条数,这里兜底 —— 数量错配宁可炸也不能
            # 静默丢尾巴还返 200(调用方会删 outbox,数据永久丢)。
            for msg, emb in zip(msgs, embeddings, strict=True):
                await conn.execute(
                    """
                    INSERT INTO messages (
                        message_id, company_id, session_id, user_id,
                        timestamp, received_at, role, content,
                        embedding, lang, channel_id, worker_id, thread_id,
                        sentiment, is_commitment, intent,
                        user_attributes, session_attributes, extra
                    ) VALUES (
                        $1, $2, $3, $4, $5, $6, $7, $8,
                        $9::vector, $10, $11, $12, $13,
                        $14, $15, $16, $17, $18, $19
                    )
                    ON CONFLICT (company_id, message_id) DO NOTHING
                    """,
                    msg.message_id,
                    msg.company_id,
                    msg.session_id,
                    msg.user_id,
                    msg.timestamp,
                    msg.received_at,
                    msg.role,
                    msg.content,
                    format_vector(emb),
                    msg.lang,
                    msg.channel_id,
                    msg.worker_id,
                    msg.thread_id,
                    msg.sentiment,
                    msg.is_commitment,
                    msg.intent,
                    msg.user_attributes,
                    msg.session_attributes,
                    msg.extra,
                )
    logger.info(f"ingested {len(msgs)} messages")
