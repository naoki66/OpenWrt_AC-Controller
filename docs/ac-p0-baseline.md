# P0 基线：现网 XR1710G 实测

> 采集时间：2026-10-31 01:00–01:05（设备本地时间）
> 采集方式：SSH 只读命令（`uci export`、`ubus call ... status`、`ip`、`bridge`、`cat /sys/kernel/debug/*`）。
> **未做任何配置写入**：没有 `uci set/commit`、没有 `reload`/`restart`、没有重启。
> 原始文件见本目录。
>
> **已脱敏**：dump 原样包含现网 Wi-Fi 密码，提交前已将所有 `option key` 替换为 `REDACTED-PSK`，
> `wireless-status.json` 中同名字段一并处理。

---

## 1. 设备

| 项 | 值 |
|---|---|
| 型号 | Gemtek XR1710G |
| 固件 | ImmortalWrt naoki66 XR1710G 20261026-7c7c84f7（`airoha/an7581`） |
| 内核 | 6.18.55 aarch64 |
| 主机名 | `AP-A` |
| uptime | 6 h 16 m（采集时） |
| br-lan MAC | `00:00:00:00:00:21` |
| 管理地址 | `10.0.30.251/24`（另有 .10/.20/.40/.50/.60 各一个 /24） |

注意：设计文档中记录的 `10.0.10.251` 与本次采集的 `10.0.30.251` 是**同一台设备的不同 VLAN 地址**，
`br-lan.1`–`br-lan.6` 六个 VLAN 各配了一个静态地址。

## 2. 结论先行：配置模型与设计文档不符

设计文档 §5 D2 选的是 **方案 A**（单 `br-lan` + `br-lan.<vid>` 软件 802.1q 子接口），
并明确否决了 **方案 C**（`vlan_filtering=1` + 每口 PVID）。

**现网实际是方案 C。**

```
# ip -d link show br-lan
bridge ... vlan_filtering 1 vlan_protocol 802.1Q ... vlan_default_pvid 1

# bridge vlan show
wan               1 PVID Egress Untagged
                  2 3 4 5 6 3169
lan2              2 PVID Egress Untagged
lan3              3 PVID Egress Untagged
lan4              3169 PVID Egress Untagged
br-lan            1 2 3 4 5 6 3169
phy0.2-ap0        1 PVID Egress Untagged
phy0.2-ap1        2 PVID Egress Untagged
```

配置侧对应 7 条 `config bridge-vlan`（VLAN 1–6 + 3169），netifd interface 直接写
`option device 'br-lan.5'`。整个 `/etc/config/network` 里**没有任何**
`config device` + `option type '8021q'` 段。

这意味着：目前 `internal/render.Network()` 生成的软件 802.1q 子接口模型**不能用于这台设备**——
在已经是 `vlan_filtering=1` 的桥上再叠一层软件 VLAN，标签会被桥先行剥掉，结果是链路不通或双重打标。
P1 的渲染器必须改成输出 `bridge-vlan` + 端口 PVID。

### 端口与 VLAN 映射（实测）

| 端口 | PVID | 承载 VLAN | 说明 |
|---|---|---|---|
| wan | 1 | 1,2,3,4,5,6,3169 | trunk 上行（只有 1 是 untagged） |
| lan2 | 2 | 2 | USXGMII |
| lan3 | 3 | 3 | MT7530 |
| lan4 | 3169 | 3169 | MT7530，独立的 3169 |
| wlan | 每 BSS 一个 PVID | 随 SSID 的 VLAN | 由 netifd 自动挂 |

## 3. 无线实录

### 3.1 radio

三个 radio **共用同一个 PCIe path**，靠 `option radio '0'/'1'/'2'` 区分：

| radio | band | channel | htmode | country | txpower | path 后缀 |
|---|---|---|---|---|---|---|
| radio0 | 2g | auto | **HE20** | US | 28 | `…/0000:01:00.0` |
| radio1 | 5g | 36 | **EHT160** | US | 30 | 同上 |
| radio2 | 6g | 37 | **EHT320** | US | 30 | 同上 |

三者都带 `cell_density '0'`、beamformer 开关（`he_su/he_mu`、`eht_su/eht_mu`）、
`he_bss_color`（radio1=2、radio2=1），radio1 另有 `radar_background '1'`，radio2 有 `he_twt_responder '1'`。

