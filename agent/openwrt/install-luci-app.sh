#!/bin/sh
# Install the LuCI application on a running AP, without a buildroot.
#
#   ./install-luci-app.sh <AP_IP>
#
# Installs: the view, the menu entry, the ACL, the rpcd backend and the compiled
# zh-cn catalog. The agent binary itself is not copied — build it with
# `make agent` and put it at /usr/sbin/ac-agent.
#
# The catalog is compiled here with po2lmo.py, not on the device: many
# trimmed firmware builds ship without po2lmo, and a byte-compatible build on the
# host is both faster and verifiable (`po2lmo.py -d`).
set -eu

AP="${1:?usage: install-luci-app.sh <ap-address>}"
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
APP="$HERE/luci-app-ac-agent"
PO2LMO="$HERE/po2lmo.py"

if [ ! -d "$APP" ]; then
	echo "missing $APP" >&2
	exit 1
fi

PY=$(command -v python3 || command -v python || true)
if [ -z "$PY" ]; then
	echo "python3 is required to compile the catalog" >&2
	exit 1
fi

# OpenSSH 9 speaks SFTP first, which dropbear does not provide; older releases
# still default to the legacy protocol and reject -O. Pick per local version.
SCP_OPT=""
case "$(ssh -V 2>&1)" in
	*OpenSSH_[1-8]\.*) SCP_OPT="" ;;
	*) SCP_OPT="-O" ;;
esac
SCP="scp $SCP_OPT"

if ! ssh "root@$AP" 'test -x /usr/sbin/ac-agent'; then
	echo "warning: /usr/sbin/ac-agent is missing on $AP; the page will report the agent as stopped" >&2
fi

echo "==> copying view, menu, ACL and rpcd backend to $AP"
$SCP -r "$APP/htdocs/luci-static/resources/view/ac-agent" \
	"root@$AP:/www/luci-static/resources/view/"
$SCP "$APP/root/usr/share/luci/menu.d/luci-app-ac-agent.json" \
	"root@$AP:/usr/share/luci/menu.d/"
$SCP "$APP/root/usr/share/rpcd/acl.d/luci-app-ac-agent.json" \
	"root@$AP:/usr/share/rpcd/acl.d/"
$SCP "$APP/root/usr/libexec/rpcd/luci.ac_agent" "root@$AP:/usr/libexec/rpcd/"
ssh "root@$AP" 'chmod 0755 /usr/libexec/rpcd/luci.ac_agent'

echo "==> compiling the zh-cn catalog"
# Deliberately no cleanup trap: mktemp -d lands in the OS temp directory and a
# `rm -rf` on it gets rejected by path-guard shims on Windows, which turns a
# successful deploy into a scary (and harmless) error line.
TMP=$(mktemp -d)

# Catalog name must match the view directory (view/ac-agent), not the package
# name: LuCI looks up ac-agent.<lang>.lmo. luci.mk derives the same name from
# po/zh_Hans/ac-agent.po when the app is built from the feed.
"$PY" "$PO2LMO" "$APP/po/zh_Hans/ac-agent.po" "$TMP/ac-agent.zh-cn.lmo"
ssh "root@$AP" 'mkdir -p /usr/lib/lua/luci/i18n'
$SCP "$TMP/ac-agent.zh-cn.lmo" "root@$AP:/usr/lib/lua/luci/i18n/"

echo "==> refreshing caches and rpcd"
ssh "root@$AP" 'sh -s' <<'REMOTE'
set -eu
# LuCI stamps luci.js with the package database mtime, so copying files alone
# leaves the browser on a cached view. Newer images use apk, older ones opkg.
touch /lib/apk/db/installed 2>/dev/null || touch /usr/lib/opkg/status 2>/dev/null || true
rm -f /tmp/luci-indexcache.* 2>/dev/null || true
rm -rf /tmp/luci-modulecache/ 2>/dev/null || true
/etc/init.d/rpcd restart 2>/dev/null || /etc/init.d/rpcd reload 2>/dev/null || true
/etc/init.d/uhttpd reload 2>/dev/null || true
REMOTE

echo "==> done. Open LuCI -> Control -> AC Agent"
echo "    (/cgi-bin/luci/admin/control/ac-agent; hard-refresh once: Ctrl+Shift+R)"
