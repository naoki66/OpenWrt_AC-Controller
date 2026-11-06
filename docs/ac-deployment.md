# 部署：控制器在 NAS，agent 在两台 XR1710G

> 本文记录 2026-10-31 的实际部署。所有地址、端口、路径都是现网值。
> 相关：`docs/ac-p2-provisioning.md`（协议与包格式）、`docs/ac-11k-neighbors.md`（11k 下发）。

## 1. 现网拓扑

| 角色 | 地址 | 硬件 / 系统 | 接管范围 | 说明 |
|---|---|---|---|---|
| 控制器 | `10.0.30.5` | x86_64 Debian 12，无外网 | — | Docker，HTTP `:8090`，发现 UDP `:29820` |
| AP-A | `10.0.30.251` | Gemtek XR1710G / ImmortalWrt 20261026 | 无线 + 网络（rev 5） | br-lan MAC `00:00:00:00:00:21` |
| AP-B | `10.0.30.250` | Gemtek XR1710G / ImmortalWrt 20261027 | 无线 + 网络（rev 3） | br-lan MAC `00:00:00:00:00:5a` |

两台 AP 的无线配置除 BSSID 外**逐字节相同**（`docs/baseline/ap250` 与 `ap251` 的
`wireless.uci` 零差异）。

网络部分原本"完全不同"——管理 IP、DUID、VLAN 端口映射都不同（`.250` 的 lan2/lan3
VLAN 甚至是反的、且没有 3169）。引入**设备级参数**后这些差异被外置，两台现已统一到
同一套端口映射（见 §4.2），只有管理 IP 末位不同。

## 2. 控制器（NAS）

### 2.1 为什么用 scratch 而不是 alpine

NAS 没有外网（`goproxy.cn` 都连不上），任何需要拉基础镜像的构建都会失败。而 `ac`
是 `CGO_ENABLED=0` 的静态二进制（SQLite 用纯 Go 驱动），`FROM scratch` 就够了：
构建全程离线，连 `FROM` 都不需要解析。

代价：容器里没有 shell、没有 wget，因此 `docker-compose.yml` 里**没有 healthcheck**。

### 2.2 端口：8090，不是 8080

NAS 的 8080 被宿主 apache2 占着。部署前先确认：

```sh
ssh root@10.0.30.5 'ss -lntup | grep -E ":8090|:29820"'
```

### 2.3 目录

```
/srv/ac-controller/
├── ac                     # 预编译的 linux/amd64 二进制
├── Dockerfile             # 来自 deploy/nas/Dockerfile
├── docker-compose.yml     # 来自 deploy/nas/docker-compose.yml
├── .env                   # AC_SHARED_SECRET，权限 600
└── data/                  # 挂给容器 /var/lib/ac，属主 1000:1000
```

`data/` 里是 `ac.db`（含模板与 PSK，明文）、`ac.id`、`ac_signing.key`。
**私钥只在 NAS 上**，丢了就得重新适配所有 AP（AP 侧 TOFU 会拒绝新公钥）。

### 2.4 部署

```sh
# 1. 交叉编译（Windows/macOS 上执行，产物是 linux/amd64）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X github.com/naoki66/ac-controller/internal/version.Version=deploy-$(date +%Y%m%d)" \
  -o .deploy/nas/ac ./cmd/ac

# 2. 若没有密钥，先生成一个
python3 -c "import secrets;print('AC_SHARED_SECRET='+secrets.token_urlsafe(32))" > .deploy/nas/.env
chmod 600 .deploy/nas/.env

# 3. 数据目录必须先存在并属于 1000
ssh root@10.0.30.5 'mkdir -p /srv/ac-controller/data && chown -R 1000:1000 /srv/ac-controller/data'

# 4. 上传并启动
scp .deploy/nas/{ac,Dockerfile,docker-compose.yml,.env} root@10.0.30.5:/srv/ac-controller/
ssh root@10.0.30.5 'cd /srv/ac-controller && docker compose up -d --build'

# 5. 验证
curl -s http://10.0.30.5:8090/api/v1/health
```

