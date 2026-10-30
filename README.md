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
  --c2h-request-bytes 32KiB \
  --max-procs 4
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
c2h_request_bytes = "32KiB"
poll_interval = "50us"
drain_idle = "200us"
reset_timeout = "2s"
start_timeout = "1s"
progress_interval = "100ms"
progress = true

[runtime]
max_procs = 4
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

每个 C2H 请求先写 `C2H_REQ_LEN`，再写 `CTRL.C2H_START`，然后对 `/dev/xdma0_c2h_0` 执行一次等长 `read()`。本机 `shorttail72` shell 的默认请求为 32 KiB，并按 8 字节 beat 对齐；最后一个请求可以携带 partial beat。256 KiB 在这块卡上可能触发 C2H/H2C 超时，换了 RTL 后应重新验证请求粒度。XDMA 驱动偶尔会把最后一个 SG 结果按页边界计数，SDK 对小于 4 KiB 的差异按 packetizer 的 TLAST 处理；更大的差异仍会报协议错误。

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

本机 `/data1` NVMe 上的历史 10 GiB 随机数据记录如下；旧记录使用 8 个槽位、4 MiB H2C、256 KiB C2H 请求和 `max_procs=1`，仅用于说明当时的测试环境：

| 实现 | 总耗时 | 流水线/FPGA 耗时 | 端到端吞吐 | 输出字节 |
| --- | ---: | ---: | ---: | ---: |
| `fpga-gzip`（`max_procs=1`） | 6.950 s | 6.946/6.948 s | 1473.30 MiB/s | 11325240047 |

`/data1` 裸盘直读约 6.3 GB/s、直写约 3.8 GB/s，所以表中的 620–1470 MiB/s 是压缩流水线吞吐，不是硬盘上限。这次 10 GiB 输出的 `Sync` 另耗时 2.528 s，未计入表中 `Total`。该输出随后通过 `gzip -t`，并用 `gzip -dc | cmp` 与原始 10 GiB 输入逐字节校验。旧的 256 KiB C2H 配置或 `GOMAXPROCS=1` 不能代表 FPGA 核本身的性能；前者可能让 packetizer/驱动超时，后者会把 H2C、C2H、落盘和轮询 goroutine 串在一个 P 上。当前默认配置使用 32 KiB C2H 和 4 个 P。库调用不会修改宿主进程的 `GOMAXPROCS`，只有独立 CLI 根据 `--max-procs` 设置自己的进程；这避免 FUSE 进程被压成单 P。

在相同的 `/data1/fpga-bench-64MiB.bin` 随机输入上，当前 `FPGA-4IP` bitstream 的单核对照如下：

| 实现 | C2H | H2C | Go P 数 | 吞吐 | 结果 |
| --- | ---: | ---: | ---: | ---: | --- |
| `FPGA-4IP/gzip_demand_scheduler` | 32 KiB | 512 KiB | pthread | 704.80 MiB/s | 成功 |
| 旧 Go 默认 | 256 KiB | 4 MiB | 1 | 51.50 MiB/s | 不稳定/可能超时 |
| Go 调优后 | 32 KiB | 4 MiB | 4 | 748.28 MiB/s | 成功，gzip 校验通过 |

这版 bitstream 的 post-route 时序为 `axis_clk=250 MHz`；LUT 使用率约 91%、BRAM 约 89%、URAM 100%。构建日志还显示 C2H packetizer 的 `req_busy/req_done` 没有接到 shell 控制寄存器，因此主机必须依赖 `OUT_BYTES` 和等长 `read()` 判断请求完成。它不解释 50 MiB/s 的主因，但使软件必须严格遵守这套请求协议。

## HTTP/Unix 压缩服务

`server` 包和 `fpga-gzipd` 命令把 FPGA 放到独立进程中，FUSE 或其他程序通过 Unix socket 或 TCP 发送请求。服务进程默认使用 4 个 Go P；库本身不会改调用方的调度器。请求必须有精确的 `Content-Length`，因为 shell 在 H2C 开始前需要完整输入长度：

```bash
go build -o fpga-gzipd ./cmd/fpga-gzipd
sudo ./fpga-gzipd --unix /run/fpga-gzip.sock

curl --unix-socket /run/fpga-gzip.sock \
  -H 'Content-Type: application/octet-stream' \
  --data-binary @input.bin \
  -o output.gz \
  http://localhost/compress
```

也可以监听 TCP：

```bash
sudo ./fpga-gzipd --tcp 127.0.0.1:9080
curl --data-binary @input.bin -o output.gz http://127.0.0.1:9080/compress
curl http://127.0.0.1:9080/healthz
curl http://127.0.0.1:9080/metrics
```

认证默认关闭。需要保护压缩和指标接口时，为 daemon 设置一个 token；`/healthz` 始终保持公开，便于服务管理器探活：

```bash
sudo ./fpga-gzipd --unix /run/fpga-gzip.sock --auth-token 'change-me'
curl --unix-socket /run/fpga-gzip.sock \
  -H 'Authorization: Bearer change-me' \
  --data-binary @input.bin -o output.gz \
  http://localhost/compress
curl --unix-socket /run/fpga-gzip.sock \
  -H 'Authorization: Bearer change-me' \
  http://localhost/metrics
```

也可以通过环境变量 `FPGA_GZIPD_AUTH_TOKEN` 或 daemon 配置文件中的 `auth_token` 设置。没有设置 token 时不会发送或校验认证头。Go 客户端支持同一个可选配置：

```go
c, err := client.NewUnix(
	"/run/fpga-gzip.sock",
	client.WithAuth("change-me"),
)
if err != nil {
	log.Fatal(err)
}
defer c.CloseIdleConnections()
```

服务按请求创建新的 XDMA 句柄，默认单并发并带有限队列；`--workers` 只有在工厂为每个 worker 提供独立 H2C/C2H stream 时才应增加。客户端断开连接会取消流水线并关闭阻塞的 DMA I/O。`server.Config` 可直接嵌入已有 daemon，`Server.Handler()` 也可挂到自有 `http.Server`。

其他 Go 程序可以使用公开的 `client` 包连接 daemon；`client.NewUnix` 和 `client.NewTCP` 会复用 HTTP 连接，`Client.Compress` 只需要一个已知长度的 `io.Reader` 和输出 `io.Writer`。

## 排查

```bash
lspci -Dk -s 39:00.0
modinfo xdma | grep -i 903f
ls -l /dev/xdma0_user /dev/xdma0_h2c_0 /dev/xdma0_c2h_0
dmesg | grep -i xdma
```

如果设备没有 `Kernel driver in use: xdma`，先按 [`docs/driver-install.md`](docs/driver-install.md) 安装驱动。XDMA 只提供 PCIe/DMA 传输；它本身不会把任意 FPGA 变成 gzip 压缩卡，bitstream 必须包含上面描述的 gzip shell。
