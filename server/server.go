// Package server exposes the FPGA gzip pipeline as a small HTTP service.
//
// The service is intentionally a separate process boundary from FUSE or an
// application using the SDK. A request supplies Content-Length because the
// FPGA shell must know the complete input size before H2C starts. The response
// body is a standard gzip stream and is written as the FPGA produces it.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/lfhy/fpga-gzip/pipeline"
)

var (
	// ErrBusy means the bounded request queue is full.
	ErrBusy = errors.New("fpga-gzip service queue is full")
	// ErrContentLength means the client used chunked/unknown-length input.
	// The FPGA shell cannot start a transaction without an exact input length.
	ErrContentLength = errors.New("Content-Length is required")
)

// Config controls one service instance. HardwareFactory is called once for
// each admitted request and must return a fresh hardware handle. A Device's
// streaming descriptors are closed when a transaction ends, so sharing one
// handle across requests is not supported.
type Config struct {
	HardwareFactory func(context.Context) (pipeline.Hardware, error)
	Options         pipeline.Options

	// MaxConcurrent is normally one for a single XDMA H2C/C2H pair. It may be
	// raised when HardwareFactory selects independent FPGA streams.
	MaxConcurrent int
	// QueueDepth bounds requests waiting for a worker. A full queue returns
	// HTTP 503 instead of buffering request bodies in the daemon.
	QueueDepth int
	// MaxInputBytes limits request bodies. Zero disables the limit.
	MaxInputBytes int64
}

// Server is an HTTP handler and listener lifecycle for the compression daemon.
type Server struct {
	cfg Config

	admission chan struct{}
	workers   chan struct{}
	handler   http.Handler

	requests  atomic.Uint64
	successes atomic.Uint64
	failures  atomic.Uint64
	busy      atomic.Uint64
	input     atomic.Uint64
	output    atomic.Uint64
	active    atomic.Int64
}

// New validates config and creates a service. A zero Options value selects the
// SDK defaults; non-zero options are passed to pipeline.RunReader unchanged.
func New(cfg Config) (*Server, error) {
	if cfg.HardwareFactory == nil {
		return nil, errors.New("hardware factory is required")
	}
	if cfg.Options.RingSlots == 0 {
		cfg.Options = pipeline.DefaultOptions()
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.QueueDepth < 0 {
		return nil, errors.New("queue depth cannot be negative")
	}
	if cfg.MaxInputBytes < 0 {
		return nil, errors.New("max input bytes cannot be negative")
	}
	capacity := cfg.MaxConcurrent + cfg.QueueDepth
	if capacity < cfg.MaxConcurrent {
		return nil, errors.New("request queue size overflow")
	}

	s := &Server{
		cfg:       cfg,
		admission: make(chan struct{}, capacity),
		workers:   make(chan struct{}, cfg.MaxConcurrent),
	}
	s.handler = s.newHandler()
	return s, nil
}

// Handler returns the service's net/http handler. It can be mounted below a
// reverse proxy or used with httptest without opening a socket.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return s.handler
}

// Serve runs an HTTP server on l until ctx is canceled or the listener fails.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	if s == nil || s.handler == nil {
		return errors.New("server is nil")
	}
	if l == nil {
		return errors.New("listener is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	h := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = h.Shutdown(shutdownCtx)
			cancel()
		case <-stop:
		}
	}()

	err := h.Serve(l)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return err
}

// ListenAndServeTCP listens on addr and serves HTTP until ctx is canceled.
func (s *Server) ListenAndServeTCP(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, l)
}

// ListenAndServeUnix listens on a Unix-domain socket. A stale socket left by
// a crashed daemon is removed; an active listener is never replaced.
func (s *Server) ListenAndServeUnix(ctx context.Context, path string, mode os.FileMode) error {
	if path == "" {
		return errors.New("unix socket path is empty")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("unix socket path %q exists and is not a socket", path)
		}
		probe, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			return fmt.Errorf("unix socket %q is already in use", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale unix socket %q: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if mode != 0 {
		if err := os.Chmod(path, mode); err != nil {
			_ = l.Close()
			_ = os.Remove(path)
			return err
		}
	}
	defer os.Remove(path)
	return s.Serve(ctx, l)
}

func (s *Server) newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/compress", s.handleCompress)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "fpga_gzip_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(w, "fpga_gzip_success_total %d\n", s.successes.Load())
	fmt.Fprintf(w, "fpga_gzip_failures_total %d\n", s.failures.Load())
	fmt.Fprintf(w, "fpga_gzip_busy_total %d\n", s.busy.Load())
	fmt.Fprintf(w, "fpga_gzip_active_requests %d\n", s.active.Load())
	fmt.Fprintf(w, "fpga_gzip_input_bytes_total %d\n", s.input.Load())
	fmt.Fprintf(w, "fpga_gzip_output_bytes_total %d\n", s.output.Load())
}

func (s *Server) handleCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.requests.Add(1)
	if r.ContentLength < 1 {
		s.failures.Add(1)
		writeServiceError(w, http.StatusLengthRequired, ErrContentLength)
		return
	}
	if s.cfg.MaxInputBytes > 0 && r.ContentLength > s.cfg.MaxInputBytes {
		s.failures.Add(1)
		writeServiceError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("input is %d bytes, limit is %d", r.ContentLength, s.cfg.MaxInputBytes))
		return
	}

	if !s.admit(r.Context()) {
		s.busy.Add(1)
		writeServiceError(w, http.StatusServiceUnavailable, ErrBusy)
		return
	}
	defer func() { <-s.admission }()
	select {
	case s.workers <- struct{}{}:
		defer func() { <-s.workers }()
	case <-r.Context().Done():
		s.failures.Add(1)
		return
	}
	s.active.Add(1)
	defer s.active.Add(-1)

	hw, err := s.cfg.HardwareFactory(r.Context())
	if err != nil {
		s.failures.Add(1)
		writeServiceError(w, http.StatusServiceUnavailable, err)
		return
	}
	if hw == nil {
		s.failures.Add(1)
		writeServiceError(w, http.StatusServiceUnavailable, errors.New("hardware factory returned nil"))
		return
	}
	if closer, ok := hw.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-FPGA-Gzip", "true")
	w.Header().Set("X-FPGA-Input-Bytes", fmt.Sprintf("%d", r.ContentLength))
	w.Header().Add("Trailer", "X-FPGA-Output-Bytes")
	tracked := &commitWriter{ResponseWriter: w}
	result, err := pipeline.RunReader(r.Context(), r.Body, uint64(r.ContentLength), tracked, hw, s.cfg.Options, nil)
	if err != nil {
		s.failures.Add(1)
		if !tracked.committed {
			writeServiceError(w, http.StatusBadGateway, err)
		}
		return
	}
	s.successes.Add(1)
	s.input.Add(result.InputBytes)
	s.output.Add(result.OutputBytes)
	// Trailer was declared before the first body write, so net/http will emit
	// this value after the gzip stream when the transport supports trailers.
	w.Header().Set("X-FPGA-Output-Bytes", fmt.Sprintf("%d", result.OutputBytes))
}

func (s *Server) admit(ctx context.Context) bool {
	select {
	case s.admission <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	default:
		return false
	}
}

type commitWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *commitWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
		w.committed = true
	}
	return w.ResponseWriter.Write(p)
}

func writeServiceError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error()+"\n", status)
}
