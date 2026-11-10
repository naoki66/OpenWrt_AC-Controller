"""将安装包、说明、来源记录与源码增量整理为可审阅的 ZIP。"""
import hashlib
import os
import subprocess
import zipfile
from pathlib import Path

repo = Path(__file__).resolve().parents[1]
dist = repo / "dist"
go = os.environ.get("GO", "go")
source_base = os.environ.get("SOURCE_BASE", "abf5013fe91a8cfdce446b0e1175aa003bdb1045")
info = "\n".join([
    "XR1710G AC Mesh development build 0.3.1-aimesh-probe.1",
    "Upstream: https://github.com/naoki66/OpenWrt_AC-Controller",
    "Base: " + subprocess.check_output(["git", "rev-parse", source_base], cwd=repo, text=True).strip(),
    "Source HEAD: " + subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo, text=True).strip(),
    "Source: local changes; mesh-source.patch + mesh-new-files.tar.gz",
    subprocess.check_output([go, "version"], text=True).strip(),
    "Target: linux/arm64; CGO_ENABLED=0; APK aarch64_cortex-a53",
    "Validation: Go tests, go vet, Mesh UI interaction checks, APK payload extraction",
    "Hardware: NOT installed or radio-tested; two-node validation remains required",
    "AiMesh: passive LLDP/cfg-UDP diagnostics only; authentication, stock ASUS joining and wireless backhaul NOT implemented",
    "",
])
(dist / "build-info.txt").write_text(info)
packages = [dist / "ac-agent-0.3.1-r1.apk", dist / "ac-controller-0.2.1-r1.apk"]
files = sorted([*packages, dist / "mesh-source.patch", dist / "mesh-new-files.tar.gz", dist / "build-info.txt", repo / "MESH_安装说明.md", repo / "AIMESH_PROTOCOL.md", repo / "CHANGELOG_MESH.md"])
(dist / "SHA256SUMS").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in files))
with zipfile.ZipFile(dist / "XR1710G_AC_Mesh_0.3.1-aimesh-probe.zip", "w", zipfile.ZIP_DEFLATED) as archive:
    for path in [*files, dist / "SHA256SUMS", repo / "LICENSE"]:
        archive.write(path, path.name)
print("Release ZIP: " + str(dist / "XR1710G_AC_Mesh_0.3.1-aimesh-probe.zip"))
