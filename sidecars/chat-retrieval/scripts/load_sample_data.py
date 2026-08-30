"""
灌测试数据 - 模拟一个公司内部聊天场景, 包含中英德三语混杂消息.

跑法:
  pip install httpx
  python scripts/load_sample_data.py

会写入 ~30 条消息, 涉及:
  - 4 个用户 (alice, hans, marie, xiaoming)
  - 2 个频道 (ch_phoenix, ch_apollo)
  - 3 种语言
  - 跨越 3 个月时段 (让你能测时间窗口)
"""
import asyncio
import json
import sys
from datetime import datetime, timezone, timedelta
import httpx


BASE_URL = "http://localhost:8000"


# 用户档案
USERS = {
    "alice": {"department": "engineering", "title": "tech_lead", "location": "shanghai"},
    "hans":  {"department": "engineering", "title": "senior_engineer", "location": "berlin"},
    "marie": {"department": "product",     "title": "pm",              "location": "paris"},
    "xiaoming": {"department": "engineering", "title": "engineer",     "location": "shanghai"},
}


def msg(mid, sess, uid, ts, role, content, lang, channel, thread=None, company="demo_co"):
    return {
        "message_id": mid,
        "company_id": company,        # 分区键 (示例数据归一个 demo 公司)
        "session_id": sess,
        "user_id": uid,               # = source_handle
        "timestamp": ts.isoformat(),  # source 声明时间
        "received_at": ts.isoformat(),# 示例里等同 source 时间
        "role": role,
        "content": content,
        "lang": lang,
        "channel_id": channel,        # = agora inbox
        "thread_id": thread,
        "user_attributes": USERS[uid],
    }


def t(year, month, day, hour=10, minute=0):
    return datetime(year, month, day, hour, minute, tzinfo=timezone.utc)


# 一组消息样例
MESSAGES = [
    # === 2024-09: 凤凰项目启动期 ===
    msg("m001", "s_phx_001", "alice", t(2024, 9, 5),
        "user", "我们启动凤凰项目, 目标 11 月底交付", "zh", "ch_phoenix"),
    msg("m002", "s_phx_001", "marie", t(2024, 9, 5, 11),
        "user", "Phoenix project kickoff. We need to align on API design.",
        "en", "ch_phoenix"),
    msg("m003", "s_phx_002", "hans", t(2024, 9, 6),
        "user", "API-Design ist wichtig. Ich schlage REST vor.",
        "de", "ch_phoenix"),
    msg("m004", "s_phx_002", "xiaoming", t(2024, 9, 6, 14),
        "user", "我倾向 GraphQL, 前端需求多变", "zh", "ch_phoenix"),

    # === 2024-10: 设计讨论期 ===
    msg("m005", "s_phx_003", "hans", t(2024, 10, 8),
        "user", "Lass uns einen Kompromiss finden - REST for core, GraphQL for queries",
        "de", "ch_phoenix"),
    msg("m006", "s_phx_003", "alice", t(2024, 10, 8, 15),
        "user", "Hans 的方案不错, 我同意混合方式", "zh", "ch_phoenix"),
    msg("m007", "s_phx_004", "marie", t(2024, 10, 15),
        "user", "Documentation needs to be in English so all teams can read",
        "en", "ch_phoenix"),
    msg("m008", "s_phx_004", "xiaoming", t(2024, 10, 16),
        "user", "测试覆盖率需要至少 80%, 现在只有 60%", "zh", "ch_phoenix"),
    msg("m009", "s_phx_004", "hans", t(2024, 10, 17),
        "user", "Wir sollten mutation testing einführen", "de", "ch_phoenix"),

    # === 2024-11: 延期出现 ===
    msg("m010", "s_phx_005", "alice", t(2024, 11, 5),
        "user", "凤凰项目可能要延期, API 对接有问题", "zh", "ch_phoenix"),
    msg("m011", "s_phx_005", "hans", t(2024, 11, 6),
        "user", "Ich bestätige die Verzögerung von 2 Wochen", "de", "ch_phoenix"),
    msg("m012", "s_phx_005", "marie", t(2024, 11, 7),
        "user", "We need to communicate the delay to stakeholders ASAP",
        "en", "ch_phoenix"),
    msg("m013", "s_phx_006", "xiaoming", t(2024, 11, 15),
        "user", "Hans 推荐我看 Designing Data-Intensive Applications 这本书",
        "zh", "ch_phoenix"),
    msg("m014", "s_phx_006", "alice", t(2024, 11, 16),
        "user", "好书, 我也读过. Hans 总能推荐好书", "zh", "ch_phoenix"),

    # === 阿波罗项目频道 ===
    msg("m015", "s_apo_001", "marie", t(2024, 10, 3),
        "user", "Apollo project: focusing on ML pipeline this quarter",
        "en", "ch_apollo"),
    msg("m016", "s_apo_001", "alice", t(2024, 10, 4),
        "user", "阿波罗的 ML 流水线要不要也用 K8s", "zh", "ch_apollo"),
    msg("m017", "s_apo_002", "marie", t(2024, 11, 1),
        "user", "Customer feedback shows our model latency is too high",
        "en", "ch_apollo"),
    msg("m018", "s_apo_002", "xiaoming", t(2024, 11, 2),
        "user", "可以试试 model quantization, 牺牲一点精度换速度", "zh", "ch_apollo"),
    msg("m019", "s_apo_003", "hans", t(2024, 11, 20),
        "user", "Apollo project: we should also consider edge deployment",
        "en", "ch_apollo"),

    # === alice 的 1on1 session (假装跟 AI 助手聊) ===
    msg("m020", "s_alice_chat_01", "alice", t(2024, 11, 18),
        "user", "帮我整理一下凤凰项目最近的进展", "zh", None),
    msg("m021", "s_alice_chat_01", "alice", t(2024, 11, 18, 10, 5),
        "assistant",
        "根据频道讨论, 凤凰项目延期 2 周, Hans 提出的混合 API 方案被采纳, 测试覆盖率正在提升.",
        "zh", None),
    msg("m022", "s_alice_chat_01", "alice", t(2024, 11, 18, 10, 10),
        "user", "Hans 之前给我推荐过什么技术书来着", "zh", None),
    msg("m023", "s_alice_chat_01", "alice", t(2024, 11, 18, 10, 11),
        "assistant",
        "11 月 15 日 Hans 推荐了 Designing Data-Intensive Applications.",
        "zh", None),

    # === 跨语言提及"凤凰" / "Phoenix" 测试 ===
    msg("m024", "s_phx_007", "hans", t(2024, 11, 25),
        "user", "Phoenix delay - we need to revise the timeline",
        "en", "ch_phoenix"),
    msg("m025", "s_phx_007", "marie", t(2024, 11, 26),
        "user", "Phoenix-Projekt: ich erstelle eine Risiko-Analyse",
        "de", "ch_phoenix"),

    # === 12 月: 完工讨论 ===
    msg("m026", "s_phx_008", "alice", t(2024, 12, 10),
        "user", "凤凰项目终于上线了, 大家辛苦了", "zh", "ch_phoenix"),
    msg("m027", "s_phx_008", "hans", t(2024, 12, 10, 11),
        "user", "Glückwunsch! Lass uns post-mortem machen", "de", "ch_phoenix"),
    msg("m028", "s_phx_008", "marie", t(2024, 12, 11),
        "user", "Great work team! Let's plan the v2 roadmap",
        "en", "ch_phoenix"),

    # === 阿波罗 12 月 ===
    msg("m029", "s_apo_004", "xiaoming", t(2024, 12, 15),
        "user", "阿波罗模型 latency 优化完成, P99 降到 50ms", "zh", "ch_apollo"),
    msg("m030", "s_apo_004", "marie", t(2024, 12, 16),
        "user", "Excellent! Customer satisfaction should improve",
        "en", "ch_apollo"),
]


