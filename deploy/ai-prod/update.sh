#!/usr/bin/env bash
# 把 minimaxcode2api 切到指定镜像版本（CI 发布在 GHCR）。
#
#   ./update.sh 0.5.1    升级/回滚到 0.5.1
#   ./update.sh          查看当前镜像与用法
#
# 幂等：重复跑同一版本不会产生副作用（pull 命中缓存、compose 不重建）。
set -euo pipefail

IMAGE=ghcr.io/rsfree/minimaxcode2api
DIR=$(cd "$(dirname "$0")" && pwd)
ENV_FILE="$DIR/.env"
VERSION="${1:-}"

command -v docker >/dev/null || { echo "缺少 docker"; exit 1; }

if [ -z "$VERSION" ]; then
  echo "当前容器镜像：$(docker inspect -f '{{.Config.Image}}' minimaxcode2api 2>/dev/null || echo '<未运行>')"
  echo "用法：./update.sh <版本，如 0.5.1>"
  exit 0
fi

VERSION="${VERSION#v}"   # 允许传 v0.5.1

# .env 里幂等写入 IMAGE_TAG（compose 用 ${IMAGE_TAG:-latest} 取值）
set_env() {
  local key=$1 val=$2
  [ -f "$ENV_FILE" ] || { echo "缺少 $ENV_FILE（至少要含 MMX2A_API_KEY）"; exit 1; }
  chmod 600 "$ENV_FILE"
  if grep -q "^${key}=" "$ENV_FILE"; then
    sed -i "s|^${key}=.*|${key}=${val}|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$val" >> "$ENV_FILE"
  fi
}

echo "→ 拉取 ${IMAGE}:${VERSION}"
if ! docker pull "${IMAGE}:${VERSION}"; then
  cat <<'EOF'
拉取失败，常见两种原因：
  1) 包是私有的 ⇒ docker login ghcr.io -u <github用户> -p <PAT，需 read:packages>
  2) 本机架构与镜像不匹配（CI 当前只出 linux/amd64）⇒ 用 uname -m 确认是否 x86_64
EOF
  exit 1
fi

set_env IMAGE_TAG "$VERSION"
cd "$DIR"
docker compose up -d

# 健康检查：容器内 /healthz 在零账号时也返回 200，可当存活探针
printf '→ 等待 /healthz'
for _ in $(seq 1 30); do
  if docker exec minimaxcode2api wget -qO- http://127.0.0.1:7864/healthz > /tmp/mmx2a-hz.json 2>/dev/null; then
    echo
    cat /tmp/mmx2a-hz.json; echo
    docker inspect -f '镜像 digest: {{index .RepoDigests 0}}' minimaxcode2api 2>/dev/null || true
    echo "✓ 已切到 ${VERSION}；回滚用 ./update.sh <上一版本>"
    exit 0
  fi
  printf '.'; sleep 1
done

echo
echo "✗ 30s 内健康检查未通过：docker logs --tail 50 minimaxcode2api"
exit 1
