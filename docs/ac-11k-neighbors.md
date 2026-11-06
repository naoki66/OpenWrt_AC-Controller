# 11k 邻居列表：由控制器集中构建

> 状态：已实现并在**两台真机**（10.0.30.250 / .251）上验证通过，见 §7.2。
> 相关：`docs/ac-roaming-survey.md`（k/v/r 现状与开源方案对比）、`docs/ac-p0-baseline.md` §8（k/v/r 在现网全部生效的证据）。

## 1. 问题：11k 开着，但邻居列表是空的

现网（AP-A / 10.0.30.251）实测：

```
phy0.1-ap0  rrm_nr_get_own → ["00:00:00:00:00:23","SSID-A_5G","000000000023ef58..."]
phy0.1-ap0  rrm_nr_list    → { "list": [ ] }
```

`ieee80211k=1` 生效、hostapd 也支持 RRM，但 `rrm_nr_list` 在每个 BSS 上都是空的。原因是协议分工：**hostapd 只负责把别人告诉它的邻居报告放进 Neighbor Report 响应里，它自己不会去发现邻居。**

后果：客户端要漫游时仍然得自己逐个信道扫描。11k 的收益（免扫描选候选）一点没拿到，而配置文件上看起来"11k 已开"。

这不是配置错误，是缺一个填充者。

## 2. 已有的填充者，以及它们的共同点

| 方案 | 机制 | 代价 |
|---|---|---|
| `static-neighbor-reports` | 每条邻居报告写死在每个 AP 的配置里 | 加一台 AP 要手改所有 AP，还会随射频改信道而过期 |
| `nrsyncd` | 每台 AP 跑守护进程，UDP 互相交换 `rrm_nr_get_own`，用 `rrm_nr_set` 推回 | 每台 AP 多一个包、多一个进程 |
| `DAWN` | 逻辑类似，但顺带做带负载/信号阈值的漫游决策和踢人 | 同上，且决策参数难调 |

三者的共同点：**都要在每台 AP 上跑东西，由 AP 之间横向协商。**

## 3. 本方案：AC 已经知道所有 BSS，不需要 AP 互相 Gossip

这个项目的既定前提是控制面集中、数据面留在 AP。邻居数据是控制面信息，不是数据面信息——它不需要 AP 之间互相发现，因为**每台 AP 都已经把身份报给了控制器**。

于是：

```
AP 每次 hello 上报自己的 rrm_nr_get_own（每 BSS 一条）
        ↓
AC 落库 bss_neighbors（ap_mac, ifname, bssid, ssid, report）
        ↓
生成配置包时，为 AP X 的每个 BSS 挑出「同 SSID 且 BSSID 不同」的条目
        ↓
包内多一个 neighbors 文件（JSON，键=接口名，值=[[bssid,ssid,report],...]）
        ↓
AP 侧 agent 对每个接口 ubus call hostapd.<if> rrm_nr_set
```

AP 上不需要装任何额外软件包、不需要再跑一个守护进程。`nrsyncd`/`usteer`/`dawn` 的启停开关（`steering` 文件）保持独立，两条路互不干扰：即使装了 DAWN，DAWN 也只负责决策，邻居数据仍可由 AC 提供。

### 3.1 为什么让 AP 上报原始 report 而不由模板推导

`rrm_nr_get_own` 返回的第三个字段是 hostapd 自己编好的 Neighbor Report element body，里面编码了 operating class、信道号、PHY type 等。这些值取决于射频**运行时**状态（实际信道、实际带宽），模板里没有。

如果由 AC 从模板推导，就要在三种 band × 每种带宽下重新实现 operating class 的选取规则，而且一旦射频因为 DFS 换信道，推导结果立刻与事实不符。

所以：**AC 把这段二进制当成不透明字符串，只做归属和搬运。** 这也是 `nrsyncd` 的做法，好处是跨硬件、跨固件不会算错。

### 3.2 两个由 hostapd 语义决定的设计点

查 hostapd ubus 文档（`package/network/services/hostapd/README.md`）确认了两条：

1. **"An element for the node on which this command is executed will always be added."**
   `rrm_nr_set` 会自动补上本 BSS 自己的条目。所以 AC 生成的列表**必须排除自身**，否则空中会出现重复条目。已实现并有单测（`TestNeighborsExcludesSelf`）。

2. **"先清空现有邻居条目，再逐条校验写入"**
   `rrm_nr_set` 是替换而非追加，因此可以反复调用而不会累积。这是 `EnsureNeighbors` 敢每轮比对、发现不一致就重写的前提。

