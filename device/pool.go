package device

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lfhy/fpga-gzip/firmware"
)

// FourCorePaths maps the validated four-core shell's independent DMA channels
// to their control banks. Explicit opt-in is required: node count alone does
// not establish that a bitstream contains independent gzip cores.
func FourCorePaths(first Paths, count int) ([]Paths, error) {
	if count < 1 || count > 4 {
		return nil, errors.New("core count must be between 1 and 4")
	}
	if count == 1 {
		return []Paths{first}, nil
	}
	if first.RegisterBase != 0 || !strings.HasSuffix(first.H2C, "_0") || !strings.HasSuffix(first.C2H, "_0") {
		return nil, errors.New("multiple cores require channel-0 paths and register base 0")
	}
	paths := make([]Paths, count)
	for core := range paths {
		paths[core] = Paths{User: first.User, H2C: strings.TrimSuffix(first.H2C, "_0") + fmt.Sprint("_", core),
			C2H: strings.TrimSuffix(first.C2H, "_0") + fmt.Sprint("_", core), RegisterBase: uint32(core) * 0x1000, LegacyPackets: first.LegacyPackets}
	}
	return paths, nil
}

// Pool leases independent streams. Each lease opens fresh handles and retains
// its core until Close, including after RunReader closes the streaming FDs.
// Pool owns no persistent FDs and requires no shutdown call.
type Pool struct {
	available chan Paths
	open      func(Paths) (*Device, error)
}

func NewPool(paths []Paths) (*Pool, error) {
	if len(paths) == 0 {
		return nil, errors.New("at least one device path is required")
	}
	seen := make(map[string]bool)
	banks := make(map[string][]uint32)
	for _, path := range paths {
		if path.User == "" || path.H2C == "" || path.C2H == "" || path.RegisterBase%4 != 0 || path.RegisterBase > ^uint32(0)-firmware.RegBuildHi {
			return nil, errors.New("invalid device paths or register base")
		}
		user := canonicalPath(path.User)
		for _, base := range banks[user] {
			// Include the final 32-bit register, without uint32 overflow.
			if uint64(path.RegisterBase) < uint64(base)+uint64(firmware.RegBuildHi)+4 &&
				uint64(base) < uint64(path.RegisterBase)+uint64(firmware.RegBuildHi)+4 {
				return nil, fmt.Errorf("overlapping control banks on %s: %#x and %#x", user, base, path.RegisterBase)
			}
		}
		banks[user] = append(banks[user], path.RegisterBase)
		for _, key := range []string{"h:" + canonicalPath(path.H2C), "c:" + canonicalPath(path.C2H)} {
			if seen[key] {
				return nil, fmt.Errorf("overlapping device stream or control bank: %s", key)
			}
			seen[key] = true
		}
	}
	p := &Pool{available: make(chan Paths, len(paths)), open: Open}
	for _, path := range paths {
		p.available <- path
	}
	return p, nil
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}

// Lease must be closed by the caller, even after a failed or cancelled run.
type Lease struct {
	*Device
	pool  *Pool
	paths Paths
	once  sync.Once
	err   error
}

func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var paths Paths
	select {
	case paths = <-p.available:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		p.available <- paths
		return nil, err
	}
	hw, err := p.open(paths)
	if err != nil {
		p.available <- paths
		return nil, err
	}
	return &Lease{Device: hw, pool: p, paths: paths}, nil
}

func (l *Lease) Close() error {
	l.once.Do(func() {
		l.err = l.Device.Close()
		l.pool.available <- l.paths
	})
	return l.err
}
