# OpenWrt package: AP-side agent (ac-agent).
#
# This repository's deploy/openwrt directory is a feed root. On OpenWrt main a
# git feed can be pointed at that subdirectory:
#
#   src-git --root=deploy/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git
#
# Older feeds scripts (24.10 and earlier) have no --root flag; use a local
# checkout instead:
#
#   src-link ac_agent /path/to/ac-controller/deploy/openwrt
#
# The agent is compiled from the repository's git source, not from a prebuilt
# binary. PKG_SOURCE_VERSION is pinned: bump it together with PKG_VERSION when
# the agent code changes, otherwise a feed update keeps building the old commit.
#
# This package needs the packages feed for golang-package.mk, and the buildroot's
# Go must satisfy the go directive in go.mod.

include $(TOPDIR)/rules.mk

PKG_NAME:=ac-agent
PKG_VERSION:=0.1.0
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE
PKG_MAINTAINER:=naoki66 <naoki66@163.com>

PKG_SOURCE_PROTO:=git
PKG_SOURCE_URL:=https://github.com/naoki66/OpenWrt_AC-Controller.git
PKG_SOURCE_VERSION:=5255f1337edfbe326b396176b9943dcbbaf65097
# No mirror tarball is published for this git source.
PKG_MIRROR_HASH:=skip

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1

GO_PKG:=github.com/naoki66/ac-controller
# Import path, not a relative one: go runs from the .go_work GOPATH root and
# resolves the module through the go.mod in PKG_BUILD_DIR.
GO_PKG_BUILD_PKG:=github.com/naoki66/ac-controller/cmd/acagent
GO_PKG_LDFLAGS_X:= \
	github.com/naoki66/ac-controller/internal/version.Version=$(PKG_VERSION) \
	github.com/naoki66/ac-controller/internal/version.Commit=$(PKG_SOURCE_VERSION)

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
endef

$(eval $(call GoBinPackage,ac-agent))
$(eval $(call BuildPackage,ac-agent))
