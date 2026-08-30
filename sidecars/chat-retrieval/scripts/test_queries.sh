#!/bin/bash
# 测试各 query_type 是否工作
# 用法: bash scripts/test_queries.sh

set -e
BASE=${BASE_URL:-http://localhost:8000}

echo "=== 健康检查 ==="
curl -sf $BASE/health && echo " ✅"

echo
echo "=== 1. entity_mentions: 查 Hans ==="
curl -sf -X POST $BASE/retrieve \
  -H "Content-Type: application/json" \
  -d '{
    "query_type": "entity_mentions",
    "params": {"who_id": "hans", "who_text": "Hans", "mode": "both"},
    "filters": {"company_id": "demo_co"},
    "limit": 5
  }' | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  返回 {d[\"metadata\"][\"result_count\"]} 条消息')"

echo
echo "=== 2. entity_mentions: 查凤凰项目 (跨语言: 也应命中 Phoenix) ==="
curl -sf -X POST $BASE/retrieve \
  -H "Content-Type: application/json" \
  -d '{
    "query_type": "entity_mentions",
    "params": {"who_text": "凤凰项目", "mode": "mention"},
    "filters": {"company_id": "demo_co"},
    "limit": 10
  }' | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  返回 {d[\"metadata\"][\"result_count\"]} 条消息'); [print(f'    [{m[\"match\"][\"type\"]}] {m[\"content\"][:60]}') for m in d['context'][:5]]"

echo
echo "=== 3. time_window: 11 月凤凰频道 ==="
curl -sf -X POST $BASE/retrieve \
  -H "Content-Type: application/json" \
  -d '{
    "query_type": "time_window",
    "params": {"scope": "channel"},
    "filters": {
      "company_id": "demo_co",
      "channel_id": "ch_phoenix",
      "time_range": ["2024-11-01", "2024-12-01"]
    }
  }' | python3 -c "import sys,json; d=json.load(sys.stdin); ag=d['aggregations']; print(f'  总消息: {ag[\"total_messages\"]}, 用户: {ag[\"unique_users\"]}, 消息切片: {len(d[\"context\"][\"messages\"])}')"

echo
echo "=== 4. semantic: 语义检索 'API 接口设计' ==="
curl -sf -X POST $BASE/retrieve \
  -H "Content-Type: application/json" \
  -d '{
    "query_type": "semantic",
    "params": {"query_text": "API 接口设计"},
    "filters": {"company_id": "demo_co"},
    "limit": 5
  }' | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  返回 {d[\"metadata\"][\"result_count\"]} 条消息'); [print(f'    [{m[\"metadata\"][\"lang\"]}] score={m[\"scores\"][\"final\"]:.3f}: {m[\"content\"][:60]}') for m in d['context'][:3]]"

echo
echo "=== 5. time_window: 某公司某时段总览 (消息切片 + 聚合) ==="
curl -sf -X POST $BASE/retrieve \
  -H "Content-Type: application/json" \
  -d '{
    "query_type": "time_window",
    "params": {},
    "filters": {"company_id": "demo_co", "time_range": ["2024-09-01", "2024-12-31"]},
    "limit": 5
  }' | python3 -c "import sys,json; d=json.load(sys.stdin); a=d['aggregations']; print(f'  共 {a[\"total_messages\"]} 条 / {a[\"unique_users\"]} 人; 返回 {len(d[\"context\"][\"messages\"])} 条样例')"

echo
echo "✅ 所有测试通过"
