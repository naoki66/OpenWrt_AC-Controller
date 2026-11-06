# 参考：集客 AC（gecoosac / gecoos-ac）功能对照与数据模型

> 用途：本项目 `ac-controller` 的对照参考基线。两个容器（`tearsful/gecoosac` @ `:8891`、`adxing/gecoos-ac` @ `:8892`）部署于 NAS `10.0.30.5`，作功能/数据模型借鉴。
> 探测时间：2026-11-02。探测方式：Web UI 静态资源提取 + 后端二进制字符串分析 + 登录后实时抓包（`curl` 直连 `127.0.0.1`）。
> 注意：两个镜像**共用同一套前端**（Vue + ElementUI，构建哈希完全一致：`app.38f11b9e.js`、`chunk-elementUI.393c9e41.js`、`chunk-libs.06fa4dca.js`），后端均为 **Go**，数据持久化为 **LevelDB**（`data/data.db/` 下 `*.log`/`*.ldb`/`MANIFEST`/`CURRENT`）。二者是同一集客 AC 产品的不同打包源，API 契约一致。

---

## 1. 实时抓取证据（可复现）

| 项 | 值 |
|---|---|
| 前端入口 | `http://10.0.30.5:8891/`、`http://10.0.30.5:8892/` |
| API 基址 | `baseURL = "/api/"`（SPA 内 `app.38f11b9e.js`） |
| 登录 | `POST /api/sysauth` `{"username":"admin","password":"admin"}` → `{"msg":"LANG646","ret":0,"token":""}`（HTTP 200；`token` 为空，会话走 cookie） |
| 端点形态 | `/api/<action>`，部分 action 名自带 `ac/` 前缀（如 `ac/approxy`、`ac/apfirmware_del`） |
| 响应外壳 | 统一 `{ret:<int>, msg:<string/i18n键>, ...}`（`ret:0/1` 成功，`-99` 多表示无数据） |
| 数据层 | LevelDB（非 SQLite；本项目选 SQLite 无问题，仅说明其走 KV 存储） |

### 1.1 完整 API action 清单（从 SPA `url:"..."` 提取）

| 域 | action |
|---|---|
| 认证 | `sysauth`、`user/logout`、`updatepw` |
| AP 设备 | `apinfo`、`apsearch`、`apedit`、`del_ap`、`apcmd`、`apfirmware_list`、`apupgrade_url`、`ac/apfirmware_del`、`ac/apupgrade_reuse`、`ac/approxy` |
| 无线/模板 | `tempview`、`tempedit`、`tempdel`（SSID 模板；`enc_table` 在 `tempview` 返回） |
| 客户端/ACL/磁吸 | `macfilterview`、`macfilteredit`、`macfilterdel`、`stabindview`、`stabindedit`、`stabinddel`、`stasearch`、`hotspotsearch` |
| 系统 | `basicsetup`、`basicview`、`getrandom`、`oeminfo`、`somodeledit`、`somodelreq`、`somodeltempview`、`somodelview`、`selfoemtempdown` |
| 前端路由名（疑似同名校验 action） | `aplist`、`devicelist`、`template`、`dashboard` |

---

## 2. 实时抓取的数据模型（直接照抄）

> 字段名均为**真实 API 返回的 JSON key**。标 `interface{}` 或“结构待补”的字段当前为 `null`（因参考实例无真实 AP/未建模板），结构待接入真实设备后补全。

### 2.1 AP 状态/遥测 —— `GET /api/apinfo`

