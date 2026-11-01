# 可切换的压缩固件

统一控制协议和 DMA shell，每个 bitstream 包含一种算法：gzip、LZ4、Zstd、MinLZ 或 raw。
整卡切换替换压缩核，不要求将所有算法同时放进 KU5P。压缩数据保留标准
格式，切换硬件不影响已有数据的 CPU 解码。框架源码位于 [multialgo](multialgo/)。
优化版 LZ4/Zstd 的构建 ID、性能、源码归档与切换路径见
[固件优化实测](../docs/algorithm-optimization-20261101.md)。

```sh
CGO_ENABLED=0 GOWORK=off go build -o bin/fpga-fw ./cmd/fpga-fw
CGO_ENABLED=0 GOWORK=off go build -o bin/fpga-compress ./cmd/fpga-compress
bin/fpga-fw inspect
bin/fpga-fw check --manifest /path/to/image/manifest.json
bin/fpga-fw switch --manifest /path/to/image/manifest.json \
  --pci 0000:39:00.0 \
  --target localhost:3121/xilinx_tcf/Digilent/210251A08870 \
  --vivado /opt/Xilinx/2025.2/Vivado/bin/vivado
bin/fpga-compress --algorithm lz4 --input data.bin --output data.lz4
```

PCI 地址和 JTAG 序列号是本机示例，其他主机需填写实际值。`switch` 要求 root，
只写易失 SRAM。它先核对 SHA256 并保存私有副本，阻止新任务进入，等待现有
SDK 句柄释放，检查所有 XDMA 描述符，再卸载驱动、移除目标 PCI 功能、装载、
扫描原父桥、恢复 XDMA，最后逐核校验算法、ABI、格式、核数和构建 ID。
任一步失败会返回错误；PCI/驱动恢复不等于旧 bitstream 恢复。回滚使用同一条
`switch` 命令指定之前验证过的 gzip manifest。该操作不写 Flash。

应用必须在每次事务结束后调用 `Device.Close` / `Lease.Close`。新版 SDK 在
每个句柄上持有共享锁，切换工具持有独占锁；切换期间新事务得到 `ErrSwitching`，
可以排队或回退 CPU。锁覆盖同一 Unix 用户，跨用户旧程序由 `/proc` 检查和
驱动的使用计数阻止卸载，不会强杀其他程序。不同用户或非协作程序应先停止提交。

## SDK 接口

根包的 `CompressReader`、`Writer` 等原有接口继续输出 gzip。多算法可用根包的
`CompressAlgorithm(ctx, "lz4", src, size, dst, hw, options, nil)`，或公开包
`github.com/lfhy/fpga-gzip/accelerator`。`InspectFirmware(ctx, userDevice)`
只查询身份，不打开 DMA、不重置压缩核。所有接口无 cgo、无 `internal` 包。

```go
hw, err := device.Open(device.Paths{
    User: "/dev/xdma0_user", H2C: "/dev/xdma0_h2c_0", C2H: "/dev/xdma0_c2h_0",
})
if err != nil { return err }
defer hw.Close()
result, err := accelerator.CompressReader(ctx, accelerator.LZ4,
    bytes.NewReader(raw), uint64(len(raw)), dst, hw, accelerator.DefaultOptions(), nil)
```

算法不匹配在 RESET、START、DMA 之前返回 `firmware.ErrAlgorithm`。
未知 ABI、格式或未初始化 BAR 返回 `firmware.ErrIncompatible`。已验证的旧 gzip
EOP 镜像只识别为 legacy gzip，不能被当作其他算法使用。其他没有身份信息的旧
镜像不会自动接受，需要通过适配或重新构建加入版本信息。

gzip 使用已有的头部、CRC32、ISIZE 校验。LZ4 和 Zstd HLS 输出没有内容校验和，
因此通用 SDK 同时用有界纯 Go 解码器验证整个输出，再比对输入 CRC32 和长度；
这部分消耗 CPU，计入 `Result.Total`。raw 直接校验长度和 CRC32。
输入在调用结束前必须保持不变。空输入暂不支持。库不改变应用的 GOMAXPROCS。
文件 CLI 在验证并同步临时文件后才发布目标文件。

## HTTP / Unix 服务

```sh
CGO_ENABLED=0 GOWORK=off go build -o bin/fpga-gzipd ./cmd/fpga-gzipd
bin/fpga-gzipd --unix /run/fpga-compress.sock --cores 4 \
  --algorithms gzip,lz4,zstd,minlz,raw
```

使用 `client.CompressAlgorithm(ctx, firmware.LZ4, src, size, dst)` 或
`POST /compress?algorithm=lz4`，请求仍需完整 Content-Length。只接受与当前镜像
匹配的算法，未装载时 HTTP 409。认证沿用 `WithAuth`，默认关闭。
多算法响应包含 `X-FPGA-Algorithm`，并在成功后给出 `X-FPGA-Complete: 1` 和
输出长度 trailer。SDK 客户端检查该完成标志，避免将传输中途失败误判为成功。

同一卡的 DMA 核由一个调度进程管理：使用此 daemon，或使用 compactfs 的算法
worker。应用通过该进程提交请求；各进程的 `device.Pool` 不共享核心租借状态。

## ABI v1

控制 bank 为 `core * 0x1000`。原有 `0x00..0x40` 的 DMA 控制和 EOP 保持不变。
下列字段只读，每个有效 bank 内容相同。

| 偏移 | 含义 |
| --- | --- |
| `0x44` | Magic `0x434d5058`，CMPX |
| `0x48` | ABI，major:minor 各 16 bit，当前 `0x00010000` |
| `0x4c` | 算法 ID：1 raw、2 gzip、3 LZ4、4 Zstd |
| `0x50` | 编码格式版本，当前 1 |
| `0x54` | 可用独立 DMA 核数，1–4 |
| `0x58` | 特性位：bit 0 EOP、bit 1 标准帧 |
| `0x5c/0x60` | 构建 ID 低/高 32 bit |

新算法首先增加稳定 ID、格式验证器和固件 profile，再接入压缩核。
`manifest.json` 与 `.bit` 同目录，包含 SHA256、算法、构建 ID、核数和验证阶段；
只有通过时序检查的镜像才生成可装载 manifest，首次上板仍需完整解码验证。
