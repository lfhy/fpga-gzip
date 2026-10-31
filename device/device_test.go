package device

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegisterBanksRemainIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bar")
	if err := os.WriteFile(path, make([]byte, 0x4000), 0600); err != nil {
		t.Fatal(err)
	}
	for core := 0; core < 4; core++ {
		d, err := Open(Paths{User: path, H2C: "/dev/null", C2H: "/dev/null", RegisterBase: uint32(core) * 0x1000})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.WriteReg32(context.Background(), RegInputSizeLo, uint32(100+core)); err != nil {
			t.Fatal(err)
		}
		got, err := d.ReadReg32(context.Background(), RegInputSizeLo)
		if err != nil || got != uint32(100+core) {
			t.Fatalf("core %d: %d %v", core, got, err)
		}
		if err := d.SetRegisterBase(3); err == nil {
			t.Fatal("unaligned base accepted")
		}
		d.Close()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for core := 0; core < 4; core++ {
		if got := binary.LittleEndian.Uint32(data[core*0x1000+8:]); got != uint32(100+core) {
			t.Fatalf("bank %d overwritten: %d", core, got)
		}
	}
}

func TestSoftResetWaitsForOldCompletionToClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bar")
	data := make([]byte, 4096)
	binary.LittleEndian.PutUint32(data[RegStatus:], StatusDone)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(Paths{User: path, H2C: "/dev/null", C2H: "/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	done := make(chan error, 1)
	go func() { done <- d.SoftReset(context.Background(), time.Second) }()
	select {
	case err := <-done:
		t.Fatalf("accepted stale DONE: %v", err)
	case <-time.After(5 * time.Millisecond):
	}
	if err := d.WriteReg32(context.Background(), RegStatus, 0); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSoftResetHasNoFixedDelayAndHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bar")
	if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(Paths{User: path, H2C: "/dev/null", C2H: "/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.SoftReset(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err = d.WriteReg32(context.Background(), RegStatus, StatusResetBusy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = d.SoftReset(ctx, time.Second); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestFourCorePathValidation(t *testing.T) {
	first := Paths{User: "/dev/xdma1_user", H2C: "/dev/xdma1_h2c_0", C2H: "/dev/xdma1_c2h_0"}
	paths, err := FourCorePaths(first, 4)
	if err != nil {
		t.Fatal(err)
	}
	if paths[3].RegisterBase != 0x3000 || paths[3].H2C != "/dev/xdma1_h2c_3" {
		t.Fatal(paths)
	}
	if _, err = NewPool(append(paths, paths[0])); err == nil {
		t.Fatal("duplicate core accepted")
	}
	paths[1].H2C = paths[0].H2C
	if _, err = NewPool(paths); err == nil {
		t.Fatal("overlapping DMA accepted")
	}
	for _, count := range []int{0, 5} {
		if _, err := FourCorePaths(first, count); err == nil {
			t.Fatal(count)
		}
	}
	first.H2C = "/dev/xdma1_h2c_2"
	if _, err := FourCorePaths(first, 4); err == nil {
		t.Fatal("nonzero first channel accepted")
	}
}

func TestPoolRejectsPartialRegisterOverlap(t *testing.T) {
	paths, err := FourCorePaths(Paths{User: "/dev/xdma0_user", H2C: "/dev/xdma0_h2c_0", C2H: "/dev/xdma0_c2h_0"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []uint32{4, RegDebug} {
		paths[1].RegisterBase = base
		if _, err := NewPool(paths); err == nil {
			t.Fatalf("accepted overlapping bank at %#x", base)
		}
	}
	paths[1].RegisterBase = RegDebug + 4
	if _, err := NewPool(paths); err != nil {
		t.Fatalf("rejected adjacent nonoverlapping banks: %v", err)
	}
}

func TestPoolKeepsCoreUntilLeaseClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bar")
	if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := NewPool([]Paths{{User: path, H2C: "/dev/null", C2H: "/dev/zero"}})
	if err != nil {
		t.Fatal(err)
	}
	l, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = l.CloseStreams(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = p.Acquire(ctx); err != context.Canceled {
		t.Fatal(err)
	}
	if len(p.available) != 0 {
		t.Fatal("CloseStreams released the active control bank")
	}
	waitCtx, stop := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() { _, err := p.Acquire(waitCtx); waited <- err }()
	select {
	case err := <-waited:
		t.Fatalf("admitted overlapping stream: %v", err)
	case <-time.After(time.Millisecond):
	}
	stop()
	if err := <-waited; err != context.Canceled {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	if len(p.available) != 1 {
		t.Fatal("lease was not returned exactly once")
	}
	next, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestPoolReturnsFailedOpen(t *testing.T) {
	p, err := NewPool([]Paths{{User: "/nonexistent/bar", H2C: "/nonexistent/h", C2H: "/nonexistent/c"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Acquire(context.Background()); err == nil {
		t.Fatal("open succeeded")
	}
	if len(p.available) != 1 {
		t.Fatal("failed open leaked a slot")
	}
}
