# AP 侧 LuCI 应用：AC 纳管

> 2026-11-01 新增。代码在 `deploy/openwrt/luci-app-ac-agent/`，结构参照
> `luci-app-airoha-npu`（同一台设备上的既有应用）：视图 + rpcd 后端 + po 三件套。
> 目标：运维站在 AP 前面就能回答一个问题——**这台 AP 现在能不能被控制器管住**，
> 不能的话卡在哪一步。

## 1. 为什么需要它

控制器界面只显示"我这边看到的 AP"。它看不到三种情况：

- agent 进程挂了，但控制器仍显示最后一次心跳的数据；
- AP 配了 `ac_addr` 却根本连不上（跨三层时尤其常见，广播发现过不去）；
- 连上了但从未被采纳，所以一个配置包都没下发。

这三种在控制器侧长得都很像"设备离线"。LuCI 页面的作用是把它们区分开。

## 2. 状态怎么来的

页面不读文件、不执行命令，只调一个 rpcd 后端 `luci.ac_agent`：

| 方法 | 作用 | 数据来源 |
|---|---|---|
| `status` | 返回 `{status, running, autostart}` | `status.json` + `ubus service list` + `init.d enabled` |
| `restart` | 重启 agent（等价于立即跑一轮同步） | `/etc/init.d/ac-agent restart` |
| `applyEnabled` | 按配置里的 `enabled` 启停 agent，并增删 WAN 放行规则 | `/etc/config/ac` + UCI firewall |

`status.json` 由 agent 每轮心跳重写（默认 20s），见 `internal/agent/status.go`；
`/var` 在 OpenWrt 上是 `/tmp` 的软链，tmpfs，重启自然清空，不会留下上一代固件的假状态。

单看文件不够：进程崩溃后文件会留在原处。所以判据是「进程在跑 **且** 文件在三个心跳内
更新过」，否则显示"agent 未响应"。这个合并放在后端做，只做一次。

### 状态机

agent 侧（`Status.State`）：

| 值 | 含义 |
|---|---|
| `starting` | 进程刚起，第一轮还没跑完 |
| `discovering` | 没有控制器地址，正在（或等待）广播发现 |
| `unreachable` | 有地址，但最近一次 hello 失败 |
| `connected` | hello 成功，设备**尚未被采纳** |
| `managed` | 已采纳，且 AP 上有一个控制器下发的 rev |

页面侧额外派生两个（`stale` / `stopped`），由 `running` 与 `updated_at` 判定。

`adopted` 是粘性的：只要 `applied_rev > 0` 就算已采纳。控制器数据库被还原或重装后
可能把设备重新报成 `pending`，此时显示"等待采纳"会误导——AP 跑着的仍是控制器渲染的配置。

## 3. 界面

页面顺序固定为：**AC 纳管标题 → 纳管状态 → 配置**。

**纳管状态卡**（每 10s 轮询 `status`）：

- 状态徽章 + 一句话原因（红/橙/绿）；
- 控制器地址与来源：`手动配置的地址` / `广播发现`；
- 控制器 ID、已生效/待应用 rev、最近心跳（含成功与否）、MAC/型号、上报 BSS 数、当前实际运行的引导守护、agent 版本与启动时间；
- 最近一次失败原因单独一行。

**按钮**：`重启 agent`。没有单独的"立即同步"：
`ac-agent once` 会与常驻进程争抢 `state.json`，而重启本来就立刻跑一轮 `Step()`。
开机自启由下面那个总开关统一管理，不再单独放按钮。

**表单**（写 `/etc/config/ac` 的 `agent` 段）：

| 选项 | 作用 |
|---|---|
| `enabled` | **启用**总开关，缺省开。开启 = 启动 agent、设为开机自启，并在 WAN zone 放行 TCP 8090、UDP 29820/29821；关闭 = 停止 agent、取消自启并删除这三条规则。打开页面或保存时都会按它对齐实际状态 |
| `ac_addr` | 控制器 `host:port`；留空 = 二层广播发现。**AC 在 WAN 上游时必须填**，广播跨不了三层 |
| `secret` | 与控制器 `AC_SHARED_SECRET` 一致 |
| `interval` | 心跳秒数，缺省 20 |
| `peer_port` / `peer_interval` | AP 间协同端口与播报间隔 |
| `peer_network` | 承载 AP 间流量的逻辑网络名（缺省 `lan`），用于 IPv6 link-local 组播绑定 |
| `peer_secret` | AP 间 HMAC-SHA256 共享密钥；所有 AP 一致，留空仅限可信管理网段 |
| `peer_steer` | `peer_steer` 缺省开，控制器失联后由 agent 内置的 11v 建议接管，不依赖 nrsyncd |

