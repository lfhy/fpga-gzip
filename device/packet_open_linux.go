//go:build linux

package device

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openPacketC2H(path string) (*os.File, error) {
	// O_PATH pins the target inode without invoking the XDMA open callback.
	// Reopen that exact inode after fstat, avoiding O_TRUNC on a regular file
	// even if a symlink or pathname changes during capability negotiation.
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		return nil, fmt.Errorf("packet EOP requires an XDMA C2H character device: %s", path)
	}
	// The official XDMA driver selects eop_flush from O_TRUNC at open time.
	return os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", fd), os.O_RDONLY|os.O_TRUNC, 0)
}
