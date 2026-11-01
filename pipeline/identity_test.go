package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lfhy/fpga-gzip/firmware"
)

// Any hardware operation beyond identity panics through the nil embedded
// interface. The mismatch must be rejected before reset/start/stream closures.
type wrongFirmware struct{ Hardware }

func (*wrongFirmware) FirmwareInfo(context.Context) (firmware.Info, error) {
	return firmware.Info{Magic: firmware.Magic, ABI: firmware.ABI, Algorithm: firmware.LZ4, Format: 1, Cores: 4, Features: 3, Build: 42}, nil
}

func TestGzipRejectsOtherImageBeforeReset(t *testing.T) {
	_, err := RunReader(context.Background(), bytes.NewReader([]byte("input")), 5, io.Discard, &wrongFirmware{}, DefaultOptions(), nil)
	if !errors.Is(err, firmware.ErrAlgorithm) {
		t.Fatal(err)
	}
}
