// Command verify-hls decodes every standard frame emitted by the C simulation
// with independent pure Go code and compares every byte with the input.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func main() {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: verify-hls lz4|zstd <csim-output-directory>")
		os.Exit(2)
	}
	paths, err := filepath.Glob(filepath.Join(os.Args[2], "case-*.raw"))
	want := 115
	if len(os.Args) == 4 {
		switch os.Args[3] {
		case "quick":
			want = 25
		case "random":
			want = 3
		case "entropy":
			want = 5
		default:
			panic("unknown case selection")
		}
	}
	if err != nil || len(paths) != want {
		panic(fmt.Sprintf("expected %d cases, got %d: %v", want, len(paths), err))
	}
	failed := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		data, err := os.ReadFile(strings.TrimSuffix(path, ".raw") + ".encoded")
		if err != nil {
			panic(err)
		}
		var r io.Reader
		var close func()
		switch os.Args[1] {
		case "lz4":
			r = lz4.NewReader(bytes.NewReader(data))
		case "zstd":
			d, e := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
			err = e
			if err == nil {
				r, close = d, d.Close
			}
		default:
			panic("unknown algorithm")
		}
		var decoded []byte
		if err == nil {
			decoded, err = io.ReadAll(io.LimitReader(r, int64(len(raw))+1))
		}
		if close != nil {
			close()
		}
		if err != nil || !bytes.Equal(raw, decoded) {
			failed++
			fmt.Printf("FAIL %s input=%d encoded=%d decoded=%d error=%v\n", filepath.Base(path), len(raw), len(data), len(decoded), err)
		}
	}
	fmt.Printf("%s: %d/%d fully decoded and byte-compared\n", os.Args[1], len(paths)-failed, len(paths))
	if failed != 0 {
		os.Exit(1)
	}
}
