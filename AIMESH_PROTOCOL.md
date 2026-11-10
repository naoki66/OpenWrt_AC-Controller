# XR1710G 与华硕 AiMesh 混组适配记录

当前交付为 `0.3.1-aimesh-probe.1` 协议诊断开发版。**尚不能把 XR1710G 加入原厂华硕 AiMesh，也不能让原厂华硕成为本控制器的节点。** 已有的 OpenWrt 802.11s 功能与这里的 AiMesh 适配是两条独立路径。

## 参考对象和边界

按华硕 Wi-Fi 7 / AiMesh 方向开发。当前可复核的二进制样本为 `GT-BE98_PRO`；它是协议参考样本，不代表已核实其为市场最新型号，也不代表所有 Wi-Fi 7 固件具有相同协议。XR1710G 的可用频段和驱动能力仍以设备自身为准，移植软件不会增加 6 GHz 或 MLO 硬件能力。

参考仓库：[RMerl/asuswrt-merlin.ng](https://github.com/RMerl/asuswrt-merlin.ng)，检索版本 `41ca8269b02b60e7ee0844ee117f45e914ad8230`（2026-11-10）。分析公开发布的头文件、预编译 ELF、ARM 反汇编及选定函数的反编译结果；插件不分发华硕二进制。没有尝试绕过设备认证。

参考路径均位于 `release/src/router/`：

| 样本 | SHA-256 |
| --- | --- |
| `cfg_mnt/prebuild/GT-BE98_PRO/cfg_server` | `2b537b8abbc5e82b1afb5034493a1975c7c59f44b37adcc48f50c82c5c9e7cbc` |
| `amas-utils/prebuild/GT-BE98_PRO/libamas-utils.so` | `1a279ce81393bf6cac5f85ec89720e1501cbba39c990243579589026c5011880` |

例如：[cfg_server 样本](https://github.com/RMerl/asuswrt-merlin.ng/blob/41ca8269b02b60e7ee0844ee117f45e914ad8230/release/src/router/cfg_mnt/prebuild/GT-BE98_PRO/cfg_server)、[公开接口头文件](https://github.com/RMerl/asuswrt-merlin.ng/blob/41ca8269b02b60e7ee0844ee117f45e914ad8230/release/src/router/amas-utils/amas-utils.h)。函数地址是原 ELF 虚拟地址，适用于上述哈希。

## 已确认并实现的封包层

`shared/aimesh` 提供以下编解码及被动分析，`ac-aimesh` 随 agent 包安装到 `/usr/sbin/`：

- 以太网 LLDP（`0x88cc`）及 ASUS 组织扩展 OUI `F8:32:E4`；支持最多两层 VLAN 标签。
- LLDP 前三个必需 TLV、结束 TLV、字段边界检查。支持未知 ASUS 字段，但诊断只输出字段编号和长度。
- 已核对的字段包括：1 配对状态、4 LAN MAC、5 型号数据、6 时间戳、8 会话密钥数据、13 安全状态、14 分组数据、15/16/17 超时数据。
- IPv4 UDP 配置报文：端口 `7788`；12 字节头，依次为大端 `uint32` 类型、载荷长度、载荷校验；接收路径接受类型 1–7。
- 校验为反射 IEEE CRC32 多项式 `0xedb88320`，初始寄存器 0，无最终取反。例如 ASCII `123456789` 得到 `0x2dfd2d88`，不是普通 `crc32.ChecksumIEEE`。
- 有界流式读取经典 Ethernet PCAP，支持大小端和微秒/纳秒时间戳；拒绝 PCAPNG、Linux cooked 格式及过大记录，跳过截断帧。

诊断不会输出载荷、会话密钥、分组标识或未知字段原文；不会写入网络配置、发送配对封包或自动宣告入网成功。输出 `joined:false` 表示工具没有执行入网；不能据此判断设备此前是否已在其他系统中入网。

## 可复核证据

| 结论 | ELF / 函数或指令地址 |
| --- | --- |
| LLDP subtype 13 为安全状态 | `libamas-utils.so` `amas_set_secstatus`，`0x184e4` |
| LLDP subtype 8 为会话密钥数据 | `amas_set_sessionkey`，`0x1a508` |
| LLDP subtype 14 为分组数据 | `amas_set_group`，`0x1f53c` |
| LLDP subtype 1/4/5/6、超时字段 | `amas_set_obstatus`，`0x214ec` |
| 整数字段编码成 4 字节大端 | `LLDP_NBR_TLV_SET_INT`，`0xae30`，使用 `rev` 后传入长度 4 |
| UDP 校验函数 | `cfg_server` `0x23f30`；完整 256 项表在 `0xe5274` |
| UDP 头长度、类型及校验检查 | `cm_udpPacketHandler`，`0x24034`，关键检查位于 `0x241bc`、`0x24270`、`0x2431c` 附近 |
| TCP/UDP 配置端口 | `cm_openSocket` 路径 `0xe1a88` / `0xe1c08`，引用 `0x138490` 中的 7788 |

`shared/aimesh/testdata/wire-evidence.json` 保存从 ELF 校验表计算的确定性测试向量。LLDP 测试帧是按已确认布局手工构造的合成帧，**不是华硕实机抓包**。目前仅实现 UDP 外层；没有据此推断 TCP 内层、密钥派生、消息语义或认证状态机。

可用安装了 `pyelftools` 的 Python 环境复核校验表和端口常量：`python tools/aimesh-extract-evidence.py /path/to/cfg_server > evidence.json`。脚本先校验样本 SHA-256，再逐项核对全部 256 个 CRC 表项；不执行固件。生成结果应与测试数据一致。

## 安装后诊断

安装包和已有 OpenWrt Mesh 配置见 `MESH_安装说明.md`。仅运行诊断不需要启用 agent/controller 服务。

```sh
# 先列出实际 LAN 接口；不要把示例 lan1 当作所有固件通用名称。
ip link show

# 在收到华硕邻居封包的 LAN 端口观察，输出为逐行 JSON。
# 配对动作在华硕管理页面手动触发。本工具只接收。
ac-aimesh -interface lan1 -duration 60s > /tmp/aimesh-observation.jsonl

# 也可以分析已有 Ethernet PCAP。
ac-aimesh -pcap /tmp/aimesh.pcap > /tmp/aimesh-observation.jsonl
```

实时监听需要 root 或 `CAP_NET_RAW`，绑定单个接口，不开启混杂模式。桥接、硬件交换卸载会影响报文可见性；LLDP 属于链路本地报文，应优先在直连华硕的物理 LAN 端口观察。没有观测到报文不等于协议不兼容。程序只解析 IPv4 UDP 外层，不进行 IP 分片重组、TCP 重组、无线监听或解密。

## 混组还缺少的部分

1. 完整恢复并验证发现、配对、密钥交换、认证状态机和配置同步消息语义。
2. 在 OpenWrt 上实现与原厂兼容的控制器/节点身份和配置映射，处理 DHCP、桥接、恢复与断线重连。
3. 将华硕无线回程 AP/STA/WDS 行为映射到 XR1710G 驱动，实测四地址转发、切换和漫游。仅建立 802.11s 邻居不能完成此步骤。
4. 与明确固件版本的真实华硕 Wi-Fi 7 设备进行有线配对及无线回程联调，双向验证入网、重启保持和故障恢复。

继续实机工作需要一台可测试的华硕 AiMesh 路由器及其固件版本、对应配对过程封包；当前工作区没有设备连接信息或实机抓包。因此当前包应被视为协议适配基础与诊断版本，不能作为已完成混组的固件安装使用。
