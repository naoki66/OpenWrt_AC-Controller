# 开发指南

面向要继续改这个仓库的人。分工：

| 想了解 | 看哪份 |
|---|---|
| 为什么这么设计（控制面/数据面、CAPWAP 取舍、漫游 D4） | `ac-ap-controller-design.md` |
| 怎么用（部署、运维、API） | `README.md`、`ac-deployment.md` |
| 现网实际长什么样、与设计文档的偏差 | `ac-p0-baseline.md`、`baseline/` |
| **怎么改代码、哪里有坑** | **本文** |
| 某一次事故的复盘 | `ac-251-recovery.md`、`ac-boot-wireless.md` |

本文第 9 节「坑点大全」是**改代码前必读**，每一条都是真机踩出来的，不是推测。

---

## 1. 环境与命令

### 1.1 构建

```sh
go build -o bin/ac ./cmd/ac                 # 控制器（本机）

# AP 侧 agent：静态二进制，一台 arm64 OpenWrt 通吃（与 libc 无关）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -o bin/ac-agent-linux-arm64 ./cmd/acagent
```

有 `make` 时等价的 target：`make build` / `make agent` / `make agent-local` / `make test` /
`make vet` / `make fmt` / `make smoke` / `make up` / `make up-dev` / `make down` / `make docker`。
**Windows 本机没有 make**，直接用上面的 `go` 命令即可（见坑 G1）。

### 1.2 测试

```sh
go test ./...                        # 全量
go test ./internal/render/ -run Baseline -v   # 渲染 vs 现网基线（改模板必跑）
go test ./internal/agent/ -count=1            # agent（含合并、回滚、11k、自愈）
go vet ./... && gofmt -l .
```

测试的风格是「桩要能区分成功与丢」：`neighborRunner` 真的保存每个 BSS 的
`rrm_nr_list`，而不是只数调用次数——否则测不出"写进了一个马上要被替换的 hostapd 实例"。

### 1.3 没有真机时

```sh
python scripts/fake-ap.py --target 127.0.0.1           # 模拟 AP 广播，验证发现链路
python scripts/fake-ap.py --interval 10                # 持续广播
ac-agent once --dry-run -v --ac 127.0.0.1:8080         # 只渲染 + 暂存，不写 /etc/config
```

`--dry-run` 的产物在 `/etc/ac/staging/rev<N>/`（`--root` 可改根，本机调试用）。
**首次接真机必须先 dry-run 看合并结果**，再 `enable && start`。
注意 dry-run 是**零副作用**的：它连 hostapd 的 11k 列表都不会写，更不会 `wifi up`（坑 H7）。
不要让生产 agent 常驻在 `--dry-run`：模板改了也不下发，控制器却仍显示 `applied`。

### 1.4 真机

```sh
# 1. 停（否则 /usr/sbin/ac-agent 被占用，scp 报 dest open Failure）
ssh root@<ap> '/etc/init.d/ac-agent stop'
# 2. 传
scp bin/ac-agent-linux-arm64 root@<ap>:/usr/sbin/ac-agent
# 3. 起
ssh root@<ap> 'chmod +x /usr/sbin/ac-agent; /etc/init.d/ac-agent start'

# 新 AP 接入的第一条命令（否则开机可能起不来无线，见坑 D4）
ssh root@<ap> '/etc/init.d/wifi_schedule disable'
```

agent 参数：`-v`（debug 日志）、`--ac host:port`、`--secret`、`--root`、`--dry-run`、
`--interval`、`--mac`、`--model`、`--keep-dhcp`、`--peer-port`、`--peer-steer`、
`--peer-secret`、`--peer-network`。
生产由 `/etc/init.d/ac-agent` 从 `/etc/config/ac` 读取（`uci get`，不用 `config_load`，见坑 G4）。

---

## 2. 代码地图

```
cmd/ac                控制器入口：serve / version / pubkey / verify
cmd/acagent           AP 侧入口：run / once / status
internal/
  proto/              AC ↔ AP 共享线协议（hello / reply / 客户端 / 邻居上报）
  model/              模板数据模型 + 校验（template.go）
  render/             模板 → UCI 文本。产物：wireless / network / steering / neighbors
  bundle/             打包 + Ed25519 签名/验签（manifest 去签名字段再签）
  keys/               控制器签名身份，首次启动生成并持久化
  provision/          采纳 → 渲染 → 校验 → 签名 → 存库（service.go）
  ucimerge/           UCI 段级合并
  store/              SQLite（设备/模板/包/客户端/邻居）
  discovery/          UDP 29820 广播与应答
  api/                HTTP 路由 + 内嵌 Web UI（go:embed web/）
  agent/              AP 侧：发现、心跳、拉包、验签、合并、apply、回滚
    agent.go          循环、Step()、发现/重新发现、开机恢复（restoreNeighborsIfDue）
    apply.go          Apply()：暂存 → 备份 → 写 → reload → 健康检查 → 回滚
    neighbors.go      11k 列表写入、settle、缓存、无线自愈（EnsureWireless）
    peer.go           AP 间 UDP 广播（29821）：收集/过期/快照，不依赖控制器
    coordinate.go     合并控制器缓存 + 本机自报 + peer；可选的本地 11v 建议
    download.go       固件流式下载（.part + 增量 SHA-256，不整包进内存）
    clients.go        `iw station dump` 采集
    netcheck.go       地址/设备存在性检查
```

