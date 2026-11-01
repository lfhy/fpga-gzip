//go:build linux && amd64

package device

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMappedRegisterBanksAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bar")
	if err := os.WriteFile(path, make([]byte, 0x4000), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for core := 0; core < 4; core++ {
		d, err := Open(Paths{User: path, H2C: "/dev/null", C2H: "/dev/zero", RegisterBase: uint32(core) * 0x1000})
		if err != nil {
			t.Fatal(err)
		}
		if err = d.EnableRegisterMmap(); err != nil {
			d.Close()
			t.Fatal(err)
		}
		if err = d.SetRegisterBase(uint32(core+1) * 0x1000); err == nil {
			t.Fatal("changed a mapped bank")
		}
		if err = d.WriteReg32(ctx, RegInputSizeLo, uint32(core+100)); err != nil {
			t.Fatal(err)
		}
		got, err := d.ReadReg32(ctx, RegInputSizeLo)
		if err != nil || got != uint32(core+100) {
			t.Fatal(got, err)
		}
		if err = d.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = d.ReadReg32(ctx, RegStatus); !errors.Is(err, os.ErrClosed) {
			t.Fatal(err)
		}
		if err = d.EnableRegisterMmap(); !errors.Is(err, os.ErrClosed) {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for core := 0; core < 4; core++ {
		if binary.LittleEndian.Uint32(data[core*0x1000+8:]) != uint32(core+100) {
			t.Fatal("bank alias")
		}
	}
}

func TestMappedReadConcurrentClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bar")
	if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(Paths{User: path, H2C: "/dev/null", C2H: "/dev/zero"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.EnableRegisterMmap(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			_, err := d.ReadReg32(context.Background(), RegStatus)
			if err != nil {
				if !errors.Is(err, os.ErrClosed) {
					t.Error(err)
				}
				return
			}
		}
	}()
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
