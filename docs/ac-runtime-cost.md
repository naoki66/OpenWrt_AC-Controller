# 运行开销：测量、优化与验证

本文记录 2026-11-04 对 agent 周期开销的实测与改动：采样层（A1–A3）、节奏与离线
自治（C1–C3、E1–E2）、无效漫游建议的抑制（第四节）、nl80211 直采（B1，第五节）、
以及把最后两项外部命令也搬进 netlink（第六节，单次采集至此从 43 ms 降到 2.8 ms）。
漫游建议的质量改进（速率闸门、候选过滤、客户端能力闸门）另有专文：
`docs/ac-steering-quality.md`。

目标不是"少花钱"本身，而是让开销随**实际负载**变化：没有客户端时不该付采样费，
控制器离线时不该把节奏拖慢，客户端明确拒绝时不该重复劝说。

## 一、为什么改：实测基线

设备 10.0.30.250（Gemtek XR1710G，AN7581 四核，ImmortalWrt 20261026，
kernel 6.18.55），11 个纳管 BSS，14 个在线 station，其中 **8 个 BSS 没有任何
station**。

单次外部调用成本（每项 50 次取平均，含 fork+exec）：

| 调用 | 单次 |
|---|---|
| `iw dev <bss> station dump` | 6.0 ms |
| `ubus call network.wireless status` | 6.4 ms |
| `ubus call hostapd.<bss> rrm_nr_list` | 4.4 ms |
| `ubus list` | 3.6 ms |
| `ip neigh show` | 5.8 ms |

进程累计：连续运行 25 小时耗 CPU 246 s / 92100 s = **0.27% 单核**
（`steer_policy_active=false`，漫游循环在 `waitIntent` 空等）。

推算到启用本地漫游策略之后：

| 场景 | 周期动作 | exec / 分钟 | 单核估算 |
|---|---|---|---|
| 无策略（旧） | 20s：1 ubus + 11 iw + 1 ip neigh | 39 | 0.47% |
| 有策略（旧） | 3s：11 iw（另加 20s 那份） | 259 | 2.6% |
| 有策略（新） | 3s hot / 10s idle：3 iw（空 BSS 已短路） | 18–60 | 0.45% |

三个结论：

1. 稳态开销本身不大，**真正的浪费在"有策略"这一档**：每 3 秒对 11 个 BSS
   全量采样，其中 8 个恒定为空。83% 的采样成本不产生任何信息。
2. 成本与 **BSS 数**成正比，与客户端数无关。11 个 SSID 的站点和 3 个 SSID
   的站点付一样的钱，这是结构性错配。
3. 离线时的问题不是 CPU 而是**节奏**：统一 30 秒 HTTP 超时会把 20 秒心跳
   拖成 30 秒以上，而 peer 广播串在同一个串行循环里。

## 二、改动清单

八项，逐条独立提交，可单独回滚。

### A1 空 BSS 短路（`stationprobe.go`）

mac80211 把关联表暴露成目录树：

```
/sys/kernel/debug/ieee80211/phy0/netdev:phy0.1-ap2/stations/<mac>/
```

读目录判断是否为空，成本比 spawn 一次 `iw` 低两个数量级。

**三态而非两态**是这一条的关键：`有` / `确知为空` / `无法判断`。只有确知为空
才跳过 `iw`；debugfs 未挂载、非 mac80211 驱动、BSS 不在表中一律回退原路径。
把"无法判断"并进"空"会让一次驱动差异变成永久为空的遥测，而症状看起来是
"这个 AP 没有客户端"而不是"采不到数据"。

实测：11 次 → 3 次（−73%），无客户端时 → 0 次。

> debugfs 的 station 目录只有 `airtime`/`aqm`/`flags` 等字段，**没有 signal、
> rate、connected time**（6.18.55 实测），所以它只能当闸门，不能替代 `iw`。

### A2 共享客户端快照（`clients.go`）

漫游循环每 3 秒、心跳每 20 秒各跑一遍同一批 `iw station dump`。改为
`Snapshot(ctx, maxAge)`：参数是"可接受的最大陈旧度"，不是缓存寿命。

- 漫游循环给 2s（低于自身周期，每轮仍是新信号）
- 心跳给 12s（覆盖循环的空闲节奏，不超过一个心跳周期）

采集失败返回 `nil` 且**不缓存**：`nil` 在协议里表示"未上报"，缓存它会把一次
ubus 抖动放大成整轮空的客户端表。采集在互斥锁内串行，两个消费者不可能同时
对同一 BSS 跑 `iw`。

### A3 批量 ubus（`neighbors.go`）

