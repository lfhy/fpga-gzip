package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPlanChunksUsesAvailableCores(t *testing.T) {
	plan, err := planChunks(100, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if plan.count != 4 || plan.bytes != 25 {
		t.Fatalf("plan = %+v, want 4 chunks of 25 bytes", plan)
	}
}

func TestPlanChunksBoundsMemberSizeAndCoversInput(t *testing.T) {
	plan, err := planChunks(130, 2, 64)
	if err != nil {
		t.Fatal(err)
	}
	if plan.count != 3 || plan.bytes != 44 {
		t.Fatalf("plan = %+v, want 3 chunks of 44 bytes", plan)
	}
	var covered uint64
	for index := 0; index < plan.count; index++ {
		if index > 0 && plan.offset(index) != int64(covered) {
			t.Fatalf("chunk %d starts at %d, want %d", index, plan.offset(index), covered)
		}
		covered += plan.length(index)
	}
	if covered != plan.size {
		t.Fatalf("covered %d bytes, want %d", covered, plan.size)
	}
}

func TestPlanChunksDoesNotInventEmptyChunks(t *testing.T) {
	plan, err := planChunks(2, 4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if plan.count != 2 || plan.bytes != 1 {
		t.Fatalf("plan = %+v, want two one-byte chunks", plan)
	}
}

func TestPlanChunksRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		size  uint64
		cores int
		bytes uint64
	}{
		{size: 0, cores: 1, bytes: 1},
		{size: 1, cores: 0, bytes: 1},
		{size: 1, cores: 5, bytes: 1},
		{size: 1, cores: 1, bytes: 0},
	} {
		if _, err := planChunks(test.size, test.cores, test.bytes); err == nil {
			t.Fatalf("accepted invalid plan: %+v", test)
		}
	}
}

func TestAppendFileCreatesValidConcatenatedGzip(t *testing.T) {
	dir := t.TempDir()
	input := bytes.Repeat([]byte("chunked FPGA gzip member\n"), 1000)
	parts := [][]byte{input[:100], input[100:13000], input[13000:]}
	var paths []string
	for index, part := range parts {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(part); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("part-%d.gz", index))
		if err := os.WriteFile(path, compressed.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}

	outputPath := filepath.Join(dir, "output.gz")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for index, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		n, err := appendFile(output, path, uint64(info.Size()))
		if err != nil || n != uint64(info.Size()) {
			t.Fatalf("append part %d: n=%d err=%v", index, n, err)
		}
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, input) {
		t.Fatal("concatenated gzip payload differs from input")
	}
}
