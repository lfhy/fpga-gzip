# Xilinx XDMA 驱动安装与验证

本文适用于本项目使用的 Xilinx XDMA endpoint。当前机器的目标设备是 `0000:39:00.0`，PCI ID 为 `10ee:903f`，但其他机器应以自己的 `lspci` 输出为准。

官方参考驱动源码：<https://github.com/Xilinx/dma_ip_drivers/tree/master/XDMA/linux-kernel>。

## 1. 确认内核和开发包

```bash
uname -r
test -e /lib/modules/$(uname -r)/build
gcc --version
make --version
```

如果 `build` 链接不存在，先安装与当前运行内核完全匹配的 kernel headers/devel 包。Alma/RHEL 系统通常需要：

```bash
sudo dnf install gcc make git kernel-devel-$(uname -r) kernel-headers-$(uname -r)
```

## 2. 下载并编译官方驱动

本机已确认官方 XDMA 2025.2 的同步等待会被 Go 的 SIGURG 提前唤醒，随后把未完成请求当成超时中止。
使用本项目的 [DMA 等待补丁](patches/xdma-2025.2-killable-dma-wait.patch)，避免长流随机失败或丢失流数据。
只读 mmap 输入还需要 [H2C 页权限补丁](patches/xdma-2025.2-readonly-h2c.patch)：
官方代码对两个方向都要求 `FOLL_WRITE`，导致只读输入在 DMA 提交前返回 `-EFAULT`。
补丁让 H2C 只固定可读页，C2H 保持可写页要求。SDK 对未修补驱动的零字节
`EFAULT` 会复制到自有可写缓冲后重试一次；已经提交部分字节或其他错误不重试。
使用新 EOP shell 时，继续应用 [EOP 连续页合并补丁](patches/xdma-2025.2-eop-sg-merge.patch)。
官方 EOP 路径给每个 SG 段设置完成通知，并在中断中扫描已完成段；原来每页一个段，
大包读取反而增加开销。补丁仅对 streaming C2H 的 EOP 读取使用内核
`sg_alloc_table_from_pages_segment`，在设备 DMA 段长度上限内合并物理连续页。
这个基础补丁保留 H2C 和原精确长度读取的 SG 布局；页固定、DMA 映射和释放仍由驱动执行。
内核 helper 的实现见 [Linux v6.12 scatterlist.c](https://github.com/torvalds/linux/blob/v6.12/lib/scatterlist.c)。
EOP 还需要 [描述符链尾修补](patches/xdma-2025.2-eop-chain-tail.patch)：一个读取请求
可能在描述符环回绕处拆成多次传输。官方代码只在部分描述符完成时检查 EOP；
若末包落在当前链的最后一个描述符，驱动会漏判并继续提交剩余链，空等 10 秒后
才返回已经收到的数据。补丁也在整链完成时传播 EOP，正常结束当前读取。
四核长测和带链尾命中日志的定向压力测试覆盖了这个边界，真实超时保持原值。
再应用 [DMA 段长度补丁](patches/xdma-2025.2-max-dma-segment.patch)：PCI 设备默认的
64 KiB DMA 段上限仍会把物理连续内存切成多段。XDMA 的描述符长度支持 28 位，
驱动将设备声明的上限至少提高到 1 MiB，让上述 EOP SG 合并发挥作用；物理不连续
的页不会被强行合并，其他读取路径不改变 SG 构造。卸载和 probe 失败时恢复原上限。
硬件描述符格式见 [AMD PG195](https://docs.amd.com/r/en-US/pg195-pcie-dma)。
配合 gzip 输入预取镜像，再应用 [H2C 连续页合并补丁](patches/xdma-2025.2-h2c-sg-merge.patch)：
将同一内核 helper 扩展到 streaming H2C，减少描述符提交和设备取描述符的开销。
只合并物理相邻页并遵守 DMA 段长度上限；页固定、方向、字节偏移、DMA 映射和释放保持原逻辑。
四核 256 MiB 分块的三轮对照中，文本从约 7.74 提升至 7.84 GB/s；720 组边界输入
及三种数据各 96 GiB 的完整解压比较通过。性能条件见[gzip 吞吐实测](gzip-throughput-20261102.md)。
补丁基于官方提交 `a918c5196a09f95756af244a607d543d9dce5bfb`，实测内核为 Linux 6.12。
下面固定到该源码版本；其他版本需要重新核对补丁及实机测试。

```bash
fpga_gzip_dir=/root/fpga-gzip  # 改成保存本 SDK 的路径
cd /usr/local/src
git clone --depth 1 https://github.com/Xilinx/dma_ip_drivers.git
cd dma_ip_drivers
git fetch --depth 1 origin a918c5196a09f95756af244a607d543d9dce5bfb
git checkout --detach FETCH_HEAD
git apply --check "$fpga_gzip_dir/docs/patches/xdma-2025.2-killable-dma-wait.patch"
git apply "$fpga_gzip_dir/docs/patches/xdma-2025.2-killable-dma-wait.patch"
git apply --check "$fpga_gzip_dir/docs/patches/xdma-2025.2-readonly-h2c.patch"
git apply "$fpga_gzip_dir/docs/patches/xdma-2025.2-readonly-h2c.patch"
git apply --check "$fpga_gzip_dir/docs/patches/xdma-2025.2-eop-sg-merge.patch"
git apply "$fpga_gzip_dir/docs/patches/xdma-2025.2-eop-sg-merge.patch"
git apply --check "$fpga_gzip_dir/docs/patches/xdma-2025.2-eop-chain-tail.patch"
git apply "$fpga_gzip_dir/docs/patches/xdma-2025.2-eop-chain-tail.patch"
git apply --check "$fpga_gzip_dir/docs/patches/xdma-2025.2-max-dma-segment.patch"
git apply "$fpga_gzip_dir/docs/patches/xdma-2025.2-max-dma-segment.patch"
git apply --check "$fpga_gzip_dir/docs/patches/xdma-2025.2-h2c-sg-merge.patch"
git apply "$fpga_gzip_dir/docs/patches/xdma-2025.2-h2c-sg-merge.patch"
cd XDMA/linux-kernel/xdma
make
```

已经应用补丁的源码不需要重复应用。补丁将 transfer 完成等待改为 killable wait，普通信号不会中止 DMA，
真正的驱动超时和 SIGKILL 仍然有效。正常操作无需禁用 Go 抢占，也无需使用 polling 模式。
普通信号不再打断内核 DMA 等待，取消可能等待当前传输结束或驱动超时（本机默认 10 秒）。

只要 bitstream 实现了 XDMA IP，官方驱动的 PCI ID 表包含 `10ee:903f`，模块会尝试 probe 该设备。驱动匹配并不意味着 FPGA 内部一定有 gzip 算法。

## 3. 安装并加载

先停止所有使用 `/dev/xdma*` 的程序；如果已加载旧模块，执行 `sudo modprobe -r xdma`。

```bash
sudo install -D -m 0644 xdma.ko "/lib/modules/$(uname -r)/updates/xdma.ko"
sudo depmod -a
sudo modprobe xdma
modinfo -n xdma
cat /sys/module/xdma/srcversion
```

以上命令将模块放到：

```text
/lib/modules/$(uname -r)/updates/xdma.ko
```

`modinfo -n xdma` 应指向该文件；本机六个补丁的构建 `srcversion` 为
`2B62117683A9D7702D0CF20`，固定上游版本依次应用六个补丁重建后与测试候选一致。
原五个补丁的修补版为 `EE98305A8E92F08D1564207`。前四个补丁为 `E3CDCD760B1013A292DE373`，
前三个补丁为 `94DCC95A2173C02F0AD9552`，
仅等待与只读页补丁的版本为 `C6055D7044FEB6C79C6C438`，
只含等待补丁的旧版为 `4053DCC3523124C6CC000FA`。安装前应另存当前已验证的模块用于回滚。
内核升级后需要为新内核重新编译安装。此驱动补丁不会加载或持久化 FPGA bitstream。

需要开机自动加载时创建 `/etc/modules-load.d/xdma.conf`：

```text
xdma
```

## 4. 验证

```bash
lspci -Dk -s 39:00.0
modinfo xdma | grep -i 903f
dmesg | grep -i xdma
ls -l /dev/xdma*
```

成功时应看到：

```text
Kernel driver in use: xdma
/dev/xdma0_user
/dev/xdma0_h2c_0
/dev/xdma0_c2h_0
```

如果 `lspci` 仍显示 `BusMaster-` 或没有 driver，检查：

```bash
lspci -Dnn -s 39:00.0
cat /sys/bus/pci/devices/0000:39:00.0/uevent
modprobe xdma
dmesg | tail -100
```

如果模块绑定但日志显示 `NO engine found`，当前 FPGA 镜像不是该 XDMA reference driver 期望的 XDMA 设计，单独修改 PCI ID 不能解决寄存器或 DMA 协议不匹配。

## 5. 卸载和回滚

先停止所有使用 `/dev/xdma*` 的程序。回滚到安装前另存的同内核、已验证模块：

```bash
sudo modprobe -r xdma
sudo install -D -m 0644 /absolute/path/to/verified-backup/xdma.ko \
  "/lib/modules/$(uname -r)/updates/xdma.ko"
sudo depmod -a
sudo modprobe xdma
cat /sys/module/xdma/srcversion
```

本轮安装前的四补丁模块保存在本机
`/data1/fpga-sixgb-20261101-133035/xdma-before-max-segment.ko`。
`/root/xdma.ko` 是更早的原始模块，不含上述 Go DMA 等待修复。
其他机器使用自己提前保存的同内核模块；内核升级后重新编译。

如果只想临时验证，可以跳过安装步骤，直接使用编译目录中的 `xdma.ko`：

```bash
sudo insmod ./xdma.ko
```

## 6. 与本项目的关系

XDMA 驱动只提供 PCIe DMA 和 user BAR 访问。`fpga-gzip` 在 `/dev/xdma0_user` 上写 gzip shell 的控制寄存器，再通过 H2C/C2H 通道传输数据。安装驱动不会加载 bitstream，也不会为任意 FPGA 自动提供 gzip 功能；bitstream 必须实现项目 README 中列出的寄存器和 packetizer 协议。

官方 XDMA 自带的 loopback 测试会向 FPGA 写数据，只适合已知的 XDMA example design。对未知的压缩 shell，先使用本项目的 gzip 程序或只读寄存器检查，避免把 loopback 数据写入正在使用的用户逻辑。
