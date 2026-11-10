"""在 fakeroot 下使用 OpenWrt 构建工具的 apk mkpkg 生成 APK v3 安装包。

仅处理本地构建产物，不连接路由器，也不修改现有固件构建树。
"""
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

repo = Path(__file__).resolve().parents[1]
apk = os.environ.get("APK", "apk")
specs = [
    ("ac-agent", "0.3.1-r1", "ac-agent-linux-arm64", "iw"),
    ("ac-controller", "0.2.1-r1", "ac-controller-linux-arm64", ""),
]

def install(source, root, target, mode):
    destination = root / target
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    destination.chmod(mode)

with tempfile.TemporaryDirectory(prefix="ac-mesh-packaging-") as temporary:
    work = Path(temporary)
    for name, version, binary, depends in specs:
        root = work / name
        upstream = repo / "agent/openwrt" / name / "files"
        install(repo / "dist" / binary, root, "usr/sbin/" + name, 0o755)
        if name == "ac-agent":
            install(repo / "dist/ac-aimesh-linux-arm64", root, "usr/sbin/ac-aimesh", 0o755)
            install(repo / "AIMESH_PROTOCOL.md", root, "usr/share/doc/ac-agent/AIMESH_PROTOCOL.md", 0o644)
        install(upstream / (name + ".init"), root, "etc/init.d/" + name, 0o755)
        config_name = "ac" if name == "ac-agent" else name
        install(upstream / (config_name + ".config"), root, "etc/config/" + config_name, 0o600)
        # 开发版全新安装时默认禁用服务，避免未经配置就下发网络变更。
        config = root / "etc/config" / config_name
        config.write_text(config.read_text().replace("option enabled '1'", "option enabled '0'", 1))
        if name == "ac-controller":
            install(upstream / "ac-controller.keep", root, "lib/upgrade/keep.d/ac-controller", 0o644)
        install(repo / "LICENSE", root, "usr/share/licenses/" + name + "/LICENSE", 0o644)
        for path in [root, *root.rglob("*")]:
            os.chown(path, 0, 0)
        pre = work / (name + ".pre")
        pre.write_text("#!/bin/sh\n[ -n \"$IPKG_INSTROOT\" ] && exit 0\n/etc/init.d/" + name + " stop 2>/dev/null || true\n")
        cmd = [apk, "mkpkg", "--info", "name:" + name,
               "--info", "version:" + version, "--info", "arch:aarch64_cortex-a53",
               "--info", "license:MIT", "--info", "description:XR1710G AC Mesh development build",
               "--info", "origin:ac-mesh", "--info", "url:https://github.com/naoki66/OpenWrt_AC-Controller",
               "--script", "pre-upgrade:" + str(pre),
               "--files", str(root), "--output", str(repo / "dist" / (name + "-" + version + ".apk"))]
        if depends:
            cmd += ["--info", "depends:" + depends]
        subprocess.run(cmd, check=True)
