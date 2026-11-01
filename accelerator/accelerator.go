// Package accelerator uses the common FPGA shell with raw, gzip, LZ4 and Zstd
// images. It does not program the card or change process-wide scheduler settings.
// The root fpgagzip package remains the gzip-only API.
package accelerator

import (
	"context"
	"io"

	"github.com/lfhy/fpga-gzip/firmware"
	"github.com/lfhy/fpga-gzip/pipeline"
)

type Algorithm = firmware.Algorithm

const (
	Raw  = firmware.Raw
	Gzip = firmware.Gzip
	LZ4  = firmware.LZ4
	Zstd = firmware.Zstd
)

type Hardware = pipeline.Hardware
type Options = pipeline.Options
type Result = pipeline.Result
type Progress = pipeline.Progress

func DefaultOptions() Options { return pipeline.DefaultOptions() }

// CompressReader emits the standard representation selected by algorithm. For
// LZ4 and Zstd it verifies the entire stream with a bounded pure Go decoder and
// compares decoded CRC32/length with the input, because those HLS encoders do
// not emit content checksums. Gzip retains its fast header/trailer validation;
// raw compares CRC32 and length directly. Verification is part of the timing.
// Input must remain immutable until this call returns. The caller closes hw.
func CompressReader(ctx context.Context, algorithm Algorithm, input io.Reader, size uint64, output io.Writer, hw Hardware, options Options, report func(Progress)) (Result, error) {
	if algorithm == Gzip {
		return pipeline.RunReader(ctx, input, size, output, hw, options, report)
	}
	validator, err := newValidator(algorithm, output, size)
	if err != nil {
		return Result{}, err
	}
	defer validator.Close()
	return pipeline.RunReaderAlgorithm(ctx, input, size, output, hw, options, report, algorithm, validator)
}
