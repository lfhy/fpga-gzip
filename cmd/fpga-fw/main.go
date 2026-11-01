// Command fpga-fw inspects and switches versioned compression images.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	lfhflag "github.com/lfhy/flag"
	"github.com/lfhy/fpga-gzip/firmware"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fpga-fw inspect|check|switch [options]")
	}
	command := args[0]
	if command != "inspect" && command != "check" && command != "switch" {
		return fmt.Errorf("unknown firmware command %q", command)
	}
	flags := lfhflag.NewFlagSet("fpga-fw "+command, lfhflag.ContinueOnError)
	var user, manifestPath, pci, target, vivado, server string
	var timeout time.Duration
	flags.StringVar(&user, "user", "/dev/xdma0_user", "XDMA user BAR")
	flags.StringVar(&manifestPath, "manifest", "", "image manifest adjacent to .bit file")
	flags.StringVar(&pci, "pci", "", "exact PCI address, e.g. 0000:39:00.0")
	flags.StringVar(&target, "target", "", "exact JTAG target including cable serial")
	flags.StringVar(&vivado, "vivado", "vivado", "Vivado executable")
	flags.StringVar(&server, "hw-server", "localhost:3121", "hardware server address")
	flags.DurationVar(&timeout, "timeout", 3*time.Minute, "drain, programming and verification timeout")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if len(flags.Args()) != 0 || timeout <= 0 {
		return errors.New("unexpected argument or invalid timeout")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var manifest firmware.Manifest
	var bit string
	if command != "inspect" {
		var err error
		manifest, bit, err = firmware.LoadManifest(manifestPath)
		if err != nil {
			return err
		}
	}
	if command == "check" {
		_, cleanup, err := firmware.StageBitstream(bit, manifest.SHA256)
		if err != nil {
			return err
		}
		cleanup()
		return json.NewEncoder(os.Stdout).Encode(manifest)
	}
	if command == "inspect" {
		lock, err := firmware.LockTransaction(user)
		if err != nil {
			return err
		}
		defer lock.Close()
		info, err := firmware.InspectBAR(ctx, user, 0)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Algorithm string `json:"algorithm"`
			firmware.Info
		}{info.Algorithm.String(), info})
	}
	info, err := firmware.Switch(ctx, manifest, bit, firmware.SwitchOptions{UserDevice: user, PCI: pci, Target: target, Vivado: vivado, HWServer: server, Log: os.Stderr})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(info)
}
