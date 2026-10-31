// Command verify-cores validates the real board's independent gzip cores.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/pipeline"
	"io"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"time"
)

type result struct {
	Core    int    `json:"core"`
	Pattern string `json:"pattern"`
	Size    int    `json:"size"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

func main() {
	cores := flag.Int("cores", 4, "validated independent gzip cores, 1 to 4")
	output := flag.String("output", "", "JSON output")
	flag.Parse()
	if *output == "" {
		flag.Usage()
		os.Exit(2)
	}
	paths, err := device.FourCorePaths(device.Paths{User: "/dev/xdma0_user", H2C: "/dev/xdma0_h2c_0", C2H: "/dev/xdma0_c2h_0"}, *cores)
	if err != nil {
		panic(err)
	}
	runtime.GOMAXPROCS(8)
	sizes := []int{1, 2, 3, 7, 8, 9, 15, 16, 17, 63, 64, 65, 67, 68, 71, 72, 73, 79, 80, 81, 4095, 4096, 4097, 8191, 8192, 8193, 8255, 8256, 8257, 8259, 8260, 8263, 8264, 8265, 8271, 8273, 1000003, 1048643}
	rows := make(chan result, len(paths)*len(sizes)*3)
	var wg sync.WaitGroup
	for core, path := range paths {
		wg.Add(1)
		go func(core int, path device.Paths) {
			defer wg.Done()
			random := rand.New(rand.NewSource(int64(20261101 + core)))
			for _, pattern := range []string{"zero", "text", "random"} {
				for _, size := range sizes {
					raw := make([]byte, size)
					switch pattern {
					case "text":
						copy(raw, bytes.Repeat([]byte("gzip FPGA independent core validation\n"), size/37+2))
					case "random":
						random.Read(raw)
					}
					raw[0] = byte(core + 1)
					row := result{Core: core, Pattern: pattern, Size: size}
					hw, e := device.Open(path)
					if e == nil {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						var compressed bytes.Buffer
						opts := pipeline.DefaultOptions()
						opts.RingSlots = 4
						opts.H2CSlotBytes = 1 << 20
						if uint64(size) < opts.H2CSlotBytes {
							opts.H2CSlotBytes = uint64(size)
						}
						_, e = pipeline.RunReader(ctx, bytes.NewReader(raw), uint64(size), &compressed, hw, opts, nil)
						hw.Close()
						cancel()
						if e == nil {
							var rd *gzip.Reader
							rd, e = gzip.NewReader(&compressed)
							if e == nil {
								var decoded []byte
								decoded, e = io.ReadAll(rd)
								rd.Close()
								if e == nil && !bytes.Equal(raw, decoded) {
									e = fmt.Errorf("decoded content differs")
								}
							}
						}
					}
					row.OK = e == nil
					if e != nil {
						row.Error = e.Error()
					}
					rows <- row
					if !row.OK {
						return
					}
				}
			}
		}(core, path)
	}
	wg.Wait()
	close(rows)
	report := []result{}
	pass := true
	for row := range rows {
		report = append(report, row)
		pass = pass && row.OK
	}
	b, err := json.MarshalIndent(struct {
		Pass    bool     `json:"pass"`
		Results []result `json:"results"`
	}{pass, report}, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(*output, append(b, '\n'), 0600); err != nil {
		panic(err)
	}
	fmt.Printf("pass=%v cases=%d report=%s\n", pass, len(report), *output)
	if !pass {
		os.Exit(1)
	}
}
