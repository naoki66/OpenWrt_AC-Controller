# ac-controller：OpenWrt AP 的集中配置下发与漫游协同

`ac-controller` 是一套轻量的 OpenWrt 无线控制器：AC 端是一个 Go 单二进制（自带 Web UI、REST API 和 SQLite），AP 端是一个跑在 procd 下的 `ac-agent`。它**不转发用户流量**，也不实现 CAPWAP；它只做一件事——把“这台 AP 应该长什么样”编译成带 Ed25519 签名的配置包，由 AP 主动拉取、验签、与本地 UCI 段级合并、应用、自检、失败回滚，然后把结果带回心跳。

读完本文你将掌握：

- 它解决现网里的哪些具体问题，以及刻意不做什么；
- AC 与 AP 如何发现、心跳、下发配置，为什么“控制器不主动连 AP”；
- 如何在无外网的 NAS 上用 Docker 部署 AC，以及配置、自检、升级、回滚；
- 如何在一台 XR1710G（arm64 / ImmortalWrt）上安装 agent，完成注册、采纳、首次下发和上线验证；
- 出现 401、discovering、apply 回滚、11k 列表为空、升级后双套 `wifi-device` 时，按什么顺序排查。

> 本文的命令、路径、配置项均以仓库代码和 `docs/` 内的现网记录为准。仓库内 `docs/ac-deployment.md` 记录了 2026-10-31 的一次真实部署（控制器 `10.0.30.5:8090`，两台 Gemtek XR1710G）。本次重写只做了代码与文档核对，没有再次上真机复测；推断性内容和未验证项在文中和 §9 单独标注。

---

## 1. 它解决什么问题

### 1.1 起点：AP 配置是“手工艺品”

一台 OpenWrt AP 的无线行为分散在 `/etc/config/wireless` 里：每个 radio 的信道、带宽、功率、国家码，每个 BSS 的 SSID、加密、802.11k/v/r、隐藏、客户端隔离，再叠加 `/etc/config/network` 的网桥、VLAN、管理地址。现网两台 XR1710G 的无线配置除 BSSID 外逐字节相同，却要人工保证 18 个 SSID、3 个 radio 完全一致；任何一次改密码都要登录每台机器重复一遍。

更麻烦的是“胖 AP 变瘦 AP”的接管过程：设备出厂配置里 `br-lan` 是一个**匿名 `config device`**，控制器若再生成一个 `config device 'ac_bridge'`，同一网桥就被定义两次；两台机器的 VLAN 端口映射还可能是反的（现网 `.250` 的 lan2/lan3 就是反的，且没有 3169）；管理 IP 也各不相同。整文件覆盖 `network` 会立刻把 AP 锁在门外。

### 1.2 三条硬约束

| 约束 | 代码里的应对 |
|---|---|
| 接管失败不能把 AP 弄失联 | 只做**段级合并**，不整文件覆盖；应用前备份，应用后检查网桥地址与 BSS，失败自动回滚 |
| 不能和现网 DHCP/地址冲突 | AP 应用成功后默认停用 `dnsmasq`/`odhcpd`（`--keep-dhcp` 可显式保留）；管理地址由设备级参数逐台给出 |
| AC 挂了，局域网不能跟着坏 | 配置与状态落在 AP 本地；AP 之间用 UDP 29821 继续交换 11k/客户端信息并本地建议漫游；NTP 源持久化在 AP 上 |

### 1.3 边界：它不做什么

- 不做数据面：用户流量不经过 AC，AC 也不做 AP 的网关。
- 不实现标准 CAPWAP；AC 与 agent 之间是自定义 HTTP + UDP 协议，只服务于本项目的 agent。
- 不做 PoE 控制（XR1710G 的 LAN 口本身没有 PoE 输出）。
- 不做自动信道规划/DFS 决策；模板可以下发固定信道和功率，运行时的 ACS 结果由 AP 上报。
- 不做交换机编排、不做 802.1X/ Captive Portal、不做计费。
- 不支持 Mesh 组网编排；依赖固件自身的 Mesh 能力。

---

## 2. 能力总览

| 类别 | 已实现（以代码/API 为准） |
|---|---|
| 发现与纳管 | UDP 29820 广播发现；也可手填 `ac_addr` 跨三层；设备列表、采纳、解绑、重新下发 |
| 配置模型 | 模板（无线 + 可选二层）；设备级 `network_params`（端口/VLAN/管理地址）与 `radio_overrides`（信道/功率/模式）；模板克隆、改名、SSID 局部更新、分组 |
| 下发 | 每台设备单调递增的 `rev`；Ed25519 签名 + manifest 逐文件 sha256；MAC/型号白名单校验；失败回滚；`rev` 不复用，解绑不清零 |
| 安全 | AP 侧 `AC_SHARED_SECRET`（Bearer）；管理侧登录账户或 `AC_ADMIN_TOKEN`；AP 首次心跳钉住 AC 公钥与 `ac_id`（TOFU）；审计日志 |
| 802.11k | AP 上报每 BSS 的 `rrm_nr_get_own`；AC 汇总后把兄弟 AP 的条目写进下一版配置包；agent 在 reload 与健康检查之后写入 `rrm_nr_set` |
| 802.11v | 控制器下发 steering 策略/客户端绑定；AC 失联时 agent 可在本地对弱信号客户端发 BTM 建议（默认开启） |
| 遥测 | 客户端、射频（实际信道/利用率）、有线端口、历史采样；离线判定（默认 90s） |
| 运维 | 重启、重载无线、恢复出厂、LED 计划、定时重启、固件仓库与 OTA、配置导入导出、诊断、事件日志 |
| 界面 | 内置 Web UI；AP 侧可选 LuCI 应用（`控制 → AC 纳管`） |

---

## 3. 架构

### 3.1 三个平面

