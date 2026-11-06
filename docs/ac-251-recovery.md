# 10.0.30.251 恢复手册（管理面被防火墙锁死）

## 0. 为什么必须带外

`.251` 入站被 netfilter REJECT，远程没有任何通道能拿到 shell。已验证过的路径，全部失败：

| 路径 | 结果 |
|---|---|
| SSH 22 / HTTP 80 / 443 / 7681 | 全部不通 |
| 六个子网逐个测（`10.0.10.251` / `.20.` / `.30.`） | 全部收到 ICMP port-unreachable = REJECT |
| 40 / 50 / 60 网段 | 同上，逐一实测 |
| IPv6 link-local `fe80::200:ff:fe00:21` | 同样被 REJECT |
| 单播 `arping` | 2/2 通（ARP 绕过 netfilter） |
| 控制器心跳 | 正常（OUTPUT 仍是 ACCEPT） |

`.251` 在**每个** VLAN 上都自己发出 ICMP unreachable，抓包实证：

```
IP 10.0.10.251 > 10.0.10.250: ICMP 10.0.10.251 protocol 1 port 63456 unreachable
IP 10.0.20.251 > 10.0.20.250: ICMP 10.0.20.251 protocol 1 port 6799   unreachable
IP 10.0.30.251 > 10.0.30.250: ICMP 10.0.30.251 protocol 1 port 12618  unreachable
```

**别用 SSH 的报错文案判断通不通**：客户端会把 ICMP port-unreachable 映射成
`Connection refused`，把静默丢包映射成 `Connection timed out`。实测中
`10.0.10.251` 报 refused、另两个报 timeout，看起来像 VLAN 1 通了，其实三者
都是 REJECT，只是中间设备对 unreachable 的处理不同。判定一律以抓包为准。

**设备本身是健康的**，只是管理面被自己的配置锁死。根因与修复见 `git show 05dc1e1`。

---

## 1. 恢复出厂并拿到 shell

1. 断电。
2. 按住 reset 键，上电，保持按住。
3. 松开后观察：
   - **进入 failsafe**：LED 快速闪烁，设备地址 `192.168.1.1`，dropbear 免密，防火墙未启动 —— **配置还在**，这时只需第 2 步修 network，不用全量恢复。
   - **进入出厂态**：LED 常亮/正常启动，同样 `192.168.1.1`，但配置已清空。
4. 电脑直连任意 LAN 口，网卡配 `192.168.1.2/24`，然后：

```sh
ssh root@192.168.1.1
```

5. 判断落在哪种状态：

```sh
uci show network | grep "interface" | head
```

有 `lan2`…`lan6` 和 `bridge-vlan` → failsafe，配置还在，跳到第 3 步核对即可。
只有 `lan` → 出厂态，继续第 2 步。

---

## 2. 恢复 network（出厂态才需要）

基线就是事故前的真实配置，在仓库里：

```sh
# 在我的机器上执行（D:\GitData\ac-controller）
scp docs/baseline/ap251/network.uci root@192.168.1.1:/etc/config/network
```

设备上：

```sh
uci show network | grep -c "^network\.[a-z0-9_]*=interface"   # 应为 7（loopback + lan..lan6）
reboot
```

重启后应恢复六个管理 IP：`10.0.10.251 / 10.0.20.251 / 10.0.30.251 / 10.0.40.251 / 10.0.50.251 / 10.0.60.251`。
之后所有操作改走 `root@10.0.30.251`。

---

## 3. 核对防火墙 zone（这一步是事故根因，必须亲眼确认）

```sh
uci show firewall.@zone[0].network
```

必须输出 `lan lan2 lan3 lan4 lan5 lan6`。若只有 `lan`（出厂态常见），补齐：

```sh
for i in lan2 lan3 lan4 lan5 lan6; do uci add_list firewall.@zone[0].network=$i; done
uci commit firewall
```

对照样本见 `docs/baseline/ap250/firewall.uci`（同型号健康设备，2026-10-31 取样）。

---

## 4. 安装新 agent

**顺序约束：必须先装新 agent，再 repush。** 旧 agent 会把新包里不带前缀的接口段丢掉，同时把本地 `lan*` 全 disable —— 结果比现在更糟（接口全部消失）。

```sh
scp bin/ac-agent-linux-arm64          root@10.0.30.251:/usr/sbin/ac-agent
scp deploy/openwrt/ac-agent/files/ac-agent.init root@10.0.30.251:/etc/init.d/ac-agent
scp deploy/openwrt/ac-agent/files/ac.config     root@10.0.30.251:/etc/config/ac
```

设备上（secret 取自 NAS `/srv/ac-controller/.env` 的 `AC_SHARED_SECRET`）：

```sh
chmod 0755 /usr/sbin/ac-agent /etc/init.d/ac-agent
uci set ac.agent.ac_addr='10.0.30.5:8090'
uci set ac.agent.secret='<AC_SHARED_SECRET>'
uci commit ac
/etc/init.d/ac-agent enable
/etc/init.d/ac-agent start
```

