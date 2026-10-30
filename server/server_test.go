package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lfhy/fpga-gzip/pipeline"
)

func TestHealthMetricsAndContentLength(t *testing.T) {
	svc, err := New(Config{
		HardwareFactory: func(context.Context) (pipeline.Hardware, error) {
			return nil, errors.New("test hardware unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	svc.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("health = %d %q", health.Code, health.Body.String())
	}

	unknownLength := httptest.NewRequest(http.MethodPost, "/compress", io.NopCloser(strings.NewReader("abc")))
	unknownLength.ContentLength = -1
	lengthResponse := httptest.NewRecorder()
	svc.Handler().ServeHTTP(lengthResponse, unknownLength)
	if lengthResponse.Code != http.StatusLengthRequired {
		t.Fatalf("unknown length status = %d, want %d", lengthResponse.Code, http.StatusLengthRequired)
	}

	knownLength := httptest.NewRequest(http.MethodPost, "/compress", strings.NewReader("abc"))
	knownLength.ContentLength = 3
	failed := httptest.NewRecorder()
	svc.Handler().ServeHTTP(failed, knownLength)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("factory error status = %d, want %d", failed.Code, http.StatusServiceUnavailable)
	}

	metrics := httptest.NewRecorder()
	svc.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		"fpga_gzip_requests_total 2\n",
		"fpga_gzip_failures_total 2\n",
		"fpga_gzip_active_requests 0\n",
	} {
		if !strings.Contains(metrics.Body.String(), want) {
			t.Fatalf("metrics missing %q in %s", want, metrics.Body.String())
		}
	}
}

func TestNewRejectsInvalidQueue(t *testing.T) {
	_, err := New(Config{
		HardwareFactory: func(context.Context) (pipeline.Hardware, error) { return nil, nil },
		QueueDepth:      -1,
	})
	if err == nil {
		t.Fatal("negative queue depth was accepted")
	}
}
