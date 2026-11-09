# OpenWrt AC Controller

**简体中文** · [English](README.en.md)

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![Docker pulls](https://img.shields.io/docker/pulls/naoki66/openwrt_ac-controller.svg)](https://hub.docker.com/r/naoki66/openwrt_ac-controller)
[![Docker Image Version](https://img.shields.io/docker/v/naoki66/openwrt_ac-controller?sort=semver&label=image)](https://hub.docker.com/r/naoki66/openwrt_ac-controller/tags)
[![Docker Last Updated](https://img.shields.io/docker/last-updated/naoki66/openwrt_ac-controller)](https://hub.docker.com/r/naoki66/openwrt_ac-controller)

> 轻量的 OpenWrt 无线控制器：AC 端是一个 Go 单二进制（内嵌 Web UI、REST API、SQLite），AP 端是跑在 procd 下的 `ac-agent`。一条 compose 就能在 NAS、x86 软路由、ARM 盒子上跑起来，也可以把 AC 编译成 OpenWrt 软件包直接跑在路由器上。

**最快路径**：起容器 → 给 AP 装上 agent 并按需填共享密钥 → 在「设备」页采纳模板。

> 本文是中文文档，完整英文版见 [README.en.md](README.en.md)。所有命令与配置均可直接复制执行。

## 1. 项目定位与适用边界

它**不转发用户流量**，也不实现 CAPWAP，只做一件事：把「这台 AP 应该长什么样」编译成带 Ed25519 签名的版本化配置包，由 AP 主动拉取、验签、与本地 UCI 段级合并、应用、自检，失败自动回滚。AC 只维护期望状态，因此 AC 宕机不会让局域网失去无线。

| 适合 | 不适合 |
|---|---|
| 多台 OpenWrt AP 统一管 SSID / VLAN / 射频参数 | 需要 CAPWAP、集中转发或隧道的商用 AC 场景 |
| 希望「控制器挂了无线照常工作」的家庭 / 小办公室 | 需要 AAA、Portal 认证、计费网关 |
| 愿意自己编译或刷配套固件的玩家 / 开发者 | 开箱即用、无需接触命令行的产品 |

**动手前请先看这里：**

- 项目处于 **v0.5.x 早期阶段**，功能、API 与配置格式仍可能变化，不建议直接用于关键生产网络。
- AP 侧**已验证的硬件**只有 Brightspeed/Gemtek **XR1710G**（airoha/an7581、arm64、DSA），配套 [ImmortalWrt 固件](https://github.com/naoki66/ImmortalWrt-for-Gemtek-brightspeed/releases)。其他 OpenWrt 系设备需自行验证：要求 DSA 网络模型、`procd`/`ubus`/`uci`/`hostapd` 可用，11k/11v 还要求 hostapd 暴露 `rrm_nr_get_own`、`rrm_nr_set`、`bss_transition_request`。设备页的能力徽章是**运行时实测**：11k/11v 探测 ubus 方法面，11r（FT）读取 netifd 生成的 hostapd 运行配置（`/var/run/hostapd-*.conf`）并附带移动域（MDID），全部 BSS 满足才亮。
- 自动发现要求 AP 与 AC 在**同一广播域**；跨三层需在 AP 的 LuCI 页面填「控制器地址」（见第 8 节）。
- 镜像为 `linux/amd64` + `linux/arm64` 多架构清单，`docker pull` 会自动选择。

## 2. 核心特性

- **段级合并，不整文件覆盖**：应用前备份、失败自动回滚，避免把自己锁在 AP 门外。
- **防锁死三道防线**（v0.5.1 之后）：下发前 `preflight` 预检管理地址迁移；下发后校验 BSS 与管理地址是否丢失；自动把新增管理接口补进 `lan` 防火墙 zone 并修正 dropbear 绑定。
- **AC 不主动连 AP**：配置永远由 AP 拉取；AP 之间经 UDP 29821 交换 11k 邻居与客户端信息，AC 失联时本地 11v 漫游建议仍可用（`peer_steer=1`）；检测到 usteer/dawn 时自动让位。
- **签名配置包**：Ed25519 签名 + manifest 逐文件 sha256 + 目标 MAC / 型号白名单；AP 首次心跳钉住 AC 公钥（TOFU）。
- **一个密钥，两个用途**：AC 与每个 AP 只填同一个 `AC_SHARED_SECRET`，由 HKDF-SHA256 派生出 AP→AC 鉴权令牌和 AP↔AP 报文 HMAC 密钥，两者互不相同，原始密钥永不上线传输。
- **模板 + 设备覆盖 + 分组**：模板定义无线与二层，分组决定成员用哪个模板，设备级 `network_params` / 射频覆盖给出端口、VLAN、管理地址、信道与功率。
- **运维闭环**：固件镜像仓库与升级、定时重启 / LED、客户端命名与踢除、遥测图表、事件日志、配置导入导出、诊断信息。
- **两种运行档位**：`full`（Docker 默认，全功能）与 `openwrt`（路由器上关闭遥测历史与固件仓库、限制配置包版本数与事件条数、限制 Go 内存）。

管理界面内置 8 个页面：设备概览、无线终端、运行情况、无线服务、射频管理、设备端口、固件升级、系统配置（含登录密码、共享密钥、NTP、审计日志与备份）。

## 3. 架构与端口

```
浏览器 ──HTTP──▶ AC（单二进制：Web UI + REST + SQLite；TCP 8090 / UDP 29820）
                   ▲ 全部由 AP 主动：广播发现 → hello（20s）→ 拉取签名配置包
                   │
AP：ac-agent ──────┘  验签 → UCI 段级合并 → reload → 自检 → 失败回滚
                   └── UDP 29821（可选）──▶ 其它 AP：11k 邻居、本地漫游建议
```

| 端口 | 协议 | 用途 |
|---|---|---|
| TCP 8090 | TCP | Web UI 与 REST API |
| UDP 29820 | UDP | AP 广播发现（host 网络下生效） |
| UDP 29821 | UDP | AP 间协同，可关闭（`peer_port 0`） |

> `network_mode: host` 是 Docker 部署的**硬要求**：发现报文是 UDP 广播，bridge 隔离子网收不到，AP 会一直停在 `discovering`。host 模式下无需 `ports:` 映射。`init: true` 用于把 SIGTERM 透传给容器内的 Go 进程，便于优雅停止。
>
> host 网络依赖 Linux 宿主；在 Docker Desktop（Windows/macOS）上，请在 LuCI 里给 AP 填「控制器地址」直连（见第 8 节）。

## 4. 快速开始（Docker，推荐）

镜像：[Docker Hub · naoki66/openwrt_ac-controller](https://hub.docker.com/r/naoki66/openwrt_ac-controller)。

**1. 准备数据目录，属主必须是 1000:1000**（镜像内以 uid 1000 运行）

```sh
mkdir -p data && chown 1000:1000 data
```

**2. 写 `docker-compose.yml`**：`AC_SHARED_SECRET` 可选——填自己的密钥，或留空后到 Web「系统配置」页设置。compose 只保留部署必需项，其余变量都有内置默认值；NTP、登录密码等可在「系统配置」页修改并持久化，不必写进 compose。完整变量清单见第 6 节。下面这份与仓库里的 [`controller/deploy/docker-compose.yml`](controller/deploy/docker-compose.yml) 一致。

```yaml
services:
  ac:
    image: naoki66/openwrt_ac-controller:latest
    container_name: ac-controller
    restart: unless-stopped
    init: true
    network_mode: host
    environment:
      TZ: Asia/Shanghai
      # 共享密钥（可选，留空可在 Web 内设置）
      AC_SHARED_SECRET: ""
      AC_ADMIN_USER: "admin"
      AC_ADMIN_PASSWORD: "123456"
    volumes:
      - ./data:/var/lib/ac
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

若 8090 / 29820 被占用，再加 `AC_HTTP_ADDR` / `AC_DISCOVER_ADDR` 覆盖默认值（见第 6 节）。

**3. 启动并自检**

```sh
docker compose up -d
curl -s http://<宿主机IP>:8090/api/v1/health
# 返回 {"status":"ok","version":...,"commit":...,"devices":0,...}
```

打开 `http://<宿主机IP>:8090/`，用 `admin` / `123456` 登录，**第一件事就是改密码**。然后给 AP 装 agent（见第 8 节）。

## 5. 其他部署方式

| 方式 | 适用场景 | 入口 |
|---|---|---|
| Docker Hub 镜像（推荐） | NAS、x86 软路由、ARM 盒子，有外网 | 第 4 节 |
| OpenWrt 本机软件包（开发测试中） | 不想依赖 Docker，直接用一台路由器跑控制器 | 下方 |

### 在 OpenWrt 路由器上跑 AC（开发测试中）

> **开发测试中，尚未正式发布。** 这一形态把同一个二进制做成 `ac-controller` 软件包，由 procd 托管，Web UI 仍由二进制自己提供。预计占用 **XXX MB 内存 / XXX CPU**（数值待实测；当前 Go 内存上限设为 64MiB，非实测占用）；为适配 ARM 路由器，已裁剪遥测历史、固件仓库、配置包版本数与事件日志上限（即 `AC_PROFILE=openwrt` 档位），并限制 Go 运行时内存。优化完成后正式发布，当前不建议用于正式网络。

## 6. 配置

compose 只列出 4 个常用部署项。下表是**全部**可用变量与内置默认值，未写进 compose 的都走默认值；共享密钥、NTP、登录密码可在「系统配置」页在线修改并持久化（页面设置优先于环境变量）。只有需要改端口、切运行档位或调优时，才往 compose 里加对应变量。

<details>
<summary>展开全部 19 个环境变量与默认值</summary>

| 变量 | 默认值 | 说明 |
|---|---|---|
| `AC_SHARED_SECRET` | 空（**可选**） | AC 与所有 AP 共用的密钥；留空可在「系统配置」页设置或在线轮换（设置时 ≥12 位），页面值优先于环境变量 |
| `AC_ADMIN_USER` | `admin` | 初始登录账号，**仅首次启动播种一次** |
| `AC_ADMIN_PASSWORD` | `123456` | 初始登录密码，同上，首次登录后立即修改 |
| `AC_ADMIN_TOKEN` | 空 | 自动化用的 Bearer 凭据，必须与共享密钥不同 |
| `AC_HTTP_ADDR` | `:8090` | Web UI / API 监听地址，端口冲突时才需覆盖 |
| `AC_HTTP_PORT` | 由 `AC_HTTP_ADDR` 推导（`8090`） | 宣告给 AP 的端口 |
| `AC_DISCOVER_ADDR` | `:29820` | UDP 发现应答监听地址 |
| `AC_ADVERTISE_IP` | 空 | 强制 AC_OFFER 里宣告的 AC 地址 |
| `AC_DATA_DIR` | `./data`（镜像内 `/var/lib/ac`） | SQLite、身份与签名私钥目录，镜像内已设好 |
| `AC_PROFILE` | `full` | `full` 全功能 / `openwrt` 资源受限档位 |
| `AC_NTP_SERVERS` | 4 个公共 NTP 源 | 下发给 AP 的授时源，直接在「系统配置」页设置即可 |
| `AC_OFFLINE_AFTER` | `90s` | 超过多久没心跳判定离线 |
| `AC_SAMPLE_INTERVAL` | `60s`（openwrt 档位 `120s`） | 遥测采样间隔 |
| `AC_SAMPLE_RETENTION` | `168h`（openwrt 档位 `24h`） | 遥测保留期 |
| `AC_TELEMETRY_HISTORY` | `true`（openwrt 档位 `false`） | 是否落库历史图表数据 |
| `AC_FIRMWARE` | `true`（openwrt 档位 `false`） | 是否启用固件仓库 |
| `AC_PACKAGE_HISTORY` | `0` 不限（openwrt 档位 `1`） | 每台 AP 保留的配置包版本数 |
| `AC_EVENTS_MAX` | `0` 不限（openwrt 档位 `500`） | 事件日志条数上限 |
| `TZ` | `Asia/Shanghai` | 定时重启 / LED 的 HH:MM 时区 |

</details>

> 首次启动后账号密码写入 SQLite，改环境变量**不再生效**；共享密钥与 NTP 可在「系统配置」页在线修改并持久化，页面设置优先于环境变量。若要改监听端口，加 `AC_HTTP_ADDR` / `AC_DISCOVER_ADDR`，并同步 `AC_HTTP_PORT`。

## 7. 升级

数据在宿主 `data/` 目录，重建容器不丢。

```sh
docker compose pull && docker compose up -d
```

`latest` 是滚动标签；想钉版本可用 `naoki66/openwrt_ac-controller:v0.5.1` 这类发布标签，或用 `sha-<短commit>` 标签回溯到具体构建。

## 8. 在 AP 上安装 agent

镜像只包含 **AC 端**；每台 AP 还需要 `ac-agent`。

> AP 侧需要两个包：`ac-agent`（agent 本体）与 `luci-app-ac-agent`（LuCI 设置页），版本对应关系见方式 C。三种安装方式任选其一。

### 方式 A：刷入配套固件（最省事）

适用 Brightspeed/Gemtek XR1710G：从 [ImmortalWrt 固件 Releases](https://github.com/naoki66/ImmortalWrt-for-Gemtek-brightspeed/releases) 刷入，固件里已带 agent 与 LuCI 页面。开机后按下方「在 LuCI 里配置 AP」按需填写控制器地址与共享密钥即可。

### 方式 B：从 OpenWrt feed 编译 ipk/apk

```sh
src-git --root=agent/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git
./scripts/feeds update ac_agent
./scripts/feeds install ac-agent luci-app-ac-agent
# menuconfig 选中后编译，把生成的包装到 AP
```

两个包的 `PKG_SOURCE_VERSION` 跟随仓库默认分支 `main`，不再钉死 commit，改了 agent 代码无需同步改 Makefile。三点需要注意：

- OpenWrt 的源码包缓存以 `<包名>-<PKG_VERSION>.tar.zst` 命名（`include/download.mk`），已有的 `dl/` 缓存不会因分支更新而自动刷新；发版时递增 `PKG_VERSION` / `PKG_RELEASE`，或删掉 `dl/ac-agent-*.tar.zst`、`dl/ac-controller-*.tar.zst` 强制重新拉取。
- `PKG_MIRROR_HASH:=skip` 必须保留（分支 tip 会移动，tarball 内容不可复现）；两个包也不再向二进制注入具体 commit，实际源码提交记录在每个 Release 的 `build-info.txt` 里。
- 想复现某次旧构建，本地覆盖即可：`make ... PKG_SOURCE_VERSION=<完整 commit SHA>`。

### 方式 C：从 Releases 下载 ipk / apk 安装

`ac-agent`、`luci-app-ac-agent` 及自动生成的 `luci-i18n-ac-agent-zh-cn` 由本仓库的 [OpenWrt Packages 工作流](https://github.com/naoki66/OpenWrt_AC-Controller/actions/workflows/openwrt-packages.yml) 用官方 OpenWrt SDK 编译，随 Release 发布。AP 侧包版本与 feed 里的 `Makefile` 一致，请下载与控制器同一 Release 的包：

| 包 | 版本 | 源码 | 内容 |
|---|---|---|---|
| `ac-agent` | `0.2.1-1` | `main` 分支（不锁 commit） | agent 本体、procd init 脚本、`/etc/config/ac` |
| `luci-app-ac-agent` | `1.0.0-5` | 随仓库 | LuCI 设置页与 `luci-i18n-ac-agent-zh-cn` 中文语言包 |

AC 端本机包 `ac-controller` 为 `0.1.0-1`，源码同样跟随 `main`，见第 5 节（开发测试中，工作流默认不构建，可打开 `build_controller` 输入）。

默认只发布 `25.12.6` 的 `.apk`，架构为 `aarch64_cortex-a53`；`x86_64`、`mipsel_24kc` 和 24.10 的 `.ipk` 已从默认矩阵移除。需要其它架构或 `.ipk` 时，手动运行 [OpenWrt Packages 工作流](https://github.com/naoki66/OpenWrt_AC-Controller/actions/workflows/openwrt-packages.yml) 并修改 `targets` 输入即可。每个 Release 还附带 `build-info.txt`（源码 commit、Go 版本、SDK 标签清单）与 `SHA256SUMS`。

```sh
# 1. 在有网的机器上，从本仓库 Releases 下载对应版本的包（<RELEASE_TAG> 换成实际标签）
#    https://github.com/naoki66/OpenWrt_AC-Controller/releases
gh release download <RELEASE_TAG> -R naoki66/OpenWrt_AC-Controller \
  -p '*ac-agent*' -D ./pkgs
scp ./pkgs/* root@<AP_IP>:/tmp/

# 2. 在 AP 上安装（25.12 / snapshots 的 apk）
apk add --allow-untrusted /tmp/*ac-agent*.apk
```

> 包管理器分界以 OpenWrt 官方为准：**24.10 及更早用 opkg / `.ipk`，25.12 与 snapshots 起用 apk / `.apk`**；当前 Release 默认只提供 `.apk`。资产随本仓库 [Releases](https://github.com/naoki66/OpenWrt_AC-Controller/releases) 发布，请下载与控制器同一 Release 的资产；`apk` 默认拒绝未签名包，本地文件安装必须带 `--allow-untrusted`。旧固件需要 `.ipk` 时可用工作流手动构建。

### 在 LuCI 里配置 AP

打开 LuCI → `Control` → `AC Agent`，在「控制器连接」里只确认这三项，其余保持默认，点「保存并应用」即可（保存时会自动启动 agent 并设为开机自启）：

| 界面字段 | 怎么填 | 是否必填 |
|---|---|---|
| 启用 | 保持打开（默认已开）。这一个开关同时负责启动 agent、开机自启和放行发现端口 | 默认即可 |
| 控制器地址 | 同一广播域**留空**，靠 UDP 广播自动发现，发现后页面会回填并可保存固定；跨三层必须填 `<AC_IP>:8090` | 跨三层必填 |
| 共享密钥 | 与 AC 侧生效的密钥 **完全一致**（环境变量或系统页设置） | AC 设置了密钥时必填 |

心跳间隔（默认 20 秒）、AP 间协同端口（29821）、本地漫游建议（默认开）等都不用动。页面上方的状态卡片会显示 `discovering` / `connected` / `managed`，需要立刻重新同步时点「重启 agent」。

## 9. 上线流程

1. 在 LuCI 的 `AC Agent` 页面确认「启用」已打开、共享密钥与 AC 侧一致（AC 未设置则留空），点「保存并应用」；状态卡片从 `discovering` 变为 `connected`。
2. AC 的「设备概览」出现 `pending` 设备 → 选模板 → **采纳**。
3. AP 下一轮心跳拉取 rev1 并应用。
4. **再点一次 repush**：第一版生成时 AC 还没收到接口/BSS 报告，rev2 才会带上兄弟 AP 的 11k 邻居表。

两个务必遵守的顺序：

- 首次正式接管前先演练：`ac-agent once --dry-run -v`，只渲染、验签、暂存，不动 `/etc`。
- 若要做网络全接管：**先 `PUT /devices/{mac}/network` 写入管理地址 / 端口 / VLAN，再采纳模板**；页面会在下发前调用 `preflight` 提示地址迁移。

`ac-agent status` 的 `state`：`discovering`（没找到 AC）、`unreachable`（心跳失败）、`connected`（未采纳）、`managed`（已纳管）。

## 10. REST API 速查

管理接口需要登录会话 Cookie，或 `Authorization: Bearer <AC_ADMIN_TOKEN>`；`/api/v1/health`、`/api/v1/ac/pubkey`、`/api/v1/ap/hello` 例外。

```sh
# 登录并拿会话 Cookie
curl -s -c ac.jar -X POST -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"123456"}' http://<AC_IP>:8090/api/v1/auth/login
curl -s -b ac.jar http://<AC_IP>:8090/api/v1/devices
```

<details>
<summary>展开 REST API 速查表</summary>

| 端点 | 用途 |
|---|---|
| `GET /api/v1/health` | 健康、版本、commit、设备数、AC 公钥 |
| `GET/POST /api/v1/devices`, `.../{mac}` | 设备列表与详情 |
| `POST /api/v1/devices/{mac}/adopt` · `repush` · `release` | 采纳 / 重新下发 / 释放 |
| `GET /api/v1/devices/{mac}/preflight` | 下发前的地址迁移预检 |
| `PUT /api/v1/devices/{mac}/network` · `radios` · `group` | 设备级网络 / 射频 / 分组覆盖 |
| `GET /api/v1/devices/{mac}/package` | 下载签名配置包（AP 用共享密钥，管理员用会话） |
| `GET/POST /api/v1/templates` · `clone` · `ssids` · `name` | 模板管理 |
| `GET/POST /api/v1/groups` | AP 分组 |
| `GET /api/v1/clients` · `PUT/POST .../{mac}/kick` | 终端查看、命名、踢除 |
| `GET /api/v1/radios` · `ports` · `telemetry/*` | 射频、端口、遥测 |
| `POST /api/v1/devices/{mac}/actions` · `PUT .../reboot-schedule` · `led-schedule` | 一次性命令与计划任务 |
| `GET/POST /api/v1/firmwares` · `POST /api/v1/devices/{mac}/firmware` | 固件仓库与升级目标 |
| `GET /api/v1/events` · `diagnostics` · `config/export` · `config/import` | 审计日志、诊断、配置导入导出 |
| `PUT /api/v1/settings/shared-secret` · `ntp` · `PUT /api/v1/auth/account` | 在线轮换密钥 / NTP / 账号 |

</details>

完整路由以 [`controller/internal/api/server.go`](controller/internal/api/server.go) 为准；`GET /api/v1/features` 会告诉界面当前档位开放了哪些功能。

## 11. 安全须知

- AC 只提供 **HTTP，没有内置 HTTPS**；模板里含明文 PSK。请把管理面放在可信网段，对外访问必须加 TLS 反向代理，**不要把 8090 暴露到公网 / WAN**。
- 初始账号默认 `admin` / `123456`，首次登录后立即修改；认证失败有 5 次/分钟的限流与 1 分钟锁定。
- 若启用共享密钥，它与管理密码、`AC_ADMIN_TOKEN` 三者必须互不相同、且足够长（≥12 位）；**留空时 AP 注册不鉴权**，启动会告警，仅适合完全可信的管理网段。
- TOFU 公钥钉扎**不防首次连接时的中间人**，首次配对应在可信网段完成；配置包下载目前只凭共享密钥，没有设备级证书绑定。
- `ac.db`、`/etc/ac/backup/`、配置导出文件、签名私钥与固件镜像都含敏感信息，需同等保护，别贴到 Issue 里。
- 固件升级与网络下发会改动 AP 的关键配置，请先用 dry-run / preflight 演练，并保留串口或本地管理口作为兜底。

## 12. 故障排查

<details>
<summary>展开常见现象与排查项</summary>

| 现象 | 先看什么 |
|---|---|
| AP 一直 `discovering` | compose 是否 `network_mode: host`；AC 是否监听 UDP 29820；是否同一广播域；跨三层是否在 LuCI 填了「控制器地址」 |
| 每 20s 收到 401 | LuCI 里的「共享密钥」与 AC 侧生效的密钥（环境变量或系统页设置）是否一致；是否轮换过（页面值优先） |
| 容器起不来 / 数据库报错 | `./data` 是否存在且属主 `1000:1000` |
| `connected` 但没有 rev | 设备仍是 `pending`，页面未采纳 |
| `sync_state=rollback` | `sync_message`、`/etc/ac/backup/`、`logread`；多半是 BSS 或管理地址自检失败 |
| 下发后 SSH 断开 | 管理地址迁移是否经过 `preflight`；防火墙 zone / dropbear 绑定（新版会自动修复） |
| apply 成功但 11k 为空 | 是否做过第二次 repush |
| 模板开了 11r 但徽章是灰的 | 能力徽章是运行时实测：等 agent 下一次心跳（探针缓存 5 分钟）；仍灰则 `cat /var/run/hostapd-*.conf` 确认每个 BSS 段都有 `ieee80211r=1`；跨 AP 漫游还要求各 AP 的 `mobility_domain` 一致 |
| 页面显示离线 | `last_seen_age` 是否超 90s；AP 系统时间是否漂移（签名校验依赖时间） |

</details>

## 13. 版本与发布

- 语义化标签 `vX.Y.Z` → 同名镜像 tag；`latest` 为滚动标签，`sha-<短commit>` 便于回溯。
- 实际运行版本以 `GET /api/v1/health` 的 `version` / `commit` 字段，或 `ac version` 为准。
- 发布记录见 [Releases](https://github.com/naoki66/OpenWrt_AC-Controller/releases)，标签见 [Tags](https://github.com/naoki66/OpenWrt_AC-Controller/tags)。
- 镜像版本状态：[Docker Hub 标签页](https://hub.docker.com/r/naoki66/openwrt_ac-controller/tags) 列出全部 tag 与架构（linux/amd64、linux/arm64）；顶部徽章显示当前最新发布版本与最后更新时间。

## 14. 反馈、许可证与链接

欢迎 Issue 与 PR。提问题请附上：`ac version` 输出、`/api/v1/health` 结果、`ac-agent status`、`logread | grep ac-agent` 相关片段、复现步骤，以及部署形态（Docker / OpenWrt 包）。**请勿粘贴共享密钥、PSK、`ac.db`、配置导出或签名私钥。**

- 许可证：[MIT](LICENSE) © 2026 naoki66
- 仓库：<https://github.com/naoki66/OpenWrt_AC-Controller>
- 镜像：[Docker Hub · naoki66/openwrt_ac-controller](https://hub.docker.com/r/naoki66/openwrt_ac-controller)
- 配套 AP 固件：[ImmortalWrt for XR1710G](https://github.com/naoki66/ImmortalWrt-for-Gemtek-brightspeed/releases)

## 15. 赞赏

如果这个项目对你有帮助，可以请作者喝杯咖啡 ☕

<img src="sponsor-wechat.png" alt="微信赞赏码" width="300">

**一句话总结**：AC 只维护「期望状态」，AP 自己拉取并在本地合并、应用、自检与回滚——控制器离线不会让局域网失去无线，错误配置也不会在无人看守时把 AP 锁死。
