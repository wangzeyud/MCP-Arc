package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleCfg = `
server:
  client_id: test
audit:
  enabled: true
  driver: memory
rate_limit:
  enabled: true
  qps: 5
  daily_quota: 100
admin:
  enabled: true
  port: 8080
  token: change-me
`

const sampleCfgChanged = `
server:
  client_id: test
audit:
  enabled: true
  driver: memory
rate_limit:
  enabled: true
  qps: 50
  daily_quota: 100
admin:
  enabled: true
  port: 8080
  token: change-me
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

func TestManagerReloadFromFile(t *testing.T) {
	p := writeTempConfig(t, sampleCfg)
	loaded, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := NewWatcherFromFile(p, loaded)
	if got := m.Get().RateLimit.QPS; got != 5 {
		t.Fatalf("initial qps = %v, want 5", got)
	}

	fired := false
	m.OnReload(func(c *Config) { fired = true })

	if err := os.WriteFile(p, []byte(sampleCfgChanged), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m.Get().RateLimit.QPS; got != 50 {
		t.Fatalf("reloaded qps = %v, want 50", got)
	}
	if !fired {
		t.Fatal("OnReload callback was not fired")
	}
}

func TestManagerInMemoryReloadNoop(t *testing.T) {
	m := NewManager(nil)
	if err := m.Reload(); err != nil {
		t.Fatalf("in-memory reload should be a no-op, got %v", err)
	}
}

func TestRestartFingerprint(t *testing.T) {
	a := Default()
	b := Default()
	if RestartFingerprint(a) != RestartFingerprint(b) {
		t.Fatal("identical configs should hash to the same fingerprint")
	}
	b.Transport.Client = "sse"
	if RestartFingerprint(a) == RestartFingerprint(b) {
		t.Fatal("changing a restart-required field must change the fingerprint")
	}
}