```go
type APInfo struct {
    Status struct {
        Com           string      `json:"com"`            // 备注/注释
        APFactory     string      `json:"ap_factory"`     // 出厂标识
        McloudSupport string      `json:"mcloud_support"` // 微云支持
        McloudURL     string      `json:"mcloud_url"`
        HotspotList   interface{} `json:"hotspotlist"`     // 热点列表（结构待补）
        CustomStatus  interface{} `json:"cusomStatus"`    // 注意：原串拼写 cusom（非 custom）
        DevType       string      `json:"devtype"`        // 设备类型
        ACCtrlType    string      `json:"ac_ctrltype"`    // 被控类型
        LEDStatus     string      `json:"ledstatus"`
        IP            string      `json:"ip"`
        IPv6          string      `json:"ipv6"`
        Stabind       int         `json:"stabind"`        // 是否吸附/绑定
        APDid         string      `json:"ap_did"`         // 注意原串 ap_did
        APCheck       string      `json:"ap_check"`
        Band24G       interface{} `json:"2.4g"`           // 每射频配置/状态（结构待补）
        PVersion      int         `json:"pversion"`
        MAC           string      `json:"mac"`
        AuthedList    interface{} `json:"authedlist"`      // 已认证终端列表
        Band5G        interface{} `json:"5g"`             // 每射频配置/状态
        SyncTime      string      `json:"synctime"`
        Fingerprint   string      `json:"fingerprint"`
        ModelOld      string      `json:"model_old"`
        TxRate        int         `json:"txrate"`          // 上行速率
        APPid         string      `json:"ap_pid"`         // 注意原串 ap_pid
        SysLoad       string      `json:"sysload"`         // 系统负载
        Memory        string      `json:"memory"`
        Version       string      `json:"version"`        // AP 固件版本
        Model         string      `json:"model"`          // 设备型号
        RxRate        int         `json:"rxrate"`          // 下行速率
        DevName       string      `json:"devname"`
        Uptime        int         `json:"uptime"`         // 在线时长（秒）
        Reged         string      `json:"reged"`          // 注册状态
        AuthHash      string      `json:"authhash"`
    } `json:"status"`
    ReportTime   int64 `json:"reporttime"`  // 上报时间
    LastTime     int64 `json:"lasttime"`    // 最后响应
    IsLowVersion bool  `json:"is_low_version"`
}
```

> 借鉴点：射频级状态挂在 `status."2.4g"` / `status."5g"`（三频设备可能另有 `"6g"` 键）；速率用 `txrate`/`rxrate` 整数、在线时长 `uptime` 秒——与本项目的客户端遥测（信号/速率/在线时长/IP 尽力）对齐。

### 2.2 AP 列表聚合 —— `GET /api/apsearch`

```go
type APSearch struct {
    Ret            int           `json:"ret"`
    Msg            string        `json:"msg"`
    Info           []string      `json:"info"`            // i18n 键数组（界面文案）
    IsSetDefalutIP bool          `json:"is_set_defalut_ip"`
    APList         []APInfo      `json:"aplist"`          // 设备数组（无设备时为 []）
    APNum          int           `json:"apnum"`
    TempDict       map[string]int `json:"tempdict"`       // 模板→AP 计数
    CustomDict     map[string]int `json:"customdict"`
    ModelDict      map[string]int `json:"modeldict"`      // 型号分布
    RegStatDict    map[string]int `json:"regstatdict"`    // 注册状态分布
    StatusDict     map[string]int `json:"statusdict"`     // 在线/离线分布
    VersionDict    map[string]int `json:"versiondict"`    // 版本分布
}
```

> 借鉴点：`*Dict` 聚合分布非常适合仪表盘“带机量/型号/状态”统计卡片——本项目 P4 遥测可照搬此聚合思路。

### 2.3 系统基本配置 —— `GET /api/basicview`（PUT → `basicsetup`）

```go
type ACConfig struct {
    EnableAC     string `json:"enableac"`      // "yes"/"no"
    ACVersion    string `json:"ac_version"`    // 例 "V2.2_202510151606"
    Prot         string `json:"prot"`          // 发现/接口端口，例 "60650"
    IsDebug      string `json:"is_debug"`
    UploadPath   string `json:"uploadpath"`    // 例 "/tmp/"
    DBPath       string `json:"dbpath"`        // 例 "/data/"
    IsCheckToken string `json:"ischecktoken"`  // "1"
    DefaultLang  string `json:"default_lang"`  // "zh"
}
```

