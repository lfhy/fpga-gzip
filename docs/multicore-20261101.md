# 四核 FPGA gzip 压缩

本机验证过的 KU5P `FPGA-4IP` shell 已有四个独立 gzip 核。本次让主机端使用它们，没有重刷 bitstream、修改 HLS 算法或驱动。此前 SDK 与 compactfs 都只用了 core 0；不能将“单核串行提交”当成整张卡的能力。

四核 API 目前位于本地源码，尚未发布新版，已发布的 v0.0.2 不包含这些接口。[机器可读实测报告](multicore-20261101.json)记录逐轮数据、原始证据哈希及二进制构建信息。

## SDK 与 daemon

每个独立事务必须同时选择匹配的 DMA 通道和寄存器区：

| core | H2C / C2H 后缀 | user BAR 基址 |
| --- | --- | --- |
| 0 | `_0` | `0x0000` |
| 1 | `_1` | `0x1000` |
| 2 | `_2` | `0x2000` |
| 3 | `_3` | `0x3000` |

`device.Paths.RegisterBase` 默认 0；`Device.SetRegisterBase` 只应在事务开始前设置，不能与 DMA 或寄存器访问并发。`FourCorePaths` 验证核心数量和 channel 0 路径，再生成匹配的通道与寄存器区。

`device.NewPool` / 根包 `NewDevicePool` 提供有界租借，拒绝重叠的 DMA 通道或控制区。`Acquire(ctx)` 等待空闲核，返回的 lease 必须 `Close`。流水线完成时的 `CloseStreams` 只关闭流句柄；lease 继续持有控制区，直到最终状态核对完成、调用方关闭它。每次事务仍打开新的 XDMA 流句柄。

```go
paths, err := fpgagzip.FourCorePaths(fpgagzip.DevicePaths{
    User: "/dev/xdma0_user", H2C: "/dev/xdma0_h2c_0", C2H: "/dev/xdma0_c2h_0",
}, 4)
if err != nil { return err }
pool, err := fpgagzip.NewDevicePool(paths)
if err != nil { return err }
// 多个 goroutine 各自执行下面的完整事务：
hw, err := pool.Acquire(ctx)
if err != nil { return err }
defer hw.Close()
options := fpgagzip.DefaultOptions()
options.RingSlots = 4
options.H2CSlotBytes = 1 << 20
_, err = fpgagzip.CompressReader(ctx, input, inputSize, output, hw, options, nil)
```

上例是函数片段；每个请求需要自己的输入/输出 writer，`defer hw.Close()` 应位于单个请求函数内。四核显式启用，不根据 `/dev/xdma*` 节点数量猜测 bitstream 能力。设备路径可对应其他卡，但必须实现相同的协议与四核寄存器布局。

单文件 CLI 也可显式使用全部通道：

```bash
sudo ./bin/fpga-gzip input.bin output.gz \
  --cores 4 --chunk-bytes 64MiB --max-procs 8
```

`--cores` 默认为 1。多核模式把输入按 `--chunk-bytes` 上限切分，各分块作为
独立 gzip member 并行压缩，再按原顺序拼接；文件较小时会继续细分，使请求的
核数尽量都有工作。每个 member 的 gzip 完整性和输入 CRC 仍单独校验。

```bash
mkdir -p bin
CGO_ENABLED=0 GOWORK=off go build -trimpath -o bin/fpga-gzipd ./cmd/fpga-gzipd
bin/fpga-gzipd --unix /run/fpga-gzip.sock \
  --cores 4 --workers 4 --max-procs 8 \
  --ring-slots 4 --h2c-slot-bytes 1MiB --c2h-request-bytes 32KiB
```

配置仍由 `github.com/lfhy/flag` 统一解析，新增 `[device] cores = 4` / `FPGA_GZIP_CORES`。`--workers=0` 默认使用核心数量，超过核心数量的请求仍通过有界队列等待。认证默认关闭，原 `--auth-token` 和 `WithAuth` 保持不变。同一张卡运行测试、SDK daemon 或 compactfs worker 时应只保留一个硬件所有者。

## 复位与数据校验

