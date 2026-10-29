// Package pipeline implements the gzip shell's file-to-file streaming
// workflow.  It knows the application protocol, while the device package only
// knows how to access registers and XDMA character devices.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lfhy/fpga-gzip/device"
)

const (
	beatBytes = uint64(8)
)

// Options controls buffering, polling and timeout behavior.
type Options struct {
	RingSlots        int
	H2CSlotBytes     uint64
	C2HRequestBytes  uint64
	PollInterval     time.Duration
	DrainIdle        time.Duration
	ResetTimeout     time.Duration
	StartTimeout     time.Duration
	ProgressInterval time.Duration
	MaxProcs         int
}

// DefaultOptions uses the ring and H2C defaults validated for the bundled
// gzip shell. The Go default uses the 256 KiB C2H request that avoided the 1 MiB boundary
// timeout observed with this shell; callers must revalidate it for other RTL.
func DefaultOptions() Options {
	return Options{
		RingSlots:    8,
		H2CSlotBytes: 4 * 1024 * 1024,
		// Larger requests can leave a 262160-byte partial packet outstanding on
		// this shell during a long transfer.
		C2HRequestBytes:  256 * 1024,
		PollInterval:     50 * time.Microsecond,
		DrainIdle:        200 * time.Microsecond,
		ResetTimeout:     2 * time.Second,
		StartTimeout:     time.Second,
		ProgressInterval: 100 * time.Millisecond,
		MaxProcs:         1,
	}
}

// Hardware is the application-facing device contract. device.Device is the
// production implementation; keeping this interface here makes the pipeline
// independent of OS files and straightforward to exercise with a simulator.
type Hardware interface {
	WriteH2C([]byte) (int, error)
	ReadC2H([]byte) (int, error)
	CloseStreams() error
	Snapshot(context.Context) (device.Snapshot, error)
	TransferSnapshot(context.Context) (device.Snapshot, error)
	SoftReset(context.Context, time.Duration) error
	Start(context.Context, uint64, time.Duration) error
	StartC2HRequest(context.Context, uint32) error
}

// Progress is a point-in-time view suitable for a terminal or structured log.
type Progress struct {
	InputSize      uint64
	H2CSent        uint64
	FPGAInBytes    uint64
	FPGAOutBytes   uint64
	C2HReceived    uint64
	FileWritten    uint64
	Status         uint32
	Debug          uint32
	CompressedSize uint64
}

// Result contains validated counters and timing information from one run.
type Result struct {
	InputBytes  uint64
	OutputBytes uint64
	Total       time.Duration
	Sync        time.Duration
	FPGA        time.Duration
	C2H         time.Duration
	Pipeline    time.Duration
	Status      uint32
	Debug       uint32
}

type chunk struct {
	data []byte
	last bool
}

// bytePool owns the fixed-size DMA buffers used by one stage of the
// pipeline.  Reusing buffers is important for XDMA: allocating a new 1 MiB
// or 4 MiB slice for every packet adds allocator/zeroing work and can leave
// the FPGA output FIFO without a reader on long runs.
type bytePool struct {
	buffers chan []byte
}

func newBytePool(slots int, size uint64) (*bytePool, error) {
	maxInt := uint64(^uint(0) >> 1)
	if size == 0 || size > maxInt {
		return nil, fmt.Errorf("buffer size %d is too large for this platform", size)
	}
	p := &bytePool{buffers: make(chan []byte, slots)}
	for i := 0; i < slots; i++ {
		p.buffers <- make([]byte, int(size))
	}
	return p, nil
}

