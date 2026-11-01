package accelerator

import (
	"bytes"
	"hash/crc32"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
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
	for _, a := range []Algorithm{Raw, LZ4, Zstd} {
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