async def load():
    async with httpx.AsyncClient(timeout=60.0) as client:
        # 健康检查
        r = await client.get(f"{BASE_URL}/health")
        if r.status_code != 200:
            print(f"❌ API not healthy: {r.status_code}")
            sys.exit(1)
        print(f"✅ API healthy")

        # 批量灌入
        print(f"灌入 {len(MESSAGES)} 条消息...")
        r = await client.post(
            f"{BASE_URL}/messages/batch",
            json={"messages": MESSAGES},
        )
        if r.status_code != 200:
            print(f"❌ ingest failed: {r.status_code} {r.text}")
            sys.exit(1)
        print(f"✅ {r.json()}")

        print()
        print("数据灌完了, 试试这些查询:")
        print()
        print("# 1. 查 Hans 相关 (他自己说的 + 别人提到他的)")
        print("""curl -X POST http://localhost:8000/retrieve \\
  -H "Content-Type: application/json" \\
  -d '{"query_type":"entity_mentions","params":{"who_id":"hans","who_text":"Hans","mode":"both"},"filters":{"company_id":"demo_co"},"limit":10}' | jq""")
        print()
        print("# 2. 11 月份凤凰频道发生了什么")
        print("""curl -X POST http://localhost:8000/retrieve \\
  -H "Content-Type: application/json" \\
  -d '{"query_type":"time_window","params":{"scope":"channel"},"filters":{"company_id":"demo_co","channel_id":"ch_phoenix","time_range":["2024-11-01","2024-12-01"]}}' | jq""")
        print()
        print("# 3. 通用语义检索: 跨语言查 凤凰项目延期")
        print("""curl -X POST http://localhost:8000/retrieve \\
  -H "Content-Type: application/json" \\
  -d '{"query_type":"semantic","params":{"query_text":"凤凰项目延期"},"filters":{"company_id":"demo_co"},"limit":10}' | jq""")
        print()
        print("# 4. 对象检索: 跟 alice 相关的消息")
        print("""curl -X POST http://localhost:8000/retrieve \\
  -H "Content-Type: application/json" \\
  -d '{"query_type":"entity_mentions","params":{"who_id":"alice","mode":"both"},"filters":{"company_id":"demo_co"}}' | jq""")


if __name__ == "__main__":
    asyncio.run(load())