`hasInterface` 原来对每个 BSS 问一次 `ubus list hostapd.<if>`，一个 5 分钟
周期 22 次 exec。`ubus list` 的代价与返回对象数无关，一次整表查询即可回答
全部存在性问题。

**缓存只在一次扫描内有效，由各 sweep 入口显式失效，不设时间 TTL。** 最初写的
TTL 版本有两个错：`settleNeighbors` 必须看见窗口中途才出现的 BSS，活得比检查
更久的缓存恰好把这件事藏起来；用可注入时钟做 TTL 判断时，时间冻结的测试里
缓存永不过期，一次延迟调优变成一台卡住的射频。`EnsureWireless` 现在每次决策
前都重读——它触发的 `wifi up` 会断开所有客户端。

### C1 按需加速（`steerloop.go`）

固定 3 秒 → 双节奏。上一轮只要发现需要关注的客户端（弱信号、未决的强制踢除、
停在错误 SSID 上的绑定客户端、刚发出的 BTM）就走 3 秒，否则 10 秒。

检测延迟没有退化：**第一个弱样本立刻切到快节奏**，不必等到连续阈值。
`pendingKick` 单独算一条理由——那正是"要么漫游要么被强踢"的窗口，用信号强度
判断会漏掉（客户端可能刚好回升）。首轮从"等一个周期"改成立即执行。

### C2 拆分 HTTP 超时（`agent.go`）

| 阶段 | 旧 | 新 |
|---|---|---|
| 拨号 | 30s（共用总超时） | 3s |
| 响应头 | 30s | 6s |
| 总量 | 30s | 15s |

总预算保留 15 秒，因为同一个 client 还要下载配置包，几百 KB 走受限上行不算
故障。实测不可路由地址（192.0.2.1:9）**3.00 秒**返回，不再拖到 30 秒。
keep-alive 保持开启，20 秒一次的心跳复用连接。

### C3 离线退避（`hellobackoff_test.go` 覆盖）

前三次失败刻意保持原节奏：重新发现和自主接管兜底都按连续失败次数判定，把
早期失败拉长会等量推迟"判定控制器失联"——拿可用的离线模式换几个包。从第 4
次失败起 2x/3x 退避，上限 `AC_HELLO_BACKOFF_MAX`（默认 60s），恢复即归零。
上限低于心跳间隔时不会反向加速。

### E1 peer 变化即时回灌（`peer.go`）

`PeerBus` 维护一个只由 **BSS 集合**派生的版本号（客户端列表天天变，但不驱动
任何本地写入）。`Snapshot` 是唯一决定 live 集合的地方（它也负责过期清理），
所以在那里比对签名，顺序不同不算变化。

新 AP 上线后，既有 AP 从"最多等 5 分钟"变成"下一个心跳内"把它的 BSSID 写进
11k 列表；对端重启的同理，立即从列表里消失。

### E2 离线加快 peer 广播（`peer.go`）

控制器失联后广播 30s → 20s（`AC_PEER_INTERVAL_OFFLINE`）。切换点复用"判定
控制丢失"的同一阈值，只在跨越时变一次，避免抖动导致节奏来回摆。

## 三、真机验证（2026-11-04，10.0.30.250 / .251）

部署 `perf-20261104a`，两台均替换 `/usr/sbin/ac-agent` 并重启服务
（原文件备份在 `/root/ac-agent.bak.20261104`）。

| 项目 | .250 | .251 |
|---|---|---|
| state | managed | managed |
| applied_rev | 7（不变） | 7（不变） |
| 11k 条目 | 22（11 BSS × 2） | 22（11 BSS × 2） |
| station 数 | 12 | 10 |
| CPU（120s 窗口） | **0.21% 单核**（25 ticks） | — |

对比基线 0.27%，下降幅度与"hello 采集从 11 次 iw 降到 3 次"相符（A1/A2/A3
在无策略时只影响心跳那一份采集）。

E1 在日志中可见且自限：

```
msg="peer BSS set changed, reasserting 11k lists"
```

启动后出现一次（对端广播到达），之后不再重复——说明签名稳定，没有每轮抖动。

## 四、对无视建议的客户端停止重发

### 问题

现网 `02:00:00:00:00:9a` 长期停在 -81 dBm 的 2.4G BSS 上，目标 .251 同 SSID
就在旁边，信号好得多。agent 每 2 分钟发一次 11v BTM 建议，客户端**从不响应，
也从不漫游**。原来的 `steerCooldown=2min` 是纯时间闸门，它只知道"上次发是什么
时候"，不知道"上次发的结果是什么"，于是无限重试——空口帧、hostapd 状态机
开销、`logread` 刷屏全都白付。

### 判据：三种结局，而不是"发出去就算完成"

