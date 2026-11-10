# XR1710G Mesh 插件开发版

基于 naoki66/OpenWrt_AC-Controller，基线提交 `abf5013`，版本 `0.3.1-aimesh-probe.1`。
参考 AiMesh 的统一管理方式，重建节点管理和回程配置。当前没有实现华硕私有配对协议，与原厂 AiMesh 节点的混组能力未验证。

AiMesh 私有协议适配进度和诊断命令见 [AIMESH_PROTOCOL.md](AIMESH_PROTOCOL.md)。本版增加被动诊断工具，尚不能与原厂华硕配对入网。

## 本版可用功能

- 原项目的节点发现、采纳、统一 SSID、802.11k/v/r、签名配置下发与回滚。
- 有线回程 / SAE 加密的 802.11s 无线回程；2.4 GHz 或 5 GHz，可共享频段或独占频段。
- Web「Mesh 组网」页设置回程、查看真实邻接节点与信号、区分过期上报。
- 无线变更后检测控制器可达性，失败恢复上一版 network/wireless。
- Mesh 桥开启 STP；主路由可保留 DHCP/DNS。

第一版仅支持单一、无 VLAN 的 LAN。暂未实现一键无线配对、自动选上级、自动有线优先策略、客网 VLAN 跨无线传输、6 GHz 回程。邻接链路不等于实际流量路径。STP 在目标驱动和多节点混合回程下的行为需要实测，不应据此认为无环或切换性能已经验证。

## 安装包

`dist/` 内：

| 文件 | 安装位置 |
| --- | --- |
| `ac-controller-0.2.1-r1.apk` | 一台主控 XR1710G，提供网页和配置服务 |
| `ac-agent-0.3.1-r1.apk` | 每一台参与 Mesh 的 XR1710G，主控如果也发射无线也要安装 |
| `SHA256SUMS` | 安装包、构建信息及源码增量校验 |

目标：使用 apk 的 OpenWrt / ImmortalWrt，`aarch64_cortex-a53`。二进制为 Linux ARM64 静态链接。需要 procd、ubus、uci、iw，以及支持 SAE Mesh 的 wpad（例如当前固件已有的 `wpad-mesh-mbedtls`）。此包不自动替换 wpad。

先复制安装包到路由器 `/tmp/`，保留网线连接：

```sh
# 核对目标与依赖
ubus call system board
apk info -e wpad-mesh-mbedtls
iw list
# iw list 需有 mesh point，并确认接口组合允许所选 AP + Mesh 模式。

# 每台节点安装；不需要使用 --force-overwrite
apk add --allow-untrusted /tmp/ac-agent-0.3.1-r1.apk

# 主控额外安装
apk add --allow-untrusted /tmp/ac-controller-0.2.1-r1.apk
```

新安装默认禁用服务；升级保留既有 UCI 配置。升级前备份 `/etc/config/ac`、`/etc/config/ac-controller`、`/etc/ac` 和 `/etc/ac-controller`。升级会停止旧服务，安装后按角色检查配置再启动。已修改的配置文件可能产生 `.apk-new`，请核对 `keep_dhcp` 选项。

## 主控设置

在主控执行，替换示例密钥：

```sh
uci set ac-controller.controller.enabled='1'
uci set ac-controller.controller.shared_secret='替换成至少12位的随机共享密钥'
uci commit ac-controller
/etc/init.d/ac-controller enable
/etc/init.d/ac-controller restart
```

从 LAN 打开 `http://主控IP:8090/`，初始 `admin / 123456`，进入系统配置更改密码。控制器原项目使用 HTTP；在可信管理 LAN 使用。插件不会新增 WAN 放行规则。

主控本机如果也运行 ac-agent：

```sh
uci set ac.agent.enabled='1'
uci set ac.agent.keep_dhcp='1'
uci set ac.agent.ac_addr='127.0.0.1:8090'
uci set ac.agent.secret='与主控相同的随机共享密钥'
uci commit ac
/etc/init.d/ac-agent enable
/etc/init.d/ac-agent restart
```

