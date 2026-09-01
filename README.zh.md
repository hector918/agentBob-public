# agentbob

一个自托管的、以即时通讯为第一入口的 AI agent，用 Go 从零写成。

跑在你自己的机器上，接你自己的模型，从你本来就在用的聊天软件里跟它说话。它维持对话、调用工具、跨会话保有记忆，也可以被编成一个小团队——多个 agent 之间互相派活。

*Read this in [English](README.md).*

---

## 它能做什么

**在你已经在的地方跟你说话。** Telegram、飞书、Discord、邮件、本地控制台——同时接入，共用一个网关。每个平台都是一个插件，挂在统一的 source/sink 契约后面，所以再加一个平台是一件自包含的活。

**跑你自己的模型。** 模型池同时持有多个后端，按标签和健康度为每次请求选路，维持提示词缓存亲和性让重复上下文保持热的，后端忙时退避重试而不是让这个回合失败。本地 llama.cpp、托管 API，或者混着来。

**会用工具。** shell 执行、沙箱文件存储、网页搜索与抓取、一个它能真正操作并登录的浏览器、视觉与 OCR、语音转写、图像生成、EPUB 翻译、WordPress 发布。工具授权是按调用者算的，不是全局开关。

**记得住。** 会话持久化在 Postgres。长对话走压缩而不是截断；更早的材料通过检索仍然够得着，而不是从末端掉出去。

**可以是一个团队，不只是一个 bot。** 组织层让你定义带角色的成员，各自有独立的收件箱、技能和身份。他们之间路由工作、互相汇报——这和"一个助手回答一个人"是形状不同的问题。

**是被治理的。** 授权是一等模块：单一策略源决定任何调用者能做什么，并留决策日志。没有管理员后门——闸门是唯一通路。

**也把自己暴露出去。** 一个网关模块可以把模型池发布成 API key 认证的端点，让别的软件经由 bob 使用同一批后端。

---

## 它是怎么搭的

五层，核心有一条硬规则：**leaf 模块之间绝不互相 import**。它们只能通过从一根薄脊梁上取得的能力接口彼此触达。

```
contract     能力接口 + 跨模块信封数据。零行为。
heartwood    所有模块都可直接 import 的共享原语（时钟、附件暂存、
             提示词卫生、凭证脱敏）。
trunk        脊梁：能力注册表、生命周期定序器、housekeeping 调度器。
             不知道什么是消息、会话、公司。
leaf/        模块——有生命周期和资源的行为子系统。
flow/        流程——薄编排脚本，可整条替换。
```

这样切的意义是**机制与策略分离**。系统「能做什么」在模块里；某种对话「怎么编排」在一条你可以整条换掉的流程里。

**可交互系统图。** 两张，浏览器里直接看——缩放平移、搜索、追踪某条关系、按引导视图逐段读、导出 PNG/SVG：
[五层结构](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.zh.html) ·
[一条消息的一生](https://hector918.github.io/agentBob-public/docs/diagrams/message-lifecycle.zh.html)
*(English: [five layers](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.en.html) ·
[life of one message](https://hector918.github.io/agentBob-public/docs/diagrams/message-lifecycle.en.html))*

[![agentbob — 机制与策略的五层结构](docs/diagrams/architecture-layers.zh.preview.png)](https://hector918.github.io/agentBob-public/docs/diagrams/architecture-layers.zh.html)

这不是一个靠 review 自觉维持的愿景，而是**焊在测试里的**。`arch/` 包持有已批准的模块连接图，任何模块增减一条依赖边都会让构建变红，直到这个改动在那里被 review 并批准。

重依赖的部分（浏览器引擎、CUDA 推理、Python 生态）住在 `sidecars/`——独立进程，主二进制只通过 HTTP 与之对话，于是它们可以部署到别的硬件上而核心零改动。

**→ 完整文档：[中文](docs/zh/architecture.md) · [English](docs/en/architecture.md) · [系统图](docs/diagrams/)**

---

## 它是怎么长成这样的

这个项目的成长有几个能辨认出来的阶段。

它从一条核心循环开始：一个网关收消息，一个模型池回答，一个回合引擎驱动工具调用，一个存储让这一切能挺过重启。这条循环立住之后，问题变成了「一个 agent 到底是不是正确的单位」——这催生了组织层，几个角色和收件箱各不相同的成员在其中互相传递工作，以及一个用来看他们在干什么的 web 控制台。

再往后是触达面：更多聊天平台、邮件、OCR 与视觉（好让 agent 能读懂人们真正发过来的东西）、跨入口身份（同一个人从 Telegram 来还是从邮件来都是同一个人）、以及一套承载任务专用指令的技能系统。

到这时，代码库的生长已经快过了它的结构。加一个功能要横切十几个包，wiring 全挤在一个臃肿的启动文件里，不同对话类型之间的差异散成了到处都是的条件分支。于是有了一次刻意的重建——围绕上面那套 trunk/leaf/flow 架构，同时把回合循环重写成「一个内核 + 可互换的 driver」。

重建之后的工作是纵深而非铺开：更厚的工具面、带持久登录的浏览器自动化、一个拿真实对话记录调优自身提示词的经验优化器、任务编排、对外 API 网关——以及反复的全项目 review，文档里记下的相当一部分设计判断就是从那里来的。

---

## 谁写的

由 [hector918](https://github.com/hector918) 构建，agent 循环部分以 [hermes-agent](https://github.com/NousResearch/hermes-agent) 为设计蓝本。

许可条款见 [LICENSE](LICENSE)。

---

## 快速开始

前置：带 compose 插件的 Docker、一个可达的 Postgres、至少一个 OpenAI 兼容的模型端点。

```bash
git clone <this-repo> agentbob
cd agentbob
docker compose build           # 首次约 5-8 分钟（要下载 chromium）
docker compose up -d
docker compose logs -f bob
```

然后配置 `$BOB_HOME`：

```bash
# .env —— 机密。BOB_POSTGRES_DSN 是必需的，没有它 bob 拒绝启动
#（compose 不自带数据库，请自备）。
BOB_POSTGRES_DSN=postgres://user:pass@<db-host>:5432/bob?sslmode=disable
TELEGRAM_BOT_TOKEN=<从 BotFather 拿到的 token>
```

```yaml
# models.yaml —— 你的模型在哪。用局域网 IP，不要用 host.docker.internal
#（那个在 Linux 上不工作）。
entries:
  - name: smart
    provider: llama-cpp
    model: <你的模型 id>
    base_url: http://<model-host>:11434/v1
    tags: [smart, local, toolcall]
```

聊天权限**默认是关的**——自动生成的 `sources/telegram.yaml` 会丢弃每一条消息，直到你把自己加进 `allowlist`。这是刻意的：一个在公开平台上敞开的 bot 等于一个敞开的 shell。

平台支持是架构可移植的：在 macOS arm64、Linux amd64、Linux arm64（树莓派 5、Graviton）上都测过。构建出的镜像是 CPU 特定的——要么在目标机上构建，要么用 `buildx` 出多架构镜像。

**完整部署指南（含全新 Linux 服务器从零走一遍、以及状态迁移）：[中文](docs/zh/deployment.md) · [English](docs/en/deployment.md)**

---

## 命令

两类：CLI 子命令（`bob <cmd>`）和聊天里敲的斜杠命令（`/<cmd>`）。斜杠命令直接在 bob 里执行，从不惊动模型。见 [commands.md](commands.md)。
