package override

import (
	"net/http"
	"net/http/httptest"
	"time"
)

// hostsServer serves fixed hosts-format text over HTTP for LoadURLs tests.
type hostsServer struct {
	*httptest.Server
}

func newHostsServer(t testingT, body string) *hostsServer {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &hostsServer{Server: srv}
}

func (s *hostsServer) client() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

// testingT is the minimal *testing.T surface used by helpers.
type testingT interface {
	Cleanup(func())
}
