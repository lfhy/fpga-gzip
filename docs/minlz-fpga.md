# MinLZ FPGA profile

This implements the MinLZ specification v1.0.

MinLZ 使用算法 ID `5`，与现有 raw/gzip/LZ4/Zstd 共用版本化 shell、XDMA、
AXI-Lite、TLAST/EOP、核租借和整卡切换。每个镜像只包含一种算法。
SDK 和 MinLZ 官方 Go 解码器要求 Go 1.25 或更新版本，不使用 cgo。

## 编码器范围

本实现按[官方格式规范](https://github.com/minio/minlz/blob/v1.2.3/SPEC.md)
输出标准 MinLZ 流，能由 `github.com/minio/minlz v1.2.3` 直接解码：

- 每个 DMA 引擎配置四条 64 位匹配通道，整卡配置四个 DMA 引擎、250 MHz。
  配置频率不是板测性能；镜像必须通过完整布线与时序检查后才可装载。
- 独立块为 16 KiB；搜索 8 字节对齐的完整 64 位匹配，一个哈希候选，合并连续
  相同距离的匹配。窗口限于当前块，策略与官方 CPU 编码器不同。
- 输出 literal、Copy1、Copy2、repeat；不生成 fused copy、Copy3、字典或索引。
  这是编码策略的子集，不是私有文件格式。官方解码器支持读取该子集。
- 类型 2 压缩块或类型 1 原文块，每块携带原文的 masked CRC32C；流末尾包含
  类型 `0x20` EOF 及完整输入长度。流标识声明最大块 16 KiB。
- 尝试压缩的输出达到原块大小后截断该尝试，并选择保留的原文。尝试 FIFO 能容纳
  整块，原文和尝试编码一同排出；被截断的尝试不会作为压缩数据发布。
- 输入 CRC32C 使用 64 位并行变换，最后不足 8 字节单独处理。字典保持真实 RAM
  依赖，没有将同地址冲突声明为不存在。

SDK 对完整输出使用有界官方 Go 解码器，检查块 CRC32C、EOF、长度，并比对解码后
CRC32 与输入。校验时间包含在 SDK 吞吐中。编码通道基准将完整解压比对放在计时后，
两种计时分别报告，不能混用。

## 构建和使用

从 SDK 根目录运行，生成文件置于新的目录。HLS 头文件会和 C++、Tcl、测试向量、
解码验证器一同冻结并记录 SHA256：

```sh
VITIS_HLS_BIN=/opt/Xilinx/2025.2/Vitis/bin/vitis-run \
  firmware/multialgo/hls/build.sh minlz /data1/my-minlz-hls \
  /data/lsw/Vitis_Libraries-2021.1 cosim
```

`csim` 验证 130 个输入；`cosim` 验证 25 组 C/RTL 输出；第五个参数 `dma`
另测 3 组与板测同种子的随机输入，包含 1 MiB。每份输出都由官方 Go 解码并逐字节比较。
普通文件、短尾、16 KiB 块边界和更长输入均需在上板后复测。

完整工程使用同一构建脚本，HLS 仓库指向刚导出的 `sol1/impl/ip`：

```sh
VIVADO_BIN=/opt/Xilinx/2025.2/Vivado/bin/vivado \
  firmware/multialgo/build.sh minlz /data1/my-minlz-card \
  /path/to/pcie-project /path/to/gzip-ip \
  /data1/my-minlz-hls/hls-minlz/sol1/impl/ip /path/to/reference.dcp
```

构建器在 setup/hold、时钟覆盖和 DRC 检查通过后生成 manifest。
装载流程、驱动准备与回滚见[固件指导](../firmware/README.md)。

```sh
CGO_ENABLED=0 GOWORK=off go build -o bin/fpga-fw ./cmd/fpga-fw
CGO_ENABLED=0 GOWORK=off go build -o bin/fpga-compress ./cmd/fpga-compress
bin/fpga-fw switch --manifest /data1/my-minlz-card/manifest.json \
  --pci 0000:39:00.0 \
  --target localhost:3121/xilinx_tcf/Digilent/210251A08870 \
  --vivado /opt/Xilinx/2025.2/Vivado/bin/vivado
bin/fpga-compress --algorithm minlz --input data.bin --output data.mz
```

公开 SDK 可用 `accelerator.MinLZ`，或根包
`CompressAlgorithm(ctx, "minlz", src, size, dst, hw, options, nil)`。
HTTP/Unix daemon 通过 `--algorithms gzip,lz4,zstd,minlz,raw` 启用请求；
客户端使用 `firmware.MinLZ`，HTTP 路径为 `/compress?algorithm=minlz`。
默认仍只接受 gzip，Token 认证仍默认关闭。

新格式的实际压缩率取决于块长和匹配策略，不能套用官方 CPU Fastest/Balanced 的
数字。CPU 候选比较见[算法报告](algorithm-candidates-20261101.md)。
