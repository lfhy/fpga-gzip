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
	"strconv"
	"strings"
	"time"

	"github.com/lfhy/fpga-gzip/firmware"
)

// Client talks to one fpga-gzipd endpoint.
type Client struct {
	baseURL   string
	http      *http.Client
	authToken string
}

// Option customizes a Client at construction time.
type Option func(*Client) error

// WithAuth configures an optional Bearer token for daemon requests. An empty
// token is rejected; omit this option to leave authentication disabled.
func WithAuth(token string) Option {
	return func(c *Client) error {
		token = strings.TrimSpace(token)
		if token == "" {
			return errors.New("auth token cannot be empty")
		}
		c.authToken = token
		return nil
	}
}

// Result describes one completed service request.
type Result struct {
	InputBytes  int64
	OutputBytes int64
	Algorithm   firmware.Algorithm
}

// NewTCP creates a client for a daemon listening on host:port.
func NewTCP(addr string, options ...Option) (*Client, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("TCP address is empty")
	}
	return NewHTTP("http://"+addr, nil, options...)
}

// NewUnix creates a client using a Unix-domain socket.
func NewUnix(path string, options ...Option) (*Client, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("Unix socket path is empty")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", path)
		},
	}
	c := &Client{
		baseURL: "http://fpga-gzipd",
		http:    &http.Client{Transport: transport},
	}
	if err := applyOptions(c, options); err != nil {
		return nil, err
	}
	return c, nil
}

// NewHTTP creates a client from an HTTP base URL. If hc is nil, a client with
// a default transport is used. This is useful for tests and for callers that
// already configure TLS, proxies or connection pooling.
func NewHTTP(baseURL string, hc *http.Client, options ...Option) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("HTTP base URL is empty")
	}
	if hc == nil {
		hc = &http.Client{}
	}
	c := &Client{baseURL: baseURL, http: hc}
	if err := applyOptions(c, options); err != nil {
		return nil, err
	}
	return c, nil
}

func applyOptions(c *Client, options []Option) error {
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(c); err != nil {
			return err
		}
	}
	return nil
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
	c.authorize(req)
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
	return c.CompressAlgorithm(ctx, firmware.Gzip, src, inputSize, dst)
}

// CompressAlgorithm checks image selection and the final verification trailer.
// A late DMA or integrity error cannot be mistaken for successful HTTP output.
func (c *Client) CompressAlgorithm(ctx context.Context, algorithm firmware.Algorithm, src io.Reader, inputSize int64, dst io.Writer) (Result, error) {
	if c == nil || c.http == nil {
		return Result{}, errors.New("client is nil")
	}
	if src == nil || dst == nil {
		return Result{}, errors.New("source and destination are required")
	}
	if inputSize < 1 {
		return Result{}, errors.New("input size must be greater than zero")
	}
	if algorithm < firmware.Raw || algorithm > firmware.MinLZ {
		return Result{}, errors.New("invalid algorithm")
	}
	endpoint := c.baseURL + "/compress"
	if algorithm != firmware.Gzip {
		endpoint += "?algorithm=" + algorithm.String()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, src)
	if err != nil {
		return Result{}, err
	}
	req.ContentLength = inputSize
	req.Header.Set("Content-Type", "application/octet-stream")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("fpga-gzipd compress: HTTP %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	loaded := resp.Header.Get("X-FPGA-Algorithm")
	if loaded != "" && loaded != algorithm.String() {
		return Result{}, fmt.Errorf("response algorithm %q differs from requested %s", loaded, algorithm)
	}
	if loaded == "" && algorithm != firmware.Gzip {
		return Result{}, errors.New("server did not identify the response algorithm")
	}
	written, err := io.Copy(dst, resp.Body)
	if err != nil {
		return Result{}, err
	}
	if loaded != "" {
		expected, err := strconv.ParseInt(resp.Trailer.Get("X-FPGA-Output-Bytes"), 10, 64)
		if resp.Trailer.Get("X-FPGA-Complete") != "1" || err != nil || expected != written {
			return Result{}, fmt.Errorf("FPGA response did not complete verification: %s", resp.Trailer.Get("X-FPGA-Error"))
		}
	}
	return Result{InputBytes: inputSize, OutputBytes: written, Algorithm: algorithm}, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
}
