# AP 侧 agent（ac-agent）

> 状态：已实现并在 Linux 容器内做过端到端运行（假 `ubus`/`wifi` 提供系统接口）。
> 尚未在真机 XR1710G 上运行。

---

## 1. 它做什么

```
每 20 s 一轮：
  ┌─ 心跳/注册/上报 ──────────────────────────────────────────┐
  │ POST /api/v1/ap/hello                                    │
  │   → {ac_id, state, desired_rev, package_path, pubkey}    │
  └──────────────────────────────────────────────────────────┘
        │ desired_rev ≠ 本地 applied_rev ？
        ▼
  下载配置包 → 用钉住的公钥验签 → 校验 MAC/型号/rev
        → 备份 → 段级合并写入 → 单次 reload
        → 停用 dnsmasq/odhcpd → ubus 健康检查
        → 失败则回滚并上报 rollback / 成功上报 applied
```

一轮只有一个出站请求，注册、上报、取期望版本合并成一次调用，天然幂等。

## 2. 安全模型

| 项 | 做法 |
|---|---|
| 控制器身份 | 首次 hello 时**钉住**公钥与 `ac_id`，之后任何变化都拒绝执行并报错（TOFU） |
| 配置包完整性 | Ed25519 验签 + 逐文件 sha256；manifest 未列出的文件一律拒绝 |
| 目标校验 | `manifest.device_mac` 必须等于本机 MAC；`model_whitelist` 必须包含本机型号 |
| 身份认证 | `AC_SHARED_SECRET` 作为 Bearer token 发给控制器；控制器未配置时为空转 |
| 密钥持久化 | 公钥钉在 `/etc/ac/state.json`（0600） |

钉住公钥意味着**换控制器必须人工清 state**。这是刻意的：自动接受新公钥等于把中间人防护关掉。

## 3. 配置合并规则（最危险的部分）

整文件覆盖 `/etc/config/network` 会让 AP 立刻失联，所以 agent 做的是**段级合并**：

- 按「类型 + 名字」就地替换：`device` / `bridge-vlan` 整类替换（匿名段只能这样），
  `interface` 只替换控制器渲染了同名段的那些（`lan`、`lan2`…），不是只认 `ac_` 前缀；
- 其余 section 原样保留（`wwan`、`loopback`、网关全都保住）；
- 例外：本地 `interface` 若绑定在 AC 网桥已经收编的物理口上（出厂 `wan` / `wan6`
  绑裸 `wan`），合并时删除——桥口不能承载 L3，留下就是一条永远起不来的路由模式接口；
- 本地 `wifi-device` / `wifi-iface` 不删除，只加 `option disabled '1'`，便于回落胖模式；
- 写入前用 `ucimerge.Validate` 校验：没有 `ac_` 段、段名重复 → 直接拒写；
- 写入用「临时文件 + rename」，崩溃不会留下半个文件。

合并是幂等的：同样内容重复应用不会产生新文件、不会触发 reload。

## 4. DHCP 必须关掉

AP 是纯二层设备，桥接到同一个广播域。它要是开着 dnsmasq，会和站点 DHCP 抢答，把整个网段的客户端指到错误网关 —— 影响面是全站，不是单台 AP。

因此每次 apply 成功后 agent 会：

```sh
uci -q set dhcp.@dnsmasq[0].disabled=1
uci -q set dhcp.odhcpd.disabled=1
uci -q commit dhcp
/etc/init.d/dnsmasq stop && /etc/init.d/dnsmasq disable
/etc/init.d/odhcpd  stop && /etc/init.d/odhcpd  disable
```

- `uci set` 保证重启后仍然关闭；`stop/disable` 保证立刻生效。
- 任一步失败只记日志、**不影响本次 apply**：odhcpd 不存在不该导致 wifi 配置被回滚。
- 回滚时**不会**重新打开 DHCP —— AP 仍是受管模式。
- 确认需要 AP 兼做路由器时用 `--keep-dhcp`（或 `AC_KEEP_DHCP=1`）关掉这段逻辑。

## 5. reload 纪律

设计 §7.1 的硬约束在这里落地：

- 只有 `network` 真的变了才 `ubus call network reload`；
- `wifi reload` 每次 apply **只调用一次**；
- 永不 `ifup`/`ifdown` 整个 bridge —— 那会 flush PPE 表，表现为数秒吞吐塌陷。

健康检查用 `ubus call network.wireless status`，逐个核对渲染出的 `ac_` BSS 都是 `up`，超时（默认 15 s）则回滚。