### 两条主链路

**控制器侧**（`provision.Service`）：
`Adopt/Repush` → 取模板 → 套设备级覆盖（`device.network_params`）→ 校验 →
`render` → `bundle.Sign` → `store` 存包（rev 不可变）→ API 提供 `package` 下载。

**AP 侧**（`agent.Step`）：
`buildHello`（采集客户端/邻居/射频，并向 peer 播报、可选 11v 建议）→
`restoreNeighborsIfDue`（首轮/每 15 轮，合并控制器缓存 + 本机自报 + peer）→
发现或复用已发现的 AC → `hello` → 有更高 rev 则拉包 → 验签 → 合并 → `Apply` → 上报结果。

发现/采集/peer 协同都在 `hello` **之前**完成，所以 AC 不可达的轮次里它们照常运行；
这也解释了为什么 `Step` 的顺序不能反过来（否则 peer 合并拿不到本机最新的 11k 自报）。

**关键点：AP 侧只在「rev 变了」时才会动无线。** 无线状态自己变坏（重启、ACS、
hostapd 丢列表）不会触发任何 apply —— 这是坑 D2/D4 的根源，也是为什么自愈逻辑
必须挂在周期循环里而不是挂在 apply 里。

---

## 3. 改一个无线字段要动哪些地方

以"给 SSID 加一个选项"为例：

1. `internal/model/template.go` 加字段（三态语义用 `*bool`，见坑 H4）
2. `internal/render/render.go` 输出该行
3. `docs/baseline/ap251/template.json` 黄金模板同步（否则 parity 测试失败）
4. `go test ./internal/render/ -run Baseline`
5. 真机：`repush` → agent apply → 用**运行时 hostapd conf** 验证（不是 UCI，见坑 D6）

第 5 步最容易错：UCI 写了 ≠ hostapd 生效。

---

## 4. 渲染器与黄金模板

- 产物为 `wireless` / `network` / `steering` / `neighbors` 四个文件（`render.FileWireless` 等常量）。
- 渲染输出**零时间戳、零注释、全排序**：这是 P1 逐字节比对和 rev 可复现的前提。
- `render.Options{Prefix}`：生产传 `ac_`；与现网基线比对时传 `""`，否则每段都报"被改名"。
- 黄金模板来自真机 dump：`scripts/uci-to-template.py`（`--redact-psk` 必带）。
- `parity_test.go` 的比对口径：按段身份匹配、比较**选项集合**（非字节），
  唯一归一化是 `option ipaddr` ≡ `list ipaddr`；比对器自带反向测试
  `TestComparatorDetectsDifferences`，改比对器要先确认它还能失败。

---

## 5. 配置包、签名、rev

```
acpkg-<mac>-rev<N>.tar.gz
├── manifest.json     rev / device_mac / ac_id / template_id / model_whitelist
│                     files{name→sha256} / signature
├── wireless / network / steering / neighbors
```

- 签名覆盖「去掉 `signature` 后的 manifest」，manifest 再用 sha256 覆盖每个文件 →
  **只要公钥**就能校验全部内容（`GET /api/v1/ac/pubkey`）。
- rev 单调递增、永不复用；包按 rev 不可变留存（可回溯、可回滚）。控制器数据库从旧备份恢复时，设备行可能带着更低的 `config_rev`；agent 以 `desired_rev ≠ applied_rev` 为准，会按该 rev 降级，而不是把"已更新"误判成无事可做。
- TOFU：首次 hello 钉住 `pubkey + ac_id`，变化即拒绝。换控制器必须人工删
  `/etc/ac/state.json`。

---

## 6. 一次 apply 的顺序（每一步都有原因）