`btmguard.go` 给每个 MAC 记一条 `btmRecord{attempts, sentAt, ifname, assocAt,
silencedTo}`。建议发出时拍下**当时的接口名与关联时间**，接下来只可能发生三件事：

| 结局 | 判定 | 含义 |
|---|---|---|
| 走了 | 接口名变了 | 建议生效（或客户端自己漫游了，同样算达成目的） |
| 重连了 | `connected_time` 前移超过 2s 容差 | 生效，但可能回到同一 BSS |
| 原地不动 | 超过 `cooldown` 仍未移动 | **无效**，`attempts++` |

`assocTime(now, connectedSeconds)` 把 `iw station dump` 的 `connected_time`
换算成关联的绝对时刻——这是唯一能区分"一直连着"与"刚重连"的信号，也是为什么
不能用"上次看到它"当基准。

连续 `btmMaxUnanswered = 3` 次无效后进入 **30 分钟静默**。静默期内：
`allow()` 返回 `("silenced")`，只打一条 DEBUG。静默**不是永久放弃**——一旦
客户端移动（换了接口或重新关联），`clientMoved()` 立即解开静默并把 `attempts`
归零，因为状态已经变了，之前的证据不再适用。

### 为什么阈值是 3 和 30 分钟

3 次 = 6 分钟的连续拒绝，足够区分"客户端不支持 BTM"与"偶尔一次的失败"；30 分钟
覆盖了手机锁屏刷新、信道环境变化的典型周期，又不至于让一台换了位置的机器整天
被无视。两者都是常量而非配置项——可配置化会让现场排障多一个未知变量。

### 观测

`status.json` 新增 `steer_suppressed`（被静默的次数），与 `steer_btm_suggestions`
（累计发出）并列。**这两个数一起看才有意义**：`suggestions` 停涨而 `suppressed`
在涨 = guard 在工作；两个都不涨 = 客户端已经不粘了。

日志侧对应改动：`suggested roaming` 由 INFO 降为 DEBUG。它每 2 分钟一条且不含
可操作信息，留在 INFO 会淹没真正的事件。

### 真机结果（`perf-20261104b`，观察 7 分钟）

`.250`：

| 指标 | 值 |
|---|---|
| `cycles` | 24 |
| `steer_btm_suggestions` | **6**（两客户端各 3 次） |
| `steer_suppressed` | **7** |

日志侧交叉验证：最后一条 `suggested roaming` 是 `Wed Nov 4 19:40:32 2026
daemon.err ac-agent[784]`，新进程 `started_at 2026-11-04T11:40:51Z`，此后 7 分钟
零条。计数停在 6 = 2 × 3，正好等于阈值——说明两个客户端都被正确判定为无效并
进入了静默。

`.251` 同步部署，`steer_btm_suggestions: 0` / `steer_suppressed: 0`——该 AP
没有粘性客户端，guard 不产生任何副作用。

## 五、B1：nl80211 直采

`iw dev <bss> station dump` 的成本不在解析，而在 **fork+exec 一个动态链接的
二进制**。前面几项（A1/A2/A3）只是减少调用次数，没有改变单次成本。netlink 改变
了单次成本：一次 `NL80211_CMD_GET_STATION` dump 是两次系统调用加属性解码。

### 实现

`nl80211_parse.go`（跨平台）+ `nl80211_linux.go`（socket）+ `stationsource.go`
（源选择）。属性号是内核 ABI，硬编码在常量块里，并由测试按真实报文钉住。

源可切换，`AC_STATION_SOURCE`：`auto`（默认，netlink 失败则永久回退 iw）/
`nl80211` / `iw`。回退是**永久的而非每次**：拒绝一次 dump 的内核会拒绝接下来
十一次，每 BSS 每轮付一次失败系统调用比它想替换的进程创建更糟。

### 实测

同一进程内重复 40 次全量采集（11 个 BSS，其中 3 个有客户端，8 个被 A1 短路），
排除进程启动时间：

| 源 | 单次采集 |
|---|---|
| `iw` | 39.00 ms |
| `nl80211` | **26.00 ms** |

省 33%。剩下的是固定成本：`ubus call network.wireless status`（6.4 ms）+
`ip neigh show`（5.8 ms）≈ 12 ms，与本次改动无关——这两项在第六节解决。

稳态整轮 CPU（10 分钟窗口，30 轮心跳，142 ticks）：

```
single_core = 0.237%    per_cycle = 47.3 ms
```

与上一轮记录的 0.21% 属同一水平。**这两个数不可直接相减**：0.21% 来自 120 秒
窗口的 25 ticks，量化误差按 tick 粒度算有 ±4%；本次的 10 分钟窗口才是有意义的
口径。（第六节末尾的 0.157% 用 5 分钟窗口、47 ticks，±2%，与这里的 47.3 ms
同口径可比。）单次采集的 33% 是确定收益，整轮 CPU 的差异被噪声和负载变化淹没了。