## 6. 部署

```sh
# 1. 交叉编译（arm64 静态，约 6.6 MB）
make agent                      # → bin/ac-agent-linux-arm64

# 2. 推送到 AP
scp bin/ac-agent-linux-arm64 root@10.0.10.250:/usr/sbin/ac-agent
scp deploy/openwrt/ac-agent/files/ac-agent.init root@10.0.10.250:/etc/init.d/ac-agent
scp deploy/openwrt/ac-agent/files/ac.config     root@10.0.10.250:/etc/config/ac
ssh root@10.0.10.250 'chmod +x /usr/sbin/ac-agent /etc/init.d/ac-agent'

# 3. 先空跑，确认合并结果再真跑
ssh root@10.0.10.250 'ac-agent -v --root / --mac $(cat /sys/class/net/br-lan/address) once --dry-run'
ssh root@10.0.10.250 'ls -l /etc/ac/staging/rev*/'

# 4. 正式启用
ssh root@10.0.10.250 'uci set ac.agent.secret=<AC_SHARED_SECRET>; uci commit ac;
                       /etc/init.d/ac-agent enable; /etc/init.d/ac-agent start'
```

### 6.1 用 OpenWrt feed 编译（可选）

不想手工 `scp` 时，把本仓库的 `deploy/openwrt` 当成一个 feed，由 buildroot 直接编出安装包：

```sh
# OpenWrt main：feeds 脚本支持 --root，可把 feed 根指到仓库子目录
src-git --root=deploy/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git

# 24.10 及更早的 feeds 脚本没有 --root（会报 Syntax error），改用本地 checkout
src-link ac_agent /path/to/ac-controller/deploy/openwrt

./scripts/feeds update ac_agent
./scripts/feeds install ac-agent luci-app-ac-agent
make menuconfig        # Network → ac-agent；LuCI → Applications → ac-agent
```

`ac-agent` 包从同一仓库的 git 源编译，`deploy/openwrt/ac-agent/Makefile` 里的
`PKG_SOURCE_VERSION` 钉住 commit，改 agent 代码时要与 `PKG_VERSION` 一起 bump，
否则 feed 更新后仍编出旧代码。构建机需要 packages feed 提供 `golang-package.mk`，
且 buildroot 的 Go 版本要满足 `go.mod` 的 `go 1.26.0`。

buildroot 编出的二进制跟随目标 libc；需要一份跨固件（musl/glibc）通用的静态二进制时，
仍用上面的 `make agent`。两种方式装出来的路径都是 `/usr/sbin/ac-agent`，
服务与配置分别是 `/etc/init.d/ac-agent`、`/etc/config/ac`。

`/etc/config/ac` 支持：`enabled`（缺省 1，启用 agent 并在 WAN 放行 8090/29820/29821）、`ac_addr`（留空则广播自动发现）、`secret`、`interval`、`dry_run`。

查看状态：

```sh
ac-agent status
# state        managed
# ac_addr      10.0.30.5:8090 (config)
# applied_rev  1
# sha256       821e4a0a…   ← 应与控制器 GET /packages 记录的摘要一致
# pubkey       906eb4ea…   ← 钉住的控制器公钥
```

`ac-agent status --json` 输出的是页面读的同一份结构；agent 每轮心跳还会把它写进
`/var/run/ac-agent/status.json`（tmpfs，不含 secret/pubkey），供 LuCI 直接读取，
不必为展示状态引入第二个端口或额外的执行权限。字段定义见 `internal/agent/status.go`，
界面见 [`ac-luci-app.md`](ac-luci-app.md)。

## 7. 故障与恢复

| 现象 | agent 行为 | 恢复 |
|---|---|---|
| 控制器不可达 | 记日志、按间隔重试；**不做任何配置变更**（维持最后配置）；遥测、11k 列表重建、AP 间协同照常运行 | 控制器恢复后自动继续 |
| 动态发现的 AC 连续 3 次无响应 | 清掉发现到的地址，退避 **60 s** 后再广播发现；配置地址（`AC_ADDR`）永远不覆盖 | 网络恢复或新 AC 应答后自动重新绑定 |
| 401 | 立即以错误退出，不重试（token 不会自愈） | 修正 `secret` 后重启服务 |
| 包验签失败 | 拒写、上报 `rollback`、`applied_rev` 不变 | 检查控制器私钥是否被重置 |
| 公钥/`ac_id` 变化 | 停在上报，拒绝应用 | 人工确认后删除 `/etc/ac/state.json` |
| BSS 未起来 | 回滚配置、再 reload、上报 `rollback` | 看 `sync_message` 与 hostapd 日志 |
| 上报丢失 | 结果写进 `state.json`，下一轮补报 | 自动 |