func (p *bytePool) acquire(ctx context.Context) ([]byte, error) {
	select {
	case buf := <-p.buffers:
		return buf, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *bytePool) release(buf []byte) {
	if buf == nil {
		return
	}
	p.buffers <- buf[:cap(buf)]
}

type runStats struct {
	fileLoaded   atomic.Uint64
	h2cSent      atomic.Uint64
	fpgaInBytes  atomic.Uint64
	fpgaOutBytes atomic.Uint64
	c2hReceived  atomic.Uint64
	fileWritten  atomic.Uint64
	compressed   atomic.Uint64
	status       atomic.Uint32
	debug        atomic.Uint32

	mu        sync.Mutex
	firstH2C  time.Time
	lastH2C   time.Time
	fpgaStart time.Time
	fpgaDone  time.Time
	firstC2H  time.Time
	lastC2H   time.Time
}

func (s *runStats) setSnapshot(snapshot device.Snapshot) {
	s.status.Store(snapshot.Status)
	s.debug.Store(snapshot.Debug)
	s.fpgaInBytes.Store(snapshot.InBytes)
	s.fpgaOutBytes.Store(snapshot.OutBytes)
}

func (s *runStats) setTransferSnapshot(snapshot device.Snapshot) {
	s.status.Store(snapshot.Status)
	s.debug.Store(snapshot.Debug)
	s.fpgaOutBytes.Store(snapshot.OutBytes)
}

func (s *runStats) mark(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	switch name {
	case "first-h2c":
		if s.firstH2C.IsZero() {
			s.firstH2C = now
		}
	case "last-h2c":
		s.lastH2C = now
	case "fpga-start":
		s.fpgaStart = now
	case "fpga-done":
		s.fpgaDone = now
	case "first-c2h":
		if s.firstC2H.IsZero() {
			s.firstC2H = now
		}
	case "last-c2h":
		s.lastC2H = now
	}
}

func (s *runStats) duration(name string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch name {
	case "fpga":
		if !s.fpgaStart.IsZero() && !s.fpgaDone.IsZero() {
			return s.fpgaDone.Sub(s.fpgaStart)
		}
	case "c2h":
		if !s.firstC2H.IsZero() && !s.lastC2H.IsZero() {
			return s.lastC2H.Sub(s.firstC2H)
		}
	case "pipeline":
		if !s.firstH2C.IsZero() && !s.lastC2H.IsZero() {
			return s.lastC2H.Sub(s.firstH2C)
		}
	}
	return 0
}

// Run compresses inputPath to outputPath through the FPGA.
func Run(ctx context.Context, inputPath, outputPath string, hw Hardware, opts Options, report func(Progress)) (Result, error) {
	input, err := os.Open(inputPath)
	if err != nil {
		return Result{}, fmt.Errorf("open input %q: %w", inputPath, err)
	}
	defer input.Close()

	info, err := input.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("stat input %q: %w", inputPath, err)
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("input %q is not a regular file", inputPath)
	}
	if info.Size() <= 0 {
		return Result{}, errors.New("empty input files are not supported by this gzip shell")
	}

	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return Result{}, fmt.Errorf("open output %q: %w", outputPath, err)
	}
	defer output.Close()

	return RunReader(ctx, input, uint64(info.Size()), output, hw, opts, report)
}

