# fpga-gzip

`fpga-gzip` 是一个纯 Go 的 FPGA gzip 主机端 SDK 和命令行工具。它直接访问 Xilinx XDMA 字符设备和 FPGA user BAR，不依赖 cgo、XRT 或厂商用户态库。当前实现对应本机已经验证过的 gzip shell：一个 H2C streaming 通道、一个 C2H streaming 通道，以及 user BAR 上的 gzip 控制寄存器。

当前目录已经在本机 FPGA 上验证：1 MiB 输入可以得到 gzip 输出，`gzip -t` 通过，解压结果与原始文件逐字节一致。

## 目录结构

```text
fpgagzip.go          根 SDK 门面，包名为 fpgagzip
config/              github.com/lfhy/flag 配置文件/环境变量/参数解析
device/              公开的纯 Go XDMA 文件、寄存器和控制协议
pipeline/            公开的输入/H2C/FPGA/C2H/输出并发流水线
size/                4MiB、1GiB 等人类可读大小解析
cmd/fpga-gzip/       CLI 入口、信号处理、进度和结果输出
examples/sdk/        外部 Go 程序调用 SDK 的示例
docs/                驱动安装与设备准备文档
```

## 依赖和构建

需要 Go 1.21 或更新版本，以及已经加载的 Xilinx XDMA 驱动。驱动安装步骤见 [`docs/driver-install.md`](docs/driver-install.md)。

```bash
cd fpga-gzip
export PATH=/usr/local/go/bin:$PATH
go mod download
gofmt -w cmd config device pipeline size examples fpgagzip.go writer.go
CGO_ENABLED=0 go test ./...
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -o bin/fpga-gzip ./cmd/fpga-gzip
```

`CGO_ENABLED=0` 是项目的预期构建方式；程序通过 Go 标准库 `os.File` 的 `Read`/`Write`/`ReadAt`/`WriteAt` 访问 `/dev/xdma*`。

## SDK 调用

根包的导入路径是 `github.com/lfhy/fpga-gzip`，Go 包标识符是 `fpgagzip`。它不依赖 `internal` 包，其他模块可以直接导入：

```bash
go get github.com/lfhy/fpga-gzip
```

最小调用示例：

```go
package main

import (
	"context"
	"log"

	fpgagzip "github.com/lfhy/fpga-gzip"
)

func main() {
	hw, err := fpgagzip.Open(fpgagzip.DevicePaths{
		User: "/dev/xdma0_user",
		H2C:  "/dev/xdma0_h2c_0",
		C2H:  "/dev/xdma0_c2h_0",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer hw.Close()

	result, err := fpgagzip.Compress(
		context.Background(), "input.bin", "output.gz", hw,
		fpgagzip.DefaultOptions(), nil,
	)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%d -> %d bytes, pipeline=%s",
		result.InputBytes, result.OutputBytes, result.Pipeline)
}
```

`fpgagzip.Run` 与 `Compress` 等价；`fpgagzip.ParseConfig` 可复用 CLI 的配置文件、环境变量和参数优先级。需要更细的控制时，可以直接导入公开的 `device`、`pipeline`、`config` 和 `size` 子包。完整可编译示例见 [`examples/sdk/main.go`](examples/sdk/main.go)。

如果输入已经在内存中，或来自网络、管道等其他 `io.Reader`，并且调用方知道准确的输入长度，可以直接使用 `CompressReader`。它不会创建输入或输出临时文件，H2C 和 C2H 会直接使用传入的 reader/writer：

```go
import "bytes"

var compressed bytes.Buffer
result, err := fpgagzip.CompressReader(
	context.Background(),
	bytes.NewReader(input), uint64(len(input)), &compressed,
	hw, fpgagzip.DefaultOptions(), nil,
)
```

根包还提供了 `NewWriter`、`NewWriterOptions` 和 `NewWriterContext`，调用形状接近 `compress/gzip.Writer`：