```
        管理浏览器 / 自动化脚本
                │  HTTP（非 HTTPS）+ 登录会话或 Bearer
                ▼
┌──────────────────────────────────────────────┐
│ AC：单二进制，:8080（现网 :8090）              │
│  Web UI + REST /api/v1                        │
│  SQLite: data/ac.db（模板、PSK、遥测、审计）    │
│  Ed25519 签名私钥: data/ac_signing.key         │
│  UDP 29820：AP_DISCOVER → AC_OFFER             │
└───────┬──────────────────────────▲───────────┘
        │ ① 广播发现（仅无 ac_addr 时）│ ② hello（默认 20s）
        │ ③ 拉配置包（HTTP GET）        │ ④ 下一轮 hello 带 apply 结果
        ▼                              │
┌──────────────────────────────────────────────┐
│ AP：ac-agent（arm64 静态二进制，procd）        │
│  /etc/config/ac、/etc/ac/state.json            │
│  uci 段级合并 → /etc/config/wireless|network   │
│  ubus network reload / wifi reload → 健康检查  │
└───────┬──────────────────────────────────────┘
        │ UDP 29821（AP ↔ AP，AC 离线仍工作）
        ▼
   其它 AP：11k 邻居、客户端快照、本地漫游建议
```

关键点：**配置永远是 AP 拉的，AC 不主动连 AP，也不推包。** AC 只在 `hello` 应答里告诉 AP“我期望的 rev 是多少”，以及有哪些待执行动作、固件目标、NTP 源和客户端绑定。

### 3.2 端口与路径速查

| 项目 | 值 | 说明 |
|---|---|---|
| AC HTTP（代码默认） | `:8080/tcp` | `AC_HTTP_ADDR` |
| AC HTTP（现网 NAS） | `:8090/tcp` | 8080 被宿主 apache2 占用 |
| 发现 | `:29820/udp` | AP 广播，AC 应答；桥接网络收不到广播，必须 host 网络 |
| AP 间协同 | `29821/udp` | 可关闭（`peer_port=0`），默认开启 |
| AC 数据目录 | `/var/lib/ac`（容器内） | `ac.db`、`ac.id`、`ac_signing.key` |
| AP 二进制 | `/usr/sbin/ac-agent` | 静态 arm64 |
| AP 服务 | `/etc/init.d/ac-agent` | `START=95`，procd 托管 |
| AP 配置 | `/etc/config/ac` | 安装时 `chmod 0600` |
| AP 状态 | `/etc/ac/state.json` | TOFU 公钥、`ac_id`、`applied_rev` |
| AP 备份/暂存 | `/etc/ac/backup/`、`/etc/ac/staging/` | 每次 apply 前备份 |
| AP 运行快照 | `/var/run/ac-agent/status.json` | 每轮心跳重写，tmpfs |
| AP LuCI | `/cgi-bin/luci/admin/control/ac-agent` | 需安装 LuCI 应用 |

### 3.3 AC 侧组件

| 路径 | 职责 |
|---|---|
| `cmd/ac/main.go` | 入口：`serve`、`version`、`pubkey`、`verify <pkg>`；启动时迁移库、生成/加载身份与签名密钥、播种 `default` 模板、播种 `admin/123456` 账户 |
| `internal/api` | HTTP 路由、登录会话、AP/管理双凭据、设备/模板/固件/告警/审计、内嵌 Web UI |
| `internal/provision` | 采纳/重新下发/换模板；渲染 `wireless`/`network`/`steering`/`neighbors`，打包并签名，按 `(mac, rev)` 存库 |
| `internal/bundle` | `manifest.json` + `tar.gz`；签名覆盖 manifest；AP 侧用公钥复验 |
| `internal/store` | SQLite：设备、模板、包、遥测、动作、计划、审计、设置 |
| `internal/discovery` | UDP 29820 监听与 `AC_OFFER` 应答；按 AP 来源路由推导本机地址 |
| `internal/config` | 全部 `AC_*` 环境变量与默认值、弱凭据告警 |
| `internal/keys` | Ed25519 私钥 `ac_signing.key` 的生成与加载 |
| `internal/render` / `internal/ucimerge` | 模板 → UCI 文本；AP 侧与本地 UCI 段级合并 |
| `internal/model` | 模板、设备覆盖、校验规则 |
| `web/`（内嵌） | 单页管理界面，与 REST 同端口 |

### 3.4 AP 侧组件

| 文件 | 职责 |
|---|---|
| `cmd/acagent/main.go` | `run`、`once`、`status`、`stations`、`version`；另有 `nl-dump`/`nl-ifaces`/`neigh`/`ubus-dump`/`radios`/`neighbors` 等现场诊断命令 |
| `internal/agent/agent.go` | 发现、hello、取包、应用、心跳退避；无 `ac_addr` 时每 3 次失败后每 60s 重新广播 |
| `internal/agent/apply.go` | 合并、备份、写盘、`ubus call network reload`（仅 network 变化时）、`wifi reload`、等 netdev、等 BSS、11k 写入、回滚 |
| `internal/agent/bundle`（`internal/bundle`） | 验签、逐文件 sha256、MAC/型号校验 |
| `internal/agent/peer.go` | UDP 29821：AP 间状态播报、可选 HMAC、报文新鲜度 1 小时、重放窗口 |
| `internal/agent/steering.go` / `steerloop.go` | 11v 与本地漫游建议；`ac_priority` 模式下控制器优先 |
| `internal/agent/uci*` / `ucimerge` | 保留本地 `wifinet*`/`radio*` 段，只动 `ac_` 前缀段 |
| `internal/agent/clients.go` / `radios.go` / `ports.go` | 客户端、射频、端口采样 |
| `internal/agent/status.go` | `status.json` 与 `ac-agent status` 的数据源 |

### 3.5 通信协议：发现、心跳、下发

**发现（仅在没有配置 `ac_addr` 时发生）**

1. agent 向 `255.255.255.255:29820` 和各接口定向广播发 `AP_DISCOVER`，等 3 秒。
2. AC 的 `discovery.Listener` 回 `AC_OFFER`，其中带 `ac_id`、HTTP 端口、推导出的 AC 地址、以及是否需要密钥。
3. agent 选中第一个应答并开始 HTTP 心跳；若已经显式配置 `ac_addr`，**一个广播包都不会发**（现网用 `tcpdump -ni any udp port 29820` 抓 8 秒为 0 个包验证过）。

跨三层时广播必然失败，此时必须手填 `ac_addr`；跨 NAT 还必须设置 `AC_ADVERTISE_IP`，否则 AC 推导出的地址可能是错的。