**`htmode` 是 EHT（Wi-Fi 7），不是 HE。** 当前渲染器只会输出 `HT*`/`HE*`，
6 GHz 的 EHT320 无法表达。

### 3.2 BSS 矩阵（18 个 section，11 个实际启用）

| band | SSID | network | 加密 | 11r | mobility_domain | ft_over_ds | 启用 |
|---|---|---|---|---|---|---|---|
| 2g | SSID-A | lan | psk2+ccmp | – | – | – | ✅ |
| 2g | SSID-A2–SSID-A6 | lan2–lan6 | psk2+ccmp | – | – | – | ❌ disabled |
| 5g | SSID-A_5G | lan | psk2+ccmp | 1 | 1111 | 0 | ✅ |
| 5g | SSID-B_5G | lan2 | sae-mixed | 1 | 2222 | 0 | ✅ |
| 5g | SSID-C_5G | lan3 | sae-mixed | 1 | 3333 | 0 | ✅ |
| 5g | SSID-D_5G | lan4 | sae-mixed | 1 | 4444 | 0 | ✅ |
| 5g | SSID-E_5G / SSID-F_5G | lan5 / lan6 | sae-mixed | 1 | 5555 / 6666 | 0 | ❌ disabled |
| 6g | SSID-A_6G | lan | sae+ccmp | 1 | 1111 | 0 | ✅ |
| 6g | SSID-B_6G | lan2 | sae+ccmp | 1 | 2222 | 0 | ✅ |
| 6g | SSID-C_6G | lan3 | sae+ccmp | 1 | 3333 | 0 | ✅ |
| 6g | SSID-D_6G | lan4 | sae+ccmp | 1 | 4444 | 0 | ✅ |
| 6g | SSID-E_6G | lan5 | sae+ccmp | 1 | 5555 | 0 | ✅ |
| 6g | SSID-F_6G | lan6 | sae+ccmp | 1 | 6666 | 0 | ✅ |

运行时 `ubus call network.wireless status` 报告 radio0/1/2 均 `up: true`，
接口数 1 + 4 + 6 = **11**，与上表启用数一致。

### 3.3 与设计文档的差异

| 设计文档 §2.3 的说法 | 实测 |
|---|---|
| "已载入 6 组 SSID × 三频" | 2.4G 只启用 1 个（SSID-A），5G 启用 4 个，6G 启用 6 个；共 18 个 section / 11 个启用 |
| `SSID → netifd interface（lan/lan2/…）→ br-lan.<vid> 子接口` | interface 名是 `lan`…`lan6`，但设备是 `br-lan.1`…`br-lan.6`，由 **bridge-vlan** 生成，不是软件 8021q |
| 加密 `psk2` | 实际 `psk2+ccmp` / `sae-mixed` / `sae+ccmp` 三种 |
| 11r 参数未记录 | 每个 VLAN 一个 `mobility_domain`（1111…6666），跨 5G/6G 同 VLAN 一致；`ft_over_ds '0'` |
| 2.4G 也开 k/v/r | 2.4G 只有 k/v（`ieee80211k`、`bss_transition`、`ieee80211v`、`wnm_sleep_mode`），**没有 11r** |

## 4. DHCP / DNS

```
dnsmasq: disabled   （进程数 0）
odhcpd:  disabled
```

与设计文档一致。这正是 agent 必须强制维持的状态（见 `docs/ac-agent.md` §4）。

## 5. 卸载（PPE）现状

```
uci get firewall.@defaults[0].flow_offloading     = 1
uci get firewall.@defaults[0].flow_offloading_hw  = 1

nft flowtable ft { hook ingress; devices = { lan2, lan3, lan4,
   phy0.0-ap0, phy0.1-ap0..ap3, phy0.2-ap0..ap5, wan }; flags offload }

/sys/kernel/debug/ppe/summary
type      total bound packets bytes
IPv4 5T      46     1     0     0
IPv4 3T       7     0     0     0
L2B          13     0     0     0
IPv6 3T       2     0     0     0

/proc/net/nf_conntrack: 169 entries, 10 marked OFFLOAD
```

现网 2.4G 的 SSID-A 挂着 **19 个客户端**，wan 累计 RX 4.31 GB / 3.73 M 包。

### 关于 R1 的判定——本次**没有**得出结论，原因如下

设计文档 §7.3 用「CPU 端口（`eth0`/GDM1）RX 包数增速 < 总包数 5%」作为判据。实测：