// RunReader compresses exactly inputSize bytes from input and writes the gzip
// stream to output. The input and output are used directly; no temporary file
// is created. This is the preferred API for memory buffers, pipes, sockets and
// other callers that already know the input length.
func RunReader(ctx context.Context, input io.Reader, inputSize uint64, output io.Writer, hw Hardware, opts Options, report func(Progress)) (Result, error) {
	if input == nil {
		return Result{}, errors.New("input reader is nil")
	}
	if output == nil {
		return Result{}, errors.New("output writer is nil")
	}
	if inputSize == 0 {
		return Result{}, errors.New("empty input is not supported by this gzip shell")
	}
	if hw == nil {
		return Result{}, errors.New("FPGA hardware is nil")
	}
	if err := validateOptions(opts); err != nil {
		return Result{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime.GOMAXPROCS(opts.MaxProcs)
	runStart := time.Now()

	if err := hw.SoftReset(ctx, opts.ResetTimeout); err != nil {
		return Result{}, fmt.Errorf("soft-reset FPGA: %w", err)
	}

	stats := &runStats{}
	h2cPool, err := newBytePool(opts.RingSlots, opts.H2CSlotBytes)
	if err != nil {
		return Result{}, fmt.Errorf("allocate H2C buffers: %w", err)
	}
	c2hPool, err := newBytePool(opts.RingSlots, opts.C2HRequestBytes)
	if err != nil {
		return Result{}, fmt.Errorf("allocate C2H buffers: %w", err)
	}
	stats.mark("fpga-start")
	if err := hw.Start(ctx, inputSize, opts.StartTimeout); err != nil {
		return Result{}, fmt.Errorf("start FPGA gzip task: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interruptFinished := make(chan struct{})
	go func() {
		<-runCtx.Done()
		_ = hw.CloseStreams()
		close(interruptFinished)
	}()

	tx := make(chan chunk, opts.RingSlots)
	rx := make(chan chunk, opts.RingSlots)
	storeDone := make(chan struct{})
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	worker := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errCh <- err:
				default:
				}
				cancel()
			}
		}()
	}

	validatedOutput := &gzipHeaderWriter{dst: output}
	worker(func() error { return storeOutput(runCtx, validatedOutput, rx, c2hPool, stats, storeDone) })
	worker(func() error { return receiveC2H(runCtx, hw, opts, rx, c2hPool, stats) })
	worker(func() error { return sendH2C(runCtx, hw, inputSize, tx, h2cPool, stats) })
	worker(func() error {
		return loadInput(runCtx, input, inputSize, opts.H2CSlotBytes, tx, h2cPool, stats)
	})
	if report != nil {
		worker(func() error {
			return reportProgress(runCtx, inputSize, opts.ProgressInterval, report, stats, storeDone)
		})
	}

	wg.Wait()

	var runErr error
	select {
	case runErr = <-errCh:
	default:
		if err := ctx.Err(); err != nil {
			runErr = err
		}
	}
	if runErr == nil {
		if err := validatedOutput.Validate(); err != nil {
			runErr = err
		}
	}

	// Capture the final register state before cancelling the watcher. The user
	// BAR remains open while CloseStreams only interrupts H2C/C2H.
	finalSnapshot, snapshotErr := hw.Snapshot(context.Background())
	if runErr == nil && snapshotErr != nil {
		runErr = fmt.Errorf("read final FPGA state: %w", snapshotErr)
	}

	cancel()
	<-interruptFinished

	if runErr != nil {
		if truncater, ok := output.(interface{ Truncate(int64) error }); ok {
			_ = truncater.Truncate(0)
		}
		resetCtx, resetCancel := context.WithTimeout(context.Background(), opts.ResetTimeout)
		resetErr := hw.SoftReset(resetCtx, opts.ResetTimeout)
		resetCancel()
		if resetErr != nil {
			return Result{}, fmt.Errorf("%w (recovery reset failed: %v)", runErr, resetErr)
		}
		return Result{}, runErr
	}

	outputBytes := stats.compressed.Load()
	writtenBytes := stats.fileWritten.Load()
	if outputBytes == 0 || outputBytes != writtenBytes {
		return Result{}, fmt.Errorf("output size mismatch: FPGA=%d file=%d", outputBytes, writtenBytes)
	}
	if finalSnapshot.Status&device.StatusDone == 0 || finalSnapshot.Status&device.StatusError != 0 {
		return Result{}, fmt.Errorf("invalid final FPGA status: status=0x%08x debug=0x%08x", finalSnapshot.Status, finalSnapshot.Debug)
	}
	if finalSnapshot.InBytes != inputSize || finalSnapshot.OutBytes != outputBytes {
		return Result{}, fmt.Errorf("FPGA byte counters mismatch: in=%d/%d out=%d/%d", finalSnapshot.InBytes, inputSize, finalSnapshot.OutBytes, outputBytes)
	}

	// Total ends after the streaming pipeline and final register checks, while
	// durable-file flushing is reported separately. Files are synced before Run
	// returns; generic writers do not need a sync step.
	total := time.Since(runStart)
	var syncDuration time.Duration
	if syncer, ok := output.(interface{ Sync() error }); ok {
		syncStart := time.Now()
		if err := syncer.Sync(); err != nil {
			return Result{}, fmt.Errorf("sync output: %w", err)
		}
		syncDuration = time.Since(syncStart)
	}

	return Result{
		InputBytes:  inputSize,
		OutputBytes: outputBytes,
		Total:       total,
		Sync:        syncDuration,
		FPGA:        stats.duration("fpga"),
		C2H:         stats.duration("c2h"),
		Pipeline:    stats.duration("pipeline"),
		Status:      finalSnapshot.Status,
		Debug:       finalSnapshot.Debug,
	}, nil
}

func validateOptions(opts Options) error {
	if opts.RingSlots <= 0 {
		return errors.New("ring slots must be greater than zero")
	}
	if opts.MaxProcs <= 0 {
		return errors.New("max procs must be greater than zero")
	}
	if opts.H2CSlotBytes == 0 {
		return errors.New("H2C slot size must be greater than zero")
	}
	if opts.H2CSlotBytes > uint64(^uint(0)>>1) {
		return errors.New("H2C slot size is too large for this platform")
	}
	if opts.C2HRequestBytes < beatBytes || opts.C2HRequestBytes > math.MaxUint32 {
		return fmt.Errorf("C2H request size must be between %d and %d", beatBytes, uint64(math.MaxUint32))
	}
	if opts.PollInterval <= 0 || opts.DrainIdle <= 0 || opts.ResetTimeout <= 0 || opts.StartTimeout <= 0 || opts.ProgressInterval <= 0 {
		return errors.New("pipeline timing values must be greater than zero")
	}
	return nil
}