`Dockerfile` 里 `COPY --chmod=0755 ac /ac` 是必需的：二进制在 Windows 上编译，
落到宿主机是 644，`scp` 不会补可执行位，直接 COPY 会得到
`exec: /ac: permission denied`。

### 2.5 GHCR 私有镜像部署与更新（可选）

镜像 `ghcr.io/naoki66/openwrt_ac-controller:latest`（private）由仓库根的 `Dockerfile`
构建并推送（linux/amd64）。宿主能访问 ghcr.io 时，不必编译或传二进制，直接拉镜像：

**沿用 §2.3 的同一个目录与 `data/`**：AC 的 `ac.id` 与签名私钥就在这里，
换部署方式时不能换目录，否则所有 AP 都要重新 TOFU 适配。

```sh
/srv/ac-controller/
├── docker-compose.yml    # 替换为 deploy/fnos/docker-compose.yml
├── update-docker.sh      # 来自 deploy/fnos/update-docker.sh
├── .env                  # AC_SHARED_SECRET、AC_ADVERTISE_IP、AC_ADMIN_TOKEN、AC_NTP_SERVERS（可选）
├── .ghcr_pat             # read:packages 的 PAT，chmod 600，不入库
└── data/                 # 挂给容器 /var/lib/ac，属主 1000:1000
```

首次部署：

```sh
mkdir -p /srv/ac-controller/data
chown -R 1000:1000 /srv/ac-controller/data
# 从本机上传两个文件（覆盖原来的 compose）
scp deploy/fnos/{docker-compose.yml,update-docker.sh} \
  root@10.0.30.5:/srv/ac-controller/
# 在 NAS 上写密钥与 token
cd /srv/ac-controller
chmod +x update-docker.sh
printf 'AC_SHARED_SECRET=%s\n' \
  "$(openssl rand -base64 32)" > .env && chmod 600 .env
printf '%s' '<带 read:packages 的 PAT>' > .ghcr_pat && chmod 600 .ghcr_pat
./update-docker.sh
```

已有 `.env` 时不要覆盖：只需确认里面有 `AC_SHARED_SECRET`；
`AC_ADVERTISE_IP` 留空即可（NAS 多 IP 时按 AP 来源路由逐请求推导，不会把某一台
NAS 的地址硬塞给所有 AP）；`AC_ADMIN_TOKEN` 留空不影响页面使用——管理面始终要求
登录，首次启动用 `admin` / `123456` 进入后到「系统 → 账户」修改用户名或密码即可。
`AC_NTP_SERVERS` 缺省为 `ntp.ntsc.ac.cn,ntp1.aliyun.com,ntp.tencent.com,cn.pool.ntp.org`
（公共源默认，适配任意网段，控制器本身不在列表里）；如果站点有内网 NTP
（网关/NAS），在「系统 → 站点时间源（NTP）」里把它放到第一项。AP 会持久化该列表，
AC 离线后继续同步。

`update-docker.sh` 的行为：

1. 从 `GHCR_TOKEN` 或同目录 `.ghcr_pat` 登录 ghcr.io（失败会明确提示 token 问题）；
2. 记录当前容器版本与镜像 digest；
3. `compose pull`，digest 未变且容器在跑 → 直接退出，不重建（零停机）；
4. digest 变了先备份 `data/` 到 `data-backup-<时间戳>.tgz`
   （其中含 `ac.id` 与 `ac_signing.key`，私钥丢了要重新适配所有 AP）；
5. `compose up -d --force-recreate --remove-orphans`；
6. 最多等 20 秒宿主 `curl /api/v1/health`，失败则输出 40 行日志并以非 0 退出。

注意：

- 包默认私有，token 只需 `read:packages`；用完建议换成只读 token。
- **NAS 无外网时不要走这条路径**（`goproxy.cn` 都连不上），回到 §2.4 的离线构建。
- `network_mode: host` 不变，AP 发现仍靠 UDP 广播；host 模式监听 0.0.0.0，
  不绑定某一台 NAS 的 IP，只占用 8090/29820 端口。`AC_ADVERTISE_IP` 留空即可。
- 回滚镜像：把 `.env` 的 `AC_IMAGE` 指到某个 commit tag
  （如 `ghcr.io/naoki66/openwrt_ac-controller:94d346e`）后 `docker compose up -d`。