**心跳（默认每 20s 一次）**

`POST /api/v1/ap/hello`，`Authorization: Bearer <AC_SHARED_SECRET>`。请求体（`internal/proto`）包含：

- 身份：`mac`、`model`、`firmware`、`hostname`、`uptime`
- 同步：`applied_rev`、`sync_state`（`idle`/`applied`/`rollback`/`failed`）、`sync_message`
- 遥测：完整客户端列表、每 BSS 的 11k 自描述、射频实际信道/利用率、有线端口状态
- 结果：上一轮动作的执行结果、固件升级状态

AC 的应答 `HelloReply` 包含：`ac_id`、设备 `state`（`pending`/`adopted`）、`desired_rev`、`package_path`、`pubkey`、`secret_required`、固件目标、待执行动作、客户端绑定、站点 NTP 列表。

注意：**没有单独的 report 接口**。apply 的结果通过下一轮 hello 的 `sync_state`/`sync_message` 回传。

**取包与应用**

只有当 `desired_rev != applied_rev` 时才取包，包括“控制器从旧库恢复导致 rev 变小”的情况——这会把 AP 回滚到旧版本。

1. `GET <package_path>`，携带同一个 Bearer 密钥。
2. 用首次心跳钉住的公钥验 Ed25519 签名；逐文件比对 manifest 里的 sha256；拒绝 manifest 未列出的文件。
3. 校验 `manifest.device_mac` 与本机 MAC、`model_whitelist` 是否包含本机型号。
4. 备份当前文件到 `/etc/ac/backup/rev<N>-<epoch>/`（缺失文件也记入 `backup.txt`）。
5. 段级合并后写入 `/etc/config/`；`network` 有变化才执行 `ubus call network reload`，随后 `wifi reload`。
6. 等待 `br-lan.<vid>` 等 netdev 出现，轮询 `ubus call network.wireless status` 直到预期 BSS 为 up。
7. 在健康检查通过之后写入 11k 邻居表（reload 期间 hostapd 会重建，过早写入会丢 2.4G 的列表）。
8. 默认停用 `dnsmasq`/`odhcpd`；任一健康检查失败则整体回滚并上报 `rollback`。
9. 如果所有预期 BSS 都不存在，执行一次 `wifi up`（这是开机后无线全灭的恢复路径，普通 apply 不会触发）。

### 3.6 从首次上电到上线

```
t0  /etc/init.d/ac-agent start
    ├─ 有 ac_addr → 直接 HTTP；没有 → 广播 3s 发现
    └─ hello → AC 记为 pending
t1  运维在页面选模板并采纳 → AC 生成 rev1（此时没有 11k 列表）
t2  下一轮 hello 拿到 desired_rev=1 → 下载/验签/备份/合并/写盘/reload/健康检查
    → 下一轮 hello 上报 applied_rev=1、sync_state=applied
t3  该 AP 在 hello 里上报每 BSS 的 11k 自描述 → 运维执行“重新下发”
    → 生成 rev2，这次包含兄弟 AP 的 neighbors 文件
t4  agent 应用 rev2 → 11k 列表落到每个 BSS

稳态：每 20s 一次 hello；AC 90s 没收到心跳就在页面标离线
```

第一次采纳后**必须再 repush 一次**，否则 11k 列表为空。这不是 bug：第一版生成时 AC 还没收到这台 AP 的接口名与 BSS 报告。

### 3.7 rev 与离线自治

- `rev` 对每台设备单调递增，解绑（release）只回退 `state`/`template_id`，**不清零 rev**；`network_params` 也会保留，因为它记录的是机器的物理接线与自有地址。
- 配置包按 `(mac, rev)` 不可变保存，AP 不会看到“内容不同但编号相同”的包。
- AP 本地保存 `state.json` 和实际 UCI；AC 失联不影响已生效的配置，也不会删除时间源。
- AP 间 29821 通道独立于控制器：AC 不在时继续交换邻居/客户端信息；`peer_steer=1` 时本地可发 11v 建议。`peer_secret` 为空则只适用于可信二层网段。

---

## 4. 部署 AC

### 4.1 前置条件

| 项目 | 要求 |
|---|---|
| 运行环境 | Docker + Compose v2；或 Linux 直接跑静态二进制 |
| 构建环境 | Go 1.26（`go.mod` 的 `go` 指令）；`CGO_ENABLED=0` |
| 资源 | 2 vCPU / 1 GB 内存 / 100 MB 数据盘（以现网规模估算，未做压测） |
| 网络 | AP 与管理端能访问 AC 的 HTTP 端口；自动发现要求 AP 与 AC 在同一广播域 |
| 端口 | TCP 8080（或 8090）；UDP 29820；如启用 AP 间协同，还需 UDP 29821 在 AP 间可达 |
| 时间 | AC 与 AP 的时钟偏差不要超过 1 小时，否则 AP 间报文会被判为不新鲜 |
| 数据 | `data/` 必须可写，容器内以 uid 1000 运行 |

### 4.2 选择哪条部署路径

| 场景 | 路径 | 特点 |
|---|---|---|
| 生产 NAS 无外网 | `deploy/nas` + 预编译二进制 + `FROM scratch` | 全程离线，镜像内没有 shell/wget，因此**没有 healthcheck** |
| 生产 NAS 能访问 ghcr.io | `deploy/fnos` + 私有镜像 + `update-docker.sh` | 自动拉取、digest 比对、升级前备份 |
| 本机开发 / Docker Desktop | 仓库根 `docker-compose.yml`（可叠加 `docker-compose.dev.yml`） | 根镜像基于 alpine，带 healthcheck；开发覆盖文件改成端口映射 |
| 裸机 / 调试 | `make build` 后直接 `./bin/ac serve` | 需要自己准备 systemd 或前台运行 |

### 4.3 路径 A：无外网 NAS（现网采用）

目录按现网约定放在 `/srv/ac-controller`：

