# contract：契约层

`contract` 是让模块彼此对话而**不必互相 import** 的共享词汇表：能力接口，加上流经这些接口的信封数据。零行为。

---

## 在架构中的位置

contract 是依赖图的底：所有模块都可以 import 它，它不 import 本项目的任何包，只 import 标准库。

它与 [trunk](trunk.md) 是一对：trunk 提供发现机制（接口类型 → 唯一实现），contract 提供被发现的那些**接口类型本身**。没有 contract，trunk 的注册表就没有 key；没有 trunk，contract 里的接口就只是一堆没人注册的声明。

它与 [heartwood](heartwood.md) 的分工是另一条线：heartwood 是**被直接 import 的实现**（时钟、脱敏、提示词工具函数），contract 是**不被实现的声明**。前者共享代码，后者共享形状。

---

## 功能

一条准入规则决定一个类型是否属于这里——它必须是**经 trunk 中介**的合同的一部分：

1. **一个注册到 trunk 的能力接口**。也就是注册表的 key：消费方靠它替代对提供方的直接 import。
2. **出现在这种接口方法签名里的载荷数据**。流过这些调用的通用信封——消息、附件、工具调用、模型响应、回合规格。

其余一律留在外面：

- 只被一个模块用、且不出现在任何 contract 接口签名里的类型留在那个模块里，**哪怕它"只是个 struct"**。
- **直接耦合**的共享类型不进来。一个插件和它的父模块（一个 source 与网关，一个 tool 与工具目录）本来就直接互相引用，共享类型留在拥有它的那一方，另一方直接 import。直接引用不配占用公共词汇表。

这条规则的作用是防止 contract 变成"公共类型垃圾场"。凡是往这里加类型的动机是"两个地方都要用"而非"这条调用穿过 trunk"，都应该被挡回去。

contract 刻意是一个**扁平单包**。词汇之间引用密度极高（一个 `MessageEvent` 牵着 `Attachment`、`ChatType`、`Target`、`Caps`），拆成子包只会立刻招来 import 环。

---

## 内部结构

包内文件按**数据流的先后**编号，读一遍编号就是读一遍请求路径：存储面 → 消息与附件 → 准入筛查 → 源与网关 → 回复渲染 → 会话 → 模型消息与模型池 → 用量 → 回合 → 流程 → 之后是各能力缝（令牌、提示词、斜杠、面板、账户、工具、权限、通道、技能、组织、编排、API key）。

内容上分四类。

### 一、能力接口清单

注册到 trunk 的能力共 41 项（外加 trunk 自己提供的 `trunk.Housekeeper`）。

**基础设施**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `DB` | pgpool | SQL 通用的存储面；"后端是 Postgres"这个事实止步于 pgpool 与各模块自己的持久化文件 |
| `Housekeeper` | trunk 自身 | 周期性维护任务的登记口（详见 [trunk.md](trunk.md)） |

**接入与身份**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `Gateway` | stoma | source 总线：持有每个源的生命周期，把入站事件汇成一条流，探测健康 |
| `Screener` | gate | 入站发送者的单一筛查权威（黑名单 > 裸码兑换 > 白名单），并回答 admin 判定 |
| `AccessGranter` | gate | 改写某个源的访问策略：把发送者加入白名单、切换其 admin 位 |
| `ClaimTokens` | claimtoken | 纯令牌生命周期：铸造随机密钥、冻结载荷、按 TTL 存、只兑付一次。渠道无关、后续流程无关 |
| `Accounts` | accounts | 身份读侧：跨入口 handle → 账户与其 flow 策略 |
| `AccountProvisioner` | accounts | 身份写侧，与只读的 `Accounts` 刻意分开，保证路由热路径只读 |
| `APIKeys` | accounts | bearer token → 计费账户 + 可用模型策略。缺席则一律 401（fail-closed） |
| `ConsumptionReporter` | accounts | 每次模型调用的真实 token 记到消费者 handle 名下——按用户的计费账本 |
| `AdminLine` | adminline | 运维召唤漏斗：源健康、模型条目死亡等紧急事件经此送达管理员 |

**会话与回合**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `SessionManager` | session | 会话子系统入口：解析入站事件属于哪个会话、按会话串行（一次一个回合）、持久化状态 |
| `MessageStore` | session | 回合内核读尾/追加历史的唯一耦合面——历史归会话所有，回合只是短命的执行者 |
| `ChatHistory` | session | 按聊天 scope 寻址的只读浏览视图，与回合写热路径分开，避免一个接口越长越宽 |
| `MessageIndexer` | session | 记录已投递回复的平台消息 id → 会话 id，使"回复某条消息"能续回同一会话 |
| `SessionResume` | session | 外部事件结束后唤醒会话（浏览器接管交还等）；只唤醒 auto 模式会话 |
| `Turn` | turn | 回合内核：轮次迭代（模型 → 工具派发 → 重复）、打捞、压缩、硬轮次上限 |
| `TurnHandler` | flow/router | 会话仲裁者知道**何时**跑一个回合，这个接口知道**怎么**跑 |
| `FlowRegistry` | flow/router | 收集所有回合执行流程；路由器按 Priority 询问谁接单 |
| `PromptFactory` | heartwood/prompt | 每回合发一个新的分层系统提示词构建器，构建器本身不持会话状态 |