`procd` 配置了无限重生（`respawn 3600 5 10`）：agent 退出会不断被拉起，因为"放弃了"比"一直重试"更糟。

## 8. 未做

- 批量重启与批量升级编排（单台升级已实现，见 §11）。
- 客户端/射频遥测的长期归档（当前由控制器按 `AC_SAMPLE_RETENTION` 裁剪）。
- `/etc/config/ac` 目前由 init 脚本读取，没有 LuCI 页面。

## 9. 客户端遥测

控制器不在数据面（设计 C1），**看不到任何客户端**，全部数据由 AP 采集后随心跳上报：

| 字段 | 来源 | 可靠性 |
|---|---|---|
| MAC | `iw dev <if> station dump` | 权威 |
| SSID / 频段 / ifname | `ubus call network.wireless status` | 权威 |
| 信号 (dBm) | station dump 的 `signal:`（取首值，不用 `signal avg`） | 权威 |
| 协商速率 | `rx bitrate` / `tx bitrate`，四舍五入到 Mbps | 权威 |
| 在线时长 | `connected time` | 权威 |
| 静默时长 | `inactive time`（毫秒，截断为秒） | 权威；>60 s 的样本不参与 11v 引导（避免追踪已漫游走的幽灵条目） |
| **IP** | `ip neigh show` | **尽力而为，通常为空** |

**关于 IP**：AP 是纯二层设备，从不与客户端做三层通信，内核邻接表里根本没有这些条目。
要准确的 IP↔MAC 对应关系，只能另找来源：站点 DHCP/DNS（现网是 10.0.10.1）、
或在 AP 上启用 DHCP snooping。这一点写在返回结构体的注释里，避免以后误以为采集有 bug。

上报语义（`ReplaceClientsForAP`）：

- AP 每轮发**全量列表**，不是增量。丢一条心跳或 agent 重启都能在下一轮自愈。
- 列表里消失的客户端标记 `online=0`，不删除 —— 保留 `first_seen` 以便看"来过多久"。
- agent 完全不上报（字段缺失）时，控制器**不动**客户端表；只有显式空数组才表示"这台 AP 现在没有客户端"。

代价：每 20 s、每 Z 个 BSS 各一次 `iw` 调用。11 个 BSS 就是 11 次进程创建，
在 XR1710G 上可忽略，但这是心跳从 30 s 改到 20 s 后要留意的一处开销。

## 10. 漫游引导

**控制器不做实时踢除。** 按设计 §D4：漫游决策是毫秒级的，放到网络另一端只会适得其反
（DAWN 已废弃，usteer 的强制踢除在多份实测里产生断连风暴）。
控制器只做两件事：

1. **下发 802.11k/v/r 参数**（渲染器负责，见 `docs/ac-p2-provisioning.md` §3）；
2. **指定 AP 本地跑哪个引导守护进程**，并通过配置包里的 `steering` 文件传达：

```
acpkg-<mac>-rev<N>.tar.gz
├── manifest.json
├── wireless
├── network
└── steering        # 一行：nrsyncd | usteer | dawn | none
```

agent 的规则：

- 只允许**一个**守护进程运行，其余 `stop` + `disable`（两个同时跑会互相抢客户端）；
- 指定的守护进程未安装 → 记 warn、**不影响本次 apply**，实际状态由 `probeSteering`
  探测后上报，控制器据此显示"期望 vs 实际"；
- 配置包里**没有** `steering` 文件 = 控制器不管这件事，一个守护进程都不碰。

当前支持 `nrsyncd`（只发布 802.11k 邻居报告，客户端自行决策）、`usteer`、`dawn`、`none`。

### 实测：11k/11v/11r 配置在现网全部生效

在 XR1710G（ImmortalWrt 20261026）上逐 BSS 核对过 hostapd 的运行时配置：

| 项 | 落地键 | 2.4G | 5G | 6G |
|---|---|---|---|---|
| 11k | `rrm_neighbor_report` / `rrm_beacon_report` | 1/1 | 1/1 ×4 | 1/1 ×6 |
| 11v | `bss_transition` / `wnm_sleep_mode` | 1 | 1 ×4 | 1 ×6 |
| 11r | `mobility_domain` + `wpa_key_mgmt` 含 `FT-*` | 无 | 有 | 有 |

