# 漫游建议的质量：什么时候不该发 BTM

本文记录 2026-11-04 对本地漫游建议（11v BTM）的三项质量改进：信号判据引入
有效速率（D1）、候选过滤（D2a）、客户端能力闸门（D2b），以及 beacon report
方案的实测否决。

前提没有变：**AC 不在数据面，AP 也不知道邻道的信号**。所以这里做的不是"测出
最好的目标"，而是"排除已知更差的目标，并且不打扰那些根本做不到的客户端"。

## 一、问题：三类"发了也没用"的 BTM

现网（10.0.30.250/.251，13 台在线客户端）的日志暴露了三件事：

1. **信号弱 ≠ 服务差。** `02:00:00:00:00:58` 长期 -78～-80 dBm，但接收速率一直
   在 18～144 Mbps 之间摆动。旧判据只看 RSSI，于是持续建议它漫游——它其实工作
   正常。
2. **候选里有同信道的 BSS。** 候选池按 SSID 收集，包含当前 BSS 的所有邻居，
   包括 operating class 与信道都相同的那一个。漫游到同信道不换干扰，只多付
   一次重连。
3. **客户端根本解析不了 BTM。** BTM 里携带的是 802.11k Neighbor Report 元素。
   现网 13 台客户端中 **11 台** 的 RM Enabled Capabilities 位图为
   `[0,0,0,0,0]` 且 MBO 为 false，包括那台被建议了几个小时的
   `02:00:00:00:00:9a`。对它们来说不是"建议被拒绝"，是"建议读不懂"。

第 3 点解释了此前 BTM 抑制（`btmguard.go`）为什么需要工作：它用"连续 3 次
无效"反推能力缺失，而能力在关联时就已经告诉过 AP。

## 二、D1：有效速率闸门

`ratewindow.go`。判据从"信号弱"变成"信号弱**且**速率掉下来了"。

**为什么取窗口最大值而不是最新值。** 无线速率由速率自适应决定，本身就在跳。
一次 6 Mbps 的采样不足以说明链路坏了，而下行大流量结束后的瞬时低速率更是正常。
所以每个客户端记录最近 `rateWindowKeep = 30s` 内的**最高**接收速率，与阈值比较：
只要窗口内曾经跑到过可用速率，链路就还有能力。误判方向是刻意选的——漏掉一次
漫游损失吞吐，错误的漫游损失关联。

**阈值与语义。** `defaultMinRateMbps = 24`，能撑 4K 视频与常规办公。policy 字段
`min_rate_mbps`：`0` 用默认，负数关闭（回到纯 RSSI），正数覆盖。

**"未知"不等于"够快"。** 驱动不报速率时 `known = false`，闸门不生效，回退到纯
信号判定。把未知当成合格会让一台真的坏掉的客户端永远被豁免。

**换接口即失效。** 客户端漫游后 `ifname` 变化，旧窗口描述的是已经不存在的链路，
必须重置，否则它会在新 BSS 上永久免检。

## 三、D2a：候选过滤

`candidates.go`。数据来源是 AP 已经持有的 802.11k 报告体——hostapd 的
`rrm_nr_get_own` 返回的 hex 去掉了 Element ID 与 Length，固定部分从偏移 0 开始：

```
BSSID (6) | BSSID Info (4) | Operating Class (1) | Channel (1) | PHY Type (1) | 可选子元素
```

解析出 operating class 与信道后做三件事：

| 规则 | 理由 |
|---|---|
| 排除当前 BSS | 不建议客户端回到它已经在的地方 |
| 排除同 class 同信道 | 不换干扰，只多一次重连 |
| 同频段优先，再按频段容量、信道排序 | 5G 客户端去 6G 好过掉到 2.4G，即使后者信道号更小 |

**解析失败一律保留。** 不同 hostapd 构建的编码可能有差异；因为读不懂就丢掉候选，
会让候选池在最需要它的时候静默缩小。同理，自身报告还没到时（`neighborInfo`
零值）不做前两条过滤，只保留原顺序。

## 四、D2b：客户端能力闸门

`btmcap.go`。在**决策点**——已经有候选、已经通过 guard、正要发 BTM——查一次
`ubus call hostapd.<bss> get_clients`，读该站点的 `rrm` 位图与 `mbo`：

- 任意一位非 0 或 `mbo = true` → 认为能读懂，照常发送。
- 全 0 且无 MBO → 不发，直接让 guard 静默，计数进 `steer_unsupported`。
- 查询失败、MAC 不在回复里 → 按"未知"处理，**照常发送**。ubus 抖动不能变成
  静默功能。

