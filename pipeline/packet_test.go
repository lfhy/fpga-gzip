package pipeline

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/lfhy/fpga-gzip/device"
)

type packetScript struct {
	Hardware
	data                   []byte
	read, request          int
	missingEOS, wrongCount bool
	order                  []string
}

func (p *packetScript) StartC2HRequest(_ context.Context, n uint32) error {
	p.request = int(n)
	p.order = append(p.order, "arm")
	return nil
}
func (p *packetScript) ReadC2H(dst []byte) (int, error) {
	p.order = append(p.order, "read")
	n := copy(dst, p.data[p.read:])
	p.read += n
	return n, nil
}
func (p *packetScript) TransferSnapshot(context.Context) (device.Snapshot, error) {
	p.order = append(p.order, "snapshot")
	s := device.Snapshot{OutBytes: uint64(len(p.data))}
	if p.read == len(p.data) {
		s.Status = device.StatusDone
		if !p.missingEOS {
			s.Status |= device.StatusC2HDone
		}
	}
	if p.wrongCount {
		s.OutBytes++
	}
	return s, nil
}

func TestPacketEOSCountsAndEarlyArming(t *testing.T) {
	for _, length := range []int{7, 16, 51} {
		p := &packetScript{data: bytes.Repeat([]byte{'x'}, length)}
		opts := DefaultOptions()
		opts.PacketRequestBytes = 16
		pool, err := newBytePool(2, 16)
		if err != nil {
			t.Fatal(err)
		}
		rx := make(chan chunk)
		done := make(chan []byte, 1)
		go func() {
			var data []byte
			for c := range rx {
				data = append(data, c.data...)
				pool.release(c.data)
			}
			done <- data
		}()
		stats := &runStats{}
		if err = receivePackets(context.Background(), p, opts, rx, pool, stats); err != nil {
			t.Fatal(err)
		}
		if got := <-done; !bytes.Equal(got, p.data) {
			t.Fatal("decoded byte accounting")
		}
		if stats.compressed.Load() != uint64(length) {
			t.Fatal("final count")
		}
		if len(p.order) != ((length+15)/16)*3 || p.order[0] != "arm" || p.order[1] != "read" || p.order[2] != "snapshot" {
			t.Fatal("polled before arming or read after EOS", p.order)
		}
	}
}

func TestPacketRejectsUnconfirmedTail(t *testing.T) {
	for _, p := range []*packetScript{{data: []byte("tail"), missingEOS: true}, {data: []byte("tail"), wrongCount: true}} {
		opts := DefaultOptions()
		opts.PacketRequestBytes = 16
		pool, err := newBytePool(2, 16)
		if err != nil {
			t.Fatal(err)
		}
		rx := make(chan chunk, 2)
		if err := receivePackets(context.Background(), p, opts, rx, pool, &runStats{}); err == nil {
			t.Fatal("accepted unconfirmed short DMA tail")
		}
		if c, ok := <-rx; ok {
			t.Fatal("published invalid packet", c)
		}
	}
	p := &packetScript{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rx := make(chan chunk)
	if err := receivePackets(ctx, p, DefaultOptions(), rx, nil, &runStats{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
