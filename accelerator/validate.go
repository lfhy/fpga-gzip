package accelerator

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minlz"
	"github.com/pierrec/lz4/v4"
)

type validator interface {
	io.Writer
	Validate(uint32, uint64) error
	Close() error
}

type rawValidator struct {
	output io.Writer
	crc    hash.Hash32
	size   uint64
}

func (v *rawValidator) Write(p []byte) (int, error) {
	n, err := v.output.Write(p)
	if n > 0 && n <= len(p) {
		v.crc.Write(p[:n])
		v.size += uint64(n)
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (v *rawValidator) Validate(crc uint32, size uint64) error {
	return compare(v.crc.Sum32(), v.size, crc, size)
}
func (*rawValidator) Close() error { return nil }

type decodedResult struct {
	crc  uint32
	size uint64
	err  error
}
type decodedValidator struct {
	output  io.Writer
	pipe    *io.PipeWriter
	done    chan decodedResult
	result  *decodedResult
	prefix  []byte
	magic   [4]byte
	checked bool
}

func newValidator(algorithm Algorithm, output io.Writer, size uint64) (validator, error) {
	if output == nil {
		return nil, errors.New("output writer is nil")
	}
	if size == 0 || size >= math.MaxInt64 {
		return nil, errors.New("input size must be between 1 and MaxInt64-1")
	}
	if algorithm == Raw {
		return &rawValidator{output: output, crc: crc32.NewIEEE()}, nil
	}
	if algorithm != LZ4 && algorithm != Zstd && algorithm != MinLZ {
		return nil, fmt.Errorf("unsupported FPGA algorithm %d", algorithm)
	}
	r, w := io.Pipe()
	v := &decodedValidator{output: output, pipe: w, done: make(chan decodedResult, 1)}
	if algorithm == LZ4 {
		v.magic = [4]byte{4, 0x22, 0x4d, 0x18}
	} else if algorithm == MinLZ {
		v.magic = [4]byte{0xff, 6, 0, 0}
	} else {
		v.magic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}
	}
	go func() {
		defer r.Close()
		var decoded io.Reader
		var cleanup func()
		var err error
		if algorithm == LZ4 {
			decoded = lz4.NewReader(r)
		} else if algorithm == MinLZ {
			decoded = minlz.NewReader(r, minlz.ReaderMaxBlockSize(8<<20))
		} else {
			var z *zstd.Decoder
			z, err = zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(128<<20))
			if err == nil {
				decoded, cleanup = z, z.Close
			}
		}
		if cleanup != nil {
			defer cleanup()
		}
		crc := crc32.NewIEEE()
		var n int64
		if err == nil {
			n, err = io.Copy(crc, io.LimitReader(decoded, int64(size)+1))
		}
		if err == nil && uint64(n) != size {
			err = fmt.Errorf("FPGA %s decoded size %d, expected %d", algorithm, n, size)
		}
		if err != nil {
			_ = r.CloseWithError(err)
		}
		v.done <- decodedResult{crc: crc.Sum32(), size: uint64(n), err: err}
	}()
	return v, nil
}

func (v *decodedValidator) Write(p []byte) (int, error) {
	if !v.checked {
		take := 4 - len(v.prefix)
		if take > len(p) {
			take = len(p)
		}
		v.prefix = append(v.prefix, p[:take]...)
		if len(v.prefix) < 4 {
			return len(p), nil
		}
		if !bytes.Equal(v.prefix, v.magic[:]) {
			return 0, errors.New("FPGA output does not match the selected frame format")
		}
		v.checked = true
		if err := v.write(v.prefix); err != nil {
			return take, err
		}
		if err := v.write(p[take:]); err != nil {
			return take, err
		}
		return len(p), nil
	}
	if err := v.write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (v *decodedValidator) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if _, err := v.pipe.Write(p); err != nil {
		return err
	}
	n, err := v.output.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return err
}

func (v *decodedValidator) wait() decodedResult {
	if v.result == nil {
		result := <-v.done
		v.result = &result
	}
	return *v.result
}

func (v *decodedValidator) Validate(crc uint32, size uint64) error {
	_ = v.pipe.Close()
	r := v.wait()
	if r.err != nil {
		return fmt.Errorf("verify FPGA output: %w", r.err)
	}
	return compare(r.crc, r.size, crc, size)
}

func (v *decodedValidator) Close() error {
	_ = v.pipe.CloseWithError(io.ErrClosedPipe)
	v.wait()
	return nil
}

func compare(gotCRC uint32, gotSize uint64, wantCRC uint32, wantSize uint64) error {
	if gotSize != wantSize || gotCRC != wantCRC {
		return fmt.Errorf("FPGA decoded data mismatch: size=%d/%d CRC32=%08x/%08x", gotSize, wantSize, gotCRC, wantCRC)
	}
	return nil
}
