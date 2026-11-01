package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lfhy/fpga-gzip/firmware"
)

func TestMultiAlgorithmRequiresVerifiedCompletion(t *testing.T) {
	for _, complete := range []bool{false, true} {
		service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("algorithm") != "lz4" {
				t.Error("missing algorithm")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("X-FPGA-Algorithm", "lz4")
			w.Header().Add("Trailer", "X-FPGA-Complete")
			w.Header().Add("Trailer", "X-FPGA-Output-Bytes")
			_, _ = io.WriteString(w, "frame")
			w.Header().Set("X-FPGA-Output-Bytes", "5")
			if complete {
				w.Header().Set("X-FPGA-Complete", "1")
			}
		}))
		c, err := NewHTTP(service.URL, service.Client())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CompressAlgorithm(context.Background(), firmware.LZ4, strings.NewReader("abc"), 3, io.Discard)
		service.Close()
		if complete && err != nil {
			t.Fatal(err)
		}
		if !complete && err == nil {
			t.Fatal("accepted unverified partial response")
		}
	}
}
