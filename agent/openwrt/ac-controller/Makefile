# OpenWrt package: AC controller (management side).
#
# This repository's agent/openwrt directory is a feed root shared with ac-agent.
# On OpenWrt main a git feed can be pointed at that subdirectory:
#
#   src-git --root=agent/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git
#
# Older feeds scripts (24.10 and earlier) have no --root flag; use a local
# checkout instead:
#
#   src-link ac_agent /path/to/ac-controller/agent/openwrt
#
# The controller is a single static Go binary with the web UI embedded, so the
# package is just the binary, a procd init script and one UCI file. It does not
# depend on LuCI: the UI is served on TCP 8090 by the controller itself.
#
# The controller is compiled from this repository's git source, not from a
# prebuilt binary. PKG_SOURCE_VERSION follows the default branch instead of
# pinning a commit, so a fresh feed build picks up the latest controller code
# without any Makefile edit. Override it locally with a full SHA to reproduce
# an older build. As with ac-agent, the dl/ tarball is named after PKG_VERSION
# and is cached: bump the version for a release or delete
# dl/ac-controller-*.tar.zst to force a fresh clone. PKG_MIRROR_HASH must stay
# "skip" because the branch tip moves.
#
# The package needs the packages feed for golang-package.mk, and the buildroot's
# Go must satisfy the go directive in go.mod.

include $(TOPDIR)/rules.mk

PKG_NAME:=ac-controller
PKG_VERSION:=0.1.0
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE
PKG_MAINTAINER:=naoki66 <naoki66@163.com>

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/naoki66/OpenWrt_AC-Controller.git
# Follow the default branch; see the note at the top of this file.
PKG_SOURCE_VERSION:=main
PKG_MIRROR_HASH:=skip

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1

GO_PKG:=github.com/naoki66/ac-controller
# Import path, not a relative one: go runs from the .go_work GOPATH root and
# resolves the module through the go.mod in PKG_BUILD_DIR. The controller main
# package lives at controller/cmd/ac on the main branch.
GO_PKG_BUILD_PKG:=github.com/naoki66/ac-controller/controller/cmd/ac
# No Commit injection for the same reason as ac-agent: releases record the
# exact source commit in build-info.txt instead.
GO_PKG_LDFLAGS_X:= \
	github.com/naoki66/ac-controller/shared/version.Version=$(PKG_VERSION)

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/ac-controller
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Web AC controller for OpenWrt APs
  URL:=https://github.com/naoki66/OpenWrt_AC-Controller
  DEPENDS:=$(GO_ARCH_DEPENDS)
endef

define Package/ac-controller/conffiles
/etc/config/ac-controller
endef

# GoBinPackage would install the build binary as /usr/bin/ac; a generic name
# like that is too easy to collide with on a router, so install it explicitly
# under the service name and point the init script at it.
define Package/ac-controller/install
	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/ac $(1)/usr/sbin/ac-controller

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/ac-controller.init $(1)/etc/init.d/ac-controller

	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/ac-controller.config $(1)/etc/config/ac-controller

	# Keep the controller identity and templates across sysupgrade. Without this
	# the box comes back as a fresh controller and every AP has to be adopted
	# again, because the pinned signing key no longer matches.
	$(INSTALL_DIR) $(1)/lib/upgrade/keep.d
	$(INSTALL_DATA) ./files/ac-controller.keep $(1)/lib/upgrade/keep.d/ac-controller
endef

$(eval $(call GoBinPackage,ac-controller))
$(eval $(call BuildPackage,ac-controller))