```
1 暂存到 /etc/ac/staging/rev<N>/        （dry-run 就停在这里）
2 备份现有配置到 /etc/ac/backup/rev<N>/
3 写文件（先 wireless，再 network）
4 reload：
   - 仅 network 变更才 `ubus call network reload`
   - `waitForNetdevs`：等 br-lan.<vid> 真的出现再往下（坑 C3）
   - `wifi reload`  整个 apply 只这一次
5 健康检查：
   - radio 层的 up（不是接口，见坑 C2）
   - network 变更时 `waitForAddrs`：管理地址不能丢（坑 B1 的护栏）
   失败 → 回滚（恢复备份 + 再 reload）
6 停 dnsmasq/odhcpd（`--keep-dhcp` 可豁免，仅当 AP 故意当路由用）
7 写 11k 列表 → settle 窗口补写 → 无线整体缺失时自愈
```

**永不 down/up 整个 bridge**：会 flush PPE 表，表现为几秒吞吐塌方。

---

## 7. 运行态的三件事

### 11k 邻居列表（`docs/ac-11k-neighbors.md`）

控制器聚合各 AP 上报的自报条目（`rrm_nr_get_own`）→ 生成 `neighbors` 文件 →
AP 用 `rrm_nr_set` 写进 hostapd。 AP 根本不需要装 `nrsyncd`。

- 列表**只在 hostapd 内存**，reload/重启即清空 → 必须重设 + 周期补。
- `rrm_nr_set` 是**替换**语义，且**会自动加入本 BSS** → 生成列表时必须排除自身。
- report body 是 hostapd 的不透明编码（op class / 信道 / PHY），**只搬运不解释**。
- 顺序约束：新采纳 AP 的第一个包必然没有 neighbors（AC 还不知道它的接口与 SSID）
  → 「先全部采纳，再逐个 repush」。

### 客户端

全部由 AP 采集（`iw station dump`）随 hello 全量上报。AC 不在数据面，**看不到任何客户端**。
纯二层 AP 拿不到客户端 IP（邻接表为空），IP 只能尽力而为；准确来源是站点 DHCP 或 snooping。

### steering

AC 只下发 k/v/r 参数 + 指定 AP 本地的引导守护进程，**不做实时踢除**（设计 D4）。

---

## 8. 真机验收清单

```sh
# 1) 无线是否真的在服务 —— 看 BSS 对象，不看进程、不看 netifd 的 up
ssh root@<ap> "ubus list | grep -c '^hostapd\.'"        # 应为 11
ssh root@<ap> "ip -o link show | grep -c phy0"

# 2) 11k 每个 BSS 的条目数（ubus 是 pretty-print，必须按行计数，见坑 F3）
ssh root@<ap> 'for o in $(ubus list | grep "^hostapd\."); do
  printf "%s=%s " "${o#hostapd.}" "$(ubus call $o rrm_nr_list | grep -c "\"[0-9a-f][0-9a-f]:")"
done; echo'

# 3) 控制器视角
curl -s --noproxy "*" http://10.0.30.5:8090/api/v1/devices | python -m json.tool
curl -s --noproxy "*" http://10.0.30.5:8090/api/v1/clients  | python -m json.tool

# 4) k/v/r 是否真的进了运行时配置（diff 整个 conf，不要按同名键 grep）
ssh root@<ap> 'grep -E "rrm_|bss_transition|mobility_domain|ft_" /var/run/hostapd-phy0.1.conf'
```

改模板后的下发：`POST /api/v1/devices/<mac>/repush`（无去重，每次都 bump rev）。

---

## 9. 坑点大全

### A. 渲染与 UCI

**A1. 现网是 DSA `bridge-vlan`，不是 802.1q 子接口。**
设计文档 D2 选的"软件 `br-lan.<vid>` 子接口"是错的，照它渲染会**双重打标**。
现网是 `vlan_filtering=1` + 每口 PVID + `config bridge-vlan`（VLAN 1–6、3169）。

**A2. htmode 是 `EHT160`（5G）/ `EHT320`（6G）**，Wi-Fi 7。只支持 `HT*`/`HE*` 会渲染出错。

**A3. 三个 radio 共用同一 PCIe path**，靠 `option radio '0/1/2'` 区分，不是 `option phy`。

**A4. UCI 段名在同一文件内全局唯一（不分 type）。**
`bridge-vlan` 用 `ac_bvlan<vid>`，`interface` 用 `ac_vlan<vid>`，不得重名。

**A5. radio 的 `index` 必须唯一**，旧模板（无 index）升级后会被校验拒绝，需重推模板。

**A6. VLAN↔interface 命名是 `lan`…`lan6`**（设备 `br-lan.1`…`br-lan.6`），不是 `ac_vlan<vid>`。

---

### B. 段名、前缀与防火墙（本项目最大的一次事故）

