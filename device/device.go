// Package device contains the small pure-Go access layer for the Xilinx XDMA
// character devices.  It deliberately uses os.File Read/Write and ReadAt/
// WriteAt rather than cgo or a vendor userspace library.
package device

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Paths identifies the XDMA character devices created by xdma.ko.
type Paths struct {
	User string
	H2C  string
	C2H  string
}

// Device is a handle to the user BAR and one H2C/C2H streaming pair.
type Device struct {
	user *os.File
	h2c  *os.File
	c2h  *os.File

	closeStreamsOnce sync.Once
	closeOnce        sync.Once
}

// Open opens the three device nodes.  The caller owns the returned device and
// must call Close.
func Open(paths Paths) (*Device, error) {
	if paths.User == "" || paths.H2C == "" || paths.C2H == "" {
		return nil, errors.New("all XDMA device paths are required")
	}

	user, err := os.OpenFile(paths.User, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return nil, fmt.Errorf("open user device %q: %w", paths.User, err)
	}
	h2c, err := os.OpenFile(paths.H2C, os.O_WRONLY, 0)
	if err != nil {
		_ = user.Close()
		return nil, fmt.Errorf("open H2C device %q: %w", paths.H2C, err)
	}
	c2h, err := os.OpenFile(paths.C2H, os.O_RDONLY, 0)
	if err != nil {
		_ = user.Close()
		_ = h2c.Close()
		return nil, fmt.Errorf("open C2H device %q: %w", paths.C2H, err)
	}

	return &Device{user: user, h2c: h2c, c2h: c2h}, nil
}

// CloseStreams interrupts blocking DMA reads/writes.  It is safe to call more
// than once and is used by cancellation handling in the pipeline.
func (d *Device) CloseStreams() error {
	var first error
	d.closeStreamsOnce.Do(func() {
		if err := d.h2c.Close(); err != nil && !errors.Is(err, os.ErrInvalid) {
			first = err
		}
		if err := d.c2h.Close(); err != nil && !errors.Is(err, os.ErrInvalid) && first == nil {
			first = err
		}
	})
	return first
}

// Close closes all device files.  It also interrupts any pending stream I/O.
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	var first error
	d.closeOnce.Do(func() {
		if err := d.CloseStreams(); err != nil {
			first = err
		}
		if err := d.user.Close(); err != nil && !errors.Is(err, os.ErrInvalid) && first == nil {
			first = err
		}
	})
	return first
}

// WriteH2C writes one or more bytes to the FPGA input stream.
func (d *Device) WriteH2C(p []byte) (int, error) {
	return d.h2c.Write(p)
}

// ReadC2H reads one FPGA output packet.  The caller intentionally controls the
// requested length because the gzip shell exposes a length-defined packetizer.
func (d *Device) ReadC2H(p []byte) (int, error) {
	return d.c2h.Read(p)
}

// ReadReg32 reads a little-endian 32-bit user-BAR register.
func (d *Device) ReadReg32(_ context.Context, offset uint32) (uint32, error) {
	var buf [4]byte
	n, err := d.user.ReadAt(buf[:], int64(offset))
	if err != nil {
		return 0, fmt.Errorf("read register 0x%08x: %w", offset, err)
	}
	if n != len(buf) {
		return 0, fmt.Errorf("read register 0x%08x: %w", offset, io.ErrUnexpectedEOF)
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

// WriteReg32 writes a little-endian 32-bit user-BAR register.
func (d *Device) WriteReg32(_ context.Context, offset uint32, value uint32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	n, err := d.user.WriteAt(buf[:], int64(offset))
	if err != nil {
		return fmt.Errorf("write register 0x%08x: %w", offset, err)
	}
	if n != len(buf) {
		return fmt.Errorf("write register 0x%08x: %w", offset, io.ErrShortWrite)
	}
	return nil
}

// ReadReg64Stable reads a 64-bit counter exposed as adjacent low/high words.
func (d *Device) ReadReg64Stable(ctx context.Context, loOffset, hiOffset uint32) (uint64, error) {
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		hi1, err := d.ReadReg32(ctx, hiOffset)
		if err != nil {
			return 0, err
		}
		lo, err := d.ReadReg32(ctx, loOffset)
		if err != nil {
			return 0, err
		}
		hi2, err := d.ReadReg32(ctx, hiOffset)
		if err != nil {
			return 0, err
		}
		if hi1 == hi2 {
			return uint64(hi1)<<32 | uint64(lo), nil
		}
	}
	return 0, errors.New("64-bit register changed too quickly to read consistently")
}

// Snapshot is the gzip shell's observable progress state.
type Snapshot struct {
	Status   uint32
	Debug    uint32
	InBytes  uint64
	OutBytes uint64
}

// Snapshot reads the status and byte counters from the gzip shell.
func (d *Device) Snapshot(ctx context.Context) (Snapshot, error) {
	status, err := d.ReadReg32(ctx, RegStatus)
	if err != nil {
		return Snapshot{}, err
	}
	debug, err := d.ReadReg32(ctx, RegDebug)
	if err != nil {
		return Snapshot{}, err
	}
	inBytes, err := d.ReadReg64Stable(ctx, RegInBytesLo, RegInBytesHi)
	if err != nil {
		return Snapshot{}, err
	}
	outBytes, err := d.ReadReg64Stable(ctx, RegOutBytesLo, RegOutBytesHi)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Status: status, Debug: debug, InBytes: inBytes, OutBytes: outBytes}, nil
}

