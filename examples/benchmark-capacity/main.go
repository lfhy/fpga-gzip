// Command benchmark-capacity compares bounded chunks and long streams in RAM.
// All outputs are retained and fully decoded after the timed compression group.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/pipeline"
)

type operation struct {
	Calls atomic.Uint64
	Nanos atomic.Uint64
}

func (o *operation) finish(start time.Time) { o.Calls.Add(1); o.Nanos.Add(uint64(time.Since(start))) }
func (o *operation) values() map[string]uint64 {
	return map[string]uint64{"calls": o.Calls.Load(), "nanos": o.Nanos.Load()}
}

type measured struct {
	pipeline.Hardware
	h2c, c2h, snapshots, arms operation
}

func (m *measured) PacketReadsEnabled() bool {
	p, ok := m.Hardware.(interface{ PacketReadsEnabled() bool })
	return ok && p.PacketReadsEnabled()
}

func (m *measured) WriteH2C(p []byte) (int, error) {
	start := time.Now()
	defer m.h2c.finish(start)
	return m.Hardware.WriteH2C(p)
}
func (m *measured) ReadC2H(p []byte) (int, error) {
	start := time.Now()
	defer m.c2h.finish(start)
	return m.Hardware.ReadC2H(p)
}
func (m *measured) TransferSnapshot(ctx context.Context) (device.Snapshot, error) {
	start := time.Now()
	defer m.snapshots.finish(start)
	return m.Hardware.TransferSnapshot(ctx)
}
func (m *measured) StartC2HRequest(ctx context.Context, n uint32) error {
	start := time.Now()
	defer m.arms.finish(start)
	return m.Hardware.StartC2HRequest(ctx, n)
}

type comparison struct {
	raw    []byte
	offset int
}

func (c *comparison) Write(p []byte) (int, error) {
	if len(p) > len(c.raw)-c.offset || !bytes.Equal(p, c.raw[c.offset:c.offset+len(p)]) {
		return 0, fmt.Errorf("decoded data mismatch at %d", c.offset)
	}
	c.offset += len(p)
	return len(p), nil
}

type result struct {
	Pattern           string                         `json:"pattern"`
	Round             int                            `json:"round"`
	Cores             int                            `json:"cores"`
	Procs             int                            `json:"procs"`
	ChunkBytes        int                            `json:"chunk_bytes"`
	IterationsPerCore int                            `json:"iterations_per_core"`
	InputBytes        int64                          `json:"input_bytes"`
	OutputBytes       int64                          `json:"output_bytes"`
	Seconds           float64                        `json:"seconds"`
	GBs               float64                        `json:"gb_s"`
	AllocatedBytes    uint64                         `json:"allocated_bytes"`
	CoreOperations    []map[string]map[string]uint64 `json:"core_operations"`
	Verified          bool                           `json:"verified"`
	PacketReads       bool                           `json:"packet_reads"`
	C2HRequestBytes   uint64                         `json:"c2h_request_bytes"`
}