### 4a. 先空跑一遍看暂存结果

演练模式只保留为命令行开关，不再写入 UCI；常驻 agent 启动前先手工跑一轮：

```sh
ac-agent once --dry-run -v
```

检查暂存文件里：

- `config interface 'lan'` / `'lan2'` … `'lan6'` —— **不能**出现 `config interface 'ac_'`
- `config device 'ac_bridge'`、`config bridge-vlan 'ac_bvlan*'` —— 这两个带前缀是对的

确认无误后直接重启常驻 agent（它不带 `--dry-run`，会正常落地）：

```sh
/etc/init.d/ac-agent restart
```

---

## 5. repush 并验收

控制器已经用修复后的代码重建（`/api/v1/health` 里 `commit` 应为 `05dc1e1`）。

```sh
curl -X POST http://10.0.30.5:8090/api/v1/devices/00:00:00:00:00:21/repush
```

验收清单（**每 apply 一项都要当场确认，别只看服务 running**）：

```sh
# 1. 管理面还在（这条最重要）
ssh root@10.0.30.251 hostname

# 2. 六个 IP 都在
ip -o -4 addr | grep -E "10\.10\.(10|20|30|40|50|60)\.251"

# 3. 接口名没有被改名 —— 绝不能有 ac_ 接口
uci show network | grep "=interface"
uci show network | grep "ac_"        # 只应出现 ac_bridge / ac_bvlan*

# 4. 防火墙 zone 仍然覆盖全部
uci show firewall.@zone[0].network

# 5. 控制器侧
curl -s http://10.0.30.5:8090/api/v1/devices/00:00:00:00:00:21 | \
  grep -o '"applied_rev":[0-9]*\|"sync_state":"[a-z]*"'
```

---

## 6. 若再次失联

现象会和第 0 节完全一样。回滚手段：

- 控制器二进制已备份在 NAS `/srv/ac-controller/ac.bak-prefixfix`，换回后 `docker compose up -d` 即可。
- 设备侧重复第 1 步。

`.250` 目前只接管无线（network 不在它的包里），**不受本次事故影响，不要动它**。等 `.251` 验证通过再考虑。

---

## 实际执行结果（2026-10-31 13:29，已闭环）

`10.0.30.251` 已恢复并由 AC 接管 network + wireless：

- 控制器侧 `AP-A rev=4/4 sync=applied`，心跳 3 秒
- 六个管理 IP 齐全；network 无 `ac_` 接口段；firewall zone 覆盖 lan..lan6
- 三个射频 `ac_radio0/1/2` 全部 up，11 个 BSS（2.4G 1 / 5G 4 / 6G 6）
- 11k 邻居列表 **11/11** 指向 `.250` 的同名 SSID
- 客户端已回流（29 个，其中 `.251` 上 3 个）

### 坑一：dropbear 被绑死接口，网通但 SSH 不通

恢复 network 后 `192.168.1.1` 能 ARP、80 端口通，但 22 refused。原因不是防火墙，
是出厂 `/etc/config/dropbear` 有 `option DirectInterface 'lan'`，而恢复后 `lan` 是
`br-lan.1`（挂在没插线的 wan 口），dropbear 只在 `10.0.10.251` 上监听。

**判据**：先测 80。80 通 = 防火墙没问题，是服务没监听；80 不通才是防火墙。

**不用串口也能救**（80 通即可）：
1. `POST /cgi-bin/luci/` 带 `luci_username=root&luci_password=`（空密码）→ 302 + Set-Cookie
2. `POST /ubus` JSON-RPC（GET 是 404）：用全 0 sessionid 调 `session.login` 换 `ubus_rpc_session`
3. rpcd 的 `file.read` / `file.exec` 对 root 会话也返回 `[6]`（PERMISSION_DENIED），
   但 **`uci` 对象可用**：`uci set` → `commit` → **`uci apply`**（会触发 reload_config），
   dropbear 就地重启，SSH 恢复，不必重启设备。

### 坑二：健康检查读 radio 层 `up`，ACS 未完成时误判就绪

`13:23:58` agent 设置 11k 列表只覆盖了 6 GHz 的 6 个 BSS；2.4G 的 `phy0.0-ap0`
到 `13:24:07` 才 AP-ENABLED（晚 9 秒，因为走 ACS 自动信道扫描）。radio 层的 `up`
在 ACS 完成前就为真，健康检查因此放行，写进未 ENABLE 的 BSS 的列表随后被清空。

设计里的兜底（每 15 轮比对补回）在 `13:28:40` 补齐，自愈，代价是最多约 5 分钟
11k 不完整。**已知未修**：要让 11k 在 apply 后立即可用，应等 BSS 进入 ENABLED 而不
是只看 radio 的 `up`，或在 apply 后开一个短周期补写窗口。
