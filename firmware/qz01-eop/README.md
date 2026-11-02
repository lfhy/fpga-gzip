# gzip EOP 主机协议

本页描述 SDK 使用外部 gzip 固件时的 EOP 协商、读取和校验行为。
板级 shell RTL、派生补丁、仿真与整卡构建脚本保存在独立私有工程，
不在本仓库分发。已有镜像通过外部 manifest 使用，见[固件指导](../README.md)。

支持 EOP 的固件允许主机提前提交较大的 C2H 请求，gzip 最终 TLAST 提前
结束末次请求，XDMA EOP flush 返回实际长度。旧协议仍按已观察到的输出
计数提交精确长度读取。驱动配置及补丁见[驱动指导](../../docs/driver-install.md)。

## 协商与校验

每核控制 bank 的步长为 `0x1000`，主机使用以下能力字段：

| 寄存器/位 | 含义 |
| --- | --- |
| `0x40` | EOP 协议标识 `0x475a0001` |
| `STATUS[4]` | gzip 最终 TLAST 已完成 C2H 握手；软复位清零 |
| `STATUS[5]` | 当前 C2H 请求尚未完成 |

SDK 仅在完整标识匹配时启用 EOP。Linux XDMA C2H 字符设备以 `O_TRUNC`
启用驱动的 `eop_flush`；SDK 先固定 inode 并核实字符设备，避免截断普通文件。
`--legacy-packets` 可强制精确长度读取。非最终请求必须 8 字节对齐。

收到短包后仍需确认 EOS、精确输出计数和最终状态；满长包也检查 EOS，
避免输出大小恰好整除请求长度时再提交多余读取。输入长度、CRC32、gzip
头尾、最终 IN/OUT 计数及 DONE/ERROR 状态校验始终保留。

## 主机端验证

```sh
CGO_ENABLED=0 GOWORK=off go test ./device ./pipeline
CGO_ENABLED=0 GOWORK=off go run ./examples/verify-firmware \
  --algorithm gzip --output /data1/gzip-board.json
CGO_ENABLED=0 GOWORK=off go run ./examples/verify-firmware \
  --algorithm gzip --edge --output /data1/gzip-edge.json
```

实际板测需要兼容设备及已装载的有效镜像。芯片时序和 shell 仿真由私有
工程独立验证；这些 Go 测试只覆盖主机侧协议，不能替代硬件验证。
CLI/daemon 可配置 `--packet-request-bytes=1MiB`、`--legacy-packets=false`、
`--mmap-registers=false`。库不会改变宿主的 GOMAXPROCS。

持续压缩使用 `examples/benchmark-capacity`，输出留在 RAM，计时后全部
用标准 gzip 解压并逐字节比较。历史数据及计时口径见
[容量报告](../../docs/capacity-20261101.md)与[gzip 吞吐](../../docs/gzip-throughput-20261102.md)。