func loadInput(ctx context.Context, input io.Reader, inputSize, slotSize uint64, tx chan<- chunk, pool *bytePool, stats *runStats) error {
	defer close(tx)
	var loaded uint64
	for loaded < inputSize {
		wanted := slotSize
		if remaining := inputSize - loaded; remaining < wanted {
			wanted = remaining
		}
		buf, err := pool.acquire(ctx)
		if err != nil {
			return err
		}
		n, err := io.ReadFull(input, buf[:int(wanted)])
		if err != nil {
			pool.release(buf)
			return fmt.Errorf("read input at %d: %w", loaded, err)
		}
		loaded += uint64(n)
		stats.fileLoaded.Store(loaded)
		select {
		case tx <- chunk{data: buf[:n], last: loaded == inputSize}:
		case <-ctx.Done():
			pool.release(buf)
			return ctx.Err()
		}
	}
	return nil
}

func sendH2C(ctx context.Context, hw Hardware, inputSize uint64, tx <-chan chunk, pool *bytePool, stats *runStats) error {
	var sent uint64
	first := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-tx:
			if !ok {
				if sent != inputSize {
					return fmt.Errorf("H2C ended early: sent=%d expected=%d", sent, inputSize)
				}
				return nil
			}
			if first {
				stats.mark("first-h2c")
				first = false
			}
			err := writeAll(hw, item.data)
			pool.release(item.data)
			if err != nil {
				return fmt.Errorf("XDMA H2C write: %w", err)
			}
			sent += uint64(len(item.data))
			stats.h2cSent.Store(sent)
			stats.mark("last-h2c")
		}
	}
}

