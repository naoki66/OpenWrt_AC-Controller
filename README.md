# ac-controller

面向 OpenWrt 设备的轻量无线控制器（AC），用于集中管理 **Gemtek XR1710G**（Airoha AN7581 + MediaTek MT7996 三频）接入点。

设计前提只有一条：**控制面集中，数据面留在 AP**。AC 只下发配置、收集遥测；用户流量始终在 AP 本地经 bridge / VLAN / PPE 转发，不经控制器。因此本方案不使用 CAPWAP 数据隧道 —— 那会绕过 Airoha PPE 的 L2B 卸载与 mt76 的 WLAN→NPU 绑定路径。

完整设计见 [`docs/ac-ap-controller-design.md`](docs/ac-ap-controller-design.md)。
现网实测基线与设计文档的偏差见 [`docs/ac-p0-baseline.md`](docs/ac-p0-baseline.md) ——
**改模板之前先读这一份**：现网用的是 DSA `bridge-vlan`（`vlan_filtering=1`），不是软件 802.1q 子接口。

要继续改代码，看 [`docs/development.md`](docs/development.md)：环境命令、代码地图、
以及一份全是真机踩出来的**坑点清单**（改渲染、改 agent、动无线之前先扫一遍第 9 节）。

渲染器已按现网重写，并用真机 dump 做了逐段比对：`docs/baseline/ap251/template.json` 渲染出的
3 radio + 18 BSS + 7 bridge-vlan + 6 interface **与现网配置零差异**（`internal/render/parity_test.go`）。
给另一台已上线的 AP 生成黄金模板：

```sh
ssh root@<ap> 'uci export wireless' > w.uci && ssh root@<ap> 'uci export network' > n.uci
python scripts/uci-to-template.py --wireless w.uci --network n.uci \
    --id site-a --name "现场 A" --redact-psk REDACTED-PSK --out template.json
```

## 状态

P2 已闭环（控制器 + AP agent）。当前可用能力：

- UDP 29820 广播发现，落库并展示待采纳 AP
- **采纳流程**：模板 → 渲染 → 打包 → Ed25519 签名 → 按 rev 不可变留存
- **配置包下载**：`manifest.json` + `wireless` + `network` 的 tar.gz，公钥可验签
- **AP 侧 agent**：心跳/上报/拉包/验签/段级合并/备份/reload/健康检查/回滚，强制关闭 dnsmasq/odhcpd
- **客户端视图**：MAC / IP（尽力而为）/ SSID / 频段 / 信号 / 协商速率 / 在线时长，由 AP 每 20 s 上报
- **漫游引导**：AC 下发 k/v/r 参数并指定 AP 本地的引导守护进程；不做实时踢除（设计 D4）
- Web UI（设备 / 客户端 / 遥测 / 无线网络 / 射频 / 端口 / 固件，5 秒轮询；遥测页含客户端信号与速率、射频信道与占用率曲线）
- REST + CLI（`ac pubkey`、`ac verify`；`ac-agent run/once/status`）
- SQLite 持久化（纯 Go 驱动，`CGO_ENABLED=0`，可直接交叉编译到 arm64 NAS）
- **时序遥测（P4）**：客户端信号/速率、射频信道/占用率按分钟采样入库，默认保留 7 天
- **固件升级（P4）**：镜像仓库按 `(model, version)` 保存；Web UI 可上传/下载/删除镜像并给设备设定目标
  （5 秒实时刷新状态，失败可直接重试），目标随 Hello 下发，AP 下载前上报 `downloading`、
  校验 SHA-256 后调度 `sysupgrade`，并回报 `upgrading/applied/failed`；
  失败原因（下载失败、摘要不符、超时未重启）显示在设备行上，失败后控制器不再重发

验证方式：本地 Docker 端到端跑通「发现 → 采纳 → 生成签名包 → 下载 → 验签 → 篡改被拒」，
以及 agent 侧「注册 → 拉包 → 验签 → 合并写入 → 单次 reload → 健康检查 → 上报」全链路
（Linux 容器内用假 `ubus`/`wifi` 提供系统接口）。**真机已部署（2026-10-31）**：控制器跑在 `10.0.30.5:8090`（Docker，host 网络），
agent 以 procd 服务常驻 `10.0.30.250` / `.251` 两台 XR1710G；`.251` 的 wireless 与
network 均由 AC 渲染，11k 两台互指，实测 11 个 BSS、29 个客户端在线。
部署步骤与验收清单见 [`docs/ac-deployment.md`](docs/ac-deployment.md)。

尚未实现：商品化批量重启与批量升级编排、自动信道规划。

管理 REST 与 Web UI 由 `AC_ADMIN_TOKEN` 保护（Bearer），AP 通道由 `AC_SHARED_SECRET`
保护，两者相互独立——持有共享密钥的 AP 不能读取管理面。配置包下载接口两者之一即可。
未设置 `AC_ADMIN_TOKEN` 时管理面保持开放（启动会告警，`/api/v1/health` 的
`admin_auth_required` 会如实报告），只应这样部署在受控管理网段。
Web UI 首次访问会提示输入令牌，存在 `localStorage` 并仅随同源 `/api` 请求发送。

