package pipeline

import (
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
		if err = hw.StartC2HRequest(ctx, uint32(request)); err != nil {
			pool.release(buf)
			return err
		}
		if received == 0 {
			stats.mark("first-c2h")
		}
		n, err := hw.ReadC2H(buf[:int(request)])
		if err != nil {
			pool.release(buf)
			return fmt.Errorf("packet C2H read: %w", err)
		}
		if n <= 0 || uint64(n) > request {
			pool.release(buf)
			return fmt.Errorf("invalid packet size %d for request %d", n, request)
		}
		received += uint64(n)
		stats.c2hReceived.Store(received)
		stats.mark("last-c2h")
		snapshot, err := hw.TransferSnapshot(ctx)
		if err != nil {
			pool.release(buf)
			return err
		}
		stats.setTransferSnapshot(snapshot)
		if snapshot.Status&device.StatusError != 0 || snapshot.OutBytes < received {
			pool.release(buf)
			return fmt.Errorf("invalid packet state: status=%#x produced=%d received=%d", snapshot.Status, snapshot.OutBytes, received)
		}
		last := snapshot.Status&device.StatusC2HDone != 0
		if last {
			if snapshot.OutBytes != received {
				pool.release(buf)
				return fmt.Errorf("EOS count mismatch: produced=%d received=%d", snapshot.OutBytes, received)
			}
			stats.compressed.Store(received)
			stats.mark("fpga-done")
		} else if uint64(n) != request {
			pool.release(buf)
			return fmt.Errorf("short packet without gzip EOS: requested=%d received=%d", request, n)
		}
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