### 11r FT 实机证据（2026-11-03）

| 场景 | 结果 | 证据 |
|---|---|---|
| SSID-A_5G / FT-PSK `.250`→`.251` | **通过** | `.250`：`BSS-TM-RESP ... status_code=0 target_bssid=00:00:00:00:00:23`；`.251`：`AP-STA-CONNECTED ... auth_alg=ft`，同一 MAC 无 `EAPOL-4WAY-HS-COMPLETED` / `WPA: pairwise key handshake completed` |
| SSID-C_5G / FT-SAE `.250`→`.251` | 客户端未通过 | `.251` 连续 `did not acknowledge authentication response`；手机回退 `.250` 后 `auth_alg=sae` 正常接入 |
| SSID-C_6G / FT-SAE `.250`→`.251` | 客户端未通过 | 同上；6G 为纯 `SAE FT-SAE`、`ieee80211w=2`，排除 5G 过渡模式因素 |
| Windows + FastConnect 7800 | 未发起 FT | SSID-A_5G / SSID-C_5G 两种 AKM 均为 `auth_alg=open/sae` + 完整 4-way |

两台 AP 的 5G/6G `mobility_domain`、`wpa_key_mgmt` 一致，5G/6G 使用相同信道和 30 dBm；
FT-SAE 失败不是 AP 配置或覆盖差异。SSID-A_5G 的 `ft_psk_generate_local=1` 只影响 PSK
的 PMK-R1 本地生成，与 FT-SAE 无关。结论：FT-PSK 已实机通过，FT-SAE 需换 Linux
`wpa_supplicant` 或确认支持 FT-SAE 的手机补测；不能把“FT-* 已下发”当成“FT 已实机通过”。

模板里的 `rrm_neighbor_report` 字段与 LuCI 的模型一致（LuCI 也把 Neighbour Report /
Beacon Report 作为 11k 的两个独立开关），但**在现网固件上是冗余的**：`ieee80211k '1'`
已经默认带出这两个键。保留它只是为了显式控制。

验证方法与一次误判的教训见 `docs/ac-p0-baseline.md` §8。

> **关键补充（2026-10-31 实测）**：协议开着不等于数据存在。
> `ubus call hostapd.<bss> rrm_nr_list` 在现网是**空的**——hostapd 自己不生成跨 AP 的邻居列表，
> 必须由外部守护进程（nrsyncd / dnrsd / usteer）喂进去。所以现在的 11k 只有"能力"没有"内容"，
> 客户端仍要自己扫描。见 `docs/ac-roaming-survey.md` §5。

### 10.1 控制器离线时的 AP 间协同（UDP 29821）

11k 列表由控制器从各 AP 上报的 BSS 自报聚合后下发（`docs/ac-11k-neighbors.md`）。
但控制器可能不在：新 AP 在维护窗口加入、AC 重启、交换机把管理网切断。这时漫游数据不能跟着消失，
所以 agent 之间还有一条不经过控制器的通道：

```sh
# 每台 AP 每 30 s 广播一次（管理网二层广播）
AP_PEER {type, v, mac, model, hostname, bss[], clients[], radios[]}
```

- 端口 `29821`（比发现端口 29820 大 1，避免同一 socket 冲突），本地 UDP socket，**不额外起进程、不写盘**；
- 收到的 peer 状态按 AP MAC 归一化去重，超过 `max(3×interval, 90s)` 不再播报即过期；
- `restoreNeighborsIfDue`（首轮 + 每 15 轮）写入 hostapd 的列表 =
  **控制器缓存 ∪ 本机自报的兄弟 BSS ∪ 在线 peer 的同 SSID BSS**，按 BSSID 去重排序，
  始终排除本 BSS 自身（hostapd 的 `rrm_nr_set` 会自己补上）；
- 跨 SSID 的条目一律丢弃：把客户端引到它连不上的 BSS 只会污染整张列表；
- 单条消息上限 48 KB，客户端列表过大时先丢弃 `clients` 再发——BSS 自报是 peer 必需的部分。

配置（`/etc/config/ac`，均为可选）：

| 键 | 默认 | 说明 |
|---|---|---|
| `peer_port` | `29821` | `0`/负值关闭 AP 间协同 |
| `peer_interval` | `30s` | 播报间隔（`AC_PEER_INTERVAL`） |
| `peer_steer` | `0` | 是否允许本地 11v 建议 |