```
cpu   RX: 0 bytes / 0 packets        ← GDM1，MT7530 的 conduit
wan   RX: 4.31 GB / 3.73 M packets
```

`cpu` 口为 0 **不能**证明卸载生效：在这台机器上 GDM1 是内部 MT7530 交换机的 conduit，
而 lan3/lan4 当前都没有载波（`NO-CARRIER`），所以它本来就该是 0。
wan 的 punt 路径不走 GDM1，§7.3 的判据取错了对象。

能确认的是：`flow_offloading_hw=1`、flowtable 已挂载含 wlan 与 wan 成员、
conntrack 有 10 条 `OFFLOAD` 标记 —— 卸载**处于启用且有活动的状态**。

要给出 R1 的定量结论，需要一次受控打流窗口（客户端 iperf3 + 同时采样
`/sys/kernel/debug/ppe/bind` 与 wan/cpu 计数），并且判据应改为
**PPE bound 条目随活跃流增长 + 打流时 CPU 侧 `ksoftirqd` 占比不上升**。
这需要中断业务或安排窗口，本次未做。

## 6. 对后续阶段的直接影响

1. **P1 渲染器必须重写 `network` 部分**：输出 `config bridge-vlan` + 端口 PVID，
   不再生成 `config device type '8021q'`。
2. **VLAN ↔ interface 命名**：现网用 `lan`/`lan2`…`lan6`，不是 `ac_vlan<vid>`。
   AC 侧要么沿用现网命名（推荐，破坏性最小），要么引入显式映射表。
3. **radio 模型要扩字段**：`radio`（phy 索引）、`htmode` 需支持 EHT160/EHT320、
   `country`、`cell_density`、beamformer 开关、`he_bss_color`、`radar_background`、`he_twt_responder`。
4. **模板要能表达"同 SSID 名但不同 VLAN/加密"**，以及逐 VLAN 的 `mobility_domain`。
5. **加密串**要支持 `psk2+ccmp` / `sae-mixed` / `sae+ccmp`。
6. **agent 健康检查已按真机修正**：`up` 在 radio 层而非接口层。修正前会把所有 BSS 判为 down
   并触发回滚——这个 bug 只有拿到真机输出才暴露得出来。

## 7. P1 验收结果（2026-10-31）

渲染器已按本文档重写（`bridge-vlan`、EHT、`option radio`、每 VLAN mobility_domain），
并以本目录的 dump 作为基准做了逐段比对：

```
docs/baseline/ap251/template.json   由 dump 生成的黄金模板（18 SSID / 3 radio / 7 VLAN）
internal/render/parity_test.go      渲染该模板，与 wireless.uci / network.uci 逐段比对
```

结果：**零差异**。wireless 比对了 3 个 `wifi-device` 与 18 个 `wifi-iface` 的全部选项；
network 比对了 1 个 bridge device、7 个 `bridge-vlan`、6 个 interface。

比对口径（两处必须说明，否则"零差异"不可信）：

1. **按段身份匹配、按选项集合比较**，不按字节。现网是手写的，同一选项在不同段里顺序不同；
   而控制器生成的段带 `ac_` 前缀（生产）或不带（比对时），段名本就会变。
2. **唯一归一化**：`option ipaddr X` 与 `list ipaddr X` 视为等价（netifd 语义相同）。
   现网 `lan` 用 list、`lan2`…`lan6` 用 option，渲染器统一用 list。

比对器自身有反向测试（`TestComparatorDetectsDifferences`）：改动一个选项、删掉一个段，
必须被报出来，否则"零差异"可能只是比对器失灵。

### 过程中修掉的两个真问题

- **段名冲突**：`bridge-vlan` 与同 VLAN 的 `interface` 都叫 `ac_vlan<vid>`。UCI 段名在同一
  文件内全局唯一，uci 会静默改名。已改为 `ac_bvlan<vid>`，并加了唯一性回归测试。
- **`ieee80211v` 被错误推导**：原本从 `bss_transition` 推导，但现网只有 2.4G 的 BSS 设了
  这个遗留开关，5G/6G 没有。已拆成独立的三态字段。

## 8. 802.11k / 11v / 11r 生效情况（2026-10-31 逐 BSS 实测）

结论：**三项全部生效**。以下数据取自 hostapd 的运行时配置
`/var/run/hostapd-phy{0,1,2}.conf`（hostapd 启动时读取的就是这些文件，
镜像里没有 `hostapd_cli`，所以以文件为准）。

