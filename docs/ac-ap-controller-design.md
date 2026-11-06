# AC + AP 集中管理方案（XR1710G 胖瘦一体）

> 状态：方案草案（v0.1），待评审后进入 P0 验证
> 目标设备：Gemtek XR1710G × 2（AP250 / AP251）
> 参照产品：TP-Link Omada 软件控制器（EAP 系列）

---

## 1. 目标与硬约束

### 1.1 目标

1. 一台 AC 统一下发无线 SSID、端口 VLAN、射频参数，AP 侧零手工配置。
2. AP 保持**胖瘦一体**：纳管时由 AC 接管，脱离 AC 时可回落到本地配置独立工作。
3. 功能面尽量对齐 Omada：设备发现与纳管、SSID/加密/VLAN 模板、射频与功率、802.11k/v/r 漫游、客户端列表、AP 状态遥测、批量重启与升级。
4. AC 侧交互形态参照 Omada（左侧功能栏 + 站点视图 + 设备/无线/客户端三大块），不追求像素级复刻。

### 1.2 硬约束（不可协商）

| 编号 | 约束 | 影响 |
|---|---|---|
| C1 | **保留 NPU（AN7581 PPE）加速** | 用户数据必须本地转发，禁止任何回传 AC 的隧道 |
| C2 | 不改动上游内核的 PPE / mt76 卸载路径 | 只在配置层做工作，不引入新的数据面旁路 |
| C3 | 单 AP 故障不得导致整网站点不可用 | AC 失联时 AP 维持最后一次生效配置 |
| C4 | 配置下发必须可回滚 | 每次下发前备份，健康检查失败自动回退 |

约束 C1 是架构分水岭：它直接排除了 CAPWAP 数据隧道（Data Channel）、EoGRE、VXLAN 回传等"集中转发"模型。**本方案只做控制面集中，数据面留在 AP。**

---

## 2. 现状基线

### 2.1 硬件（已从仓库确认，非推断）

依据 `target/linux/airoha/dts/an7581-gemtek-xr1710g-ubi.dts`：

- SoC：Airoha AN7581（`airoha,an7581` / `airoha,en7581`），2 GiB DRAM
- 无线：PCIe0 下挂 **MT7996**，三个 band 节点分别为 2.4 GHz / 5 GHz / 6 GHz，各自独立 MAC（`lan_mac` +1/+2/+3）
- 有线：GDM2 = wan（USXGMII）、GDM4 = lan2（USXGMII）、GDM1 = cpu；LAN2/LAN3 由内部 MT7530 switch 直驱
- NPU：`&npu { status = "okay" }`，且 `mt7996@0,0` 绑定了 `airoha,npu = <&npu>` 与 `airoha,eth = <&eth>`

### 2.2 NPU 卸载现状（决定方案边界）

仓库中已具备的卸载能力：

| 层 | 实现 | 文件 |
|---|---|---|
| L2 桥接 | native L2B 条目，驱动在 QDMA RX 未命中时解析 Ethernet + VLAN(1~2 层) + PPPoE 并绑定 | `9999-net-airoha-ppe-l2b-encap-decode.patch`、`9994-...fix-native-l2b-entry-layout`、`9995-complete-ppe-offload` |
| WLAN 绑定 | WiFi 流在 L2 cache miss 时绑定到 PPE | `9990-net-airoha-bind-WLAN-bound-flows-on-PPE-driver-L2-cache-miss.patch` |
| STA 生命周期 | mt7996 删链时经 NPU mailbox 发 `WLAN_FUNC_SET_WAIT_DEL_STA`，清理陈旧 per-station 路由 | `package/kernel/mt76/patches/0003-...notify-NPU-on-station-removal...` |
| 组播 | MDB → PPE 组播绑定/泛洪 | `9997`、`9998`、`9999-...multicast-*` |

**两条对方案有直接约束的既有结论**（`docs/airoha-bridge-flowtable-offload.md`）：

1. 当前内核（6.18.55）**没有** `NFPROTO_BRIDGE` 类型的 flowtable，`table bridge fw4` 永远为空表；桥接加速由 Airoha native L2B 完成，与 nftables 无关。
2. `inet` 表的路由/NAT 卸载仍由 `firewall4` 的 `flow_offloading` / `flow_offloading_hw` 控制。