## 3. AP 侧 agent

### 3.1 安装

```sh
# 交叉编译（两台是同一个架构）
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
  -ldflags "-s -w -X github.com/naoki66/ac-controller/internal/version.Version=deploy-$(date +%Y%m%d)" \
  -o .deploy/ac-agent ./cmd/acagent

# OpenWrt 没有 install(1)，用 cp + chmod
scp .deploy/ac-agent root@10.0.30.251:/tmp/ac-agent
ssh root@10.0.30.251 'cp /tmp/ac-agent /usr/sbin/ac-agent && chmod 0755 /usr/sbin/ac-agent && rm -f /tmp/ac-agent'

# 服务与配置
scp deploy/openwrt/ac-agent/files/ac-agent.init root@10.0.30.251:/etc/init.d/ac-agent
ssh root@10.0.30.251 'chmod 0755 /etc/init.d/ac-agent'
```

### 3.2 配置 `/etc/config/ac`

```sh
config agent 'agent'
	option enabled '1'
	option ac_addr '10.0.30.5:8090'
	option secret '<AC_SHARED_SECRET>'
	option interval '20'
	option peer_network 'lan'
	option peer_secret '<PEER_SHARED_SECRET>'
	option peer_steer '1'
```

`peer_secret` 必须与站点内所有 AP 一致，且不要复用 `AC_SHARED_SECRET`；留空表示 AP 间
报文不认证，只适用于可信管理网段。首次接入先用命令行演练：`ac-agent once --dry-run -v`，
它只渲染并暂存到 `/etc/ac/staging/rev<N>/`，不写 `/etc/config`、不 reload；
`dry_run` 已不再是 UCI 选项。

```sh
ssh root@10.0.30.251 '/etc/init.d/ac-agent enable && /etc/init.d/ac-agent start'
```

### 3.3 状态

```sh
/etc/init.d/ac-agent status                 # running
logread | grep ac-agent | tail              # 心跳与 apply 记录
ac-agent status                             # 人读摘要（state / ac_addr / applied_rev）
ac-agent status --json                      # 与 LuCI 读的同一份结构
cat /var/run/ac-agent/status.json           # 每轮心跳重写的运行时快照
```

`status.json` 里 `state` 是控制器关系的判据：`discovering`（没地址/没应答）、
`unreachable`（地址在但不通）、`connected`（通了但未被采纳）、`managed`（已采纳并下发过 rev）。
`ac_addr_source` 区分地址是手填（`config`，跨三层时必然如此）还是广播发现（`discovered`）。

LuCI 界面见 [`ac-luci-app.md`](ac-luci-app.md)：**LuCI → 控制 → AC 纳管**
（`/cgi-bin/luci/admin/control/ac-agent`）。

## 4. 模板策略：全接管（无线 + 网络）

### 4.1 为什么一开始只接管无线

现网 `network` 里 `config device`（br-lan）是**匿名段**，而渲染器在生产前缀下会
生成 `config device 'ac_bridge'` + `option name 'br-lan'` —— 两个段定义同一个
网桥。加上两台 AP 的 VLAN 端口映射本来就不同（`.250` 的 lan2/lan3 是反的、
且没有 3169）、管理 IP 不同，共用一份 network 模板是不可能的。

所以第一阶段模板的 `bridge` 是全空的：

```json
"bridge": {"name": "", "ports": null, "vlans": null, "interfaces": null}
```

`render.Network` 输出 0 字节，`Apply` 里 `len(network) > 0` 不成立，整个 network
文件不被触碰（真机日志确认 `files=[wireless]`）。

### 4.2 引入设备级参数后改为全接管

差异被外置到 **设备级参数**（`PUT /api/v1/devices/{mac}/network`），模板只写
"机队共性"，每台的三样东西（端口映射、VLAN 端口归属、管理 IP）单独给。
两台现已统一到 `ap-xr1710g-full`。

`.250` 用的参数（与 `.251` 只有 IP 末位不同），也存了一份在
`docs/baseline/ap250/device-network.json`：

