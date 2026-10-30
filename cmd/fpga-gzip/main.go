package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/lfhy/fpga-gzip/config"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/pipeline"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return 2
	}
	// The standalone CLI owns its process, so it may honor max-procs. The
	// public pipeline library deliberately does not change GOMAXPROCS globally;
	// callers such as FUSE daemons must keep their own scheduler width.
	runtime.GOMAXPROCS(int(cfg.MaxProcs))
	hw, err := device.Open(device.Paths{
		User: cfg.UserDevice,
		H2C:  cfg.H2CDevice,
		C2H:  cfg.C2HDevice,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 FPGA 设备失败: %v\n", err)
		return 1
	}
	defer hw.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := pipeline.Options{
		RingSlots:        int(cfg.RingSlots),
		H2CSlotBytes:     cfg.H2CSlotBytes,
		C2HRequestBytes:  cfg.C2HRequestBytes,
		PollInterval:     cfg.PollInterval,
		DrainIdle:        cfg.DrainIdle,
		ResetTimeout:     cfg.ResetTimeout,
		StartTimeout:     cfg.StartTimeout,
		ProgressInterval: cfg.ProgressInterval,
		MaxProcs:         int(cfg.MaxProcs),
	}

	var report func(pipeline.Progress)
	if cfg.Progress {
		report = makeProgressPrinter()
	}
	result, err := pipeline.Run(ctx, cfg.InputPath, cfg.OutputPath, hw, opts, report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FPGA gzip 失败: %v\n", err)
		return 1
	}
	if cfg.Progress {
		fmt.Println()
	}

	if cfg.CSVPath != "" {
		if err := pipeline.AppendCSV(cfg.CSVPath, result, opts); err != nil {
			fmt.Fprintf(os.Stderr, "写入 CSV 失败: %v\n", err)
			return 1
		}
	}

	ratio := float64(result.OutputBytes) / float64(result.InputBytes) * 100
	fmt.Printf("输入                  : %d bytes\n", result.InputBytes)
	fmt.Printf("压缩输出              : %d bytes\n", result.OutputBytes)
	fmt.Printf("压缩后/原始大小       : %.4f%%\n", ratio)
	fmt.Printf("STATUS                : 0x%08x\n", result.Status)
	fmt.Printf("DEBUG                 : 0x%08x\n", result.Debug)
	fmt.Printf("总耗时                : %s\n", result.Total)
	fmt.Printf("输出落盘 Sync         : %s\n", result.Sync)
	fmt.Printf("FPGA 处理             : %s\n", result.FPGA)
	if result.Total > 0 {
		fmt.Printf("总吞吐                : %.2f MiB/s\n", float64(result.InputBytes)/(1024*1024)/result.Total.Seconds())
	}
	return 0
}

func makeProgressPrinter() func(pipeline.Progress) {
	return func(progress pipeline.Progress) {
		inputPercent := percentage(progress.H2CSent, progress.InputSize)
		c2hPercent := percentage(progress.C2HReceived, progress.CompressedSize)
		if progress.CompressedSize == 0 {
			c2hPercent = percentage(progress.C2HReceived, progress.FPGAOutBytes)
		}
		fmt.Printf("\rH2C %6.2f%% (%d/%d)  FPGA %-5s  OUT %d  C2H %6.2f%% (%d)  FILE %d   ",
			inputPercent,
			progress.H2CSent, progress.InputSize,
			device.StatusName(progress.Status),
			progress.FPGAOutBytes,
			c2hPercent,
			progress.C2HReceived,
			progress.FileWritten)
	}
}

func percentage(value, total uint64) float64 {
	if total == 0 {
		return 100
	}
	percent := float64(value) * 100 / float64(total)
	return math.Min(percent, 100)
}