**模型**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `ModelPool` | model | 把一个 `ModelRequest` 路由到活着的后端条目（多条目标签匹配、存活性、回退） |
| `ImageCatalog` | model | 画风清单与提示词说明的唯一副本，供生图工具与对外网关共用 |
| `Transcriber` | asr | 摄取期把入站音轨转成文本，下游一律当普通文本处理 |

**能力与授权**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `ToolCatalog` | tools | 全量已注册工具集（它本身就是一个 `ToolSet`），流程从中投影 |
| `ChannelPool` | tools | 有状态通道（终端会话、远程连接）的存活复用；按 principal 整体回收 |
| `BrowserControlHold` | tools | 人类接管浏览器期间的租约式让位登记；租约自过期，死标签页无法永久卡住 |
| `BrowserTakeover` | tools（条件注册） | 远程浏览器实时接管的中继面（屏幕流 + 输入），只在配置了浏览器后端时注册 |
| `TakeoverMinter` | webui | 铸造一次性、按 coverage 锁定的接管令牌——撞上登录墙时交还人类的那道缝 |
| `SkillCatalog` | skills | 内置 + 外置技能目录，同名时外置覆盖内置 |
| `SkillFailureSink` | skills | 记录降级回合中被使用技能的轨迹快照，作为学习信号 |
| `Warrant` | warrant | 能力裁判：拿到一份 `GrantSet` 后过滤目录/金库，并发放受闸的通道 |
| `Broker` | credentials | 由凭证名构建好一个配置完毕的客户端，**密钥不经过调用方** |

**组织与编排**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `Agora` | agora | 组织读图：scope → 收件箱 → 成员 → 在职角色的路由、上下文与自鉴权 |
| `AgoraSend` | agora | 发消息工具的目标名解析 + 发送鉴权，字符串进出，调用方不 import 组织类型 |
| `MemberFailureSink` | agora | 按（公司, 角色）记录降级回合快照，供角色指引的失败驱动学习 |
| `Arrangements` | arrangement | 按角色优先级桶的任务编排表：定义、注入、认领、提交、状态 |

**学习与记忆**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `LearnRegistry` | learn | 收集"可训练文本"目标；引擎每个维护周期遍历所有已注册来源 |
| `URLLibrary` | urllib | 共享 URL 记忆：记录成功抓取、召回已证明有用的 URL |
| `RetrievalClient` | retrieval | 冷记忆读侧；**fail-open**——不可达就跳过，绝不失败整个回合 |
| `RetrievalFeed` | retrieval | 回合结束后的写侧，进持久 outbox 再由推送器投递 |

**界面**

| 接口 | 提供方 | 作用 |
|---|---|---|
| `SlashRegistry` | slash | 命令表与派发；拥有命令的模块在 Start 时注册自己的命令 |
| `PanelRegistry` | webui | 面板登记表，形态镜像 `SlashRegistry`：每个模块自描述要展示的状态与设置 |

后两项体现了同一个模式：**界面是通用渲染器，模块自己描述自己。** webui 对任何子系统一无所知，它只收集面板并用一套固定的字段词汇渲染；因为耦合面是一个装满闭包的 struct，webui 除 contract 外什么都不 import，每个模块的状态留在自己的闭包里。

### 二、信封数据

流经上述接口的载荷，按管线位置分几族：

- **入站**：`MessageEvent`（一条入站消息的全部事实：源、聊天类型、发送者、文本、附件、派发元信息）、`Attachment`、`ChatType`、`Caps`（一个源能做什么）、`Target`（回复投递到哪）。
- **出站**：`Sink`——回复的渲染面。本地终端实时打印增量，IM 源缓冲增量并按限速节奏编辑消息。两条增量通道刻意分开：面向用户的正文内容，与可关闭的过程痕迹。
- **模型侧**：`Message` / `ToolSpec` / `ToolCall` / `ImageRef` / `AudioRef` / `StreamEvent` / `StreamAccumulator` / `ChatResponse` / `Usage`，以及 `ModelRequest` 与模型池的快照类型。
- **回合侧**：`UserMsg`（一个发言者对本回合的贡献，含作者与结构化附件）、`TurnSpec`（流程交给回合内核的完整规格：提示词构建器、用户输入、模型选择、输出 sink）、`TurnResult` / `TurnOutcome` / `TurnMode`。
- **授权**：`GrantSet`——一次请求的已解析授权投影（`tool:use:X` / `skill:use:X` / `credential:use:X`）。多个供应方把各自的来源塌缩成这一种货币，裁判只对它做一次判断，且**成员判定只定义一次、就在这里**（空集不授予任何东西）。
- **面板**：`Panel` / `StateField` / `Cell` / `TablePage` / `SettingSpec` 等一套固定的展示词汇。

