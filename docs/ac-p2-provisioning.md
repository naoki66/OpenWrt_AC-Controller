# P2：采纳与配置下发

> 状态：控制器侧已完成并验证（2026-10-31）。AP 侧 agent 未实现，因此"推送到 AP 并生效"尚无闭环。
> 上游设计：`docs/ac-ap-controller-design.md` §5 D3、§6、§10 P2。

---

## 1. 已实现的数据流

```
POST /devices/{mac}/adopt          模板 → 渲染 → 打包 → Ed25519 签名 → 存库
        │
        ├─ render.Wireless()      完整 /etc/config/wireless（ac_ 前缀 section）
        ├─ render.Network()       设备 + 接口片段（br-lan.<vid> 模型）
        ├─ bundle.Build()         manifest.json + tar.gz，签名覆盖 manifest
        └─ store.CreatePackage()  按 (mac, rev) 不可变留存

GET  /devices/{mac}/package       原样返回该 rev 的字节，附 X-Ac-Rev / X-Ac-Sha256
GET  /ac/pubkey                   AP 侧验签所需的公钥
```

采纳（adopt）与重新下发（repush）的区别只在设备状态：rev 计数器单调递增、永不复用，
解绑（release）只把状态回退到 `pending` 并清空 `template_id`，**不清零 rev**。
这样 AP 永远不会看到两个内容不同却同号的包。

## 2. 配置包格式

```
acpkg-<mac>-rev<N>.tar.gz
├── manifest.json      rev / device_mac / ac_id / template_id / generated_at
│                      model_whitelist / files{name→sha256} / signature
├── wireless          完整 /etc/config/wireless
└── network           片段，仅 ac_ 相关 device + interface section
```

签名与校验的关系：

- 签名消息 = `manifest.json` 去掉 `signature` 字段后的 JSON 字节（字段顺序固定、map key 按字典序）；
- `manifest.files` 记录每个 payload 文件的 sha256；
- 因此**只需公钥**即可校验全部内容，不需要额外分发摘要。

`bundle.Extract` 额外做三条防御：

1. 归档里出现 manifest 未列出的文件 → 拒绝（防止夹带未签名的 `apply.sh`）；
2. manifest 列出了但归档里缺失 → 拒绝；
3. 只接受普通文件，符号链接/设备节点/tar 路径穿越（`../`）一律拒绝。

## 3. 渲染约定

> 2026-10-31 按现网实测修正：网络模型已从"软件 802.1q 子接口"改为 DSA `bridge-vlan`，
> htmode 支持 EHT。差异细节见 [`ac-p0-baseline.md`](ac-p0-baseline.md)。

| 项 | 约定 | 理由 |
|---|---|---|
| section 前缀 | BSS / bridge-vlan / bridge device 用 `ac_`；`wifi-device` 与 `config interface` 不加 | AP 侧 agent 可整段替换而不碰本地手配；后两类名字被段外引用（见下两行） |
| radio 命名 | `radio<index>`（**不加前缀**），`index` 见 `option radio` | BSS 的 `option device`、LuCI 射频页、原厂配置都叫这个名字；MT7996 三个 phy 共用一条 PCIe，只给 path 无法区分，仍靠 `option radio` 定 phy |
| BSS 命名 | `ac_ssid_<slug>_<band>` | slug 冲突时追加 `_2`，避免静默覆盖；段外无人引用，可安全加前缀 |
| bridge-vlan 命名 | `ac_bvlan<vid>` | **不能叫 `ac_vlan<vid>`**：UCI 段名全文件唯一，会和同 VLAN 的 interface 撞名 |
| interface 命名 | 由模板给出（`ac_vlan20` 等），不加前缀 | SSID 的 `option network` 要引用它，站点也要按名字寻址 |
| 排序 | band → network → ssid | 输出与提交顺序无关，同一模板恒得同一字节 |
| 网络模型 | `config bridge-vlan` + 每口 PVID + `config interface`（`device 'br-lan.<vid>'`） | 现网 `vlan_filtering=1`，再叠软件 802.1q 会双重打标 |
| 加密 | wpa2→`psk2+ccmp`、wpa3→`sae+ccmp`、wpa2/wpa3→`sae-mixed`、owe→`owe` | 显式写 ccmp，避免由别的包决定默认值 |
| 802.11r | 开启时下发 `ft_over_ds`（现网为 0）与每 VLAN 的 `mobility_domain` | 同 VLAN 的 5G/6G 用同一 MD 才能跨频段 FT |
| htmode | 2g→HE20/HE40；5g→EHT20…EHT160；6g→EHT20…EHT320 | 现网 5G 跑 EHT160、6G 跑 EHT320 |

> 2026-11-01：`wifi-device` 段名由 `ac_radio<index>` 改回原生的 `radio<index>`。
> 所有权不再依赖前缀——`ucimerge.Merge` 按「类型+名字」就地替换，渲染集声明了
> `radio0` 就替换本地的 `radio0`，上一版遗留的 `ac_radio*` 仍被 `IsAC()` 识别并清除。
> 首次升级到该命名后 apply 一次，段名变化会触发 `wifi reload` 全量重建 BSS，
> 客户端断开一次（约 15–60 秒），需在低峰执行。迁移断言见
> `internal/render/render_test.go` 的 `TestMigrationRetiresPrefixedRadios`。