推论：**AP 作为纯二层设备时，转发路径完全由 L2B 承担；任何在 AP 上做的软件旁路（tc/eBPF 限速、nft 桥接规则、隧道封装）都会让流量掉出快路径。**

### 2.3 现网基线（AP250 / AP251 实测状态）

两台 AP 当前已处于桥接（胖）模式并承载业务，这是方案最现实的起点：

| 项 | 现状 |
|---|---|
| 地址 | AP250 = 10.0.10.250，AP251 = 10.0.10.251 |
| 上联 | 交换机 Port8，wan 口 trunk VLAN1–6 |
| 无线 | 已载入 **6 组 SSID × 三频**（SSID-A/SSID-A_5G/SSID-A_6G、SSID-A2/SSID-B_5G/SSID-B_6G、SSID-A3/SSID-C_5G/SSID-C_6G、SSID-A4-6/SSID-D-F_5G/6G） |
| VLAN 映射 | SSID → netifd interface（lan/lan2/…/lan6）→ `br-lan.<vid>` 子接口 → wan trunk |
| 6 GHz | **已启用并在用**（radio2 承载 SSID-A_6G / SSID-B_6G / SSID-C_6G 等） |
| DHCP/DNS | AP 上已全部关闭（dnsmasq/odhcpd disable+stop），由 10.0.10.1 统一提供 |
| 网关 | `network.lan.gateway = 10.0.10.1`，单条默认路由 |
| NTP | 客户端模式，源为 10.0.10.1 |

三点直接结论：

1. **6 GHz 与多 BSSID 不是风险项**，已在生产流量下运行（原 §7.4 的 R2、R3 降级为"已具备"）。
2. **VLAN 模型已定型**：`br-lan` 上做 `br-lan.<vid>` 软件子接口，每个 VLAN 一个 netifd interface，SSID 用 `option network` 绑定。方案 D2 直接沿用，不再另创新模型。
3. **AP 必须是纯二层**：现网已无 DHCP/DNS，符合 §7.2 的最优形态（L2B 承担全部转发）。

---

## 3. 开源方案调研结论

| 项目 | 定位 | 活跃度 | 对本项目的适用性 |
|---|---|---|---|
| **TIP OpenWiFi / uCentral**（`wlan-cloud-ucentralgw`、`wlan-ap`） | 云 AC + AP NOS，JSON over WebSocket，schema 驱动 | 活跃（2026-10 仍在更新） | **架构最接近**。但需要 Kafka + PostgreSQL + 多个微服务，AP 侧需集成整套 `ucentral-client` 与自研 Airoha 内核，整合成本高。**取其协议与 schema 设计，不整体引入。** |
| **OpenWISP**（`openwisp-controller` 792★ / `openwisp-config`） | Django 控制器 + AP 侧 agent，NetJSON 下发，x509 PKI，monitoring | 非常活跃 | **AP 侧机制可复用**（`openwisp-config` 是 OpenWrt 标准包，apk/opkg 可直接装）。但 Python 栈重、NetJSON 对 WiFi7/6GHz/MLO 覆盖不足。**作为一期落地通道的备选。** |
| **OpenSOHO**（199★，Go + PocketBase） | OpenWISP 的轻量替代，2~20 台设备，复用 openwisp-config | 活跃（2026-10） | 与本项目规模高度匹配，可直接读源码借鉴"设备注册 + 配置下发 + 监控"三段式实现。功能面偏薄（无模板、无固件升级）。 |
| **DAWN** | 去中心化客户端引导（802.11k/v） | **基本废弃**，近年无实质开发，新版 OpenWrt 上不可靠 | 不采用 |
| **usteer** | OpenWrt 官方客户端引导守护 | 活跃 | 可选。已知问题较多（`roam_kick_delay` 不生效、load steering 不触发、部分网卡忽略 BTM）。**只用宽松参数（仅 BTM 建议，不做强制踢除）。** |
| **nrsyncd** | 仅同步 802.11k 邻居报告，不做 steering | 新 | **推荐替代 usteer**：职责单一、不易出错。客户端自己决策漫游。 |
| **openCAPWAP-OpenWRT**（177★） | 标准 CAPWAP 实现 | 2025 有更新 | **不采用**。CAPWAP Data Channel 是集中转发模型，违反 C1；且其 hostapd 集成与 DSA/VLAN 支持薄弱。 |
| **FreeSDN** | 多厂商适配层（含 Omada/UniFi） | 新，7★ | 参考其"厂商适配器"抽象，不直接使用（自述仅限实验环境）。 |

