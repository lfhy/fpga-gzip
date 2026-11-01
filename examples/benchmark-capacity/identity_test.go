package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lfhy/fpga-gzip/firmware"
	"github.com/lfhy/fpga-gzip/pipeline"
)

type mismatchedFirmware struct{ pipeline.Hardware }

func (*mismatchedFirmware) FirmwareInfo(context.Context) (firmware.Info, error) {
	return firmware.Info{Magic: firmware.Magic, ABI: firmware.ABI, Algorithm: firmware.MinLZ, Format: 1, Cores: 4, Features: 3, Build: 123}, nil
}

func TestMeasurementPreservesFirmwareRejection(t *testing.T) {
	// The embedded nil Hardware panics if the wrapper drops identity checks
	// and starts a reset or DMA operation on the incompatible image.
	hw := &measured{Hardware: &mismatchedFirmware{}}
	_, err := pipeline.RunReader(context.Background(), bytes.NewReader([]byte("input")), 5, io.Discard, hw, pipeline.DefaultOptions(), nil)
	if !errors.Is(err, firmware.ErrAlgorithm) {
		t.Fatal(err)
	}
}
