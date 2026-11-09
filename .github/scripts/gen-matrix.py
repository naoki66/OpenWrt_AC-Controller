#!/usr/bin/env python3
"""Generate the OpenWrt SDK build matrix consumed by openwrt-packages.yml.

Reads TARGETS (comma-separated "<arch>-<release>" SDK tags) and prints a
GitHub Actions matrix JSON to stdout. OpenWrt 24.10 and older produce .ipk;
25.12, SNAPSHOT and main produce .apk.
"""

import json
import os
import sys

targets = [t.strip() for t in os.environ.get("TARGETS", "").split(",") if t.strip()]
if not targets:
    print("::error::No SDK targets configured", file=sys.stderr)
    sys.exit(1)

items = []
for tag in targets:
    release = tag.rsplit("-", 1)[-1]
    head = release.split(".")[0]
    # Non-numeric heads (SNAPSHOT / main) are rolling builds and use apk.
    major = int(head) if head.isdigit() else 99
    items.append({"sdk": tag, "format": "apk" if major >= 25 else "ipk"})

print(json.dumps({"sdk": [i["sdk"] for i in items], "include": items},
                 separators=(",", ":")))
