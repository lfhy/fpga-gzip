# 独立 HLS 算法适配器

本目录公开 LZ4、Zstd 和 MinLZ 的 HLS 适配器、算法测试及 Vitis 上游补丁。
板级 DMA shell、寄存器 RTL、约束、shell 派生补丁、整卡构建和 shell 仿真
保存在独立私有工程中。公开 SDK 接受外部镜像 manifest，使用方法见
[固件与 SDK 指导](../README.md)。

| 算法 | 算法 ID | 公开实现 |
| --- | --- | --- |
| raw | 1 | 主机端格式与校验接口 |
| gzip | 2 | 主机端接口；压缩 IP 从外部提供 |
| LZ4 | 3 | Vitis 编码器的并行块适配器 |
| Zstd | 4 | Vitis 多核编码器适配器及正确性补丁 |
| MinLZ | 5 | 独立的 MinLZ HLS 编码器和 CRC32C 实现 |

这些算法 ID 与公开 SDK 的固件身份协议一致。最终 DMA 核数和运行时能力
由已装载镜像报告，不能仅凭 HLS 综合估算推断整卡性能。

## 依赖与许可

- Vitis HLS 2025.2；本机验证器件为 `xcku5p-ffvb676-2-e`。
- Vitis Libraries revision `f217398d03b018c2774816200e562750872d9c31`。
  构建依赖 `data_compression/L1/include/hw`、`security/L1/include`。
- Go 1.25 或更新版本，算法验证器使用纯 Go 解码器。

原创适配器按 MIT 发布。Vitis 库及其派生补丁保留 Apache-2.0；详见
[NOTICE](NOTICE) 和[上游许可证](LICENSE.Vitis-Libraries)。
本仓库的许可证不覆盖外部板级设计、gzip IP 或厂商工具。

## 构建与验证算法 IP

从 SDK 根目录运行，构建输出必须使用新目录：

```sh
VITIS_HLS_BIN=/opt/Xilinx/2025.2/Vitis/bin/vitis-run \
  firmware/multialgo/hls/build.sh lz4 /data1/my-compression-build/lz4-hls \
  /path/to/Vitis_Libraries cosim
```

算法可改为 `zstd` 或 `minlz`。脚本冻结 C++、Tcl、补丁、算法测试和 Go
解码验证器，记录输入 SHA256。`csim` 运行 115 组输入，MinLZ 为 130 组；
`cosim` 运行 25 组 C/RTL 联合仿真，并完整解码、逐字节比较。
验证失败会终止构建，不继续导出 IP。`synth` 仅生成候选，不代表通过验证。

可选第五个参数 `entropy` 覆盖全零、文本、随机、重复字符和二符号输入；
`random` 运行随机回归；`dma` 生成与板测相同种子的 32767、65536、1 MiB
输入，检查跨块背压和尾部处理：

```sh
VITIS_HLS_BIN=/opt/Xilinx/2025.2/Vitis/bin/vitis-run \
  firmware/multialgo/hls/build.sh zstd /data1/zstd-dma \
  /path/to/Vitis_Libraries cosim dma

CGO_ENABLED=0 GOWORK=off go run ./examples/verify-hls zstd \
  /path/to/cosim-output quick
```

算法 IP 可由私有板级工程集成。整卡实现需要在私有工程中独立检查 setup、
hold、时钟覆盖和 DRC，再生成 bitstream 与 manifest，并进行板上完整解码验证。
本仓库没有可直接构建整卡镜像的 shell 源码或脚本。

## 使用外部固件

取得有权使用且已验证的 bitstream 与 manifest 后：

```sh
bin/fpga-fw check --manifest /path/to/image/manifest.json
bin/fpga-fw inspect
CGO_ENABLED=0 GOWORK=off go run ./examples/verify-firmware \
  --algorithm lz4 --output /data1/lz4-board.json
CGO_ENABLED=0 GOWORK=off go run ./examples/verify-firmware \
  --algorithm lz4 --edge --output /data1/lz4-edge.json
```

装载、通道排空与回滚见[固件指导](../README.md)。每份输出使用对应的独立
CPU 解码器完整验证；固件身份不匹配会在 RESET、START、DMA 前拒绝。
算法 IP 的 C/RTL 仿真、板级 shell 仿真和实际板测是不同的验证阶段。

历史实测见 [gzip 吞吐](../../docs/gzip-throughput-20261102.md)、
[LZ4/Zstd 优化](../../docs/algorithm-optimization-20261101.md)及
[MinLZ FPGA](../../docs/minlz-fpga-20261101.md)。这些数据来自独立私有固件工程，
公开源码范围不改变既有板卡镜像或 SDK 兼容性。
