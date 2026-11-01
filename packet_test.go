package fpgagzip

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A whole gzip member becomes available after the input is accepted, while
// C2H requests may already be armed. Reads return EOP-short packets at EOS.
type packetHardware struct {
	*fakeHardware
	ready, stop         chan struct{}
	readyOnce, stopOnce sync.Once
}

func (*packetHardware) PacketReadsEnabled() bool { return true }
func (p *packetHardware) SoftReset(ctx context.Context, timeout time.Duration) error {
	if err := p.fakeHardware.SoftReset(ctx, timeout); err != nil {
		return err
	}
	p.ready = make(chan struct{})
	p.stop = make(chan struct{})
	p.readyOnce = sync.Once{}
	p.stopOnce = sync.Once{}
	return nil
}
func (p *packetHardware) WriteH2C(data []byte) (int, error) {
	n, err := p.fakeHardware.WriteH2C(data)
	p.mu.Lock()
	done := uint64(len(p.input)) == p.expected
	p.mu.Unlock()
	if done {
		p.readyOnce.Do(func() { close(p.ready) })
	}
	return n, err
}
func (p *packetHardware) ReadC2H(dst []byte) (int, error) {
	select {
	case <-p.ready:
	case <-p.stop:
		return 0, context.Canceled
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(dst) != int(p.request) {
		return 0, fmt.Errorf("wrong request")
	}
	n := copy(dst, p.output[p.read:])
	p.read += n
	p.request = 0
	if p.read == len(p.output) {
		p.status |= 1 << 4
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}
func (p *packetHardware) CloseStreams() error { p.stopOnce.Do(func() { close(p.stop) }); return nil }

func TestPacketCompressionPreservesCRCAndLength(t *testing.T) {
	input := bytes.Repeat([]byte("packet stream test, gzip CRC and exact input;"), 3000)
	for _, direct := range []bool{false, true} {
		for _, request := range []uint64{8, 64, 1 << 20} {
			hw := &packetHardware{fakeHardware: newFakeHardware()}
			opts := DefaultOptions()
			opts.PacketRequestBytes = request
			opts.DirectOutput = direct
			opts.H2CSlotBytes = 4096
			var out bytes.Buffer
			result, err := CompressReader(context.Background(), bytes.NewReader(input), uint64(len(input)), &out, hw, opts, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !result.PacketReads || result.C2HRequestBytes != request || result.InputChecksumIEEE() != crc32.ChecksumIEEE(input) {
				t.Fatal(result)
			}
			if result.DirectOutput != (direct && request >= 16) {
				t.Fatal("wrong direct-output selection", result)
			}
			assertGzipData(t, out.Bytes(), input)
		}
	}
}

func TestPacketRejectsUnalignedFallbackBeforeHardwareReset(t *testing.T) {
	hw := &packetHardware{fakeHardware: newFakeHardware()}
	opts := DefaultOptions()
	opts.PacketRequestBytes = 0
	opts.C2HRequestBytes = 9
	_, err := CompressReader(context.Background(), bytes.NewReader([]byte("input")), 5, io.Discard, hw, opts, nil)
	if err == nil || !strings.Contains(err.Error(), "multiple of 8") || hw.ready != nil {
		t.Fatal("unsafe request reached hardware", err)
	}
}
