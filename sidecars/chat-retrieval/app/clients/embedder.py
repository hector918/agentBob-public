"""调用外部 Embedding 服务 (OpenAI 兼容协议)"""
import httpx
from typing import Union
from config import settings


class EmbedderClient:
    def __init__(self):
        self.client = httpx.AsyncClient(
            base_url=settings.embedder_base_url.rstrip("/"),
            headers={"Authorization": f"Bearer {settings.embedder_api_key}"},
            timeout=settings.embedder_timeout,
        )

    async def embed(
        self,
        texts: Union[str, list[str]],
        is_query: bool = False,
    ) -> list[list[float]]:
        """
        is_query=True  -> 查询场景, 加 Qwen3 instruction prefix (推荐)
        is_query=False -> 文档/消息场景, 直接 embed
        """
        if isinstance(texts, str):
            texts = [texts]

        if is_query:
            texts = [
                f"Instruct: Given a question, retrieve relevant chat messages "
                f"that answer the question.\nQuery: {t}"
                for t in texts
            ]

        # 单条截断,防单条超 embedder 输入上限。
        if settings.embed_max_chars > 0:
            texts = [t[: settings.embed_max_chars] for t in texts]

        dim = settings.embedder_dimensions
        out: list[list[float]] = []
        # 按"条数(TEI max-client-batch-size)+ token 预算(max-batch-tokens)"分块,
        # 否则一整批(几十~上百条 / 含长消息)会被 TEI 整批拒。
        for chunk in _chunks(texts, settings.embed_batch_size, settings.embed_max_batch_tokens):
            response = await self.client.post(
                "/embeddings",
                json={
                    "model": settings.embedder_model,
                    "input": chunk,
                    # 请求里带 dimensions 让服务端按 MRL 截断到目标维度。
                    "dimensions": settings.embedder_dimensions,
                },
            )
            response.raise_for_status()
            data = response.json()
            # 客户端兜底:`dimensions` 服务端不一定认(忽略→返原生维度),与 schema 的
            # vector(N) 不符则每条 INSERT 失败。这里再截一刀(MRL 前缀仍合法,cosine
            # scale-invariant 无需重归一化);比期望短=配错模型,直接报错别写脏。
            # OpenAI 协议不保证返回顺序(所以才有 index 字段)——乱序会把 A 的
            # 向量配到 B 的消息上(静默语料损坏);少返会让 zip 丢尾巴还返 200。
            items = sorted(data["data"], key=lambda x: x["index"])
            if len(items) != len(chunk):
                raise ValueError(
                    f"embedder 返回 {len(items)} 条 != 请求 {len(chunk)} 条"
                )
            for item in items:
                emb = item["embedding"]
                if len(emb) < dim:
                    raise ValueError(
                        f"embedder 返回 {len(emb)} 维 < 期望 {dim} —— 模型/配置不对?"
                    )
                out.append(emb[:dim])
        return out

    async def probe_raw_dim(self) -> int:
        """返回服务端**原生**返回的维度(不截断)—— 给 /selftest 看服务端到底认不认
        请求里的 `dimensions`。==期望=完美;>期望=被忽略、靠客户端截断(MRL 模型 OK,
        非 MRL 模型会出错);<期望=配错模型。"""
        response = await self.client.post(
            "/embeddings",
            json={
                "model": settings.embedder_model,
                "input": ["dimension probe"],
                "dimensions": settings.embedder_dimensions,
            },
        )
        response.raise_for_status()
        return len(response.json()["data"][0]["embedding"])

    async def close(self):
        await self.client.aclose()


def _chunks(texts: list[str], max_count: int, max_tokens: int):
    """把 texts 贪心切块:每块 ≤ max_count 条,且 token 估算 ≤ max_tokens。
    估算用 UTF-8 字节数 —— 对任何 byte-level BPE 都是保证上界(1 token ≥ 1 字节);
    按字符数估会低估 emoji/生僻字(1 字符可拆 4 token),超 TEI 限额整批被拒,
    而调用方对同一批的重试会永远失败。代价是 CJK 多切几块,正确性优先。"""
    chunk: list[str] = []
    toks = 0
    for t in texts:
        tt = len(t.encode("utf-8"))
        if chunk and (len(chunk) >= max_count or toks + tt > max_tokens):
            yield chunk
            chunk, toks = [], 0
        chunk.append(t)
        toks += tt
    if chunk:
        yield chunk


embedder = EmbedderClient()
