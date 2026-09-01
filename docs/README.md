# agentbob documentation · 文档

Documentation is maintained in two languages. The two trees mirror each other
path for path.

文档维护两种语言，两棵目录树逐路径镜像。

| | |
|---|---|
| **English** | [Architecture overview](en/architecture.md) · [Deployment](en/deployment.md) |
| **中文** | [架构总览](zh/architecture.md) · [部署](zh/deployment.md) |
| **Diagrams · 图** | [Five layers](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.en.html) · [五层结构](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.zh.html) · [Life of one message](https://hector918.github.io/agentBob-public/docs/diagrams/message-lifecycle.en.html) · [一条消息的一生](https://hector918.github.io/agentBob-public/docs/diagrams/message-lifecycle.zh.html) |

---

## Layout · 结构

```
docs/
├── en/  ─┐
│         ├── architecture.md      overview, start here
│         ├── deployment.md        running it
│         ├── core/                trunk · contract · heartwood · infra
│         ├── modules/             the leaf modules
│         ├── flows.md             the flow layer
│         └── sidecars/            out-of-process services
├── zh/  ─┘  (same paths, in Chinese · 同样的路径，中文)
└── diagrams/     self-contained interactive HTML + the JSON they render from
                  自包含可交互 HTML，以及渲染它们的 JSON 源
```

Start with `architecture.md` — it explains the five layers and links onward to
everything else.

从 `architecture.md` 开始——它讲清五层结构，并链接到其余全部内容。

---

## A note on code comments · 关于代码注释

Comments throughout the source refer to design documents by paths such as
`docs/some-topic.md`. Those were internal working documents — design discussions,
review records, and work-in-progress notes — and they are not part of this
public tree. The references are left in place because they are load-bearing
inside the codebase's own history; treat a link you cannot follow as a marker
that a decision was recorded elsewhere, not as a broken file.

The documentation here is written fresh against the code as it stands, organised
by module rather than by the chronology of how each piece was decided.

源码注释中会以 `docs/某主题.md` 这样的路径引用设计文档。那些是内部工作文档——设计讨论、review 记录、在制品笔记——不属于本公开目录树。这些引用被保留原样，因为它们在代码库自身的脉络里是承重的；遇到点不开的链接，请把它当作「某个决定被记录在别处」的标记，而不是一个坏掉的文件。

这里的文档是对照当前代码重新撰写的，按模块组织，而不是按各部分当初被决定的先后顺序。