**离线自治的边界**：AC 不可达时 agent 只做"保持现状 + 重建运行态"，绝不改写 `/etc/config`。
配置变更仍然只能来自验签通过的包，这是安全模型的一部分。

### 10.2 可选的本地 11v 建议（默认关闭）

`peer_steer=1` 时才启用，且故意保守——AP **测不到关联在其他 AP 的客户端**，任何"对方信号更好"的
判断都是猜的。因此只在**本机链路已经弱到快断**（≤ -78 dBm、已连接 ≥ 30 s）时，向客户端发一条
BTM 建议（`ubus call hostapd.<if> bss_transition_request`）：

- 参数名必须是 `disassociation_imminent`（hostapd WNM 接口的真实键名；写成
  `disassoc_imminent` 虽不报错，但会被忽略）；
- `neighbors` 必须是**邻居报告 hex 数组**，即 `rrm_nr_get_own` / `rrm_nr_list` 三元组的
  第 3 项（如 `06000000005cef58200081240e0603033200`），**不是 BSSID 字符串**。传 BSSID
  会被 hostapd 以 `Invalid argument`（ubus exit 254）拒绝；2026-11-03 在 `10.0.30.251` 实测确认；
- 每轮最多 1 次、每客户端冷却 2 分钟、候选最多 4 个；
- 只建议，不 `del_client`、不强制断连；
- `probeSteering` 发现 usteer/dawn 已启用时自动让位——两个决策者会互相打架；`nrsyncd` 只分发 11k 邻居表，不参与切换决策，可与本地 BTM 并行；
- dry-run 下完全不发。

默认关闭是刻意的：已有站点的漫游行为不应因为 agent 升级而改变。

### 10.3 漫游引导优先级策略（steering_mode）

UCI `ac.agent.steering_mode`（环境变量 `AC_STEERING_MODE`），决定"谁来决定本机跑哪个引导守护"：

| 模式 | 行为 |
|---|---|
| `ac_priority`（缺省） | **AC 优先**。控制器在线时严格按其 steering 指令执行（`applySteering` 启用指定守护并停掉其余全部，dawn/usteer 天然没有机会与 AC 争抢）；连续失联约 1 分钟后启动 `steering_fallback` 指定的自主引导；控制器恢复应答后自动交回。 |
| `ac_only` | 只听控制器的。失联时保持最后一次指令的状态，不回退（升级前行为）。 |
| `autonomous` | agent 完全不碰引导守护（apply 跳过 steering 文件），usteer/dawn 由本地自行管理；hello 仍如实上报实测到的守护。 |

`ac_priority` 的失联回退细节（`internal/agent/steering.go`）：

- 判据 = 连续 3 个心跳失败（与重新发现控制器同一阈值），**或**（无控制器地址 + `applied_rev > 0` + 已过 3 轮）——即"曾被纳管的设备重启后控制器一直不在"。
- **从未被纳管的设备不会自作主张启动守护**（`applied_rev = 0` 不触发）。
- 回退只做**一次**：`state.steering_fallback_active` 落盘，重启不会把决定翻回去；恢复分支同样只做一次。
- 交回时按 `state.steering`（最近一次包里 steering 文件的内容）重新 `applySteering`；若为空（升级前采纳、从未记录）则只停掉回退守护，不猜"AC 想要什么"。
- `steering_fallback`（`AC_STEERING_FALLBACK`）取值 `none / usteer / dawn / nrsyncd`，缺省 `none`（失联后不启用自主引导，等价 ac_only）。

状态出口：`/var/run/ac-agent/status.json` 新增 `steering_mode` / `steering_fallback` /
`steering`（实测当前启用的守护）/ `steering_fallback_active`；LuCI 页面（控制 → AC 纳管）
有对应的两个选项（引导模式、失联回退守护）与状态行/告警。

## 11. 射频遥测与固件升级

### 11.1 射频采样

每 **3 轮心跳**（默认 20 s → 每分钟）采一次，与控制器 `AC_SAMPLE_INTERVAL` 一致；
更快的采样只会让 AP 多做几次 ubus 调用，数据照样被控制器丢弃。

来源只有两处：

```sh
ubus call network.wireless status          # radio 列表、band、htmode、txpower、interfaces
ubus call hostapd.<第一个 BSS> get_status   # 实际信道、freq、phy、airtime.utilization
```

**为什么不信 `/etc/config/wireless`**：`option channel 'auto'` 在 UCI 里永远是 `auto`，
ACS 换信道不会改它。控制器若读配置，曲线会是一条直线，看不出任何干扰或换信道事件。
同一 radio 上的所有 BSS 共享信道，所以取第一个 BSS 询问即可。