**结论：没有可直接拿来即用的开源 AC 满足"三频 WiFi7 + NPU 本地转发 + 胖瘦一体"。采取"自研轻 AC + 借鉴 uCentral 协议形态 + 复用 OpenWrt 原生组件"的路线。**

---

## 4. 总体架构

### 4.1 分层

```
┌──────────────────────────── AC（Go 单二进制 / Docker）────────────────────────────┐
│  Web UI (仿 Omada)          REST API            WebSocket 推送                     │
│  ├ 站点/设备    ├ 无线模板   ├ 发现服务(UDP)    ├ 配置引擎(模板→渲染→签名)          │
│  ├ 射频/漫游    ├ 客户端     ├ 遥测入库(SQLite)  └ 固件仓库                         │
└───────────────────────────────────┬──────────────────────────────────────────────┘
                                    │ HTTPS 443（mTLS 或 shared-secret 换 token）
                                    │ 配置拉取 / 遥测上报 / 心跳
┌───────────────────────────────────┴──────────────────────────────────────────────┐
│                        AP：OpenWrt + ac-agent（ucode 或 Go）                       │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────────────────────────┐  │
│  │ ac-agent     │  │ hostapd      │  │ 数据面（NPU 快路径，AC 不参与）          │  │
│  │ 拉取/验签/应用 │→ │ (wpad-mesh)  │→ │ wlan → bridge → VLAN → GDM/MT7530      │  │
│  │ 备份/回滚/上报 │  │ 多 BSSID     │  │        ↑ PPE L2B（native，非 nftables） │  │
│  └──────────────┘  └──────────────┘  └────────────────────────────────────────┘  │
│  本地 LuCI 保留（胖模式直接生效；瘦模式下 ac_ 段只读）                              │
└──────────────────────────────────────────────────────────────────────────────────┘
```

### 4.2 职责边界

| 平面 | 归属 | 说明 |
|---|---|---|
| 管理面 | AC | 配置生成、模板、审计、UI、遥测聚合 |
| 控制面 | AP 本地 hostapd | 关联、认证、EAPOL、key 交换全在 AP，AC 不碰 |
| 数据面 | AP 本地 | bridge + VLAN + PPE，**永不出 AP** |
| 认证面 | 无 | 不做 802.1X / Portal；无线安全只用 PSK / SAE / OWE |

### 4.3 部署形态

控制器以单二进制 + Docker 运行在 **x86 NAS**：

- `network_mode: host` —— AP 发现依赖接收子网广播，bridge 网络收不到；
- 数据卷 `./data` 挂载 SQLite 与控制器身份文件，容器重建不丢 `ac_id`；
- 镜像多阶段构建，`CGO_ENABLED=0`，产物约 11 MB；同一份源码可直接交叉编译到 arm64（若日后换 arm NAS）。

---

## 5. 关键技术决策

### D1：数据面本地转发（否决 CAPWAP 隧道）

- **选择**：AP 本地转发，AC 仅下发配置。
- **理由**：C1。CAPWAP Data Channel 会把 802.3 帧封进 UDP 送回 AC，PPE 的 L2B 条目失去意义，MT7996 的 WLAN→NPU 绑定路径（`9990` 补丁）也被绕过，实测吞吐会退回 CPU 软转发量级。
- **代价**：AC 无法在数据面做集中策略（如统一限速、集中 ACL）。这些能力下沉到 AP 本地，由 AC 下发规则实现。

### D2：SSID → VLAN 沿用现网 `br-lan.<vid>` 模型

