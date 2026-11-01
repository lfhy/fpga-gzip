// Command benchmark-algorithms compares RAM compression using the current SDK,
// the DMA pipeline with verification deferred, and reusable pure Go encoders.
// A row is published only after every output has been decoded and byte-compared.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/lfhy/fpga-gzip/accelerator"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/firmware"
	"github.com/lfhy/fpga-gzip/pipeline"
	"github.com/minio/minlz"
	"github.com/pierrec/lz4/v4"
)

type row struct {
	Engine              string  `json:"engine"`
	Algorithm           string  `json:"algorithm"`
	Pattern             string  `json:"pattern"`
	Round               int     `json:"round"`
	Lanes               int     `json:"lanes"`
	ChunkBytes          int     `json:"chunk_bytes"`
	InputBytes          int64   `json:"input_bytes"`
	OutputBytes         int64   `json:"output_bytes"`
	Seconds             float64 `json:"seconds"`
	GBs                 float64 `json:"gb_s"`
	CPUSeconds          float64 `json:"cpu_seconds"`
	CPUCores            float64 `json:"cpu_cores"`
	AllocatedBytes      uint64  `json:"allocated_bytes"`
	VerificationSeconds float64 `json:"verification_seconds"`
	Verified            bool    `json:"verified"`
}

type report struct {
	Arguments     []string       `json:"arguments"`
	GoVersion     string         `json:"go_version"`
	Procs         int            `json:"procs"`
	FixtureSHA256 string         `json:"fixture_decoded_sha256"`
	Identity      *firmware.Info `json:"identity,omitempty"`
	Results       []row          `json:"results"`
}

type encoder func(raw []byte, dst *bytes.Buffer) error

func cpuEncoder(a firmware.Algorithm) (encoder, func(), error) {
	switch a {
	case firmware.Raw:
		return func(raw []byte, dst *bytes.Buffer) error { _, e := dst.Write(raw); return e }, func() {}, nil
	case firmware.Gzip:
		w, e := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return func(raw []byte, dst *bytes.Buffer) error {
			w.Reset(dst)
			if _, e := w.Write(raw); e != nil {
				return e
			}
			return w.Close()
		}, func() {}, e
	case firmware.LZ4:
		w := lz4.NewWriter(io.Discard)
		e := w.Apply(lz4.BlockSizeOption(lz4.Block64Kb), lz4.ConcurrencyOption(1), lz4.ChecksumOption(true))
		return func(raw []byte, dst *bytes.Buffer) error {
			w.Reset(dst)
			if _, e := w.Write(raw); e != nil {
				return e
			}
			return w.Close()
		}, func() {}, e
	case firmware.Zstd:
		w, e := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
		if e != nil {
			return nil, nil, e
		}
		return func(raw []byte, dst *bytes.Buffer) error {
			data := w.EncodeAll(raw, dst.AvailableBuffer())
			_, e := dst.Write(data)
			return e
		}, func() { _ = w.Close() }, nil
	case firmware.MinLZ:
		w := minlz.NewWriter(io.Discard, minlz.WriterConcurrency(1), minlz.WriterBlockSize(1<<20), minlz.WriterLevel(minlz.LevelFastest))
		return func(raw []byte, dst *bytes.Buffer) error {
			w.Reset(dst)
			if _, err := w.Write(raw); err != nil {
				return err
			}
			return w.Close()
		}, func() {}, nil
	}
	return nil, nil, errors.New("unknown algorithm")
}

type comparison struct {
	raw    []byte
	offset int
}

func (c *comparison) Write(p []byte) (int, error) {
	if len(p) > len(c.raw)-c.offset || !bytes.Equal(p, c.raw[c.offset:c.offset+len(p)]) {
		return 0, fmt.Errorf("decoded content differs at byte %d", c.offset)
	}
	c.offset += len(p)
	return len(p), nil
}

func verify(a firmware.Algorithm, encoded, expected []byte) error {
	var reader io.Reader = bytes.NewReader(encoded)
	switch a {
	case firmware.Gzip:
		g, e := gzip.NewReader(reader)
		if e != nil {
			return e
		}
		defer g.Close()
		reader = g
	case firmware.LZ4:
		reader = lz4.NewReader(reader)
	case firmware.MinLZ:
		reader = minlz.NewReader(reader, minlz.ReaderMaxBlockSize(8<<20))
	case firmware.Zstd:
		z, e := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
		if e != nil {
			return e
		}
		defer z.Close()
		reader = z
	}
	c := &comparison{raw: expected}
	_, e := io.Copy(c, reader)
	if e == nil && c.offset != len(expected) {
		e = fmt.Errorf("decoded length %d != %d", c.offset, len(expected))
	}
	return e
}

