package fpgagzip

import (
	"bytes"
	"context"
	"errors"
	"io"
)

var (
	// ErrWriterClosed is returned when Write is called after Close.
	ErrWriterClosed = errors.New("fpgagzip: writer is closed")
	// ErrWriterNotClosed is returned when Result is requested before Close.
	ErrWriterNotClosed = errors.New("fpgagzip: writer is not closed")
)

// Writer has the shape of compress/gzip.Writer while using the FPGA gzip
// shell as its compressor. Write appends uncompressed bytes to an in-memory
// buffer. Close submits the complete buffer to the FPGA and writes the gzip
// stream to the destination.
//
// The current shell requires the complete input length before it can start,
// so Write does not begin FPGA work. This API is intended for small and
// medium-sized payloads. For large inputs, use CompressReader with a known
// length or Compress/Run with a file path to avoid holding the input in RAM.
type Writer struct {
	dst    io.Writer
	hw     Hardware
	opts   Options
	report func(Progress)
	ctx    context.Context

	input     bytes.Buffer
	closed    bool
	err       error
	result    Result
	hasResult bool
}

// NewWriter returns an FPGA-backed writer using DefaultOptions. The argument
// order follows compress/gzip.NewWriter: the destination comes first.
func NewWriter(dst io.Writer, hw Hardware) *Writer {
	return NewWriterOptions(dst, hw, DefaultOptions(), nil)
}

// NewWriterOptions returns an FPGA-backed writer with explicit pipeline
// options and an optional progress callback.
func NewWriterOptions(dst io.Writer, hw Hardware, opts Options, report func(Progress)) *Writer {
	return NewWriterContext(context.Background(), dst, hw, opts, report)
}

// NewWriterContext is NewWriterOptions with a context used when Close starts
// the FPGA operation.
func NewWriterContext(ctx context.Context, dst io.Writer, hw Hardware, opts Options, report func(Progress)) *Writer {
	if ctx == nil {
		ctx = context.Background()
	}
	w := &Writer{dst: dst, hw: hw, opts: opts, report: report, ctx: ctx}
	if dst == nil {
		w.err = errors.New("fpgagzip: nil destination writer")
		return w
	}
	if hw == nil {
		w.err = errors.New("fpgagzip: nil FPGA hardware")
		return w
	}
	return w
}

// Write appends uncompressed data to the writer's memory buffer.
func (w *Writer) Write(p []byte) (int, error) {
	if w == nil {
		return 0, ErrWriterClosed
	}
	if w.closed {
		return 0, ErrWriterClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	return w.input.Write(p)
}

// Close compresses the complete input buffer through the FPGA and writes the
// resulting gzip stream to the destination. Close is idempotent and returns
// the first error from the operation.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	if w.closed {
		return w.err
	}
	w.closed = true
	defer func() { w.input = bytes.Buffer{} }()

	if w.err != nil {
		return w.err
	}
	result, err := CompressReader(w.ctx, bytes.NewReader(w.input.Bytes()), uint64(w.input.Len()), w.dst, w.hw, w.opts, w.report)
	if err != nil {
		w.err = err
		return w.err
	}
	w.result = result
	w.hasResult = true
	return nil
}

// Reset discards the current input and reuses the writer for a new
// destination. It mirrors compress/gzip.Writer.Reset; the hardware, options,
// context, and progress callback remain unchanged.
func (w *Writer) Reset(dst io.Writer) {
	if w == nil {
		return
	}
	w.dst = dst
	w.closed = false
	w.err = nil
	w.result = Result{}
	w.hasResult = false
	w.input = bytes.Buffer{}
	if dst == nil {
		w.err = errors.New("fpgagzip: nil destination writer")
	}
	if w.hw == nil {
		w.err = errors.New("fpgagzip: nil FPGA hardware")
	}
}

// Result returns timing and byte counters after a successful Close.
func (w *Writer) Result() (Result, error) {
	if w == nil || !w.closed {
		return Result{}, ErrWriterNotClosed
	}
	if w.err != nil {
		return Result{}, w.err
	}
	if !w.hasResult {
		return Result{}, errors.New("fpgagzip: compression result is unavailable")
	}
	return w.result, nil
}
