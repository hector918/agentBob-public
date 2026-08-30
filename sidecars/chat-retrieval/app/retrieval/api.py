"""POST /retrieve - 统一检索入口, 按 query_type 路由"""
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from . import semantic, entity_mentions, time_window
from .utils import FilterError

router = APIRouter()


class RetrieveRequest(BaseModel):
    query_type: str = Field(
        ...,
        description="entity_mentions | time_window | semantic",
    )
    params: dict = Field(default_factory=dict, description="query_type 相关参数")
    filters: dict | None = Field(default=None, description="通用过滤条件")
    limit: int = Field(default=10, ge=1, le=100, description="返回结果数")


HANDLERS = {
    "semantic": semantic.handle,
    "entity_mentions": entity_mentions.handle,
    "time_window": time_window.handle,
}


@router.post("/retrieve", tags=["retrieval"])
async def retrieve(req: RetrieveRequest):
    """
    统一检索入口

    三种 query_type:

    1. **entity_mentions** - 查"某人/对象"相关内容
       params: {who_id?, who_text?, mode?: "author"|"mention"|"both"}

    2. **time_window** - 查某时段发生了什么 (消息切片 + 聚合统计)
       params: {time_range, query_text?}

    3. **semantic** - 通用语义检索
       params: {query_text, candidates?}

    话题演化/聚合不在本系统 —— 由调用方拿 time_window/entity 的结果做
    LLM 现场综述。

    filters 通用:
       company_id, company_ids, user_id, user_ids, worker_id, worker_ids,
       channel_id, channel_ids, session_id, lang, role,
       sentiment, is_commitment, intent, time_range, user_attributes
    """
    # 叶子侧兜底:company scope 必须显式给(调用方是 scope 权威,但它哪天
    # 丢了 filters,这里绝不能静默返回跨全部公司分区的数据)。同公司内的
    # "全库搜索" = 只给 company_id、不给其它 filter,不受影响。
    f = req.filters or {}
    if f.get("company_id") is None and f.get("company_ids") is None:
        raise HTTPException(
            400,
            "filters.company_id or filters.company_ids is required "
            "(use '__personal__' for non-company data)",
        )

    handler = HANDLERS.get(req.query_type)
    if not handler:
        raise HTTPException(
            400,
            f"unknown query_type: {req.query_type}. "
            f"valid: {list(HANDLERS.keys())}",
        )

    try:
        result = await handler(req.params, req.filters, req.limit)
        if isinstance(result, dict) and result.get("error"):
            raise HTTPException(400, result["error"])
        return result
    except HTTPException:
        raise
    except FilterError as e:
        # 查询形状错误是调用方的问题 —— 400 让它修查询,而不是 500 触发
        # 它的"服务故障"处理(fail-open / 重试)。
        raise HTTPException(400, str(e))
    except Exception as e:
        raise HTTPException(500, f"retrieval failed: {e}")