// postponedCheck is exclusively a benchmark observer. It deliberately postpones
// format/content verification until after timing; run refuses to publish its row
// unless every retained output passes independent full decoding and comparison.
// Production callers must use accelerator.CompressReader instead.
type postponedCheck struct {
	dst    *bytes.Buffer
	size   uint64
	called bool
}

func (v *postponedCheck) Write(p []byte) (int, error) { return v.dst.Write(p) }
func (v *postponedCheck) Validate(_ uint32, size uint64) error {
	v.called = true
	if size != v.size {
		return errors.New("unexpected pipeline input size")
	}
	return nil
}

func cpuTime() float64 {
	var u syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_SELF, &u); e != nil {
		panic(e)
	}
	return float64(u.Utime.Sec+u.Stime.Sec) + float64(u.Utime.Usec+u.Stime.Usec)/1e6
}

func tag(raw []byte, lane, iteration int) {
	binary.LittleEndian.PutUint64(raw[:8], uint64(lane)+uint64(iteration)*256)
}

func run(engine string, algorithm firmware.Algorithm, pattern string, round, size, lanes, iterations int, base []byte, procs int) (row, error) {
	inputs := make([][]byte, lanes)
	outputs := make([][]*bytes.Buffer, lanes)
	encoders := make([]encoder, lanes)
	for c := range inputs {
		inputs[c] = append([]byte(nil), base...)
		outputs[c] = make([]*bytes.Buffer, iterations)
		for i := range outputs[c] {
			backing := make([]byte, size+size/4+(2<<20))
			for p := 0; p < len(backing); p += 4096 {
				backing[p] = 1
			}
			outputs[c][i] = bytes.NewBuffer(backing[:0])
		}
		if engine == "cpu" {
			enc, close, e := cpuEncoder(algorithm)
			if e != nil {
				return row{}, e
			}
			defer close()
			encoders[c] = enc
			// Fault encoder state and dictionary pages in before timing.
			if e := enc(inputs[c], outputs[c][0]); e != nil {
				return row{}, e
			}
			if e := verify(algorithm, outputs[c][0].Bytes(), inputs[c]); e != nil {
				return row{}, e
			}
			outputs[c][0].Reset()
		}
	}
	opts := accelerator.DefaultOptions()
	opts.RingSlots, opts.H2CSlotBytes, opts.PacketRequestBytes, opts.MaxProcs = 4, 1<<20, 1<<20, procs
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	startCPU := cpuTime()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, lanes)
	for c := range inputs {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i, dst := range outputs[c] {
				tag(inputs[c], c, i)
				if engine == "cpu" {
					if e := encoders[c](inputs[c], dst); e != nil {
						errs <- e
						return
					}
					continue
				}
				hw, e := device.Open(device.Paths{User: "/dev/xdma0_user", H2C: fmt.Sprintf("/dev/xdma0_h2c_%d", c), C2H: fmt.Sprintf("/dev/xdma0_c2h_%d", c), RegisterBase: uint32(c) * 0x1000})
				if e != nil {
					errs <- e
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				if engine == "fpga-sdk" {
					_, e = accelerator.CompressReader(ctx, algorithm, bytes.NewReader(inputs[c]), uint64(size), dst, hw, opts, nil)
				} else {
					v := &postponedCheck{dst: dst, size: uint64(size)}
					_, e = pipeline.RunReaderAlgorithm(ctx, bytes.NewReader(inputs[c]), uint64(size), dst, hw, opts, nil, algorithm, v)
					if e == nil && !v.called {
						e = errors.New("deferred verification was not scheduled")
					}
				}
				cancel()
				closeErr := hw.Close()
				if e == nil {
					e = closeErr
				}
				if e != nil {
					errs <- fmt.Errorf("lane %d iteration %d: %w", c, i, e)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	usedCPU := cpuTime() - startCPU
	runtime.ReadMemStats(&after)
	close(errs)
	for e := range errs {
		return row{}, e
	}
	vstart := time.Now()
	var stored int64
	for c := range outputs {
		for i, dst := range outputs[c] {
			tag(inputs[c], c, i)
			if e := verify(algorithm, dst.Bytes(), inputs[c]); e != nil {
				return row{}, fmt.Errorf("lane %d iteration %d: %w", c, i, e)
			}
			stored += int64(dst.Len())
		}
	}
	input := int64(size) * int64(lanes) * int64(iterations)
	return row{Engine: engine, Algorithm: algorithm.String(), Pattern: pattern, Round: round, Lanes: lanes, ChunkBytes: size, InputBytes: input, OutputBytes: stored, Seconds: elapsed, GBs: float64(input) / elapsed / 1e9, CPUSeconds: usedCPU, CPUCores: usedCPU / elapsed, AllocatedBytes: after.TotalAlloc - before.TotalAlloc, VerificationSeconds: time.Since(vstart).Seconds(), Verified: true}, nil
}

func positiveList(s string) ([]int, error) {
	var out []int
	for _, v := range strings.Split(s, ",") {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 {
			return nil, errors.New("expected comma-separated positive integers")
		}
		out = append(out, n)
	}
	return out, nil
}

func main() {
	engine := flag.String("engine", "fpga-sdk", "fpga-sdk, fpga-pipeline, or cpu")
	algorithm := flag.String("algorithm", "gzip", "raw, gzip, lz4, zstd or minlz")
	source := flag.String("source", "", "gzip-compressed fixture")
	output := flag.String("output", "", "JSON output")
	sizes := flag.String("sizes-mib", "4,64", "chunk sizes")
	lanes := flag.String("lanes", "1,4", "independent cores or CPU encoder lanes")
	patterns := flag.String("patterns", "fixture,text,random", "input patterns")
	perLane := flag.Int("per-lane-mib", 128, "input per lane and case")
	rounds := flag.Int("rounds", 3, "repetitions")
	procs := flag.Int("procs", 8, "standalone benchmark GOMAXPROCS")
	flag.Parse()
	if e := mainRun(*engine, *algorithm, *source, *output, *sizes, *lanes, *patterns, *perLane, *rounds, *procs); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func mainRun(engine, name, source, output, sizesText, lanesText, patternsText string, perLane, rounds, procs int) error {
	if engine != "cpu" && engine != "fpga-sdk" && engine != "fpga-pipeline" {
		return errors.New("unknown engine")
	}
	algorithm, e := firmware.ParseAlgorithm(name)
	if e != nil {
		return e
	}
	sizes, e := positiveList(sizesText)
	if e != nil {
		return e
	}
	lanes, e := positiveList(lanesText)
	if e != nil {
		return e
	}
	if output == "" || perLane < 1 || rounds < 1 || procs < 1 {
		return errors.New("invalid benchmark settings")
	}
	for _, n := range sizes {
		if n > 256 || perLane%n != 0 {
			return errors.New("chunk must divide per-lane size and be at most 256 MiB")
		}
	}
	for _, n := range lanes {
		if n > 32 {
			return errors.New("at most 32 lanes")
		}
	}
	for _, p := range strings.Split(patternsText, ",") {
		if p != "fixture" && p != "text" && p != "random" {
			return errors.New("unknown pattern")
		}
	}
	runtime.GOMAXPROCS(procs)
	var fixture []byte
	if strings.Contains(patternsText, "fixture") {
		f, e := os.Open(source)
		if e != nil {
			return e
		}
		g, e := gzip.NewReader(f)
		if e != nil {
			f.Close()
			return e
		}
		fixture, e = io.ReadAll(g)
		closeErr := g.Close()
		fileErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if fileErr != nil {
			return fileErr
		}
		if len(fixture) == 0 {
			return errors.New("empty fixture")
		}
	}
	rep := report{Arguments: os.Args, GoVersion: runtime.Version(), Procs: procs, FixtureSHA256: fmt.Sprintf("%x", sha256.Sum256(fixture))}
	if engine != "cpu" {
		lock, e := firmware.LockTransaction("/dev/xdma0_user")
		if e != nil {
			return e
		}
		info, e := firmware.InspectBAR(context.Background(), "/dev/xdma0_user", 0)
		lock.Close()
		if e != nil {
			return e
		}
		if info.Algorithm != algorithm {
			return firmware.ErrAlgorithm
		}
		for _, n := range lanes {
			if n > int(info.Cores) {
				return errors.New("lane count exceeds active firmware cores")
			}
		}
		rep.Identity = &info
	}
	for round := 1; round <= rounds; round++ {
		for _, pattern := range strings.Split(patternsText, ",") {
			for _, mib := range sizes {
				base := make([]byte, mib<<20)
				switch pattern {
				case "fixture":
					for pos := 0; pos < len(base); {
						pos += copy(base[pos:], fixture)
					}
				case "text":
					text := []byte("FPGA gzip capacity test: repeated text, complete byte verification.\n")
					for pos := 0; pos < len(base); {
						pos += copy(base[pos:], text)
					}
				case "random":
					_, e = rand.New(rand.NewSource(20261101)).Read(base)
					if e != nil {
						return e
					}
				}
				for _, count := range lanes {
					v, e := run(engine, algorithm, pattern, round, len(base), count, perLane/mib, base, procs)
					if e != nil {
						return e
					}
					rep.Results = append(rep.Results, v)
					b, e := json.MarshalIndent(rep, "", "  ")
					if e != nil {
						return e
					}
					if e = os.WriteFile(output, append(b, '\n'), 0600); e != nil {
						return e
					}
					fmt.Printf("PASS %s %s %s r%d %dMiB lanes=%d %.3f GB/s ratio=%.5f CPU=%.2f cores\n", engine, name, pattern, round, mib, count, v.GBs, float64(v.OutputBytes)/float64(v.InputBytes), v.CPUCores)
				}
			}
		}
	}
	return nil
}
