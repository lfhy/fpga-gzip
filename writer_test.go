package fpgagzip

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

func TestCompressReaderAndWriter(t *testing.T) {
	input := bytes.Repeat([]byte("fpga-gzip-test-data-"), 4096)
	opts := DefaultOptions()

	var direct bytes.Buffer
	result, err := CompressReader(context.Background(), bytes.NewReader(input), uint64(len(input)), &direct, newFakeHardware(), opts, nil)
	if err != nil {
		t.Fatalf("CompressReader: %v", err)
	}
	if result.InputBytes != uint64(len(input)) || result.OutputBytes != uint64(direct.Len()) {
		t.Fatalf("unexpected result: %+v, output=%d", result, direct.Len())
	}
	assertGzipData(t, direct.Bytes(), input)

	var buffered bytes.Buffer
	writer := NewWriter(&buffered, newFakeHardware())
	if _, err := writer.Write(input[:len(input)/2]); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := writer.Write(input[len(input)/2:]); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}
	if _, err := writer.Result(); err != nil {
		t.Fatalf("Writer.Result: %v", err)
	}
	assertGzipData(t, buffered.Bytes(), input)
}

func TestCompressReaderRejectsInvalidFPGAOutput(t *testing.T) {
	input := bytes.Repeat([]byte("input"), 1024)
	var output bytes.Buffer
	_, err := CompressReader(context.Background(), bytes.NewReader(input), uint64(len(input)), &output, newInvalidFakeHardware(), DefaultOptions(), nil)
	if err == nil {
		t.Fatal("CompressReader accepted invalid FPGA output")
	}
	if output.Len() != 0 {
		t.Fatalf("invalid FPGA output leaked to destination: %d bytes", output.Len())
	}
}

func assertGzipData(t *testing.T, compressed, want []byte) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip data: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close gzip reader: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decompressed data mismatch: got=%d want=%d", len(got), len(want))
	}
}

type fakeHardware struct {
	mu       sync.Mutex
	expected uint64
	invalid  bool
	input    []byte
	output   []byte
	read     int
	request  uint32
	status   uint32
}

func newFakeHardware() *fakeHardware { return &fakeHardware{} }

func newInvalidFakeHardware() *fakeHardware { return &fakeHardware{invalid: true} }

func (f *fakeHardware) WriteH2C(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uint64(len(f.input)+len(p)) > f.expected {
		return 0, fmt.Errorf("input exceeds expected size")
	}
	f.input = append(f.input, p...)
	if uint64(len(f.input)) == f.expected {
		if f.invalid {
			f.output = bytes.Repeat([]byte("0"), 32)
		} else {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(f.input); err != nil {
				return 0, err
			}
			if err := writer.Close(); err != nil {
				return 0, err
			}
			f.output = compressed.Bytes()
		}
		f.status = 1 << 1
	}
	return len(p), nil
}

func (f *fakeHardware) ReadC2H(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uint32(len(p)) != f.request {
		return 0, fmt.Errorf("unexpected C2H request length")
	}
	if f.read+len(p) > len(f.output) {
		return 0, io.ErrUnexpectedEOF
	}
	copy(p, f.output[f.read:])
	f.read += len(p)
	f.request = 0
	return len(p), nil
}

func (f *fakeHardware) CloseStreams() error { return nil }

func (f *fakeHardware) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Snapshot{Status: f.status, InBytes: uint64(len(f.input)), OutBytes: uint64(len(f.output))}, nil
}

func (f *fakeHardware) TransferSnapshot(context.Context) (Snapshot, error) {
	return f.Snapshot(context.Background())
}

func (f *fakeHardware) SoftReset(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.input = nil
	f.output = nil
	f.read = 0
	f.request = 0
	f.status = 0
	return nil
}

func (f *fakeHardware) Start(ctx context.Context, inputSize uint64, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expected = inputSize
	f.input = nil
	f.output = nil
	f.read = 0
	f.request = 0
	f.status = 1
	return nil
}

func (f *fakeHardware) StartC2HRequest(ctx context.Context, requestLen uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.request = requestLen
	return nil
}
