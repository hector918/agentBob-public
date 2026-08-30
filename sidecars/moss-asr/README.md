# moss-asr sidecar

`OpenMOSS MOSS-Transcribe-Diarize`（Whisper-Medium 编码器 + Qwen3-0.6B 解码器；转写 +
说话人分离 + 时间戳一次出，50+ 语言，单次最长 90 分钟）经 **ggml C++ 移植**
[`moss-transcribe.cpp`](https://github.com/mudler/moss-transcribe.cpp)，套一层
OpenAI-compat FastAPI shim，端口 **11503**。

定位：**替换 `fun-asr`**。说的是同一套线协议，切换 = models.yaml 改一行
`base_url` + `model`，**bob 侧零代码改动**。

## 为什么是 ggml 移植，不是官方 PyTorch

目标机是 `Intel Xeon E5-2630L v4` —— 2016 年 Broadwell 低功耗版（10 核 20 线程，
基频 1.8 GHz），flags 只到 **AVX2**：没有 AVX-512、**没有 AMX**、没有 VNNI。

MOSS 的解码器是自回归的，在 CPU 上是内存带宽瓶颈 —— fp32 下每生成一个 token 要读
一遍 3.6 GB 权重。官方 PyTorch 路径在同级硬件上实测 **RTF ≈ 1.0**，也就是 40 分钟
的录音要跑 40 分钟。ggml 移植在 q5_k 下快 2–3 倍，权重从 3.4 GB 降到 619 MB。

移植的正确性论证是它敢用的原因：逐组件对着参考模型 dump 的张量做数值门禁
（cosine 1.0），端到端 **F16 到 q5_0 逐字节相同**，q4_k/q4_0 逐词相同（时间戳漂移
0.02–0.07 秒）。

### 性能预期

移植方自己的 benchmark（20 核 x86，8 线程，热跑）：

| 音频 | moss-transcribe.cpp | PyTorch | 提速 |
|---|---|---|---|
| 11 s | 6.5 s（RTF 0.59） | 10.5 s（RTF 0.96） | 1.62× |
| 44 s | 24.2 s（RTF 0.55） | 43.0 s（RTF 0.98） | 1.78× |
| 132 s | 102.8 s（RTF 0.78） | 162.1 s（RTF 1.23） | 1.58× |

量化再叠一层：F16 快 1.6×、q5_k 快 1.8×、q4_k 快 2.1×（相对 F32）。

### 实机实测（，xeon-llm，q5_k，MTD_THREADS=8）

估计作废，下面是真数字：

| 音频 | fun-asr | MOSS | 备注 |
|---|---|---|---|
| **1.0s** | — | **15.3s**（RTF 15.3） | 固定地板，见下 |
| 4.1s 粤语 | **3.2s** ✓ | 16.0s ✓ | 内容相同，MOSS 输出繁体 |
| 6.3s 粤语 | 4.4s，`book king` ✗ | 16.5s，`Booking` ✓ | **MOSS 专名更准** |
| 65s 视频音轨 | **8.9s**，1 句 | 53.8s（RTF 0.82），4 句 | MOSS 多挖出内容 |
| 4.4 分钟 | 176.5s，**输出 `the the the…` 垃圾** | 272.7s（RTF 1.04），19 段 / 2 个说话人 | fun-asr 在长音频上崩 |

**两个结构性发现，比单点数字重要：**

**① MOSS 有约 15 秒的固定地板。** 1 秒的音频也要 15.3 秒 —— Whisper 编码器按 30 秒
窗口对齐，短片付的是整窗的钱。所以它的 RTF 随音频变长而**变好**（1s→15.3，65s→0.82，
262s→1.04），和直觉相反。

**② fun-asr 在长音频上不是慢，是坏。** 4.4 分钟那条返回的是
`the the the the …` 的退化重复 —— 一个看起来像转写、实际是垃圾的字符串。今天线上
靠 `transcribeTimeout=60s` 挡住了（176s 超时），但一旦为长音频放宽超时，这个垃圾就
会被折进消息正文。**长音频这条路 fun-asr 根本走不通，不是快慢问题。**

于是两个后端是**互补而不是替代**：

| 场景 | 该用谁 | 为什么 |
|---|---|---|
| 短语音条（ingestion，同步，用户在等） | **fun-asr** | 3-4 秒 vs 16 秒；15 秒地板在这里最痛 |
| 长录音（按需工具，用户不在等） | **MOSS** | fun-asr 输出垃圾；MOSS 还给说话人分离 |

这正好落在已经设计好的 push/pull 那条线上：**音频即消息 → push → fun-asr；音频是
素材 → pull → MOSS**。不是折衷，是各用各的长处。

路由用现成机制即可，不需要新东西：MOSS 条目打 `tags: [asr, longform]`，fun-asr 保持
更高 priority；ingestion 照常 `Kind: KindASR` 拿到 fun-asr，将来的 audio 工具发
`Requires: ["longform"]` 拿到 MOSS。

### 调参扫描（，4.1s 粤语，每格取最好一次）

|  线程 | q5_k | q4_k |
|---:|---|---|
| 4 | 25.8s | 21.7s |
| 6 | 19.5s | 15.9s |
| **8** | **16.0s** | 13.5s |
| 10 | 14.1s | 12.0s |
| 16 | 13.5s | 11.5s |
| 20 | 13.3s | 11.4s |

**线程在物理核数（10）处见顶**：4→6 省 24%、6→8 省 18%、8→10 省 12%、10→20 只再省
5%。超线程对自回归解码基本白给。这台盒子还跑着 fun-asr / TEI / nginx-lb，**8 是速度
和不与邻居抢的平衡点，6 也合理（多付 18%）**。

**q4_k 稳定快 15-16%**（4.1s 短片和 65s 长片上都是），但**不能无条件用**：

| 素材 | q5_k | q4_k | 是否一致 |
|---|---|---|---|
| 4.1s 清晰粤语 | `去睇一下其他網站上面有冇轉載。` | 同左 | **逐字节相同** |
| 65s 嘈杂现场 | `…我猜他没写那都。` | `…那车都没写那都。` | **不同** |

上游说 q4_k 是"逐词相同"，实测**只在信号清楚时成立**；音频一嘈杂、模型一犹豫，量化
噪声就显出来了。而 MOSS 的用武之地恰恰是会议/长录音这种嘈杂多人的场合 —— 15% 的速度
换在这里的保真度不划算。

**结论：默认保持 q5_k @ 8 线程**（也就是现在的配置，扫描没有改变它，只是把它从"抄
benchmark 的猜测"变成了实测的最优点）。q4_k 仍然烤在镜像里，`MOSS_QUANT: q4_k` 一行
就能换 —— 留给"延迟比保真更重要"的场合。

**地板是结构性的。** 最激进的组合（q4_k + 20 线程）也要 11.4s，仍是 fun-asr 3.2s 的
3.5 倍。Whisper 编码器按 30 秒窗口对齐这件事没法绕过，所以"短音频 fun-asr、长音频
MOSS"的分工是定论，不是权宜。

### 短音频自动提线程（已验）

时长 < `MOSS_SHORT_SECONDS`(15s) 的片子用 `MTD_THREADS_SHORT`(10)，其余用
`MTD_THREADS`(8)。刻意和"公平"反着来：短片的成本被固定编码器窗口主导，而且是**同步**
付的（有人在等 turn 起步）；长录音按需跑，没人看着，就对这台机上的邻居客气点。

实测，和调参扫描的预测吻合：

| 素材 | 线程 | 耗时 | 对照(8 线程) |
|---|---|---|---|
| 4.1s 语音条 | 10（自动） | **14.2s** | 16.0s |
| 6.3s 语音条 | 10（自动） | **14.5s** | 16.5s |
| 65s 视频音轨 | 8 | 54.7s | 54.7s |

### 一个待办、一个不做

- **繁简不统一 —— 不做（拍板，别再报）。** 粤语音频出繁体、普通话出简体，
  而且会**传导到 bob 的回复语言**（语言检测读的是转写文本）。拍板是「没什么分别」：
  对粤语使用者回繁体本来就不算错，为此在 shim 里挂一个 opencc 依赖不划算。
- ~~地板能不能压低~~ —— 已扫描，见上：能压到 11.4s，压不动更多，分工结论不变。

## 接口

| 方法 | 路径 | 谁在用 |
|---|---|---|
| POST | `/v1/chat/completions` | **bob**（`audio_url` data-URI 进，转写文字出） |
| POST | `/v1/audio/transcriptions` | 没人 —— 标准 OpenAI 转写端点，给将来 bob 侧 provider 留的 |
| GET | `/v1/models` | 模型池的可达性 Ping |
| GET | `/healthz` | 存活 + 二进制/权重/ffmpeg 三项自检 |

**bob 发的是 FLAC，CLI 只吃 WAV** —— shim 每次请求都用 ffmpeg 转一道 16 kHz 单声道。
这不是可选优化，漏掉就是第一个请求直接失败。

## 输出形态（这条最要紧）

MOSS 原始输出是 `[0.48][S01]文字[1.66]` 这种带时间戳和说话人标签的串。

**默认返回干净文字**，时间戳丢掉 —— bob 会把这个字符串直接折进消息正文，时间戳会在
之后每一次 prompt 里占预算，而没有任何下游读者用它。

**说话人前缀只在真有 2 人及以上时出现**，沿用 bob 给群消息加说话人前缀的同一条规则
（少于 2 人不加）。所以**单人语音条的渲染结果和 fun-asr 完全一致** —— 对今天 100%
的真实流量，这次替换在输出上是不可见的。

解析走的是 CLI 的**默认原始输出**而不是 `--format json`：方括号格式是**模型自己的**
输出契约（上游模型卡和移植方文档写法一致），而 JSON schema 是移植方的、可能变。
解析有三层降级 —— 完整分段 → 无说话人标签 → 剥掉所有方括号只留字 —— **格式出意外
会退化成纯文字，不会退化成空转写**。

要看原始串和分段结构，请求里加 `"response_format": "verbose_json"`，响应多一个
`moss` 字段带 `raw` / `segments`（start/end/speaker/text）/ `speakers`。

## 部署

```bash
# 在 sidecar 盒子（xeon-llm）上
cd sidecars/moss-asr
docker compose up -d --build     # 编译 C++ + 拉 ~620MB GGUF，要联网
docker logs -f moss-asr          # 等 "moss-asr up on :11503" 和 "prewarmed 619 MB"
```

镜像里**没有 torch、没有 transformers、没有 HF 客户端** —— 运行期就是一个 C++ 二进制
加一个 GGUF 加一层 FastAPI。

### 验证清单

```bash
curl -s localhost:11503/healthz    # binary/gguf/ffmpeg 三项都要 true

python3 - <<'PY'
import base64, json, time, urllib.request
b = base64.b64encode(open("clip.flac","rb").read()).decode()
body = {"model":"OpenMOSS-Team/MOSS-Transcribe-Diarize",
        "response_format":"verbose_json",
        "messages":[{"role":"user","content":[
          {"type":"audio_url","audio_url":{"url":"data:audio/flac;base64,"+b}}]}]}
r = urllib.request.Request("http://localhost:11503/v1/chat/completions",
      data=json.dumps(body).encode(), headers={"Content-Type":"application/json"})
t0=time.time(); out=json.loads(urllib.request.urlopen(r, timeout=1800).read())
print("%.1fs"%(time.time()-t0))
print(json.dumps(out, ensure_ascii=False, indent=2)[:2000])
PY
```

看四样：耗时、`choices[0].message.content` 是干净文字、`moss[0].raw` 里有 `[Sxx]`
标签、`moss[0].speakers` 的人数对得上录音。

### nginx-lb：加一个 `/moss/` 前缀（不要改 `/asr/`）

隧道后面那台的统一出口 nginx（监听 8080，按前缀分发）需要一段 location，否则外面
根本进不来。整段见 `nginx-lb.conf.snippet`，**贴到 sidecar 盒子的 nginx 配置里**
（不在本仓库生效），然后 `nginx -t && nginx -s reload`。

三处和 `/asr/` 那段不一样，都是必须的：

| 项 | `/asr/`（fun-asr） | `/moss/` | 为什么 |
|---|---|---|---|
| `proxy_read_timeout` | 300s | **1800s** | fun-asr RTF 0.15；MOSS 在这台机上约 0.4，20 分钟录音要跑十几分钟。300s 会在解码中途掐断，而且表现为 504，很难认 |
| `client_max_body_size` | 继承 16m | **200m** | 16m 只够约 8 分钟音频（见下），而"单次 90 分钟"正是换它的理由 |
| 前缀 | 保持指向 fun-asr | 新增 | 两个同时在线才能做真正的 A/B |

**刻意并存，不是替换。** bob 那边同 kind 的低优先级条目只在高优先级挂掉时才轮到，
靠模型池比不出质量；两个 URL 同时在线，才能拿同一批音频各打一次。验收过了再决定
`/asr/` 改不改指向。

#### 体积那笔账

bob 送的是 16 kHz 单声道 flac，实测约 **24.5 KB/s**，base64 再膨胀 1.33 倍，即约
**32.7 KB/s 的线上体积**：

| `client_max_body_size` | 能过多长的音频 |
|---|---|
| 16m（现状） | 约 **8 分钟** |
| 200m（建议） | 约 **100 分钟** |

所以 16m 不是"够不够用"的问题 —— 它会让 MOSS 最核心的那个能力（90 分钟单次）
根本到不了后端。将来真要常态处理长录音，更省的做法是让 bob 别送 flac 而送 opus
（16 kbps 下 90 分钟只有约 11 MB），但那是 bob 侧的改动，不在这次范围里。

### 接进 bob

nginx 那段就位之后，bob 的 models.yaml：

```yaml
  - name: moss-asr
    provider: vllm
    kind: asr
    base_url: http://<deploy-host>:11433/moss/v1   # 新前缀；/asr/ 继续指 fun-asr 作回退
    model: OpenMOSS-Team/MOSS-Transcribe-Diarize
    tags: [asr]
    priority: 2
    concurrency: 1        # CPU 上是串行活，别让两个请求互相抢线程
```

两条 entry 可以并存（`fun-asr` 降 priority 留作回退），但**同 kind 的低优先级条目
只在高优先级挂掉时才轮到 —— 那不是 A/B**。要比质量必须离线跑。

## 验收判据（换之前必须过的一条）

> **主场景不能变差。** 那两条真实语音（`spaces/telegram_dm_222222222/inbox/` 下的
> 24KB / 16KB 粤语短句）在 MOSS 上的转写质量，不能低于 fun-asr。

这是今天唯一真实发生的场景，而 MOSS 公布的 benchmark 全是会议/播客/电影这类多人长
音频，恰好没有"几秒单人短句"的数字。加上它是 0.9B 兼做转写+分离，Fun-ASR-Nano 是
专职 ASR —— 这个风险是真的，用现成素材几分钟就能证伪。

同时要测：视频音轨（对比 fun-asr 给出的「那人怎么可能哎呀。」）、一段**真有两人对话**
的录音（否则分离能力根本没被测到）、以及一段长音频看单次能吃多长 —— 最后这条直接
决定 audio 工具那边要不要写分段转写引擎。

## 未验证的地方

镜像**没有在目标硬件上构建过**，以下几处第一次构建时要盯：

- `MT_REF=master` —— 上游还没有 release tag（42 stars / 40 commits / 单人维护，
  bus factor 1）。**有 tag 之后请钉住**，不要长期跟 master
- `MTD_THREADS=8` 抄的是 benchmark 配置，不是本机实测。10 核的盒子上 8 / 10 / 20
  哪个最快要试
- `MOSS_MAX_NEW=8192` 大约覆盖 20–30 分钟录音（MOSS 的 token 要花在时间戳和说话人
  标签上，不只是字）。**超过就会静默截断**，喂更长的之前先调大
- `mem_limit: 8g` 是 32 GB 机器上的余量，不是测量值。KV cache 随**音频长度**涨，
  不随请求数涨
- CLI 的 `--help` 在构建期跑过（二进制起不来会在 build 时炸），但**权重加载和真实
  推理没跑过** —— 那要等部署

移植方的 API 现状是 **CLI-only**（HTTP 接口在 roadmap 上，要等 LocalAI 后端），所以
shim 是按请求 fork 子进程。32 GB 内存下这不构成问题：ggml 用 mmap，启动时的 prewarm
把 619 MB 拉进 page cache 之后就一直在那儿，每次"加载"只是页表走一遍。真正的常驻
需要移植方的 flat C-API，那个还没发布。