```json
{
  "bridge_ports": ["lan2", "lan3", "lan4", "wan"],
  "vlans": [
    {"vlan": 1, "ports": ["wan:u*"]},
    {"vlan": 2, "ports": ["lan2:u*", "wan:t"]},
    {"vlan": 3, "ports": ["lan3:u*", "wan:t"]},
    {"vlan": 4, "ports": ["wan:t"]},
    {"vlan": 5, "ports": ["wan:t"]},
    {"vlan": 6, "ports": ["wan:t"]},
    {"vlan": 3169, "ports": ["lan4:u*", "wan:t"]}
  ],
  "ipaddr": {"lan": ["10.0.10.250/24"], "lan2": ["10.0.20.250"], ...},
  "igmp_snooping": true
}
```

换模板（无线 → 全接管）走 `POST /devices/{mac}/adopt` 带新 `template_id`：
`Adopt` 对已采纳的设备会用新模板重新生成并更新 `template_id` 与 rev。

> **顺序不能反：先 `PUT /network` 设参数，再 `adopt` 换模板。**
> 模板默认不含 `ipaddr`，若先换模板，会渲染出**没有管理地址**的接口，
> agent 一旦应用就把自己锁在外面（`.251` 曾因此整机失联）。
> 反过来不会出错，因为 `PUT /network` 只做存储、不校验模板。

换模板前确认无线部分一致，否则客户端会被改配置：本次 `ap-xr1710g-wifi` 与
`ap-xr1710g-full` 的 18 个 SSID 与 3 个 radio **逐字段相同**，所以切换只增加了
network，无线零变化。

### 4.2.1 解绑后重新采纳：必须指定模板

`POST /devices/{mac}/release` 只清 `state` 与 `template_id`，**刻意保留
`network_params`**——它记录的是这台机器的物理接线与自有管理地址，不是"上一次推了什么"。
副作用是：重新采纳时必须用**接口集合与覆盖匹配的模板**，否则 `provision` 会直接拒绝：

```
provision: device 00:00:00:00:00:21 network_params address [lan lan2 lan3 lan4 lan5 lan6]:
no such interface in template "default"
```

这不是故障，而是保护：模板不定义 `lan` 时，覆盖里的管理地址无处落地，渲染出的
network 会让 AP 失联。**看到这条错误先检查 `template_id`，不要去清覆盖。**

Web UI 的"采纳"曾硬编码 `template_id: 'default'`（占位示例模板），解绑后再点采纳必然
撞上这条错误。现已改为设备行内的模板下拉，默认值依次取"上次记住的选择 → 在网已采纳
设备用得最多的模板 → 第一个非 `default` 模板"，并把 `template_id` 显示在设备状态列。

命令行等价写法：

```sh
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"template_id":"ap-xr1710g-full"}' \
  http://10.0.30.5:8090/api/v1/devices/00:00:00:00:00:21/adopt
```

### 4.2.2 从全接管退回只接管无线

`ap-xr1710g-wifi` 的 `bridge` 为空，不渲染 network 文件，AP 保留本地二层配置。
此时设备上为 `ap-xr1710g-full` 写下的设备级覆盖（`lan..lan6` 的地址与端口表）
**会被忽略而不是报错**：照它渲染会凭空生成模板没要求的 bridge 段，
照它校验则把"退回只接管无线"这条路堵死。判据是
`model.Template.ManagesNetwork()`——模板声明了 bridge 端口、VLAN 或接口
三者之一才算管理二层，覆盖只在该判据成立时参与渲染与校验。

覆盖本身保留在设备上：那台机器的接线和地址没变，之后切回全接管
（`adopt` 带 `ap-xr1710g-full`）时不需要重录。回归测试见
`internal/provision/network_test.go` 的 `TestWirelessOnlyTemplateIgnoresDeviceNetwork`。

### 4.3 跨三层 / WAN 上游部署

**发现阶段是纯二层广播，跨不了三层；接入阶段是纯 HTTP，三层没问题。**

- `Discover()` 向 `255.255.255.255:29820` 与各接口定向广播发 `AP_DISCOVER`，
  等 3 秒。路由器不转发定向广播 → **零配置自动发现在跨三层时必然失败**。
