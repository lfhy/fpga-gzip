package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lfhy/fpga-gzip/config"
	"github.com/lfhy/fpga-gzip/device"
	"github.com/lfhy/fpga-gzip/pipeline"
)

type chunkPlan struct {
	size  uint64
	count int
	bytes uint64
}

type parallelRunInfo struct {
	cores  int
	chunks int
}

// planChunks keeps each gzip member below maxBytes while balancing the input
// when the requested core count would otherwise leave some cores idle.
func planChunks(size uint64, cores int, maxBytes uint64) (chunkPlan, error) {
	if size == 0 {
		return chunkPlan{}, errors.New("empty input is not supported")
	}
	if cores < 1 || cores > 4 {
		return chunkPlan{}, errors.New("core count must be between 1 and 4")
	}
	if maxBytes == 0 {
		return chunkPlan{}, errors.New("chunk size must be greater than zero")
	}

	count := 1 + (size-1)/maxBytes
	wantCount := uint64(cores)
	if size < wantCount {
		wantCount = size
	}
	if count < wantCount {
		count = wantCount
	}
	if count > uint64(^uint(0)>>1) {
		return chunkPlan{}, errors.New("chunk count is too large for this platform")
	}
	chunkBytes := 1 + (size-1)/count
	return chunkPlan{size: size, count: int(count), bytes: chunkBytes}, nil
}

func (p chunkPlan) offset(index int) int64 {
	return int64(uint64(index) * p.bytes)
}

func (p chunkPlan) length(index int) uint64 {
	start := uint64(index) * p.bytes
	if remaining := p.size - start; remaining < p.bytes {
		return remaining
	}
	return p.bytes
}

func runParallel(ctx context.Context, cfg config.Config, opts pipeline.Options, showProgress bool, runInfo *parallelRunInfo) (pipeline.Result, error) {
	started := time.Now()
	input, err := os.Open(cfg.InputPath)
	if err != nil {
		return pipeline.Result{}, fmt.Errorf("open input %q: %w", cfg.InputPath, err)
	}
	defer input.Close()

	info, err := input.Stat()
	if err != nil {
		return pipeline.Result{}, fmt.Errorf("stat input %q: %w", cfg.InputPath, err)
	}
	if !info.Mode().IsRegular() {
		return pipeline.Result{}, fmt.Errorf("input %q is not a regular file", cfg.InputPath)
	}
	if info.Size() <= 0 {
		return pipeline.Result{}, errors.New("empty input files are not supported by this gzip shell")
	}
	plan, err := planChunks(uint64(info.Size()), int(cfg.Cores), cfg.ChunkBytes)
	if err != nil {
		return pipeline.Result{}, err
	}
	workers := plan.count
	if workers > int(cfg.Cores) {
		workers = int(cfg.Cores)
	}
	if runInfo != nil {
		runInfo.cores = workers
		runInfo.chunks = plan.count
	}
	paths, err := device.FourCorePaths(device.Paths{
		User:          cfg.UserDevice,
		H2C:           cfg.H2CDevice,
		C2H:           cfg.C2HDevice,
		LegacyPackets: cfg.LegacyPackets,
	}, workers)
	if err != nil {
		return pipeline.Result{}, err
	}
	pool, err := device.NewPool(paths)
	if err != nil {
		return pipeline.Result{}, err
	}

	outputDir := filepath.Dir(cfg.OutputPath)
	outputBase := filepath.Base(cfg.OutputPath)
	output, err := os.CreateTemp(outputDir, "."+outputBase+".parallel-*")
	if err != nil {
		return pipeline.Result{}, fmt.Errorf("create temporary output: %w", err)
	}
	tempOutput := output.Name()
	published := false
	defer func() {
		_ = output.Close()
		if !published {
			_ = os.Remove(tempOutput)
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make([]string, plan.count)
	results := make([]pipeline.Result, plan.count)
	ready := make(chan int, workers)
	var next atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	setErr := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	var progress *parallelProgress
	if showProgress {
		progress = newParallelProgress(plan.size, plan.count, opts.ProgressInterval)
	}

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := int(next.Add(1) - 1)
				if index >= plan.count {
					return
				}
				lease, err := pool.Acquire(runCtx)
				if err != nil {
					setErr(fmt.Errorf("acquire FPGA core for chunk %d: %w", index, err))
					return
				}
				if cfg.MmapRegisters {
					if err := lease.EnableRegisterMmap(); err != nil {
						_ = lease.Close()
						setErr(err)
						return
					}
				}
				length := plan.length(index)
				part, err := os.CreateTemp(outputDir, "."+outputBase+".part-*")
				if err != nil {
					_ = lease.Close()
					setErr(fmt.Errorf("create temporary chunk: %w", err))
					return
				}
				partPath := part.Name()
				var report func(pipeline.Progress)
				if progress != nil {
					report = progress.callback(index)
				}
				result, runErr := pipeline.RunReader(
					runCtx,
					io.NewSectionReader(input, plan.offset(index), int64(length)),
					length,
					part,
					lease,
					opts,
					report,
				)
				closeErr := part.Close()
				leaseErr := lease.Close()
				if runErr != nil {
					_ = os.Remove(partPath)
					setErr(fmt.Errorf("chunk %d: %w", index, runErr))
					return
				}
				if closeErr != nil {
					_ = os.Remove(partPath)
					setErr(fmt.Errorf("close chunk %d: %w", index, closeErr))
					return
				}
				if leaseErr != nil {
					_ = os.Remove(partPath)
					setErr(fmt.Errorf("close FPGA core for chunk %d: %w", index, leaseErr))
					return
				}
				parts[index] = partPath
				results[index] = result
				if progress != nil {
					progress.finish(index, length, result.OutputBytes)
				}
				select {
				case ready <- index:
				case <-runCtx.Done():
					return
				}
			}
		}()
	}

	var outputBytes uint64
	nextPart := 0
	assembled := make(map[int]bool, workers)
	for nextPart < plan.count && runCtx.Err() == nil {
		var index int
		select {
		case index = <-ready:
		case <-runCtx.Done():
			continue
		}
		assembled[index] = true
		for assembled[nextPart] {
			n, err := appendFile(output, parts[nextPart], results[nextPart].OutputBytes)
			if err != nil {
				setErr(fmt.Errorf("assemble chunk %d: %w", nextPart, err))
				break
			}
			outputBytes += n
			_ = os.Remove(parts[nextPart])
			parts[nextPart] = ""
			delete(assembled, nextPart)
			nextPart++
		}
	}

	cancel()
	wg.Wait()
	for _, path := range parts {
		if path != "" {
			_ = os.Remove(path)
		}
	}
	if firstErr == nil && nextPart != plan.count {
		if err := ctx.Err(); err != nil {
			firstErr = err
		} else {
			firstErr = errors.New("parallel compression stopped before all chunks were assembled")
		}
	}
	if firstErr != nil {
		return pipeline.Result{}, firstErr
	}
	if outputBytes == 0 {
		return pipeline.Result{}, errors.New("parallel compression produced no output")
	}

	total := time.Since(started)
	syncStarted := time.Now()
	if err := output.Sync(); err != nil {
		return pipeline.Result{}, fmt.Errorf("sync temporary output: %w", err)
	}
	syncDuration := time.Since(syncStarted)
	if err := output.Close(); err != nil {
		return pipeline.Result{}, fmt.Errorf("close temporary output: %w", err)
	}
	if err := os.Rename(tempOutput, cfg.OutputPath); err != nil {
		return pipeline.Result{}, fmt.Errorf("publish output %q: %w", cfg.OutputPath, err)
	}
	published = true
	if progress != nil {
		progress.print(true)
	}

	result := pipeline.Result{
		InputBytes:  plan.size,
		OutputBytes: outputBytes,
		Total:       total,
		Sync:        syncDuration,
		Pipeline:    total,
	}
	for _, chunkResult := range results {
		result.FPGA += chunkResult.FPGA
		result.C2H += chunkResult.C2H
		result.Status |= chunkResult.Status
		result.Debug |= chunkResult.Debug
		if result.C2HRequestBytes == 0 {
			result.C2HRequestBytes = chunkResult.C2HRequestBytes
		}
		result.PacketReads = result.PacketReads || chunkResult.PacketReads
		result.DirectOutput = result.DirectOutput || chunkResult.DirectOutput
	}
	return result, nil
}