```go
writer := fpgagzip.NewWriter(&compressed, hw)
if _, err := writer.Write(input); err != nil {
	log.Fatal(err)
}
if err := writer.Close(); err != nil {
	log.Fatal(err)
}
```

由于当前 shell 启动时必须先写入完整输入长度，`Writer` 会先缓存输入，`Close` 时才启动 FPGA；它适合需要 gzip 风格接口的场景。大文件或高吞吐场景应优先使用 `CompressReader`（已知长度）或 `Compress`/`Run`，避免额外的临时文件读写。

## 运行

可以使用两个位置参数：

```bash
sudo ./bin/fpga-gzip input.bin output.gz
```

也可以用显式参数：

```bash
sudo ./bin/fpga-gzip \
  --input input.bin \
  --output output.gz \
  --ring-slots 8 \
  --h2c-slot-bytes 4MiB \
  --c2h-request-bytes 256KiB \
  --max-procs 1
```

运行后验证输出：

```bash
gzip -t output.gz
gzip -dc output.gz | cmp - input.bin
```

输入文件必须是非空普通文件。`Ctrl-C` 会停止新的 H2C 提交、关闭阻塞的 XDMA stream，并尝试复位 FPGA datapath。

## 配置文件、环境变量和参数

配置解析统一使用 [`github.com/lfhy/flag`](https://github.com/lfhy/flag)。优先级是：**命令行 > 环境变量 > 配置文件 > 默认值**。程序默认查找当前目录的 `config.toml`；也可以通过隐藏的 `-c` 或别名 `--config` 指定文件。

```bash
cp config.example.toml config.toml
vi config.toml
sudo ./bin/fpga-gzip --config config.toml
```

配置文件示例：

```toml
[gzip]
input = "/data/input.bin"
output = "/data/output.gz"
csv = "/data/benchmark.csv"

[device]
user = "/dev/xdma0_user"
h2c = "/dev/xdma0_h2c_0"
c2h = "/dev/xdma0_c2h_0"

[pipeline]
ring_slots = 8
h2c_slot_bytes = "4MiB"
c2h_request_bytes = "256KiB"
poll_interval = "50us"
drain_idle = "200us"
reset_timeout = "2s"
start_timeout = "1s"
progress_interval = "100ms"
progress = true

[runtime]
max_procs = 1
```

所有配置键也有对应的命令行参数和环境变量。例如：

```bash
FPGA_GZIP_PROGRESS=false \
sudo -E ./bin/fpga-gzip --config config.toml --output result.gz
```

大小参数支持纯字节数以及 `K/KiB`、`M/MiB`、`G/GiB`、`T/TiB` 后缀。

## FPGA 寄存器协议

程序使用 `/dev/xdma0_user` 的 user BAR：

| 偏移 | 名称 | 说明 |
| ---: | --- | --- |
| `0x00` | `CTRL` | bit0 启动，bit1 软复位，bit2 启动 C2H 请求 |
| `0x04` | `STATUS` | bit0 busy，bit1 done，bit2 reset busy，bit3 error |
| `0x08/0x0c` | `INPUT_SIZE_LO/HI` | 输入字节数 |
| `0x10/0x14` | `OUT_BYTES_LO/HI` | FPGA 已产生的 gzip 字节数 |
| `0x18/0x1c` | `IN_BYTES_LO/HI` | FPGA 已接受的输入字节数 |
| `0x20` | `C2H_REQ_LEN` | 下一次 C2H packet 请求长度 |
| `0x24` | `DEBUG` | shell 调试状态 |

每个 C2H 请求先写 `C2H_REQ_LEN`，再写 `CTRL.C2H_START`，然后对 `/dev/xdma0_c2h_0` 执行一次等长 `read()`。默认 C2H 请求为 256 KiB，并按 8 字节 beat 对齐；最后一个请求可以携带 partial beat。这个粒度在本机 gzip shell 的长文件测试中可避免 1 MiB 请求触发的 262160 字节提前排空包超时；如果换了 RTL，应重新验证请求粒度。

这个协议是 bitstream 专用协议。换成其他 FPGA shell 时，必须同时调整寄存器映射、状态位和数据包语义。

## 硬件兼容性边界

这个 SDK 可以复用在满足以下条件的 AMD/Xilinx FPGA 设计上：

1. PCIe 端点使用 Xilinx/AMD XDMA，系统能创建 `/dev/xdma*_user`、H2C 和 C2H 设备节点；
2. bitstream 提供本 README 中的 user BAR 寄存器、复位/启动/完成状态和输入输出计数器；
3. H2C 是 gzip 输入流，C2H packetizer 接受请求长度并对每个请求输出一次等长 XDMA read，最后一个包允许 partial beat；
4. FPGA 输出的是标准 gzip 数据流，并遵守本项目的输入长度和结束语义。

因此，标准 XDMA 只解决 PCIe/DMA 传输，不能让任意 FPGA 自动兼容这个 SDK。其他 FPGA 厂商的 PCIe/DMA IP、没有 XDMA 的设计，或寄存器/packetizer 不同的 gzip RTL，都需要实现新的 `device` 后端或适配 `pipeline.Hardware` 接口；只要适配层提供相同的 `Hardware` 契约，上层文件流水线仍可复用。

## 运行结果和 CSV

设置 `--csv result.csv` 或 `[gzip].csv` 后，程序会追加输入大小、输出大小、缓冲配置、流水线耗时、输出 `Sync` 耗时、FPGA 耗时、C2H 耗时、吞吐和压缩比例。`Total` 在流水线和最终寄存器检查后结束，输出仍会在程序返回前完成 `Sync`。随机数据的 gzip 输出可能比输入更大，这是正常现象。

本机 `/data1` NVMe 上的 10 GiB 随机数据记录如下，使用 8 个槽位、4 MiB H2C、256 KiB C2H 请求和 `max_procs=1`：

| 实现 | 总耗时 | 流水线/FPGA 耗时 | 端到端吞吐 | 输出字节 |
| --- | ---: | ---: | ---: | ---: |
| `fpga-gzip`（`max_procs=1`） | 6.950 s | 6.946/6.948 s | 1473.30 MiB/s | 11325240047 |

`/data1` 裸盘直读约 6.3 GB/s、直写约 3.8 GB/s，所以表中的 620–1470 MiB/s 是压缩流水线吞吐，不是硬盘上限。这次 10 GiB 输出的 `Sync` 另耗时 2.528 s，未计入表中 `Total`。该输出随后通过 `gzip -t`，并用 `gzip -dc | cmp` 与原始 10 GiB 输入逐字节校验。使用 `max_procs` 大于 1 或把 C2H 请求改回 1 MiB 时，当前 bitstream 仍可能让 XDMA C2H 请求超时；出现 `connection timed out` 后应先做 PCI function reset，再重试，不能把失败重试的结果当作性能数据。

本机测试还发现，Go 运行时使用 `GOMAXPROCS=1` 时长流更稳定：1 GiB 和 10 GiB 测试均可完成，10 GiB 记录为 9.422 s、1086.82 MiB/s；`max_procs` 默认就是 1。这个设置只限制 Go 调度器的 P 数，阻塞的 XDMA 读写仍会由运行时调度其他 goroutine。

## 排查

```bash
lspci -Dk -s 39:00.0
modinfo xdma | grep -i 903f
ls -l /dev/xdma0_user /dev/xdma0_h2c_0 /dev/xdma0_c2h_0
dmesg | grep -i xdma
```

如果设备没有 `Kernel driver in use: xdma`，先按 [`docs/driver-install.md`](docs/driver-install.md) 安装驱动。XDMA 只提供 PCIe/DMA 传输；它本身不会把任意 FPGA 变成 gzip 压缩卡，bitstream 必须包含上面描述的 gzip shell。