| 字段 | 取值 | 说明 |
|---|---|---|
| `radio` / `band` / `up` | netifd | `up = radio.up && !disabled` |
| `htmode` / `width_mhz` | UCI | 宽度由 htmode 末尾数字解析（`EHT160` → 160，`HE20` → 20） |
| `txpower` | UCI | 配置值，不是运行时实测 |
| `channel` / `freq_mhz` / `phy` | hostapd | BSS 未建立时退回配置里的数字信道，freq 为 0 |
| `utilization` | hostapd airtime | **-1 = 未知**，0 = 真的空闲 |
| `clients` | 本轮客户端列表 | 按 `ifname` 归属到 radio |

`-1` 与 `0` 必须分开：老 hostapd 没有 `airtime` 对象，把"未知"画成 0 % 会让
"干扰很低"变成一条看似有依据的结论。曲线里遇到 -1 应断开而不是连成 0。

上报语义：`radios` 字段无 `omitempty`。**nil = 这台 agent 不采集或读不到**，
控制器不动遥测表；**空数组 = 确实没有任何射频**。

### 11.2 固件升级

控制器**只声明目标，不推送**：`POST /api/v1/devices/<mac>/firmware` 写库，
下一次 Hello 回复带 `firmware{model,version,path,sha256,size}`，离线 AP 回连时自然取到。

下发条件（`api/firmware.go` 的 `firmwareRef`）：设备已采纳 + 有目标版本 +
最近一次回报不是 `applied`/`failed`。**失败后不再重发**——一张起不来的镜像
如果每 20 秒重试一次，就是重启风暴。

agent 侧顺序：

```
0  model 不符                → failed（不下载）
1  运行固件已包含目标版本      → applied（sysupgrade 不保留 /etc/ac，靠这条收敛）
2  dry-run                   → 只记日志，不下载、不执行（坑 H7）
3  先落 state=downloading 并立即 hello 上报
4  流式下载并校验 size + SHA-256 → 失败即 failed
5  落到 /tmp/ac-firmware.img   → 落 state=upgrading 并立即 hello 上报
6  sh -c "sleep 2; sysupgrade -q <img> >>/tmp/ac-sysupgrade.log 2>&1 &"
7  任一步失败                → state=failed 并立即上报
```

第 3 步先于下载上报是有意的：一张几十 MB 的镜像在无线链路上要下好几分钟，
没有这条上报，控制器只会看到旧固件，操作员会以为指令丢了。下载使用独立的
20 分钟超时（共享 HTTP 客户端的 30 秒只够一次 Hello 或几十 KB 的配置包）。

下载**直接流式写入 `ac-firmware.img.part` 并同步计算 SHA-256**，不把整镜像读进内存：
镜像大小与 AP 的 hostapd 共享内存，原来"缓冲整包 + 再写一份"的峰值是现在的两倍。
写盘后核对 `size`（限制读取 `size+1` 字节，多一个就判定超长）与摘要，全部通过才
`rename` 成正式文件；任何一步失败都删掉 `.part`，`sysupgrade` 永远不会看到一个坏镜像。

第 5 步先上报同样是有意的：重启前后可能没有下一次心跳，控制器需要先看到 `upgrading`。
第 6 步必须分离执行——`sysupgrade` 阻塞会把心跳卡住整场刷写，AP 在重启前就先"死"了。

**重启后如何收敛**：每轮开头 `reconcileFirmware()`（`agent.go` 的 `Step` 第一行）。

- 运行固件字符串（`DISTRIB_DESCRIPTION`）包含目标版本 → `applied`；
- 仍是 `upgrading` 且超过 **10 分钟**没有变化 → `failed`（`no reboot into … within 10m`）。

状态写在 `/etc/ac/state.json`，但 `sysupgrade` 只保留 `/etc/config` 与 keep 列表里的路径，
`/etc/ac` 通常会被清掉。所以**最终判据是"现在跑的固件"，不是本地状态文件**：
比对用大小写不敏感的包含匹配，因为仓库里的 version 是 build 号，而设备上报的是整串描述。

镜像要求：新固件**必须自带 `/usr/sbin/ac-agent`**（或把它放进 `/lib/upgrade/keep.d`），
否则升级后 AP 失去 agent，只能等到 10 分钟超时被判 `failed`，且再也无法回报。

运维操作：

