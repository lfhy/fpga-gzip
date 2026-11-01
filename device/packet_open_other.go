//go:build !linux

package device

import (
	"errors"
	"os"
)

func openPacketC2H(string) (*os.File, error) {
	return nil, errors.New("XDMA packet EOP requires Linux")
}
