#!/usr/bin/env bash
# =============================================================================
# up.sh — 在 Ubuntu 上拉起 chat-retrieval 冷记忆叶子(docker compose)。
#
# 代码跟 repo 走(常 git pull / 重 clone);部署状态放 repo 之外的独立空间
# STATE_DIR(默认 ~/.chat-retrieval),重 clone 不丢:
#   STATE_DIR/.env   ← 三个重点:DATABASE_URL(DSN)/ EMBEDDER_BASE_URL / RERANKER_BASE_URL
#   pg 数据 / 模型     ← docker 具名卷(chat_pg_data / chat_models),本就独立于目录
#
# 内置 vs 外置 = 看地址(decouple:app 永远只是个 HTTP 客户端,不分内外):
#   DATABASE_URL host == postgres                         → 起内置 postgres
#   EMBEDDER/RERANKER host ∈ host.docker.internal/localhost → 本机起 llama-server
#   其它 host                                              → 外部服务,只连不起
#
# 新部署(STATE_DIR/.env 不存在)+ 交互终端 + 没带地址 flag → 逐个问这三个(默认外置)。
# 已部署(.env 已在)→ 不问、不理 flag,按 .env 来。无终端 → 用 flag,缺的取内置默认。
#
# flag:--db <DSN> / --embedder <URL> / --reranker <URL>(或同名大写环境变量);
#      --state-dir <path> / --rebuild / --down [--purge]。
# 端口:EMBEDDER_PORT=8080 RERANKER_PORT=8081 APP_PORT=8000。
# =============================================================================
set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")"
REPO_DIR="$PWD"

STATE_DIR="${STATE_DIR:-$HOME/.chat-retrieval}"
EMBEDDER_PORT="${EMBEDDER_PORT:-8080}"
RERANKER_PORT="${RERANKER_PORT:-8081}"
APP_PORT="${APP_PORT:-8000}"
MODEL_READY_TIMEOUT="${MODEL_READY_TIMEOUT:-900}"
APP_READY_TIMEOUT="${APP_READY_TIMEOUT:-120}"
PG_IMAGE="${PG_IMAGE:-pgvector/pgvector:pg16}"

OPT_DB="${DATABASE_URL:-}"; OPT_EMB="${EMBEDDER_BASE_URL:-}"; OPT_RER="${RERANKER_BASE_URL:-}"
REBUILD=0; DOWN=0; PURGE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --db)        OPT_DB="${2:?--db 需要一个 DSN}"; shift 2 ;;
    --embedder)  OPT_EMB="${2:?--embedder 需要一个 URL}"; shift 2 ;;
    --reranker)  OPT_RER="${2:?--reranker 需要一个 URL}"; shift 2 ;;
    --state-dir) STATE_DIR="${2:?--state-dir 需要一个路径}"; shift 2 ;;
    --rebuild)   REBUILD=1; shift ;;
    --down)      DOWN=1; shift ;;
    --purge)     PURGE=1; shift ;;
    -h|--help)   sed -n '2,33p' "$0"; exit 0 ;;
    *) echo "未知参数: $1 (用 --help)" >&2; exit 2 ;;
  esac
done

ENV_FILE="$STATE_DIR/.env"
export RETRIEVAL_ENV_FILE="$ENV_FILE"
EMB_INTERNAL="http://host.docker.internal:${EMBEDDER_PORT}/v1"
RER_INTERNAL="http://host.docker.internal:${RERANKER_PORT}"

if docker compose version >/dev/null 2>&1; then DC() { docker compose "$@"; }
elif command -v docker-compose >/dev/null 2>&1; then DC() { docker-compose "$@"; }
else echo "✗ 找不到 docker compose(装 docker.io + docker-compose-plugin)" >&2; exit 1; fi
MAIN="-f docker-compose.yml"; GPU="-f gpu-server/docker-compose.yml"
log() { printf '\033[1m▶ %s\033[0m\n' "$*"; }

