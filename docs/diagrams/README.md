# Diagrams · 系统图

Two diagrams, each in English and Chinese. Every `.html` is a **self-contained**
page — no CDN, no network — with light/dark themes, pan/zoom, search, relationship
tracing, guided views and PNG/SVG/WebM export.

两张图，各有中英文版本。每个 `.html` 都是**自包含**页面——不连 CDN、不联网——带明暗主题、
缩放平移、搜索、关系追踪、引导视图，以及 PNG/SVG/WebM 导出。

| Diagram · 图 | English | 中文 | Source |
|---|---|---|---|
| Mechanism and policy in five layers · 机制与策略的五层结构 | [html](architecture-layers.en.html) · [png](architecture-layers.en.preview.png) | [html](architecture-layers.zh.html) · [png](architecture-layers.zh.preview.png) | [`docs/en/architecture.md`](../en/architecture.md) |
| The life of one message · 一条消息的一生 | [html](message-lifecycle.en.html) · [png](message-lifecycle.en.preview.png) | [html](message-lifecycle.zh.html) · [png](message-lifecycle.zh.preview.png) | [`docs/en/flows.md`](../en/flows.md) |

**Viewing · 怎么看.** GitHub serves a `.html` blob as source, not as a page. Clone the
repo and open the file, or enable GitHub Pages for this repository. The `.preview.png`
next to each one is a static snapshot for reading in place.

GitHub 会把 `.html` 当源码展示，不会渲染成页面。请 clone 下来本地打开，或为本仓库开启
GitHub Pages。每张图旁边的 `.preview.png` 是可以就地阅读的静态快照。

**Provenance · 出处.** The `.json` beside each page is the specification it renders from —
typed, schema-validated, and the only thing edited by hand. The architecture spec also
carries per-node `sources` pointing at real files at a pinned revision, machine-verified
against this repository at render time. Rendered with [archify](https://github.com/tt-a1i/archify)
(MIT).

每个页面旁边的 `.json` 就是它的渲染规格——有类型、过 schema 校验，也是唯一手写的东西。架构图的
规格里还带着每个节点的 `sources`，指向固定版本下的真实文件，渲染时会对着本仓库做机器核验。
渲染工具为 [archify](https://github.com/tt-a1i/archify)（MIT）。