**B1. netifd 接口名绝不能加 `ac_` 前缀**（`config interface 'lan3'` 保持原样，
`wifi-iface` 的 `option network` 同理）。`device` / `bridge-vlan` 可以带前缀。

- 症状：改完 apply 成功、心跳正常，但 AP 的 SSH/HTTP/ICMP **全部不可达**。
- 原因：`/etc/config/firewall` 的 zone 按名字引用接口
  （`network 'lan lan2 ... lan6'`），而 `defaults.input=REJECT`。改名的接口掉出 zone
  即被拒；但 **ARP 仍应答、出站仍正常**，所以心跳照常 —— 看起来完全像死机。
- 判据：`arping` 通而 IP 不通 = netfilter 在拒，不是链路断了。
- 证据：2026-10-31 在 10.0.30.251 实测，commit `05dc1e1`。
  恢复过程与带外救法见 `docs/ac-251-recovery.md`。

---

### C. 合并、回滚、健康检查

**C1. 合并器按「类型 + 名字」就地替换本地段**，不只认 `ac_` 前缀。
`ReplaceTypes` 只用于匿名段（`device` / `bridge-vlan`）—— 用它一刀切替换
`interface` 会把控制器没见过的本地上行口一起删掉（测试 `local uplink lost` 会拦住你）。

**C2. 健康检查要读 radio 层的 `up`**，接口层没有这个字段。按接口读会把所有 BSS
判 down 并触发回滚。

**C3. `ubus call network reload` 返回 ≠ VLAN 设备已就绪。**
必须 `waitForNetdevs` 等 `br-lan.<vid>` 真的出现，否则 hostapd 把 BSS 挂到还不存在的
设备上，接口起来就是 down 且永不恢复 —— 真机上一次 takeover 就是 10/11 BSS 挂掉并回滚。

**C4. 地址保持检查会拦住"我自己临时加的地址"。**
apply 因 `address(es) lost after network reload` 回滚时，先确认那是不是你为了防失联
临时挂的地址。**这道闸是对的**，摘掉临时地址即可过；退避也生效（`retry_in=1m`）。

**C5. 桥口上的本地 interface 必须删掉，光保留会留下路由模式残骸。**
出厂镜像的 `wan` / `wan6` 绑在裸 `wan` 口；AC 渲染的 br-lan 把 `wan` 收进桥后，
这两个接口不可能再承载 L3。`ucimerge.Options.DropInterfacesOnBridgePorts` 按 bridge
device 的 `list ports` 精确匹配删除（`wan.2`、`br-lan.N` 不受影响，也不按类型一刀切）。
证据：2026-11-03 在 10.0.10.250 首次采纳后 `uci show network.wan`、`network.wan6` 仍在。

---

### D. 无线运行态（11k / ACS / 开机）

**D1. 11k 列表只在 hostapd 内存**：`wifi reload`、重启、radio 重启都会清空。
必须 apply 后重设（**在 reload 之后**）+ 每 15 轮比对 `rrm_nr_list` 补回，
否则功能"看起来装了、其实什么都没做"。

**D2. ACS 未完成时 radio 已经报 up。**
实测 2.4G 的 `phy0.0-ap0` 比 6G 晚 9 秒才 `AP-ENABLED`；写在它上面的列表会被丢。
修法：`settleNeighbors`，apply 后开 60s 窗口、2s 一轮回读 `rrm_nr_list` 补写。
只有**首轮**（cycles==1）走这个窗口；周期刷新保持单次廉价通过 ——
永远起不来的 BSS 不许拖住心跳循环。

**D3. 「没有」和「还没」必须分开判。**
`pendingNeighbors` 里 absent 的记为 `xxx(absent)`。我一开始把 11k 缺失当成"ACS 慢"，
真机跑出来是 BSS 压根不存在 —— 两种失败长得很像，处理方式完全不同。

**D4. 开机后无线可能整体不恢复（AC 接管后最大的坑）。**
- 症状：SSH 通、`br-lan` + 全部 VLAN 接口 UP、心跳正常、netifd 报 radio `up=true`、
  hostapd 进程在，但 `ubus list | grep '^hostapd\.'` **为空**、`ip link` 无 `phy0.*`
  —— 管理面/数据面/控制面全正常，只有射频是死的，控制器完全看不出来。
- boot 期间每个 BSS 都到达过 `AP-ENABLED`，随后**全部被 teardown 且不再回来**
  （3 次重启中 2 次复现）。