func writeAll(hw Hardware, data []byte) error {
	for len(data) > 0 {
		n, err := hw.WriteH2C(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func receiveC2H(ctx context.Context, hw Hardware, opts Options, rx chan<- chunk, pool *bytePool, stats *runStats) error {
	defer close(rx)
	received := uint64(0)
	lastProduced := uint64(0)
	lastChange := time.Now()
	firstC2H := true
	fpgaDoneMarked := false
	maxRequest := opts.C2HRequestBytes
	if maxRequest > math.MaxUint32 {
		maxRequest = math.MaxUint32
	}
	alignedPreferred := maxRequest - maxRequest%beatBytes
	if alignedPreferred == 0 {
		return errors.New("C2H request size must be at least 8 bytes")
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := hw.TransferSnapshot(ctx)
		if err != nil {
			return fmt.Errorf("read FPGA status/out-bytes: %w", err)
		}
		stats.setTransferSnapshot(snapshot)
		if snapshot.Status&device.StatusError != 0 {
			return fmt.Errorf("FPGA reported error: status=0x%08x debug=0x%08x", snapshot.Status, snapshot.Debug)
		}
		produced := snapshot.OutBytes
		if produced < received {
			return fmt.Errorf("OUT_BYTES moved backwards: produced=%d received=%d", produced, received)
		}
		if produced != lastProduced {
			lastProduced = produced
			lastChange = time.Now()
		}
		gzipDone := snapshot.Status&device.StatusDone != 0
		if gzipDone && !fpgaDoneMarked {
			stats.mark("fpga-done")
			fpgaDoneMarked = true
		}

		available := produced - received
		var request uint64
		lastPacket := false
		switch {
		case available >= alignedPreferred:
			request = alignedPreferred
		case gzipDone && available > 0:
			if available <= maxRequest {
				request = available
				lastPacket = true
			} else {
				request = alignedPreferred
			}
		case available >= beatBytes && time.Since(lastChange) >= opts.DrainIdle:
			request = available - available%beatBytes
			if request > alignedPreferred {
				request = alignedPreferred
			}
		}

		if request > 0 {
			buf, err := pool.acquire(ctx)
			if err != nil {
				return err
			}
			if err := hw.StartC2HRequest(ctx, uint32(request)); err != nil {
				pool.release(buf)
				return fmt.Errorf("start C2H request (%d bytes): %w", request, err)
			}
			if firstC2H {
				stats.mark("first-c2h")
				firstC2H = false
			}
			n, err := hw.ReadC2H(buf[:int(request)])
			if err != nil {
				pool.release(buf)
				return fmt.Errorf("XDMA C2H read (%d bytes): %w", request, err)
			}
			if n != int(request) {
				pool.release(buf)
				return fmt.Errorf("C2H packet length mismatch: requested=%d received=%d", request, n)
			}
			received += request
			stats.c2hReceived.Store(received)
			stats.mark("last-c2h")
			select {
			case rx <- chunk{data: buf[:int(request)], last: lastPacket}:
			case <-ctx.Done():
				pool.release(buf)
				return ctx.Err()
			}
			if lastPacket {
				stats.compressed.Store(produced)
				return nil
			}
			continue
		}

		if gzipDone && available == 0 {
			stats.compressed.Store(produced)
			select {
			case rx <- chunk{last: true}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := sleepContext(ctx, opts.PollInterval); err != nil {
			return err
		}
	}
}

func storeOutput(ctx context.Context, output io.Writer, rx <-chan chunk, pool *bytePool, stats *runStats, done chan<- struct{}) error {
	defer close(done)
	var written uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-rx:
			if !ok {
				return errors.New("C2H ended without an end-of-stream marker")
			}
			if len(item.data) > 0 {
				err := writeFileAll(output, item.data)
				pool.release(item.data)
				if err != nil {
					return fmt.Errorf("write output: %w", err)
				}
				written += uint64(len(item.data))
				stats.fileWritten.Store(written)
			}
			if item.last {
				return nil
			}
		}
	}
}

// gzipHeaderWriter rejects a successful FPGA transaction that did not produce
// a gzip stream. XDMA and the shell expose status and byte counters separately
// from payload correctness; checking the small fixed gzip header keeps a bad
// bitstream from being accepted and stored as a valid chunk. The full stream
// remains streamed directly to the caller without buffering it in memory.
type gzipHeaderWriter struct {
	dst     io.Writer
	header  [10]byte
	seen    int
	checked bool
	err     error
}

func (w *gzipHeaderWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if !w.checked {
		need := len(w.header) - w.seen
		take := len(p)
		if take > need {
			take = need
		}
		copy(w.header[w.seen:], p[:take])
		w.seen += take
		if w.seen < len(w.header) {
			// Keep the prefix private until the complete fixed header has been
			// checked. This prevents an invalid FPGA stream from being exposed
			// through a generic writer before RunReader can report the error.
			return len(p), nil
		}
		if err := validateGzipHeader(w.header[:]); err != nil {
			w.err = err
			return 0, err
		}
		w.checked = true
		if _, err := w.write(w.header[:]); err != nil {
			return take, err
		}
		if take == len(p) {
			return len(p), nil
		}
		written, err := w.write(p[take:])
		if err != nil {
			return take + written, err
		}
		return len(p), nil
	}
	return w.write(p)
}

func (w *gzipHeaderWriter) write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if err != nil {
		w.err = err
		return n, err
	}
	if n != len(p) {
		w.err = io.ErrShortWrite
		return n, w.err
	}
	return n, nil
}

func (w *gzipHeaderWriter) Validate() error {
	if w.err != nil {
		return w.err
	}
	if !w.checked || w.seen < len(w.header) {
		return errors.New("FPGA output is shorter than a gzip header")
	}
	return nil
}

func validateGzipHeader(header []byte) error {
	if len(header) < 10 || header[0] != 0x1f || header[1] != 0x8b || header[2] != 8 {
		return errors.New("FPGA output is not a gzip stream")
	}
	if header[3]&0xe0 != 0 {
		return fmt.Errorf("FPGA gzip header has reserved flags: 0x%02x", header[3])
	}
	return nil
}

func writeFileAll(output io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := output.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func reportProgress(ctx context.Context, inputSize uint64, interval time.Duration, report func(Progress), stats *runStats, storeDone <-chan struct{}) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			report(Progress{
				InputSize:      inputSize,
				H2CSent:        stats.h2cSent.Load(),
				FPGAInBytes:    stats.fpgaInBytes.Load(),
				FPGAOutBytes:   stats.fpgaOutBytes.Load(),
				C2HReceived:    stats.c2hReceived.Load(),
				FileWritten:    stats.fileWritten.Load(),
				Status:         stats.status.Load(),
				Debug:          stats.debug.Load(),
				CompressedSize: stats.compressed.Load(),
			})
		case <-storeDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