```sh
# 1. 在能联网、装了 Go 1.26 的机器上交叉编译（产物是 linux/amd64 静态二进制）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X github.com/naoki66/ac-controller/internal/version.Version=deploy-$(date +%Y%m%d)" \
  -o .deploy/nas/ac ./cmd/ac

# 2. 生成 AP 共享密钥（至少 16 字节；建议 32）
python3 -c "import secrets;print('AC_SHARED_SECRET='+secrets.token_urlsafe(32))" > .deploy/nas/.env
chmod 600 .deploy/nas/.env

# 3. 数据目录必须先存在，且属主 1000:1000
ssh root@10.0.30.5 'mkdir -p /srv/ac-controller/data && chown -R 1000:1000 /srv/ac-controller/data'

# 4. 上传并启动
scp .deploy/nas/{ac,Dockerfile,docker-compose.yml,.env} root@10.0.30.5:/srv/ac-controller/
ssh root@10.0.30.5 'cd /srv/ac-controller && docker compose up -d --build'

# 5. 自检
curl -s http://10.0.30.5:8090/api/v1/health
```

`deploy/nas/Dockerfile` 里的 `COPY --chmod=0755 ac /ac` 不能省：二进制在 Windows 上编译后落到宿主机是 644，`scp` 不会补可执行位，直接 `COPY` 会得到 `exec: /ac: permission denied`。

现网 compose 刻意不传 `AC_ADMIN_TOKEN`。这意味着自动化脚本若要用 Bearer 调管理 API，需要自己在 compose 的 `environment` 里补这一项；不补也能用页面（管理面始终要求登录，首次是 `admin`/`123456`）。

### 4.4 路径 B：GHCR 私有镜像

```sh
# 沿用同一个 /srv/ac-controller 与同一个 data/，否则 ac.id 与签名私钥会换，
# 所有 AP 都已钉住旧公钥，换目录等于全部重新 TOFU。
scp deploy/fnos/{docker-compose.yml,update-docker.sh} root@10.0.30.5:/srv/ac-controller/

cd /srv/ac-controller
chmod +x update-docker.sh
printf 'AC_SHARED_SECRET=%s\n' "$(openssl rand -base64 32)" > .env && chmod 600 .env
printf '%s' '<带 read:packages 的 PAT>' > .ghcr_pat && chmod 600 .ghcr_pat
./update-docker.sh
```

`update-docker.sh` 的流程是：登录 ghcr → 记录当前版本/digest → `compose pull` → digest 未变且容器在跑则直接退出（零停机）→ digest 变了先把 `data/` 打成 `data-backup-<时间戳>.tgz` → `up -d --force-recreate` → 最多等 20 秒 `curl /api/v1/health`，失败打印 40 行日志并以非 0 退出。

回滚镜像：把 `.env` 里的 `AC_IMAGE` 指到某个 commit tag（如 `ghcr.io/naoki66/openwrt_ac-controller:94d346e`），再 `docker compose up -d`。

### 4.5 路径 C：仓库根 compose / 裸机

```sh
cp .env.example .env
# 编辑 .env：AC_SHARED_SECRET 必须是长随机值，否则 compose 会拒绝启动
docker compose up -d --build
curl -s http://127.0.0.1:8080/api/v1/health
```

根 compose 使用 `network_mode: host`，所以 UI 在宿主机 `:8080`。Docker Desktop 的 host 网络只在它的 Linux VM 内生效，本机联调要叠加开发覆盖文件：

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

开发覆盖会把网络改成端口映射，并把 `AC_ADVERTISE_IP` 固定为 `127.0.0.1`。它**收不到其他主机的广播**，真机 AP 不可能被自动发现；验证发现链路请用 `make smoke`（`scripts/fake-ap.py` 单播到 `127.0.0.1`）。

裸机：

```sh
make build          # 产出 bin/ac
./bin/ac serve -v   # 默认 ./data、:8080、:29820
./bin/ac version
./bin/ac pubkey     # 打印 AP 验签用的公钥
```

### 4.6 配置项（环境变量）

| 变量 | 默认值 | 作用 |
|---|---|---|
| `AC_DATA_DIR` | `./data` | `ac.db`、`ac.id`、`ac_signing.key` 所在目录 |
| `AC_HTTP_ADDR` | `:8080` | Web UI 与 REST 监听地址 |
| `AC_DISCOVER_ADDR` | `:29820` | 发现报文监听地址 |
| `AC_HTTP_PORT` | 从 `AC_HTTP_ADDR` 推导 | 通告给 AP 的回连端口；在端口映射环境里可单独指定 |
| `AC_SHARED_SECRET` | 空 | AP 侧 Bearer 密钥；空 = 不校验 AP，启动时告警 |
| `AC_ADMIN_TOKEN` | 空 | 仅给自动化用的管理 Bearer；空不影响页面登录 |
| `AC_ADVERTISE_IP` | 空 | 强制 `AC_OFFER` 里通告的地址；跨 NAT 必填 |
| `AC_OFFLINE_AFTER` | `90s` | 超过多久没心跳判离线 |
| `AC_NTP_SERVERS` | `ntp.ntsc.ac.cn,ntp1.aliyun.com,ntp.tencent.com,cn.pool.ntp.org` | 下发给 AP 的站点时间源；页面设置优先于环境变量 |
| `AC_SAMPLE_INTERVAL` | `60s` | 同一 AP 两次遥测采样的最小间隔 |
| `AC_SAMPLE_RETENTION` | `168h` | 遥测保留时长，每小时清理一次 |

两个凭据必须不同。启动日志会检查：密钥为空、令牌为空、长度不足 16 字节、或两者相同，都会打印告警。

运行时还可以在页面“系统”里改共享密钥、NTP、登录密码；共享密钥的修改会写入 `settings` 表，重启后仍然生效，并**优先于环境变量**。

### 4.7 首次登录与自检

1. 浏览器打开 `http://<AC>:<端口>/`，首次启动已播种账户 `admin` / `123456`，登录后立刻在“系统 → 账户”改密。
2. 健康检查（无鉴权）：

```sh
curl -s http://10.0.30.5:8090/api/v1/health
# {"status":"ok","version":"...","commit":"...","ac_id":"...","devices":0,
#  "pubkey":"...","algorithm":"ed25519","admin_auth_required":true,...}
```

3. 公钥（AP 验签与人工核对用）：

```sh
curl -s http://10.0.30.5:8090/api/v1/ac/pubkey
curl -s 'http://10.0.30.5:8090/api/v1/ac/pubkey?format=text'
```

