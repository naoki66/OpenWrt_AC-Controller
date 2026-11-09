# LuCI application: AC agent status.
#
# Packages the web UI and its rpcd backend only. The agent binary comes from the
# sibling ac-agent package (or from `make agent` when installing by hand) and is
# installed at /usr/sbin/ac-agent.
#
# Follows the LuCI feed convention, the same shape as luci-app-onu: luci.mk
# copies htdocs/ and root/ verbatim, generates the luci-i18n-ac-agent-<lang>
# packages from po/, and installs its own postinst (rpcd reload + LuCI cache
# drop), so this Makefile only carries metadata. The mode recorded in git is
# what lands on the target, which is why root/usr/libexec/rpcd/luci.ac_agent is
# 0755: rpcd execs it, it does not source it.
#
# Do not evaluate BuildPackage here: luci.mk already does it for this app and
# for the generated i18n packages, and a second evaluation would print two
# menuconfig rows. The marker comment at the end is still required: include/
# scan.mk discovers package Makefiles by grepping for it.

include $(TOPDIR)/rules.mk

LUCI_TITLE:=AC controller agent status
LUCI_DESCRIPTION:=Shows whether this access point is under the control of an AC controller, how it reaches it, and lets the operator set the uplink address and the boot behaviour.
LUCI_DEPENDS:=+luci-base +rpcd +ac-agent
PKG_VERSION:=1.0.0
PKG_RELEASE:=5
PKG_LICENSE:=MIT

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