| 方案 | 结构 | 结论 |
|---|---|---|
| **A（采用）** | 单 `br-lan`（含 uplink 口），其上建 `br-lan.<vid>` 软件 802.1q 子接口；每 VLAN 一个 netifd interface；SSID 用 `option network` 绑定 | **现网 6 SSID × 三频已在跑**，AC 只需参数化，不换模型 |
| B | 每 SSID 独立 bridge + 独立 802.1q 子接口 | 不采用。bridge 数随 SSID 膨胀，且与现网不兼容 |
| C | 单 `br-lan` + `vlan_filtering=1`，每 wlan 口设 PVID（DSA bridge-vlan） | 不采用。需改写现网结构，且 L2B 在 vlan domain 下的行为无任何验证数据 |

**决策：A。** 理由：现网已用该模型稳定承载业务，换模型只增加风险不增加能力；AC 的价值在于把"6 组 SSID × 3 频 × 6 VLAN"从手工 UCI 变成模板一次生成，而不是改变转发结构。

> 唯一遗留待验证：`wlan → br-lan → br-lan.<vid> → wan` 的 egress 是否命中 L2B（§7.3）。现网在用，但从未做过卸载实测——这是 P0 的核心任务。

### D3：配置下发通道——自研轻 agent，参照 OpenWISP 而非直接使用

| 选项 | 说明 | 结论 |
|---|---|---|
| 直接用 `openwisp-config` | OpenWrt 标准包，成熟，自带 monitoring | 备选。受限于 NetJSON 表达力（6GHz/MLO/802.11r 细节缺失），且应用过程是黑盒，难以精确控制 reload 顺序 |
| **自研 ac-agent** | ~600 行（ucode + shell，或 Go 静态二进制） | **主选**。可精确控制"验签 → 备份 → 应用 → 健康检查 → 回滚"，且能保证 `wifi reload` 只触发一次、不反复重建 bridge（避免 flush PPE 表） |

**决策：自研 agent，协议形态抄 uCentral（版本化配置包 + 心跳 + 遥测），数据模型参考 NetJSON DeviceConfiguration 的字段命名以便未来兼容。**

配置包格式：

```
acpkg-<rev>.tar.gz
├── manifest.json      # rev, sha256, 目标设备型号白名单, 生成时间
├── wireless           # 完整 /etc/config/wireless（含 ac_ 前缀 section）
├── network            # 片段，仅 ac_ 相关 interface/device
├── system             # 可选：hostname/timezone/led
└── apply.sh           # 可选钩子（默认不启用）
```

应用流程（原子）：

1. 拉取 → 校验 Ed25519 签名 + sha256
2. `cp -a /etc/config /etc/ac/backup/<rev-1>`
3. 写入 → `uci commit`
4. `reload_config`（仅当 network 变更）→ `wifi reload`
5. 健康检查：5 s 内 `ubus call network.wireless status` 中所有预期 BSS 处于 up
6. 失败 → 回滚 backup → 上报 `apply_failed`；成功 → 上报 `applied_rev`

### D4：漫游层不自研

- **决策**：AC 下发 802.11k/v/r 参数；steering 交给 AP 本地 `nrsyncd`（首选）或 `usteer`（宽松参数）。
- **理由**：DAWN 已废弃；usteer 的强制踢除类功能在多份实测中产生断连风暴。AC 若承担实时 steering 决策，等于把毫秒级决策放到网络另一侧，收益为负。
- **前提**：固件内置 `wpad-mesh-mbedtls`（1710.config 已经是此默认），含 802.11k/v/r + SAE。

### D5：发现协议（仿 Omada 的二层发现）

AP 启动 / 每次心跳失败后：

1. 向 `255.255.255.255:29820` 与站点组播组发 UDP `AP_DISCOVER`（MAC、型号、固件版本、当前 AC、uptime）
2. AC 监听并回 `AC_OFFER`（AC 地址、TLS 端口、证书指纹）
3. AP 用指纹校验后建立 HTTPS，携带 shared-secret 或预置客户端证书注册
4. 兜底：mDNS `_acproto._tcp`、手工填写 AC 地址（`/etc/config/ac` 的 `ac_addr`）

端口选 29820 而非 Omada 的 29810，避免与真机 Omada 共存时冲突。