### 一致性

逐字段对照 `iw` 与 `nl80211` 的同一时刻采样，13 台客户端：

- 客户端集合完全一致（13/13，无多无少）。
- 信号、收/发速率、连接时长、inactive 时长、接口、SSID、频段：**无系统性偏差**。
- 剩余差异全是采样时刻不同造成的正常漂移：`connected_seconds` +1、
  `signal_dbm` ±1 dBm（信号抖动）、`tx_rate_mbps` 变化（速率自适应）。

### 踩到的坑：`NLA_F_NESTED`

第一次跑出来 MAC 全部正确、其余字段全部为 0。原因不是偏移错，而是**内核给嵌套
属性打了标志位**：`NL80211_ATTR_STA_INFO` 在报文里出现为 **32789**
（`0x8000 | 21`），直接比较类型值永远匹配不上。

这类错误不会报错，只会给出一个看起来合理的零。修复是解析时统一
`& 0x3fff` 去掉标志位。定位方式是在设备上加了一个 `ac-agent nl-dump <iface>`
子命令，把原始属性树打出来——现在它作为诊断工具保留。

同一条经验适用于任何 netlink 属性解析：**先看内核实际发了什么，再相信常量表**。

### 顺带验证的结论

- **不带 `ifindex` 的 dump 不返回任何数据**（本内核），所以"一次请求拿全站"的
  想法不可行。而按 phy 分批的收益也是零：11 个 BSS 里 8 个空，A1 的 debugfs
  短路已经让实际读取只剩 3 次，与 phy 数量相同。
- `ac-agent stations` 子命令（`AC_STATIONS_REPEAT` 控制进程内重复次数）是这次
  对照与计时的工具，保留在 CLI 里。

## 六、把剩下的 12 ms 也搬到 netlink

B1 之后单次采集 26 ms 里还有 12 ms 不是站点表：`ubus call network.wireless
status`（6.25 ms）只用来拿 BSS 列表（ifname / SSID / band），`ip neigh show`
（6.0 ms）只用来拿一张常常近乎空的地址表。两者都是 fork+exec，和 `iw` 是同一
种成本。

### 实现

- **BSS 列表**改走 `NL80211_CMD_GET_INTERFACE`（`bsssource.go` + `nl80211_parse.go`
  的 `parseInterfaceMessage`）。一次 dump 给出 name / type / SSID / 频率，反正
  还需要它给的 ifindex。只保留 `NL80211_IFTYPE_AP`，频段由中心频率推出
  （`bandOfFreq`：2.4G / 5G 到 5925 MHz 为止 / 6G 到 7125 MHz）。
- **邻居表**改走 rtnetlink `RTM_GETNEIGH`（`neighsource.go` + `nlroute_parse.go`），
  请求里就把 `ndm_family` 设成 `AF_INET`，让内核替我们过滤——参考站点的 58 行
  邻居里有 56 行是 IPv6 链路本地 router 项。
- **ifindex 提示**：`StationReader.Stations` 多了一个 `ifindex` 参数。BSS 列表
  顺手把它带下来，站点读取就不用再按名字解析一次接口索引。
- 两条新路径都是"失败即永久回退到原来的命令"（`netifd` / `ip`），并且
  **netlink 的 BSS 列表为空是非 nil 空切片**：`nil` 保留给"读不到"，两者不能混。

有一处**有意的语义差异**：netlink 只保留 `IFTYPE_AP`，netifd 不做这个过滤。若某台
AP 上配了 `mode 'sta'` 的上行接口，旧路径会把 `iw station dump` 在对端 AP 上看到的那
一条当成客户端上报，新路径直接跳过它。参考站点全是 AP 模式，11/11 一致，所以这条
差异目前只是"更正确"，不是行为变更。

`AC_NETLINK=off` 一次性关掉这两条新路径，回到 ubus + ip。没有细分成两个开关：
这只是排障用的总闸，不是需要按现场调优的参数。

### 实测

同一进程内重复 100 次全量采集（11 个 BSS，8 个被 A1 短路）：

| 配置 | 单次采集 |
|---|---|
| `iw` + ubus + ip（本轮之前） | 43.17 ms |
| `nl80211` + ubus + ip（B1 之后） | 27.43 ms |
| 全部 netlink | **2.80 ms** |

从 43.17 到 2.80 是 **-93.5%**；本轮这一段是 27.43 → 2.80，**-90%**。

稳态整轮 CPU（5 分钟窗口，47 ticks / 300 s）：

```
single_core = 0.157%    per_cycle = 31.3 ms
```

