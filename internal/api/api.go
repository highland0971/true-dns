// Package api exposes a small loopback-only HTTP control API. It is the
// integration point for the tray GUI and for the CLI's status/flush commands,
// keeping the engine itself free of any UI concerns.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"truedns/internal/core"
	"truedns/internal/paths"
)

// fallbackPorts are tried in order when the configured port cannot be bound
// (Windows reserves ranges, e.g. 5378 can be WSAEACCES). Port 0 (OS-assigned)
// is the last resort.
var fallbackPorts = []string{"5378", "15378", "25378", "35378", "45378"}

// PortFile returns the path where the actually bound API port is recorded so
// other processes (tray GUI, CLI status/flush) can discover it.
func PortFile() string { return filepath.Join(paths.StateDir(), "api-port") }

// WritePortFile persists the bound port for discovery by other processes.
func WritePortFile(port int) error {
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(PortFile(), []byte(strconv.Itoa(port)+"\n"), 0o644)
}

// ReadPortFile returns the persisted API port, if any.
func ReadPortFile() (int, error) {
	data, err := os.ReadFile(PortFile())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// Server is the control API server.
type Server struct {
	addr       string
	token      string
	httpSrv    *http.Server
	ln         net.Listener
	boundPort  int
	engine     *core.Engine
	reloadFn   func() error
	shutdownFn func()
	extraFn    func() map[string]any
}

// New creates the API server. reloadFn re-reads the config file when the
// /reload endpoint is hit (nil disables the endpoint's reload capability).
func New(addr, token string, eng *core.Engine, reloadFn func() error) *Server {
	s := &Server{addr: addr, token: token, engine: eng, reloadFn: reloadFn}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /api/v1/flush", s.auth(s.handleFlush))
	mux.HandleFunc("POST /api/v1/reload", s.auth(s.handleReload))
	mux.HandleFunc("POST /api/v1/shutdown", s.auth(s.handleShutdown))
	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// SetExtraStatus registers a callback whose result is merged into the
// /status payload under the "takeover" key (used by the CLI to expose the
// system DNS takeover state).
func (s *Server) SetExtraStatus(fn func() map[string]any) { s.extraFn = fn }

// SetShutdown registers the callback invoked by POST /api/v1/shutdown (used
// by the tray GUI to stop the proxy; the run process then restores DNS).
func (s *Server) SetShutdown(fn func()) { s.shutdownFn = fn }

// Start binds and serves the API until Shutdown. When the configured address
// cannot be bound (reserved port, conflict), it falls back through a fixed
// candidate list and finally to an OS-assigned port, persisting the chosen
// port to the api-port file.
func (s *Server) Start() error {
	host := s.addr
	port := ""
	if h, p, err := net.SplitHostPort(s.addr); err == nil {
		host, port = h, p
	}
	candidates := []string{port}
	for _, c := range fallbackPorts {
		if c != port {
			candidates = append(candidates, c)
		}
	}
	candidates = append(candidates, "0")
	var lastErr error
	for _, p := range candidates {
		addr := net.JoinHostPort(host, p)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			lastErr = fmt.Errorf("api listen %s: %w", addr, err)
			continue
		}
		s.ln = ln
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			s.boundPort = tcp.Port
		}
		go func() { _ = s.httpSrv.Serve(ln) }() // Serve returns ErrServerClosed after Shutdown
		if err := WritePortFile(s.boundPort); err != nil {
			slog.Warn("could not persist api port file", "err", err)
		}
		slog.Info("control API listening", "addr", ln.Addr().String(), "port_file", PortFile())
		return nil
	}
	return fmt.Errorf("control API could not bind any port: %w", lastErr)
}

// BoundPort returns the port the API actually bound (0 when not started).
func (s *Server) BoundPort() int { return s.boundPort }

// Addr returns the bound address (useful when port 0 was requested).
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Shutdown stops the API server.
func (s *Server) Shutdown(ctx context.Context) error { return s.httpSrv.Shutdown(ctx) }

// auth guards endpoints with the configured token. An empty token disables
// authentication; the API only listens on loopback by default.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" &&
			r.Header.Get("X-TrueDNS-Token") != s.token &&
			r.URL.Query().Get("token") != s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"status": "running",
		"engine": s.engine.Status(),
	}
	if s.extraFn != nil {
		body["takeover"] = s.extraFn()
	}
	writeJSON(w, body)
}

func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	s.engine.FlushCache()
	writeJSON(w, map[string]any{"flushed": true})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.reloadFn == nil {
		http.Error(w, "reload is not available", http.StatusNotImplemented)
		return
	}
	if err := s.reloadFn(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"reloaded": true})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.shutdownFn == nil {
		http.Error(w, "shutdown is not available", http.StatusNotImplemented)
		return
	}
	writeJSON(w, map[string]any{"stopping": true})
	go s.shutdownFn()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