- 抢救：`wifi reload` 在该状态**无效**；`wifi down && sleep 3 && wifi up` 才有效（约 15s）。
- 自愈：`EnsureWireless` —— **仅当期望的 BSS 一个都不存在**才 `wifi up`，
  且**在 settle 窗口耗尽之后**（boot 时 agent START=95 早于 hostapd 建完 BSS，
  提前 `wifi up` 会亲手打断 bring-up）。节流 5 分钟。
- 验收：判定无线未运行 → 17 秒后 11/11，29 个客户端回流。
- 详见 `docs/ac-boot-wireless.md`。

**D5. 每台新接入 AP 先 `/etc/init.d/wifi_schedule disable`。**
设备自带的定时开关 WiFi 在开机末尾跑 `wifi_schedule.sh startup` → 遍历**所有**
`wifi-device` 段（含 AC 渲染的 `radio0..radio2`）改写 `disabled` → `uci commit` →
无条件 `/sbin/wifi`，正好落在 netifd 建 BSS 的窗口里。禁用后仍复现过一次，
所以它是干扰源、不是唯一根因。

**D6. 不要用「同名键」判断 UCI 选项是否生效。**
`ieee80211k` 在 hostapd conf 里根本不存在（派生为 `rrm_*`），我因此误判过"11k 未生效"。
另外 `option ieee80211v` 在现代 hostapd 已被 `bss_transition` 取代，是空操作。
正确做法：查 LuCI 映射，或 **diff 整个运行时 conf**。

**D7. 2.4G 没有 11r**（现网只有 k/v），5G/6G 才有 `mobility_domain` + `FT-*`。

**D8. 已漫游走的客户端会在原 AP 的 `station dump` 里留到 inactivity 超时。**
2026-11-03 在 10.0.30.251 观察到：客户端已漫游到 `.250`，`.251` 的条目 `inactive time`
持续增长但仍带旧 signal，本地建议每 2 分钟对它发一次 BTM（打到幽灵上）。修法：
`ClientStat.InactiveSeconds` 解析 `inactive time`，11v 引导（ad-hoc 与 `SteerLoop`）
跳过 `≥ 60 s` 的样本；遥测仍照实上报。

**D9. 11r 要分 FT-PSK / FT-SAE 验收，客户端可能只支持前者。**
2026-11-03 实测同一台 Android 手机：SSID-A_5G（WPA2-PSK + FT-PSK）经 BTM 从 `.250`
漫游到 `.251`，`.250` 回 `BSS-TM-RESP status_code=0 target_bssid=00:00:00:00:00:23`，
`.251` 记录 `AP-STA-CONNECTED ... auth_alg=ft`，且该 MAC 没有
`EAPOL-4WAY-HS-COMPLETED` / `WPA: pairwise key handshake completed`，FT-PSK 实握手成立。
同一手机在 SSID-C_5G（5G 过渡模式）和 SSID-C_6G（纯 `SAE FT-SAE`、`ieee80211w=2`）上，
`.251` 连续报 `did not acknowledge authentication response`；手机回退 `.250` 后仍能正常
`auth_alg=sae` 接入。两台 AP 的 `wpa_key_mgmt=SAE FT-SAE`、`mobility_domain` 与信道/功率
一致，所以不是 AP 配置或覆盖差异，判定为客户端不支持 FT-SAE。另：Windows +
Qualcomm FastConnect 7800 在 SSID-A_5G / SSID-C_5G 两种 AKM 下也都只做全握手
（`auth_alg=open/sae` + 4-way），没有发起 FT。**不要把「FT-* 已下发」当成「FT 已实机通过」**；
补测 FT-SAE 需要 Linux `wpa_supplicant` 或确认支持 FT-SAE 的客户端。

---

### E. 网络与连通性判据

**E1. SSH 的报错文案不可靠。**
ICMP unreachable 会被映射成 `Connection refused`；静默丢包才是 `Connection timed out`。
这台设备有六个子网管理 IP（`.10/.20/.30/.40/.50/.60`），逐个实测前别信文案。

**E2. netfilter REJECT 的判据**：抓包看到 `ICMP <ip> protocol 1 port N unreachable`
= type 3 / code 3。ARP 绕过 netfilter，所以 `arping` 通不代表 IP 通。

**E3. Windows 的 ping 有假象**：收到的 ICMP 错误报文会被算成"已接收"。

**E4. 出厂 `/etc/config/dropbear` 可能有 `option DirectInterface 'lan'`。**
恢复 network 后 `lan` 变成 `br-lan.1`，于是 22 refused 但 80 通。
判据：**先测 80** —— 通 = 服务/绑定问题，不通 = 防火墙问题。

---

### F. 真机操作与测量

