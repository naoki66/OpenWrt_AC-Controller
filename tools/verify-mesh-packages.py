"""在临时目录解包核对载荷，不向路由器安装软件。"""
import os
import stat
import subprocess
import tempfile
from pathlib import Path

repo = Path(__file__).resolve().parents[1]
apk = os.environ.get("APK", "apk")
for name, version in [("ac-agent", "0.3.1-r1"), ("ac-controller", "0.2.1-r1")]:
    with tempfile.TemporaryDirectory(prefix="ac-mesh-verify-") as temporary:
        root = Path(temporary)
        package = repo / "dist" / (name + "-" + version + ".apk")
        subprocess.run([apk, "extract", "--allow-untrusted", "--destination", str(root), str(package)], check=True)
        binary = root / "usr/sbin" / name
        raw = binary.read_bytes()
        assert raw[:4] == b"\x7fELF" and int.from_bytes(raw[18:20], "little") == 183, "not ARM64 ELF"
        assert raw == (repo / "dist" / (name + "-linux-arm64")).read_bytes(), "packaged binary differs"
        if name == "ac-agent":
            probe = root / "usr/sbin/ac-aimesh"
            probe_raw = probe.read_bytes()
            assert probe_raw[:4] == b"\x7fELF" and int.from_bytes(probe_raw[18:20], "little") == 183
            assert probe_raw == (repo / "dist/ac-aimesh-linux-arm64").read_bytes()
            assert stat.S_IMODE(probe.stat().st_mode) == 0o755 and probe.stat().st_uid == 0
            assert (root / "usr/share/doc/ac-agent/AIMESH_PROTOCOL.md").is_file()
        config = root / "etc/config" / ("ac" if name == "ac-agent" else name)
        assert "option enabled '0'" in config.read_text(), "service must start disabled on fresh install"
        assert stat.S_IMODE(config.stat().st_mode) == 0o600
        assert config.stat().st_uid == 0 and config.stat().st_gid == 0
        subprocess.run(["sh", "-n", str(root / "etc/init.d" / name)], check=True)
        print(name + ": APK extraction, ARM64 payload, root ownership, private config, disabled initial service and init syntax passed")
