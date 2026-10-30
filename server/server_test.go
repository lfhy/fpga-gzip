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

func TestWithAuthProtectsCompressionAndMetrics(t *testing.T) {
	svc, err := New(Config{
		HardwareFactory: func(context.Context) (pipeline.Hardware, error) {
			return nil, errors.New("test hardware unavailable")
		},
	}, WithAuth("secret"))
	if err != nil {
		t.Fatal(err)
	}

	probe := httptest.NewRecorder()
	svc.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", probe.Code, http.StatusOK)
	}

	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/compress", strings.NewReader("abc"))
	request.ContentLength = 3
	svc.Handler().ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	wrong := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/compress", strings.NewReader("abc"))
	request.ContentLength = 3
	request.Header.Set("Authorization", "Bearer wrong")
	svc.Handler().ServeHTTP(wrong, request)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want %d", wrong.Code, http.StatusUnauthorized)
	}

	metrics := httptest.NewRecorder()
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("Authorization", "Bearer secret")
	svc.Handler().ServeHTTP(metrics, metricsRequest)
	if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "fpga_gzip_failures_total 2\n") {
		t.Fatalf("authorized metrics = %d %q", metrics.Code, metrics.Body.String())
	}
}

func TestWithAuthRejectsEmptyToken(t *testing.T) {
	_, err := New(Config{
		HardwareFactory: func(context.Context) (pipeline.Hardware, error) { return nil, nil },
	}, WithAuth("  "))
	if err == nil {
		t.Fatal("empty auth token was accepted")
	}
}