- 但只要给了 `ac_addr`，`New()` 会直接建 HTTP client，`Step()` 里
  `if a.client == nil` 不再成立 —— **一次广播都不发**。
  真机实测：配了 `ac_addr` 后 `tcpdump -ni any udp port 29820` 抓 8 秒，0 个包。

所以 AP 初始化在 WAN 口上游、AC 在另一个网段是**可行的**：

```sh
uci set ac.agent.ac_addr='10.0.10.5:8090' && uci commit ac
```

也就是 `/etc/config/ac` 里的 `option ac_addr`，或命令行 `--ac`。前提是 TCP 8090 可达。

两个注意点：

- `AC_OFFER.ac_ip` 是控制器用 `DialUDP` 到 peer 推导的本地地址，**跨 NAT 会推错**；
  跨三层部署必须显式配 `AC_ADVERTISE_IP`。
- TOFU 钉的是 `pubkey + ac_id`，与 IP 无关，换网段不会触发重新信任。

尚未实现的"自动跨三层发现"：DHCP Option 下发 AC 地址（类似 CAPWAP Option 43）、
固定域名 / DNS SRV、或把 `broadcastTargets()` 扩展成"先试预设单播再广播"。

## 5. 首次接入顺序（顺序错了会以为功能坏了）

1. agent 启动 → hello 注册，设备进入 `pending`。
2. 采纳（`POST /devices/{mac}/adopt`）→ 生成 rev1。**这个包没有邻居列表**，
   因为此时控制器还不知道这台 AP 有哪些接口名和 SSID。
3. agent 应用 rev1 → 上报自己的 11k 自我描述。
4. **再 repush 一次** → 生成 rev2，这次带上 `neighbors` 文件。
5. AP 应用 rev2，11k 列表落地。

第 4 步不能省，两台都要做。`TestPackageCarriesPeerNeighborLists` 把这个约束固化
成了断言，`docs/ac-11k-neighbors.md` §6 限制 2 有同样说明。

## 6. 回滚

agent 在 apply 前自动备份到 `/etc/ac/backup/rev<N>-<epoch>/`，健康检查不过会自动
回滚（真机已验证：一份被误判的配置在 15 秒后被完整还原）。

手工回滚：

```sh
ls /etc/ac/backup/
cp /etc/ac/backup/rev2-<epoch>/wireless /etc/config/wireless
cp /etc/ac/backup/rev2-<epoch>/network  /etc/config/network   # 若备份里有
wifi reload
```

彻底脱离控制器：删 `/etc/ac/state.json`（TOFU 钉子），然后手工恢复
`/etc/config/wireless`。本地的 `wifinet*` / `radio*` 段只是被 `disabled '1'`，
没有删除，把 `ac_` 段删掉、去掉 disabled 即可。

## 7. 踩过的坑（都会静默失败）

| 现象 | 原因 |
|---|---|
| 容器 `exec: /ac: permission denied` | Windows 编译产物无执行位，需 `COPY --chmod=0755` |
| 容器起不来但 compose 说 Started | 基础镜像拉不动；NAS 无外网，必须用 scratch + 预编译 |
| apply 后回滚，日志说 7 个 BSS absent | 健康检查把 `disabled` 的 SSID 也算进必须 up 的集合 |
| apply 成功但 11k 列表全空 | 设置在 `wifi reload` 之后仍太早，hostapd 还在重建；改为健康检查通过后 + 回读重试 |
| 服务 running，但每 20 秒 401 | `procd_set_param env` 连写两次，第二次覆盖第一次，`AC_SECRET` 被冲掉 |
| init 脚本读不到 `ac_addr` | `config_load` + `config_get` 静默返回空，改用 `uci -q get` |
| 拉私有镜像 401 / denied | PAT 过期、缺 `read:packages`，或包的所有者与 `GHCR_USERNAME` 不一致 |
| `update-docker.sh` 报 missing AC_SHARED_SECRET | `.env` 不在脚本同目录，或该行被注释 / 拼错 |
| 升级后 AP 上出现两套 `wifi-device` | 段名改过（`ac_radio0..2` → `radio0..2`），opkg 升级不清理 UCI 里的旧段；`rev` 未变所以控制器看不出。见 §8「升级后遗症」 |
| 射频页面出现 `up=false` 的幽灵条目 | 旧版控制器：`LatestRadios` 按 `(ap_mac, radio)` 取最新一条且无时间窗，历史用过的 radio 名一直保留到裁剪。现版改为 `radio_state` 按 hello 全量收敛，升级后第一个心跳即清除旧名 |