### 2.4 加密方式枚举 —— `GET /api/tempview` 内 `enc_table`

```go
type EncMethod struct {
    Label string `json:"label"` // i18n 键
    Value string `json:"value"`
    Desc  string `json:"desc"`
}
// 实测 value 集合（直接照抄为枚举）：
//   none       开放网络
//   owe        OWE（增强型不加密）
//   wpa_1      WPA-PSK
//   wpa_1_2    WPA-PSK/WPA2-PSK
//   wpa_2      WPA2-PSK
//   wpa_2_3    WPA2-PSK/WPA3-SAE
//   wpa_3      WPA3-SAE
```

> 借鉴点：本项目现网基线用 `psk2+ccmp`/`sae-mixed`/`sae+ccmp`。集客用 `wpa_1_2`/`wpa_2_3` 这类“混合”值，语义与我们的 `sae-mixed` 等价——SSID 加密字段可直接采用此 `value` 枚举，避免自造字符串。

### 2.5 MAC 过滤（黑白名单）—— `GET /api/macfilterview`

```go
type MACFilter struct {
    Ret       int         `json:"ret"`
    Msg       string      `json:"msg"`
    Info      []string    `json:"info"`
    MACFilter interface{} `json:"macfilter"` // 结构待补（接入真实配置后补全）
}
```

### 2.6 终端吸附/智能磁吸（sticky）—— `GET /api/stabindview`

```go
type StaBind struct {
    Ret         int           `json:"ret"`
    Msg         string        `json:"msg"`
    Info        []string      `json:"info"`
    StaBindList []interface{} `json:"stabindlist"` // 结构待补（每项为“终端→AP/SSID 绑定串”）
}
```

> 借鉴点：集客的“智能磁吸/绑定串”本质是把某终端**吸附**到指定 AP/SSID（粘性漫游）。本项目已有 steering 守护（k/v/r），可把“吸附/绑定”作为更高层的粘性策略，复用 `stabindlist` 这类结构。

### 2.7 模板视图 —— `GET /api/tempview`

```go
type TemplateView struct {
    Ret          int         `json:"ret"`
    Msg          string      `json:"msg"`
    TempList     interface{} `json:"templist"`       // 模板数组（结构待补）
    MACFilterDict interface{} `json:"macfilterdict"`
    Info         []string    `json:"info"`
    EncTable     []EncMethod `json:"enc_table"`      // 见 2.4
}
```

> 借鉴点：模板维度含 `templist` + `macfilterdict`（模板→MAC 过滤计数）。本项目模板已含 SSID 集合；可补“引用计数 / 缺省模板 / 移动到其他模板”等生命周期（见 §3）。

---

## 3. 功能对照与借鉴清单