渲染器不输出任何时间戳或注释，这是"渲染结果 == 黄金基线"可逐项比对的前提。

**验收结果**：用 `docs/baseline/ap251/template.json` 渲染，与现网 dump 逐段比对，
wireless（3 radio + 18 BSS）与 network（1 bridge + 7 bridge-vlan + 6 interface）**零差异**
（`internal/render/parity_test.go`）。比对只做一处归一化：`option ipaddr X` 与
`list ipaddr X` 视为等价，因为 netifd 语义相同而现网两种写法混用。

## 4. API 合同

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/health` | 状态、`ac_id`、设备数、`pubkey` |
| GET | `/api/v1/ac/pubkey` | 公钥（JSON；`?format=text` 返回纯文本） |
| POST | `/api/v1/ap/hello` | **AP 面向**：注册 + 上报 + 取期望版本，需 Bearer |
| GET | `/api/v1/devices` | 设备列表（含 `state` / `template_id` / `config_rev` / `applied_rev` / `sync_state`） |
| GET | `/api/v1/devices/{mac}` | 单台设备 |
| POST | `/api/v1/devices/{mac}/adopt` | body `{"template_id":"..."}`，空 body 用 `default` |
| POST | `/api/v1/devices/{mac}/repush` | 按当前模板重新生成下一个 rev |
| POST | `/api/v1/devices/{mac}/release` | 回到 `pending`，保留 rev 计数 |
| GET | `/api/v1/devices/{mac}/package` | `?rev=N` 指定版本，缺省为最新 |
| GET | `/api/v1/devices/{mac}/packages` | 版本元数据（不含 blob） |
| GET | `/api/v1/templates` | 模板列表 |
| GET | `/api/v1/templates/{id}` | 单个模板 |
| POST | `/api/v1/templates` | 新建/覆盖模板，校验不过返回 400 |

错误映射：MAC 非法 → 400；设备/模板不存在 → 404；未采纳就 repush、模板 id 未知 → 400；
模板校验失败（渲染期）→ 422。

## 5. CLI

```sh
ac pubkey                 # 打印公钥 hex
ac verify pkg.tar.gz      # 复验签名与逐文件 sha256，模拟 AP 侧检查
```

`ac verify` 读取 `AC_DATA_DIR` 下的私钥推出公钥；AP 侧应改用 `/api/v1/ac/pubkey`
拿到的公钥，二者等价。

## 6. 已知缺口（进入 P3 前必须处理）

| 缺口 | 影响 | 建议 |
|---|---|---|
| **管理 API 无鉴权** | 管理网内任何人都能采纳设备、下载配置包（含 PSK 明文） | P3 加会话认证；部署时至少用反向代理 + 白名单 |
| 配置包无设备绑定令牌 | 任一 AP 可下载其他 AP 的包 | 注册后下发 per-device token，package 端点校验 |
| manifest 无有效期 | 旧 rev 可被重放 | 加 `not_after`，AP 侧拒绝过期包 |
| 私钥与数据库同目录 | 拿到数据卷即可伪造配置包 | 生产建议私钥外置（挂载独立卷或 KMS） |
| HTTPS 未启用 | 公钥首次分发可被中间人替换 | AP 首次注册时人工核对指纹，之后固定 |

> AP 面向的 `/api/v1/ap/hello` 已强制 `AC_SHARED_SECRET`（Bearer），
> 但配置包下载端点仍在同一信任域内、未做设备级鉴权。

## 7. AP 侧 agent

已实现，见 [`ac-agent.md`](ac-agent.md)。与本节最初设想的对应关系：

| 设计 §5 D3 步骤 | 实现位置 |
|---|---|
| 1. 拉取 → 验签 + sha256 | `internal/bundle.Extract`（AP 与 AC 共用同一份实现，语义不会漂移） |
| 2. 备份 `/etc/config` | `Applier.backup`，按 `rev<N>-<ts>` 落 `/etc/ac/backup/`，缺失文件也记录 |
| 3. 写入 + `uci commit` | `ucimerge.Merge` 段级合并 + 原子写；只替换 `ac_` 段，不用 `uci commit`（直接写文件） |
| 4. `reload_config` → `wifi reload` | `Applier.reload`：仅 network 变更才 `ubus call network reload`，`wifi reload` 只一次 |
| 5. 健康检查 / 回滚 | `waitForBSS`（ubus status，默认 15 s）+ `Applier.rollback` |
| 6. 上报 `applied_rev` | `proto.Hello.SyncState`，结果先落 `state.json` 再上报，掉线不丢 |

与原设计的两处偏差，都是有意的：

1. **配置包里的 `network` 不做整文件替换**，改成段级合并。原设计的"完整 `/etc/config/wireless` + network 片段"在 network 上仍然要求 AP 侧有合并能力，否则上行口会丢。
2. **额外强制关闭 dnsmasq/odhcpd**。原设计只在 §2.3 记录了"现网已关闭"这一事实，但工厂复位后的 AP 会重新启动 DHCP 服务，必须在 apply 时强制收敛。

