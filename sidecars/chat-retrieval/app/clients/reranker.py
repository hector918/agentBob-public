"""调用外部 Reranker 服务 (llama.cpp /rerank,Cohere/Jina 风格)。

接口适配层 —— 调用方拿到的永远是 [{"index", "score"}],与后端无关(decouple:
功能一致,只是 wire 接口从 TEI 换成了 llama.cpp)。后端是 llama-server 起的
Qwen3-Reranker GGUF(--reranking),内置/外置同一协议,只是 URL 不同。
"""
import httpx
from config import settings

# 单条送入 rerank 的字符上限。Qwen3-Reranker 由 llama-server 起(-c 8192);一条超长
# 文档会让 llama.cpp 拒掉整个 /rerank 请求(不像 TEI 有 truncate),那会把整次查询打回
# 向量序。截断只影响打分输入,召回返回的 content 仍是全文。需要时提升为 config 项。
_RERANK_MAXCHARS = 6000


class RerankerClient:
    def __init__(self):
        self.client = httpx.AsyncClient(
            base_url=settings.reranker_base_url.rstrip("/"),
            headers={"Authorization": f"Bearer {settings.reranker_api_key}"},
            timeout=settings.reranker_timeout,
        )

    async def rerank(
        self,
        query: str,
        documents: list[str],
        top_n: int = 10,
    ) -> list[dict]:
        """
        返回: [{"index": int, "score": float}, ...] 按 score 降序(对调用方稳定的契约)。
        """
        if not documents:
            return []

        merged: list[dict] = []
        # 分块 + 带 offset 合并:rerank 分数是 (query, doc) 逐对独立的,分块不改语义。
        # 保留分块是给后端一个可控的单请求上限(大候选集不一次性砸过去);最终全局
        # re-sort 后取 top_n。
        bs = max(1, settings.rerank_batch_size)
        q = query[:_RERANK_MAXCHARS]
        for off in range(0, len(documents), bs):
            chunk = [d[:_RERANK_MAXCHARS] for d in documents[off : off + bs]]
            response = await self.client.post(
                "/rerank",
                json={
                    "model": settings.reranker_model,  # 单模型 llama-server 仅作日志
                    "query": q,
                    "documents": chunk,
                },
            )
            response.raise_for_status()
            data = response.json()
            # llama.cpp 返回 {"results": [{"index", "relevance_score"}]};容忍裸数组 /
            # "score" 命名。缺 index 或分数的条目跳过 —— 真正的降级(而非崩在下面排序里)。
            items = data["results"] if isinstance(data, dict) and "results" in data else data
            for r in items:
                idx = r.get("index")
                score = r.get("relevance_score")
                if score is None:
                    score = r.get("score")
                if idx is None or score is None:
                    continue
                merged.append({"index": idx + off, "score": score})
        merged.sort(key=lambda r: -r["score"])
        return merged[:top_n]

    async def close(self):
        await self.client.aclose()


reranker = RerankerClient()
