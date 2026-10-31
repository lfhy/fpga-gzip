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

`modinfo -n xdma` 应指向该文件；本机修补版的 `srcversion` 为 `4053DCC3523124C6CC000FA`。
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

先停止所有使用 `/dev/xdma*` 的程序：

```bash
sudo modprobe -r xdma
sudo rm "/lib/modules/$(uname -r)/updates/xdma.ko"
sudo depmod -a
```

本机原始模块保留在 `/root/xdma.ko`，回滚后可用 `sudo insmod /root/xdma.ko` 临时加载。
其他机器应使用自己提前保存的同内核模块；不要混用内核版本不同的 `.ko`。

如果只想临时验证，可以跳过安装步骤，直接使用编译目录中的 `xdma.ko`：

```bash
sudo insmod ./xdma.ko
```

## 6. 与本项目的关系

XDMA 驱动只提供 PCIe DMA 和 user BAR 访问。`fpga-gzip` 在 `/dev/xdma0_user` 上写 gzip shell 的控制寄存器，再通过 H2C/C2H 通道传输数据。安装驱动不会加载 bitstream，也不会为任意 FPGA 自动提供 gzip 功能；bitstream 必须实现项目 README 中列出的寄存器和 packetizer 协议。

官方 XDMA 自带的 loopback 测试会向 FPGA 写数据，只适合已知的 XDMA example design。对未知的压缩 shell，先使用本项目的 gzip 程序或只读寄存器检查，避免把 loopback 数据写入正在使用的用户逻辑。
