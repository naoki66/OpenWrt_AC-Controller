# LuCI application: AC agent status.
#
# Packages the web UI and its rpcd backend only. The agent binary comes from the
# sibling ac-agent package (or from `make agent` when installing by hand) and is
# installed at /usr/sbin/ac-agent.
#
include $(TOPDIR)/rules.mk

LUCI_NAME:=luci-app-ac-agent
LUCI_TITLE:=AC controller agent status
LUCI_DESCRIPTION:=Shows whether this access point is under the control of an AC \
	controller, how it reaches it, and lets the operator set the uplink address \
	and the boot behaviour.
PKG_VERSION:=1.0.0
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_MAINTAINER:=ac-controller

LUCI_DEPENDS:=+luci-base

include $(TOPDIR)/feeds/luci/luci.mk

define Package/$(LUCI_NAME)/install
	# View
	$(INSTALL_DIR) $(1)/www/luci-static/resources/view/ac-agent
	$(INSTALL_DATA) ./htdocs/luci-static/resources/view/ac-agent/overview.js \
		$(1)/www/luci-static/resources/view/ac-agent/

	# Menu and ACL
	$(INSTALL_DIR) $(1)/usr/share/luci/menu.d
	$(INSTALL_DATA) ./root/usr/share/luci/menu.d/luci-app-ac-agent.json $(1)/usr/share/luci/menu.d/
	$(INSTALL_DIR) $(1)/usr/share/rpcd/acl.d
	$(INSTALL_DATA) ./root/usr/share/rpcd/acl.d/luci-app-ac-agent.json $(1)/usr/share/rpcd/acl.d/

	# rpcd backend. Must be executable: rpcd execs it, it does not source it.
	$(INSTALL_DIR) $(1)/usr/libexec/rpcd
	$(INSTALL_BIN) ./root/usr/libexec/rpcd/luci.ac_agent $(1)/usr/libexec/rpcd/
endef

# The backend is registered by rpcd at start time, so reloading it is what makes
# the page work without a reboot. The LuCI cache must go too, or the browser keeps
# serving luci.js?v=<old package-db mtime> and the menu entry never appears.
define Package/$(LUCI_NAME)/postinst
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
chmod 0755 /usr/libexec/rpcd/luci.ac_agent 2>/dev/null
/etc/init.d/rpcd reload 2>/dev/null
rm -f /tmp/luci-indexcache.*
rm -rf /tmp/luci-modulecache/
exit 0
endef

define Package/$(LUCI_NAME)/prerm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] && exit 0
/etc/init.d/rpcd reload 2>/dev/null
exit 0
endef

# call BuildPackage - OpenWrt buildroot signature
#
# luci.mk already evaluates BuildPackage for this app and for the generated
# luci-i18n-ac-agent-<lang> packages; evaluating it again would define the
# package twice and print two menuconfig rows. The literal string above is still
# required: include/scan.mk discovers package Makefiles by grepping for it.
