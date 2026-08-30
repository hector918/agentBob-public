"""检索 handler 共用工具"""
from datetime import datetime
from typing import Any


class FilterError(ValueError):
    """filters 形状不合法 —— api 层据此返 400(调用方的查询错误),
    而不是 500(会被调用方当服务故障走 fail-open/重试)。"""


def build_filter_sql(
    filters: dict | None,
    starting_param_idx: int = 1,
) -> tuple[str, list[Any], int]:
    """
    根据 filters dict 生成 WHERE 子句和参数

    支持的 filters:
      - company_id: str / company_ids: list[str]  (分区键; 放最前以触发分区裁剪)
      - user_id: str / user_ids: list[str]         (= 不可变 source_handle)
      - worker_id: str / worker_ids: list[str]     (接手的 agora worker)
      - channel_id: str / channel_ids: list[str]   (= agora inbox)
      - session_id: str
      - lang: str
      - role: str
      - sentiment: str / is_commitment: bool / intent: str  (派生标签)
      - time_range: [start, end]  (任一可为 None)
      - user_attributes: {key: value}  (JSONB 过滤)

    返回: (where_sql, params, next_param_idx)
    """
    if not filters:
        return "TRUE", [], starting_param_idx

    clauses = []
    params = []
    idx = starting_param_idx

    def add_eq(key: str, col: str):
        """标量等值;空串照常绑参(= '' 自然匹配不到 = fail-closed)。"""
        nonlocal idx
        v = filters.get(key)
        if v is None:
            return
        params.append(v)
        clauses.append(f"{col} = ${idx}")
        idx += 1

    def add_any(key: str, col: str):
        """列表 = ANY。**空列表 ≠ 缺省**:调用方把 account 解析成 0 个
        handle 时语义是"一个都不匹配";按 falsy 丢掉子句会反转成"匹配
        全部"(company_ids=[] 时 = 跨全部公司分区扫描)。fail-closed。"""
        nonlocal idx
        vs = filters.get(key)
        if vs is None:
            return
        if not vs:
            clauses.append("FALSE")
            return
        params.append(list(vs))
        clauses.append(f"{col} = ANY(${idx})")
        idx += 1

    # company 放最前 —— 是分区键,触发 LIST 分区裁剪 (只扫该公司分区)
    add_eq("company_id", "company_id")
    add_any("company_ids", "company_id")
    add_eq("worker_id", "worker_id")
    add_any("worker_ids", "worker_id")

    if (v := filters.get("sentiment")) is not None:
        params.append(v)
        clauses.append(f"sentiment = ${idx}")
        idx += 1

    if (v := filters.get("is_commitment")) is not None:
        params.append(bool(v))
        clauses.append(f"is_commitment = ${idx}")
        idx += 1

    if (v := filters.get("intent")) is not None:
        params.append(v)
        clauses.append(f"intent = ${idx}")
        idx += 1

    add_eq("user_id", "user_id")
    add_any("user_ids", "user_id")
    add_eq("channel_id", "channel_id")
    add_any("channel_ids", "channel_id")
    add_eq("session_id", "session_id")
    add_eq("lang", "lang")
    add_eq("role", "role")

    if tr := filters.get("time_range"):
        start, end = _time_range_bounds(tr)
        if start:
            params.append(_parse_dt(start))
            clauses.append(f"timestamp >= ${idx}")
            idx += 1
        if end:
            params.append(_parse_dt(end))
            clauses.append(f"timestamp < ${idx}")
            idx += 1

    if attrs := filters.get("user_attributes"):
        for k, v in attrs.items():
            params.append({k: v})
            clauses.append(f"user_attributes @> ${idx}::jsonb")
            idx += 1

    if not clauses:
        return "TRUE", [], idx

    return " AND ".join(clauses), params, idx


def _time_range_bounds(tr) -> tuple:
    """time_range 的两种合法形状 → (start, end);形状不对抛 FilterError
    (以前直接 tuple-unpack / .get,长度不对或传了字符串会炸成 500)。"""
    if isinstance(tr, (list, tuple)):
        if len(tr) != 2:
            raise FilterError(f"time_range list must be [start, end], got {len(tr)} items")
        return tr[0], tr[1]
    if isinstance(tr, dict):
        return tr.get("start"), tr.get("end")
    raise FilterError("time_range must be [start, end] or {start, end}")


def _parse_dt(v) -> datetime:
    if isinstance(v, datetime):
        return v
    from dateutil import parser
    try:
        return parser.parse(v)
    except (ValueError, TypeError, OverflowError) as e:
        raise FilterError(f"unparseable time_range value {v!r}: {e}")


def serialize_row(row) -> dict:
    """把 asyncpg Row 转成可 JSON 序列化的 dict"""
    result = {}
    for k, v in dict(row).items():
        if isinstance(v, datetime):
            result[k] = v.isoformat()
        else:
            result[k] = v
    return result
