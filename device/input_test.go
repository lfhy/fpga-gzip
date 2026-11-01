package device

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestReadOnlyInputRetriesBeforeDMAAndOwnsCopy(t *testing.T) {
	var input dmaInput
	raw := []byte("immutable mmap input")
	calls := 0
	write := func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			if &p[0] != &raw[0] {
				t.Fatal("fast path copied writable input")
			}
			return 0, fmt.Errorf("pin user pages: %w", syscall.EFAULT)
		}
		if &p[0] == &raw[0] || !bytes.Equal(p, raw) {
			t.Fatal("retry must own an equal writable copy")
		}
		return len(p), nil
	}
	for i := 0; i < 3; i++ {
		if n, err := input.write(raw, write); err != nil || n != len(raw) {
			t.Fatal(n, err)
		}
	}
	if calls != 4 {
		t.Fatal("retried known read-only source or dropped a write", calls)
	}
}

func TestInputDoesNotRetryAfterPartialDMAOrOtherError(t *testing.T) {
	for _, test := range []struct {
		n   int
		err error
	}{{0, syscall.EIO}, {1, syscall.EFAULT}, {2, nil}} {
		var input dmaInput
		calls := 0
		n, err := input.write([]byte("input"), func([]byte) (int, error) {
			calls++
			return test.n, test.err
		})
		if calls != 1 || n != test.n || !errors.Is(err, test.err) {
			t.Fatal("unsafe retry", calls, n, err)
		}
	}
}