set_env_kv() {  # FILE KEY VALUE —— grep -v + printf,URL/DSN 特殊字符安全
  local f="$1" k="$2" v="$3"; grep -v "^${k}=" "$f" > "$f.tmp" 2>/dev/null || true
  printf '%s=%s\n' "$k" "$v" >> "$f.tmp"; mv "$f.tmp" "$f"
}
url_host() { local u="${1#*://}"; u="${u##*@}"; printf '%s' "${u%%[:/?]*}"; }
val_of() { grep "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2-; }

# --- 拆栈 ------------------------------------------------------------------
if [[ $DOWN -eq 1 ]]; then
  vol=""; [[ $PURGE -eq 1 ]] && vol="-v"
  log "停 app(+postgres)${vol:+ 并删具名卷}"; DC $MAIN down $vol || true
  log "停 embedder + reranker${vol:+ 并删模型卷}"; DC $GPU down $vol || true
  echo "✓ 已停($ENV_FILE 配置保留)。"; exit 0
fi
docker info >/dev/null 2>&1 || { echo "✗ docker 守护进程没起:sudo systemctl start docker" >&2; exit 1; }

# --- 1) 独立空间的 .env -----------------------------------------------------
ask_external() {  # PROMPT  -> echo 用户输入(可空)
  local p="$1" v; printf '  %s' "$p" >&2; read -r v || true; printf '%s' "$v"
}
prompt_service() {  # KEY  LABEL  INTERNAL_VALUE  [MANAGED_KEY]
  # 内置/外置由你选的 i/E 决定,显式记进 MANAGED_KEY(1=本脚本起本机服务,0=连外部),
  # 不再从地址猜 —— tunnel/外部服务恰好在 127.0.0.1/host.docker.internal 时也能正确判为外置。
  local key="$1" label="$2" internal="$3" mkey="${4:-}" ans u
  printf '%s internal / external? [i/E]: ' "$label" >&2; read -r ans || true
  case "$ans" in
    [iI]*)
      [[ -n "$mkey" ]] && set_env_kv "$ENV_FILE" "$mkey" 1
      if [[ -n "$internal" ]]; then set_env_kv "$ENV_FILE" "$key" "$internal"; fi
      log "$label → 内置${internal:+ ($internal)}"
      ;;
    *)  # 回车=external。留空=暂不配(写空,下面 guard 拦下提示填了再重跑)。
      [[ -n "$mkey" ]] && set_env_kv "$ENV_FILE" "$mkey" 0
      u="$(ask_external "$key: ")"
      set_env_kv "$ENV_FILE" "$key" "$u"
      log "$label → 外置 ${u:-(留空 —— 待填 $ENV_FILE 后重跑)}"
      ;;
  esac
}

if [[ ! -f "$ENV_FILE" ]]; then
  mkdir -p "$STATE_DIR"; cp "$REPO_DIR/app/.env.example" "$ENV_FILE"; log "新部署:已生成 $ENV_FILE"
  if [[ -t 0 && -z "$OPT_DB$OPT_EMB$OPT_RER" ]]; then
    echo "== 配置三个服务(回车=external 并输入地址;输 i = 内置由本脚本起)=="
    prompt_service DATABASE_URL      "[1/3] 数据库(内置 postgres)"            ""                              # 内置=保留默认 DSN(host=postgres)
    prompt_service EMBEDDER_BASE_URL "[2/3] Embedding(内置 llama-server GGUF)" "$EMB_INTERNAL" EMBEDDER_MANAGED
    prompt_service RERANKER_BASE_URL "[3/3] Reranker(内置 llama-server GGUF)"  "$RER_INTERNAL" RERANKER_MANAGED
  else
    # 非交互(无终端或带了 flag):给了地址 flag=外置(MANAGED=0),没给=内置默认(MANAGED=1)
    [[ -n "$OPT_DB"  ]] && set_env_kv "$ENV_FILE" DATABASE_URL "$OPT_DB"
    if [[ -n "$OPT_EMB" ]]; then set_env_kv "$ENV_FILE" EMBEDDER_MANAGED 0; set_env_kv "$ENV_FILE" EMBEDDER_BASE_URL "$OPT_EMB"
    else set_env_kv "$ENV_FILE" EMBEDDER_MANAGED 1; set_env_kv "$ENV_FILE" EMBEDDER_BASE_URL "$EMB_INTERNAL"; fi
    if [[ -n "$OPT_RER" ]]; then set_env_kv "$ENV_FILE" RERANKER_MANAGED 0; set_env_kv "$ENV_FILE" RERANKER_BASE_URL "$OPT_RER"
    else set_env_kv "$ENV_FILE" RERANKER_MANAGED 1; set_env_kv "$ENV_FILE" RERANKER_BASE_URL "$RER_INTERNAL"; fi
    log "非交互新部署:flag/内置默认已写入 $ENV_FILE"
  fi
else
  [[ -n "$OPT_DB$OPT_EMB$OPT_RER" ]] && log "已部署:忽略命令行地址 flag —— 改请直接编辑 $ENV_FILE"
fi

# --- 2) 读生效地址,判定内置/外置 -----------------------------------------
DSN="$(val_of DATABASE_URL)"; EMB="$(val_of EMBEDDER_BASE_URL)"; RER="$(val_of RERANKER_BASE_URL)"
# 空地址 = 外置但没填(不是内置)。只有 host==postgres 才算内置库;空 → 外置(下面 guard 拦)。
[[ "$(url_host "$DSN")" == "postgres" ]] && DB_MODE=internal || DB_MODE=external
EMB_LOCAL=0; [[ "$(val_of EMBEDDER_MANAGED)" == "1" ]] && EMB_LOCAL=1
RER_LOCAL=0; [[ "$(val_of RERANKER_MANAGED)" == "1" ]] && RER_LOCAL=1
log "DB=$DB_MODE  embedder=$([[ $EMB_LOCAL -eq 1 ]] && echo 内置 || echo 外置)  reranker=$([[ $RER_LOCAL -eq 1 ]] && echo 内置 || echo 外置)"

