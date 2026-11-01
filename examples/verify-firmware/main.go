// Command verify-firmware checks every active core against independent software
// decoders, including sub-beat tails, block boundaries and a large input.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/lfhy/fpga-gzip/accelerator"
	"github.com/lfhy/fpga-gzip/client"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/firmware"
	"github.com/pierrec/lz4/v4"
)

type row struct {
	Core    int     `json:"core"`
	Pattern string  `json:"pattern"`
	Input   int     `json:"input"`
	Output  int     `json:"output"`
	Seconds float64 `json:"seconds"`
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitempty"`
}
type report struct {
	Algorithm              string        `json:"algorithm"`
	Identity               firmware.Info `json:"identity"`
	WrongAlgorithmRejected bool          `json:"wrong_algorithm_rejected_before_dma"`
	Pass                   bool          `json:"pass"`
	Results                []row         `json:"results"`
	Service                string        `json:"service,omitempty"`
}

func main() {
	name := flag.String("algorithm", "", "loaded image: raw, gzip, lz4 or zstd")
	output := flag.String("output", "", "JSON report path")
	large := flag.Int("large", 8<<20, "additional input size per pattern/core (bytes)")
	socket := flag.String("unix", "", "exercise fpga-gzipd through this Unix socket instead of direct DMA")
	quick := flag.Bool("quick", false, "check four boundary sizes per pattern/core")
	edge := flag.Bool("edge", false, "check pure zero, one-symbol and two-symbol inputs without a core marker")
	flag.Parse()
	algorithm, err := firmware.ParseAlgorithm(*name)
	if err != nil || *output == "" || *large < 1 || *large > 256<<20 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	lock, err := firmware.LockTransaction("/dev/xdma0_user")
	if err != nil {
		panic(err)
	}
	identity, err := firmware.InspectBAR(ctx, "/dev/xdma0_user", 0)
	lock.Close()
	if err != nil || identity.Algorithm != algorithm {
		panic(fmt.Sprintf("unexpected image %+v: %v", identity, err))
	}
	cores := int(identity.Cores)
	if identity.Legacy {
		cores = 4
	}
	paths, err := device.FourCorePaths(device.Paths{User: "/dev/xdma0_user", H2C: "/dev/xdma0_h2c_0", C2H: "/dev/xdma0_c2h_0"}, cores)
	if err != nil {
		panic(err)
	}
	opts := accelerator.DefaultOptions()
	opts.RingSlots, opts.H2CSlotBytes = 4, 1<<20
	r := report{Algorithm: algorithm.String(), Identity: identity, Pass: true}
	var service *client.Client
	if *socket != "" {
		service, err = client.NewUnix(*socket)
		if err != nil {
			panic(err)
		}
		defer service.CloseIdleConnections()
		r.Service = *socket
	}
	// Both counters and output must stay unchanged for an incompatible request.
	hw, err := device.Open(paths[0])
	if err != nil {
		panic(err)
	}
	before, e := hw.TransferSnapshot(ctx)
	if e != nil {
		panic(e)
	}
	wrong := firmware.Gzip
	if algorithm == wrong {
		wrong = firmware.LZ4
	}
	var rejected bytes.Buffer
	_, e = accelerator.CompressReader(ctx, wrong, bytes.NewReader([]byte("must not start")), 14, &rejected, hw, opts, nil)
	after, e2 := hw.TransferSnapshot(ctx)
	hw.Close()
	r.WrongAlgorithmRejected = errors.Is(e, firmware.ErrAlgorithm) && e2 == nil && rejected.Len() == 0 && before.InBytes == after.InBytes && before.OutBytes == after.OutBytes
	r.Pass = r.WrongAlgorithmRejected
	sizes := []int{1, 7, 8, 9, 64, 65, 71, 72, 73, 127, 128, 129, 255, 1023, 8191, 8192, 8193, 32767, 32768, 32769, 65536, 65537, 1048576, *large}
	if *quick {
		sizes = []int{73, 8193, 65537, *large}
	}
	patterns := []string{"zero", "text", "random"}
	if *edge {
		patterns = []string{"all-zero", "repeat-A", "binary"}
		if !*quick {
			sizes = []int{129, 8191, 8192, 8193, 65537, *large}
		}
	}
	rows := make(chan row, cores*len(sizes)*3)
	var wg sync.WaitGroup
	for core, path := range paths {
		wg.Add(1)
		go func(core int, path device.Paths) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(20261101 + core)))
			for _, pattern := range patterns {
				for _, size := range sizes {
					raw := make([]byte, size)
					switch pattern {
					case "text":
						copy(raw, bytes.Repeat([]byte("standard compression image verification\n"), size/39+2))
					case "random":
						_, _ = rng.Read(raw)
					case "repeat-A":
						for i := range raw {
							raw[i] = 'A'
						}
					case "binary":
						for i := range raw {
							raw[i] = byte(i & 1)
						}
					}
					if !*edge {
						raw[0] = byte(core + 1)
					}
					v := row{Core: core, Pattern: pattern, Input: size}
					var h *device.Device
					var e error
					if service == nil {
						h, e = device.Open(path)
					}
					if e == nil {
						transaction, cancel := context.WithTimeout(ctx, 60*time.Second)
						var encoded bytes.Buffer
						start := time.Now()
						if service == nil {
							_, e = accelerator.CompressReader(transaction, algorithm, bytes.NewReader(raw), uint64(size), &encoded, h, opts, nil)
						} else {
							_, e = service.CompressAlgorithm(transaction, algorithm, bytes.NewReader(raw), int64(size), &encoded)
						}
						v.Seconds = time.Since(start).Seconds()
						v.Output = encoded.Len()
						if h != nil {
							h.Close()
						}
						cancel()
						if e == nil {
							e = compare(algorithm, raw, encoded.Bytes())
						}
					}
					v.OK = e == nil
					if e != nil {
						v.Error = e.Error()
					}
					rows <- v
					if e != nil {
						return
					}
				}
			}
		}(core, path)
	}
	wg.Wait()
	close(rows)
	for v := range rows {
		r.Results = append(r.Results, v)
		r.Pass = r.Pass && v.OK
	}
	sort.Slice(r.Results, func(i, j int) bool {
		a, b := r.Results[i], r.Results[j]
		if a.Core != b.Core {
			return a.Core < b.Core
		}
		if a.Pattern != b.Pattern {
			return a.Pattern < b.Pattern
		}
		return a.Input < b.Input
	})
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(*output, append(b, '\n'), 0600); err != nil {
		panic(err)
	}
	fmt.Printf("%s pass=%v cases=%d wrong-algorithm-rejected=%v report=%s\n", r.Algorithm, r.Pass, len(r.Results), r.WrongAlgorithmRejected, *output)
	if !r.Pass {
		os.Exit(1)
	}
}

func compare(algorithm firmware.Algorithm, raw, encoded []byte) error {
	var reader io.Reader = bytes.NewReader(encoded)
	switch algorithm {
	case firmware.Gzip:
		r, err := gzip.NewReader(reader)
		if err != nil {
			return err
		}
		defer r.Close()
		reader = r
	case firmware.LZ4:
		reader = lz4.NewReader(reader)
	case firmware.Zstd:
		r, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		if err != nil {
			return err
		}
		defer r.Close()
		reader = r
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(len(raw))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, decoded) {
		return errors.New("independent decoded output differs from original bytes")
	}
	return nil
}
