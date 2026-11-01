// Command fpga-gzipd exposes the pure-Go FPGA gzip pipeline over HTTP.
// Clients may connect through a Unix socket or TCP; both use POST /compress
// with an exact Content-Length and receive a standard gzip response stream.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	lfyflag "github.com/lfhy/flag"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/firmware"
	"github.com/lfhy/fpga-gzip/pipeline"
	"github.com/lfhy/fpga-gzip/server"
	"github.com/lfhy/fpga-gzip/size"
)

type daemonConfig struct {
	Unix string
	TCP  string

	User          string
	H2C           string
	C2H           string
	Cores         uint
	LegacyPackets bool
	MmapRegisters bool

	RingSlots         uint
	H2CSlotText       string
	C2HRequestText    string
	PacketRequestText string
	PollInterval      timeDuration
	DrainIdle         timeDuration
	ResetTimeout      timeDuration
	StartTimeout      timeDuration
	MaxProcs          uint
	MaxConcurrent     uint
	QueueDepth        uint
	MaxInputText      string
	AuthToken         string
	Algorithms        string
}

// timeDuration is a named alias so the flag package can bind it through its
// duration helper without exposing a second config type from the SDK.
type timeDuration = durationValue

type durationValue struct{ value time.Duration }

func (d *durationValue) String() string { return d.value.String() }

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, lfyflag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		return 2
	}

	h2cBytes, err := size.Parse(cfg.H2CSlotText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "h2c-slot-bytes: %v\n", err)
		return 2
	}
	c2hBytes, err := size.Parse(cfg.C2HRequestText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "c2h-request-bytes: %v\n", err)
		return 2
	}
	var maxInput int64
	packetBytes, err := size.Parse(cfg.PacketRequestText)
	if err != nil || packetBytes < 8 || packetBytes > math.MaxUint32 || packetBytes%8 != 0 {
		fmt.Fprintln(os.Stderr, "packet-request-bytes must be an 8-byte multiple between 8 and uint32 maximum")
		return 2
	}
	if cfg.MaxInputText != "" && cfg.MaxInputText != "0" {
		value, parseErr := size.Parse(cfg.MaxInputText)
		if parseErr != nil || value > uint64(^uint64(0)>>1) {
			if parseErr == nil {
				parseErr = errors.New("value is too large for int64")
			}
			fmt.Fprintf(os.Stderr, "max-input-bytes: %v\n", parseErr)
			return 2
		}
		maxInput = int64(value)
	}
	if cfg.Unix == "" && cfg.TCP == "" {
		fmt.Fprintln(os.Stderr, "至少指定 --unix 或 --tcp")
		return 2
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = cfg.Cores
	}
	if cfg.MaxConcurrent > uint(^uint(0)>>1) || cfg.QueueDepth > uint(^uint(0)>>1) {
		fmt.Fprintln(os.Stderr, "workers or queue-depth is too large")
		return 2
	}

	runtime.GOMAXPROCS(int(cfg.MaxProcs))
	options := pipeline.DefaultOptions()
	options.RingSlots = int(cfg.RingSlots)
	options.H2CSlotBytes = h2cBytes
	options.C2HRequestBytes = c2hBytes
	options.PacketRequestBytes = packetBytes
	options.PollInterval = cfg.PollInterval.value
	options.DrainIdle = cfg.DrainIdle.value
	options.ResetTimeout = cfg.ResetTimeout.value
	options.StartTimeout = cfg.StartTimeout.value
	options.MaxProcs = int(cfg.MaxProcs)

	paths := device.Paths{User: cfg.User, H2C: cfg.H2C, C2H: cfg.C2H, LegacyPackets: cfg.LegacyPackets}
	corePaths, err := device.FourCorePaths(paths, int(cfg.Cores))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	pool, err := device.NewPool(corePaths)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	serverOptions := make([]server.Option, 0, 1)
	if cfg.Algorithms != "" {
		var algorithms []firmware.Algorithm
		for _, name := range strings.Split(cfg.Algorithms, ",") {
			a, err := firmware.ParseAlgorithm(strings.TrimSpace(name))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			algorithms = append(algorithms, a)
		}
		serverOptions = append(serverOptions, server.WithAlgorithms(algorithms...))
	}
	if cfg.AuthToken != "" {
		serverOptions = append(serverOptions, server.WithAuth(cfg.AuthToken))
	}
	svc, err := server.New(server.Config{
		HardwareFactory: func(ctx context.Context) (pipeline.Hardware, error) {
			lease, err := pool.AcquireActive(ctx)
			if err != nil {
				return nil, err
			}
			if cfg.MmapRegisters {
				if err = lease.EnableRegisterMmap(); err != nil {
					lease.Close()
					return nil, err
				}
			}
			return lease, nil
		},
		Options:       options,
		MaxConcurrent: int(cfg.MaxConcurrent),
		QueueDepth:    int(cfg.QueueDepth),
		MaxInputBytes: maxInput,
	}, serverOptions...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建服务失败: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	endpoints := make([]func(context.Context) error, 0, 2)
	if cfg.Unix != "" {
		endpoints = append(endpoints, func(ctx context.Context) error {
			return svc.ListenAndServeUnix(ctx, cfg.Unix, 0o660)
		})
	}
	if cfg.TCP != "" {
		endpoints = append(endpoints, func(ctx context.Context) error {
			return svc.ListenAndServeTCP(ctx, cfg.TCP)
		})
	}

	errCh := make(chan error, len(endpoints))
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		endpoint := endpoint
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- endpoint(ctx)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		<-done
		return 0
	case err := <-errCh:
		stop()
		<-done
		if err != nil {
			fmt.Fprintf(os.Stderr, "服务监听失败: %v\n", err)
			return 1
		}
		return 0
	}
}

