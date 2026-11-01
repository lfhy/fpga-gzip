package fpgagzip

import (
	"context"
	"io"

	"github.com/lfhy/fpga-gzip/accelerator"
	"github.com/lfhy/fpga-gzip/firmware"
)

var (
	ErrFirmwareSwitching    = firmware.ErrSwitching
	ErrFirmwareIncompatible = firmware.ErrIncompatible
	ErrFirmwareAlgorithm    = firmware.ErrAlgorithm
)

// InspectFirmware reads the image identity under a temporary shared card lock.
// It never opens DMA channels or resets an engine. The transaction still checks
// the identity again after opening its own device, so a switch between these
// two operations cannot submit data to the wrong algorithm.
func InspectFirmware(ctx context.Context, userDevice string) (firmware.Info, error) {
	lock, err := firmware.LockTransaction(userDevice)
	if err != nil {
		return firmware.Info{}, err
	}
	defer lock.Close()
	return firmware.InspectBAR(ctx, userDevice, 0)
}

// CompressAlgorithm selects raw, gzip, lz4, zstd or minlz explicitly. The existing
// CompressReader and gzip.Writer-like API continue to emit only gzip. This
// convenience API uses the same validation and bounded transport as accelerator.
func CompressAlgorithm(ctx context.Context, algorithm string, input io.Reader, size uint64, output io.Writer, hw Hardware, options Options, report func(Progress)) (Result, error) {
	a, err := firmware.ParseAlgorithm(algorithm)
	if err != nil {
		return Result{}, err
	}
	return accelerator.CompressReader(ctx, a, input, size, output, hw, options, report)
}
