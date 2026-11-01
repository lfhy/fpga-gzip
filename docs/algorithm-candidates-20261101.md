# 高吞吐压缩算法候选与本机实测（2026-11-01）

MinLZ 和 Snappy 值得继续评估。这里完成的是无需 cgo 的 CPU 编码比较，新候选尚未
接入 compactfs，也没有对应的本卡 FPGA 镜像。本轮先检验速度、空间收益与实现条件，
没有根据 CPU 数字推算它们上板后的吞吐。

[MinLZ](https://github.com/minio/minlz) 使用固定的字节对齐 LZ77 编码，提供独立块、
带校验的流格式和多个压缩等级。[S2 作者的说明](https://github.com/klauspost/compress/blob/master/s2/README.md)
已把 MinLZ 作为后续改进方向，S2 主要继续修复错误。对独立算法 worker，MinLZ 是
优先考察的新格式；对需要广泛格式互通及较简单硬件编码的场景，保留
[Snappy](https://github.com/google/snappy) 作为候选。硬件适配难度是根据格式作出的判断，
不是本轮已经验证的硬件性能。

## 真实文件：完整 Silesia，最大 chunk 4 MiB

输入为官方 [Silesia corpus](https://sun.aei.polsl.pl/~sdeor/index.php?page=silesia) 的
12 个文件，合计 211,938,580 字节，包含文本、源码、程序、数据库及图像。每文件独立切块，
每路读取所有文件，共 57 个 chunk；保留末块的实际长度，没有重复前缀填充，没有跨文件连接。
表内为三轮中位数，GB/s 为十进制；输出/输入越低越节省空间。

| 编码器 | 单路 GB/s | 8 路 GB/s | 8 路三轮范围 GB/s | 输出/输入 | 编码进程等效 CPU 核 |
| --- | ---: | ---: | ---: | ---: | ---: |
| gzip BestSpeed | 0.098 | 0.783 | 0.739–0.786 | 37.764% | 7.95 |
| LZ4 fast | 0.256 | 2.031 | 2.029–2.043 | 47.762% | 7.94 |
| Zstd SpeedFastest | 0.197 | 1.542 | 1.536–1.557 | 34.899% | 7.89 |
| Snappy 兼容（S2 实现） | 0.533 | 4.222 | 3.777–4.224 | 48.907% | 7.97 |
| S2 default | 0.495 | 3.926 | 3.402–3.941 | 45.789% | 7.94 |
| MinLZ SuperFast (-1) | 0.779 | 6.167 | 5.122–6.179 | 52.490% | 7.94 |
| MinLZ Fastest (1) | 0.478 | 3.809 | 3.809–3.814 | 42.875% | 7.97 |
| MinLZ Balanced (2) | 0.288 | 2.167 | 2.138–2.219 | 38.405% | 7.61 |

本组 MinLZ 的取舍为：Balanced 比本轮标准库 gzip 快 2.77×，压缩后体积增加 1.70%；Fastest 比本轮标准库 gzip 快 4.86×，压缩后体积增加 13.54%；SuperFast 比本轮标准库 gzip 快 7.87×，压缩后体积增加 39.00%。

Silesia 是较老的公开参照集，结果不能代表所有用户数据。较快模式通常留下更大的文件，
因此不宜只按吞吐选择默认值。完整的最大 64 MiB、1/8 路结果也保存在 JSON：
因所有文件都小于该上限，这组实际是每文件一个流，共 12 个流，不是固定 64 MiB 输入。

## 与此前板测相同的约 50% 可压缩 fixture，64 MiB chunk

这一组与之前 FPGA gzip/LZ4/Zstd 的 fixture 相同。每路 128 MiB，每项三轮；
8 路每轮输入 1 GiB。下面仍全部是 CPU 编码数字。

| 编码器 | 单路 GB/s | 8 路 GB/s | 8 路三轮范围 GB/s | 输出/输入 | 编码进程等效 CPU 核 |
| --- | ---: | ---: | ---: | ---: | ---: |
| gzip BestSpeed | 0.587 | 4.593 | 4.583–4.601 | 50.123% | 7.94 |
| LZ4 fast | 2.309 | 17.030 | 11.936–17.099 | 50.219% | 7.86 |
| Zstd SpeedFastest | 2.867 | 13.394 | 9.628–13.395 | 50.012% | 7.87 |
| Snappy 兼容（S2 实现） | 5.112 | 29.926 | 24.193–29.969 | 52.518% | 7.88 |
| S2 default | 5.003 | 24.496 | 19.149–24.654 | 50.002% | 6.58 |
| MinLZ SuperFast (-1) | 5.084 | 25.133 | 24.405–29.823 | 50.948% | 7.16 |
| MinLZ Fastest (1) | 4.795 | 28.678 | 28.675–29.102 | 50.001% | 7.78 |
| MinLZ Balanced (2) | 3.804 | 23.827 | 16.917–24.041 | 50.010% | 7.81 |

fixture 包含较易压缩的内容，因此比真实文件集明显更快。重复文本和确定性随机数据的
4/64 MiB、1/8 路结果也完整保留；重复的小 chunk 可能受 CPU cache 帮助，不能当成
任意大型文件的持续速度。

## 现有 FPGA 为什么仍可能比新算法快

此前同 fixture 的[固件优化实测](algorithm-optimization-20261101.md)中，
LZ4 整卡 SDK 为 3.714 GB/s、Zstd 为 0.605 GB/s，低于现有 gzip。gzip 单独的
96 GiB/轮[持续测试](capacity-20261101.md)为 7.289 GB/s。它们的测试输入总量不同，
持续测试数字只作架构背景，不混入本轮 CPU 排名。

本卡现有 shell 是四路 64 位、250 MHz：

```text
4 × (64 / 8) × 250,000,000 = 8,000,000,000 B/s
```

这是当前 shell 的理想输入带宽，包含空隙、背压、复位和输出约束后会更低；
不是 PCIe Gen3 x16 链路的带宽上限。7.289 GB/s 已达到该输入上限约 91%。
仅替换算法无法突破这条上限。要明显超过 8 GB/s，需加宽或改造 XDMA/AXIS 数据通路，
并一起验证编码、输出打包、DMA 与主机调度。

位宽见 `firmware/multialgo/rtl/top_4core.v` 和 `gzip_min_shell_64.v`；已验收镜像的
`gzip/reports/timing_summary.rpt` 记录 `axis_clk` 周期 4.000 ns。gzip 的成熟并行流水线
与新 LZ4/Zstd 的块初始化、LZ77、输出汇合或熵编码结构不同，不能用算法名称代替实现评估。

[AMD/Xilinx 官方 Vitis 2022.1 数据](https://xilinx.github.io/Vitis_Libraries/data_compression/2022.1/source/results.html)
也说明了实现条件的重要性：其 U200 单引擎 8 位 LZ4/Snappy 编码约 290 MB/s，
8 引擎约 2.2 GB/s。该数据不属于本卡；官方 64 位 Snappy 的 1.97 GB/s 属于解码，
不能拿来当编码速度。直接换成现成逐字节 HLS Snappy 核，不足以证明能超越现有 gzip。

更值得尝试的硬件路线是多个字节并行匹配、减少匹配扩展的反馈依赖、并行 token 打包，
先用 LZ4 或 Snappy 的较简单格式验收吞吐和压缩率，再评估 MinLZ。
[相关 LZ4 架构论文](https://arxiv.org/html/2409.12433v1)给出了限制匹配和消除反馈环的方案，
其报告的 16.10 Gb/s 换算为 2.0125 GB/s，且存在压缩率取舍；不能将其当作
本卡单核 16 GB/s，也不能在资源未经综合和布线前直接乘以四。

## 参数、计时与校验

- 本机为双路 Xeon Gold 6336Y。固定 NUMA0 CPU `8-23,56-71`，Go 1.26.0，
  `GOMAXPROCS=8`，`CGO_ENABLED=0`，每路编码器内部并发为 1。允许库自带的 Go 汇编优化。
- gzip 为 Go 标准库 BestSpeed。LZ4 为 `pierrec/lz4/v4 v4.1.22`，fast、64 KiB
  独立块和校验。Zstd 为 `klauspost/compress v1.20.1` 的 SpeedFastest。
- Snappy 使用同版本 S2 的 `WriterSnappyCompat`，64 KiB 块，不是 Google C++ 库或
  原始 golang/snappy 的 CPU 实现。S2 使用默认等级、1 MiB 块。
- MinLZ 固定 `github.com/minio/minlz v1.2.3`，分别测 -1/1/2 等级，1 MiB 内部块。
  该依赖要求 Go 1.25；本轮运行于单独的临时模块，正式 SDK/compactfs 的依赖没有更新。
- 各格式内部块长不同，以上比较针对列明的实现与参数，不表示各算法的理论极限。
  允许编码器内置的不可压缩检测和原始块输出，没有外部预测跳过或去重。
- 数据读入 RAM、输入与输出预分配并触页、编码器预热后计时。计时包含 Reset、Write/EncodeAll
  和 Close；不包含数据生成、磁盘读写、页预热和后续完整解码。
- 每轮保留所有编码输出，再全部解码并逐字节比较，确认长度一致后才发布结果。
  CPU 核为计时区间进程 user+system 秒 / 墙钟秒，不包含后续解码，也不表示整机功耗。

本轮共 384 项、247.27 GiB 输入，全部输出完整解压比对通过；其中合成测试
288 项、162 GiB，Silesia 96 项、85.27 GiB。所有 CPU 基线均重新运行，
避免把旧 `klauspost/compress v1.17.11` 和新候选依赖混作同一轮结果。
[机器可读报告](algorithm-candidates-20261101.json)保留全部原始行、三轮范围、
命令、版本及 SHA256。

## 本机复现

完整基准源码、独立 go.mod/go.sum、二进制、源数据哈希及逐项日志位于：

```text
/data1/codec-candidates-20261101-wxaydnsn
```

`main.go` / `run.py` 为合成输入，`corpus-bench/main.go` / `corpus-run.py` 为完整真实文件。
`complete.json` 和 `corpus-complete.json` 分别记录验收；`summarize.py` 校验并汇总 384 项。
保持 `GOWORK=off`、现有 `GOPROXY=https://goproxy.cn`，在该独立目录可重建：

```sh
GOWORK=off CGO_ENABLED=0 go build -trimpath -o benchmark .
GOWORK=off CGO_ENABLED=0 go build -trimpath -o corpus-benchmark ./corpus-bench
```

运行参数逐项保存在机器可读报告的 `synthetic_run.commands` 和 `silesia_run.commands`，
可更换 `-output` 路径复跑。不要同时运行两个性能测试。Silesia 从官方站点下载，
ZIP 的 SHA256 为 `0626e25f45c0ffb5dc801f13b7c82a3b75743ba07e3a71835a41e3d9f63c77af`；
每文件 SHA256 和基准源码/二进制哈希一并保留在 JSON。

本轮未装载候选固件；板卡继续运行已验收 gzip，构建 ID `86950339975258`。
算法选择阶段的建议是：CPU worker 优先评估 MinLZ 的速度与空间取舍，
FPGA 优先改善多字节并行流水线与数据通路，然后用同数据集确认新格式是否胜出。
