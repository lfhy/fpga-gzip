// Command hls-vectors reproduces the random inputs that exposed the Zstd
// sequence lookahead deadlock on hardware. It never opens an FPGA device.
package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hls-vectors <new-output-directory>")
		os.Exit(2)
	}
	if err := os.Mkdir(os.Args[1], 0700); err != nil {
		panic(err)
	}
	sizes := []int{1, 7, 8, 9, 64, 65, 71, 72, 73, 127, 128, 129, 255, 1023, 8191, 8192, 8193, 32767, 32768, 32769, 65536, 65537, 1048576}
	for core := 0; core < 2; core++ {
		rng := rand.New(rand.NewSource(int64(20261101 + core)))
		for _, size := range sizes {
			raw := make([]byte, size)
			_, _ = rng.Read(raw)
			raw[0] = byte(core + 1)
			if !(core == 0 && (size == 32767 || size == 1048576) || core == 1 && size == 65536) {
				continue
			}
			name := filepath.Join(os.Args[1], fmt.Sprintf("case-2-%d.raw", size))
			if err := os.WriteFile(name, raw, 0600); err != nil {
				panic(err)
			}
		}
	}
}
