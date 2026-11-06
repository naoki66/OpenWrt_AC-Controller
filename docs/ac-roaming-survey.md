# 开源 AC/AP 的漫游是怎么实现的

> 调研时间：2026-10-31。对象：OpenWrt 生态的 usteer / DAWN / nrsyncd / dnrsd，
> 以及两个真正的"控制器"项目 OpenWISP 与 TIP OpenWiFi（uCentral）。
> 结论先行：**没有一个开源 AC 在做集中式实时 steering，决策全在 AP 本地。**

---

## 1. 先厘清一件事：漫游的决策权在客户端

AP 不能"命令"一台设备漫游。802.11 里 AP 只有三种手段，力度从弱到强：

| 手段 | 标准 | AP 能做什么 | 客户端可否无视 |
|---|---|---|---|
| 给信息 | 11k 邻居报告 | 告诉客户端"附近还有哪些 BSS、什么信道、什么能力" | 可以，但多数会用 |
| 提建议 | 11v BTM（BSS Transition Management） | 发一个带候选列表的"你该走了"请求 | 可以，iPhone/新 Android 通常接受，部分 Intel 网卡完全无视 |
| 断连接 | 无（私有手段） | 拒绝关联 / `del_client` 踢除 | 不能，但体验最差 |

所以所有开源项目的差别，不在于"能不能让设备漫游"，而在于**用什么数据判断该不该建议**、
**敢不敢强制**。

## 2. 三层分工（这是理解全局的关键）

```
┌─ 控制器层（AC）─────────────────────────────────────────┐
│ OpenWISP / uCentral / Omada / UniFi                     │
│ 只做两件事：下发 k/v/r 参数、下发 steering 策略参数       │
│ 不做实时决策（uCentral 也有 cloud 模式，但默认 local）     │
└─────────────────────────────────────────────────────────┘
                          │ 配置
┌─ AP 本地协调层（守护进程）──────────────────────────────┐
│ usteer / DAWN：交换 STA 信号 + 做决策 + 发 BTM/踢除       │
│ nrsyncd / dnrsd / static-neighbor-reports：只交换邻居报告 │
└─────────────────────────────────────────────────────────┘
                          │ ubus
┌─ 协议层（hostapd）──────────────────────────────────────┐
│ 实现 11k/11v/11r 的报文动作，但**不含决策逻辑**，          │
│ 邻居列表必须由外部喂进来（rrm_nr_set）                     │
└─────────────────────────────────────────────────────────┘
```

OpenWrt 官方文档写得很明确：*"hostapd includes support for 802.11k and 802.11v, but relies on
external software to make roaming recommendations and provide it with lists of alternative BSSIDs"*。

**这句话解释了大量"开了 11k 却没用"的现场**——见本文 §5。

## 3. 各项目的实现方式

### 3.1 usteer（OpenWrt 官方推荐）

- **发现对端**：UDP 广播/组播，默认在 `lan` 接口上互发；`ipv6` 可切换。
- **数据来源**：每个实例通过 ubus 从**本地 hostapd** 取 STA 信息（信号、连接时长），
  同时把"我能看到某 STA 的信号强度"广播给其它实例，形成全局 **hearing map**。
- **决策**：按 SNR 差、channel load、band、station 数打分；策略可配
  `signal_diff_threshold`、`roam_scan_snr`、`roam_trigger_snr`、`assoc_steering`、
  `load_balancing_threshold` 等 30 多个参数。
- **执行**：通过 ubus 调 hostapd：

  | usteer 动作 | hostapd 方法 | 说明 |
  |---|---|---|
  | 发 BTM 建议 | `bss_transition_request` | 最主要的温和手段 |
  | 触发测量 | `link_measurement_req` | 让客户端上报链路质量 |
  | 让客户端扫描 | `rrm_beacon_req` | 11k beacon report |
  | 踢人 | `del_client` | 强制断连，风险最高 |

- **也负责 11k 邻居列表**：把各节点的 `rrm_nr` 汇总后喂给 hostapd。
- **已知问题**（社区共识，不是个例）：`roam_kick_delay` 不生效、channel load steering 从不触发、
  Intel 网卡无视 BTM、snapshot 上偶发不稳。所以现在的推荐用法是"**关掉所有 kick，只留温和引导**"。

### 3.2 DAWN（设计文档里被我否决的那个，否决理由成立）

- 去中心化**对等**网络，无主节点；用 **umdns(mDNS)** 发现 peer。
- `network_option`：0=UDP 广播、1=组播、2=TCP、3=…（默认依赖 umdns，社区反馈最不稳定的一项）。
- 打分模型：`rssi`、`chan_util`、`ht_support`/`vht_support`、`use_station_count`、`freq`（频段偏好），
  按 `sort_order`（默认 `cbfs`）比较，选出最优 AP。