两个凭据必须互不相同：复用会让持有共享密钥的 AP 直接读管理面。任一凭据为空或短于
16 字节时启动日志会告警；管理面响应统一带 `Cache-Control: no-store`，避免中间代理缓存 PSK。
错误凭据按来源 IP 计失败次数（5 次/分钟）并锁定 1 分钟，期间返回 `429` + `Retry-After`
并写一条 `auth` 审计事件；未带凭据只返回 401，不消耗预算。

## 快速开始

### Docker（推荐，部署在 NAS）

```sh
cp .env.example .env
# 编辑 .env，设置一个足够长的 AC_SHARED_SECRET
docker compose up -d --build
```

打开 `http://<NAS-IP>:8080/`。

> `docker-compose.yml` 使用 `network_mode: host`。AP 发现依赖接收子网广播，桥接网络收不到。Linux 宿主（NAS）上可用。

### 本地开发

```sh
make run          # 监听 :8080 与 :29820，数据写入 ./data
make build        # 产出 bin/ac
```

### 在 Docker Desktop 上本地验证（Windows / macOS）

Docker Desktop 的 host 网络把端口绑在它自己的 Linux VM 内，宿主的 `127.0.0.1:8080`
不会被监听，需要叠加开发用覆盖文件改成端口映射形态：

```sh
make up-dev       # = docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

该覆盖文件会把 `AC_ADVERTISE_IP` 固定为 `127.0.0.1`，因为映射形态下 AC 只能看到
Docker 网关地址。**生产部署不要使用它**：端口映射收不到子网广播，真机 AP 无法自动发现。

### 没有真机时验证发现链路

`scripts/fake-ap.py` 按 AP 的报文格式发送一条 `AP_DISCOVER`，并校验回包：

```sh
make smoke        # 等价于 python scripts/fake-ap.py --target 127.0.0.1

# 指定身份、或模拟多台 AP 持续广播
python scripts/fake-ap.py --mac 8c:de:52:11:22:44 --hostname ap251 --firmware 20261026
python scripts/fake-ap.py --interval 10
```

退出码 0 表示收到合法 `AC_OFFER`；控制器这边的 `GET /api/v1/devices` 应同时出现该设备，
状态为 `pending`。重复广播不会产生重复记录（按 MAC upsert）。

## 配置项

全部通过环境变量覆盖，Docker 与裸机使用同一份二进制。

| 变量 | 默认 | 说明 |
|---|---|---|
| `AC_DATA_DIR` | `./data` | SQLite 与控制器 ID 存放目录 |
| `AC_HTTP_ADDR` | `:8080` | Web UI 与 REST 监听地址 |
| `AC_DISCOVER_ADDR` | `:29820` | UDP 发现监听地址 |
| `AC_HTTP_PORT` | 由 `AC_HTTP_ADDR` 推导 | 通告给 AP 的端口 |
| `AC_SHARED_SECRET` | 空 | AP 注册密钥；为空时仅告警，不阻断（仅限可信管理网段） |
| `AC_ADMIN_TOKEN` | 空 | 管理 REST / Web UI 令牌（Bearer）；为空时管理面不鉴权，仅告警 |
| `AC_ADVERTISE_IP` | 空 | 强制通告给 AP 的地址；为空时按路由表逐请求推导 |
| `AC_OFFLINE_AFTER` | `90s` | 超过这个时长没有心跳即判为离线（三个心跳 + 余量）；`state` 只表示采纳关系，在线与否由它派生 |

端口 29820 刻意避开 TP-Link Omada 的 29810，便于与真机 Omada 共存排障。

## 发现协议

AP 广播：

```json
{"type":"AP_DISCOVER","v":1,"mac":"8c:de:52:11:22:33","model":"XR1710G","firmware":"20261026","hostname":"ap250"}
```

控制器应答：

```json
{"type":"AC_OFFER","v":1,"ac_id":"<32 hex>","ac_ip":"192.168.1.10","http_port":8080,"secret_required":true}
```

`ac_id` 为首次启动生成并持久化，用于让 AP 固定信任同一控制器。

## 采纳与配置下发

```sh
# 采纳一台已发现设备（不传 template_id 时使用内置 default 模板）
curl -X POST http://127.0.0.1:8080/api/v1/devices/8c:de:52:11:22:33/adopt \
     -H 'Content-Type: application/json' -d '{"template_id":"default"}'

# 下载签名配置包
curl -OJ http://127.0.0.1:8080/api/v1/devices/8c:de:52:11:22:33/package

# 以 AP 的视角验签
ac verify acpkg-8cde52112233-rev1.tar.gz
```

配置包结构：

```
acpkg-<mac>-rev<N>.tar.gz
├── manifest.json   rev / device_mac / ac_id / template_id / model_whitelist
│                   files{name→sha256} / signature
├── wireless        完整 /etc/config/wireless（ac_ 前缀 section）
└── network         片段，仅 ac_ 相关的 device + interface section
```

签名覆盖"去掉 `signature` 之后的 manifest"，manifest 再用 sha256 覆盖每个文件，
所以**只要公钥**就能校验全部内容：`GET /api/v1/ac/pubkey`。

完整 API 合同、渲染约定、已知安全缺口见
[`docs/ac-p2-provisioning.md`](docs/ac-p2-provisioning.md)。

## AP 侧 agent

```sh
make agent        # 交叉编译 → bin/ac-agent-linux-arm64（静态，约 6.6 MB）

