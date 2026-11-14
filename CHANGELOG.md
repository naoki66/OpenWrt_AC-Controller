# 修改记录 / Changelog

## v0.7.0

### 自动发现与采纳

- 未纳管或刚重置的 AP 通过 UDP 29820 广播型号、MAC、管理地址、设备公钥和 Agent 版本。AC 仅回复登记地址；AP 通过一次性挑战证明自己持有对应私钥后，AC 才创建或更新设备记录。广播里没有私钥或纳管密钥。
- 管理员在 AC 点「采纳」即可接入。每个 AC 部署首次启动时自动生成一把纳管密钥并保存在 SQLite，向该 AC 已采纳的所有 AP 加密下发同一把密钥。保留 AC 数据目录即可沿用密钥。绑定后 AP 改发认证心跳；需要重新定位 AC 时，只查找已经绑定的 AC。

### 重置后重新发现

- AP 的 LuCI 新增「重置纳管」。重置只清除 AC 绑定，保留当前 Wi‑Fi、网络和 AP 设备身份，并递增持久化的重置代次，随后恢复待采纳广播。不需要事先通知原 AC。
- 原 AC 验证同一设备身份和更高代次后，将同一条设备记录转回「待采纳」，清理旧的应用与升级状态，让待执行命令失效，并记一条「重置纳管」事件。配置版本历史保留，管理员须重新点击「采纳」。
- 旧代次的登记、同代次的重复登记，以及重置前已经开始但尚未完成的采纳，都不能覆盖新的绑定状态。

### 升级说明

- 本次协议版本为 9；请配套升级 AC、`ac-agent` 和 LuCI，保留 AC 数据目录、AP 的 `/etc/ac/state.json` 和 `/etc/ac/enrollment.key`。旧的自动纳管记录按代次 0 读取；升级本身不会触发重置。原有手填共享密钥模式仍可继续运行。
- 软件包版本：`ac-controller 0.2.0-1`、`ac-agent 0.3.0-1`、`luci-app-ac-agent 1.0.0-6`。
- 原 AC 必须能收到 AP 的新登记才会更新记录；AP 若已移到原 AC 不可达的网络，原记录仍显示离线。

### English

- Unbound and reset APs broadcast public device metadata. The AC changes enrollment state only after a fresh proof of identity. Adoption delivers the same persistent deployment key to every approved AP on that AC.
- LuCI **Reset management** preserves Wi-Fi, networking and AP identity. A verified newer reset returns the existing record to **Pending** on any AC that receives it, expires queued commands and requires fresh approval. Stale registrations and approvals cannot replace the new binding.
- Update the AC, agent and LuCI together. Keep the AC data directory and the AP's enrollment state and identity key. Manual shared-secret deployments remain supported.

### 下载 / Downloads

- AC 容器镜像：`naoki66/openwrt_ac-controller:v0.7.0` 或 `ghcr.io/naoki66/openwrt_ac-controller:v0.7.0`。
- AP 安装本次 Release 的 `ac-agent` 和 `luci-app-ac-agent` 软件包；中文界面另装 `luci-i18n-ac-agent-zh-cn`。用 `SHA256SUMS` 校验，构建信息见 `build-info.txt`。

Earlier releases / 历史版本：[GitHub Releases](https://github.com/naoki66/OpenWrt_AC-Controller/releases).
