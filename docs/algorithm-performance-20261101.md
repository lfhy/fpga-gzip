# 多算法 FPGA / CPU 性能对照（2026-11-01）

同一台 KU5P 与 Xeon Gold 6336Y，比较 gzip、LZ4、Zstd、raw 的内存编码吞吐。
四个镜像与功能验收相同；gzip/LZ4/raw 四核，Zstd 两核、125 MHz。
全部数值为三轮中位数，GB/s 使用十进制。比例为输出 / 输入，越小越省空间。
[机器可读结果](algorithm-performance-20261101.json)保留每组的三轮吞吐、最小值、最大值、
压缩比、进程 CPU 时间和原始报告 SHA256。

## 约 50% 可压缩数据，64 MiB chunk

FPGA 两列均为当前镜像的整卡合计；CPU 列是 1 或 8 个独立编码器，使用相同数据。
CPU 4 路结果也保存在 JSON 中，可按相同并发流数比较。

| 算法 / FPGA 核数 | FPGA 编码通道 GB/s | FPGA SDK GB/s | CPU 1 路 GB/s | CPU 8 路 GB/s | 输出比例 FPGA / CPU |
| --- | ---: | ---: | ---: | ---: | ---: |
| gzip (4 核) | 6.856 | 6.869 | 0.597 | 4.702 | 53.63% / 50.12% |
| lz4 (4 核) | 0.280 | 0.279 | 2.311 | 17.096 | 50.90% / 50.22% |
| zstd (2 核) | 0.160 | 0.160 | 2.712 | 13.140 | 50.57% / 50.01% |
| raw (4 核) | 6.958 | 6.811 | 7.658 | 37.609 | 100.00% / 100.00% |

`raw` 的 CPU 列为内存复制，FPGA 列为完整 H2C→透传→C2H；此行作为无压缩基线。
CPU 使用纯 Go 的 stdlib gzip level 1、LZ4 默认快速模式（64 KiB 独立块与 checksum）、
Zstd SpeedFastest（每个 encoder concurrency=1）。CPU 编码器复用并预热，计时内包含
每个 chunk 的 Reset / 编码 / Close 或 EncodeAll。CPU 与 FPGA 的块大小、窗口及编码策略
不同，输出符合相同格式，压缩比并不相同。

两种 FPGA 口径：

- **编码通道**：输入 CRC、真实 DMA、硬件编码、设备打开关闭和计数/结束状态均计时；
  输出格式与完整内容校验放在计时后。每个结果仍需完整解压比对成功才发布。
- **SDK**：使用实际 `accelerator.CompressReader`；LZ4/Zstd 的完整 CPU 解码与 CRC 校验
  在调用内部，计入时间；gzip 使用帧头和 trailer 快速校验，raw 校验 CRC 与长度。

## 输入内容的影响，64 MiB chunk

| 算法 | 数据 | FPGA SDK 整卡 GB/s | CPU 8 路 GB/s | 输出比例 FPGA / CPU |
| --- | --- | ---: | ---: | ---: |
| gzip | fixture | 6.869 | 4.702 | 53.629% / 50.123% |
| gzip | text | 7.024 | 10.058 | 2.778% / 0.704% |
| gzip | random | 6.532 | 3.207 | 105.533% / 100.008% |
| lz4 | fixture | 0.279 | 17.096 | 50.903% / 50.219% |
| lz4 | text | 0.379 | 12.428 | 1.987% / 0.533% |
| lz4 | random | 0.219 | 11.178 | 100.012% / 100.006% |
| zstd | fixture | 0.160 | 13.140 | 50.568% / 50.012% |
| zstd | text | 0.215 | 16.343 | 2.261% / 0.021% |
| zstd | random | 0.120 | 10.826 | 100.134% / 100.005% |
| raw | fixture | 6.811 | 37.609 | 100.000% / 100.000% |
| raw | text | 6.831 | 37.613 | 100.000% / 100.000% |
| raw | random | 6.801 | 37.555 | 100.000% / 100.000% |

`fixture` 是 fio 生成的约 50% 可压缩数据；`text` 为固定重复文本；`random` 为相同
种子的确定性随机数据。随机数据仍执行所选压缩算法，保留编码器自身的标准 raw-block
择优行为；没有启用 compactfs 的预测、去重或 CPU 回退。

## 4 MiB chunk 对照

约 50% 可压缩输入，沿用 compactfs 默认 chunk 大小，比较压缩组件自身的吞吐：

| 算法 | FPGA SDK 整卡 GB/s | CPU 8 路 GB/s |
| --- | ---: | ---: |
| gzip | 5.631 | 4.842 |
| lz4 | 0.272 | 18.153 |
| zstd | 0.154 | 17.812 |
| raw | 5.018 | 43.607 |

## 单路与 CPU 占用

约 50% 可压缩输入、64 MiB chunk：

