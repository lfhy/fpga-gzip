//go:build linux

package firmware

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type SwitchOptions struct {
	UserDevice string
	PCI        string
	Target     string
	Vivado     string
	HWServer   string
	Log        io.Writer
}

func (o SwitchOptions) validate() error {
	if !regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`).MatchString(o.PCI) {
		return errors.New("an exact PCI address is required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_./:-]+$`).MatchString(o.Target) || strings.ContainsAny(o.Target, "*?[]") {
		return errors.New("an exact JTAG target including cable serial is required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`).MatchString(o.HWServer) {
		return errors.New("invalid hw_server address")
	}
	if o.Vivado == "" || o.UserDevice == "" {
		return errors.New("Vivado executable and user device are required")
	}
	return nil
}

// Switch programs volatile SRAM through JTAG. It never writes configuration
// Flash. The complete artifact is verified before blocking admissions, and
// the programming lock is retained through post-load identity verification.
// Older clients are checked through /proc and must finish before driver unload.
func Switch(ctx context.Context, manifest Manifest, bitPath string, options SwitchOptions) (Info, error) {
	if err := manifest.Validate(); err != nil {
		return Info{}, err
	}
	if err := options.validate(); err != nil {
		return Info{}, err
	}
	if os.Geteuid() != 0 {
		return Info{}, errors.New("firmware switching requires root for XDMA and PCI rescan")
	}
	staged, cleanup, err := StageBitstream(bitPath, manifest.SHA256)
	if err != nil {
		return Info{}, err
	}
	defer cleanup()
	l, err := LockProgramming(ctx, options.UserDevice)
	if err != nil {
		return Info{}, fmt.Errorf("drain FPGA tasks: %w", err)
	}
	defer l.Close()
	if err := waitDeviceOwners(ctx); err != nil {
		return Info{}, err
	}
	pciDir := filepath.Join("/sys/bus/pci/devices", options.PCI)
	for name, expected := range map[string]string{"vendor": "0x10ee", "device": "0x903f"} {
		got, err := os.ReadFile(filepath.Join(pciDir, name))
		if err != nil {
			return Info{}, err
		}
		if strings.TrimSpace(string(got)) != expected {
			return Info{}, fmt.Errorf("unexpected PCI %s: %s", name, got)
		}
	}
	if err := verifyUserPCI(options.UserDevice, options.PCI); err != nil {
		return Info{}, err
	}
	realPCI, err := filepath.EvalSymlinks(pciDir)
	if err != nil {
		return Info{}, err
	}
	rescan := filepath.Join(filepath.Dir(realPCI), "rescan")
	if _, err := os.Stat(rescan); err != nil {
		return Info{}, err
	}
	if options.Log == nil {
		options.Log = io.Discard
	}
	run := func(ctx context.Context, program string, args ...string) error {
		c := exec.CommandContext(ctx, program, args...)
		c.Stdout, c.Stderr = options.Log, options.Log
		if err := c.Run(); err != nil {
			return fmt.Errorf("%s: %w", program, err)
		}
		return nil
	}
	fmt.Fprintln(options.Log, "FPGA tasks drained; unloading XDMA")
	if err := run(ctx, "modprobe", "-r", "xdma"); err != nil {
		return Info{}, err
	}
	// Recover PCI enumeration/driver on every failure, including cancellation.
	// Reprogramming the previous image is explicit: use its manifest with Switch.
	recovered := false
	defer func() {
		if !recovered {
			recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = os.WriteFile(rescan, []byte("1\n"), 0200)
			if err := run(recovery, "modprobe", "xdma"); err != nil {
				fmt.Fprintln(options.Log, "XDMA recovery:", err)
			}
		}
	}()
	if err := os.WriteFile(filepath.Join(pciDir, "remove"), []byte("1\n"), 0200); err != nil {
		return Info{}, err
	}
	tcl := filepath.Join(filepath.Dir(staged), "program.tcl")
	// Target/host are restricted above; staged is a generated absolute path.
	body := fmt.Sprintf(`open_hw_manager
connect_hw_server -url {%s}
set target [get_hw_targets -quiet {%s}]
if {[llength $target] != 1} {error "expected exactly one JTAG target"}
current_hw_target $target
open_hw_target
set device [get_hw_devices -quiet xcku5p_0]
if {[llength $device] != 1} {error "expected exactly one KU5P device"}
current_hw_device $device
set_property PROGRAM.FILE {%s} $device
program_hw_devices $device
refresh_hw_device $device
close_hw_manager
exit
`, options.HWServer, options.Target, staged)
	if err := os.WriteFile(tcl, []byte(body), 0600); err != nil {
		return Info{}, err
	}
	fmt.Fprintf(options.Log, "Programming %s (%s), build %016x\n", manifest.Name, manifest.Algorithm, manifest.Build)
	if err := run(ctx, options.Vivado, "-mode", "batch", "-nojournal", "-nolog", "-source", tcl); err != nil {
		return Info{}, err
	}
	if err := os.WriteFile(rescan, []byte("1\n"), 0200); err != nil {
		return Info{}, err
	}
	if err := run(ctx, "modprobe", "xdma"); err != nil {
		return Info{}, err
	}
	var info Info
	for {
		info, err = InspectBAR(ctx, options.UserDevice, 0)
		if err == nil {
			break
		}
		if err := waitTick(ctx); err != nil {
			return Info{}, fmt.Errorf("wait for re-enumerated FPGA: %w", err)
		}
	}
	if err := verifyUserPCI(options.UserDevice, options.PCI); err != nil {
		return Info{}, fmt.Errorf("verify re-enumerated device: %w", err)
	}
	if err := manifest.Check(info); err != nil {
		return Info{}, err
	}
	for bank := uint32(1); bank < manifest.Cores; bank++ {
		other, err := InspectBAR(ctx, options.UserDevice, bank)
		if err != nil {
			return Info{}, err
		}
		if err := manifest.Check(other); err != nil {
			return Info{}, fmt.Errorf("core %d: %w", bank, err)
		}
	}
	recovered = true
	fmt.Fprintln(options.Log, "Loaded image identity verified on every active core")
	return info, nil
}

