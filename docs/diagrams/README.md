# Diagrams · 系统图

Two diagrams, each in English and Chinese. Every `.html` is a **self-contained**
page — no CDN, no network — with light/dark themes, pan/zoom, search, relationship
tracing, guided views and PNG/SVG/WebM export.

两张图，各有中英文版本。每个 `.html` 都是**自包含**页面——不连 CDN、不联网——带明暗主题、
缩放平移、搜索、关系追踪、引导视图，以及 PNG/SVG/WebM 导出。

| Diagram · 图 | English | 中文 | Source |
|---|---|---|---|
| Mechanism and policy in five layers · 机制与策略的五层结构 | [html](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.en.html) · [png](architecture-layers.en.preview.png) | [html](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.zh.html) · [png](architecture-layers.zh.preview.png) | [`docs/en/architecture.md`](../en/architecture.md) |
| The life of one message · 一条消息的一生 | [html](https://hector918.github.io/agentBob-public/docs/diagrams/message-lifecycle.en.html) · [png](message-lifecycle.en.preview.png) | [html](https://hector918.github.io/agentBob-public/docs/diagrams/message-lifecycle.zh.html) · [png](message-lifecycle.zh.preview.png) | [`docs/en/flows.md`](../en/flows.md) |

**Viewing · 怎么看.** The `html` links above open the live pages on GitHub Pages. Reading
this file on github.com instead, the `.preview.png` beside each page is a static snapshot;
a raw `.html` blob there is served as source, not rendered.

上面的 `html` 链接指向 GitHub Pages 上的在线页面。如果你是在 github.com 上读这个文件，
每张图旁边的 `.preview.png` 是静态快照；在 github.com 上直接点 `.html` 得到的是源码而非页面。

**Provenance · 出处.** The `.json` beside each page is the specification it renders from —
typed, schema-validated, and the only thing edited by hand. The architecture spec also
carries per-node `sources` pointing at real files at a pinned revision, machine-verified
against this repository at render time. Rendered with [archify](https://github.com/tt-a1i/archify)
(MIT).

每个页面旁边的 `.json` 就是它的渲染规格——有类型、过 schema 校验，也是唯一手写的东西。架构图的
规格里还带着每个节点的 `sources`，指向固定版本下的真实文件，渲染时会对着本仓库做机器核验。
渲染工具为 [archify](https://github.com/tt-a1i/archify)（MIT）。
