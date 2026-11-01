# KU5P 四核 gzip：可提前提交的 C2H 请求

此补丁针对本机已验证的 `qz01_compress_4ip_8k8lane_ml2_bramfifo` RTL shell，
需要原工程、原 HLS IP、约束及 Vivado 2025.2。不是通用 FPGA bitstream，
也不改变 gzip/HLS 压缩算法。完整工程、IP 及 bitstream 不放进 SDK 仓库。

旧 packetizer 只能按主机指定的精确长度结束请求。主机必须先观察
`OUT_BYTES`，等 FIFO 内有足够输出才能提交读取；盲目增大请求会因 FIFO
背压而死锁。新 packetizer 同时识别 gzip 的最终 TLAST，因此可先提交
1 MiB 请求，末包在 gzip EOS 提前结束。XDMA 的 EOP flush 返回真实长度。

新协议的性能配置还包含[驱动补丁](../../docs/driver-install.md)：EOP 连续页合并、
描述符链尾 EOP 修复及 1 MiB DMA 段上限。逐页 SG 和默认 64 KiB 段会增加完成中断，
漏判链尾 EOP 则会导致多余读取等待 10 秒。
最终 50% 可压缩输入的四核三轮持续测试均约 7.29 GB/s，每轮 96 GiB，
全部输出解压比对通过。完整条件与其他输入见[容量基准](../../docs/capacity-20261101.md)。

寄存器按每核 `0x1000` 分区，原寄存器和状态位保持含义：

| 寄存器/位 | 新增含义 |
| --- | --- |
| `0x40` | 协议标识 `0x475a0001` |
| `STATUS[4]` | gzip 最终 TLAST 已完成 C2H 握手；软复位清零 |
| `STATUS[5]` | 当前 C2H 请求尚未完成 |

SDK 只有读到完整标识才使用新协议。Linux XDMA C2H 字符设备以
`O_TRUNC` 打开启用官方驱动的 `eop_flush`；SDK 先用 `O_PATH` 固定 inode
并核实字符设备，防止意外截断普通文件。老固件及 `--legacy-packets`
继续走 32 KiB 精确长度协议。非最终请求必须 8 字节对齐。

新协议收到短包后，必须同时确认 EOS 和精确的 `OUT_BYTES`；满长包
也检查 EOS，避免输出长度刚好整除请求长度时多读一次。输入 CRC32、
gzip 头/尾、最终 IN/OUT 计数及 DONE/ERROR 状态的检查继续保留。

在原 RTL 的副本上应用，先 `--dry-run` 检查版本：

```bash
patch --dry-run -d /path/to/copied/rtl -p1 < firmware/qz01-eop/packet-eop.patch
patch -d /path/to/copied/rtl -p1 < firmware/qz01-eop/packet-eop.patch
firmware/qz01-eop/run-stream.sh /path/to/copied/rtl/axis_c2h_packetizer_64.v
```

仿真包含 781 个 EOS、请求长度、TKEEP、4/8/64 KiB 边界、供数间隔及背压用例。
旧 `tb_axis_boundaries.sv` 实例需将新增 `s_axis_tlast` 接 0 后再次运行。
完整工程必须重新综合及布局布线，检查 WNS/WHS 非负、布线完成及 DRC
无 error，再生成并在线装载。保留原 bitstream，装载前关闭所有 DMA
使用者，装载后核对四个 bank 的协议标识并重新跑真实板卡边界校验。
仅做易失性 JTAG 装载；不写配置 Flash。

`build.tcl` 是本次构建流程的参数化版本。先把已验证的原工程复制为
`<build-root>/project`，原 RTL 复制为 `<build-root>/rtl`，在该 RTL
副本应用补丁。保留原 IP 的 OOC 综合结果，不重综合 HLS IP、不扩大 FIFO；
原工程不会被脚本打开或重置。参考 checkpoint 和 IP 仓库均以原文件只读使用：

```bash
/opt/Xilinx/2025.2/Vivado/bin/vivado -mode batch \
  -source firmware/qz01-eop/build.tcl -tclargs \
  /path/to/copied-build-root /path/to/reference/top_4core_routed.dcp \
  /path/to/original/ip_repo_shorttail72
```

脚本验证工程及 run 目录位于副本内部，生成 timing/utilization/route/DRC
报告，只有时序和 DRC 通过才写出 `qz01_gzip_4ip_eop.bit`。

CLI/daemon 的新选项：`--packet-request-bytes=1MiB`、
`--legacy-packets=false`、`--mmap-registers=false`。BAR mmap 仅作为
Linux amd64 的可选实验项。库不会修改宿主的 `GOMAXPROCS`。

持续压缩用 `examples/benchmark-capacity`：计时包含四核整组事务，
压缩输出保留在 RAM，全部输出在计时后由标准 gzip 解压并逐字节比较。
输入生成与解压校验不计入压缩吞吐；该指标不等同于文件系统持久化速度。