```sh
# 上传：body 就是镜像，model/version 走 query
curl -X POST --data-binary @xr1710g-20261026.img \
  "http://127.0.0.1:8080/api/v1/firmwares?model=XR1710G&version=20261026" \
  -H "Authorization: Bearer $AC_ADMIN_TOKEN"

# 设目标（清空传 {"version":""}）
curl -X POST "http://127.0.0.1:8080/api/v1/devices/<mac>/firmware" \
  -H "Authorization: Bearer $AC_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -d '{"version":"20261026"}'
```

重复上传同一 `(model, version)` 返回 409；下载接口同时接受管理员令牌与 AP 共享密钥，
响应带 `X-Ac-Sha256` 头，agent 用它和本地计算结果比对。

测试覆盖：`internal/agent/firmware_test.go`（停链、已运行该版本、dry-run、重启后
applied、超时 failed），`internal/agent/download_test.go`（流式落盘、摘要不符、截断、
超长、HTTP 错误），`internal/api/firmware_test.go`（仓库 CRUD、双鉴权、目标下发与停止、
pending 设备不下发），`internal/store/firmware_test.go`。AP 间协同与离线自治见
`internal/agent/coordinate_test.go`（peer 校验/过期、列表合并、AC 不可达仍重建 11k、
本地 11v 的开关/让位/冷却/强信号保护）。

### 11.3 控制器下发动作

控制器**不主动连接 AP**：`POST /api/v1/devices/<mac>/actions` 把命令写入
`device_actions`，下一次 Hello 回复的 `actions[]` 带下来，agent 执行后在下一次
Hello 的 `actions[]` 回报结果。离线 AP 回连时自然取到积压命令。

命令是**封闭集合**（`proto.ActionReboot` / `proto.ActionWifiReload` /
`proto.ActionFactoryReset` / `proto.ActionKick` / `proto.ActionDiagnose`），
控制器 API 和 agent 两侧都校验；参数是 `map[string]string`：前三个不需要参数，
`kick_client` 需要 `mac` 与 `ifname`（MAC 归一化为小写，ifname 只允许
`[A-Za-z0-9._-]+`，防止参数变成新的命令行）；`diagnose` 需要 `kind`
（`network` / `system` / `kernel`），agent 只按这个封闭集合选择固定命令，
不接受自由 shell。下发条件：设备已采纳（pending 设备返回 409），每次最多 8 条。

agent 侧顺序（`internal/agent/actions.go`）：

```
1  空 ID / 已在 executed_actions 里 → 跳过
2  先 rememberAction 并 saveState，再执行（防 reboot 后重放）
3  dry-run / 无 Runner          → failed，不执行
4  未知类型                      → failed
5  wifi_reload                   → wifi reload
6  reboot                        → sh -c "sleep 2; reboot &"（分离执行）
7  factory_reset                 → 还原最早备份 + reload + 分离重启（先回报）
8  kick_client                   → ubus call hostapd.<ifname> deauthenticate {"addr":"<mac>"}
9  diagnose                      → 固定命令：ip -br addr + ip route / logread -l 80 /
                                  dmesg（Go 侧截取最后 80 行）；输出截断到 4000 字节
10 结果追加到 state.action_results；reboot/factory_reset 成功时先立刻 hello 再结束本轮
```

幂等键是**执行前落盘的 action ID**（`executed_actions`，环形保留 64 条），
不是控制器状态：结果丢失、hello 重试或 AP 重启后，同一条 reboot 也不会执行第二次。
`action_results` 只在一次 hello 成功后才清空，否则断网期间的结果会在恢复时丢失。

控制器侧：`PendingActions` 在读取时顺带把「已下发超过 10 分钟」或「入队超过
24 小时」的 pending 标为 `expired`，队列不会无限增长；`CompleteAction` 只更新
pending 行，重复结果（hello 重试）不会把已完成的动作改回去。

测试覆盖：`internal/store/actions_test.go`（生命周期、超时过期）、
`internal/api/actions_test.go`（下发/送达/回报/去重、非法类型/未采纳/非法 MAC）、
`internal/agent/actions_test.go`（只执行一次、重启后不重放、reboot 结束本轮、
dry-run 与未知类型）。

### 11.4 运行时长与恢复出厂

Hello 额外带 `uptime`（`/proc/uptime` 的秒数，读取失败为 0）。控制器只在
`uptime > 0` 时更新设备行，一次读取失败不会把已有时长清空；UI 对在线设备
再叠加最后一次心跳的年龄，显示"现在"的连续运行时间。

