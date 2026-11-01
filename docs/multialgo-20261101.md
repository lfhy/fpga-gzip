# KU5P 多算法固件验收（2026-11-01）

本页保留首版镜像的验收记录。后续 LZ4/Zstd 并行优化与新镜像归档见
[固件优化实测](algorithm-optimization-20261101.md)。

gzip、LZ4、Zstd、raw 四种独立镜像均完成本机上板验收，共用 XDMA shell、ABI、Go SDK
和切换工具。新数据跟随当前镜像编码；已有对象按其保存的格式解码。
框架源码与构建说明见 [firmware/multialgo](../firmware/multialgo/README.md)，
接口与恢复流程见[固件指导](../firmware/README.md)。

本机归档：`/root/FPGA-4IP/qz01_compress_4ip/validated/20261101-multialgo`。每种算法目录保存 `.bit`、硬件验收 manifest、
实际构建的 RTL、路由 DCP 和时序报告。LZ4/Zstd 另外保存实际使用的 HLS IP ZIP；
`framework` 是当前源码，`build-base` 保存 PCIe 工程、gzip IP 与参考 DCP。
旧 profile 的实际构建快照以各算法目录为准。机器可读结果见
[multialgo-20261101.json](multialgo-20261101.json)。

## 切换

```sh
images=/root/FPGA-4IP/qz01_compress_4ip/validated/20261101-multialgo
"$images/switch.sh" inspect
"$images/switch.sh" lz4
"$images/switch.sh" zstd
"$images/switch.sh" raw
"$images/switch.sh" gzip
# 如需回到原来的无新 ABI gzip 镜像
"$images/switch.sh" legacy-gzip
```

此本机脚本固定 PCI `0000:39:00.0`、JTAG 序列号 `210251A08870` 和工具路径。
其他主机按固件指导填写实际参数。脚本在本地 hw_server 尚未监听时以独立会话自动启动它，
仅监听 127.0.0.1:3121；日志写到归档目录的 `hw-server.log`。
只装载易失 SRAM，整卡掉电后需要重新装载。
当前最终保留新版 gzip 镜像。manifest 中的 SHA256 与逐核身份都参与切换校验。

## 完整镜像与功能验证

| 算法 | 核数 | 核时钟 MHz | WNS / WHS ns | 直连用例 + 退化用例 | HTTP/Unix 用例 |
| --- | --- | --- | --- | --- | --- |
| gzip | 4 | 250 | 0.035 / 0.010 | 288 + 72 | 48 |
| lz4 | 4 | 250 | 0.007 / 0.010 | 288 + 72 | 48 |
| zstd | 2 | 125 | 0.108 / 0.009 | 144 + 36 | 24 |
| raw | 4 | 250 | 0.016 / 0.002 | 288 + 72 | 48 |

全部镜像通过完整布线、非负 setup/hold、零 DRC Error、零无时钟和无约束内部端点检查。
直连测试每核 72 组，覆盖核心标记、字节/块边界及 8 MiB 输入；额外每核 18 组
覆盖没有核心标记的全零、单字符、二符号数据。每次都用独立 CPU 解码器逐字节比较，
并核对错误算法请求在 RESET/START/DMA 前被拒绝、传输计数保持不变。
服务测试检查成功完成 trailer，错误算法返回 HTTP 409。

LZ4/Zstd 当前源码各通过 115 组 C 仿真和 25 组 RTL 仿真后的独立 Go 解码。
Zstd 另通过 3 组同板上种子的 `dma` RTL 回归（包含 1 MiB 随机输入）。
实际装载 IP 与压力回归的 336 个 RTL/ROM 文件逐字节一致；LZ4 的 49 个
文件在仅归一化源码行号生成的标识符后相同，原始差异也保留在归档中。
构建器在标准解码失败时停止导出 IP，避免仅凭 HLS 自身的 PASS 接受错误帧。
另外使用本机 Zstd/LZ4 C 参考解码器完整解压并逐字节比较，结果同样通过；
这些原生工具仅用于离线验收，产品运行时保持 nocgo。

Zstd 修正包含序列反转预读导致的跨块背压死锁、频率 RAM 真实依赖、小字母表
完整树及补齐项初始化、隐含末权重计数、
FSE 单符号状态、帧头标志和背压缓冲边界。补丁只应用于私有副本，上游库保持不变。

## gzip 新镜像性能回归

4 核、64 MiB chunk、P=8，每种模式输入 8 GiB，共 24 GiB。内存缓冲预先准备，
NUMA0 CPU 与 XDMA IRQ 亲和；1 MiB DMA 分段、4 个 H2C slot、EOP packet 读取。
计时包含设备开关和编码传输；全部输出随后在计时外用标准 Go gzip 解码并逐字节比较。

| 输入 | GB/s（十进制） | 输出 / 输入 |
| --- | --- | --- |
| fixture | 7.232 | 0.5363 |
| text | 7.319 | 0.0278 |
| random | 6.958 | 1.0553 |

本次是新 ABI gzip 镜像回归。96 GiB/轮的持续测试和 FUSE/NVMe 口径见
[容量基准](capacity-20261101.md)。LZ4/Zstd 是功能优先的首版，不套用 gzip 吞吐：
LZ4 的 8 MiB 直连测试约 52–88 MB/s/核，Zstd 当前使用 2 核、125 MHz。
通用 SDK 对 LZ4/Zstd FPGA 输出的完整 CPU 解码校验计入调用时间。
四种算法使用相同输入的三轮编码通道、SDK 与纯 Go CPU 对照见[性能报告](algorithm-performance-20261101.md)。

## 混合存储验证

同一 compactfs 挂载和算法 worker 完成四种镜像切换，每个镜像写入并排空 64 MiB。
正常阶段 CPU 回退增量为 0；切换期间另有 64 MiB 数据成功回退 CPU LZ4。
重复写入先前 LZ4 内容时，backend 对象数没有增加。

随后重新挂载，关闭 SSD cache、解码缓存、预取和 raw 回填，从 backend 完整读取
7 个文件、448 MiB，所有 SHA256 一致。
该测试保留 Linux/NVMe 页缓存，验证的是 compactfs 缓存关闭后的 backend 解码路径。
原始记录、同挂载 PID 与同 worker PID 的证明保存在 `fuse-validation`。
服务器在验证期间重启；恢复后先关闭 compactfs 缓存读回原来的 6 个文件、384 MiB，
全部 SHA256 一致，再用新 backend 重跑四种镜像的同挂载切换。
重启前已完成的综合和 RTL 回归保留，未完成阶段从检查点续跑或重新仿真。

| 算法 | 构建 ID | bitstream SHA256 |
| --- | --- | --- |
| gzip | 86950339975258 | `56f523015c885728f530679c35a5e3f261cf2cb45e3380fb5bcf2cdb4ef700fb` |
| lz4 | 200317347967794 | `4b0b50d38a722fadb2803cf39072e4d09639ac79829d83367ef9de60d931f8da` |
| zstd | 167671449958172 | `32dd90b5e64e4c8f6eb6b1d5cc4e1309e78c2ca161928fa940eeebd7a157771e` |
| raw | 149839125801747 | `12fc3ab1d6cd72494147a36a2be10d311ea33b8277c8cb9816f614f8edda4a3e` |
