package accelerator

import (
	"bytes"
	"hash/crc32"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minlz"
	"github.com/pierrec/lz4/v4"
)

func encodeTest(t *testing.T, a Algorithm, raw []byte) []byte {
	t.Helper()
	if a == Raw {
		return raw
	}
	var output bytes.Buffer
	var w io.WriteCloser
	if a == LZ4 {
		w = lz4.NewWriter(&output)
	} else if a == MinLZ {
		w = minlz.NewWriter(&output, minlz.WriterConcurrency(1), minlz.WriterBlockSize(16<<10))
	} else {
		var err error
		w, err = zstd.NewWriter(&output, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestValidatorsCompareDecodedInputAndAbort(t *testing.T) {
	raw := bytes.Repeat([]byte("multialgorithm compression bytes!"), 1000)
	for _, a := range []Algorithm{Raw, LZ4, Zstd, MinLZ} {
		t.Run(a.String(), func(t *testing.T) {
			encoded := encodeTest(t, a, raw)
			var output bytes.Buffer
			v, err := newValidator(a, &output, uint64(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			for _, p := range [][]byte{encoded[:1], encoded[1:3], encoded[3:]} {
				if _, err := v.Write(p); err != nil {
					t.Fatal(err)
				}
			}
			if err := v.Validate(crc32.ChecksumIEEE(raw), uint64(len(raw))); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output.Bytes(), encoded) {
				t.Fatal("validator changed stream")
			}
			if err := v.Validate(crc32.ChecksumIEEE(raw)^1, uint64(len(raw))); err == nil {
				t.Fatal("accepted wrong input CRC")
			}
			abort, err := newValidator(a, io.Discard, uint64(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if err := abort.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMinLZRejectsCorruptChecksumAndTruncatedEOF(t *testing.T) {
	raw := bytes.Repeat([]byte("independently checked MinLZ frame\n"), 1000)
	valid := encodeTest(t, MinLZ, raw)
	corrupt := bytes.Clone(valid)
	corrupt[14] ^= 1 // First content chunk's CRC32C, after the 10-byte identifier.
	for name, frame := range map[string][]byte{"checksum": corrupt, "truncated-eof": valid[:len(valid)-2]} {
		t.Run(name, func(t *testing.T) {
			v, err := newValidator(MinLZ, io.Discard, uint64(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if _, err = v.Write(frame); err == nil {
				err = v.Validate(crc32.ChecksumIEEE(raw), uint64(len(raw)))
			}
			if err == nil {
				t.Fatal("accepted corrupt or incomplete MinLZ stream")
			}
		})
	}
}
