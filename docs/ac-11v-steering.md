# 11v 实时引导增强：调研与设计

> 状态：设计稿（2026-11-02）。现状见 `docs/ac-roaming-survey.md`、`docs/ac-11k-neighbors.md`。
> 结论先行：**实时引导的执行必须留在 AP（agent 本地毫秒级循环），AC 负责策略下发与全网视角**；
> 这与商用 AC 的实际做法一致——华为 WAC 也是"AP 采集判定 → AC 选目标 → AP 执行"。

## 1. 商用 AC 都做了什么

| 能力 | TP-Link Omada | 华为 WAC | Ubiquiti UniFi | ASUS |
|---|---|---|---|---|
| 11k 邻居表 | Dual Band 11k Report（可跨频下发） | AP 上报，AC 维护**终端邻居表**（每客户端对每个 AP 的 RSSI/RSNI） | 11k | - |
| 11v 引导 | BTM 建议（阈值+更优目标才发） | BTM 或强制下线，AP 临时**抑制重关联** | 软踢（BTM）/硬踢（deauth） | - |
| 弱信号踢除 | **Minimum RSSI**（按 SSID）+ Force-Disassociation | 粘性终端判定：持续低于阈值 + **连续 3 次**才触发 | Minimum RSSI | Roaming Assistant -70dBm 硬踢 |
| 防振荡 | 目标须显著更优 | 连续判定 + 老化 | hysteresis 5-10dB | - |
| 失败保护 | - | **"无法漫游"黑名单**：踢了不走的终端，一段时间内不再触发 | - | - |
| 负载均衡 | 按终端数/流量 | 按终端数/流量 | - | - |
| 频段引导 | Band Steering（偏 5/6G） | 频谱导航 | Band Steering | - |

关键调参经验（多源交叉验证）：阈值 -65 激进 / **-70 推荐** / -75 保守（IoT）/ -80 仅兜底；
目标 AP 比当前强 5–10dB 才值得踢；阈值应落在覆盖重叠区内偏弱的一侧。

## 2. 开源项目的做法与教训

| 项目 | 机制 | 现状/教训 |
|---|---|---|
| **DAWN** | umdns 对等交换，RSSI+信道负载打分，可拒绝 probe/auth/assoc 强推 | **已弃维护**，新 OpenWrt 上不可靠 |
| **usteer** | 邻居表同步 + hearing map + BTM；`signal_diff_threshold` / `min_snr` / `load_kick` | 口碑不稳（roam_kick_delay 失效、Intel 网卡无视 BTM）；社区共识"只建议不强踢"，踢类参数全关 |
| **nrsyncd** | 仅 11k 邻居表分发 | 最稳，但没有引导——本项目已用 AC 聚合替代 |
| **TIP OpenWiFi (uCentral)** | 云经 **WebSocket 实时下发**；AP 侧 `wifi-steering` 服务：`required-snr -75` / `required-probe-snr -70` / `required-roam-snr -85` / `load-kick-threshold 90%` / `assoc-steering` | 与我们最接近的架构参照：**策略云端可下发，执行在 AP 本地**，mode=local/cloud 可选 |

三条公认事实：
1. 漫游最终是**客户端的决定**——AP 能提示、能拒绝、能踢，但 iOS/Android/IoT 行为差异极大；
2. BTM 是"礼貌建议"，**大部分现代客户端尊重，部分无视**——所以商用产品都留了 Force-Disassociation 兜底；
3. 没有更优目标时踢人 = 制造断线风暴（usteer 用户血泪：min_snr 全关）。

## 3. 对本项目的启示

我们的架构（控制面集中、数据面留 AP、hello 20–30s 轮询）决定了：
- **AC 不可能做毫秒级实时引导**（轮询延迟不允许，也无长连接）；
- 但 AC 拥有**全网 BSS 视角**（11k 邻居表已由 AC 聚合）和**策略面**（steering 文件已是先例）；
- agent 有 ubus 通道，可以秒级本地循环。

因此正确的分层与华为/OpenWiFi 一致：