| radio | BSS 数 | 11k `rrm_neighbor_report` / `rrm_beacon_report` | 11v `bss_transition` / `wnm_sleep_mode` | 11r `mobility_domain` / `ft_over_ds` / key mgmt |
|---|---|---|---|---|
| radio0 (2.4G) | 1 | 1/1 | 1 / 1 | **无**（无 mobility_domain，`wpa_key_mgmt=WPA-PSK`） |
| radio1 (5G) | 4 | 1/1 ×4 | 1 / 1 ×4 | 1111–4444 / 0，`wpa_key_mgmt` 含 `FT-SAE` `FT-PSK` |
| radio2 (6G) | 6 | 1/1 ×6 | 1 / 1 ×6 | 1111–6666 / 0，含 `FT-*` |

与 UCI 配置意图完全一致：只有 2.4G 的 BSS 没开 11r（见 §3.2）。

另外两条实测：

- **驱动侧具备 RRM**：`iw phy phy0 info` 输出 `* [ RRM ]: RRM`。
- **`option ieee80211v` 已不是 hostapd 的键**：三个 phy 的 conf 里 `ieee80211v=` 出现 0 次，
  11v 由 `bss_transition` + `wnm_sleep_mode` 表达。渲染器保留该字段是为了与现网基线一致，
  它在这个固件上是空操作，不影响结果。

### 一条方法论教训（曾在此处误判）

第一次检查时我用 `grep '^ieee80211k='` 在 hostapd conf 里找 11k，得到 0 条，
据此得出"11k 未生效"的结论，并写进了文档与提交信息。**这个结论是错的。**

原因：UCI 的 `ieee80211k` 只是**开关**，落到 hostapd 的是它派生出的两个键
`rrm_neighbor_report` / `rrm_beacon_report`。LuCI 的实现印证了这一点
（`/www/luci-static/resources/view/network/wireless.js`）：

```js
o = ss.taboption('roaming', form.Flag, 'ieee80211k', _('802.11k RRM'), ...);
o = ss.taboption('roaming', form.Flag, 'rrm_neighbor_report', _('Neighbour Report'), ...);
        o.depends({ ieee80211k: '1' }); o.default = o.enabled;
o = ss.taboption('roaming', form.Flag, 'rrm_beacon_report', _('Beacon Report'), ...);
        o.depends({ ieee80211k: '1' }); o.default = o.enabled;
```

也就是说 `ieee80211k=1` 会**默认带出**两个 rrm 键——查 `ieee80211k=` 必然查不到。

正确的验证方式（也是之后每次都要用的）：

```sh
# 11k：看 rrm 键，不要看 ieee80211k
grep -cE '^rrm_neighbor_report=1' /var/run/hostapd-phy*.conf     # 应等于在线 BSS 数
# 11v：bss_transition 与 wnm
grep -cE '^bss_transition=1' /var/run/hostapd-phy*.conf
# 11r：mobility_domain 存在 + wpa_key_mgmt 含 FT-
grep -hE '^wpa_key_mgmt=' /var/run/hostapd-phy*.conf | sort -u
```

教训：**验证一个 UCI 选项是否生效，不能只 grep 同名键**——OpenWrt 的无线栈会把一个 UCI
选项展开成多个 hostapd 键，也可能整个忽略它。要么查 LuCI 的映射关系，要么在改动前后
diff 整个运行时 conf。

## 9. 原始文件

| 文件 | 内容 |
|---|---|---|
| `wireless.uci` | `uci export wireless`（337 行，已脱敏） |
| `network.uci` | `uci export network`（98 行） |
| `dhcp-system.uci` | `uci export dhcp` + `uci export system`（115 行） |
| `wireless-status.json` | `ubus call network.wireless status`（已脱敏） |
| `template.json` | 由上述 dump 生成的黄金模板，P1 比对基准 |

重新生成黄金模板（不改设备）：

```sh
python scripts/uci-to-template.py \
    --wireless docs/baseline/ap251/wireless.uci \
    --network  docs/baseline/ap251/network.uci \
    --id ap-a --name "AP-A 现网基线" --redact-psk REDACTED-PSK \
    --out docs/baseline/ap251/template.json
```

重采命令（只读）：

```sh
ssh root@10.0.30.251 'uci export wireless' > wireless.uci
ssh root@10.0.30.251 'uci export network'  > network.uci
ssh root@10.0.30.251 'ubus call network.wireless status' > wireless-status.json
```
