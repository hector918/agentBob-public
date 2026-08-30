# 部署

agentbob 以 Docker 镜像形式发布。有三样东西它不会替你准备：一个 **Postgres 数据库**、至少一个 **OpenAI 兼容的模型端点**，以及你想接入的各聊天平台的凭证。

---

## 平台支持

已测试：

- **macOS arm64**（M 系列，Docker Desktop）
- **Linux amd64**（Intel/AMD 的 Ubuntu / Debian，原生 Docker）
- **Linux arm64**（树莓派 5、AWS Graviton 等）

Dockerfile 是架构可移植的：基础镜像 `debian-bookworm-slim`，全部 Python 依赖（`scrapling`、`playwright`、`patchright`、`curl_cffi`）都有 amd64 和 arm64 的 wheel，chromium 在两种架构下的 apt 与 playwright 变体都有。

> **镜像是 CPU 特定的。** `docker compose build` 产出的镜像绑定构建主机的 CPU。不要把 arm64 镜像 `docker save | scp` 到 amd64 主机上（反之亦然）——那会退化成 QEMU 模拟，又慢又不稳。

---

## 在同一台机器上构建并运行

```bash
git clone <this-repo> agentbob
cd agentbob
docker compose build           # 首次约 5-8 分钟（要下载 chromium）
docker compose up -d
docker compose logs -f bob
```

## 构建机与运行机不同

**方案 A —— 在目标机上重新构建（推荐，最简单）。** 在目标主机上 clone 然后 `docker compose build`，它会为那台机器的 CPU 原生构建。

**方案 B —— 经镜像仓库出多架构镜像**，适用于无法在目标机构建的情况：

```bash
# 在开发机上，一次性
docker buildx create --use --name multiarch
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t <registry>/<your-name>/bob:latest \
  --push .

# 在每台目标主机上
docker pull <registry>/<your-name>/bob:latest
docker tag  <registry>/<your-name>/bob:latest bob:latest
docker compose up -d           # 用本地缓存里的 bob:latest
```

---

## 全新 Linux 服务器从零走一遍

一台干净的 Ubuntu 或 Debian，你有 root SSH，端到端。

### 1. Docker 引擎与 compose 插件

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
```

### 2. 规划目录

两个位置：仓库 clone 到哪，以及持久状态放在哪（会被 bind-mount 进容器）。下面的路径只是一种约定，按你主机的习惯来即可。

```bash
mkdir -p /srv/agentbob-dev /srv/agentbob-state/bob-home
```

### 3. Clone

```bash
cd /srv/agentbob-dev
git clone <this-repo> agentbob
cd agentbob
```

### 4. 把 `${HOME}/.bob` 指向状态目录

`docker-compose.yml` 挂载的是 `${HOME}/.bob`。做个软链让挂载点落到正确位置——或者直接改 compose 文件用绝对路径。

```bash
ln -snf /srv/agentbob-state/bob-home /root/.bob
```

### 5. 配置文件

`$BOB_HOME` 下有三个文件，只有第一个是必需的。

**`.env` —— 机密与数据库。** 没有 `BOB_POSTGRES_DSN` bob 拒绝启动；compose **不**自带数据库，请自备一个可达的 Postgres。

```bash
cat > /srv/agentbob-state/bob-home/.env <<'EOF'
BOB_POSTGRES_DSN=postgres://user:pass@<db-host>:5432/bob?sslmode=disable
TELEGRAM_BOT_TOKEN=<从 BotFather 拿到的 token>
EOF
```

**`models.yaml` —— 你的模型在哪。** 用局域网 IP；`host.docker.internal` 在 Linux 上不工作。

```bash
cat > /srv/agentbob-state/bob-home/models.yaml <<'EOF'
entries:
  - name: smart
    provider: llama-cpp
    model: <你的模型 id>
    base_url: http://<model-host>:11434/v1
    tags: [smart, local, toolcall]
  - name: small
    provider: llama-cpp
    model: <你的小模型 id>
    base_url: http://<model-host>:11433/v1
    tags: [small, fast, local, vision]
EOF
```

**`config.yaml` —— 可选。** bob 跑在默认值上就行。只有当你想调整工具执行、附件保留策略、文件系统读取根这类设置时才需要它。

### 6. 属主

UID 1000 对应容器内的 `bob` 用户，这样容器才能写日志、状态、沙箱和记忆。不论你主机上的 UID 是多少，Dockerfile 都钉死 1000——主机侧的属主名对 Docker 不重要。

```bash
chown -R 1000:1000 /srv/agentbob-state/bob-home
```

### 7. 构建并启动

```bash
docker compose build
docker compose up -d
docker exec bob tail -20 /data/.bob/logs/agent.log
```

首次启动健康的话应该看到：`bob starting version=…`、`bob serving sources=…`，以及 `seeded sources/telegram.yaml (closed-default)`。

---

## 给自己开通聊天权限

`sources/telegram.yaml` 生成时**默认是关的**——每一条进来的消息都会被丢弃，直到你打开它。这是刻意的：一个有 shell 权限、且任何找到 bot handle 的人都能触达的 agent，等于一个敞开的 shell。

要么对所有人开放（单人开发机可以这么干）：

```bash
sed -i 's/^allow_all: false$/allow_all: true/' \
  /srv/agentbob-state/bob-home/sources/telegram.yaml
```

要么只放行指定的 user ID（在 Telegram 里用 `@userinfobot` 查你自己的）：

```bash
cat >> /srv/agentbob-state/bob-home/sources/telegram.yaml <<'EOF'
allowlist:
  - "<你的 telegram user id>"
admins:
  - "<你的 telegram user id>"
EOF
```

然后 `docker restart bob`。如果跳过这步，每条进来的消息都会记一行：

```
WARN telegram: drop — user not authorized user_id=... hint=edit $BOB_HOME/sources/telegram.yaml ...
```

---

## 检查模型是否可达

```bash
# 从服务器本身发起——应该返回 200
curl -s -o /dev/null -w '%{http_code}\n' http://<model-host>:11434/v1/models

# 从容器内部发起——用来证明容器也够得着。
# 当模型服务在另一台主机或另一个 VLAN 上时，这一步很重要。
docker exec bob curl -s -o /dev/null -w '%{http_code}\n' http://<model-host>:11434/v1/models 2>&1 || \
  echo "容器里没有 curl —— 首次调用时 bob 会在 agent.log 里报 'connection refused'"
```

Docker 的 bridge 网络默认就能访问局域网 IP。只有当模型服务与容器同主机、且绑在 `127.0.0.1` 上时，才需要 `network_mode: host` 这类覆盖配置。

---

## 迁移主机

状态分两处：**Postgres** 装会话与记忆，`~/.bob/` 装其余一切（附件、技能、浏览器 profile、日志、沙箱）。两边都带上，目标机的架构就无所谓。

```bash
# 旧主机上
docker compose down
tar -czf bob-state.tgz -C ~ .bob
pg_dump <你的 bob 数据库> > bob-db.sql

# 新主机上（CPU 架构不同也没关系）
tar -xzf bob-state.tgz -C ~
# 恢复数据库，然后把 BOB_POSTGRES_DSN 指过去
docker compose up -d
```

会话、记忆、已登录的浏览器 profile 都会跟着回来。
