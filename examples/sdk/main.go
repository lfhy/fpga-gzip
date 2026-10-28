// Command sdk demonstrates using the public fpgagzip package from another
// Go program. It is intentionally a small file-to-file example; applications
// can supply their own progress callback or Hardware implementation.
package main

import (
	"context"
	"fmt"
	"os"

	fpgagzip "github.com/lfhy/fpga-gzip"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s input.bin output.gz\n", os.Args[0])
		os.Exit(2)
	}

	hw, err := fpgagzip.Open(fpgagzip.DevicePaths{
		User: "/dev/xdma0_user",
		H2C:  "/dev/xdma0_h2c_0",
		C2H:  "/dev/xdma0_c2h_0",
	})
	if err != nil {
		panic(err)
	}
	defer hw.Close()

	result, err := fpgagzip.Compress(
		context.Background(),
		os.Args[1], os.Args[2], hw,
		fpgagzip.DefaultOptions(), nil,
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("compressed %d bytes to %d bytes in %s\n",
		result.InputBytes, result.OutputBytes, result.Total)
}