| 功能域 | 集客 AC 能力（前端字符串实证） | 本项目现状 | 建议借鉴 |
|---|---|---|---|
| 设备/AP | 列表（在线/离线/注册状态/型号/最后响应）、重启、恢复出厂、周期性重启、定时 | 列表 + 在线派生 + 采纳；偏只读 | 补 **重启/恢复出厂/周期重启** 动作 + 展示 **型号、最后响应、在线时长** |
| SSID 模板 | 模板列表、**引用次数**、**缺省默认模板**、**移动到其他模板**（约束：模板下有 AP 不可删）、**SSID/密码后缀自动追加**、克隆 | 模板 + SSID 增改（P1 已完成） | 补 引用计数 / 缺省模板 / 批量移动 / 后缀 / 克隆 |
| SSID 高级 | 最大连接数(`maxsta`)、终端隔离、业务隔离、隐藏 | 加密 / 隐藏 | 补 **最大连接数 + 两类隔离** |
| 射频 | 自定义射频、禁用射频、**干扰**指示、至少启用一个射频 | radio 配置已有 | 补 **干扰展示 + 每射频启停 + 校验** |
| 客户端 | 终端列表、在线时长、上行/下行速率、信号、漫游标记、**踢下线**、**追踪**、**黑白名单**、**智能磁吸**、终端隔离 | 遥测：信号/速率/在线时长/IP 尽力 | 补 **踢下线 / 追踪 / 黑白名单(MAC ACL) / 隔离** |
| 漫游 | **漫游信号阈值**、智能磁吸(粘性)、绑定串 | steering 守护(k/v/r) | 把 **阈值 + 粘性** 做成 UI 旋钮接入现有 steering |
| 分组 | AP 分组 → 指定模板 | 无分组维度 | 引入 **AP 分组→绑定模板** |
| 定时 | 每 SSID/AP 定时启用（全天/分时/分段/整周、按星期） | AP 侧 `wifi_schedule` 已禁用 | 做 **每 SSID 定时** UI 钩子 |
| 系统 | 导入/导出配置、恢复出厂、**固件升级**(在线/本地/上传/历史固件列表/一次一型号)、系统/内核日志、**网络诊断** | 未实现固件升级 | 补 **配置导入导出 + 网络诊断**；固件升级可选 |
| 网络/端口 | 端口/扩展网口、子网、静态路由、三层交换机、**业务网段与管理网段分离**、宽带拨号、地址池、固化地址、访问列表 | network(VLAN) 配置已有 | 补 **端口视图 + 网段分离可视化 + 静态路由** |

**架构层面确认**：集客 AC 同为“控制面集中、数据面在 AP 本地转发”，与本项目设计前提（不用 CAPWAP 隧道、AP 本地经 bridge/VLAN/PPE 转发）一致——路线验证无误。后端 Go + 前端 Vue/ElementUI 也与本项目 `internal/api` + 前端技术栈对齐。

**不建议借鉴**：私有云/微云代理、OEM 自助定制/审核、任何中心化数据隧道思路（会绕开 Airoha PPE 卸载）。

---

## 4. 映射到本项目 `ac-controller`

- **设备模型**：集客 `APInfo.Status` 的 `model/version/uptime/ip/mac/reged` 可并入本项目 `store.Device`（本项目已有 `Model`/`Hostname`/`Online`/`AppliedRev`/`Steering`）。建议补 `uptime`、`ip`、`reged` 展示字段。
- **SSID 加密**：直接采用 §2.4 的 `value` 枚举（`none/owe/wpa_1/wpa_1_2/wpa_2/wpa_2_3/wpa_3`），替换/补充现网 `psk2+ccmp` 类字面量。
- **客户端页**：以 §2.1 的 `txrate/rxrate/uptime` + 黑白名单(`macfilter`) + 磁吸(`stabind`) 为字段基线，补“踢下线/追踪/隔离”动作。
- **模板生命周期**：引入 `引用次数` + `缺省模板` + `移动到其他模板` 的约束逻辑（集客 LANG548：`模板下有 AP 须先移动/删除才能删模板`）。
- **仪表盘聚合**：照搬 §2.2 的 `*Dict` 分布统计（型号/状态/版本/注册）。

---

## 5. 复探方法（需要时自行抓取）

```bash
# 登录拿 cookie
curl -c cj.txt -X POST http://127.0.0.1:8891/api/sysauth \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"admin"}'
# 带 cookie 抓任意 action（替换 <act>）
curl -b cj.txt http://127.0.0.1:8891/api/<act>
# 例：抓模板/客户端/ACL/磁吸结构
for a in tempview apinfo apsearch macfilterview stabindview basicview; do
  echo "== $a =="; curl -s -b cj.txt http://127.0.0.1:8891/api/$a | head -c 1500; echo
done
```

> 提示：部分 action（如 `tempedit`、`apedit`）为写操作，会改动参考实例状态，复探只读 `view` 类 action 即可。需补全 `2.4g/5g` 射频子结构、`templist` 元素结构、`macfilter`/`stabind` 元素结构时，须先在该实例创建模板/接入真实 AP，或直接从前端渲染后的 DOM 读取。