4. 管理 API（需要登录会话或 `AC_ADMIN_TOKEN`）：

```sh
curl -s -H "Authorization: Bearer $TOKEN" http://10.0.30.5:8090/api/v1/devices
curl -s -H "Authorization: Bearer $TOKEN" http://10.0.30.5:8090/api/v1/clients
```

5. 容器日志：

```sh
docker logs -f ac-controller
```

启动日志里应能看到 `starting controller`、`http listening`、`ac_id` 与公钥。若看到 `AC_SHARED_SECRET is empty`，说明 AP 侧心跳不会被认证，必须补上。

### 4.8 升级、回滚、备份

- **离线部署升级**：替换 `/srv/ac-controller/ac` 后 `docker compose up -d --build`；`data/` 不变，`ac.id` 与签名私钥保留。
- **镜像部署升级**：`./update-docker.sh` 自动备份并重建；回滚见 §4.4。
- **配置回滚**：把旧版 `ac.db` 恢复后，设备表里的 `config_rev` 可能变小；AP 下一次心跳发现 `desired_rev` 更低，会拉取并应用旧 rev。这是协议支持的回滚路径。
- **数据备份**：`ac.db` 是 WAL 模式，不能直接 `cp`。用 SQLite 的在线备份：

```sh
python3 - <<'PY'
import sqlite3, time
src = sqlite3.connect("/srv/ac-controller/data/ac.db", timeout=20)
src.execute("PRAGMA busy_timeout=20000")
dst = sqlite3.connect("/srv/ac-controller/data/ac.db.bak-" + time.strftime("%Y%m%d%H%M%S"))
with dst:
    src.backup(dst)
dst.close()
PY
```

- **签名私钥丢失**：换新密钥后，已纳管的 AP 会因为钉住的公钥变化而拒绝新包；必须逐台删除 `/etc/ac/state.json` 并重启 agent，重新 TOFU。私钥和 `ac.id` 都应纳入备份。

---

## 5. 在 AP 上安装 agent

### 5.1 前置条件

| 项目 | 要求 / 现网事实 |
|---|---|
| 硬件 | Gemtek XR1710G（Airoha AN7581，`aarch64`）；其他 OpenWrt 设备需自行确认架构与 UCI 布局 |
| 固件 | 现网验证：ImmortalWrt 20261026 / 20261027。网络接管依赖 DSA 的 `br-lan.<vid>` 模型；非 DSA（swconfig）固件只建议先做“仅无线”模板 |
| 架构 | `uname -m` 为 `aarch64`；发布包是 `linux/arm64` 静态二进制 |
| 系统组件 | `procd`/`/etc/rc.common`、`ubus`、`uci`、`hostapd`、可用 `wifi` 命令；`/sys` 可读 MAC/型号 |
| 无线能力 | 11k/11v 需要 hostapd 暴露 `rrm_nr_get_own`、`rrm_nr_set`、`bss_transition_request`。现网固件满足；精简 `wpad-basic` 是否具备这些 ubus 方法未实测（推断，见 §9） |
| 网络 | 与 AC 同广播域可自动发现；跨三层必须 `ac_addr` 且 TCP 端口可达 |
| 空间 | 二进制约 6.5 MB，另需 LuCI 文件与备份空间 |
| 时间 | 首次启动时钟可能偏差 12–22 分钟；AP 间报文允许 1 小时偏差，但建议尽快同步 NTP |
| 顺序 | 首次接管前先备份 `/etc/config/wireless`、`/etc/config/network`；先 `dry-run`，再正式应用 |

### 5.2 方式 A：发布包（推荐）

仓库 `dist/` 下已有构建好的发布包，例如 `dist/ac-agent-deploy-20261106-arm64.tar.gz`。包内包含二进制、init 脚本、默认 `/etc/config/ac`、LuCI 视图/菜单/ACL/rpcd 后端和中文词条。

```sh
scp dist/ac-agent-deploy-20261106-arm64.tar.gz root@10.0.30.251:/tmp/
ssh root@10.0.30.251 'cd /tmp && tar -xzf ac-agent-deploy-20261106-arm64.tar.gz && cd ac-agent-deploy-20261106-arm64 && ./install.sh'
```

`install.sh` 的行为：

1. 把 `usr/`、`www/` 覆盖到根文件系统，安装 init 脚本并 `chmod 0755`；
2. 若 `/etc/config/ac` 已存在，**保留原配置**并备份到 `/etc/config/ac.bak`；不存在才安装默认配置并 `chmod 0600`；
3. `enable` + `restart`（失败则 `start`）；
4. 刷新 LuCI 的 apk/opkg 状态戳、清 `/tmp/luci-*` 缓存、重启 `rpcd`、reload `uhttpd`。

所以“升级不丢 `ac_addr`/密钥”，但也意味着首次安装后必须编辑 `/etc/config/ac` 再重启。

### 5.3 方式 B：手工安装二进制与 init

适合没有发布包、或只想替换二进制：

```sh
# 在构建机
make agent     # 产出 bin/ac-agent-linux-arm64

# 上传并安装
scp bin/ac-agent-linux-arm64 root@10.0.30.251:/tmp/ac-agent
ssh root@10.0.30.251 'cp /tmp/ac-agent /usr/sbin/ac-agent && chmod 0755 /usr/sbin/ac-agent && rm -f /tmp/ac-agent'
scp deploy/openwrt/ac-agent/files/ac-agent.init root@10.0.30.251:/etc/init.d/ac-agent
ssh root@10.0.30.251 'chmod 0755 /etc/init.d/ac-agent'
```

LuCI 页面可选，单独安装：

```sh
./deploy/openwrt/install-luci-app.sh 10.0.30.251
```

该脚本要求本机有 `python3`（编译 `.po` 为 `.lmo`），并会提示 AP 上缺少 `/usr/sbin/ac-agent`。它只安装视图/菜单/ACL/rpcd，不复制 agent 二进制。

### 5.4 方式 C：OpenWrt feed 编译

`deploy/openwrt` 是 feed 根，包名为 `ac-agent`，另有 `luci-app-ac-agent`。新版 feed 可用：

```
src-git --root=deploy/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git
```

旧版（24.10 及以前）没有 `--root`，用本地 checkout：

