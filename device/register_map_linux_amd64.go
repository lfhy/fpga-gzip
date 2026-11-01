//go:build linux && amd64

package device

import (
	"os"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// EnableRegisterMmap maps the selected bank's page through the XDMA driver's
// noncached BAR mapping. Select the bank before enabling it. On Linux/amd64,
// aligned 32-bit accesses preserve MMIO transaction width and x86 UC ordering.
// Other platforms retain the portable ReadAt/WriteAt register path.
func (d *Device) EnableRegisterMmap() error {
	d.registerMu.Lock()
	defer d.registerMu.Unlock()
	if d.registerClosed {
		return os.ErrClosed
	}
	if len(d.registerMap) != 0 {
		return nil
	}
	page := uint64(os.Getpagesize())
	base := uint64(d.registerBase) / page * page
	// Include the full protocol even when a custom aligned bank crosses pages.
	end := uint64(d.registerBase) + uint64(RegDebug) + 4
	length := (end - base + page - 1) / page * page
	mapped, err := unix.Mmap(int(d.user.Fd()), int64(base), int(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	d.registerMap, d.registerMapBase = mapped, base
	return nil
}

func (d *Device) readMappedRegister(offset uint32) (uint32, bool, error) {
	d.registerMu.RLock()
	defer d.registerMu.RUnlock()
	if d.registerClosed {
		return 0, true, os.ErrClosed
	}
	position := uint64(d.registerBase) + uint64(offset)
	if len(d.registerMap) == 0 || position < d.registerMapBase || position+4 > d.registerMapBase+uint64(len(d.registerMap)) || position%4 != 0 {
		return 0, false, nil
	}
	p := (*uint32)(unsafe.Pointer(&d.registerMap[position-d.registerMapBase]))
	// An atomic load is a single MOVL on amd64 and cannot be folded or cached.
	return atomic.LoadUint32(p), true, nil
}

//go:noinline
func storeMMIO32(p *uint32, value uint32) {
	// Keep a single-width store. atomic.StoreUint32 uses locked XCHG on amd64,
	// which is inappropriate for device MMIO. The non-inlined call prevents
	// compiler coalescing; the driver's uncached mapping supplies UC ordering.
	*p = value
}

func (d *Device) writeMappedRegister(offset, value uint32) (bool, error) {
	d.registerMu.Lock()
	defer d.registerMu.Unlock()
	if d.registerClosed {
		return true, os.ErrClosed
	}
	position := uint64(d.registerBase) + uint64(offset)
	if len(d.registerMap) == 0 || position < d.registerMapBase || position+4 > d.registerMapBase+uint64(len(d.registerMap)) || position%4 != 0 {
		return false, nil
	}
	storeMMIO32((*uint32)(unsafe.Pointer(&d.registerMap[position-d.registerMapBase])), value)
	return true, nil
}

func (d *Device) closeRegisterMap() error {
	d.registerMu.Lock()
	defer d.registerMu.Unlock()
	d.registerClosed = true
	if len(d.registerMap) == 0 {
		return nil
	}
	err := unix.Munmap(d.registerMap)
	d.registerMap = nil
	return err
}
