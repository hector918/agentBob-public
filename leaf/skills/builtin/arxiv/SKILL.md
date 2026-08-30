---
name: arxiv
description: |
  按关键词 / 作者 / 类别 / ID 搜 arXiv 论文，拿引用 / 推荐 / 作者画像。
  Use when: 用户要找论文 / 查作者 / 查引用 / 查相关文献 / 生 BibTeX / 看 arXiv 某 ID 详情；学术研究类任务。
  Triggers: arxiv, 论文, paper, 学术, 引用, citation, BibTeX, semantic scholar, 综述, 文献, GRPO, transformer 等学术词.
  Do NOT use for: 非学术搜索（用 web_search_scrapling）；专利 / 法律文献。
---

# arXiv 研究 skill

通过 arXiv 免费 REST API 搜论文 + Semantic Scholar 拿引用关系。**无 API key、无 pip 依赖**，纯 stdlib `urllib.request` / `xml.etree.ElementTree`。

本技能附带的脚本/文件已就位于工作空间 `skills/arxiv/` 下，用 `terminal` 运行。

## 速查

| 操作 | 命令 |
|--------|---------|
| 搜论文（脚本）| `python3 skills/arxiv/scripts/search_arxiv.py "QUERY" --max 5` |
| 搜论文（API）| `curl -s "https://export.arxiv.org/api/query?search_query=all:QUERY&max_results=5"` |
| 取某论文 | `curl -s "https://export.arxiv.org/api/query?id_list=2402.03300"` |
| 读摘要（网页）| `web_search_scrapling(url="https://arxiv.org/abs/2402.03300", mode="get")` |
| 读全文（PDF）| `web_search_scrapling(url="https://arxiv.org/pdf/2402.03300", mode="get")` |

## Helper 脚本（首选，纯 stdlib 无 pip 依赖）

```bash
python3 skills/arxiv/scripts/search_arxiv.py "GRPO reinforcement learning"
python3 skills/arxiv/scripts/search_arxiv.py "transformer attention" --max 10 --sort date
python3 skills/arxiv/scripts/search_arxiv.py --author "Yann LeCun" --max 5
python3 skills/arxiv/scripts/search_arxiv.py --category cs.AI --sort date
python3 skills/arxiv/scripts/search_arxiv.py --id 2402.03300
python3 skills/arxiv/scripts/search_arxiv.py --id 2402.03300,2401.12345
```

## 直接打 API（需要自定义查询时）

arXiv API 返回 Atom XML。基础搜索：

```bash
curl -s "https://export.arxiv.org/api/query?search_query=all:GRPO+reinforcement+learning&max_results=5&sortBy=submittedDate&sortOrder=descending"
```

### 搜索语法

| 前缀 | 搜哪里 | 例 |
|--------|----------|---------|
| `all:` | 所有字段 | `all:transformer+attention` |
| `ti:` | 标题 | `ti:large+language+models` |
| `au:` | 作者 | `au:vaswani` |
| `abs:` | 摘要 | `abs:reinforcement+learning` |
| `cat:` | 类别 | `cat:cs.AI` |

布尔：AND 用 `+`、`OR`、`ANDNOT`；精确短语 `ti:"chain+of+thought"`；组合 `au:hinton+AND+cat:cs.LG`。

### 排序 + 分页

`sortBy` = `relevance` / `lastUpdatedDate` / `submittedDate`；`sortOrder` = `ascending` / `descending`；`start` = 偏移；`max_results` = 数量（默认 10，最大 30000）。

## 读论文内容

```python
# 摘要页（快，元数据 + 摘要）
web_search_scrapling(url="https://arxiv.org/abs/2402.03300", mode="get")
# 全文 PDF —— web_search_scrapling 抓后内容传给模型解析
web_search_scrapling(url="https://arxiv.org/pdf/2402.03300", mode="get")
```

## 常用类别

`cs.AI` 人工智能 · `cs.CL` NLP · `cs.CV` 视觉 · `cs.LG` 机器学习 · `cs.CR` 安全 · `stat.ML` 统计机器学习 · `math.OC` 优化。完整：https://arxiv.org/category_taxonomy

---

## Semantic Scholar（引用 + 相关 + 作者画像）

arXiv 没引用数据。**Semantic Scholar API** 免费（1 req/sec），返 JSON。

```bash
# 详情 + 引用数
curl -s "https://api.semanticscholar.org/graph/v1/paper/arXiv:2402.03300?fields=title,authors,citationCount,influentialCitationCount,year,abstract" | python3 -m json.tool
# 谁引了它
curl -s "https://api.semanticscholar.org/graph/v1/paper/arXiv:2402.03300/citations?fields=title,authors,year,citationCount&limit=10" | python3 -m json.tool
# 它引了谁
curl -s "https://api.semanticscholar.org/graph/v1/paper/arXiv:2402.03300/references?fields=title,authors,year,citationCount&limit=10" | python3 -m json.tool
# 作者画像
curl -s "https://api.semanticscholar.org/graph/v1/author/search?query=Yann+LeCun&fields=name,hIndex,citationCount,paperCount" | python3 -m json.tool
```

## 完整研究 workflow

1. **发现**：`python3 skills/arxiv/scripts/search_arxiv.py "你的题目" --sort date --max 10`
2. **评影响**：Semantic Scholar 拿 `citationCount` / `influentialCitationCount`
3. **读摘要**：`web_search_scrapling(url="https://arxiv.org/abs/ID", mode="get")`
4. **找相关工作**：Semantic Scholar `/references`
5. **追踪作者**：Semantic Scholar `/author/search`

## 备注

- arXiv 返 Atom XML，Semantic Scholar 返 JSON（`python3 -m json.tool` 美化）。
- arXiv ID 两种格式：老 `hep-th/0601001` vs 新 `2402.03300`。`v1` 后缀指定版本。
- 速率：arXiv ~1 req/3s，Semantic Scholar 1 req/s，均无需鉴权。
- 撤回论文检查 `<summary>` 内 "withdrawn" / "retracted"。