// TransferSnapshot is the low-latency snapshot used by the C2H drain loop.
// The reference C implementation reads only STATUS and OUT_BYTES on this hot
// path; DEBUG is fetched only when the shell reports an error. Keeping IN_BYTES
// out of the poll loop avoids unnecessary PCIe MMIO reads at high throughput.
func (d *Device) TransferSnapshot(ctx context.Context) (Snapshot, error) {
	status, err := d.ReadReg32(ctx, RegStatus)
	if err != nil {
		return Snapshot{}, err
	}
	outBytes, err := d.ReadReg64Stable(ctx, RegOutBytesLo, RegOutBytesHi)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Status: status, OutBytes: outBytes}
	if status&StatusError != 0 {
		snapshot.Debug, err = d.ReadReg32(ctx, RegDebug)
		if err != nil {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

// SoftReset asserts the shell reset bit and waits for RESET_BUSY to clear.
func (d *Device) SoftReset(ctx context.Context, timeout time.Duration) error {
	if err := d.WriteReg32(ctx, RegCtrl, CtrlSoftReset); err != nil {
		return err
	}
	if err := sleepContext(ctx, time.Millisecond); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := d.ReadReg32(ctx, RegStatus)
		if err != nil {
			return err
		}
		if status&StatusResetBusy == 0 {
			return nil
		}
		if err := sleepContext(ctx, 50*time.Microsecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("FPGA reset timed out after %s", timeout)
}

// Start writes the input length, verifies it, and starts one gzip operation.
func (d *Device) Start(ctx context.Context, inputSize uint64, timeout time.Duration) error {
	if err := d.WriteReg32(ctx, RegInputSizeLo, uint32(inputSize)); err != nil {
		return err
	}
	if err := d.WriteReg32(ctx, RegInputSizeHi, uint32(inputSize>>32)); err != nil {
		return err
	}
	readback, err := d.ReadReg64Stable(ctx, RegInputSizeLo, RegInputSizeHi)
	if err != nil {
		return err
	}
	if readback != inputSize {
		return fmt.Errorf("input-size register readback mismatch: wrote=%d read=%d", inputSize, readback)
	}
	if err := d.WriteReg32(ctx, RegCtrl, CtrlStart); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := d.ReadReg32(ctx, RegStatus)
		if err != nil {
			return err
		}
		if status&(StatusBusy|StatusDone) != 0 {
			return nil
		}
		if err := sleepContext(ctx, 50*time.Microsecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("FPGA start timed out after %s", timeout)
}

// StartC2HRequest arms the packetizer for exactly requestLen bytes.
func (d *Device) StartC2HRequest(ctx context.Context, requestLen uint32) error {
	if requestLen == 0 {
		return errors.New("C2H request length must be greater than zero")
	}
	if err := d.WriteReg32(ctx, RegC2HRequestLen, requestLen); err != nil {
		return err
	}
	readback, err := d.ReadReg32(ctx, RegC2HRequestLen)
	if err != nil {
		return err
	}
	if readback != requestLen {
		return fmt.Errorf("C2H request length readback mismatch: wrote=%d read=%d", requestLen, readback)
	}
	return d.WriteReg32(ctx, RegCtrl, CtrlC2HStart)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
