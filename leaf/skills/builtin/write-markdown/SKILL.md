---
name: write-markdown
description: |
  写超长回复、markdown 文档、或要一份 .md 文件时用此技能。
  Use when: 任何需要输出超过约 500 字的回复，或用户要求 markdown 文档 / 要一份 .md 文件。
  Triggers: 长回复, 写文档, 写文章, 输出 markdown, 生成 README, 文档, README, 方案, write a doc, write an article, generate markdown, 发我一份, 给我个文档, 我要存下来.
---

# write-markdown — 长 markdown 直接当回复，或发成 .md 文件

## 1. 一句话规则

打算输出超过约 500 字的 markdown 正文，就走这个 skill —— 不论用户有没有显式说"写文档"。判断标准是**你打算输出多长**。

## 2. 直接 markdown 回复

多用 markdown 格式（标题 / 列表 / 表格 / 代码块 / 强调）把内容组织清楚，写完直接当回复发出。inline 看的内容不必再写成文件。

## 3. 用户要一份 .md 文件

用户明确想拿一份文件（"发我一份 / 给我个文档 / 我要存下来"）时，两步走：

**Step 1 — 写进空间**：用 `fs` 把整篇写进默认空间的一个 .md 文件，path 只给文件名：
```
fs(cmd="write", path="<filename>.md", content="# 标题\n\n<正文>")
```
一次写入整篇即可；写完不要再把全文 inline 重复一遍。

**Step 2 — 发出去**：
```
deliver_file(path="<filename>.md", caption="<一句说明>")
```
然后一句话确认（"文档已发，叫 <filename>.md"）。如果用了 todo 且要发文件，todo 最后也插一条"发送文件"。

## 4. 场景速查

| 用户要的 | 你做的 |
|---|---|
| 讨论 / 写报告 / 写方案 / 写分析，inline 看 | 直接 markdown 回复 |
| 明确"发我一份 .md / 要保存" | `fs` 写空间 + `deliver_file` 发 + 简短确认 |
