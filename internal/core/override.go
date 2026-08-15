package core

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"truedns/internal/config"
	"truedns/internal/override"
)

// overrideManager owns the hosts-format IP override table and its background
// refresh loop. It is swapped atomically on config reload like the other
// derived components.
type overrideManager struct {
	table *override.Table
	stop  chan struct{}
	wg    sync.WaitGroup
}

func newOverrideManager(cfg *config.Config) *overrideManager {
	m := &overrideManager{table: override.New(), stop: make(chan struct{})}
	if err := m.table.LoadFiles(cfg.Override.Files); err != nil {
		slog.Warn("override files partially failed to load", "err", err)
	}
	if len(cfg.Override.URLs) > 0 {
		m.wg.Add(1)
		go m.loop(cfg)
	}
	return m
}

func (m *overrideManager) loop(cfg *config.Config) {
	defer m.wg.Done()
	client := &http.Client{Timeout: 15 * time.Second}
	fetch := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := m.table.LoadURLs(ctx, cfg.Override.URLs, cfg.Upstreams.ProxyURL, client); err != nil {
			slog.Warn("override URL fetch failed", "err", err)
			return
		}
		slog.Info("override table loaded", "entries", m.table.Meta().Entries)
	}
	fetch()
	if cfg.Override.RefreshInterval <= 0 {
		return
	}
	t := time.NewTicker(cfg.Override.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			fetch()
		}
	}
}

// shutdown stops the refresh loop and waits for it to finish.
func (m *overrideManager) shutdown() {
	close(m.stop)
	m.wg.Wait()
}

func (m *overrideManager) lookup(name string) []net.IP {
	return m.table.Lookup(name)
}

func (m *overrideManager) meta() override.Meta {
	return m.table.Meta()
}