func appendFile(dst *os.File, path string, expected uint64) (uint64, error) {
	src, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < 0 || uint64(info.Size()) != expected {
		return 0, fmt.Errorf("chunk size mismatch: file=%d FPGA=%d", info.Size(), expected)
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return 0, err
	}
	if uint64(n) != expected {
		return 0, fmt.Errorf("copied %d bytes, want %d", n, expected)
	}
	return uint64(n), nil
}

type parallelProgress struct {
	mu       sync.Mutex
	updates  map[int]pipeline.Progress
	total    uint64
	chunks   int
	finished int
	last     time.Time
	interval time.Duration
}

func newParallelProgress(total uint64, chunks int, interval time.Duration) *parallelProgress {
	return &parallelProgress{
		updates:  make(map[int]pipeline.Progress, chunks),
		total:    total,
		chunks:   chunks,
		interval: interval,
	}
}

func (p *parallelProgress) callback(index int) func(pipeline.Progress) {
	return func(progress pipeline.Progress) {
		p.mu.Lock()
		p.updates[index] = progress
		p.printLocked(false)
		p.mu.Unlock()
	}
}

func (p *parallelProgress) finish(index int, input, output uint64) {
	p.mu.Lock()
	p.updates[index] = pipeline.Progress{
		InputSize:      input,
		H2CSent:        input,
		FPGAInBytes:    input,
		FPGAOutBytes:   output,
		C2HReceived:    output,
		FileWritten:    output,
		CompressedSize: output,
	}
	p.finished++
	p.printLocked(false)
	p.mu.Unlock()
}

func (p *parallelProgress) print(force bool) {
	p.mu.Lock()
	p.printLocked(force)
	p.mu.Unlock()
}

func (p *parallelProgress) printLocked(force bool) {
	now := time.Now()
	if !force && !p.last.IsZero() && now.Sub(p.last) < p.interval {
		return
	}
	var sent, fpgaOut, received, written uint64
	for _, progress := range p.updates {
		sent += progress.H2CSent
		fpgaOut += progress.FPGAOutBytes
		received += progress.C2HReceived
		written += progress.FileWritten
	}
	fmt.Printf("\rH2C %6.2f%% (%d/%d)  并行 %d/%d  gzip member  OUT %d  C2H %d  FILE %d   ",
		percentage(sent, p.total), sent, p.total,
		p.finished, p.chunks, fpgaOut, received, written)
	p.last = now
}