判据刻意宽松：任意 11k/11v 能力位都算"能读懂"，因为假的"不能"会静默关掉一台
本可以移动的客户端，而假的"能"只是多一次无效建议——后者已有 guard 兜底。

**成本。** 一次查询只在一个客户端真正走到发送决策时发生，结果按关联缓存
（`btmCapTTL = 30min`，关联变化即失效）。不是每轮每 BSS 一次。

## 五、beacon report：实测否决

原计划用 802.11k beacon report 让客户端测量候选 BSS 的 RSSI，从而真正"择优"。
放弃，理由是实测拿不到数据。

`rrm_beacon_req` 在设备上存在（`ubus -v list` 有完整参数表），调用也被接受，
但 hostapd 当场判定：

```
hostapd: Beacon request: 02:00:00:00:00:cc does not support active beacon report
```

`02:00:00:00:00:cc` 是全站**唯一**声明了 beacon 测量能力的客户端
（`rrm=[66,0,0,0,0]`）。也就是说，在这个客户端群上继续投入 beacon report 只会
等到永远不来的回报。候选质量改用 AP 侧已有信息（operating class/信道），
诚实但不完整：**我们仍然不知道目标 BSS 的实际信号**，只知道它不是同一个信道。

## 六、闸门必须装在两条路径上

这是本轮最容易做错的地方。

现网 `steer_policy_active = false`——包里没有 steer-policy 文件，**所有 BTM
都出自 adhoc 协调路径（`coordinate.go` 的 `maybeSteer`）**，它由 peer 广播驱动，
每轮心跳跑一次。只把闸门加在 policy 循环里，等于对现网毫无影响（实测：先只改了
`steerloop.go`，重启后 `steer_btm_suggestions` 仍在涨）。

两条路径现在共用同一份观测与缓存：`SteerLoop.rates`、`SteerLoop.caps`、以及
`Agent.btm` 的抑制状态。速率窗口与能力缓存描述的是客户端的事实，不是某条决策
路径的私有状态。

## 七、真机结果（`perf-20261104f`，10.0.30.250）

部署后观察，`.250`：

| 指标 | 部署前（`perf-20261104b`） | 部署后 |
|---|---|---|
| `steer_btm_suggestions` | 6（7 分钟内） | **0** |
| `steer_unsupported` | — | **2** |
| `steer_suppressed` | 7 | 14 |

日志侧：

```
msg="11v: client cannot parse a BTM, leaving it alone" client=02:00:00:00:00:58 signal=-78 rx_mbps=18
msg="11v: client cannot parse a BTM, leaving it alone" client=02:00:00:00:00:9a signal=-80 rx_mbps=7
```

正是此前每 2 分钟收到一条建议的那两台。注意它们的 `rx_mbps` 是 18 与 7，都低于
24——D1 的速率闸门在这两台上不生效，拦住它们的是 D2b 的能力闸门。两个闸门的
分工在真机上被区分开了。

`.251` 同步部署，`steer_btm_suggestions: 0` / `steer_unsupported: 0`（该 AP
当时的粘性客户端都不在弱信号区）。

## 八、观测口径

| 字段 | 含义 |
|---|---|
| `steer_btm_suggestions` | 累计发出的 BTM（policy + adhoc 两条路径之和） |
| `steer_suppressed` | 累计被抑制的建议（含"连续无效静默"与"能力不足静默"） |
| `steer_unsupported` | 其中因客户端不具备 11k/11v 能力而从未发送的次数 |

三个一起看：`suggestions` 停涨且 `unsupported` 在涨 = 能力闸门在工作；
`suggestions` 停涨而 `suppressed` 在涨 = 客户端在拒绝；两个都不涨 = 没有弱客户端。

## 九、遗留

1. **候选的实际信号仍然未知。** 没有 beacon report 就没有客观依据，"同频段
   优先"是启发式而非测量。
2. **peer 广播的 radio 负载尚未参与排序**。`PeerRadio` 有
   `{Band, Channel, Utilization, Clients}`，但缺少 ifname↔radio 映射，无法把某个
   候选 BSS 关联到它所在的 radio。
3. **能力判据可能过严**。理论上支持 11v 但不声明任何 11k 位的客户端会被静默；
   现场样本里没有这种设备。
4. **D1 缺少真机样本**。现网每台弱信号客户端的速率都低于 24 Mbps，闸门一次都
   没触发过；它目前只有单测覆盖。