- 手段最激进：除了发 NR 与 BTM，还会**拒绝 probe / auth / assoc**，以及 `kicking`。
- **状态**：2022 年后基本停滞，`luci-app-dawn` 失修。被 usteer 取代。
  现场常见现象是"只有自己一台在列表里"，即 peer 发现根本没成功。

### 3.3 只做 11k 邻居报告的那一类（最轻、最稳）

| 项目 | 传播方式 | 特点 |
|---|---|---|
| **nrsyncd** | mDNS（umdns）发布每 BSS 的 NR 数据 | 只同步邻居报告，不做任何决策；多频段/多 BSS 支持，确定性排序 |
| **dnrsd**（blocktrron） | **通过 AP 的 beacon 帧同步** | 不需要 AP 之间 IP 可达——管理网隔离/无回程也能用；ucode 编写 |
| **static-neighbor-reports** | 静态配置，逐频段手工填 | 最简单，两台 AP 的场景够用；2.4G 只能看到 2.4G 的邻居 |
| **rmm-nr-distributor** | mDNS | nrsyncd 的前身 |

这四个的共同点：**把"附近有谁"告诉客户端，剩下交给客户端自己判断**。
不掉线、不踢人、不会因为算法打架产生断连风暴。

### 3.4 OpenWISP（最接近"开源 AC"的项目）

- 定位是**配置管理 + 监控**，不是 steering 控制器。
- 漫游的做法：在模板里勾 `roaming` → 下发 `ieee80211r` / `mobility_domain` /
  `ft_psk_generate_local` / `reassociation_deadline`。**仅此而已。**
- 客户端数据靠 monitoring 模块每 **5 分钟**上报一次，落在 "WiFi Sessions" 页——
  用于观测，不作为漫游依据（5 分钟粒度也没法用于决策）。
- 验证方式官方给的是看日志 `WPA: FT authentication already completed - do not start 4-way handshake`。

### 3.5 TIP OpenWiFi / uCentral（最"工程化"的一份参考）

schema 驱动，把漫游拆成三块，值得直接借鉴它的字段划分：

```json
"services": {
  "wifi-steering": {
    "mode": "local",              // local = AP 自行决策；cloud = 上报云端
    "network": "upstream",
    "assoc-steering": true,       // 拒绝关联以实现引导
    "required-snr": -75,          // 低于此值不允许继续连着
    "required-probe-snr": -70,    // 低于此值不回 probe
    "required-roam-snr": -85,     // 低于此值触发漫游
    "load-kick-threshold": 90     // 信道利用率超过 90% 才考虑踢
  }
},
"ssids": [{
  "roaming": { "message-exchange": "air", "generate-psk": true, "domain-identifier": "EFAB" },
  "rrm":     { "neighbor-reporting": true, "ftm-responder": true, "stationary-ap": true }
}]
```

关键点：**默认就是 `mode: local`**，AP 自己决策、本地就近协同；云端只负责下发与观测。
连最"云原生"的开源项目都把实时决策放在 AP 上——这印证了我们设计 D4 的判断。

## 4. 几条公认的工程结论

1. **决策必须留在 AP。** 毫秒级判断放到控制器那一侧，等决策回来信号已经变了。
2. **同一时刻只能跑一个 steering 守护进程**（usteer 与 DAWN 互斥）。
   两个一起跑会互相把客户端往对方踢。
3. **收益排序：11k > 11v > 11r。**
   - 11k 减少扫描时间，对 5G/6G（DFS 信道要避雷达）尤其重要；
   - 11v 只对一部分客户端有效；
   - 11r 在 802.1X/EAP 场景收益最大，PSK 场景收益有限。
4. **Windows 在 PSK 网络里不用 11r。** Win10/11（至 2024H2）只在 802.1X 上支持 FT。
   现网 `SSID-A` 是 `psk2+ccmp`——Windows 客户端不会走 FT，别指望它变快。
5. **强制踢除是断连风暴的主要来源。** 社区共识已经从"踢到最优"转向
   "**suggest, never force**"：只发 BTM，不设 SNR 门槛踢人。
6. **验证要看真实报文。** 11k 看 beacon 里的 RM Enabled Capabilities
   （wireshark `wlan.rmcap.b1 == True`）；但**开了 MFP（ieee80211w）后邻居报告帧是加密的**，
   抓包必须配置解密，否则什么都看不到。

## 5. 现网实测：11k 开着，但邻居列表是空的

在 AP-A（10.0.30.251）上用 ubus 读 hostapd 当前持有的 RRM 数据：

