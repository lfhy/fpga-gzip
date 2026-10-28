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

```bash
cd /usr/local/src
git clone --depth 1 https://github.com/Xilinx/dma_ip_drivers.git
cd dma_ip_drivers/XDMA/linux-kernel/xdma
make
```

只要 bitstream 实现了 XDMA IP，官方驱动的 PCI ID 表包含 `10ee:903f`，模块会尝试 probe 该设备。驱动匹配并不意味着 FPGA 内部一定有 gzip 算法。

## 3. 安装并加载

```bash
sudo make install
sudo depmod -a
sudo modprobe xdma
```

`make install` 将模块放到：

```text
/lib/modules/$(uname -r)/xdma/xdma.ko
```

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
cd /usr/local/src/dma_ip_drivers/XDMA/linux-kernel/xdma
sudo make uninstall
```

如果只想临时验证，可以跳过 `make install`，直接使用编译目录中的 `xdma.ko`：

```bash
sudo insmod ./xdma.ko
```

## 6. 与本项目的关系

XDMA 驱动只提供 PCIe DMA 和 user BAR 访问。`fpga-gzip` 在 `/dev/xdma0_user` 上写 gzip shell 的控制寄存器，再通过 H2C/C2H 通道传输数据。安装驱动不会加载 bitstream，也不会为任意 FPGA 自动提供 gzip 功能；bitstream 必须实现项目 README 中列出的寄存器和 packetizer 协议。

官方 XDMA 自带的 loopback 测试会向 FPGA 写数据，只适合已知的 XDMA example design。对未知的压缩 shell，先使用本项目的 gzip 程序或只读寄存器检查，避免把 loopback 数据写入正在使用的用户逻辑。
