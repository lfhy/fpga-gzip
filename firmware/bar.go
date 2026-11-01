package firmware

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

type barReader struct {
	file *os.File
	base uint32
}

func (r barReader) ReadReg32(ctx context.Context, offset uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var data [4]byte
	n, err := r.file.ReadAt(data[:], int64(r.base)+int64(offset))
	if err == nil && n != len(data) {
		err = io.ErrUnexpectedEOF
	}
	return binary.LittleEndian.Uint32(data[:]), err
}

// InspectBAR opens only the user BAR and performs read-only identity queries.
// The caller must hold a transaction or programming lock for the complete call.
func InspectBAR(ctx context.Context, user string, bank uint32) (Info, error) {
	if bank > 3 {
		return Info{}, fmt.Errorf("invalid control bank %d", bank)
	}
	f, err := os.Open(user)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	i, err := Inspect(ctx, barReader{file: f, base: bank * 0x1000})
	if err == nil && !i.Legacy && bank >= i.Cores {
		return Info{}, fmt.Errorf("%w: inactive core %d", ErrIncompatible, bank)
	}
	return i, err
}
