# MinLZ FPGA 实现与本机验收（2026-11-01）

本机 KU5P 已实现标准 MinLZ 压缩核、SDK、Unix/HTTP 服务与 compactfs worker。
四个 DMA 引擎直接运行在 250 MHz，每引擎四条匹配通道；这是独立可切换镜像。
解压仍使用官方纯 Go 实现。本次数据不支持把 MinLZ 的 CPU 性能直接套用到 FPGA。

## 同一真实文件集

完整 Silesia：12 文件、211,938,580 字节，来源为
[官方语料站点](https://sun.aei.polsl.pl/~sdeor/index.php?page=silesia)。每路完整处理一遍，
三轮；文件按最大 4/64 MiB 分片，不拼接不同文件。输入预载 RAM，编码计时不含读盘。
单位为十进制 GB/s，表中为中位数，括号为三轮范围；压缩比例为输出/输入，越低越省空间。

| 实现 | 并行路数 | 最大 4 MiB GB/s | 最大 64 MiB GB/s（范围） | 64 MiB 输出/输入 | 进程平均 CPU 核 |
| --- | ---: | ---: | ---: | ---: | ---: |
| CPU gzip BestSpeed | 8 | 0.785 | 0.745 (0.745–0.747) | 37.73% | 7.84 |
| CPU MinLZ Fastest | 8 | 3.755 | 3.758 (3.396–3.764) | 42.87% | 7.96 |
| FPGA gzip 编码通道 | 4 | 5.426 | 6.723 (6.677–6.725) | 48.55% | 1.42 |
| FPGA MinLZ 编码通道 | 4 | 4.237 | 4.979 (4.852–5.155) | 79.91% | 1.35 |
| FPGA gzip SDK | 4 | 5.397 | 6.582 (6.243–6.640) | 48.55% | 1.36 |
| FPGA MinLZ SDK | 4 | 2.869 | 4.347 (4.316–4.430) | 79.91% | 3.89 |

CPU 使用 Go 1.26.0：gzip BestSpeed、MinLZ v1.2.3 Fastest，MinLZ 内部块 1 MiB、
每路编码器并发 1，复用已预热编码状态。独立基准 GOMAXPROCS=8；FPGA 为四个物理
DMA 引擎，CPU 为八路软件编码器。CPU 核数来自进程 rusage，不包含进程外 IRQ。
构建位于另一 NUMA 节点，基准固定到 `8-23,56-71`。

MinLZ SDK 在计时内完整解码、检查块 CRC32C、EOF，并对比输入 CRC32 和长度。
gzip SDK 只校验其头部和 CRC32/ISIZE 尾部；两者 SDK 校验成本不同。
编码通道将全部输出保留，完整解压逐字节比较放在计时后；只有全部通过才发布数据。
CPU 与 SDK 的每份输出也在计时后再次完整比较。没有预测跳过、去重或 CPU 回退。

当前 FPGA 使用 16 KiB 块、8 字节对齐的整字匹配，一个哈希候选，属于独立编码策略，
并非官方 CPU Fastest 的 RTL 移植。真实文件的压缩率明显弱于 CPU MinLZ 和 FPGA gzip；
该镜像作为实验选项保留，不能仅凭算法名称选择。下一步空间收益应从字节粒度与短匹配入手。

## 相同合成数据的 RAM 对照

64 MiB chunk，每路 128 MiB，每项三轮；全部输出校验。fixture 为此前 gzip 测试中的
约 50% 可压缩数据，另测重复文本和确定性随机数据。下面每格为 GB/s / 输出比例。
这些是短时组件基准，不是 FUSE、NVMe 或持续后端提交速度。

| 实现 | 路数 | fixture | 重复文本 | 随机 |
| --- | ---: | ---: | ---: | ---: |
| CPU MinLZ Fastest | 8 | 25.124 / 50.001% | 44.351 / 0.008% | 32.463 / 100.001% |
| FPGA MinLZ 编码通道 | 4 | 6.933 / 50.104% | 7.027 / 0.946% | 6.716 / 100.049% |
| FPGA MinLZ SDK | 4 | 6.414 / 50.104% | 4.847 / 0.946% | 6.079 / 100.049% |

最初构建遗漏了算法选择分支，使 MinLZ 落入 Zstd 的 125 MHz 桥。
修复后直接连接 250 MHz，并新增 RTL 时钟路径测试及逐核构建检查。
125 MHz 的 fixture 编码通道中位数为 3.770 GB/s，
250 MHz 为 6.933 GB/s，提升 1.84 倍。
原始对照以 `125` 单独标注，不作为最终镜像；最终结果均来自构建 ID `263746753728771`。

## 正确性与时序

- HLS 原生模型 130 组，C/RTL 25 组，额外 3 组 DMA 随机向量（含 1 MiB），全部官方解码并逐字节比较。
- 完整 Silesia 原生模型 12 个文件全部解码比较，输出比例约 79.905%。
- 250 MHz 板卡直连 324 组边界、72 组纯符号/二符号、48 组含 64 MiB 的输入，四核均通过。
- Unix/HTTP 服务 48 组通过；错误算法在 DMA 前拒绝，输入/输出计数保持不变。
- MinLZ/LZ4/Zstd/raw 选择路径、背压、短尾及软复位 RTL 通过。旧错误分支的负例能被新测试拒绝。
- 最终 WNS=0.012 ns、WHS=0.009 ns，无未约束时钟/内部端点，DRC error=0。
- SDK nocgo 全量测试、vet 与关键包 race；compactfs 普通及 FPGA 构建全量测试、vet、关键包 race 通过。产品二进制 CGO_ENABLED=0。

整卡资源：LUT 130742/216960，寄存器 126699/433920，
BRAM tile 454/480，URAM 8/64。
HLS 每 DMA 引擎估算为 31,879 LUT、132 BRAM18K、2 URAM。
位流 SHA256：`ae6e67799a7d1795d3b7d12396b7f5d49c4962f4ede0598b1272f3ce4297a4b4`。

尝试四 bank 偶数字节候选后，模型输出比例降至 74.509%，但估计关键路径达到 15.064 ns，
远超 4 ns 目标，未合入。实验源码和报告单独保存，不能作为已验收镜像。

## compactfs 实际挂载

独立 daemon/worker，4 MiB chunk，worker 4 并发、GOMAXPROCS=8，原始写回 cache、
元数据日志、backend 均在 `/data1`。禁用压缩预测，使用全新 backend。
1 GiB 的 50% 数据产生 256 个 FPGA MinLZ 对象，128 MiB 随机数据产生 32 个 raw 对象；
新编码/新 backend put 均为 288，CPU 回退、编码错误与队列错误为 0，dirty/pending 排空。

卸载后切回原 gzip 镜像，追加 128 MiB 的 40% 数据，新增 32 个 gzip 对象。
随后复制关闭的元数据日志，以 `--cache "" --writeback=false --read-cache-bytes 0
--read-ahead 0 --read-raw-refill=false` 重挂，完整读回 1.25 GiB，fio CRC32C 全通过。
backend get=1280，decode=1152，compactfs cache hit=0。1 MiB 读请求会重复解码
4 MiB chunk，因此这里用于完整性验证；Linux page cache 保留，不宣称物理冷盘吞吐。
125 MHz 预验证镜像同样完成该混合数据流程。最终已经恢复 gzip 构建 ID `86950339975258`。

固件原文块仍有 MinLZ 帧开销；compactfs 按完整流大小决定是否保留编码，随机数据
直接保存原文，backend 对象无膨胀。元数据继续使用 append-only 二进制日志。

## CPU 回退缓冲复用

每个 MinLZ codec 最多保留四个空闲编码器，失败后清理输出引用和错误，再用于后续请求。
固定 4 MiB fixture，预热后每轮 512 块、三轮，输出 buffer 复用：分配中位数从
2108259 降至 2509 字节/块（减少 99.88%），
CPU 微基准从 5.30 提升至 7.15 GB/s。该微基准每轮校验最后一次输出，
另有错误后复用、不同等级和 16 路并发独立流测试；不作为 FPGA/FUSE 吞吐。

## 源码、归档与复现

SDK 算法实现 `975a7cb`、时钟修复 `668d415`；compactfs MinLZ `f49ed74`、
缓冲复用 `a85a538`。SDK 和 MinLZ 依赖要求 Go >=1.25，保持 nocgo。
compactfs 正式 SDK 依赖仍为 v0.0.2，新 worker 通过临时 modfile 使用本地新版 SDK。
没有修改 GOPROXY，没有创建 `/root/go.work`，没有写 Flash 或 push。

本机完整归档：`/root/FPGA-4IP/qz01_compress_4ip/validated/20261101-minlz`。`minlz/` 含 bitstream、manifest、HLS/RTL 源码与 IP、
路由 DCP、时序和验证记录；`performance/minlz/` 含原始 JSON、基准源码及构建信息。
`switch.sh inspect|minlz|gzip|lz4|zstd|raw` 查询或易失切换；既有四种镜像原样保留。
完整语料文件的大小/SHA256、命令参数、逐轮数据和 CPU 用量见同目录 JSON 报告。

```sh
/root/FPGA-4IP/qz01_compress_4ip/validated/20261101-minlz/switch.sh minlz
# 新 worker 接入；cache 和 backend 的路径按实际环境指定
cd /root/compactfs
scripts/build-fpga-worker.sh /root/fpga-gzip /root/compactfs/bin/compactfs-fpga-worker
# 挂载和构建参数见 MinLZ 指导
/root/FPGA-4IP/qz01_compress_4ip/validated/20261101-minlz/switch.sh gzip
```

机器可读数据：[minlz-fpga-20261101.json](minlz-fpga-20261101.json)。
