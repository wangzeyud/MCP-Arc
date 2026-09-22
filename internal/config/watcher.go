package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// watchPollInterval is how often the background poller checks the config file's
// modification time. Polling (rather than fsnotify) keeps the dependency surface
// zero and behaves identically on Windows/macOS/Linux.
const watchPollInterval = 5 * time.Second

// Manager holds the active configuration and supports reloading it from disk at
// runtime (config hot-reload, v0.7). Reads are lock-free via atomic.Pointer; a
// background poller re-reads the file when its mtime changes. Components register
// OnReload callbacks to re-apply the new config to live state.
type Manager struct {
	path    string
	current atomic.Pointer[Config]

	mu        sync.Mutex
	listeners []func(*Config)
}

// NewManager wraps an in-memory config with no file watching. Used by tests and
// when no config file exists.
func NewManager(initial *Config) *Manager {
	m := &Manager{}
	if initial == nil {
		initial = Default()
	}
	m.current.Store(initial)
	return m
}

// NewWatcherFromFile wraps a config loaded from path and watches that file for
// changes. The supplied initial config is used until the first successful reload;
// CLI overrides applied to it are dropped on the next file change (the file is
// the durable source of truth).
func NewWatcherFromFile(path string, initial *Config) *Manager {
	m := &Manager{path: path}
	if initial == nil {
		initial = Default()
	}
	m.current.Store(initial)
	return m
}

// Get returns the current active configuration. Never nil.
func (m *Manager) Get() *Config { return m.current.Load() }

// OnReload registers a callback invoked (synchronously) after a successful Reload
// with the new config. Keep callbacks cheap: they run on the poller / API
// goroutine.
func (m *Manager) OnReload(fn func(*Config)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, fn)
	m.mu.Unlock()
}

// Reload re-reads the config file, re-applies defaults and DSN anchoring, and —
// if it parses — atomically swaps in the new config and notifies listeners. A
// parse error leaves the old config in place and is returned to the caller.
func (m *Manager) Reload() error {
	if m.path == "" {
		return nil // in-memory only; nothing to reload
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("reload read %s: %w", m.path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("reload parse %s: %w", m.path, err)
	}
	applyDefaults(&c)
	if abs, err := filepath.Abs(m.path); err == nil {
		c.ConfigDir = filepath.Dir(abs)
	}
	anchorAuditDSN(&c)
	m.current.Store(&c)
	m.mu.Lock()
	listeners := append([]func(*Config){}, m.listeners...)
	m.mu.Unlock()
	for _, fn := range listeners {
		fn(&c)
	}
	return nil
}

// Start launches the background file poller. It returns immediately; the poller
// stops when ctx is cancelled (tied to process shutdown).
func (m *Manager) Start(ctx context.Context) {
	if m.path == "" {
		return
	}
	go func() {
		var lastMod time.Time
		if fi, err := os.Stat(m.path); err == nil {
			lastMod = fi.ModTime()
		}
		ticker := time.NewTicker(watchPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fi, err := os.Stat(m.path)
				if err != nil {
					continue
				}
				if !fi.ModTime().Equal(lastMod) {
					lastMod = fi.ModTime()
					if err := m.Reload(); err != nil {
						log.Printf("warn: config reload failed: %v", err)
					} else {
						log.Printf("mcp-arc: config reloaded from %s", m.path)
					}
				}
			}
		}
	}()
}

// RestartFingerprint returns a stable hash over the fields that cannot be
// hot-reloaded and therefore require a process restart when they change. Used to
// warn (and later to alert) when such a field differs after a reload.
func RestartFingerprint(c *Config) string {
	h := sha256.New()
	fmt.Fprintf(h, "%+v|%+v|%+v|%s|%s|%s|%d|%t",
		c.Transport, c.Server.Upstream, c.Server.ClientID,
		c.Audit.Driver, c.Audit.DSN, c.Admin.Token, c.Admin.Port, c.Admin.Enabled,
	)
	return fmt.Sprintf("%x", h.Sum(nil))
}
