package pipeline

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lfhy/fpga-gzip/device"
)

func packetReadsEnabled(hw Hardware) bool {
	packet, ok := hw.(interface{ PacketReadsEnabled() bool })
	return ok && packet.PacketReadsEnabled()
}

// receivePackets arms a request before all bytes have been produced. The
// negotiated shell terminates it at gzip EOS; XDMA eop_flush returns its true
// byte count. A short read is accepted only with hardware EOS and exact count.
func receivePackets(ctx context.Context, hw Hardware, opts Options, rx chan<- chunk, pool *bytePool, stats *runStats) error {
	defer close(rx)
	request := opts.PacketRequestBytes
	if request == 0 {
		request = opts.C2HRequestBytes
	}
	var received uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		buf, err := pool.acquire(ctx)
		if err != nil {
			return err
		}
		n, last, err := readPacket(ctx, hw, buf[:int(request)], received, stats)
		if err != nil {
			pool.release(buf)
			return err
		}
		received += uint64(n)
		select {
		case rx <- chunk{data: buf[:n], last: last}:
		case <-ctx.Done():
			pool.release(buf)
			return ctx.Err()
		}
		if last {
			return nil
		}
	}
}

// readPacket validates the hardware boundary before either destination publishes
// its bytes. Both the bounded writer pipeline and direct RAM output use it.
func readPacket(ctx context.Context, hw Hardware, buf []byte, received uint64, stats *runStats) (int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if err := hw.StartC2HRequest(ctx, uint32(len(buf))); err != nil {
		return 0, false, err
	}
	if received == 0 {
		stats.mark("first-c2h")
	}
	n, err := hw.ReadC2H(buf)
	if err != nil {
		return 0, false, fmt.Errorf("packet C2H read: %w", err)
	}
	if n <= 0 || n > len(buf) {
		return 0, false, fmt.Errorf("invalid packet size %d for request %d", n, len(buf))
	}
	received += uint64(n)
	stats.c2hReceived.Store(received)
	stats.mark("last-c2h")
	snapshot, err := hw.TransferSnapshot(ctx)
	if err != nil {
		return 0, false, err
	}
	stats.setTransferSnapshot(snapshot)
	if snapshot.Status&device.StatusError != 0 || snapshot.OutBytes < received {
		return 0, false, fmt.Errorf("invalid packet state: status=%#x produced=%d received=%d", snapshot.Status, snapshot.OutBytes, received)
	}
	last := snapshot.Status&device.StatusC2HDone != 0
	if last {
		if snapshot.OutBytes != received {
			return 0, false, fmt.Errorf("EOS count mismatch: produced=%d received=%d", snapshot.OutBytes, received)
		}
		stats.compressed.Store(received)
		stats.mark("fpga-done")
	} else if n != len(buf) {
		return 0, false, fmt.Errorf("short packet without gzip EOS: requested=%d received=%d", len(buf), n)
	}
	return n, last, nil
}

// receivePacketsDirect lets XDMA fill the unused capacity of a bytes.Buffer.
// Only gzip's built-in, non-transforming validator can commit these slices.
// Requests must fit the fixed header in one packet: collecting a split header
// would overwrite bytes still needed from a later overlapping DMA slice.
func receivePacketsDirect(ctx context.Context, hw Hardware, request int, output *bytes.Buffer, validator *gzipHeaderWriter, stats *runStats, done chan<- struct{}) error {
	defer close(done)
	var received uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The final DMA request can be larger than the remaining capacity
		// even when the actual gzip tail fits. Use that capacity first: a
		// blind Grow(request) would copy the whole result just for slack.
		next := request
		if available := output.Available(); available >= 16 {
			if available < next {
				next = available &^ 7
			}
		} else {
			output.Grow(request)
		}
		buf := output.AvailableBuffer()[:next]
		n, last, err := readPacket(ctx, hw, buf, received, stats)
		if err != nil {
			return err
		}
		if err = writeFileAll(validator, buf[:n]); err != nil {
			return fmt.Errorf("commit direct C2H output: %w", err)
		}
		received += uint64(n)
		stats.fileWritten.Store(received)
		if last {
			return nil
		}
	}
}
