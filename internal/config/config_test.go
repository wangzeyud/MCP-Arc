package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadSetsConfigDir verifies that Load records the directory of the loaded
// config file in ConfigDir. The proxy uses this to anchor relative paths in
// server.upstream, so a single binary + config.yaml works regardless of the
// CWD the MCP client spawned mcp-arc with.
func TestLoadSetsConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "server:\n  upstream: [\"node\", \"examples/echo-server/server.js\"]\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.ConfigDir != dir {
		t.Fatalf("ConfigDir = %q, want %q", cfg.ConfigDir, dir)
	}
}