### 三、角色接口（不进注册表）

contract 里的接口远多于 41 个，其余全部来自准入规则的第二条——它们是被能力接口的方法签名**带进来**的，而不是注册表的 key：

- **插件接口**：`Source`（网关持有 N 个）、`Tool`（工具目录持有 N 个）。多实现由父模块管，前面已述。
- **可选扩展探测**：`Sink` 的若干扩展（整块产物、换行刷新、直接发图、生图暂扣）、`Source` 的若干扩展（自我提及识别、发图、消息反应）。调用方用类型断言探测"这个实现支持这项吗"，不支持就走通用路径。这是"能力协商"而非"继承"：一个新的源实现基础面就能接入，扩展面按需实现。
- **投影包装**：`ToolSet` / `SkillSet` 与它们的具体实现 `ToolSubset` / `SkillSubset`。授权方按自己的策略筛完，把幸存者装进这两个壳，以 `ToolSet` / `SkillSet` 的形态交给流程。投影逻辑本身与身份无关，所以在这里存一份——模块之间禁止互相 import，一个"共享的 leaf 工具包"不是选项。
- **窄化的子面**：`GroupRouter` 是会话解析器需要的那一小片组织能力（`Agora` 恰好满足它），`HistoryReader`、`AttachmentSet`、`SubRunner` 同理。刻意窄化是为了让消费方看不见它用不到的东西。
- **数据面**：`Result` / `Row` / `Rows` / `Tx`——`DB` 之下的 SQL 通用面，不泄漏任何驱动类型。

### 四、纯函数与 ctx 载体

契约层"零行为"有几处刻意的例外，共同点是：**这些东西必须在两侧逐字节一致，所以只能有一份。**

- **scope 语法**：`ScopeFor` 把（源、聊天类型、聊天 id、话题 id）编成一个 scope 字符串，`TargetForScope` 是它的逆。二者互为反函数是它们同处一处的全部理由，并由一个测试守住——一旦漂移，恢复出来的图片就会投递到错误的聊天，或者哪也不到。
- **归一化**：`CleanFileName` 是附件名匹配器对**比较双方**都施加的那一种规范形式。
- **ctx 载体**：计费 handle、身份、成员、生图进度回调都以未导出的 key 挂在 `context.Context` 上，配一对 `WithX` / `XFrom`。这样一个回合深处的旁路模型调用（OCR、转写、压缩）不需要任何显式串线就能被正确归账。
- **构造糖**：`OKResult` / `ErrResult` 等，纯粹为了让每个工具不必手写同一个 struct 字面量。

---

## 设计理据

### 缺席时怎么办，是接口的一部分

项目里三十个注册节点中十七个是可选的，所以"提供方不在"是常态而非异常。每个可选能力的接口注释都必须写明消费方在它缺席时的行为，而且这个行为要经过选择：

- **fail-open**：冷记忆检索不可达就跳过召回；URL 记忆缺席时所有方法退化为空操作，调用方天然 nil-safe；用量上报缺席就是不记账。
- **fail-closed**：API key 校验缺席则每个请求 401；授权投影为空则不授予任何能力。

方向由"哪边的失败更安全"决定，而不是由实现方便决定。这条判断标准与 [trunk.md](trunk.md) 里"硬边还是软边"的判断标准是同一条。

### 窄接口是解耦的度量

同一个提供方经常发多张不同宽度的面，而不是一张大面：

- session 发了五张——回合写热路径（`MessageStore`）、管理只读浏览（`ChatHistory`）、回复索引（`MessageIndexer`）、外部唤醒（`SessionResume`）、总入口（`SessionManager`）。合成一张会让浏览路径的需求不断推宽回合热路径上的接口。
- accounts 把只读的身份查询与写侧的开户分成两个接口，路由热路径因此只拿得到只读那张。
- 会话解析器要路由一个多成员群聊时，只取 `GroupRouter` 那三个方法，而不是整个 `Agora`。

判据很简单：**如果两类消费方对同一个提供方的需求会朝不同方向生长，就发两张面。**

### 一个类型不进来，不代表它不重要

最容易误判的是插件与父模块之间的共享类型。它们跨模块边界，看起来"公共"，但那条耦合是直接的——插件本来就 import 父模块。把这类类型搬进 contract 只会让公共词汇表膨胀，同时一点耦合都没减少。

判据不是"有几个地方用它"，而是"**这条调用是否穿过 trunk**"。
