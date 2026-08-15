// Package api exposes a small loopback-only HTTP control API. It is the
// integration point for the future tray GUI and for the CLI's status/flush
// commands, keeping the engine itself free of any UI concerns.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"truedns/internal/core"
)

// Server is the control API server.
type Server struct {
	addr     string
	token    string
	httpSrv  *http.Server
	ln       net.Listener
	engine   *core.Engine
	reloadFn func() error
	extraFn  func() map[string]any
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
	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// SetExtraStatus registers a callback whose result is merged into the
// /status payload under the "takeover" key (used by the CLI to expose the
// system DNS takeover state).
func (s *Server) SetExtraStatus(fn func() map[string]any) { s.extraFn = fn }

// Start binds and serves the API until Shutdown.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("api listen %s: %w", s.addr, err)
	}
	s.ln = ln
	go func() { _ = s.httpSrv.Serve(ln) }() // Serve returns ErrServerClosed after Shutdown
	return nil
}

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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
