// Command benchmark-cores measures independent FPGA cores with distinct inputs.
// Output verification is complete and outside the timed compression window.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/pipeline"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

type result struct {
	Round     int
	Cores     int
	Procs     int
	Bytes     int64
	Seconds   float64
	MiBs      float64
	CoreMiBs  []float64
	Output    int64
	Allocated uint64
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	source := flag.String("source", "", "fixture")
	output := flag.String("output", "", "json")
	rounds := flag.Int("rounds", 3, "rounds")
	n := flag.Int("iterations", 32, "per core")
	flag.Parse()
	if *source == "" || *output == "" || *rounds < 1 || *n < 1 {
		flag.Usage()
		os.Exit(2)
	}
	encoded, err := os.ReadFile(*source)
	must(err)
	r, err := gzip.NewReader(bytes.NewReader(encoded))
	must(err)
	raw, err := io.ReadAll(r)
	must(err)
	must(r.Close())
	if len(raw) != 4<<20 {
		panic("fixture must decode to exactly 4 MiB")
	}
	var rows []result
	for round := 1; round <= *rounds; round++ {
		for _, cfg := range [][2]int{{1, 4}, {2, 8}, {4, 8}, {4, 16}} {
			runtime.GOMAXPROCS(cfg[1])
			runtime.GC()
			outputs := make([][]bytes.Buffer, cfg[0])
			for c := range outputs {
				outputs[c] = make([]bytes.Buffer, *n)
				for i := range outputs[c] {
					outputs[c][i].Grow(len(raw) + len(raw)/8 + 65536)
				}
			}
			inputs := make([][]byte, cfg[0])
			for c := range inputs {
				inputs[c] = append([]byte(nil), raw...)
				binary.LittleEndian.PutUint64(inputs[c][:8], uint64(c)+uint64(round)*100)
			}
			row := result{Round: round, Cores: cfg[0], Procs: cfg[1], CoreMiBs: make([]float64, cfg[0])}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var wg sync.WaitGroup
			start := time.Now()
			errs := make(chan error, cfg[0])
			for c := 0; c < cfg[0]; c++ {
				wg.Add(1)
				go func(c int) {
					defer wg.Done()
					var total time.Duration
					for i := 0; i < *n; i++ {
						paths := device.Paths{User: "/dev/xdma0_user", H2C: fmt.Sprintf("/dev/xdma0_h2c_%d", c), C2H: fmt.Sprintf("/dev/xdma0_c2h_%d", c), RegisterBase: uint32(c) * 0x1000}
						hw, e := device.Open(paths)
						if e != nil {
							errs <- e
							return
						}
						opts := pipeline.DefaultOptions()
						opts.RingSlots = 4
						opts.H2CSlotBytes = 1 << 20
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						stat, e := pipeline.RunReader(ctx, bytes.NewReader(inputs[c]), uint64(len(inputs[c])), &outputs[c][i], hw, opts, nil)
						cancel()
						hw.Close()
						if e != nil {
							errs <- fmt.Errorf("core %d: %w", c, e)
							return
						}
						total += stat.Total
					}
					row.CoreMiBs[c] = float64(len(raw)*(*n)) / (1 << 20) / total.Seconds()
				}(c)
			}
			wg.Wait()
			row.Seconds = time.Since(start).Seconds()
			close(errs)
			for e := range errs {
				must(e)
			}
			runtime.ReadMemStats(&after)
			row.Allocated = after.TotalAlloc - before.TotalAlloc
			row.Bytes = int64(len(raw) * (*n) * cfg[0])
			row.MiBs = float64(row.Bytes) / (1 << 20) / row.Seconds
			for c := range outputs {
				for i := range outputs[c] {
					rd, e := gzip.NewReader(bytes.NewReader(outputs[c][i].Bytes()))
					must(e)
					decoded, e := io.ReadAll(rd)
					must(e)
					must(rd.Close())
					if !bytes.Equal(inputs[c], decoded) {
						panic("FPGA content mismatch")
					}
					row.Output += int64(outputs[c][i].Len())
				}
			}
			rows = append(rows, row)
			b, e := json.MarshalIndent(rows, "", "  ")
			must(e)
			must(os.WriteFile(*output, append(b, '\n'), 0600))
			fmt.Printf("round=%d cores=%d P=%d MiB/s=%.1f core=%v\n", round, cfg[0], cfg[1], row.MiBs, row.CoreMiBs)
		}
	}
	fmt.Println("PASS", *output)
}
