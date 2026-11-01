package pipeline

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

type borrowedInputWriter struct {
	ctx                  context.Context
	tx                   chan<- chunk
	stats                *runStats
	length, loaded, slot uint64
	crc                  uint32
}

func (w *borrowedInputWriter) Write(p []byte) (int, error) {
	consumed := 0
	for len(p) > 0 && w.loaded < w.length {
		if err := w.ctx.Err(); err != nil {
			return consumed, err
		}
		n := uint64(len(p))
		if n > w.slot {
			n = w.slot
		}
		if n > w.length-w.loaded {
			n = w.length - w.loaded
		}
		data := p[:n]
		w.crc = crc32.Update(w.crc, crc32.IEEETable, data)
		w.loaded += n
		consumed += int(n)
		p = p[n:]
		w.stats.fileLoaded.Store(w.loaded)
		select {
		case w.tx <- chunk{data: data, last: w.loaded == w.length}:
		case <-w.ctx.Done():
			return consumed, w.ctx.Err()
		}
	}
	return consumed, nil
}

func loadBorrowedInput(ctx context.Context, input io.WriterTo, length, slot uint64, tx chan<- chunk, stats *runStats) error {
	defer close(tx)
	w := &borrowedInputWriter{ctx: ctx, tx: tx, stats: stats, length: length, slot: slot}
	_, err := input.WriteTo(w)
	// bytes.Reader/Buffer retain the unconsumed suffix and report ShortWrite
	// when the declared transaction ends before their remaining input does.
	if err != nil && !(errors.Is(err, io.ErrShortWrite) && w.loaded == length) {
		return err
	}
	if w.loaded != length {
		return fmt.Errorf("read input at %d: %w", w.loaded, io.ErrUnexpectedEOF)
	}
	stats.inputCRC.Store(w.crc)
	return nil
}