```
AC（策略面，随配置包/心跳下发）
  ├─ 引导策略：阈值、连判次数、BTM 模式、强踢时限、ban 时长、冷却
  ├─ 候选表：11k 邻居表（已实现，每 BSS 同 SSID 候选）
  └─ 仲裁：控制器优先（固定）；失联后由 AP 间协同 + 本地 BTM 接管，不切换守护进程
agent（执行面，本地 2–5s 循环，不依赖 hello 周期）
  ├─ 粘性判定：信号连续 N 次低于 required_snr
  ├─ BTM：携带 AC 下发的候选清单（disassoc_imminent 按策略）
  ├─ 强踢兜底：BTM 后 T 秒仍在原 AP → del_client + ban_time（抑制重关联）
  ├─ 黑名单：踢了不走的客户端记入冷却表（华为"无法漫游"保护）
  └─ 安全阀：无候选不踢、连接未满 X 秒不踢、usteer/dawn 在跑则让位
```

## 4. 方案：策略文件 `steer-policy`（v1）

### 4.1 AC 侧

模板新增（渲染进配置包，与 `steering` 文件并列）：

```json
{
  "btm_snr": -75,
  "btm_consecutive": 3,
  "btm_disassoc_imminent": false,
  "force_after_ms": 10000,
  "ban_ms": 30000,
  "min_connected_ms": 30000,
  "cooldown_ms": 120000
}
```

- 渲染为 `steer-policy` 文件（JSON）；模板缺省 = 不渲染 = agent 保持现状行为（零破坏）。
- 语义与 Omada 对齐：`btm_snr`+`btm_consecutive` = 智能漫游触发；`force_after_ms>0` =
  Force-Disassociation；`ban_ms` = 华为"抑制重关联"；`cooldown_ms` = "无法漫游"保护。
- AC Web UI（P3）后续在模板/设备页加策略编辑；设备级"立即踢除"按钮走 hello 捎带
  （延迟 ≤ 心跳周期，v1 可接受；真长连接是 P 后期的 WebSocket 通道）。

### 4.2 agent 侧

新 `internal/agent/steerloop.go`：

- 独立 ticker（默认 3s，指数退避到 10s），仅当：`steer-policy` 存在 && `steeringState()` 不是 usteer/dawn
  （`nrsyncd` 只分发 11k 邻居表，不参与切换决策，可与本地 BTM 并行；与 peer_steer 同一让位规则）&& 非 dry-run。
- 每轮 `ubus call hostapd.<if> get_clients` 采样；对低于 `btm_snr` 的客户端计连判次数；
  达标后从 `cachedNeighbors()` 取同 SSID 候选发 BTM（复用 `maybeSteer` 的候选裁剪逻辑）。
- `force_after_ms` 到期仍未走 → `ubus call hostapd.<if> del_client {"addr":…,"deauth":true,"ban_time":ban_ms}`。
- 黑名单/冷却表存内存（掉电即失效，宁可多提示一次也不永久屏蔽）；每表上限 256 条。
- 事件写入 hello 新字段 `steer_events`（计数摘要），AC 遥测可展示。

### 4.3 明确不做（v1）

- 负载均衡踢除 / 频段引导：需要 hearing map（probe 采集）或信道负载遥测，v2；
- AC 实时下发"踢某客户端"：受 hello 轮询限制，v1 只有策略下发 + 心跳捎带；
- probe/auth 阶段拒绝接入（assoc-steering）：对现状两台 AP 的场景收益低、风险高。

## 5. 验收口径

1. 策略包下发后 `ac-agent status` 能看到 policy 生效与本地循环启停；
2. 把测试客户端信号压到阈值以下，观测：连判达标 → BTM（抓包/日志）→ 客户端漫游或
   `force_after_ms` 后被 del_client 且 ban 期内无法重连原 AP；
3. 黑名单生效：踢不走的设备在 cooldown 内不再被反复踢；
4. usteer/dawn 任一启用时本地循环自动停（让位）；
5. 无 steer-policy 的包 = 行为与升级前逐字节一致（回归）。