---

## 6. 数据模型

### 6.1 AC 侧实体

```
Site        站点（一期单站点，字段预留）
ApDevice    mac, model, fw, ip, state(pending|adopted|offline), last_seen,
            wifi_template_id, port_profile_id, radio_overrides
WifiTemplate  name, ssids[], radio_policy, roaming_policy
  Ssid        ssid, hidden, bands[2g|5g|6g], vlan_id, security(wpa2|wpa3|wpa2/wpa3|
              owe|open|wpa2-ent|wpa3-ent), psk, radius, client_isolation,
              rate_limit{up,down}, schedule, mac_filter
VlanDef     vid, name, purpose(管理|业务|访客|IPTV), dhcp(relay|local|none)
PortProfile 每端口 mode(access|trunk|hybrid), pvid, allowed_vlans[], poe
RadioPolicy 每 band: channel, width, txpower, min_rssi, disassoc_low_ack,
              k/v/r 开关, mobility_domain
Client       mac, hostname, ap_mac, band, rssi, tx/rx rate, online, first/last_seen
Telemetry   ap_mac, ts, per_radio{chan_util, noise, clients, tx/rx bytes},
            per_iface{rx/tx bytes, errors}, npu{l2b_entries_bound}
```

### 6.2 下发到 AP 的关键 UCI 映射（示例）

```uci
# /etc/config/wireless —— AC 生成的 section 统一 ac_ 前缀
config wifi-device 'ac_radio0'
    option band '2g'
    option channel '6'
    option htmode 'HE40'
    option txpower '20'

config wifi-iface 'ac_ssid_corp_2g'
    option device 'ac_radio0'
    option mode 'ap'
    option ssid 'Corp'
    option encryption 'sae-mixed'
    option key '...'
    option network 'ac_vlan10'
    option ieee80211r '1'
    option ft_over_ds '1'
    option mobility_domain 'a1b2'
    option ieee80211k '1'
    option bss_transition '1'
    option isolate '1'
```

```uci
# /etc/config/network —— 沿用现网模型：br-lan 上建 802.1q 子接口
config device 'ac_br_lan_10'
    option type '8021q'
    option ifname 'br-lan'
    option vid '10'
    option name 'br-lan.10'

config interface 'ac_vlan10'
    option device 'br-lan.10'
    option proto 'none'
```

---

## 7. NPU 加速保留方案

### 7.1 五条实施纪律

1. **禁止隧道**：不做 CAPWAP data / EoGRE / VXLAN / GRE 回传。
2. **bridge 结构稳定**：AC 下发的 bridge 集合在启动时确定；运行期只允许增删 wlan 成员，不允许反复 `down/up` 整个 bridge（每次重建会 flush PPE 表，表现为数秒降速）。
3. **不用软件旁路做策略**：限速/隔离用 hostapd 与驱动能力（`isolate`、`wmm`、`airtime`），不用 tc htb / eBPF / nft limit —— PPE 卸载后这些看不到包，实测已确认失效。
4. **STA 下线必须回写 NPU**：保留 `0003-wifi-mt76-notify-NPU...` 补丁，否则重连后下载速率卡在 ~1 Mbps。
5. **管理流量走 CPU 无妨**：AC 心跳/遥测是 KB 级，不参与卸载，也不应被误配进 flowtable。

### 7.2 部署形态对卸载的影响

| 形态 | 卸载状态 |
|---|---|
| AP 纯二层（AC 侧网关做三层） | L2B 承担全部桥接转发，**最优** |
| AP 兼做三层（本地 NAT/DHCP） | `inet` flowtable 承担路由/NAT 卸载，需 `flow_offloading_hw=1` |
| AP 做隧道回 AC | **不可用**（本方案排除） |

### 7.3 验收方法（每阶段必跑）

