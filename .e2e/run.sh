#!/bin/sh
# 跑一次 AP 侧 agent 的端到端验证。
#
# 把 .e2e/seed 的夹具复制到 .e2e/root（运行目录，已 gitignore），再以共享
# 网络命名空间的方式连到正在运行的控制器容器：
#
#   ./.e2e/run.sh --mac 8c:de:52:11:22:66 --model XR1710G once
#
# 每次运行都会重置 .e2e/root，所以上一轮的 state.json 与备份不会影响本轮结论。
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(dirname "$here")

if ! docker inspect ac-controller >/dev/null 2>&1; then
	echo "ac-controller 容器不存在，先启动控制器（make up-dev）" >&2
	exit 1
fi

secret=${AC_SECRET:-}
if [ -z "$secret" ] && [ -f "$repo/.env" ]; then
	secret=$(grep -E '^AC_SHARED_SECRET=' "$repo/.env" | cut -d= -f2-)
fi

rm -rf "$here/root"
mkdir -p "$here/root/etc/config"
cp -a "$here/seed/etc/config/." "$here/root/etc/config/"

# Build from inside the repo with a relative package path: an absolute MSYS
# path (/d/...) is not a valid Go import path.
( cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -o "$here/ac-agent" ./cmd/acagent )

# Build with "." as context: passing an MSYS-style absolute path as the context
# makes the Windows docker CLI look for a Dockerfile that does not exist.
( cd "$here" && docker build -q -t ac-agent-e2e . >/dev/null )

# The fake ubus needs to know which AP it is pretending to be, so a two-AP run
# produces two distinct BSSIDs. Without that the agent's 11k self-report is
# identical on every AP and the controller would treat each as its own peer.
e2e_mac=""
prev=""
for arg in "$@"; do
	[ "$prev" = "--mac" ] && e2e_mac="$arg"
	prev="$arg"
done

exec docker run --rm \
	--network container:ac-controller \
	-v "$here/root:/root" \
	-e AC_ADDR="${AC_ADDR:-127.0.0.1:8080}" \
	-e AC_SECRET="$secret" \
	-e AC_E2E_MAC="$e2e_mac" \
	ac-agent-e2e "$@"
