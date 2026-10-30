// Package client is a small HTTP client for fpga-gzipd. It supports both
// Unix-domain and TCP endpoints and streams request/response bodies without
// buffering the compressed object in memory.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client talks to one fpga-gzipd endpoint.
type Client struct {
	baseURL string
	http    *http.Client
}

// Result describes one completed service request.
type Result struct {
	InputBytes  int64
	OutputBytes int64
}

// NewTCP creates a client for a daemon listening on host:port.
func NewTCP(addr string) (*Client, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("TCP address is empty")
	}
	return NewHTTP("http://"+addr, nil)
}

// NewUnix creates a client using a Unix-domain socket.
func NewUnix(path string) (*Client, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("Unix socket path is empty")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", path)
		},
	}
	return &Client{
		baseURL: "http://fpga-gzipd",
		http:    &http.Client{Transport: transport},
	}, nil
}

// NewHTTP creates a client from an HTTP base URL. If hc is nil, a client with
// a default transport is used. This is useful for tests and for callers that
// already configure TLS, proxies or connection pooling.
func NewHTTP(baseURL string, hc *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("HTTP base URL is empty")
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{baseURL: baseURL, http: hc}, nil
}

// CloseIdleConnections releases pooled TCP or Unix transport connections.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.http == nil {
		return
	}
	if closer, ok := c.http.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Health checks the daemon's readiness endpoint.
func (c *Client) Health(ctx context.Context) error {
	if c == nil || c.http == nil {
		return errors.New("client is nil")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fpga-gzipd health: HTTP %s", resp.Status)
	}
	return nil
}

// Compress streams exactly inputSize bytes from src to fpga-gzipd and copies
// the gzip response to dst. The caller must provide the complete input size;
// a non-rewindable reader is supported.
func (c *Client) Compress(ctx context.Context, src io.Reader, inputSize int64, dst io.Writer) (Result, error) {
	if c == nil || c.http == nil {
		return Result{}, errors.New("client is nil")
	}
	if src == nil || dst == nil {
		return Result{}, errors.New("source and destination are required")
	}
	if inputSize < 1 {
		return Result{}, errors.New("input size must be greater than zero")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/compress", src)
	if err != nil {
		return Result{}, err
	}
	req.ContentLength = inputSize
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("fpga-gzipd compress: HTTP %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	written, err := io.Copy(dst, resp.Body)
	if err != nil {
		return Result{}, err
	}
	return Result{InputBytes: inputSize, OutputBytes: written}, nil
}
