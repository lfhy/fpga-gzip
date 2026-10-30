package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressStreamsAndSetsContentLength(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/compress" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.ContentLength != 3 {
			t.Fatalf("content length = %d, want 3", r.ContentLength)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "abc" {
			t.Fatalf("body = %q", body)
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write([]byte("gzip-data"))
	}))
	defer service.Close()

	c, err := NewHTTP(service.URL, service.Client())
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	result, err := c.Compress(context.Background(), strings.NewReader("abc"), 3, &output)
	if err != nil {
		t.Fatal(err)
	}
	if result.InputBytes != 3 || result.OutputBytes != int64(len("gzip-data")) || output.String() != "gzip-data" {
		t.Fatalf("result = %+v output = %q", result, output.String())
	}
}

func TestNewUnixRejectsEmptyPath(t *testing.T) {
	if _, err := NewUnix(""); err == nil {
		t.Fatal("empty Unix path was accepted")
	}
}

func TestWithAuthAddsBearerToken(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q, want Bearer secret", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer service.Close()

	c, err := NewHTTP(service.URL, service.Client(), WithAuth(" secret "))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWithAuthRejectsEmptyToken(t *testing.T) {
	if _, err := NewHTTP("http://localhost", nil, WithAuth("  ")); err == nil {
		t.Fatal("empty auth token was accepted")
	}
}