上一版同口径是 0.237% / 47.3 ms，**每轮降 34%**。注意省下的 16 ms 小于单次采集
省下的 24.6 ms——因为一轮心跳里并不是每次都要真采样（`Snapshot` 12 s 的新鲜度
预算会被漫游循环的采样命中），平均下来约 0.65 次/轮。

拆开看：ubus 6.25 + ip 6.0 是直接测出来的。剩下的 12.5 ms 只能来自
**每 BSS 一次的接口名解析**（`net.InterfaceByName`，3 次约 4 ms/次）——这一档
与"全部 netlink"唯一的差别就是它。这条是减法推出来的，不是直接测的，别当精确值；
但量级与一个 fork+exec 相当，说明名字解析在 Go 里并不便宜。

### 一致性

- **BSS 列表**：11 个 AP 接口，`nl80211` 与 netifd 的 ifname / SSID / band
  逐个相同（2.4G 2427 / 5G 5180 / 6G 6135 MHz 推出 `2g/5g/6g`）。客户端集合
  14/14 一致，无多无少。
- **邻居表**：同一时刻两张表，MAC 集合 **42 = 42**，映射到不同地址的 **0 条**。
  （`ip -4 neigh show` 有 52 行，其中 1 行 `FAILED`、其余是同一 MAC 的多个 IPv4，
  折叠后 42 个 MAC。）

### 踩到的坑

**1. `NLA_STRING` 的长度包含结尾 NUL。** `NL80211_ATTR_IFNAME` 里
`"phy0.1-ap3"` 是 **len 11** 而不是 10，直接 `string(v)` 得到
`phy0.1-ap3\x00`，后续每次按名字找接口都变成"接口不存在"。`NL80211_ATTR_SSID`
不带 NUL，但 SSID 里不可能有 NUL，所以统一在第一个 NUL 处截断。
这是在设备上先跑 `ac-agent nl-ifaces` 看原始属性树才发现的——**又一次"先看内核
发了什么"**。

**2. `ndm_type` 必须过滤。** 邻居表里还有广播、组播和 `0.0.0.0` 的行，它们是真实
的表项但不是客户端。不过滤时 netlink 比 `ip` 多出恰好 4 个垃圾键
（`ff:ff:ff:ff:ff:ff`、两个 `01:00:5e:...`、`00:00:00:00:00:00`）；
只保留 `RTN_UNICAST` 后两边 42 = 42。**多出来的差异如果只盯着"多几个"就很容易
当成噪声放过**，而它掩盖的是真问题（见第 3 条）。

**3. `nlDump` 的接收缓冲区是复用的。** 第一版 `Neighbours()` 把 `nlDump` 回调里
的 `data` 切片攒起来、循环结束后再统一 fold，于是所有帧都指向同一块缓冲区，只有
最后一段数据报的内容是有效的。症状极隐蔽：表项数量从 46 掉到 37，**少掉的部分
是随机的**（取决于内核怎么分包），而且重启、重测都不复现同一组。修复是回调里
立刻深拷贝。

教训：**回调给出的切片只在回调期间有效**这件事要写进注释，否则下一个人还会踩。
现在 `nlDump` 的调用点里，凡是把 `data` 存起来的地方都拷贝。

### 采样层不再是开销主体

单次采集降到 2.8 ms 之后，剩下的外部命令全部是 `ubus`，而它们都不在采样层：

| 调用 | 单次 | 节奏 | 摊销到每轮 |
|---|---|---|---|
| `hostapd.<bss> get_status` × 3 | 4.0 ms | 每 3 轮 | ~4.0 ms |
| `network.wireless status`（radios） | 6.25 ms | 每 3 轮 | ~2.1 ms |
| `rrm_nr_get_own` × 11 | 4.0 ms | 每 15 轮 | ~2.9 ms |
| `ubus list` | 3.5 ms | 每 15 轮 | ~0.2 ms |

合计约 **9 ms/轮**，比整个采样层还贵三倍。这几处没有跟着改，原因各不相同：

- `radios.go` 要的是**配置派生**的字段（band 字符串、htmode、country、txpower、
  disabled）。netlink 能给频率、宽度、发射功率，给不了前两个，替换会改变遥测语义，
  得先把来源拆成"UCI 文件 + netlink"两半才谈得上省。
- `rrm_nr_get_own` 是 hostapd 的 11k 自述报文，**内容是不透明编码**（见
  `docs/ac-11k-neighbors.md`），本项目的立场一直是"只搬运、不重新编码"。要省掉
  它得自己实现一个 Go 的 ubus 客户端（走 `/var/run/ubus/ubus.sock` 的二进制协议），
  不属于本轮范围——下一节做了。