func numbers(s string) ([]int, error) {
	var out []int
	for _, field := range strings.Split(s, ",") {
		n, err := strconv.Atoi(field)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid positive integer: %q", field)
		}
		out = append(out, n)
	}
	return out, nil
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func main() {
	source := flag.String("source", "", "gzip fixture, required for fixture pattern")
	output := flag.String("output", "", "JSON report")
	sizeList := flag.String("sizes-mib", "4,64", "comma-separated chunk sizes")
	coreList := flag.String("cores", "1,4", "comma-separated independent core counts")
	patterns := flag.String("patterns", "fixture,text,random", "comma-separated input patterns")
	perCore := flag.Int("per-core-mib", 1024, "input per core per case")
	procs := flag.Int("procs", 8, "Go P count, unchanged across core counts")
	rounds := flag.Int("rounds", 1, "rounds")
	h2c := flag.Int("h2c-mib", 1, "H2C slot size")
	c2h := flag.Int("c2h-kib", 32, "C2H request size")
	slots := flag.Int("slots", 4, "buffer slots")
	poll := flag.Duration("poll-interval", 50*time.Microsecond, "status poll interval")
	drain := flag.Duration("drain-idle", 200*time.Microsecond, "partial output drain interval")
	mmap := flag.Bool("mmap", false, "use optional mapped register access")
	packetSize := flag.Int("packet-kib", 1024, "negotiated EOP request size")
	legacy := flag.Bool("legacy", false, "force legacy exact-length packets")
	profiles := flag.String("profiles", "", "optional directory for per-case CPU profiles")
	flag.Parse()
	sizes, err := numbers(*sizeList)
	fail(err)
	cores, err := numbers(*coreList)
	fail(err)
	if *output == "" || *perCore < 1 || *procs < 1 || *rounds < 1 || *h2c < 1 || *c2h < 1 || *slots < 1 {
		fail(fmt.Errorf("invalid benchmark options"))
	}
	for _, c := range cores {
		if c > 4 {
			fail(fmt.Errorf("at most four cores"))
		}
	}
	for _, n := range sizes {
		if n > *perCore || *perCore%n != 0 {
			fail(fmt.Errorf("chunk size must divide per-core MiB"))
		}
	}
	var fixture []byte
	if strings.Contains(*patterns, "fixture") {
		f, err := os.Open(*source)
		fail(err)
		r, err := gzip.NewReader(f)
		fail(err)
		fixture, err = io.ReadAll(r)
		fail(err)
		fail(r.Close())
		fail(f.Close())
		if len(fixture) == 0 {
			fail(fmt.Errorf("empty fixture"))
		}
	}
	runtime.GOMAXPROCS(*procs)
	var results []result
	for round := 1; round <= *rounds; round++ {
		for _, pattern := range strings.Split(*patterns, ",") {
			for _, mib := range sizes {
				for _, count := range cores {
					opts := pipeline.DefaultOptions()
					opts.MaxProcs = *procs
					opts.H2CSlotBytes = uint64(*h2c) << 20
					opts.C2HRequestBytes = uint64(*c2h) << 10
					opts.RingSlots = *slots
					opts.PollInterval = *poll
					opts.DrainIdle = *drain
					opts.PacketRequestBytes = uint64(*packetSize) << 10
					profile := ""
					if *profiles != "" {
						fail(os.MkdirAll(*profiles, 0700))
						profile = filepath.Join(*profiles, fmt.Sprintf("%s-%dMiB-%dcores-r%d.pprof", pattern, mib, count, round))
					}
					row, err := run(pattern, round, mib<<20, count, *procs, *perCore/mib, fixture, opts, *mmap, *legacy, profile)
					fail(err)
					results = append(results, row)
					data, err := json.MarshalIndent(map[string]any{"unit": "decimal GB/s, input bytes / group wall time", "mmap": *mmap, "legacy": *legacy, "fixture": *source, "fixture_decoded_sha256": fmt.Sprintf("%x", sha256.Sum256(fixture)), "go_version": runtime.Version(), "arguments": os.Args, "options": opts, "results": results}, "", "  ")
					fail(err)
					fail(os.WriteFile(*output, append(data, '\n'), 0600))
					fmt.Printf("PASS round=%d pattern=%s chunk=%dMiB cores=%d P=%d input=%.1fGiB %.3f GB/s ratio=%.4f\n", round, pattern, mib, count, *procs, float64(row.InputBytes)/(1<<30), row.GBs, float64(row.OutputBytes)/float64(row.InputBytes))
				}
			}
		}
	}
}

