-- =============================================================
-- Chat Retrieval System - PostgreSQL Schema
-- 需要 PostgreSQL 16+ 和 pgvector 0.8+ (用到 hnsw.iterative_scan)
--
-- 本 schema 已按 agentbob 集成决策调整 (2026-06):
--   * messages 按 company_id LIST 分区 (公司数 ≤ ~100)。解决"中等
--     选择性的元数据过滤 + 全局 HNSW 欠召回",并让跨公司隔离成为
--     结构性保证 (一次查询物理上只进自己公司的分区)。
--   * 身份键 = 不可变 source_handle (列名沿用 user_id)。account 是
--     可变的 (认领/合并),所以语料只存 handle;调用方 (bob) 在查询时
--     把 account 展开成 handle 列表传入 user_ids。
--   * 双时间戳: timestamp = source 声明时间 (可被客户端伪造);
--     received_at = 调用方权威收到时间 (pg 校准时钟)。
--   * per-message 派生标签: sentiment / is_commitment / intent
--     (调用方在 sweep 蒸馏时一并抽,随消息一起灌入)。
--   * 离线话题表 (topic_snapshots / message_topics) 已移除。话题/
--     聚合改由调用方"检索 + LLM 现场综述"完成;本系统只做检索。
--   * append-only: 不删不改 (撤回/编辑不回退);进库前由调用方 scrub。
-- =============================================================

-- 扩展
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- =============================================================
-- 主表: 消息 (按 company_id LIST 分区)
--
-- 新公司的分区由应用 ingest 路径按需创建 (ensure_partition):
--   CREATE TABLE messages_p_<hash> PARTITION OF messages
--     FOR VALUES IN ('<company_id>');
--   CREATE INDEX ... ON messages_p_<hash> USING hnsw (embedding ...);
-- 这里只预建 outer-bob (无公司) 的 '__personal__' 分区。
-- =============================================================
CREATE TABLE IF NOT EXISTS messages (
    -- 业务侧唯一 ID (不用数据库自增)
    message_id TEXT NOT NULL,

    -- 分区键 / scope: 公司 (outer-bob 无公司消息用 '__personal__')
    company_id TEXT NOT NULL,

    -- 上下文
    session_id TEXT NOT NULL,
    -- 身份: 不可变 source_handle (account 由调用方查询时解析展开)
    user_id TEXT NOT NULL,
    channel_id TEXT,                       -- agora inbox (会话室)
    worker_id  TEXT,                       -- 当时接手的 agora worker; NULL = generic bob / 非 agora
    thread_id  TEXT,

    -- 时间: source 声明时间 + 权威收到时间
    timestamp   TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ,

    role TEXT NOT NULL,                    -- user / assistant / system
    content TEXT NOT NULL,
    lang TEXT,

    -- 向量 (Qwen3-Embedding-0.6B, 1024 维)
    -- ⚠ 冻结依赖: 换 embedding 模型/维度 = 所有历史向量作废重算。
    embedding vector(1024),

    -- per-message 派生标签 (sweep 蒸馏时一并抽)
    sentiment     TEXT,                    -- e.g. neg / neu / pos
    is_commitment BOOLEAN,                 -- 承诺/待办形状
    intent        TEXT,                    -- 短意图标签

    -- 可选 metadata
    user_attributes JSONB,                 -- {"department": "eng", ...}
    session_attributes JSONB,
    extra JSONB,                           -- 其余标签 / 自定义字段

    -- 入库本系统的时间 (审计用; ≠ received_at)
    inserted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- 分区表主键必须包含分区键
    PRIMARY KEY (company_id, message_id)
) PARTITION BY LIST (company_id);

-- outer-bob (无公司) 分区预建
CREATE TABLE IF NOT EXISTS messages_personal
    PARTITION OF messages FOR VALUES IN ('__personal__');

-- ---- 普通索引: 建在父表 → 自动传播到现有及未来分区 ----
CREATE INDEX IF NOT EXISTS idx_msg_user_time
    ON messages (user_id, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_msg_channel_time
    ON messages (channel_id, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_msg_worker_time
    ON messages (worker_id, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_msg_session
    ON messages (session_id, timestamp);

CREATE INDEX IF NOT EXISTS idx_msg_timestamp
    ON messages (timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_msg_user_attrs
    ON messages USING GIN (user_attributes);

CREATE INDEX IF NOT EXISTS idx_msg_content_trgm
    ON messages USING GIN (content gin_trgm_ops);

-- ---- 向量索引 (HNSW): 按分区建 ----
-- pgvector 的 HNSW 索引按分区单独建 (不建在父表),每个分区一棵。
-- 带 company_id filter 的查询经分区裁剪只命中单分区的 HNSW → 召回正确。
-- 新分区的 HNSW 由 ensure_partition 创建;这里建 '__personal__' 的。
CREATE INDEX IF NOT EXISTS messages_personal_emb
    ON messages_personal USING hnsw (embedding vector_cosine_ops);