| 算法 | FPGA SDK 单核 GB/s | CPU 单编码器 GB/s |
| --- | ---: | ---: |
| gzip | 1.789 | 0.597 |
| lz4 | 0.070 | 2.311 |
| zstd | 0.080 | 2.712 |
| raw | 1.803 | 7.658 |

同一输入下，整卡 / CPU 8 路测试的平均进程 CPU 占用，单位为等效 CPU 核：

| 算法 | FPGA 编码通道 | FPGA SDK | CPU 8 路 |
| --- | ---: | ---: | ---: |
| gzip | 1.33 | 1.34 | 7.97 |
| lz4 | 0.06 | 0.27 | 7.91 |
| zstd | 0.04 | 0.28 | 7.86 |
| raw | 1.89 | 2.27 | 7.89 |

CPU 占用取进程 user+system CPU 秒 / 墙钟秒，包含编码阶段该进程全部线程；
不包含计时外的独立解码验证，也不代表整机 IRQ 开销或功耗。

## 测试条件与复现

- Go 1.26，`CGO_ENABLED=0`，所有模式固定 P=8；CPU 亲和 `8-23,56-71`，
  XDMA IRQ 亲和 `0-23,48-71`，都在 NUMA0。
- 1 MiB H2C、4 个 slot、1 MiB EOP C2H；本轮没有修改驱动、RTL 或 FPGA 算法。
- 每路每组输入 128 MiB；CPU 1/4/8 路，FPGA SDK 单核与整卡，编码通道仅整卡。
  每个组合运行三轮，共 396 组、173.25 GiB 输入。
- 数据、输出空间、CPU 编码状态在计时前准备。保留所有输出，随后完整解压并逐字节
  比较；所有模式均通过。每路、每次迭代具有不同的 8 字节标签。
- 测试数据与输出均在 RAM，JSON 和日志写到 `/data1`；磁盘读写、数据生成与预分配
  不计入编码速度。每组数据量为 128 MiB × 路数，属于组件对照测试。
- fio fixture 解码后 4 MiB，SHA256：
  `d378f587d40daec3d80194bf95ba89d3b8851097cd5adc538ebff7955f5b77a1`。

源码版本 `cdb915b`，工具为 SDK 的
`examples/benchmark-algorithms`。在 SDK 根目录构建：

```sh
CGO_ENABLED=0 GOWORK=off go build -o /data1/benchmark-algorithms ./examples/benchmark-algorithms
taskset -c 8-23,56-71 /data1/benchmark-algorithms \
  -engine cpu -algorithm gzip -source /path/to/fixture.gz \
  -lanes 1,4,8 -sizes-mib 4,64 -per-lane-mib 128 -rounds 3 -procs 8 \
  -output /data1/cpu-gzip.json
# 先装载匹配镜像；Zstd 的整卡 lanes 为 2
taskset -c 8-23,56-71 /data1/benchmark-algorithms \
  -engine fpga-sdk -algorithm gzip -source /path/to/fixture.gz \
  -lanes 1,4 -sizes-mib 4,64 -per-lane-mib 128 -rounds 3 -procs 8 \
  -output /data1/fpga-gzip.json
```

切换镜像及 `fpga-pipeline` 的实际命令、IRQ 设置、完整原始报告、冻结源码与二进制
保存于 `/data1/fpga-algorithms-20261101-snkvnq9w`。测试结束恢复已验收的新版 gzip 镜像。
正式依赖、GOPROXY 和 `/root/go.work` 保持原状。

## 结果解释

在约 50% 可压缩输入上，gzip 的 FPGA SDK 吞吐为 CPU 8 路的 1.46 倍。
平均进程 CPU 占用为 1.34 核，对照 CPU 为 7.97 核。
重复文本这组数据上，CPU 编码更快且输出更小。

LZ4/Zstd 当前硬件实现明显落后于优化后的 CPU 编码器：即使把同步解码校验移出计时，
整卡通道也只有 0.280 / 0.160 GB/s；
对应 SDK 为 0.279 / 0.160 GB/s。
结合源码，主要优化对象是 HLS 编码流水线：LZ4 是 32 KiB 块的读取、单字节压缩与
择优写出串行进行；Zstd 每个引擎只有一个 LZ77 lane，运行于 125 MHz，频率 RAM 的
真实依赖也限制部分循环的启动间隔。提升并行字节数与块间重叠，才有机会大幅增加吞吐。
这是基于实测与当前实现的判断，新的实现仍需重新做标准解码和板上验证。

本报告测量压缩组件；compactfs 的前台写入由 NVMe 原始缓存先接收，后台再编码与
提交 backend。持续写入超过缓存后能否继续保持速度，还取决于压缩、backend 与缓存
回收的排空能力。先前的 7.23 GB/s gzip 回归使用每组 8 GiB，长测见容量报告；
本轮每组 128 MiB × 路数，三轮中位数有正常短测波动。

实现见[HLS 适配器](../firmware/multialgo/hls/compress.cpp)，持续 gzip 测试见[容量报告](capacity-20261101.md)。
