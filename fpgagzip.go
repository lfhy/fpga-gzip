// Package fpgagzip is the public SDK for the FPGA gzip shell.
//
// The package provides a high-level file-to-file API and re-exports the
// device, pipeline, and configuration types used by the command-line tool.
// It is pure Go and accesses XDMA through the public device subpackage.
package fpgagzip

import (
	"context"
	"io"

	"github.com/lfhy/fpga-gzip/config"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/pipeline"
)

// Config is the merged CLI/config-file/environment configuration.
type Config = config.Config

// Device is an opened XDMA user BAR and H2C/C2H stream pair.
type Device = device.Device

// DevicePaths identifies the XDMA character devices.
type DevicePaths = device.Paths

// Snapshot is the FPGA shell's register snapshot used by Hardware.
type Snapshot = device.Snapshot

// Hardware is the interface accepted by Run and Compress. The production
// implementation is *Device; applications can provide a simulator or wrapper
// that implements the same contract.
type Hardware = pipeline.Hardware

// Options controls the streaming pipeline.
type Options = pipeline.Options

// Progress is a point-in-time transfer report.
type Progress = pipeline.Progress

// Result contains validated output counters and timing information. Total,
// Pipeline, and FPGA use the same streaming boundary; Sync is reported
// separately and is completed before Run returns.
type Result = pipeline.Result

// ErrHelp is returned by ParseConfig when --help is requested.
var ErrHelp = config.ErrHelp

// Open opens the XDMA devices described by paths. The caller must close the
// returned device.
func Open(paths DevicePaths) (*Device, error) {
	return device.Open(paths)
}

// DefaultOptions returns the settings validated for the gzip shell used by
// this module.
func DefaultOptions() Options {
	return pipeline.DefaultOptions()
}

// Run compresses inputPath to outputPath through hw.
func Run(ctx context.Context, inputPath, outputPath string, hw Hardware, opts Options, report func(Progress)) (Result, error) {
	return pipeline.Run(ctx, inputPath, outputPath, hw, opts, report)
}

// Compress is an alias for Run, provided as a descriptive SDK entry point.
func Compress(ctx context.Context, inputPath, outputPath string, hw Hardware, opts Options, report func(Progress)) (Result, error) {
	return Run(ctx, inputPath, outputPath, hw, opts, report)
}

// CompressReader compresses exactly inputSize bytes from input into output.
// It uses the supplied readers directly and does not create temporary files.
func CompressReader(ctx context.Context, input io.Reader, inputSize uint64, output io.Writer, hw Hardware, opts Options, report func(Progress)) (Result, error) {
	return pipeline.RunReader(ctx, input, inputSize, output, hw, opts, report)
}

// AppendCSV appends a result row to path.
func AppendCSV(path string, result Result, opts Options) error {
	return pipeline.AppendCSV(path, result, opts)
}

// ParseConfig parses CLI arguments using the same precedence rules as the
// command-line program: command line, environment, config file, then defaults.
func ParseConfig(args []string) (Config, error) {
	return config.Parse(args)
}

// Parse is kept as a short alias for ParseConfig for SDK callers that already
// use the package as their application's argument parser.
func Parse(args []string) (Config, error) {
	return ParseConfig(args)
}