另外 `rrm_nr_get_own` 调用会隐式触发 `hostapd_rrm_nr_enable()`——也就是说 agent 采集这个动作本身就会激活该 BSS 的 neighbor report 能力，不依赖额外开关。

### 3.3 只通告同 SSID 的邻居

一个 BSS 只把**服务同一 SSID** 的其他 BSS 放进列表。理由：客户端在 `SSID-A` 上收到一个只能加入 `SSID-A_6G` 的邻居报告，transition 一定失败，而客户端会把整份列表当噪声，之后连有效条目也不再采信。

同 SSID 跨频段（比如 `SSID-A` 同时在 2.4G 和 5G）是正常邻居，会被通告——这正是 band steering 需要的候选。hostapd 的 report 里已经带了 operating class 和信道，客户端自己能分辨频段。

## 4. 生命周期：hostapd 只把这些存在内存里

`rrm_nr_set` 不写 UCI、不落盘。`wifi reload`、射频重启、设备重启之后，所有邻居列表都会消失，而控制器只在下发新 rev 时才推包——可能几周才一次。

如果只做"下发时设置"，功能会在重启后静默失效，且没人会发现。因此三层兜底：

1. **下发时**（`Apply` 末尾，reload 之后）：设置。放在 reload 之后是必须的，reload 会重建 hostapd 状态。
2. **本地缓存**（`<state_dir>/neighbors.json`）：收到包就落一份，用来跨重启恢复。不放进 `/etc/config`，因为它是可随时再生的派生数据。
3. **周期复assert**（`Step` 每 15 轮 ≈ 5 分钟）：读缓存，逐 BSS 比对 `rrm_nr_list`，只在 BSSID 集合不一致时才写。

第 3 层是唯一能在重启后自动恢复的路径，所以它不是优化，是正确性的一部分。

比对只比 BSSID 列，不比整段二进制：BSSID 才是"有没有这个邻居"的判据，这样也不受 hostapd 输出的空白与键序影响。

## 5. 配置文件格式

包内新增成员 `neighbors`，键是**接口名**（该 AP 上报时的 `ifname`），值是 `rrm_nr_set` 的 `list` 参数原文：

```json
{"phy0.1-ap0":[["00:00:00:00:00:24","SSID-A_5G","000000000024ef5820008124..."]]}
```

- 键控在接口名而不是 BSSID：agent 本地按 netdev 找 BSS 最直接，包内也便于人工审阅。
- 渲染是确定性的（键排序 + 条目按 BSSID 排序），同样的输入永远产出同样的字节，否则每次生成都会产生无意义的 diff。
- 该 AP 没有任何同 SSID 邻居时**不生成这个文件**，agent 就完全不碰 hostapd。第一台 AP 永远属于这种情况。

## 6. 已知限制

1. **新增 AP 后，老 AP 需要重新下发。** 邻居列表进的是配置包，而 rev 只在操作员动作时递增。给第 2 台 AP 采纳并不会自动重推第 1 台，所以第 1 台的列表里不会有第 2 台，直到对它点一次"重新下发"。这是 rev 不可变语义的直接结果，不是缺陷；但需要在操作流程里写明（先采纳全部 AP，再逐个 repush 一遍）。
2. **新采纳的 AP 第一个包必然没有邻居列表。** 生成这个包的时候，该 AP 还没跑过 agent，控制器不知道它上面有哪些接口名、哪些 SSID，没法把列表投给谁。第一次采纳只负责让它上线；它上报之后的下一个 rev 才带列表。`TestPackageCarriesPeerNeighborLists` 把这条顺序固化成了断言。
3. **跨控制器不互通。** 本方案的邻居来源是本 AC 的已采纳设备集合。两台独立 AC 之间的 AP 不会互见。
4. **pending 设备不参与。** 未采纳设备上报的 BSS 不进邻居列表，避免未受信的设备出现在别人的报告里。

## 7. 验证状态

### 7.1 本地双 AP 端到端（已通过）

用 `.e2e/` 的假 ubus/iw 跑两台模拟 AP（8c:de:52:11:22:66 / 77），假 ubus 按 `--mac` 生成各自的 BSSID，因此两台互不为"自己"：

