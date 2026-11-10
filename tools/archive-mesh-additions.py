"""归档新增源码，与已跟踪文件的差异补丁配套使用。"""
import sys
import os
import tarfile
from pathlib import Path

with tarfile.open("dist/mesh-new-files.tar.gz", "w:gz") as archive:
    for entry in sys.stdin.buffer.read().split(b"\0"):
        path = Path(os.fsdecode(entry))
        if path.parts and path.parts[0] not in {"dist", ".git"} and path.is_file():
            archive.add(path, arcname=str(path))