func parseConfig(args []string) (daemonConfig, error) {
	defaults := pipeline.DefaultOptions()
	cfg := daemonConfig{
		User:              "/dev/xdma0_user",
		H2C:               "/dev/xdma0_h2c_0",
		C2H:               "/dev/xdma0_c2h_0",
		RingSlots:         uint(defaults.RingSlots),
		H2CSlotText:       fmt.Sprintf("%d", defaults.H2CSlotBytes),
		C2HRequestText:    fmt.Sprintf("%d", defaults.C2HRequestBytes),
		PacketRequestText: fmt.Sprintf("%d", defaults.PacketRequestBytes),
		PollInterval:      durationValue{value: defaults.PollInterval},
		DrainIdle:         durationValue{value: defaults.DrainIdle},
		ResetTimeout:      durationValue{value: defaults.ResetTimeout},
		StartTimeout:      durationValue{value: defaults.StartTimeout},
		MaxProcs:          uint(defaults.MaxProcs),
		Cores:             1,
		MaxConcurrent:     0,
		QueueDepth:        2,
		MaxInputText:      "0",
	}
	fs := lfyflag.NewFlagSet("fpga-gzipd", lfyflag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.ConfigFile("config.toml").Alias("config")
	fs.StringFullVar(&cfg.Unix, "unix", "daemon", "unix", "FPGA_GZIPD_UNIX", "", "Unix socket path")
	fs.StringFullVar(&cfg.TCP, "tcp", "daemon", "tcp", "FPGA_GZIPD_TCP", "", "TCP listen address")
	fs.StringFullVar(&cfg.User, "user-device", "device", "user", "FPGA_GZIP_USER_DEVICE", cfg.User, "XDMA user BAR")
	fs.StringFullVar(&cfg.H2C, "h2c-device", "device", "h2c", "FPGA_GZIP_H2C_DEVICE", cfg.H2C, "XDMA H2C stream")
	fs.StringFullVar(&cfg.C2H, "c2h-device", "device", "c2h", "FPGA_GZIP_C2H_DEVICE", cfg.C2H, "XDMA C2H stream")
	fs.UintFullVar(&cfg.Cores, "cores", "device", "cores", "FPGA_GZIP_CORES", cfg.Cores, "validated independent gzip cores, 1 to 4")
	fs.BoolFullVar(&cfg.LegacyPackets, "legacy-packets", "device", "legacy_packets", "FPGA_GZIP_LEGACY_PACKETS", false, "force original exact-length packet protocol")
	fs.BoolFullVar(&cfg.MmapRegisters, "mmap-registers", "device", "mmap_registers", "FPGA_GZIP_MMAP_REGISTERS", false, "optional Linux amd64 BAR mapping")
	fs.UintFullVar(&cfg.RingSlots, "ring-slots", "pipeline", "ring_slots", "FPGA_GZIP_RING_SLOTS", cfg.RingSlots, "DMA ring slots")
	fs.StringFullVar(&cfg.H2CSlotText, "h2c-slot-bytes", "pipeline", "h2c_slot_bytes", "FPGA_GZIP_H2C_SLOT_BYTES", cfg.H2CSlotText, "H2C slot size")
	fs.StringFullVar(&cfg.C2HRequestText, "c2h-request-bytes", "pipeline", "c2h_request_bytes", "FPGA_GZIP_C2H_REQUEST_BYTES", cfg.C2HRequestText, "C2H packet size")
	fs.StringFullVar(&cfg.PacketRequestText, "packet-request-bytes", "pipeline", "packet_request_bytes", "FPGA_GZIP_PACKET_REQUEST_BYTES", cfg.PacketRequestText, "negotiated EOP packet size")
	fs.DurationFullVar(&cfg.PollInterval.value, "poll-interval", "pipeline", "poll_interval", "FPGA_GZIP_POLL_INTERVAL", cfg.PollInterval.value, "FPGA poll interval")
	fs.DurationFullVar(&cfg.DrainIdle.value, "drain-idle", "pipeline", "drain_idle", "FPGA_GZIP_DRAIN_IDLE", cfg.DrainIdle.value, "early drain idle time")
	fs.DurationFullVar(&cfg.ResetTimeout.value, "reset-timeout", "pipeline", "reset_timeout", "FPGA_GZIP_RESET_TIMEOUT", cfg.ResetTimeout.value, "FPGA reset timeout")
	fs.DurationFullVar(&cfg.StartTimeout.value, "start-timeout", "pipeline", "start_timeout", "FPGA_GZIP_START_TIMEOUT", cfg.StartTimeout.value, "FPGA start timeout")
	fs.UintFullVar(&cfg.MaxProcs, "max-procs", "runtime", "max_procs", "FPGA_GZIP_MAX_PROCS", cfg.MaxProcs, "daemon GOMAXPROCS")
	fs.UintFullVar(&cfg.MaxConcurrent, "workers", "daemon", "workers", "FPGA_GZIPD_WORKERS", cfg.MaxConcurrent, "request workers; 0 uses core count; device pool serializes each core")
	fs.UintFullVar(&cfg.QueueDepth, "queue-depth", "daemon", "queue_depth", "FPGA_GZIPD_QUEUE_DEPTH", cfg.QueueDepth, "queued requests")
	fs.StringFullVar(&cfg.MaxInputText, "max-input-bytes", "daemon", "max_input_bytes", "FPGA_GZIPD_MAX_INPUT_BYTES", cfg.MaxInputText, "maximum request size; zero means unlimited")
	fs.StringFullVar(&cfg.AuthToken, "auth-token", "daemon", "auth_token", "FPGA_GZIPD_AUTH_TOKEN", "", "optional Bearer token; empty disables authentication")
	fs.StringFullVar(&cfg.Algorithms, "algorithms", "daemon", "algorithms", "FPGA_COMPRESS_ALGORITHMS", "gzip", "allowed request algorithms; each image contains one: gzip,lz4,zstd,minlz,raw")
	if err := fs.Parse(args); err != nil {
		return daemonConfig{}, err
	}
	if cfg.RingSlots == 0 || cfg.MaxProcs == 0 {
		return daemonConfig{}, errors.New("ring-slots and max-procs must be greater than zero")
	}
	if cfg.Cores < 1 || cfg.Cores > 4 {
		return daemonConfig{}, errors.New("cores must be between 1 and 4")
	}
	return cfg, nil
}