```
src-link ac_agent /path/to/ac-controller/deploy/openwrt
```

注意：`deploy/openwrt/ac-agent/Makefile` 里的 `PKG_SOURCE_VERSION` 仍钉在 `5255f133...`，而当前仓库 HEAD 是 `ceff1dc`。**用 feed 编译前必须把它更新到要构建的 commit**，否则编出来的是旧代码。

### 5.5 `/etc/config/ac`

```sh
config agent 'agent'
	option enabled '1'
	option ac_addr '10.0.30.5:8090'
	option secret '<AC_SHARED_SECRET>'
	option interval '20'
	option peer_port '29821'
	option peer_interval '30s'
	option peer_network 'lan'
	option peer_secret '<PEER_SHARED_SECRET>'
	option peer_steer '1'
```

| 选项 | 默认 | 说明 |
|---|---|---|
| `enabled` | `1` | 0 时停止 agent、取消自启；init 与 LuCI 都检查这一项 |
| `ac_addr` | 空 | `host:port`；空 = 广播发现。跨三层必填 |
| `secret` | 空 | 必须与 AC 的 `AC_SHARED_SECRET`（或页面里轮换后的值）一致 |
| `interval` | `20` | 心跳秒数；AC 离线判定默认 90s，不要设得比它大太多 |
| `peer_port` | `29821` | 0 关闭 AP 间协同 |
| `peer_interval` | `30s` | AP 间播报间隔 |
| `peer_network` | `lan` | UCI **逻辑网络名**，不是设备名；用于解析 `l3_device` 并加入 IPv6 link-local 组播，失败回退 IPv4 广播 |
| `peer_secret` | 空 | 所有 AP 必须一致；不要复用 AC 共享密钥；空 = 不认证 |
| `peer_steer` | `1` | AC 失联时允许本地 11v 建议 |

`dry_run` 已不再是 UCI 选项；演练请用命令行 `ac-agent once --dry-run -v`。

### 5.6 注册与配对（TOFU）

1. 启动 agent：`/etc/init.d/ac-agent enable && /etc/init.d/ac-agent start`。
2. 第一轮 hello 携带 MAC/型号/固件与空 `applied_rev`；AC 创建设备记录，状态 `pending`。如果 AC 配置了密钥而 AP 的 `secret` 不匹配，直接 401，不会进入待采纳。
3. 应答里的 `ac_id` 与 `pubkey` 被 agent 写入 `/etc/ac/state.json`（0600）。此后**任何公钥或 `ac_id` 变化都会被拒绝**，这是刻意的防中间人设计；换控制器或重建 AC 必须人工删 state。
4. 在页面“设备”里选模板并“采纳”，或：

```sh
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"template_id":"ap-xr1710g-wifi"}' \
  http://10.0.30.5:8090/api/v1/devices/00:00:00:00:00:21/adopt
```

不传 `template_id` 时，服务端按“设备所属分组配置的模板，否则 `default`”处理；空库时只有占位模板 `default`（示例内容，不能直接上生产）。

### 5.7 正式接管前先 dry-run

```sh
ac-agent once --dry-run -v
```

它只渲染、验签并暂存到 `/etc/ac/staging/rev<N>/`，不写 `/etc/config`、不 reload、不 `wifi up`。上线前把暂存产物与一台已知正常的同模板 AP 做 `diff`，逐字节一致再正式应用，能把“会不会锁死”从猜测变成核对。

### 5.8 验证是否上线

AP 侧：

```sh
/etc/init.d/ac-agent status                  # running
ac-agent status                              # 人读摘要
ac-agent status --json                       # 与 LuCI 同一结构
cat /var/run/ac-agent/status.json            # 每轮心跳重写
logread | grep ac-agent | tail -50
```

`ac-agent status` 的 `state` 是控制器关系的判据：

| state | 含义 |
|---|---|
| `discovering` | 没有 AC 地址，正在广播 |
| `unreachable` | 地址已知，但心跳失败 |
| `connected` | 心跳通过，但设备尚未被采纳 |
| `managed` | 已采纳，且有 rev 下发 |

同时看 `applied_rev`、`desired_rev`、`last_hello_ok`、`last_hello_error`、`pending`、`last_error`。`last_hello_ok=false` 且 `hello_error` 有 401，几乎一定是密钥不一致。

AC 侧：

```sh
curl -s -H "Authorization: Bearer $TOKEN" http://10.0.30.5:8090/api/v1/devices
```

重点字段：`state`、`config_rev`、`applied_rev`、`sync_state`、`sync_message`、`last_seen_age`、`online`。

页面：设备行应显示“已采纳”，`applied_rev == config_rev`，`sync_state=applied`；AP 的 11k 状态可在设备详情或 `docs/ac-11k-neighbors.md` 的校验脚本中查看：

```sh
./scripts/verify-11k.sh
AC_AP_HOST=10.0.30.250 ./scripts/verify-11k.sh
```

### 5.9 首次接管的正确顺序

1. agent 上线，设备进入 `pending`。
2. 如果计划做网络全接管，**先 `PUT /devices/{mac}/network` 写入该机的端口/VLAN/管理地址**。
3. 再 `adopt` 全接管模板（模板的无线部分应与旧模板逐字段一致，避免顺手改掉客户端配置）。
4. 应用 rev1 → AP 上报 11k 自描述。
5. **再 repush 一次** → rev2 带 neighbors，应用后 11k 生效。

顺序反了会出事：模板默认不含 `ipaddr`，先换模板再设 `network_params` 会渲染出没有管理地址的接口，AP 一应用就把自己锁在门外（现网 `.251` 真实发生过）。`PUT /network` 只存储不校验，所以“先参数、后模板”是安全的。

若解绑后重新采纳报：

```
provision: device ... network_params address [lan lan2 ...]: no such interface in template "default"
```

不是故障，是保护：设备仍带着为旧模板写的二层覆盖，而新模板不定义那些接口。应换用接口集合匹配的 `template_id`，不要清覆盖。

### 5.10 升级与卸载 agent