```sh
# 1) 硬卸载是否开启
nft list ruleset | grep -A2 flowtable          # 应有 inet fw4 的 flowtable，且设备为 flowtable 接口
uci get firewall.@defaults[0].flow_offloading_hw  # 期望 1

# 2) PPE L2B 条目绑定情况（驱动 debugfs）
cat /sys/kernel/debug/airoha_ppe/* 2>/dev/null | grep -c BND

# 3) 快慢路径对比：持续打流时 CPU 侧收包应远小于总包数
ip -s link show eth0        # GDM1(CPU) 计数
ip -s link show lan2        # 上联计数

# 4) 稳定性质疑点：STA 断开重连后速率不得塌陷
#    iperf3 -c <server> -t 30  → 断开 → 重连 → 再测，两次应同量级
```

**判定标准**：打流 200 Mbps 以上时，CPU 端口 RX 包数增速应 < 总包数的 5%；L2B 绑定条目数随活跃流增长。

### 7.4 待实测风险点（P0 必须回答）

| 编号 | 风险 | 验证方式 |
|---|---|---|
| **R1** | `wlan → br-lan → br-lan.<vid>` 软件子接口 egress 是否命中 L2B（**唯一高优先级未知项**） | 上述 7.3 全套，在现网 6 SSID 负载下实测 |
| R2 | ~~6 GHz 在 CN regdb 下是否可用~~ | **已排除**：现网 radio2 已承载 SSID-A_6G / SSID-B_6G / SSID-C_6G |
| R3 | 单 radio 最大 BSSID 数（MT7996 通常 16/band，需实测） | 现网每 radio 已跑 2 个 BSS；上限逐个加 BSS 试到失败 |
| R4 | 多 BSSID × 多 VLAN 时 NPU per-station 表容量 | `/sys/kernel/debug/airoha_npu/*` 表项计数 |
| R5 | 802.11r + SAE + 多 VLAN 组合的 FT 成功率 | 两台 AP 间反复漫游，抓 hostapd FT action |

---

## 8. 胖瘦一体设计

### 8.1 状态机

```
                 ┌──────────── 长按 reset 5s / LuCI 开关 ────────────┐
                 ▼                                                   │
   ┌────────────┐   注册成功    ┌────────────┐   AC 失联 >30min   ┌──┴─────────┐
   │  fat 模式   │ ────────────→ │  thin 模式  │ ─────────────────→ │ thin-standalone │
   │ 本地配置生效 │ ←──────────── │ AC 配置生效 │                    │ 维持最后配置 │
   └────────────┘   解绑/退出    └────────────┘ ←─────────────────── └────────────┘
```

- **fat**：`ac-agent` 停止，`ac_` 前缀的 UCI 段全部 `disabled`，本地手配的 SSID 生效。出厂默认。
- **thin**：`ac-agent` 运行，AC 下发的 `ac_` 段生效，本地段自动 disable（不删除，保留以便回退）。
- **thin-standalone**：AC 长时间失联，**不自动切 fat**（避免与 AC 恢复后脑裂），仅维持最后配置并持续重试；本地 LuCI 可手动接管。

### 8.2 本地保护

- 瘦模式下 LuCI 的无线/网络页面进入只读提示（通过 `luci-app-ac-client` 覆盖视图，或简单地在页面顶部插入告警横幅 + 禁用保存）。
- 保留 failover：AP 上电后 8 s 内 AC 不可达，先加载最后一次成功配置，再后台重试。
- 设备证书/密钥存 `/etc/ac/`，`sysupgrade` 时保留（加入 `/etc/sysupgrade.conf`）。

---

## 9. 功能矩阵（对齐 Omada）

| 模块 | 一期 | 二期 | 说明 |
|---|---|---|---|
| 设备发现与纳管 | ✅ UDP 广播 + 手工 IP | mDNS、批量导入 | 待采纳列表、一键采纳 |
| 设备状态 | ✅ 在线/离线、IP、固件、客户端数、uptime | 信道利用率曲线 | 遥测 10 s 粒度 |
| 无线 SSID | ✅ 多 SSID、绑定 band、VLAN、加密、隐藏、隔离 | 定时开关、MAC 过滤、速率限制 | |
| 射频 | ✅ 信道/带宽/功率 | 自动信道（按扫描结果） | 自动信道二期做 |
| 漫游 | ✅ k/v/r 开关 + mobility domain | 邻居报告视图 | steering 交给 nrsyncd |
| 端口 VLAN | ✅ access/trunk、PVID、允许列表 | — | XR1710G 的 LAN 口无 PoE 输出，不做 PoE 控制 |
| 客户端列表 | ✅ 在线客户端、RSSI、速率、所属 AP | 漫游历史、流量趋势 | |
| 批量操作 | ✅ 重启、重新下发 | 固件批量升级（sysupgrade 镜像仓库） | |
| 告警 | ✅ 离线、配置失败 | 邮件/Webhook | |
| 多站点 | — | 预留字段 | 一期单站点 |