scp bin/ac-agent-linux-arm64 root@10.0.10.250:/usr/sbin/ac-agent
scp deploy/openwrt/ac-agent/files/ac-agent.init root@10.0.10.250:/etc/init.d/ac-agent
scp deploy/openwrt/ac-agent/files/ac.config     root@10.0.10.250:/etc/config/ac
```

构建机也可以把 `deploy/openwrt` 当 feed 直接编包，见
[`docs/ac-agent.md`](docs/ac-agent.md) §6.1。

首次接入务必先空跑，确认合并结果再启用：

```sh
ac-agent --mac $(cat /sys/class/net/br-lan/address) once --dry-run -v
ls -l /etc/ac/staging/rev*/          # 合并后的 wireless / network
/etc/init.d/ac-agent enable && /etc/init.d/ac-agent start
```

agent 的两条硬规则：**只替换 `ac_` 前缀的 UCI 段**（本地上行口、网关原样保留），
以及 **apply 成功后强制停用 dnsmasq/odhcpd**（AP 抢 DHCP 会影响全站）。
完整行为、安全模型、故障恢复见 [`docs/ac-agent.md`](docs/ac-agent.md)。

> **新接入一台 AP 时先执行 `/etc/init.d/wifi_schedule disable`**。设备自带的定时开关
> WiFi 会在开机末尾改写所有 `wifi-device` 段（含 AC 渲染的 `ac_radio*`）并跑一次
> `/sbin/wifi`，会打断 netifd 正在进行的 BSS 建立。原因与事故复盘见
> [`docs/ac-boot-wireless.md`](docs/ac-boot-wireless.md)。

## 目录

```
cmd/ac/              控制器入口（serve / version / pubkey / verify）
cmd/acagent/         AP 侧 agent 入口（run / once / status）
internal/config/     环境变量配置
internal/proto/      AC ↔ AP 共享的线协议定义
internal/model/      模板/SSID/射频数据模型与校验
internal/render/     模板 → wireless / network 的 UCI 渲染器
internal/bundle/     配置包打包、Ed25519 签名与验签
internal/keys/       控制器签名身份（首次启动生成并持久化）
internal/provision/  采纳 → 渲染 → 签名 → 存库 的流水线
internal/ucimerge/   UCI 段级合并（不破坏本地配置）
internal/agent/      发现/心跳/拉包/应用/回滚
internal/store/      SQLite 模式与设备/模板/配置包读写
internal/discovery/  UDP 发现与应答（含集成测试）
internal/api/        HTTP 路由与内嵌 Web UI
internal/api/web/    前端静态资源（go:embed）
scripts/             联调脚本（fake-ap.py 模拟 AP 广播、uci-to-template.py 生成黄金模板）
docs/baseline/       真机只读 dump 与黄金模板（PSK 已脱敏）
deploy/openwrt/      AP 侧部署件（init 脚本、/etc/config/ac 模板）
docs/                设计文档、阶段说明、开发指南（development.md）
```

构建镜像时 `GOPROXY` 以 build arg 传入，默认 `https://goproxy.cn,direct`；
在境外构建可用 `--build-arg GOPROXY=https://proxy.golang.org,direct` 覆盖。

## 路线图

| 阶段 | 内容 |
|---|---|
| P0 | 在现网 XR1710G 上测 `wlan → br-lan → br-lan.<vid>` 是否命中 PPE L2B |
| P1 | 把现网 6 SSID × 三频 × 6 VLAN 抽象为参数化模板，渲染结果与现网配置 diff 为空 — **已完成** |
| P2 | 采纳流程 + 配置包生成与签名 + AP 侧 agent（备份/应用/健康检查/回滚）— **已完成** |
| P3 | 管理 API 鉴权 + Web UI 扩展：设备 / 无线 / 射频 / 端口 / 客户端 — **已完成** |
| P4 | 遥测入库与展示 + 固件升级 + 客户端管理（踢下线 / 追踪 / MAC ACL / 智能磁吸）— **已完成** |
| P5 | 系统运维（配置导入导出、控制器诊断、事件日志、AP 诊断/日志）— **已完成**（仪表盘页已移除，聚合数字并入设备页） |
| P6 | 双 AP 漫游联调（802.11k/v/r）：11k 双向邻居表、11v BTM（客户端 `BSS-TM-RESP` 并迁移）、11r FT-PSK 实机通过（`auth_alg=ft`）；FT-SAE 已下发，客户端侧待补测 — **已完成** |
| P7 | 11r FT 实机验收：FT-PSK 已通过；FT-SAE 在现有 Android/Windows 客户端上未发起成功，待支持 FT-SAE 的客户端补测 |

## 许可

MIT