**F1. 墙上时钟在开机时会被向前拨**（sysfixtime + ntpd 步进，实测 +12 / +22 分钟）。
日志时间戳会自相矛盾。**用内核时间戳 `[ 42.171245]` 校正**，否则时间线完全看不懂。

**F2. `logread` 是内存 ring buffer，操作会把它冲掉。**
做任何恢复动作之前先 `ssh root@<ap> 'logread' > boot.log` 存本地。

**F3. ubus 输出是 pretty-print**（多行带缩进）。
按 `["` 计数会得到 0；MAC 独占一行，按行 grep 才对。

**F4. `/tmp` 是 tmpfs，重启即丢。** 采样脚本要重新传，别指望它还在。

**F5. dump 到手先脱敏再落盘**：`uci export wireless` 含明文 PSK。

**F6. 不要为了证伪一个猜测去重启生产 AP 第二次。**
先把自愈做出来，再用**一次**重启验收。

**F7. 真机操作前先问：这一下会不会断网？**
`wifi down/up` 会踢掉全部客户端（这台 29 个）；需要用户同意。

---

### G. 构建与本机环境

**G1. Windows 本机没有 `make`**（Git Bash 里 `make: command not found`）。直接用 `go` 命令。

**G2. Git Bash 会把 `-e VAR=/path` 改写成 `C:/...`** → `MSYS_NO_PATHCONV=1`。

**G3. 本机 curl 访问 localhost 需要 `--noproxy '*'`**（机器上有个 `127.0.0.1:8048` 代理）。

**G4. `GOPROXY`：`proxy.golang.org` 在本网被干扰**，容器里默认 `goproxy.cn,direct`，
境外构建用 `--build-arg GOPROXY=https://proxy.golang.org,direct` 覆盖。
`Dockerfile` 构建器固定 `golang:1.26-alpine`，**必须与 `go.mod` 的 go 指令同步升版**。

**G5. init 脚本用 `uci -q get` 而不是 `config_load + config_get`**：后者在这台设备上
返回空值且静默失败，agent 会带着空 `AC_SECRET` 启动，每个 hello 都 401。
`procd_set_param env` 也**只能调一次**，第二次会替换而不是追加（同样静默丢 `AC_SECRET`）。

**G6. scp 覆盖 `/usr/sbin/ac-agent` 前必须 stop**，否则 `dest open Failure`。

---

### H. 代码级不变量

**H1. hello 只有一个构造入口 `buildHello()`。** 任何"即时上报"都必须复用它，
否则字段会漂移。

**H2. 上报用 `nil` vs 空数组区分「没采集」与「确实没有」。**
否则一次采集失败会把整张客户端表清空。

**H3. `RecordHello` 每轮更新 `applied_rev`（按上报值存储，不回退为 MAX）与 `steering`**；sync 字段仅在上报时覆盖。控制器数据库回滚后 AP 降级时，面板必须显示它真正在跑的 rev。

**H4. 三态字段用 `*bool`**（`ft_over_ds` / `ft_psk_generate_local` / `disassoc_low_ack` /
`ieee80211v` / beamformer）。"缺省" ≠ "false"。

**H5. 渲染输出零时间戳、零注释、全排序**；rev 单调递增永不复用。

**H6. 新能力优先做成「轮询终态」，不要猜「何时就绪」。**
`waitForNetdevs`、`settleNeighbors`、`EnsureWireless` 都是这个路子 ——
问"目标状态到了没有"，而不是"现在应该准备好了吧"。

**H7. `--dry-run` 的语义是「设备完全不被修改」，不只是「不写文件」。**
任何会改变运行态的动作都必须先查 `DryRun`：写 hostapd 的 11k 列表（改的是内存，
不是文件）、`wifi up`（**会踢掉全部客户端**）、停 dnsmasq/odhcpd。
在演练模式下触发 `wifi up` 就是一次事故。
2026-10-31 修过一次：`EnsureNeighbors` / `EnsureWireless` 缺 `DryRun` 检查 ——
加新动作时照着这两个的写法抄。

**H8. rev 是内容编号，不是单调闸门。**
agent 以 `desired_rev != applied_rev` 判断是否需要下发，`RecordHello` 也按上报值存
`applied_rev`，两者都不假设控制器只会发更大的 rev。控制器从旧备份恢复时会给出更低的
`config_rev`：2026-11-03 与 10.0.30.251 上出现 `applied_rev=9` / `config_rev=7`，
旧的 `>` 判定让该 AP 永远停在 stale 配置（LAN4–6 BSS 未启用）。改成 `!=` 后同一轮即
降级到 rev7，下一轮因 `applied_rev` 已写成 7 而收敛，不会反复下载。

---

