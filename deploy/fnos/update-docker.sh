#!/bin/sh
# OpenWrt AC 控制器私有镜像更新脚本（fnOS / 飞牛 NAS）
#
# 用法:
#   ./update-docker.sh
#
# 凭据来源（按优先级）:
#   1. 环境变量 GHCR_TOKEN
#   2. 同目录 .ghcr_pat（推荐，chmod 600，已加入 .gitignore）
#
# 流程: 登录 ghcr → 记录当前 digest/版本 → pull → 比对 digest → 备份 data → 重建 → 健康检查
# digest 未变且容器在跑时直接退出，不重建，零停机。
set -eu

BASE_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
COMPOSE_FILE="$BASE_DIR/docker-compose.yml"
ENV_FILE="$BASE_DIR/.env"
GHCR_USERNAME="${GHCR_USERNAME:-naoki66}"
GHCR_TOKEN_FILE="${GHCR_TOKEN_FILE:-$BASE_DIR/.ghcr_pat}"
SERVICE="ac"
AC_BIN="/usr/local/bin/ac"
# 与 docker-compose.yml 的默认值保持一致
DATA_DIR="${AC_DATA_DIR_HOST:-/srv/ac-controller/data}"
HEALTH_URL="${AC_HEALTH_URL:-http://127.0.0.1:8090/api/v1/health}"

compose() { docker compose -f "$COMPOSE_FILE" "$@"; }

container_id() { compose ps -q "$SERVICE" 2>/dev/null | head -n 1; }

# 当前运行容器的版本（ac version 输出 "ac <version> (<commit>, <date>)"）
running_version() {
  container=$(container_id)
  [ -n "$container" ] || return 1
  docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null | grep -qx 'true' || return 1
  docker exec "$container" "$AC_BIN" version 2>/dev/null | head -n 1
}

# 本地镜像内的版本（pull 之后的候选版本）
image_version() {
  # --pull never: 这里只读本地已有镜像，真正拉取由下面的 compose pull 负责。
  docker run --rm --pull never --entrypoint "$AC_BIN" "$IMAGE_REF" version 2>/dev/null | head -n 1
}

# :latest 这类 tag 名不会变，用 digest 判断是否真的更新
image_digest() {
  docker image inspect -f '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' \
    "$IMAGE_REF" 2>/dev/null || true
}

healthy() {
  command -v curl >/dev/null 2>&1 || return 0
  curl -fsS --max-time 3 "$HEALTH_URL" >/dev/null 2>&1
}

if ! command -v docker >/dev/null 2>&1; then
  echo "docker command not found" >&2
  exit 1
fi

if [ ! -f "$COMPOSE_FILE" ]; then
  echo "missing compose file: $COMPOSE_FILE" >&2
  exit 1
fi

if [ -z "${AC_SHARED_SECRET:-}" ] && \
  { [ ! -f "$ENV_FILE" ] || ! grep -q '^AC_SHARED_SECRET=' "$ENV_FILE"; }; then
  echo "missing AC_SHARED_SECRET in $ENV_FILE (or environment)" >&2
  echo "Create it with an at-least-32-byte secret (permission 600):" >&2
  echo "  printf 'AC_SHARED_SECRET=%s\n' \"\$(openssl rand -base64 32)\" > \"$ENV_FILE\"" >&2
  echo "  chmod 600 \"$ENV_FILE\"" >&2
  exit 1
fi

if [ -n "${GHCR_TOKEN:-}" ]; then
  TOKEN="$GHCR_TOKEN"
elif [ -s "$GHCR_TOKEN_FILE" ]; then
  chmod 600 "$GHCR_TOKEN_FILE" 2>/dev/null || true
  TOKEN=$(tr -d '\r\n' < "$GHCR_TOKEN_FILE")
else
  echo "missing or empty GHCR token file: $GHCR_TOKEN_FILE" >&2
  echo "Create it with a GitHub PAT that has read:packages:" >&2
  echo "  printf '%s' 'ghp_... or github_pat_...' > '$GHCR_TOKEN_FILE'" >&2
  echo "  chmod 600 '$GHCR_TOKEN_FILE'" >&2
  exit 1
fi

if ! printf '%s' "$TOKEN" | docker login ghcr.io \
  --username "$GHCR_USERNAME" --password-stdin >/dev/null 2>&1; then
  echo "ghcr.io login failed: token invalid, expired, or missing read:packages" >&2
  echo "Verify with: curl -s -H \"Authorization: Bearer <token>\" https://api.github.com/user" >&2
  exit 1
fi

IMAGE_REF=$(compose config --images 2>/dev/null | head -n 1 || true)
[ -n "$IMAGE_REF" ] || IMAGE_REF="ghcr.io/$GHCR_USERNAME/openwrt_ac-controller:latest"

# 数据目录里有 ac.id 与 ac_signing.key：私钥只在这里，丢了需要重新适配所有 AP。
if [ ! -d "$DATA_DIR" ]; then
  mkdir -p "$DATA_DIR"
fi
if ! chown 1000:1000 "$DATA_DIR" 2>/dev/null; then
  echo "WARN: cannot chown $DATA_DIR to 1000:1000 (run as root on first deploy)" >&2
fi

if CURRENT_VERSION=$(running_version); then
  RUNNING=1
else
  RUNNING=0
  CURRENT_VERSION=$(image_version || true)
fi
OLD_DIGEST=$(image_digest)
echo "Current version: ${CURRENT_VERSION:-unknown}"

echo "Pulling the private image $IMAGE_REF ..."
compose pull "$SERVICE"

NEW_DIGEST=$(image_digest)
NEW_VERSION=$(image_version || true)
echo "Latest version:  ${NEW_VERSION:-unknown}"

if [ "$RUNNING" = "1" ] && [ -n "$OLD_DIGEST" ] && [ "$OLD_DIGEST" = "$NEW_DIGEST" ]; then
  echo "Image unchanged; the running controller already serves this build."
  exit 0
fi

# 升级前备份 data（含 sqlite、AC 身份与签名私钥）。备份放在部署目录，已被 .gitignore 排除。
if [ -d "$DATA_DIR" ] && [ -n "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
  ts=$(date +%Y%m%d-%H%M%S)
  backup="$BASE_DIR/data-backup-$ts.tgz"
  if tar czf "$backup" -C "$(dirname "$DATA_DIR")" "$(basename "$DATA_DIR")" 2>/dev/null; then
    echo "Backup: $backup"
  else
    echo "WARN: backup failed, continuing anyway" >&2
  fi
fi

echo "Recreating the controller container..."
compose up -d --force-recreate --remove-orphans "$SERVICE"

# 等健康检查通过；容器是 scratch 镜像，没有内部探针，只能从宿主 curl。
i=0
while [ "$i" -lt 20 ]; do
  if healthy; then
    break
  fi
  i=$((i + 1))
  sleep 1
done

if ! healthy; then
  echo "health check failed: $HEALTH_URL" >&2
  compose logs --tail 40 "$SERVICE" >&2 || true
  exit 1
fi

echo "Controller update complete: ${CURRENT_VERSION:-unknown} -> ${NEW_VERSION:-unknown}"
echo "Health: $HEALTH_URL"
