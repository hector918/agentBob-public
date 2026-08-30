"""所有配置从环境变量读取"""
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    # PostgreSQL
    database_url: str
    db_pool_min: int = 5
    db_pool_max: int = 20

    # Embedding 服务 (OpenAI 兼容 /v1/embeddings;llama.cpp --embeddings)
    # ⚠ embedder_model + embedder_dimensions 是冻结依赖: 换了 = 所有
    # 历史向量作废,要全量重算。一开始就定死,别中途改。
    embedder_base_url: str
    embedder_api_key: str = "dummy"
    embedder_model: str = "qwen3-embedding-0.6b-q8_0.gguf"  # 单模型 llama-server 仅作日志
    embedder_dimensions: int = 1024
    embedder_timeout: float = 120.0  # CPU 推理慢(~2s/条),给足;否则一个 chunk 就超时
    # embedder 限额:单请求最多几条,单请求 token 预算(留 margin),单条最长字符。
    # 超过自动分块/截断 —— 否则长输入超后端物理批(-ub)整批被拒。
    embed_batch_size: int = 32
    embed_max_batch_tokens: int = 7000
    embed_max_chars: int = 2000      # 仅影响 embedding 向量;存储/召回的是全文,不受截断

    # Reranker 服务 (llama.cpp /rerank,Cohere/Jina 风格)
    reranker_base_url: str
    reranker_api_key: str = "dummy"
    reranker_model: str = "qwen3-reranker-0.6b-q8_0.gguf"  # 单模型 llama-server 仅作日志
    reranker_timeout: float = 120.0  # CPU 推理慢,给足
    reranker_enabled: bool = True  # 关闭时跳过 rerank, 直接用向量分
    rerank_batch_size: int = 32    # 每请求候选条数上限(给后端可控的单请求规模)

    # 应用
    log_level: str = "info"
    # 数据面鉴权(/messages* + /retrieve):设置后要求请求头 X-API-Key 一致。
    # 空 = 不鉴权(沿用 LAN 网络层信任)。/health、/selftest 始终开放(docker
    # healthcheck 和探活不带密钥)。
    api_key: str = ""

    # 检索默认参数
    default_candidates: int = 30   # 向量召回多少候选
    default_top_n: int = 10        # rerank 后返回多少

    # HNSW 检索参数 (pgvector 0.8+); 带 company 分区裁剪后仍兜公司内子过滤
    hnsw_iterative_scan: str = "strict_order"  # strict_order | relaxed_order | off
    hnsw_ef_search: int = 100


settings = Settings()