`keep_dhcp=1` 仅用于承担 DHCP/DNS 的主路由；否则原项目在配置应用后会关闭 DHCP/DNS。若主路由继续使用现有路由/NAT，模板必须保留它的 LAN 桥、地址和 WAN 端口归属。

## 卫星节点设置

先用网线接到同一 LAN。每台卫星执行：

```sh
uci set ac.agent.enabled='1'
uci set ac.agent.keep_dhcp='0'
uci set ac.agent.ac_addr='主控LAN地址:8090'
uci set ac.agent.secret='与主控相同的随机共享密钥'
uci commit ac
/etc/init.d/ac-agent enable
/etc/init.d/ac-agent restart
```

不要先拔线。802.11s 尚未建立前，agent 需要现有网络下载首次配置。

## 配置回程

1. 在无线服务页克隆一份模板。将启用 SSID 的网络统一为 `lan`。
2. 在端口页为模板明确配置单一 LAN 桥（例如 `br-lan`），关闭 VLAN。模板的 `lan` 接口设备直接为 `br-lan`。不要把主路由 WAN 放入 LAN 桥。
3. 在每台设备的网络覆盖里填写独立管理 IP；主控和卫星不能下发相同静态地址。卫星的默认网关/DNS 指向现有 LAN 网关。
4. 在射频页设置相同国家码、固定信道与频宽。建议从 5 GHz、合法非 DFS 信道、HE80 开始，再实测更宽频宽。国家码必须符合设备所在地；不要用 `auto` 信道。
5. 在 Mesh 组网页选择模板，设置 `802.11s`、频段、相同 Mesh ID、独立的 8–63 字节回程密钥以及 LAN 网络名。若独占 5 GHz，先把所有启用 SSID 的 5 GHz 频段去掉。
6. 保存后逐台预检和采纳/重新下发，等待同步成功。不同模板的节点也必须保持回程 ID、密钥、信道、频宽、国家码相同；逐台射频覆盖不得改变回程参数。
7. 页面看到已建立的无线链路后，逐台断开卫星网线验证连通性。主控必须持续连接上游网络。

此版本不自动修改 LAN 地址、创建网关或恢复旧主路由 DHCP；这些由设备现有网络和角色配置决定。

## 回退

恢复有线连接，在 Mesh 页切到有线并逐台重新下发，会移除控制器生成的 `ac_mesh_backhaul`。主控失联时 agent 的本地无线和原项目的 AP 间漫游协作继续运行；变更密钥/信道导致失联会触发应用回滚。整个站点同时换回程密钥或信道仍需分阶段迁移，本版没有协调迁移协议。

`/etc/ac/backup/` 保存配置应用前备份。删除软件包不会自动还原已下发的网络设置；需要恢复时使用原项目的备份恢复流程，避免仅卸载后留下错误配置。

## 编译与验证

需要 Go 1.26+、Python 3、fakeroot 和 OpenWrt apk-tools 3：

```sh
go test ./...
go vet ./...
node tools/check-mesh-ui.cjs
GO=/path/to/go APK=/path/to/openwrt/apk bash tools/build-mesh.sh
```

重新打包必须使用本地修改源码；原项目 feed 的 `PKG_SOURCE_URL` 仍指向上游，直接按上游 feed 下载不会包含本次修改。本目录的 `tools/build-mesh.sh` 使用当前源码，产出的是独立开发版 APK。

截至交付：已完成 Go 回归测试、签名下发集成测试、回滚测试、UI 脚本交互测试和 ARM64 静态二进制构建。尚未向路由器安装，也未完成双机射频、漫游、吞吐、重启及有线/无线混合环路验证。

上游 MIT 版权和 LICENSE 保留。`dist/mesh-source.patch` 加 `dist/mesh-new-files.tar.gz` 是相对于基线提交的源码增量。