`factory_reset` **不执行 `firstboot`**：那会清空 overlay，agent 自己也会消失，
远程 AP 再也回不来。实际做法（`internal/agent/actions.go` 的 `factoryReset`）：

```
1  在控制器备份目录里找时间戳最早的一份（pre-adoption 快照）
2  按 backup.txt 逐项还原；标记 missing 的文件删除
3  ubus call network reload + wifi reload
4  清空 /etc/ac/state.json 的纳管状态，但保留 executed_actions 与 action_results
5  结果回报后分离重启；控制器收到 done 才把设备 Release 回 pending
```

保留执行 ID 是防重放的关键：state 清空后 AP 以 pending 身份回来，若执行记录
一起被清，同一条 factory_reset 会在下一轮 Hello 再执行一次，把刚恢复的设备
再刷一遍。

### 11.5 SSID 高级项与模板生命周期

- `maxsta` 渲染为 UCI `option maxassoc`；为 0 时不输出，现网基线保持字节不变。
- 业务隔离在 hostapd 中没有跨 BSS 开关，控制器不做假选项，只强制该 SSID 独占
  一个 `network`（两条启用的 SSID 不能同时声明隔离并共用网络）。
- 定时计划只写入模板（`SSID.Schedule`）；AP 侧 `wifi_schedule` 已禁用，agent
  不会执行它，UI 也明确标注“规划”。
- 模板支持克隆（新 ID 为 `<id>-copy-<8hex>`）；删除前检查设备与 AP 分组引用，
  被引用或仅剩最后一个模板时返回 409。

### 11.6 客户端管理（踢下线 / 追踪 / MAC ACL / 智能磁吸）

**追踪与备注**：`client_meta` 表按 MAC 保存 `alias`、`note`、`tracked` 以及磁吸
目标 `bind_ssid`/`bind_ap_mac`。它与 `clients` 快照分表存放，因为 AP 每轮 Hello
都会覆盖快照字段，控制器侧的意图不能写进快照列。管理接口：

```
PUT /api/v1/clients/<mac>         部分更新（指针字段，未传的保持原值）
POST /api/v1/clients/<mac>/kick   踢下线，返回 202
```

`GET /api/v1/clients` 用 `LEFT JOIN client_meta` 合并意图字段。清空 `bind_ssid`
时同时清空 `bind_ap_mac`——AP 偏好离开 SSID 没有意义。

**踢下线**：`kick` 复用动作通道（`kick_client`），只对在线、且 AP 与接口名完整的
客户端放行；离线返回 409、未知 MAC 返回 404、AP 未采纳也返回 409。agent 收到后
执行 `ubus call hostapd.<ifname> deauthenticate`，不等待结果。

**MAC ACL**：模板 SSID 新增 `macfilter`（`allow`/`deny`）与 `maclist`，渲染为
UCI `option macfilter` + 多条 `list maclist`。校验规则：有模式必须至少一条 MAC；
无模式不得带列表；MAC 为六段十六进制，大小写不敏感去重。未配置时完全不出现在
渲染结果里，现网基线保持字节不变。

**智能磁吸**：控制器在 Hello 回复里下发 `bindings[]`（`mac`/`ssid`/可选
`ap_mac`/解析后的 `bssid`），字段**没有 omitempty**：空数组表示“清空”，字段
缺失才表示旧控制器、保留旧意图。AP 侧 `SteerLoop` 的处理原则是**只建议、不强踢**：

- 已在目标 SSID（且指定 AP 时在该 AP）→ 视为满足，清除弱信号计数与冷却；
- 在错误 SSID → 从全部接口/本机自报/peer 中聚合目标 SSID 的 BSS，发
  `disassoc_imminent=false` 的 BTM；冷却 5 分钟，刚接入 30 秒内不动；
- 没有满足条件的目标 BSS → 只记 debug 日志，不踢（否则只是断开客户端）；
- 绑定客户端不参与弱信号强制踢除，避免两套策略互相打架；
- 绑定集合变化时重置弱信号/冷却/待踢状态，并随 `state.json` 持久化
  （`client_bindings`），AP 重启后仍生效。

测试覆盖：`internal/api/clients_control_test.go`（部分更新、列表合并、Hello
Bindings/BSSID 解析、清空语义、踢下线 202/404/409、动作参数校验）、
`internal/agent/actions_test.go`（`kick_client` 命令与非法参数）、
`internal/agent/steerloop_test.go`（绑定满足时抑制弱信号、错误 SSID 定向 BTM、
冷却、指定 AP、无候选不动）。