原来每个 chunk 写入软复位后固定等待 1 毫秒，才开始观察状态。现在立即读状态，并等待 RESET_BUSY、BUSY、DONE、ERROR 全部清零；如果仍保留旧任务状态则继续等，超时和取消仍生效。没有缩短 RTL 的 256 周期复位脉冲，也没有跳过复位。读取同一设备用于确认 posted write 的机制见 [Linux device I/O 文档](https://docs.kernel.org/driver-api/device-io.html)。

输出仍检查 gzip 头、输入 CRC32、ISIZE、完整计数和最终状态。C2H 仍使用 32 KiB，非最终请求按 8 字节对齐。不能只增大请求来绕开 FIFO 或 packetizer 限制。

## 结果与复现

证据目录：`/data1/fpga-compression-optimize-20261031-235541`。日期跨越本地午夜；报告采用结束日期 2026-11-01。未清空 Linux page cache。内存测试输入为已有 backend 对象解压后的 4 MiB 真实 fio / 50% chunk，加入不同核心/轮次标签，每核压缩 16 块、三轮。输入预先加载，输出预先分配在 RAM；计时包括设备开关和整组压缩，完整解压及字节比较在计时之外。

| 内存测试，三轮中位数 MiB/s | 单核 / 4 P | 四核 / 8 P |
| --- | ---: | ---: |
| 各核不同输入，原复位等待 | 428 | 2482 |
| 各核不同输入，按状态复位 | 435 | 3059 |

四核相比单核使用了更多独立硬件和 CPU 调度资源，不能称为单核算法提速。四核 16 P 没有稳定优于 8 P；本轮建议独立 worker 使用 8 P，FUSE 调度设置不变。两组复位测试依次执行，保留逐轮范围；不将其幅度视为固定的生产环境增益。

compactfs 同一二进制只改变核心/worker P 数的 10 GiB 三轮交替测试：单核 4 P 的含后台排空中位数约 426 MiB/s，四核 8 P 约 1364 MiB/s。前台 raw 缓存写入分别约 1839 / 1620 MiB/s，四核后台同步访问 NVMe 后前台并没有更快。worker CPU 用时约 22.28 / 22.83 秒每 10 GiB，峰值 RSS 中位数约 87 / 141 MiB。压缩后/原始大小均约 53.64%，CPU 回退为 0。

最终按状态复位的四核组合另跑三轮，compactfs 含后台排空中位数 1476 MiB/s（1456–1478），前台 1605 MiB/s。该阶段与前述交替对照属于不同时间窗口。10 GiB raw local fsync 后强杀挂载与已停止的 worker，重挂四核排空及 backend CRC 通过，CPU 回退为 0。完整过程见 [compactfs 四核报告](https://github.com/lfhy/compactfs/blob/main/docs/benchmark-fpga-multicore-20261101.md)。文件系统测试区分缓存写入、压缩后端排空、raw 热读和真实 backend 解压；每次发布 2560 个新块，完整 CRC 通过。

```bash
# 不要与同卡的其他 DMA 程序同时运行
CGO_ENABLED=0 GOWORK=off go run ./examples/benchmark-cores \
  -source /data1/fixture/object.gz -iterations 16 -rounds 3 -output /data1/cores.json
CGO_ENABLED=0 GOWORK=off go run ./examples/verify-cores \
  -cores 4 -output /data1/core-boundaries.json
```

`verify-cores` 覆盖四核、不同核心标签、random/zero/text 三类数据，以及 1 字节、8/64 字节、4 KiB、8 KiB、shorttail72 和约 1 MiB 的边界，共 456 例，完整解压逐字节比较。本次全部通过。四核 Unix HTTP daemon 还完成 32 个不同的 4 MiB 请求，共 128 MiB，成功数 32、失败数 0、忙拒绝 0，所有响应完整解压比较通过。该 HTTP 用例验证服务与租借，不作为长时间持续吞吐结论。

SDK nocgo 全量测试、vet，以及 device/pipeline/server/client race 检查通过。单元测试覆盖寄存器分区、重复通道拒绝、等待取消、失败打开归还、关闭顺序和复位时旧 DONE 状态不能提前放行。

这轮没有重新评估 HLS 匹配长度、字典、Huffman 或 FIFO 资源，也没有承诺任意输入达到同样压缩率或吞吐。先使用已有四核资源；后续单核算法改动需要单独的压缩率、完整正确性与布局布线验证。
