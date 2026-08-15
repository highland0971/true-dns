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
	table  *override.Table
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newOverrideManager(cfg *config.Config) *overrideManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &overrideManager{table: override.New(), ctx: ctx, cancel: cancel}
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
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		if err := m.table.LoadURLs(ctx, cfg.Override.URLs, cfg.Upstreams.ProxyURL, client); err != nil {
			if ctx.Err() != nil {
				return // cancelled during shutdown; not a real failure
			}
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
		case <-m.ctx.Done():
			return
		case <-t.C:
			fetch()
		}
	}
}

// shutdown cancels in-flight fetches and waits for the loop to finish.
func (m *overrideManager) shutdown() {
	m.cancel()
	m.wg.Wait()
}

func (m *overrideManager) lookup(name string) []net.IP {
	return m.table.Lookup(name)
}

func (m *overrideManager) meta() override.Meta {
	return m.table.Meta()
}
