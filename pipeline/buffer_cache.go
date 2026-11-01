package pipeline

import "sync"

// DMA buffers are returned only after every transaction worker has stopped.
// Keep both byte and entry bounds so unusual option sizes cannot grow metadata.
var dmaBuffers = struct {
	sync.Mutex
	bySize map[uint64][][]byte
	bytes  uint64
	count  int
}{bySize: make(map[uint64][][]byte)}

func takeDMABuffer(size uint64) []byte {
	dmaBuffers.Lock()
	list := dmaBuffers.bySize[size]
	if len(list) != 0 {
		buf := list[len(list)-1]
		if len(list) == 1 {
			delete(dmaBuffers.bySize, size)
		} else {
			dmaBuffers.bySize[size] = list[:len(list)-1]
		}
		dmaBuffers.bytes -= size
		dmaBuffers.count--
		dmaBuffers.Unlock()
		return buf
	}
	dmaBuffers.Unlock()
	return make([]byte, int(size))
}

func returnDMABuffer(buf []byte) {
	size := uint64(cap(buf))
	dmaBuffers.Lock()
	defer dmaBuffers.Unlock()
	if dmaBuffers.count >= 64 || size > 64<<20 || dmaBuffers.bytes+size > 64<<20 {
		return
	}
	dmaBuffers.bySize[size] = append(dmaBuffers.bySize[size], buf[:cap(buf)])
	dmaBuffers.bytes += size
	dmaBuffers.count++
}

func (p *bytePool) releaseIdle() {
	if p == nil {
		return
	}
	for {
		select {
		case buf := <-p.buffers:
			returnDMABuffer(buf)
		default:
			return
		}
	}
}
