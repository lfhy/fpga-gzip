package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// verifyOutputs runs only after the compression timer and profiles have stopped.
// Each worker owns its expected input copy, including the per-member tag.
func verifyOutputs(inputs [][]byte, outputs [][]*bytes.Buffer, round, size, procs int) error {
	previous := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(previous)
	workers := (procs + len(inputs) - 1) / len(inputs)
	if workers > len(outputs[0]) {
		workers = len(outputs[0])
	}
	errs := make(chan error, len(inputs)*workers)
	var wg sync.WaitGroup
	for c := range inputs {
		for worker := 0; worker < workers; worker++ {
			wg.Add(1)
			go func(c, worker int) {
				defer wg.Done()
				expected := append([]byte(nil), inputs[c]...)
				for i := worker; i < len(outputs[c]); i += workers {
					binary.LittleEndian.PutUint64(expected[:8], uint64(c)+uint64(i)*256+uint64(round)*1000000)
					r, err := gzip.NewReader(bytes.NewReader(outputs[c][i].Bytes()))
					if err != nil {
						errs <- fmt.Errorf("verify core %d member %d: %w", c, i, err)
						return
					}
					cmp := &comparison{raw: expected}
					_, err = io.Copy(cmp, r)
					closeErr := r.Close()
					if err == nil {
						err = closeErr
					}
					if err == nil && cmp.offset != size {
						err = fmt.Errorf("decoded size %d expected %d", cmp.offset, size)
					}
					if err != nil {
						errs <- fmt.Errorf("verify core %d member %d: %w", c, i, err)
						return
					}
				}
			}(c, worker)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}