func waitTick(ctx context.Context) error {
	t := time.NewTimer(20 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func waitDeviceOwners(ctx context.Context) error {
	for {
		owners, err := DeviceOwners()
		if err != nil {
			return err
		}
		if len(owners) == 0 {
			return nil
		}
		if err := waitTick(ctx); err != nil {
			return fmt.Errorf("XDMA remains open: %s: %w", strings.Join(owners, ", "), err)
		}
	}
}

// DeviceOwners reports all XDMA descriptors because unloading a shared xdma
// module affects every card. It reads only descriptor links and process names.
func DeviceOwners() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var owners []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect DMA owners %s: %w", dir, err)
		}
		for _, fd := range fds {
			path, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil {
				continue
			}
			if strings.HasPrefix(path, "/dev/xdma") {
				comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
				owners = append(owners, fmt.Sprintf("pid=%d %s %s", pid, strings.TrimSpace(string(comm)), path))
			}
		}
	}
	return owners, nil
}

func verifyUserPCI(user, pci string) error {
	stat := syscall.Stat_t{}
	if err := syscall.Stat(user, &stat); err != nil {
		return err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		return errors.New("user device is not a character device")
	}
	major := (stat.Rdev>>8)&0xfff | (stat.Rdev>>32)&0xfffff000
	minor := stat.Rdev&0xff | (stat.Rdev>>12)&0xffffff00
	devicePath, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/char/%d:%d/device", major, minor))
	if err != nil {
		return err
	}
	if !strings.Contains(devicePath, "/"+pci+"/") && !strings.HasSuffix(devicePath, "/"+pci) {
		return errors.New("user device does not belong to the selected PCI function")
	}
	return nil
}