---

## 10. 实施阶段

| 阶段 | 内容 | 产出 | 门槛 |
|---|---|---|---|
| **P0** 卸载基线 + 黄金配方 | ① 导出 AP250/251 现有 `uci export wireless/network` 作为黄金基线；② 在现网 6 SSID/VLAN 负载下跑 §7.3 全套，确认 L2B 命中（R1/R4）；③ 记录 BSSID 上限（R3） | `docs/ac-p0-baseline.md` + 实测数据 + 两份基线配置 | R1 结论明确；基线可复现 |
| **P1** 模板化 | 把黄金基线抽象为参数化模板（VLAN 池 / SSID 组 / 三频绑定 / k+v+r），渲染结果与基线逐字节比对 | 可复用的 UCI 模板 + 渲染器 | 渲染结果 == 现网配置，diff 为空 |
| **P2** AC 骨架 | Go 服务 + SQLite + 发现 + 注册 + 配置包生成/签名 + agent 应用与回滚 | 可用的 CLI 推送 | 手工构造模板能推到 AP 并生效 |
| **P3** Web UI | 仿 Omada 布局：设备/无线/射频/端口/客户端五页 | 可演示 | 全流程不碰 SSH |
| **P4** 遥测与运维 | 客户端表、射频统计、NPU 表项、重启、日志 | 状态页可用 | 数据 10 s 内刷新 |
| **P5** 双 AP 联调 | AP250 + AP251 漫游、负载、AC 单点故障演练 | 验收报告 | 漫游切换 < 150 ms（FT） |

依赖关系：P0 → P1 → P2 →（P3 与 P4 并行）→ P5。

---

## 11. 风险清单

| 风险 | 等级 | 缓解 |
|---|---|---|
| PPE 在 SSID+VLAN 组合下不卸载 | **高** | P0 先验证；失败则接受 L2 软转发（功能不受影响，吞吐降级）或退回 `vlan_filtering` 方案 |
| hostapd 多 BSSID 反复 reload 触发 PPE 表 flush | 中 | agent 合并变更，单次 `wifi reload`；避免全量重建 |
| AC 成为单点 | 中 | AP 维持最后配置；AC 侧定期备份配置库 |
| usteer 断连风暴 | 低（已规避） | 默认不启用强制踢除，只用 nrsyncd |
| MT7996 驱动与 802.11r/MLO 兼容性 | 中 | P1 阶段压测；MLO 一期不做 |
| 现网 6 SSID 配置被 AC 覆盖后业务中断 | **高** | P1 前先导出两台 AP 的 `uci export wireless/network` 作为黄金基线；AC 首版以"渲染结果 == 基线"为验收 |

---

## 12. 已确认决策与遗留事项

### 已确认（2026-10-30）

| 项 | 决策 | 影响 |
|---|---|---|
| AC 载体 | **x86 NAS 上的 Docker** | 单二进制 + `network_mode: host`（发现依赖子网广播）；配置与身份文件挂 `./data` |
| 认证方式 | **不做 802.1X / Portal** | 不引入 FreeRADIUS，不实现 RADIUS 下发；无线安全只用 PSK / SAE / OWE |
| PoE 控制 | **不做** | XR1710G 的 LAN 口无 PoE 输出，"端口"页不含 PoE 项 |

### 遗留

1. **管理 VLAN 规划**：AC 与 AP 的管理地址是否走独立 VLAN（建议是，当前 AP 管理地址在 10.0.10.0/24）。
2. **现网 6 组 SSID 是否全部保留**：AC 首版建议逐条迁移（先接管 1 组，验证后再扩），而非一次性接管全部。
