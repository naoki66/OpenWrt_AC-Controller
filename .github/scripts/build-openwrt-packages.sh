#!/usr/bin/env bash
# Build the AP-side packages inside an official OpenWrt SDK container.
#
# Called by .github/workflows/openwrt-packages.yml:
#   docker run --rm --entrypoint /bin/bash \
#     -v <repo>/agent/openwrt:/feed:ro \
#     -v <repo>/.github/scripts:/scripts:ro \
#     -v <artifacts>:/artifacts \
#     -e SDK_TAG -e REQUIRED_GO -e BUILD_CONTROLLER \
#     ghcr.io/openwrt/sdk:<tag> /scripts/build-openwrt-packages.sh
#
# The whole repository is deliberately NOT mounted as the feed: agent/openwrt
# is the feed root, and a src-link to that directory gives us exactly the three
# package Makefiles without the Go sources being scanned as packages.

set -euo pipefail

: "${SDK_TAG:?SDK_TAG is required}"

echo "::group::Locate SDK root ($SDK_TAG)"
# Snapshot images ship a setup.sh that downloads/extracts the SDK first.
if [ -f setup.sh ]; then
    bash setup.sh
fi
if [ ! -f feeds.conf.default ]; then
    sdk_dir=$(find / -maxdepth 5 -name feeds.conf.default -type f -printf '%h\n' 2>/dev/null | head -n 1)
    if [ -z "$sdk_dir" ]; then
        echo "::error::Cannot locate the extracted SDK (feeds.conf.default)"
        exit 1
    fi
    cd "$sdk_dir"
fi
pwd
echo "::endgroup::"

echo "::group::Pre-seed Go source tarballs"
# The feed Makefiles declare a git source, but cloning this repository from
# inside the SDK container failed with "Repository not found / Authentication
# failed". Seed the dl/ cache with a tarball of the exact CI commit instead:
# download.pl reuses <name>-<version>.tar.zst verbatim when PKG_MIRROR_HASH is
# "skip", so make never attempts a download. This is both faster and exactly
# reproducible against $GITHUB_SHA.
if [ -d /dl ] && compgen -G '/dl/*.tar.zst' > /dev/null; then
    mkdir -p dl
    cp -f /dl/*.tar.zst dl/
    ls -lh dl/*.tar.zst
else
    echo "::warning::No /dl seed mount; falling back to the feed's git download"
fi
echo "::endgroup::"

echo "::group::Configure feeds"
# feeds.conf.default points at git.openwrt.org; GitHub mirrors are much more
# reliable from CI. Same rewrite used by openwrt/gh-action-sdk.
sed -e 's,https://git.openwrt.org/feed/,https://github.com/openwrt/,' \
    -e 's,https://git.openwrt.org/openwrt/,https://github.com/openwrt/,' \
    -e 's,https://git.openwrt.org/project/,https://github.com/openwrt/,' \
    feeds.conf.default > feeds.conf
printf 'src-link ac_agent /feed/\n' >> feeds.conf
cat feeds.conf
echo "::endgroup::"

echo "::group::feeds update / install"
./scripts/feeds update -a
# golang/host provides the Go toolchain and golang-package.mk; luci-base is the
# metadata dependency of luci-app-ac-agent. Install only what we need instead
# of `feeds install -a`, which would link several thousand packages.
./scripts/feeds install -p packages golang
./scripts/feeds install -p luci luci-base \
    || echo "::warning::luci-base could not be installed; luci.mk metadata may warn"
./scripts/feeds install -p ac_agent -f -a
echo "::endgroup::"

echo "::group::Go toolchain compatibility"
# go.mod declares a newer Go than most SDKs ship. OpenWrt forces
# GOTOOLCHAIN=local in golang-package.mk, which makes `go build` fail with
# "go.mod requires go >= X (GOTOOLCHAIN=local)". Relax it inside this throwaway
# container so the host Go can fetch the declared toolchain from the module
# proxy. GitHub-hosted runners have outbound access to proxy.golang.org.
golang_mk=feeds/packages/lang/golang/golang-package.mk
if [ -f "$golang_mk" ] && grep -q 'GOTOOLCHAIN=local' "$golang_mk"; then
    sed -i 's/GOTOOLCHAIN=local/GOTOOLCHAIN=auto/' "$golang_mk"
    echo "::notice::$SDK_TAG: GOTOOLCHAIN=auto (go.mod requires Go ${REQUIRED_GO:-unknown})"
else
    echo "::notice::$SDK_TAG: golang-package.mk has no GOTOOLCHAIN=local override"
fi
echo "::endgroup::"

echo "::group::Build packages"
make defconfig
# CONFIG_SIGNED_PACKAGES= keeps the packages unsigned: installing them from a
# local file already needs --allow-untrusted on apk, and no signing key exists
# in CI. gh-action-sdk passes the same empty override.
make V=s CONFIG_SIGNED_PACKAGES= "-j$(nproc)" package/ac-agent/compile
make V=s CONFIG_SIGNED_PACKAGES= "-j$(nproc)" package/luci-app-ac-agent/compile
if [ "${BUILD_CONTROLLER:-false}" = "true" ]; then
    make V=s CONFIG_SIGNED_PACKAGES= "-j$(nproc)" package/ac-controller/compile
fi
echo "::endgroup::"

echo "::group::Collect packages"
mkdir -p bin /artifacts
# luci.mk also emits luci-i18n-ac-agent-zh-cn; all our assets contain "ac-".
find bin -type f \( -name '*.ipk' -o -name '*.apk' \) -name '*ac-*' \
    -exec cp -f {} /artifacts/ \;
count=$(find /artifacts -maxdepth 1 -type f \( -name '*.ipk' -o -name '*.apk' \) | wc -l)
if [ "$count" -eq 0 ]; then
    echo "::error::No ipk/apk produced for $SDK_TAG"
    find bin -maxdepth 5 -type f | head -n 50 || true
    exit 1
fi
echo "::notice::$SDK_TAG produced $count package(s)"
ls -lh /artifacts
echo "::endgroup::"