func run(pattern string, round, size, cores, procs, iterations int, fixture []byte, opts pipeline.Options, mmap, legacy bool, profile string) (result, error) {
	raw := make([]byte, size)
	switch pattern {
	case "fixture":
		for pos := 0; pos < len(raw); {
			pos += copy(raw[pos:], fixture)
		}
	case "text":
		text := []byte("FPGA gzip capacity test: repeated text, complete byte verification.\n")
		for pos := 0; pos < len(raw); {
			pos += copy(raw[pos:], text)
		}
	case "random":
		_, err := rand.New(rand.NewSource(20261101)).Read(raw)
		if err != nil {
			return result{}, err
		}
	default:
		return result{}, fmt.Errorf("unknown pattern %s", pattern)
	}
	inputs := make([][]byte, cores)
	outputs := make([][]*bytes.Buffer, cores)
	meters := make([]*measured, cores)
	for c := range inputs {
		inputs[c] = append([]byte(nil), raw...)
		outputs[c] = make([]*bytes.Buffer, iterations)
		meters[c] = &measured{}
		for i := range outputs[c] {
			backing := make([]byte, size+size/8+65536)
			for pos := 0; pos < len(backing); pos += 4096 {
				backing[pos] = 1
			}
			outputs[c][i] = bytes.NewBuffer(backing[:0])
		}
	}
	runtime.GC()
	var pf *os.File
	if profile != "" {
		var err error
		pf, err = os.Create(profile)
		if err != nil {
			return result{}, err
		}
		if err = pprof.StartCPUProfile(pf); err != nil {
			pf.Close()
			return result{}, err
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, cores)
	for c := range inputs {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := range outputs[c] {
				binary.LittleEndian.PutUint64(inputs[c][:8], uint64(c)+uint64(i)*256+uint64(round)*1000000)
				hw, err := device.Open(device.Paths{User: "/dev/xdma0_user", H2C: fmt.Sprintf("/dev/xdma0_h2c_%d", c), C2H: fmt.Sprintf("/dev/xdma0_c2h_%d", c), RegisterBase: uint32(c) * 0x1000, LegacyPackets: legacy})
				if err != nil {
					errs <- err
					return
				}
				if mmap {
					mapped, ok := any(hw).(interface{ EnableRegisterMmap() error })
					if !ok {
						hw.Close()
						errs <- fmt.Errorf("SDK does not support mapped registers")
						return
					}
					if err = mapped.EnableRegisterMmap(); err != nil {
						hw.Close()
						errs <- err
						return
					}
				}
				meters[c].Hardware = hw
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				_, err = pipeline.RunReader(ctx, bytes.NewReader(inputs[c]), uint64(size), outputs[c][i], meters[c], opts, nil)
				cancel()
				closeErr := hw.Close()
				if err != nil {
					errs <- fmt.Errorf("core %d iteration %d: %w", c, i, err)
					return
				}
				if closeErr != nil {
					errs <- closeErr
					return
				}
			}
		}(c)
	}
	wg.Wait()
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if pf != nil {
		pprof.StopCPUProfile()
		pf.Close()
	}
	close(errs)
	for err := range errs {
		return result{}, err
	}
	row := result{Pattern: pattern, Round: round, Cores: cores, Procs: procs, ChunkBytes: size, IterationsPerCore: iterations, InputBytes: int64(size) * int64(cores) * int64(iterations), Seconds: elapsed.Seconds(), AllocatedBytes: after.TotalAlloc - before.TotalAlloc}
	row.GBs = float64(row.InputBytes) / elapsed.Seconds() / 1e9
	row.PacketReads = meters[0].PacketReadsEnabled()
	row.C2HRequestBytes = opts.C2HRequestBytes
	if row.PacketReads && opts.PacketRequestBytes != 0 {
		row.C2HRequestBytes = opts.PacketRequestBytes
	}
	for c := range outputs {
		for _, b := range outputs[c] {
			row.OutputBytes += int64(b.Len())
		}
		m := meters[c]
		row.CoreOperations = append(row.CoreOperations, map[string]map[string]uint64{"h2c": m.h2c.values(), "c2h": m.c2h.values(), "snapshots": m.snapshots.values(), "arms": m.arms.values()})
	}
	errs = make(chan error, cores)
	for c := range inputs {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i, b := range outputs[c] {
				binary.LittleEndian.PutUint64(inputs[c][:8], uint64(c)+uint64(i)*256+uint64(round)*1000000)
				r, err := gzip.NewReader(bytes.NewReader(b.Bytes()))
				if err != nil {
					errs <- err
					return
				}
				cmp := &comparison{raw: inputs[c]}
				_, err = io.Copy(cmp, r)
				closeErr := r.Close()
				if err != nil {
					errs <- err
					return
				}
				if closeErr != nil {
					errs <- closeErr
					return
				}
				if cmp.offset != size {
					errs <- fmt.Errorf("decoded size %d expected %d", cmp.offset, size)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return result{}, err
	}
	row.Verified = true
	return row, nil
}
