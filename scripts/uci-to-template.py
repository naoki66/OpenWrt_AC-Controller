#!/usr/bin/env python3
"""把现网 `uci export wireless/network` 的导出转换成 AC 模板 JSON。

用途：给一台已在上线的 AP 生成"黄金模板"，作为 P1 验收的比对基准
（渲染结果必须与现网配置逐项一致）。

    python scripts/uci-to-template.py \
        --wireless docs/baseline/ap251/wireless.uci \
        --network  docs/baseline/ap251/network.uci \
        --id ap-a --name "AP-A 现网基线" \
        --out docs/baseline/ap251/template.json

注意：输出会保留 PSK。若要把结果提交进仓库，先用 --redact-psk 脱敏。
"""

from __future__ import annotations

import argparse
import json
import re
import sys

SEC_FROM_ENCRYPTION = {
    "psk2": "wpa2",
    "psk2+ccmp": "wpa2",
    "psk2+tkip+ccmp": "wpa2",
    "sae": "wpa3",
    "sae+ccmp": "wpa3",
    "sae-mixed": "wpa2/wpa3",
    "owe": "owe",
    "none": "open",
}

BOOL_KEYS = {"1": True, "0": False}


def parse_uci(text: str) -> list[dict]:
    """按 config 段切分，返回 [{type, name, options:{k:v}, lists:{k:[v]}}]。"""
    sections: list[dict] = []
    header = re.compile(r"^config\s+(\S+)(?:\s+'([^']*)')?\s*$")
    opt = re.compile(r"^\s*option\s+(\S+)\s+'(.*)'\s*$")
    lst = re.compile(r"^\s*list\s+(\S+)\s+'(.*)'\s*$")

    for raw in text.splitlines():
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        m = header.match(raw)
        if m:
            sections.append({"type": m.group(1), "name": m.group(2), "options": {}, "lists": {}})
            continue
        if not sections:
            continue
        m = opt.match(raw)
        if m:
            sections[-1]["options"][m.group(1)] = m.group(2)
            continue
        m = lst.match(raw)
        if m:
            sections[-1]["lists"].setdefault(m.group(1), []).append(m.group(2))
    return sections


def as_bool(value, default=None):
    if value is None:
        return default
    return BOOL_KEYS.get(value, default)