# 选了外置却没填地址(空)→ 绝不退回内置、不起任何容器,提示填了再来。
missing=()
[[ -z "$DSN" ]] && missing+=("DATABASE_URL")
[[ -z "$EMB" ]] && missing+=("EMBEDDER_BASE_URL")
[[ -z "$RER" ]] && missing+=("RERANKER_BASE_URL")
if (( ${#missing[@]} )); then
  echo "✗ 这些地址还空着(选了外置但没填),没启动任何容器。填 $ENV_FILE 或重跑带 --db/--embedder/--reranker:${missing[*]}" >&2
  exit 1
fi

# --- 3) 本机模型(只起内置的)---------------------------------------------
wait_ready() {  # name url json
  local name="$1" url="$2" data="$3" deadline=$(( SECONDS + MODEL_READY_TIMEOUT ))
  log "等 $name 就绪(下模型+加载,可能数分钟)…"
  while (( SECONDS < deadline )); do
    curl -fsS -m 10 -X POST "$url" -H 'Content-Type: application/json' -d "$data" >/dev/null 2>&1 && { echo "  ✓ $name 就绪"; return 0; }
    sleep 5
  done
  echo "  ✗ $name 超时未就绪(DC $GPU logs -f)" >&2; return 1
}
if [[ $EMB_LOCAL -eq 1 ]]; then
  log "起内置 embedder(:${EMBEDDER_PORT})"; DC $GPU up -d embedder
  wait_ready embedder "http://localhost:${EMBEDDER_PORT}/v1/embeddings" '{"input":"ping","model":"x"}'
fi
if [[ $RER_LOCAL -eq 1 ]]; then
  log "起内置 reranker(:${RERANKER_PORT})"; DC $GPU up -d reranker
  wait_ready reranker "http://localhost:${RERANKER_PORT}/rerank" '{"query":"a","documents":["b","c"]}'
fi

# --- 4) 外部库:只检查扩展(不 create,app 账号无权建);缺了报错给命令。
#         内置库的扩展由其 pg 初始化时以超管创建(schema.sql 里的 CREATE EXTENSION
#         只服务那条路;外部这条永不执行到 create —— 缺就在这退出,有就往下灌表)。
if [[ "$DB_MODE" == external ]]; then
  log "外部库:检查必需扩展 vector / pg_trgm(只查不建)"
  have="$(docker run --rm --add-host=host.docker.internal:host-gateway "$PG_IMAGE" \
      psql "$DSN" -tAc "select string_agg(extname,',' order by extname) from pg_extension where extname in ('vector','pg_trgm')" 2>&1)" \
    || { echo "✗ 外部库连不上(检查 DSN / 网络):$have" >&2; exit 1; }
  if [[ "$have" != *vector* || "$have" != *pg_trgm* ]]; then
    echo "✗ 外部库缺扩展(当前:${have:-无})。app 账号无权建,请用超管在【该 DSN 连的库】执行后重跑:" >&2
    echo "      CREATE EXTENSION IF NOT EXISTS vector;" >&2
    echo "      CREATE EXTENSION IF NOT EXISTS pg_trgm;" >&2
    exit 1
  fi
  log "外部库:扩展就绪($have),应用 schema.sql(建表/索引)"
  docker run --rm --add-host=host.docker.internal:host-gateway \
    -v "$REPO_DIR/schema.sql:/schema.sql:ro" "$PG_IMAGE" \
    psql "$DSN" -v ON_ERROR_STOP=1 -f /schema.sql \
    || { echo "✗ schema 应用失败" >&2; exit 1; }
  echo "  ✓ schema 就绪"
fi

# --- 5) 起 app -------------------------------------------------------------
build=""; [[ $REBUILD -eq 1 ]] && build="--build"
if [[ "$DB_MODE" == external ]]; then
  log "起 app(:${APP_PORT},外部库)"; DC $MAIN up -d --no-deps $build app
else
  log "起 postgres + app(:${APP_PORT})"; DC $MAIN up -d $build
fi

# --- 6) health + selftest --------------------------------------------------
log "等 app /health…"; deadline=$(( SECONDS + APP_READY_TIMEOUT ))
until curl -fsS -m 5 "http://localhost:${APP_PORT}/health" >/dev/null 2>&1; do
  (( SECONDS < deadline )) || { echo "  ✗ app 未健康(DC $MAIN logs -f app)" >&2; exit 1; }
  sleep 3
done
echo "  ✓ app 健康"
log "/selftest(验 db / pgvector≥0.8 / embedder 维度 / reranker)"
if curl -fsS -m 60 "http://localhost:${APP_PORT}/selftest"; then
  echo; echo "✓ 就绪。bob config 的 retrieval.base_url 指到 http://<本机>:${APP_PORT}/。"
else
  echo; echo "✗ /selftest 没过 —— 看 $ENV_FILE 三个地址 / 模型 / 外部库 schema。" >&2; exit 1
fi
