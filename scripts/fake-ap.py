#!/usr/bin/env python3
"""模拟 AP 的发现广播，用于在没有真机时验证 AC 的发现链路。

AP 侧的真实行为是：启动时、以及每次注册失效时，向 255.255.255.255:29820
广播一条 AP_DISCOVER，AC 收到后回一条 AC_OFFER 告知注册地址。

用法：
    # 单次广播（对 localhost 上的 AC 验证）
    python scripts/fake-ap.py

    # 指定控制器地址与设备身份
    python scripts/fake-ap.py --target 10.0.10.10 --mac 00:11:22:33:44:55 \
        --model XR1710G --hostname ap250 --firmware 24.10.5

    # 每 10 秒广播一次，模拟 AP 持续宣告（Ctrl-C 退出）
    python scripts/fake-ap.py --interval 10

退出码：0 = 收到合法 AC_OFFER；1 = 超时或应答不合法。
"""

from __future__ import annotations

import argparse
import json
import socket
import sys
import time

MSG_DISCOVER = "AP_DISCOVER"
MSG_OFFER = "AC_OFFER"
PROTOCOL_VERSION = 1


def build_discover(args: argparse.Namespace) -> bytes:
    return json.dumps(
        {
            "type": MSG_DISCOVER,
            "v": PROTOCOL_VERSION,
            "mac": args.mac,
            "model": args.model,
            "firmware": args.firmware,
            "hostname": args.hostname,
        },
        separators=(",", ":"),
    ).encode()


def probe_once(args: argparse.Namespace) -> bool:
    discover = build_discover(args)

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if args.broadcast:
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    # 绑定任意端口，这样 AC 的应答会回到本 socket。
    sock.bind(("0.0.0.0", 0))
    sock.settimeout(args.timeout)

    try:
        sock.sendto(discover, (args.target, args.port))
        print(f"-> {MSG_DISCOVER} {args.mac} -> {args.target}:{args.port}")
        deadline = time.time() + args.timeout
        while time.time() < deadline:
            try:
                payload, peer = sock.recvfrom(2048)
            except socket.timeout:
                break
            except ConnectionResetError:
                # Windows reports the ICMP port-unreachable from a previous
                # send as a reset on the socket; there is simply no listener.
                break
            try:
                offer = json.loads(payload)
            except json.JSONDecodeError:
                continue
            if offer.get("type") != MSG_OFFER:
                continue

            required = ("ac_id", "ac_ip", "http_port")
            missing = [k for k in required if not offer.get(k)]
            if missing:
                print(f"<! {MSG_OFFER} 缺少字段: {', '.join(missing)}", file=sys.stderr)
                return False

            print(f"<- {MSG_OFFER} from {peer[0]}")
            print(f"   ac_id           = {offer['ac_id']}")
            print(f"   ac_ip           = {offer['ac_ip']}")
            print(f"   http_port       = {offer['http_port']}")
            print(f"   secret_required = {offer.get('secret_required')}")
            print(f"   protocol        = v{offer.get('v')}")
            return True
        print("<! 超时：未收到 AC_OFFER", file=sys.stderr)
        return False
    finally:
        sock.close()


def main() -> int:
    p = argparse.ArgumentParser(description="模拟 AP 发现广播")
    p.add_argument("--target", default="127.0.0.1", help="AC 地址，或 255.255.255.255")
    p.add_argument("--port", type=int, default=29820, help="发现端口，默认 29820")
    p.add_argument("--mac", default="02:00:00:00:00:01", help="AP 的 MAC")
    p.add_argument("--model", default="XR1710G", help="型号")
    p.add_argument("--firmware", default="24.10.5", help="固件版本")
    p.add_argument("--hostname", default="ap250", help="主机名")
    p.add_argument("--timeout", type=float, default=2.0, help="等待应答的秒数")
    p.add_argument("--interval", type=float, default=0, help=">0 时循环广播的间隔秒数")
    p.add_argument("--broadcast", action="store_true", help="启用 SO_BROADCAST（target 为广播地址时必须）")
    args = p.parse_args()

    if args.broadcast:
        args.timeout = max(args.timeout, 3.0)

    ok = probe_once(args)
    if args.interval <= 0:
        return 0 if ok else 1

    print(f"每 {args.interval}s 广播一次，Ctrl-C 退出")
    try:
        while True:
            time.sleep(args.interval)
            probe_once(args)
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    sys.exit(main())