## 七、把 ubus 也搬到 socket

第六节结束时，剩下的外部进程全是 `ubus`：单次采集 2.8 ms 之后，它们合计仍有
**约 9 ms/轮**，比整个采样层还贵三倍。

这一轮动的不是数据源，是**传输**。B1 用 netlink 替掉了 `iw` 和 `ip`，那条路对
radios 走不通：`radios.go` 要的字段一半是配置派生的（band 字符串、htmode、
country、txpower、disabled），netlink 只能给频率和宽度。既然换不了来源，就换
「怎么问」——`/var/run/ubus/ubus.sock` 上的二进制协议，一次调用省掉一次 fork+exec。

另外两条路被否：

- **自己算 utilization**（用 survey 的 `busy/active`）：hostapd 的
  `airtime.utilization` 窗口未知。实测同一时刻 hostapd 在 65 秒内报
  54 / 51 / 48 / 49 / 46（平滑），而 survey 累计 `busy/active` = 17.5%、20 秒增量
  = 18.4%——三个数对不上。改成 60 秒滑动平均，现场干扰曲线会明显低于以前，很容易
  被当成 bug，而只省 4 ms。
- **降低 radios 刷新频率**：`radioSampleCycles=3 × 20 s` 恰好等于控制器
  `AC_SAMPLE_INTERVAL` 的默认 60 s，再降就是直接丢采样点。

### 实现

- `ubus_blob.go`（跨平台）：blob / blobmsg 编解码。常量逐条注明出自
  `ubusmsg.h` / `blob.h` / `blobmsg.h`——`UBUS_ATTR_OBJID=3` 与记忆里的 2 之
  差不会报错，只会得到空回复。
- `ubus_client_linux.go`：拨 socket、读 HELLO、按 `get_next_msg` 的语义分帧、
  LOOKUP 拿对象 id、INVOKE 调方法。超时 2 s。
- `ubus.go`：对象 id 缓存（ubusd 从不复用 id）、过期 id 重查一次、连续 3 次
  传输失败退避 60 s。
- `ubusReply()`：socket 优先，任何失败回落 CLI。两处采集器共用：
  radios 的 4 次调用（`network.wireless status` + 3× `get_status`）和 11k 的
  11 次 `rrm_nr_get_own`。`ubus list` 仍是 CLI——它是通配 LOOKUP，会回多个
  DATA 消息，不值得为 0.2 ms/轮新开一条代码路径。
- `AC_UBUS=off` 一次性关掉。
- 诊断与测量：`ac-agent ubus-dump <obj> <method>`（把回复打成 JSON，便于和
  `ubus call` 逐字段比）、`ac-agent radios` / `ac-agent neighbors` 配
  `AC_RADIOS_REPEAT` / `AC_NEIGHBORS_REPEAT`（单进程内重复，进程启动时间不会
  淹没被测量的毫秒差）。

**回复仍旧转成 JSON 再交给原有解析。** 这不是图省事，而是把"数值不变"从论证变成
构造：同一份 JSON 进同一个 `json.Unmarshal`，取值和类型没有变化的余地。若直接把
blobmsg 解进结构体，就要重新推导 netifd 和 hostapd 的每个字段类型，而
`airtime.utilization` 的口径本来就不清楚——那等于把未知量搬了个地方，问题只是换
了个位置继续存在。代价是每个回复多一次 Marshal/Unmarshal，2 KB 量级几十微秒，
对着 3.5 ms 的 fork+exec 可以忽略。

### 实测

同一进程内重复（`.250`，11 个 BSS）：

| 调用 | CLI | socket | |
|---|---|---|---|
| radios（4 次调用） | 21.8 ms | **5.7 ms** | -74% |
| 11k 自述（`ubus list` + 11× `rrm_nr_get_own`） | 54.2 ms | **9.3 ms** | -83% |

11k 那 9.3 ms 里还有约 3.5 ms 是仍走 CLI 的 `ubus list`。摊销到每轮（radios 每
3 轮、11k 每 15 轮）约 5.4 + 3.0 = **8.4 ms/轮**，与第六节末尾估的 9 ms 吻合。

整轮 CPU，`.250` 单机 900 秒窗口（45 轮，同一台同一窗口长度，只换二进制）：

| | self ticks | child ticks | total ticks | 每轮 self | 每轮 total | CPU (total) |
|---|---|---|---|---|---|---|
| `perf-20261104g`（本轮之前） | 140 | 50 | 190 | 31.11 ms | 42.22 ms | 0.211% |
| 本轮（radios + 11k 走 socket） | 126 | **14** | 140 | 28.00 ms | **31.11 ms** | **0.156%** |