```
phy0.1-ap0  rrm_nr_get_own → ["00:00:00:00:00:23","SSID-A_5G","000000000023ef58..." ]   ← 只有自己
phy0.1-ap0  rrm_nr_list    → { "list": [ ] }                                          ← 空
phy0.2-ap0  rrm_nr_list    → { "list": [ ] }                                          ← 空
```

也就是说：**每台 AP 只知道自己**，`rrm_nr_list` 里没有任何邻居条目。
客户端发 Neighbor Report Request 时，拿不到"另一台 AP 在哪、哪个信道、多强"。

这与 OpenWrt 官方文档的说法完全吻合（hostapd 依赖外部软件喂列表），
也和现场文章里反复出现的 "rrm_nr_list was empty on every radio" 一致。

**结论：11k 协议是开着的，但缺少填充者。** 两台 AP 之间的漫游，客户端仍要自己扫描发现，
11k 的收益没有拿到。要补的正是 §3.3 那类守护进程。

## 6. 对本项目的建议

现状盘点（已验证）：

| 能力 | 状态 |
|---|---|
| 11k 协议开关 | ✅ 全部 BSS 生效（`rrm_neighbor_report=1`） |
| 11k 邻居数据 | ❌ 现网 `rrm_nr_list` 为空。控制器侧填充已实现（`docs/ac-11k-neighbors.md`），待真机验证 |
| 11v BTM | ✅ `bss_transition=1` + `wnm_sleep_mode=1` |
| 11r FT | ✅ 5G/6G 生效（每 VLAN 一个 MD 1111–6666），2.4G 未开 |
| steering 守护进程 | ❌ `dawn` 已装但 disabled；usteer/nrsyncd 未装 |

三步走，风险从低到高：

**第一步（建议先做）：把邻居列表填上。**

> 后续补充：这一步的实现方式已经改变。查 hostapd ubus 接口时发现 `rrm_nr_set`
> 可直接写入，而控制器本来就持有全部 AP 的 BSS 身份，因此不需要在每台 AP 上装
> `nrsyncd`——由控制器聚合、随配置包下发即可，更符合"控制面集中"的前提。
> 已实现，见 `docs/ac-11k-neighbors.md`。下面保留原始建议作为背景。

它只做 11k 数据分发，不决策、不踢人，风险几乎为零，而这恰好补上现在最缺的一环。
我们的 agent 已经支持按模板 `steering: nrsyncd` 幂等地启停它。

**第二步：观察一两周。** 看客户端的漫游是否已经够用。
很多场景做到这一步就结束了——11k 把扫描时间省掉，客户端自己会走得很好。

**第三步（仅在确实不够时）：上 usteer，但只用温和参数。**
社区实践总结出的安全区间：

```
signal_diff_threshold  9~15   # 目标 AP 至少强这么多才建议
roam_scan_snr          25     # 提前开始考虑，但不踢
min_snr / min_connect_snr / roam_trigger_snr = 0   # 关键：全部关掉
load_kick_enabled      0
assoc_steering         0      # 先不开，开了会影响首次关联
```

**不要考虑的：**
- DAWN：已废弃，且它的价值集中在"拒绝关联/踢除"这类高风险手段上。
- 自研集中式 steering：违反设计 D4，且没有任何开源先例支持这种做法。

如果以后要把 steering 参数纳入模板，建议照抄 uCentral 的字段划分
（`mode` / `required-snr` / `required-roam-snr` / `load-kick-threshold` 分开），
而不是塞一个笼统的 `steering: on`。

## 7. 参考

- OpenWrt Wiki, *WiFi Roaming* — <https://openwrt.org/docs/guide-user/network/wifi/roaming>
- OpenWrt Wiki, *Setting up usteer and band-steering* — <https://openwrt.org/docs/guide-user/network/wifi/usteer>
- usteer 源码与 ubus 接口 — <https://github.com/openwrt/usteer>
- DAWN — <https://github.com/berlin-open-wireless-lab/DAWN>
- nrsyncd — <https://github.com/Fail-Safe/nrsyncd>
- dnrsd（beacon 帧同步 NR）— <https://github.com/blocktrron/dnrsd>
- rmm-nr-distributor — <https://github.com/simonyiszk/openwrt-rrm-nr-distributor>
- TIP OpenWiFi, *Roaming RRM and SON* — <https://openwifi.tip.build/device-feature-configuration-examples/device-feature-configuration-examples/roaming-rrm-and-son>
- OpenWISP, *WiFi Access Point / Roaming* — <https://www.openwisp.org/docs/dev/tutorials/wifi-access-point.html>
- 实践总结：*wifi-dethrash*（usteer 参数与踩坑）— <https://sindro.me/posts/2026-04-03-wifi-dethrash-openwrt-mesh-analyzer>
