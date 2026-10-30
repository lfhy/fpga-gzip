// Package config owns command-line, environment and config-file parsing.
// github.com/lfhy/flag supplies one precedence model for all three sources:
// command line > environment > config file > defaults.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	lfyflag "github.com/lfhy/flag"

	"github.com/lfhy/fpga-gzip/size"
)

// ErrHelp is returned when the user asks for --help/-h.
var ErrHelp = lfyflag.ErrHelp

// Config is the fully typed application configuration after all sources have
// been merged and validated.
type Config struct {
	InputPath  string
	OutputPath string
	CSVPath    string

	UserDevice string
	H2CDevice  string
	C2HDevice  string

	RingSlots       uint
	H2CSlotBytes    uint64
	C2HRequestBytes uint64

	PollInterval     time.Duration
	DrainIdle        time.Duration
	ResetTimeout     time.Duration
	StartTimeout     time.Duration
	ProgressInterval time.Duration
	MaxProcs         uint

	Progress bool
}

// Parse parses arguments using github.com/lfhy/flag.  The default config file
// is config.toml when present; -c/--config selects another file. Positional
// arguments remain supported as input and output paths for shell compatibility.
func Parse(args []string) (Config, error) {
	var cfg Config
	var h2cSlotText = "4MiB"
	var c2hRequestText = "32KiB"

	fs := lfyflag.NewFlagSet("fpga-gzip", lfyflag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.ConfigFile("config.toml").Alias("config")

	fs.StringFullVar(&cfg.InputPath, "input", "gzip", "input", "FPGA_GZIP_INPUT", "", "输入文件路径")
	fs.StringFullVar(&cfg.OutputPath, "output", "gzip", "output", "FPGA_GZIP_OUTPUT", "", "输出 gzip 文件路径")
	fs.StringFullVar(&cfg.CSVPath, "csv", "gzip", "csv", "FPGA_GZIP_CSV", "", "追加性能结果 CSV 路径")

	fs.StringFullVar(&cfg.UserDevice, "user-device", "device", "user", "FPGA_GZIP_USER_DEVICE", "/dev/xdma0_user", "XDMA user BAR 设备节点")
	fs.StringFullVar(&cfg.H2CDevice, "h2c-device", "device", "h2c", "FPGA_GZIP_H2C_DEVICE", "/dev/xdma0_h2c_0", "XDMA H2C 设备节点")
	fs.StringFullVar(&cfg.C2HDevice, "c2h-device", "device", "c2h", "FPGA_GZIP_C2H_DEVICE", "/dev/xdma0_c2h_0", "XDMA C2H 设备节点")

	fs.UintFullVar(&cfg.RingSlots, "ring-slots", "pipeline", "ring_slots", "FPGA_GZIP_RING_SLOTS", 8, "输入/输出环形队列槽位数")
	fs.StringFullVar(&h2cSlotText, "h2c-slot-bytes", "pipeline", "h2c_slot_bytes", "FPGA_GZIP_H2C_SLOT_BYTES", h2cSlotText, "H2C 分块大小，例如 4MiB")
	fs.StringFullVar(&c2hRequestText, "c2h-request-bytes", "pipeline", "c2h_request_bytes", "FPGA_GZIP_C2H_REQUEST_BYTES", c2hRequestText, "C2H 请求缓冲大小，例如 32KiB")

	fs.DurationFullVar(&cfg.PollInterval, "poll-interval", "pipeline", "poll_interval", "FPGA_GZIP_POLL_INTERVAL", 50*time.Microsecond, "FPGA 状态轮询间隔")
	fs.DurationFullVar(&cfg.DrainIdle, "drain-idle", "pipeline", "drain_idle", "FPGA_GZIP_DRAIN_IDLE", 200*time.Microsecond, "输出暂缓时提前排空 FIFO 的等待时间")
	fs.DurationFullVar(&cfg.ResetTimeout, "reset-timeout", "pipeline", "reset_timeout", "FPGA_GZIP_RESET_TIMEOUT", 2*time.Second, "FPGA 复位超时")
	fs.DurationFullVar(&cfg.StartTimeout, "start-timeout", "pipeline", "start_timeout", "FPGA_GZIP_START_TIMEOUT", time.Second, "FPGA 启动超时")
	fs.DurationFullVar(&cfg.ProgressInterval, "progress-interval", "pipeline", "progress_interval", "FPGA_GZIP_PROGRESS_INTERVAL", 100*time.Millisecond, "进度输出间隔")
	fs.BoolFullVar(&cfg.Progress, "progress", "pipeline", "progress", "FPGA_GZIP_PROGRESS", true, "显示进度")
	fs.UintFullVar(&cfg.MaxProcs, "max-procs", "runtime", "max_procs", "FPGA_GZIP_MAX_PROCS", 4, "Go 运行时使用的最大 P 数；XDMA 长流需要并行调度 I/O 阶段")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	for _, positional := range fs.Args() {
		switch {
		case cfg.InputPath == "":
			cfg.InputPath = positional
		case cfg.OutputPath == "":
			cfg.OutputPath = positional
		default:
			return Config{}, fmt.Errorf("too many positional arguments: %q", positional)
		}
	}

	if cfg.InputPath == "" || cfg.OutputPath == "" {
		return Config{}, errors.New("input and output are required; use --input/--output or two positional paths")
	}
	if cfg.RingSlots == 0 {
		return Config{}, errors.New("ring-slots must be greater than zero")
	}
	if cfg.RingSlots > uint(^uint(0)>>1) {
		return Config{}, errors.New("ring-slots is too large for this platform")
	}
	if cfg.MaxProcs == 0 || cfg.MaxProcs > uint(^uint(0)>>1) {
		return Config{}, errors.New("max-procs must be greater than zero and fit in int")
	}

	var err error
	if cfg.H2CSlotBytes, err = size.Parse(h2cSlotText); err != nil {
		return Config{}, fmt.Errorf("h2c-slot-bytes: %w", err)
	}
	if cfg.C2HRequestBytes, err = size.Parse(c2hRequestText); err != nil {
		return Config{}, fmt.Errorf("c2h-request-bytes: %w", err)
	}
	if cfg.H2CSlotBytes == 0 {
		return Config{}, errors.New("h2c-slot-bytes must be greater than zero")
	}
	if cfg.C2HRequestBytes < 8 || cfg.C2HRequestBytes > math.MaxUint32 {
		return Config{}, fmt.Errorf("c2h-request-bytes must be between 8 and %d", math.MaxUint32)
	}
	if cfg.PollInterval <= 0 || cfg.DrainIdle <= 0 || cfg.ResetTimeout <= 0 || cfg.StartTimeout <= 0 || cfg.ProgressInterval <= 0 {
		return Config{}, errors.New("all timing values must be greater than zero")
	}

	return cfg, nil
}
