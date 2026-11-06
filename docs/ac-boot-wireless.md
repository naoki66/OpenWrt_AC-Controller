# 开机后无线不恢复：XR1710G 真机事故复盘

日期：2026-10-31，设备 `10.0.30.251`（AP-A / Gemtek XR1710G），AC 已接管 wireless + network。
结论先行：**由 AC 接管无线的 AP，重启后可能一个 BSS 都不起来，而控制器看起来完全健康。**
修法在 `internal/agent`：`settleNeighbors` + `EnsureWireless`。本文记录现象、误判过程和禁用项。

## 1. 现象

重启后：

- SSH/HTTP/ICMP 全部正常，`br-lan` 与 `br-lan.1..6` 全部 UP
- 控制器心跳正常，`applied_rev=5`、`sync=applied`
- `ps` 里 hostapd 在跑，`network.wireless status` 报告三个 radio `up: true`
- 但 `ubus list | grep '^hostapd\.'` **为空**，`ip link` 里没有 `phy0.*`
- 一个客户端都接不上

即：**管理面、数据面、控制面全部正常，只有射频是死的。**

## 2. 启动时间线（两次重启，同样的形状）

```
kernel [39s]  ac_radio0 起来，phy0.0-ap0 入 br-lan，ACS 开始
kernel [40s]  ac_radio1 起来，phy0.1-ap0 AP-ENABLED
kernel [42s]  ac_radio2 起来，phy0.2-ap0 AP-ENABLED
kernel [43s]  墙上时钟被向前拨（本次 +12 分钟，另一次 +22 分钟）
              ↓ 之后
              hostapd 逐个 Remove interface / AP-DISABLED
              radio2: wifi-scripts Setting antenna ... 然后没有任何后续输出
              hostapd 永久停在"有进程、无 BSS"
```

关键：**BSS 是先成功起来、再被整体 teardown 的**，不是配置错误。
所以 `uci show wireless` 一切正常（`ac_radio0/1/2` disabled=0，本地 `radio0/1/2` disabled=1）。

## 3. 误判过程（值得记）

- **以为 11k 只是"慢"**：最初看到 6 GHz 的 6 个列表在 `13:23:58` 写成功、2.4G/5G 在
  `13:28:40` 才补上，判定为 ACS 竞态。做了 settle 修复。真机上跑出来却是
  `pending="... 11 个全部 (absent)"` —— BSS 压根不存在，不是慢。**"没有"和"还没"是两回事。**
- **以为 `wifi reload` 能救**：手工 `wifi reload` 后 25 秒，hostapd 对象仍然为 0。
  该状态下 reload 无效，`wifi down && wifi up` 才有效。
- **以为时钟跳变是根因**：两次失败都紧跟时钟跳变，但禁用干扰项后仍然复现，
  说明跳变只是触发条件之一。**不要为了证伪一个猜测去重启生产 AP 第二次**——
  先把自愈做出来，再验证。

## 4. 必须禁用：wifi_schedule

设备自带的 LuCI 定时开关 WiFi（`luci-app-wifischedule`）会在开机末尾跑一次：

```
/etc/rc.d/S99wifi_schedule -> /etc/init.d/wifi_schedule
  -> /usr/bin/wifi_schedule.sh startup
     -> _wifi_rfkill_set_all_to <status>
        _wifi_get_devices()  # 返回"所有" wifi-device 段，含 AC 渲染的 ac_radio*
        uci set wireless.${radio}.disabled=${status}   # 逐个改写
        uci commit
        /sbin/wifi                                     # 无条件全量重载
```

两个问题：

1. 它把**所有** `wifi-device` 段（含 `ac_radio0/1/2`）的 `disabled` 一起改写，
   AC 的接管语义被它覆盖；
2. 无论判定为开还是关，都会 `uci commit` + `/sbin/wifi`，而这一下落在 netifd
   正在建 BSS 的窗口里 —— 与上面第 2 节的 teardown 时间吻合。

部署时执行：

```sh
/etc/init.d/wifi_schedule disable   # 移除 /etc/rc.d/S99wifi_schedule
```

禁用后重启**仍然**复现（时钟跳变那次），所以它只是干扰源、不是唯一根因。
真正的兜底是下面第 5 节的自愈。

## 5. 修法：轮询终态，不猜"何时就绪"

两处都在 agent，不去猜 hostapd 什么时候算真的起来了，而是**直接问"目标状态到了没有"**。

### settleNeighbors（11k 列表）

apply 后、以及开机首轮恢复后，开一个 60 秒窗口，2 秒一轮回读 `rrm_nr_list`，
BSS 一出现就补写。

- 只有**首轮**（cycles==1）走这个窗口；周期刷新（每 15 轮）保持单次廉价通过。
  永远起不来的 BSS 不许拖住心跳循环。
- 判据用 `ubus list hostapd.X`，把"不存在"和"存在但列表空"分开：前者记为
  `xxx(absent)`，是必须回来补的，后者是可以直接写的。

### EnsureWireless（无线整体缺失）

仅当**期望的 BSS 一个都不存在**时才执行 `wifi up`，且**在 settle 窗口耗尽之后**。

- "全部缺失"这个判据保证：一个慢 radio、或一个坏 BSS，永远不会触发一次
  会踢掉全部客户端的重载。
- "窗口耗尽之后"这个顺序保证：boot 时 agent（START=95）早于 hostapd 建完 BSS，
  若此时就去 `wifi up`，等于亲手打断正在进行的 bring-up。
- 节流 5 分钟（`WirelessRetryEvery`），坏射频不会被每个周期重启一次。

## 6. 真机验收（第三次重启，10.0.30.251）

```
14:23:03  6G/5G 在 boot 期间 AP-ENABLED，2 个列表写入成功（随后被 teardown）
14:23:56  settle 窗口耗尽：11 个 BSS 全部 (absent)
14:23:56  "no wireless BSS present, bringing the radios up"  -> wifi up
14:24:02  6 GHz 6 个 BSS AP-ENABLED，列表写入
14:24:05  5 GHz 4 个补齐
14:24:13  2.4 GHz（ACS 完成后）补齐  -> 11/11
```

判定到 11/11 共 **17 秒**；修复前是"永不恢复 + 5 分钟周期刷新也救不了"。

## 7. 排障速查

```sh
# 无线是否真的在服务（不是看进程，也不是看 netifd 的 up）
ubus list | grep '^hostapd\.'        # 空 = 射频是死的
ip -o link show | grep -c phy0

# 11k 每个 BSS 的条目数
for o in $(ubus list | grep '^hostapd\.'); do
  printf '%s=%s ' "${o#hostapd.}" "$(ubus call $o rrm_nr_list | grep -c '"[0-9a-f][0-9a-f]:')"
done; echo

# 抢救
wifi down && sleep 3 && wifi up      # reload 无效，必须 down/up
```

注意 `/tmp` 是 tmpfs，采样脚本重启后会丢，需要重新传。