self 口径 31.11 → 28.00（-10%），**子进程从 50 ticks 掉到 14（-72%）**，total
42.22 → 31.11（**-26%**）。与单次采集量出来的 8.4 ms/轮相比实测省了 11.1 ms/轮
——多出来的一截是"每 3 轮一次"的孤立测量里 ubus 二进制的页缓存已被反复预热，
而生产里每次 20 秒之后都是冷的，fork 更贵。

**口径修正**：前面几节的 `per_cycle` 只算 `utime+stime`，**不含 `cutime+cstime`**
——而 fork+exec 的成本恰恰记在子进程里，等于把这一轮要优化的东西排除在指标之外。
第一版测量（只改了 radios 的中间版本、父进程口径、300 秒窗口）因此只看到
32.67 → 30.44 ms，与单次采集省下的 8.4 ms 对不上，才发现的。本节三个数都给，
往后以 total 为准。

顺带一个观测：两台同码 AP 在 900 秒窗口下相差约 8%（`.250` 30.44 / `.251`
28.00 ms/轮，父进程口径）。所以**跨机器比较没有意义**，前后对比必须在同一台、
同长度窗口，且只换二进制。

### 一致性

真机上把 `ubus call` 的输出当基准，与 `ubus-dump` 做结构化 diff（逐字段、逐类型，
含 int/float 区分）：

| 调用 | 规模 | 差异 |
|---|---|---|
| `network.wireless status` | 6150 字节 | **0 处** |
| 11× `hostapd.<if> get_status` | 8576 字节 | **0 处** |
| 11× `hostapd.<if> rrm_nr_get_own` | 910 字节 | **0 处** |

覆盖到的类型差异点：INT8 布尔（`cac_active` / `bss_color_partial`）、空数组
（`stations: []`、`vlans: []`）、数组（`bss_colors_in_use`）、以及 11k 里那段
hostapd 不透明编码的 hex 报文——后者逐字节相同，因为这一路只搬运、不重新编码
（`TestNeighborCollectSocketMatchesCLI` 直接断言 fixture 里的十六进制）。

- 两台 AP 升级后 `state=managed`、`bss=11`、日志各出现一次
  `ubus transport active`（成功路径只报一次，用来回答"这个版本到底走了没有"）。
- 11k 邻居表在升级后仍非空：`rrm_nr_list` 三个样例都带着控制台下发的条目，
  说明"读走 socket、写仍走 CLI"没有打断回灌链路。

### 踩到的坑

**1. 消息体不是"一堆属性"。** `ubus_msg_new` 把它拿到的属性包进**一个 id=0
的容器**，真正的属性（`UBUS_ATTR_DATA` / `STATUS` / `OBJID`）是容器的子节点，
libubus 用 `blob_parse_untrusted(ub->data, ...)` 迭代 `blob_data(attr)`，正好下钻
一层。少下钻这一层不会报错：容器的子节点被当成一张"只有一个无名字段的表"，于是
每次 lookup 都返回"对象不存在"、每次调用都返回"无数据"，而线上字节完全正确。
第一版就是这样，靠 `ubus-dump` 打出 `{}` 才定位到。

**2. `blobmsg` 的 `namelen` 不含结尾 NUL。** `blobmsg_hdrlen(namelen)` 里那个
`+1` 就是留给终止符的。按 `namelen-1` 字节切片会**截掉每个字段名的最后一个字符**：
`radio0` 变成 `radio`、`config` 变成 `confi`。它不报错、解析也不失败，只是名字
对不上——`radio0` 少了尾巴正好是另一个听起来完全合理的字段名。`TestBlobmsgFieldGolden`
把 `freq` 的 16 字节手算出来钉死，`TestDecodePreservesFullFieldNames` 覆盖
n % 4 的四种对齐情况。

**3. 空数组和空表只能靠类型区分。** 第一版按"子节点有没有名字"判断类型，而零
子节点的数组和表都满足"没有名字"。netifd 恰好会报 `stations: []` 和
`vlans: []`，于是 22 个空数组全被解成 `{}`。控制器的 store 和 UI 原样透传，
这个形状差异会一路走到页面上。改成按 blobmsg 类型判断
（`blobmsgArray` → 切片、`blobmsgTable` → 映射），两者都保留空值语义。

**4. `NLA_STRING` 的教训在这里重演了一遍。** 三个坑里有两个是"源码读对了、线上
语义不同"：源码只说明格式，不说明对端实际发了什么。

### 教训

"先看对端发了什么"第三次生效（`iw` → nl80211、`ip` → rtnetlink、`ubus` CLI →
ubusd）。这次的办法更省事：把 `ubus call` 的输出当基准，和 `ubus-dump` 做逐字段、
逐类型的结构化 diff，而不是读源码推断。写测试时也要注意，**自己造的 fixture 会
跟着自己的错误假设走**——第一版 fixture 把 `namelen` 写成 `strlen+1`，与线上
不一致，测试因此全绿。

