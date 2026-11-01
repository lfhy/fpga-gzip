package pipeline

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
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

type directPacketScript struct {
	packetScript
	output  *bytes.Buffer
	aliased bool
}

func (p *directPacketScript) ReadC2H(dst []byte) (int, error) {
	p.aliased = &dst[0] == &p.output.AvailableBuffer()[:len(dst)][0]
	return p.packetScript.ReadC2H(dst)
}

func TestDirectPacketOutputPreservesAliasedData(t *testing.T) {
	input := bytes.Repeat([]byte("direct FPGA output retains CRC, size and every payload byte;"), 1000)
	compressed := makeGzip(t, input)
	for _, request := range []int{16, 32, 1 << 20} {
		for _, preallocated := range []bool{false, true} {
			var output bytes.Buffer
			if preallocated {
				output.Grow(len(compressed) + request + 100)
			}
			// Include an unread prefix with a nonzero Buffer offset. Grow may
			// compact it, but must not change the bytes already in the output.
			output.WriteString("consumed:prefix:")
			output.Next(len("consumed:"))
			p := &directPacketScript{packetScript: packetScript{data: compressed}, output: &output}
			v := &gzipHeaderWriter{dst: &output}
			stats := &runStats{}
			done := make(chan struct{})
			if err := receivePacketsDirect(context.Background(), p, request, &output, v, stats, done); err != nil {
				t.Fatal(err)
			}
			if err := v.Validate(crc32.ChecksumIEEE(input), uint64(len(input))); err != nil {
				t.Fatal(err)
			}
			if !p.aliased || !bytes.Equal(output.Bytes(), append([]byte("prefix:"), compressed...)) {
				t.Fatal("DMA did not fill output capacity or changed its contents")
			}
			if stats.fileWritten.Load() != uint64(len(compressed)) || stats.compressed.Load() != uint64(len(compressed)) {
				t.Fatal("wrong direct-output byte count")
			}
			select {
			case <-done:
			default:
				t.Fatal("output completion was not signaled")
			}
		}
	}
}

func TestDirectPacketOutputRejectsBadBoundariesAndFrames(t *testing.T) {
	input := []byte("gzip direct DMA validation")
	for _, problem := range []string{"eos", "count", "header", "crc", "size"} {
		compressed := append([]byte(nil), makeGzip(t, input)...)
		var output bytes.Buffer
		p := &packetScript{data: compressed}
		switch problem {
		case "eos":
			p.missingEOS = true
		case "count":
			p.wrongCount = true
		case "header":
			compressed[0] ^= 1
		case "crc":
			compressed[len(compressed)-8] ^= 1
		case "size":
			compressed[len(compressed)-1] ^= 1
		}
		v := &gzipHeaderWriter{dst: &output}
		err := receivePacketsDirect(context.Background(), p, 1024, &output, v, &runStats{}, make(chan struct{}))
		if err == nil {
			err = v.Validate(crc32.ChecksumIEEE(input), uint64(len(input)))
		}
		if err == nil {
			t.Fatalf("accepted bad %s", problem)
		}
		if problem != "crc" && problem != "size" && output.Len() != 0 {
			t.Fatalf("published unvalidated %s", problem)
		}
	}
}

func TestDirectPacketTailUsesRemainingCapacity(t *testing.T) {
	input := bytes.Repeat([]byte("bounded output capacity"), 10000)
	compressed := makeGzip(t, input)
	output := bytes.NewBuffer(make([]byte, 0, len(compressed)+16))
	capacity := output.Cap()
	v := &gzipHeaderWriter{dst: output}
	p := &packetScript{data: compressed}
	if err := receivePacketsDirect(context.Background(), p, 1<<20, output, v, &runStats{}, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	if output.Cap() != capacity || !bytes.Equal(output.Bytes(), compressed) {
		t.Fatal("grew an output buffer that already fits the final member")
	}
	if err := v.Validate(crc32.ChecksumIEEE(input), uint64(len(input))); err != nil {
		t.Fatal(err)
	}
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