```sh
# 升级：先停服务，避免 "Text file busy"
/etc/init.d/ac-agent stop
scp bin/ac-agent-linux-arm64 root@10.0.30.251:/tmp/ac-agent
ssh root@10.0.30.251 'cp /tmp/ac-agent /usr/sbin/ac-agent && chmod 0755 /usr/sbin/ac-agent && /etc/init.d/ac-agent start'
```

`/etc/ac/state.json` 保留，因此不会重新 TOFU；`/etc/config/ac` 也不会被手工覆盖。

彻底脱离控制器：

```sh
/etc/init.d/ac-agent disable
/etc/init.d/ac-agent stop
rm -f /etc/ac/state.json
```

然后手工恢复 `/etc/config/wireless`（本地 `wifinet*`/`radio*` 段只是被 `disabled '1'`，没有删除；删掉 `ac_` 前缀段并去掉 `disabled` 即可）。只删 agent 而不恢复 UCI，AP 会停在最后一次下发的配置上。

---

## 6. 日常运维

```sh
# 重新下发（使用设备当前已采纳的模板；请求体被忽略）
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://10.0.30.5:8090/api/v1/devices/<mac>/repush

# 换模板（对已采纳设备同样适用，rev 继续递增）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"template_id":"ap-xr1710g-full"}' \
  http://10.0.30.5:8090/api/v1/devices/<mac>/adopt

# 查看设备 / 客户端
curl -s -H "Authorization: Bearer $TOKEN" http://10.0.30.5:8090/api/v1/devices
curl -s -H "Authorization: Bearer $TOKEN" http://10.0.30.5:8090/api/v1/clients
```

- **改模板后**：页面或 `repush` 对每台设备生成新 rev；AP 在下一轮心跳自动拉取。不要手工改 AP 的 UCI，否则下一次 apply 会被合并结果覆盖。
- **换共享密钥**：页面修改立即生效，旧密钥的下一次心跳直接 401。必须逐台更新 `/etc/config/ac` 的 `secret` 并 restart；AC 侧不会保留宽限期。
- **固件 OTA**：把镜像上传到 AC 固件仓库，给设备设置目标版本；AP 下载后先校验 SHA-256 再 `sysupgrade`。镜像本身没有签名，只有控制器的摘要——控制器被攻破就等于 AP 被攻破。
- **动作**：重启、重载无线、恢复出厂、LED 计划都由页面下发，结果在下一轮 hello 回传。恢复出厂成功后 AC 会自动解除采纳，避免下一轮又把包推回去。
- **审计**：`GET /api/v1/events` 可看到采纳、模板变更、密钥轮换、动作结果。

---

## 7. 故障排查

### 7.1 按层次定位

| 层 | 现象 | 先看什么 |
|---|---|---|
| 发现 | `state=discovering` 一直不变 | 是否配了 `ac_addr`；AC 是否监听 29820；是否同一广播域；容器是否 host 网络 |
| 认证 | 每 20s 401 | `/etc/config/ac` 的 `secret` 与 AC 的 `AC_SHARED_SECRET`/页面轮换值是否一致；`logread` 里的 `hello_error` |
| 采纳 | `connected` 但无 rev | 设备仍是 `pending`；页面未采纳，或采纳时模板 ID 错误 |
| 取包 | `sync_state=failed`，包下载失败 | TCP 端口是否可达；反向代理是否保留 `Authorization`；包路径里的 MAC 是否本机 |
| 应用 | `sync_state=rollback` | `sync_message`、`/etc/ac/backup/` 备份内容、`logread` 里的 netdev/BSS 报错 |
| 无线 | apply 成功但无 SSID/客户端掉线 | `ubus call network.wireless status`；BSS 是否 disabled；信道/国家码是否合法 |
| 11k | 列表为空 | 是否做过第二次 repush；hostapd 是否支持 `rrm_nr_set`；同 SSID 邻居是否存在 |
| 在线 | 页面离线 | `last_seen_age` 是否超过 90s；AP 时间是否漂移；心跳是否被防火墙拦截 |

### 7.2 常见失败原因（均可能“进程还在跑”）

| 现象 | 原因 | 处理 |
|---|---|---|
| 容器 `exec: /ac: permission denied` | 预编译二进制没有执行位 | 使用 `COPY --chmod=0755` |
| compose 说 Started 但容器立刻退出 | 基础镜像拉不动；无外网 | 改用 `deploy/nas` 的 scratch + 预编译二进制 |
| apply 回滚，日志说若干 BSS absent | 健康检查把模板里 `disabled` 的 SSID 也算进必须 up 的集合 | 修正模板或确认预期 BSS 集合；查看 `/etc/ac/backup/rev<N>-*/` |
| apply 成功但 11k 全空 | 邻居表写在 `wifi reload` 返回后过早 | 代码已改为健康检查通过后写入并回读；确认 agent 版本不旧 |
| 服务 running，但每 20s 401 | 旧 init 脚本连续两次 `procd_set_param env`，第二次覆盖第一次 | 现版已合并为一次 env；手工核对 `ac.agent.secret` |
| init 读不到 `ac_addr` | 旧实现 `config_load` + `config_get` 静默为空 | 现版改用 `uci -q get` |
| LuCI 页面空白/旧内容 | 浏览器或 LuCI 缓存 | `Ctrl+Shift+R`；安装脚本已清 `/tmp/luci-*` |
| 拉私有镜像 401/denied | PAT 过期、缺 `read:packages`、所有者与 `GHCR_USERNAME` 不一致 | 重新生成只读 PAT |
| `update-docker.sh` 报 missing AC_SHARED_SECRET | `.env` 不在脚本同目录或被注释 | 检查 `.env` 与权限 600 |
| 升级后 AP 出现两套 `wifi-device` | 段名改过（`ac_radio0..2` → `radio0..2`），opkg 升级不清理旧 UCI 段，rev 又没变 | `uci show wireless \| grep '=wifi-device'` 应为 3 个；用 `repush` 清理，不要手工删 |
| 采纳/换模板后 AP 失联 | 模板不含管理地址，或覆盖与模板接口不匹配 | 先 `PUT /network` 再 adopt；用匹配的 `template_id` |
| 重启后无线全灭，但服务正常 | hostapd 起来后所有 BSS 又被拆掉 | 执行一次 `/etc/init.d/wifi_schedule disable`；agent 的 `EnsureWireless` 会在所有 BSS 缺失时 `wifi up` |
| AC 恢复旧库后 AP 不降级 | AP 只按 `desired_rev` 拉取 | 确认应答中的 `desired_rev` 确实变小；AP 支持拉取更低 rev |
| 跨三层发现失败 | 路由器不转发定向广播 | 配 `ac_addr`；跨 NAT 同时设 `AC_ADVERTISE_IP` |