```
# AP2 第一次采纳：包里没有 neighbors
$ tar tzf ap2-rev1.tgz
manifest.json
network
steering
wireless

# AP2 上线并上报自己的 BSS 后，重新下发 rev2
$ tar tzf ap2-rev2.tgz
manifest.json
neighbors          ← 出现了
network
steering
wireless

$ tar xzf ap2-rev2.tgz -O neighbors
{"phy0.1-ap0":[["00:11:22:33:44:55","E2E_5G","001122334455ef58200081240e0603033200"]]}
# ↑ 只有 AP1 的 BSS。AP2 自己的 8c:de:52:11:22:77 不在里面——符合
#   "hostapd 会自动补上本 BSS" 的语义。

# AP2 应用 rev2
level=INFO msg="11k neighbour list set" ifname=phy0.1-ap0 entries=1
```

单元测试另外覆盖了：同 SSID 过滤、排除自身、无同 SSID 邻居时不出文件、渲染确定性、`rrm_nr_set` 载荷格式、幂等（已正确则不重写）、目标接口不存在时跳过、缓存往返、以及"UCI 无变化也要补列表"的早退分支。

### 7.2 真机验证（已通过，2026-10-31）

两台 AP 都已由控制器接管无线配置，11 个 BSS 全量生效，且**互相**出现在对方的
邻居列表里：

```
.251 (00:00:00:00:00:21)          .250 (00:00:00:00:00:5a)
phy0.0-ap0 → 00:00:00:00:00:5b    phy0.0-ap0 → 00:00:00:00:00:22   SSID-A
phy0.1-ap0 → 00:00:00:00:00:5c    phy0.1-ap0 → 00:00:00:00:00:23   SSID-A_5G
phy0.2-ap0 → 00:00:00:00:00:5d    phy0.2-ap0 → 00:00:00:00:00:24   SSID-A_6G
... 11/11 全部填充，空列表 0 / 11
```

落地过程中修掉了三个只有上机才会暴露的问题（见 git log `fb16f31`）：

1. 健康检查把 `disabled` 的 SSID 也当成"必须起来"，18 个里 7 个禁用 → 每
   次 apply 都超时回滚；
2. 列表设在 `wifi reload` 之后仍然丢——reload 对每个 radio 是异步重建，
   返回到时后面的 radio 还没起来，写入随旧实例一起消失。改到健康检查通过后；
3. `rrm_nr_set` 返回成功不代表生效，加了回读确认与重试。

### 7.3 手工验证步骤（保留，用于排障）

以下步骤可在设备上手工执行。注意 `--write` 会改 hostapd 运行时状态（不写 UCI、
不影响关联与转发，`wifi reload` 或重启即清除）。

```bash
# 1. 采集基准（只读）
ubus call hostapd.phy0.1-ap0 rrm_nr_get_own
ubus call hostapd.phy0.1-ap0 rrm_nr_list      # 预期 list 为空

# 2. agent dry-run：确认 staging 与 /etc/config 零差异
/usr/sbin/ac-agent run --ac <ac>:8080 --dry-run --once

# 3. 单 BSS 写入自验证（可逆）
#    只把一个「存在但当前没有被引用」的条目写进去，看完立即清空。
#    注意 rrm_nr_set 会自动加上本 BSS，所以 list 传空数组 = 只剩自己。
ubus call hostapd.phy0.1-ap0 rrm_nr_set '{"list":[]}'
ubus call hostapd.phy0.1-ap0 rrm_nr_list      # 预期 []

# 4. 双 AP：采纳第二台 → 对第一台重新下发 → 再读 rrm_nr_list
#    预期第一台的列表里出现第二台对应 SSID 的条目

# 5. 客户端侧：手机在 SSID-A_5G 上漫游时抓 802.11k 行为
iw dev phy0.1-ap0 station dump | grep -i signal
#    更直接的是在客户端侧看是否发出 Neighbor Report Request，
#    或对比开/关列表时的一次漫游耗时
```

第 4 步是真正的功能判据。第 5 步的"漫游更快"很难在单次实验中量化，建议记录：接入点切换耗时、切换瞬间丢包数、是否发生 FT 重关联。

## 8. 与 steering 的关系

`neighbors`（数据）与 `steering`（决策）是正交的两件事：

- 有数据没决策：客户端扫描次数变少，仍自己决定何时换 AP。**这是当前默认状态，风险最低。**
- 有决策没数据：决策者要自己扫全信道，慢且不准。
- 两者都有：DAWN/usteer 可以基于准确的邻居信息做判断，但它们仍然不改邻居列表本身。

所以 `steering: nrsyncd` 在有了本功能之后就不再必要——nrsyncd 的唯一职责就是填充邻居列表，而这件事件已经由控制器直接完成。若要开启 `usteer`/`dawn` 做决策，仍可单独设置，二者不冲突。
