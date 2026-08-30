# mock —— 本地端到端测试用的假模型端

提供一个 **OpenAI 兼容的假模型服务**,让重建后的整条管线(model pool +
provider client + turn + sink + history)在**本地、无真机、无真 LLM、无网络**的
情况下端到端跑通。管线全是真的(真 HTTP/SSE、真 wire 转换、真流式渲染、真历史
持久化),只有"模型的智能"是写死的。

服务两个端点(同真 OpenAI):
- `GET /v1/models` —— pool 启动探测用
- `POST /v1/chat/completions` —— 流式 SSE 和非流式都支持(看请求的 `stream`)

## 两种用法

### 1. Go 端到端测试(`go test ./mock/`)—— 自动回归

`e2e_test.go::TestE2E_FullPipeline` 启**整个 trunk**(pgpool + gate + model +
session + turn + stoma + flow),用一个程序化 source 发一条消息,断言 mock 的
回复**流过整条管线**回到 sink。

```bash
PGPOOL_TEST_DSN='postgres://bob:rebuild@127.0.0.1:5433/agentbob_rebuild?sslmode=disable' \
  go test -v ./mock/
```

（不设 `PGPOOL_TEST_DSN` 则 skip —— 它要那个一次性重建 pg。）

别的测试也可以 `import "agentbob/mock"` 然后 `mock.NewModelServer(replyFunc)`
拿一个随机端口的假模型端,把 `models.yaml` 的 `base_url` 指过去。

### 2. 手动对话(独立 server + bob 二进制)—— 交互验证

起独立假模型端(固定端口):

```bash
go run ./mock/cmd/mockmodel          # 监听 :18080；MOCK_ADDR=:9000 可改
```

`$BOB_HOME/models.yaml` 指向它:

```yaml
entries:
  - name: mock-local
    provider: openai
    model: mock
    base_url: http://localhost:18080/v1
    tags: [smart]
```

跑 bob(interactive REPL 或 pipe 一条):

```bash
BOB_DSN='postgres://bob:rebuild@127.0.0.1:5433/agentbob_rebuild?sslmode=disable' \
  BOB_HOME=$BOB_HOME ./bob
```

输入消息 → 看 bob 把 mock 的回复渲染出来。

## 到真机

把 `models.yaml` 的 `base_url` 换成**真模型端**(本地 llama.cpp / vllm / 真
API),其余完全一样 —— 立刻出真 LLM 回复。

## 扩展 mock

`NewModelServer(reply ReplyFunc)` 的 `reply func(lastUser string) string` 可以
返回任意内容,用来构造各种测试场景(固定回复 / echo / 触发特定分支)。要测
工具调用、错误帧、慢响应等,在 `model.go` 的 handler 里加分支即可。