页面不再提供 `dry_run` / `steering_mode` / `steering_fallback` 三个选项：
演练只保留命令行 `ac-agent once --dry-run`；引导策略固定为“控制器优先”，失联后不切换守护进程，
由 29821 的 IPv6 组播（失败回退 IPv4 广播）AP 间协同与内置 11v 建议继续工作。

保存后走 `uci.apply()`，触发 init 脚本里注册的 `procd_add_reload_trigger ac`，
服务重启；随后页面调 `applyEnabled`，按刚保存的开关启动/停止 agent 并增删 WAN 规则。
WAN 规则用具名 UCI firewall 段（`ac_agent_wan_*`），只有内容变化时才 commit + reload，
普通打开页面不会反复重载防火墙。

## 4. 文件与安装

```
deploy/openwrt/luci-app-ac-agent/
├── Makefile                                          # luci feed 应用（显式 install + postinst/prerm）
├── htdocs/luci-static/resources/view/ac-agent/overview.js
├── root/usr/libexec/rpcd/luci.ac_agent               # rpcd 后端（shell）
├── root/usr/share/luci/menu.d/luci-app-ac-agent.json
├── root/usr/share/rpcd/acl.d/luci-app-ac-agent.json
├── po/zh_Hans/luci-app-ac-agent.po                   # 英文 msgid → 中文
└── po/templates/luci-app-ac-agent.pot
deploy/openwrt/install-luci-app.sh                    # 直接部署到运行中的 AP
scripts/po2lmo.py                                     # 本机编译 .lmo（不依赖设备上的 po2lmo）
scripts/luci-view-harness.js                          # 离线渲染校验（不需要设备）
```

脚本方式（无需 buildroot）：

```sh
make agent                                  # 交叉编译 bin/ac-agent-linux-arm64
scp bin/ac-agent-linux-arm64 root@10.0.30.250:/usr/sbin/ac-agent
ssh root@10.0.30.250 'chmod +x /usr/sbin/ac-agent'
./deploy/openwrt/install-luci-app.sh 10.0.30.250
```

脚本会复制视图、菜单、ACL、rpcd 后端，**在本机**用 `scripts/po2lmo.py` 编译中文词条后
scp 上去，最后刷新缓存链。入口：**LuCI → 控制 → AC 纳管**，
URL 为 `/cgi-bin/luci/admin/control/ac-agent`。

> 菜单挂在 `admin/control` 而不是 `admin/services`：`admin/control` 是**一级菜单**，
> 需要在 menu.d 里自带一个 `"action": {"type": "firstchild", "recurse": true}` 的目录节点
> （`luci-base.json` 只预置了 status/system/services/nas/vpn/network 六个）。
> 只写 `admin/control/ac-agent` 而不声明父节点，子项会挂在一个无标题的隐式节点上，
> 菜单里看不见。

feed 方式：把 `luci-app-ac-agent/` 放进 `luci/applications/`（或本地 feed），menuconfig 选中即可。
包不依赖 `ac-agent`（仓库里没有该包），二进制按上面的方式放置。

### 部署后必须刷新缓存链

`luci.js?v=<luciversion>-<包库 mtime>`：只拷文件不改包库 mtime，浏览器会继续用旧 JS，
表现为"改了没生效"。`install-luci-app.sh` 已经做了这一段，手工部署时不要漏：

```sh
touch /lib/apk/db/installed 2>/dev/null || touch /usr/lib/opkg/status
rm -f /tmp/luci-indexcache.* ; rm -rf /tmp/luci-modulecache/
/etc/init.d/rpcd restart
```

## 5. 权限

`acl.d/luci-app-ac-agent.json` 授权到**方法级**，不授"随便执行这个二进制"：

- 读：`ubus luci.ac_agent.status`、`uci` 的 `ac` 与 `luci`；
- 写：`ubus luci.ac_agent.restart`、`.applyEnabled`、`ubus uci.apply`、`uci ac` 写。

## 6. 已知限制

- **跨三层只能手填地址**。发现是 UDP 29820 二层广播；给了 `ac_addr` 后 agent 连广播都不发。
- **换控制器要人工清 `/etc/ac/state.json`**：首次 hello 钉住 pubkey + ac_id（TOFU），变化即拒绝。
  页面不会替你做这件事，因为那是唯一能挡住中间人的一道锁。
- **中文词条靠本地编译的 `.lmo`**。`po/zh_Hans` 是源，`scripts/po2lmo.py` 产出字节兼容的
  `.lmo`；设备不需要 po2lmo（很多精简固件根本没有）。
- 页面不展示 `secret` 之外的选择性信息：任何能登录 LuCI 的账号本来就能改这个配置。