### 7.3 三条现场命令

```sh
# AP 上：看无线运行时与 11k
ubus call network.wireless status
ubus call hostapd.phy0.1-ap0 rrm_nr_list

# AP 上：看当前 UCI 段数量（3 个 wifi-device 才是干净状态）
uci show wireless | grep '=wifi-device'

# AC 上：看谁在线、谁的应用失败
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8090/api/v1/devices
```

---

## 8. 安全边界与已知缺口

| 项 | 现状 |
|---|---|
| 传输 | AC 只提供 HTTP，没有内置 HTTPS。模板含明文 PSK，管理网段必须可信；对外暴露请放在反向代理/TLS 之后 |
| 两套凭据 | AP 共享密钥与管理登录/令牌独立。两者相同会让每台 AP 都能读管理面，启动时会告警 |
| 配置包 | Ed25519 签名 + 逐文件 sha256 + MAC/型号白名单；AP 首次见到公钥即钉住 |
| 首次信任 | TOFU 不防首次连接时的中间人；首次配对应在可信网段完成 |
| 包下载鉴权 | **只有共享密钥，没有设备级绑定**。任一持有密钥的 AP 可以请求其他 MAC 的包。这与 `server.go` 里“不得读取他人包”的注释不一致，以处理函数实际行为为准；`docs/ac-p2-provisioning.md` 也把这一点列为已知缺口 |
| 固件 | 只校验控制器给的 SHA-256，镜像本身不签名；也没有 A/B 自动回退 |
| AP 间 | `peer_secret` 为空时仅靠可信二层；启用后为 HMAC，重放窗口 64，时间容忍 1 小时 |
| 账户 | 单管理员账户，无角色/多租户；审计日志可查但未做不可篡改存储 |
| 本地凭据 | `secret`/`peer_secret` 存在 `/etc/config/ac`（0600）；`/etc/ac/state.json` 含钉住的公钥但不含共享密钥 |
| 备份 | `ac.db` 含模板明文 PSK；`data-backup-*.tgz` 与 `ac_signing.key` 必须按同等敏感级别保管 |

---

## 9. 验证状态与未覆盖项

**本文依据的事实**

- 代码结构、接口、默认值、错误路径：直接读 `cmd/`、`internal/`、`deploy/` 得出。
- 现网部署参数（`10.0.30.5:8090`、`.250/.251`、ImmortalWrt 20261026/20261027、18 SSID、11 BSS、回滚与 11k 实测）：来自 `docs/ac-deployment.md`、`docs/ac-11k-neighbors.md`、`docs/ac-boot-wireless.md`、`docs/ac-251-recovery.md`。
- 本次只登录了编译机 `10.10.20.6` 阅读仓库；**没有登录 AC 或 AP 复测**。

**从代码推断、未在真机复测的内容**

- `wpad-basic` 是否缺少 `rrm_nr_*`/BTM 方法（推断：精简固件可能缺少，现网固件满足）。
- 非 DSA（swconfig）固件能否安全使用网络全接管（推断：`br-lan.<vid>` 模型不适用，建议仅无线）。
- 资源占用“2 vCPU/1 GB 够用”只是按现网 2 AP 规模估算，未压测。

**已发现的文档过时点**

- `docs/ac-agent.md` 开头写“尚未在真机 XR1710G 上运行”，但同一文档后文记录了 XR1710G 的逐 BSS 核对，且 `docs/ac-deployment.md` 记录了真机部署与回滚；以部署记录为准。
- `docs/ac-p2-provisioning.md` 开头写“AP 侧 agent 未实现”，同样是阶段性状态，当前仓库已有完整 agent 与发布包。
- `deploy/openwrt/ac-agent/Makefile` 的 `PKG_SOURCE_VERSION` 落后于 HEAD，feed 构建前必须更新。
- `deploy/nas/docker-compose.yml` 未传 `AC_ADMIN_TOKEN`；需要 Bearer 自动化时自行补充。

---

## 10. 关键文件索引

| 文件 | 内容 |
|---|---|
| [docs/ac-ap-controller-design.md](docs/ac-ap-controller-design.md) | 总体设计、约束、阶段划分 |
| [docs/ac-deployment.md](docs/ac-deployment.md) | 现网部署、全接管顺序、踩坑、备份 |
| [docs/ac-agent.md](docs/ac-agent.md) | agent 行为、合并规则、状态字段 |
| [docs/ac-p2-provisioning.md](docs/ac-p2-provisioning.md) | 包格式、签名、rev 语义 |
| [docs/ac-11k-neighbors.md](docs/ac-11k-neighbors.md) | 11k 同步原理与真机验证 |
| [docs/ac-11v-steering.md](docs/ac-11v-steering.md) | 11v 漫游策略 |
| [docs/ac-luci-app.md](docs/ac-luci-app.md) | AP 侧 LuCI 安装与页面 |
| [docs/ac-boot-wireless.md](docs/ac-boot-wireless.md) | 重启后无线不恢复的事故复盘 |
| [docs/development.md](docs/development.md) | 本地开发、测试、脚本 |
| `deploy/openwrt/ac-agent/` | OpenWrt 包与 init/UCI 默认值 |
| `deploy/openwrt/luci-app-ac-agent/` | LuCI 视图、菜单、ACL、rpcd |
| `deploy/nas/`、`deploy/fnos/` | 两条生产部署路径 |
| `scripts/verify-11k.sh`、`scripts/fake-ap.py` | 11k 校验、发现链路模拟 |

---

## 11. 一句话总结

`ac-controller` 把“AP 该长什么样”变成一个带签名、可回滚、可预演的版本化配置包；AC 只维护期望状态，AP 自己拉取并在本地完成合并、应用与自检——因此控制器离线不会让局域网失去无线，而错误配置也不会在无人看守时把 AP 锁死。