## 八、观测到但未处理的事项

1. **A1 依赖 debugfs 挂载**：若某站点未挂 debugfs，行为完全回退到旧路径，
   只是没有优化，不会出错。
2. **guard 状态不持久化**：agent 重启后 `attempts` 归零，同一个客户端会再吃
   3 次建议。这是有意的——重启往往伴随配置变更，旧证据不成立。
3. **D1/D2 的遗留**（真机样本、候选实际信号、peer 负载未参与排序）见
   `docs/ac-steering-quality.md` 第九节。
4. **剩下的 ubus CLI 都是低频路径**：`ubus list`（每 15 轮，约 0.2 ms/轮）、
   apply 路径的 `network reload` 与 `network.wireless status`、`rrm_nr_set` /
   `rrm_nr_list`（写与回读）、以及踢除和 BTM 建议
   （`del_client` / `bss_transition_request` / `deauthenticate` / `get_clients`）。
   `steerloop.go` 里的 `network.wireless status` 只是 11k 报文拿不到时的兜底，
   稳态不触发。这些没有再动，原因分两类：要么**带参数**
   （`rrm_nr_set` 的嵌套参数只验证过"从 CLI 传字符串"这一种形态，换编码要重新
   对一遍），要么一天只发生几次，省下来测不出来。

## 九、可调参数

| 环境变量 | 默认 | 含义 |
|---|---|---|
| `AC_INTERVAL` | 20s | 心跳周期 |
| `AC_HELLO_BACKOFF_MAX` | 60s | 失联后心跳退避上限 |
| `AC_PEER_INTERVAL` | 30s | 在线时 peer 广播周期 |
| `AC_PEER_INTERVAL_OFFLINE` | 20s | 失联后 peer 广播周期 |
| `AC_STATION_SOURCE` | auto | 站点表来源：`auto` / `nl80211` / `iw` |
| `AC_NETLINK` | auto | `off` 关闭 BSS 列表与邻居表的 netlink 直读，回到 ubus + ip |
| `AC_UBUS` | auto | `off` 关闭 ubusd socket 直连，回到 `ubus call` |

漫游双节奏（3s / 10s）、采样新鲜度预算（2s / 12s）、BTM 抑制阈值（3 次 / 30 分钟）、
有效速率闸门（30s 窗口 / 24 Mbps）、离线降载的 radio 采样周期（15 轮）与 11k 刷新
周期（30 轮）都是代码常量，见 `steerloop.go`、`agent.go`、`btmguard.go`、
`ratewindow.go` 的常量块。刻意不做成环境变量：这些值一旦因现场排障被改动，后续
所有观测数据就失去可比性。

## 十、控制失效期间的不必要采集（A/C）

判定阈值复用 `controlLost()`（连续 3 次心跳失败，或从未拿到地址但已经应用过配置），
不新增第二套"离线"定义，避免两个判定在现场出现分歧。

| 项 | 内容 | 在线 | 离线 |
|---|---|---|---|
| A | radio 采样周期 | 3 轮 | 15 轮（`radioSampleCyclesOffline`） |
| C | 11k 自述读取与周期回灌 | 15 轮 | 30 轮（`neighborRefreshCyclesOffline`） |

两处细节：

- **A 在状态切换时强制采样一次**（置空缓存）。只有周期的话，恢复后的第一个
  在线心跳会带着一段"离线时代"的采样，最长滞后一个离线周期（15 轮）。
- **C 不改变可观测行为**：周期回灌只为"hostapd reload 后忘了邻居表"，而离线时
  不会有新的 revision 触发 reload；peer 增减仍由 `restoreNeighborsOnPeerChange`
  在一个心跳内回灌。

### 评估后放弃：裁掉 peer 广播的负载字段

曾计划把 `PeerState.Clients`/`Radios` 从广播中移除。代码核实后放弃：`PeerBus.Snapshot()`
的四个调用点都只读 `.BSS`，负载字段确实没有消费方，但**收益只有约 1 KB/次的广播
字节和几十微秒的序列化**，远小于 A/C 省下的采样费；而这两个字段是
"peer 可见负载分布"的唯一样本，删掉等于把将来的 D1/D2 排序依据也一起删了。
`marshalPeerAnnouncement` 保留超限时先丢 `Clients` 的兜底，行为与之前一致。

在线路径的行为和节奏完全不变。离线收益取决于 BSS 数与客户端数，`perf` 口径
（`total` ticks，见第七节）应在真机同一窗口长度下对比后再记录。