## 10. 未实现 / 下一步

| 项 | 状态 |
|---|---|
| 管理 API 鉴权（模板 API 明文返回 PSK） | **已做**（页面使用账户登录：首次启动自动创建 `admin`/`123456`，`PUT /api/v1/auth/account` 可同时修改用户名与密码（需当前密码），改后注销其它会话，密码以 PBKDF2-HMAC-SHA256 哈希存 `settings` 表，会话为 HttpOnly Cookie；自动化仍可用 `AC_ADMIN_TOKEN` Bearer。AP 通道 `AC_SHARED_SECRET` 独立；包下载/镜像下载两者之一即可；启动检查空/短/两值相同并告警，管理响应 `Cache-Control: no-store`；**错误凭据按来源 IP 计失败次数，5 次/分钟锁定 1 分钟并返回 429+`Retry-After`**，锁定时写 `auth` 审计事件；未带凭据只返回 401，不消耗预算；正确凭据优先校验，不会被同地址的猜测拖累） |
| 时序遥测（速率/信道曲线）入库与展示（P4） | **已做**：`client_samples`/`radio_samples` 按 `AC_SAMPLE_INTERVAL` 节流、`AC_SAMPLE_RETENTION` 裁剪；遥测页含客户端信号/速率与射频信道/占用率。实时射频视图读 `radio_state`：每次 hello 按上报集合全量收敛（nil=不上报不动表，空列表=清空该 AP），改名/删除的 radio 在下一个心跳即消失；旧库升级时从 `radio_samples` 一次性回填（settings 标记防重复） |
| 固件升级 | **已做**：镜像仓库 + 固件页（上传/下载/删除/设定目标/失败重试/实时状态）+ 目标随 Hello 下发 + agent 下载前上报 `downloading`、流式下载并校验 SHA-256/大小后调度 `sysupgrade`；未做批量重启/升级编排 |
| AC→AP 动作通道（P2/P3） | **已做**：`POST/GET /api/v1/devices/<mac>/actions`（`reboot`/`wifi_reload`/`factory_reset`，202 + 审计事件）；命令随 Hello 回复下发、结果随下一次 Hello 回报；SQLite 队列带 10 分钟派发超时与 24 小时入队 TTL；agent 执行前持久化 action ID（重启不重放），reboot/factory_reset 分离执行并先回报；设备页带「重启无线/重启 AP/恢复出厂」按钮。未做批量动作与自由 shell |
| 设备运行时长（P2） | **已做**：Hello 带 `uptime`（`ProtocolVersion=4`），设备表只在 `uptime>0` 时更新；设备页展示"上次心跳 + 心跳年龄"计算的连续运行时间，离线设备只显示最后一次上报值 |
| 恢复出厂（P2） | **已做**：agent 不跑 `firstboot`，改为还原控制器备份中**最早**的一份（pre-adoption 快照）+ reload + 分离重启；清空纳管状态但保留 `executed_actions` 防重放；控制器收到 `done` 后把设备 Release 回 pending |
| 周期性重启（P2） | **已做**：`PUT /api/v1/devices/<mac>/reboot-schedule`（0–365 天 + `HH:MM`，仅已采纳设备）；控制器每分钟扫描到期设备，通过动作队列下发 reboot，离线设备跳过本次并推进下次时间；有 pending reboot 时不重复入队；设备页带设置面板与状态徽标 |
| 状态 LED 开关与定时（P3） | **已做**：`led_set` 动作（`args.state=on/off`，`ProtocolVersion=6`）由 agent 直接写 `/sys/class/leds/*status*/{trigger,brightness}`；`on` 只点亮绿色 status 灯，`off` 全部熄灭；`PUT /api/v1/devices/<mac>/led-schedule` 保存每日开/关时间（`HH:MM`，留空即关闭该边），控制器每分钟扫描到期边并通过动作队列下发，有 pending `led_set` 时跳过；设备页带立即开关按钮、定时面板与状态徽标 |
| AP 分组 → 绑定模板（P2） | **已做**：`ap_groups` 表 + `GET/POST/PUT/DELETE /api/v1/groups`、`PUT /api/v1/devices/<mac>/group`；组内有设备时拒绝删除；已采纳设备加入带模板的组立即生成下一版（`AssignTemplate`），待采纳设备采纳时默认使用组模板（显式 `template_id` 优先）；设备页带分组面板与设备行分组徽标 |
| SSID 高级项与模板生命周期（P3） | **已做**：SSID 增加 `maxsta`（渲染为 UCI `maxassoc`，0 表示不限）、业务隔离与定时计划；业务隔离没有 hostapd 开关，保存时强制独占 `network`（前后端同规则）；定时仅保存计划意图，AP 侧 `wifi_schedule` 已禁用、UI 明示不会自动执行；`GET /api/v1/templates` 返回引用计数（设备 + 分组），被引用或最后一个模板拒绝删除（409）；`POST /api/v1/templates/<id>/clone` 自动生成 `<id>-copy-xxxx`；无线页带克隆/删除按钮与引用徽标 |
| 客户端管理（P4） | **已做**：`client_meta` 单表保存别名/备注/关注/磁吸目标，`PUT /api/v1/clients/<mac>` 部分更新，`GET /api/v1/clients` 合并返回；`POST /api/v1/clients/<mac>/kick` 通过 `kick_client` 动作调用 hostapd `deauthenticate`（离线 409）；SSID 增加 MAC ACL（`macfilter` allow/deny + `maclist`，前后端同规则校验）；智能磁吸在 Hello 回复下发 `bindings[]`（`ProtocolVersion=5`），控制器解析 BSSID，AP 本地 5 分钟冷却的 BTM 建议、绝不强踢，绑定客户端不参与弱信号强踢，绑定持久化到 `state.json`；客户端页含关注过滤、详情面板与踢下线按钮 |
| AC 中断自治与 AP 间漫游 | **已做**：AP 间 UDP 29821 交换 BSS/客户端/射频，首轮与每 15 轮把「控制器缓存 ∪ 本机自报 ∪ peer」写回 11k；动态发现的 AC 连续 3 次失败后 60 s 退避重发现（配置地址不覆盖）；可选本地 11v 建议（默认关闭）；真机双 AP 联调已通过（P6） |
| PPE L2B 卸载实测（P0/R1） | 未定论：设计 §7.3 用 `cpu`(GDM1) RX 判据不成立 —— GDM1 是内部 MT7530 conduit，本该为 0 |
| Web UI 扩展：无线服务 / 射频管理 / 端口页（P3） | **已做**：无线服务页（模板 SSID 弹窗编辑 + 设备重新下发）、射频管理页（实时状态 + 模板射频编辑 + 每 AP radio 覆盖）、设备端口页（物理端口实测 + 模板展示 + 每机覆盖） |
| 仪表盘聚合与系统运维（P5） | **已完成**：原 `GET /api/v1/dashboard` 聚合页已移除（与设备页卡片重复，聚合数字并入设备页）；保留配置导入导出、控制器诊断、事件日志；AP 诊断/系统/内核日志经 `diagnose` 动作按固定命令执行 |
| 双 AP 漫游联调（P6） | **已完成（2026-11-03）**：11k 双向邻居表互指（`.250`↔`.251`，hex 报告一致）；11v BTM 修正为真实 ubus 参数（`disassociation_imminent` + 邻居报告 hex 数组），`.251` 上客户端回 `BSS-TM-RESP status_code=0` 并从 -80 dBm 迁到 `.250`（-34/-47 dBm），旧“Invalid argument”消失；11r 在 5G/6G 上配置为 `FT-PSK` / `FT-SAE`（SSID-A_5G 带 `ft_psk_generate_local=1`，mobility domain `1111`–`6666` 双 AP 一致），2.4G 按现网基线仅 k/v、无 11r；11r 实机验收（2026-11-03）：Android 手机在 SSID-A_5G（FT-PSK）经 BTM `.250`→`.251`，`.250` 回 `BSS-TM-RESP status_code=0 target_bssid=00:00:00:00:00:23`，`.251` 记录 `AP-STA-CONNECTED ... auth_alg=ft` 且无 `EAPOL-4WAY-HS-COMPLETED`/`WPA: pairwise key handshake completed`，FT-PSK 通过；同一手机在 SSID-C_5G（5G 过渡模式）与 SSID-C_6G（纯 `SAE FT-SAE`）上三次均在 `.251` 报 `did not acknowledge authentication response`，手机回退 `.250` 后可正常 SAE 接入，判定为客户端 FT-SAE 支持缺失；Windows FastConnect 7800 两种 AKM 也只做全握手。FT-SAE 待换客户端补测。`.251` 曾因 `applied_rev>config_rev` 的旧闸门停滞在 rev9（7 BSS），修复后已降级到 rev7 并恢复全部 11 个 BSS，双 AP 配置一致 |
| `.250`（AP-B） | 2026-11-03 恢复出厂后已重装 agent（`deploy-20261103b`）、用 `ap-xr1710g-full` 重新采纳为 rev7，管理 IP 与 `maint` 防失联口在位；`network.wan`/`wan6` 残留已由 ucimerge 桥口规则删除 |
