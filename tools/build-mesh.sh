#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GO:=go}"
: "${APK:=apk}"
# 源码增量默认相对于本次 Mesh 扩展的上游基线，提交后构建也能完整还原改动。
: "${SOURCE_BASE:=abf5013fe91a8cfdce446b0e1175aa003bdb1045}"
export SOURCE_BASE
version=0.3.1-aimesh-probe.1
mkdir -p dist
commit=$(git rev-parse --short HEAD)
flags="-s -w -X github.com/naoki66/ac-controller/shared/version.Version=$version -X github.com/naoki66/ac-controller/shared/version.Commit=$commit-mesh"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO" build -p 2 -trimpath -ldflags "$flags" -o dist/ac-controller-linux-arm64 ./controller/cmd/ac
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO" build -p 2 -trimpath -ldflags "$flags" -o dist/ac-agent-linux-arm64 ./agent/cmd/acagent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO" build -p 2 -trimpath -ldflags "$flags" -o dist/ac-aimesh-linux-arm64 ./tools/cmd/ac-aimesh
export APK
fakeroot python3 tools/package-mesh.py
fakeroot python3 tools/verify-mesh-packages.py
git diff --binary "$SOURCE_BASE" > dist/mesh-source.patch
git ls-files --others --exclude-standard -z | python3 tools/archive-mesh-additions.py
python3 tools/finalize-mesh-release.py
echo "Built packages and source delta in $PWD/dist"
