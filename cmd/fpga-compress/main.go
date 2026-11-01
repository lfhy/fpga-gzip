// Command fpga-compress encodes a file using the matching loaded FPGA image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	lfhflag "github.com/lfhy/flag"
	"github.com/lfhy/fpga-gzip/accelerator"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/firmware"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	f := lfhflag.NewFlagSet("fpga-compress", lfhflag.ContinueOnError)
	var algorithm, input, output, user, h2c, c2h string
	var core uint
	f.StringVar(&algorithm, "algorithm", "gzip", "raw, gzip, lz4 or zstd; must match the loaded image")
	f.StringVar(&input, "input", "", "input file")
	f.StringVar(&output, "output", "", "output file, published atomically after verification")
	f.StringVar(&user, "user", "/dev/xdma0_user", "XDMA user BAR")
	f.StringVar(&h2c, "h2c", "/dev/xdma0_h2c_0", "H2C channel 0")
	f.StringVar(&c2h, "c2h", "/dev/xdma0_c2h_0", "C2H channel 0")
	f.UintVar(&core, "core", 0, "independent core index, 0 through 3")
	if err := f.Parse(args); err != nil {
		return err
	}
	if input == "" || output == "" || core > 3 || len(f.Args()) != 0 {
		return errors.New("--input, --output and a valid --core are required")
	}
	a, err := firmware.ParseAlgorithm(algorithm)
	if err != nil {
		return err
	}
	paths, err := device.FourCorePaths(device.Paths{User: user, H2C: h2c, C2H: c2h}, int(core)+1)
	if err != nil {
		return err
	}
	hw, err := device.Open(paths[core])
	if err != nil {
		return err
	}
	defer hw.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	info, err := firmware.Require(ctx, hw, a)
	if err != nil {
		return err
	}
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()
	stat, err := in.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return errors.New("input must be a nonempty regular file")
	}
	out, err := os.CreateTemp(filepath.Dir(output), ".fpga-compress-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	result, err := accelerator.CompressReader(ctx, a, in, uint64(stat.Size()), out, hw, accelerator.DefaultOptions(), nil)
	if err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), output); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Algorithm string             `json:"algorithm"`
		Firmware  firmware.Info      `json:"firmware"`
		Result    accelerator.Result `json:"result"`
	}{a.String(), info, result})
}
