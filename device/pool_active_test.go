package device

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lfhy/fpga-gzip/firmware"
)

func TestPoolFollowsTwoAndFourCoreImages(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user")
	identity := func(cores uint32) {
		bar := make([]byte, 0x4000)
		for _, v := range [][2]uint32{{0x44, firmware.Magic}, {0x48, firmware.ABI}, {0x4c, uint32(firmware.Zstd)}, {0x50, firmware.FormatV1}, {0x54, cores}, {0x58, 3}, {0x5c, cores}} {
			binary.LittleEndian.PutUint32(bar[v[0]:], v[1])
		}
		if err := os.WriteFile(user, bar, 0600); err != nil {
			t.Fatal(err)
		}
	}
	identity(2)
	var paths []Paths
	for i := 0; i < 4; i++ {
		h, c := filepath.Join(dir, fmt.Sprint("h2c", i)), filepath.Join(dir, fmt.Sprint("c2h", i))
		for _, name := range []string{h, c} {
			if err := os.WriteFile(name, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		paths = append(paths, Paths{User: user, H2C: h, C2H: c, RegisterBase: uint32(i) * 0x1000})
	}
	p, err := NewPool(paths)
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.AcquireActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.AcquireActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.paths.RegisterBase >= 0x2000 || b.paths.RegisterBase >= 0x2000 || a.paths == b.paths {
		t.Fatal("inactive or duplicated core")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if extra, err := p.AcquireActive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if extra != nil {
			extra.Close()
		}
		t.Fatalf("inactive core admitted: %v", err)
	}
	a.Close()
	b.Close()
	identity(4)
	var leases []*Lease
	defer func() {
		for _, l := range leases {
			l.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		l, err := p.AcquireActive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	seen := make(map[uint32]bool)
	for _, l := range leases {
		seen[l.paths.RegisterBase] = true
	}
	if len(seen) != 4 {
		t.Fatal("new image did not restore four independent cores")
	}
}
