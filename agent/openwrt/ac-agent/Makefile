# OpenWrt package: AP-side agent (ac-agent).
#
# This repository's agent/openwrt directory is a feed root. On OpenWrt main a
# git feed can be pointed at that subdirectory:
#
#   src-git --root=agent/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git
#
# Older feeds scripts (24.10 and earlier) have no --root flag; use a local
# checkout instead:
#
#   src-link ac_agent /path/to/ac-controller/agent/openwrt
#
# The agent is compiled from the repository's git source, not from a prebuilt
# binary. PKG_SOURCE_VERSION follows the default branch instead of pinning a
# commit, so a fresh feed build picks up the latest agent code without any
# Makefile edit. To reproduce an older build, override PKG_SOURCE_VERSION
# locally with a full commit SHA (make ... PKG_SOURCE_VERSION=<sha>).
#
# Two consequences of a moving branch:
#   * include/download.mk names the source tarball <name>-<PKG_VERSION>.tar.zst,
#     so an existing dl/ cache is reused verbatim. Bump PKG_VERSION /
#     PKG_RELEASE when releasing, or delete dl/ac-agent-*.tar.zst to force a
#     fresh clone of main.
#   * PKG_MIRROR_HASH must stay "skip": the tarball content is not reproducible.
#
# This package needs the packages feed for golang-package.mk, and the buildroot's
# Go must satisfy the go directive in go.mod. Older SDKs ship an older Go and
# set GOTOOLCHAIN=local; the release workflow relaxes that in its SDK container.

include $(TOPDIR)/rules.mk

PKG_NAME:=ac-agent
PKG_VERSION:=0.3.0
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE
PKG_MAINTAINER:=naoki66 <naoki66@163.com>

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/naoki66/OpenWrt_AC-Controller.git
# Follow the default branch. With PKG_MIRROR_HASH=skip OpenWrt's fast GitHub
# archive path is skipped, falling back to `git clone` + `git checkout main`,
# which resolves to the branch tip on every fresh download.
PKG_SOURCE_VERSION:=main
# Mandatory for a moving ref: the tarball has no stable checksum.
PKG_MIRROR_HASH:=skip

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1

GO_PKG:=github.com/naoki66/ac-controller
# Import path, not a relative one: go runs from the .go_work GOPATH root and
# resolves the module through the go.mod in PKG_BUILD_DIR. The agent main
# package lives at agent/cmd/acagent and the version package at shared/version
# on the main branch.
GO_PKG_BUILD_PKG:=github.com/naoki66/ac-controller/agent/cmd/acagent
# Commit is deliberately not injected: with a moving branch there is no stable
# hash at Makefile parse time. Each release ships build-info.txt with the exact
# commit that produced the published packages.
GO_PKG_LDFLAGS_X:= \
	github.com/naoki66/ac-controller/shared/version.Version=$(PKG_VERSION)
ifneq ($(strip $(AC_CONTROLLER_PUBKEY)),)
GO_PKG_LDFLAGS_X += github.com/naoki66/ac-controller/agent.FirmwareControllerKey=$(AC_CONTROLLER_PUBKEY)
endif

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/ac-agent
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Agent for the OpenWrt AC controller
  URL:=https://github.com/naoki66/OpenWrt_AC-Controller
  DEPENDS:=$(GO_ARCH_DEPENDS)
endef

define Package/ac-agent/conffiles
/etc/config/ac
endef

# GoBinPackage's default install would drop the binary under its build name
# ("acagent") in /usr/bin; the init script and every doc expect ac-agent in
# /usr/sbin, so install it explicitly.
define Package/ac-agent/install
	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/acagent $(1)/usr/sbin/ac-agent

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/ac-agent.init $(1)/etc/init.d/ac-agent

	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/ac.config $(1)/etc/config/ac

	$(INSTALL_DIR) $(1)/lib/upgrade/keep.d
	$(INSTALL_DATA) ./files/ac-agent.keep $(1)/lib/upgrade/keep.d/ac-agent
endef

$(eval $(call GoBinPackage,ac-agent))
$(eval $(call BuildPackage,ac-agent))
