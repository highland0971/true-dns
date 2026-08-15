package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"truedns/internal/config"
	"truedns/internal/core"
)

func testEngine(t *testing.T) *core.Engine {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = []string{"127.0.0.1:0"}
	cfg.Mode = config.ModeFull
	cfg.Upstreams.DoH = []string{"http://127.0.0.1:9/dns-query"}
	cfg.Upstreams.System = []string{"127.0.0.1:9"}
	cfg.API.Enabled = false
	eng, err := core.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// setTestStateDir redirects state files (api-port) into a temp directory so
// tests never touch the real config directory. Works for root too, unlike a
// HOME override.
func setTestStateDir(t *testing.T) {
	t.Helper()
	t.Setenv("TRUEDNS_STATE_DIR", t.TempDir())
}

func newTestServer(t *testing.T, token string) *Server {
	t.Helper()
	setTestStateDir(t)
	s := New("127.0.0.1:0", token, testEngine(t), nil)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func do(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("X-TrueDNS-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthzAndStatus(t *testing.T) {
	s := newTestServer(t, "")
	base := "http://" + s.Addr()

	resp := do(t, http.MethodGet, base+"/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, base+"/api/v1/status", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Status string      `json:"status"`
		Engine core.Status `json:"engine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "running" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.Engine.Version == "" || len(body.Engine.DoHUpstreams) != 1 {
		t.Fatalf("unexpected engine status: %+v", body.Engine)
	}
}

func TestFlush(t *testing.T) {
	s := newTestServer(t, "")
	resp := do(t, http.MethodPost, "http://"+s.Addr()+"/api/v1/flush", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("flush = %d", resp.StatusCode)
	}
}

func TestTokenAuth(t *testing.T) {
	s := newTestServer(t, "secret")
	base := "http://" + s.Addr()

	resp := do(t, http.MethodGet, base+"/api/v1/status", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, base+"/api/v1/status", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}

	resp = do(t, http.MethodGet, base+"/api/v1/status?token=secret", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query-token status = %d", resp.StatusCode)
	}
}

func TestReloadDisabled(t *testing.T) {
	s := newTestServer(t, "")
	resp := do(t, http.MethodPost, "http://"+s.Addr()+"/api/v1/reload", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("reload = %d, want 501", resp.StatusCode)
	}
}

func TestShutdown(t *testing.T) {
	setTestStateDir(t)
	eng := testEngine(t)
	s := New("127.0.0.1:0", "", eng, nil)
	called := make(chan struct{}, 1)
	s.SetShutdown(func() { called <- struct{}{} })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	resp := do(t, http.MethodPost, "http://"+s.Addr()+"/api/v1/shutdown", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shutdown = %d", resp.StatusCode)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown callback was not invoked")
	}
}

func TestPortFallback(t *testing.T) {
	setTestStateDir(t)
	// Occupy a port and confirm the API falls back to another candidate and
	// persists it to the api-port file.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	busy := blocker.Addr().(*net.TCPAddr).Port

	eng := testEngine(t)
	s := New(fmt.Sprintf("127.0.0.1:%d", busy), "", eng, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("Start should fall back to another port, got: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	if s.BoundPort() == busy {
		t.Fatalf("bound the busy port %d", busy)
	}
	if _, err := ReadPortFile(); err != nil {
		t.Fatalf("port file not written: %v", err)
	}
	resp := do(t, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", s.BoundPort()), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz on fallback port = %d", resp.StatusCode)
	}
}
