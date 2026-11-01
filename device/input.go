package device

import (
	"errors"
	"syscall"
)

// Older XDMA drivers pin every user buffer with FOLL_WRITE, including H2C
// input. Read-only mmap then fails before DMA is queued. Retry that specific
// zero-byte EFAULT using owned writable memory, and reuse it for this handle.
// A single stream's WriteH2C calls must be serialized by its transaction.
type dmaInput struct{ writable []byte }

func (b *dmaInput) write(p []byte, write func([]byte) (int, error)) (int, error) {
	if b.writable == nil {
		n, err := write(p)
		if n != 0 || !errors.Is(err, syscall.EFAULT) {
			return n, err
		}
	}
	if cap(b.writable) < len(p) {
		b.writable = make([]byte, len(p))
	}
	b.writable = b.writable[:len(p)]
	copy(b.writable, p)
	return write(b.writable)
}