def build_template(wireless: list[dict], network: list[dict], tpl_id: str, name: str, redact: str | None):
    radios = []
    band_of_radio = {}
    for s in wireless:
        if s["type"] != "wifi-device":
            continue
        o = s["options"]
        idx = int(o.get("radio", 0))
        band_of_radio[s["name"]] = (band_of_radio.get(s["name"]) or o.get("band"))
        radio = {
            "index": idx,
            "path": o.get("path", ""),
            "band": o.get("band", ""),
            "channel": o.get("channel", "auto"),
            "htmode": o.get("htmode", ""),
            "txpower": int(o.get("txpower", 0)),
        }
        for src, dst in (
            ("country", "country"),
            ("cell_density", "cell_density"),
            ("he_bss_color", "he_bss_color"),
        ):
            if src in o:
                radio[dst] = o[src] if dst == "country" else int(o[src])
        for key in ("he_su_beamformer", "he_mu_beamformer", "eht_su_beamformer",
                    "eht_mu_beamformer", "radar_background", "he_twt_responder"):
            if key in o:
                radio[key] = as_bool(o[key])
        if as_bool(o.get("disabled"), False):
            radio["disabled"] = True
        radios.append(radio)

    ssids = []
    for s in wireless:
        if s["type"] != "wifi-iface":
            continue
        o = s["options"]
        dev = o.get("device", "")
        band = band_of_radio.get(dev, "")
        enc = o.get("encryption", "none")
        if enc not in SEC_FROM_ENCRYPTION:
            raise SystemExit(f"未知加密方式 {enc!r}（段 {s['name']}），请先在映射表里登记")
        entry = {
            "ssid": o.get("ssid", ""),
            "bands": [band] if band else [],
            "network": o.get("network", ""),
            "security": SEC_FROM_ENCRYPTION[enc],
        }
        if o.get("key"):
            entry["psk"] = redact if redact else o["key"]
        if as_bool(o.get("hidden"), False):
            entry["hidden"] = True
        if as_bool(o.get("isolate"), False):
            entry["isolate"] = True
        if as_bool(o.get("disabled"), False):
            entry["disabled"] = True
        roaming = {
            "k": as_bool(o.get("ieee80211k"), False),
            "v": as_bool(o.get("bss_transition"), False),
            "r": as_bool(o.get("ieee80211r"), False),
        }
        if o.get("mobility_domain"):
            roaming["mobility_domain"] = o["mobility_domain"]
        if "ft_over_ds" in o:
            roaming["ft_over_ds"] = as_bool(o["ft_over_ds"])
        entry["roaming"] = roaming
        for key in ("wnm_sleep_mode", "wnm_sleep_mode_no_keys", "proxy_arp"):
            if as_bool(o.get(key), False):
                entry[key] = True
        if "ieee80211v" in o:
            entry["ieee80211v"] = as_bool(o["ieee80211v"])
        if "ft_psk_generate_local" in o:
            entry["ft_psk_generate_local"] = as_bool(o["ft_psk_generate_local"])
        if "disassoc_low_ack" in o:
            entry["disassoc_low_ack"] = as_bool(o["disassoc_low_ack"])
        ssids.append(entry)

    bridge = {"name": "br-lan", "vlans": [], "interfaces": []}
    for s in network:
        if s["type"] == "device" and s["options"].get("type") == "bridge":
            bridge["name"] = s["options"].get("name", "br-lan")
            bridge["ports"] = s["lists"].get("ports", [])
            if "igmp_snooping" in s["options"]:
                bridge["igmp_snooping"] = as_bool(s["options"]["igmp_snooping"])
        elif s["type"] == "bridge-vlan":
            bridge["vlans"].append({
                "vlan": int(s["options"].get("vlan", 0)),
                "ports": s["lists"].get("ports", []),
            })
    for s in network:
        if s["type"] != "interface" or s["name"] in (None, "loopback"):
            continue
        o = s["options"]
        iface = {"name": s["name"], "device": o.get("device", ""), "proto": o.get("proto", "none")}
        for src, dst in (
            ("netmask", "netmask"), ("gateway", "gateway"), ("dns", "dns"),
            ("broadcast", "broadcast"), ("ip6assign", "ip6assign"),
            ("ip6ifaceid", "ip6ifaceid"),
        ):
            if src in o:
                iface[dst] = o[src]
        ips = s["lists"].get("ipaddr", [])
        if not ips and "ipaddr" in o:
            ips = [o["ipaddr"]]
        if ips:
            iface["ipaddr"] = ips
        bridge["interfaces"].append(iface)

    bridge["vlans"].sort(key=lambda v: v["vlan"])
    return {"id": tpl_id, "name": name, "bridge": bridge, "radios": radios, "ssids": ssids}


def main() -> int:
    p = argparse.ArgumentParser(description="uci 导出 → AC 模板 JSON")
    p.add_argument("--wireless", required=True)
    p.add_argument("--network", required=True)
    p.add_argument("--id", default="site")
    p.add_argument("--name", default="site")
    p.add_argument("--redact-psk", default=None, help="用该占位串替换所有 PSK")
    p.add_argument("--out", default="-")
    args = p.parse_args()

    wireless = parse_uci(open(args.wireless, encoding="utf-8").read())
    network = parse_uci(open(args.network, encoding="utf-8").read())
    tpl = build_template(wireless, network, args.id, args.name, args.redact_psk)
    body = json.dumps(tpl, ensure_ascii=False, indent=2) + "\n"
    if args.out == "-":
        sys.stdout.write(body)
    else:
        open(args.out, "w", encoding="utf-8", newline="\n").write(body)
        print(f"wrote {args.out}: {len(tpl['ssids'])} ssids, {len(tpl['radios'])} radios, "
              f"{len(tpl['bridge']['vlans'])} vlans", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