共同点：**没有一条会让进程退出**。所以"服务在跑"不等于"在工作"，验收必须看控制器
侧的心跳时间、`applied_rev` 和 `sync_state`。

## 8. 日常运维

```sh
# 改模板后重新下发（一台）
# 不带 body：repush 用的是设备当前已采纳的模板，请求体被忽略。
curl -s -X POST -H "Authorization: Bearer $SECRET" \
  http://10.0.30.5:8090/api/v1/devices/<mac>/repush

# 看设备状态
curl -s http://10.0.30.5:8090/api/v1/devices

# 看客户端（信号/速率/IP）
curl -s http://10.0.30.5:8090/api/v1/clients

# 11k 现状（只读）
./scripts/verify-11k.sh
AC_AP_HOST=10.0.30.250 ./scripts/verify-11k.sh
```

升级 agent：重新编译 → `cp` 到 `/usr/sbin/ac-agent` → `/etc/init.d/ac-agent restart`。
`state.json` 保留，不会重新 TOFU。

每台新接入的 AP 上先执行（否则开机可能无法恢复无线，见 `docs/ac-boot-wireless.md`）：

```sh
/etc/init.d/wifi_schedule disable
```

### 升级后遗症：旧段名残留

段名改了（`ac_radio0..2` → `radio0..2`）之后，已经在跑的 AP **不会自己跟上**：
opkg 升级只换二进制，不碰 `/etc/config/wireless`；控制器侧的 `rev` 也没变，所以
agent 认为无事可做。结果是一台 AP 上有两套 `wifi-device`——旧的 `ac_radio*`
（`disabled=0`，真正在跑）和出厂默认的 `radio*`（`disabled=1`）。控制器完全看不出。

判据：

```sh
uci show wireless | grep '=wifi-device'   # 应该是 3 个，出现 6 个就是残留
```

修复用 **repush**，不要手工改 UCI：

```sh
curl -s -X POST http://10.0.30.5:8090/api/v1/devices/<mac>/repush
```

合并器对现有段里带 `ac_` 前缀的**无条件丢弃**（`ucimerge.Merge` 的
`if s.IsAC() { continue }`），`ac_radio*` 不在渲染产物里，所以下一次渲染就把它们
清掉了；`radio0..2` 则按「类型+名字」命中被就地替换成新渲染的版本（`disabled='0'`）。

动手前先把结果算出来，别在真机上试：用同一份 `network_params` 在本地渲染，再跑
一遍 `ucimerge.Merge`（Options 与 `apply.go` 完全一致），拿产物和另一位同模板、
已经正常的 AP 做 `diff`。**逐字节相同**才下发——这时"会不会锁死"已经不是猜测。

顺带两条与本操作相关的既成事实：

- **管理地址不依赖 wifi-device**。六条地址都在 `br-lan.1..6`（DSA bridge 的 VLAN
  子接口）上，BSS 全灭也不会失联。会掉地址的是 `config interface` 改名
  （firewall zone 按名字引用）；只动 `wifi-device` 不触发。
- 历史 `ac_radio*` 的遥测行会一直留在射频列表里（见 §7 最后一行）。它们可以删：

```sh
# WAL 库不能直接 cp 备份；用 sqlite3 的 backup() 做事务一致热备份，可不停机。
python3 - <<'PY'
import sqlite3, time
db = "/srv/DockerConfig/ac-controller/data/ac.db"
src = sqlite3.connect(db, timeout=20)
src.execute("PRAGMA busy_timeout=20000")
dst = sqlite3.connect(db + ".bak-" + time.strftime("%Y%m%d%H%M%S"))
with dst:
    src.backup(dst)
dst.close()
with src:
    print("deleted", src.execute(
        r"DELETE FROM radio_samples WHERE radio LIKE 'ac\_radio%' ESCAPE '\'").rowcount)
PY
```
