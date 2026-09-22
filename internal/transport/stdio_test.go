package transport

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestStdioUpstreamRunsRelativeArgFromConfigDir proves that a *relative path*
// passed as an argument to the upstream command is resolved against the config
// directory (passed as dir), not the process CWD. This mirrors the real-world
// gateway config `upstream: ["node", "examples/echo-server/server.js"]`, where
// `node` is found via PATH and the relative script path must resolve from the
// config dir. It is what lets a single binary + config.yaml work no matter
// where the MCP client spawned mcp-arc from.
func TestStdioUpstreamRunsRelativeArgFromConfigDir(t *testing.T) {
	dir := t.TempDir()

	// Pick a shell that is on PATH (so the executable lookup itself is fine) and
	// a SEPARATE script, referenced by a *relative* path, that lives only inside
	// dir. With cmd.Dir = dir the relative script resolves; without it (the old
	// behaviour) the lookup against the client CWD fails.
	var shell, relArg string
	switch runtime.GOOS {
	case "windows":
		shell = "cmd"
		// A "."-prefixed relative arg so the shell resolves it against the
		// working directory (cmd.Dir) rather than searching PATH by name.
		relArg = `.\\probe.bat`
		if err := os.WriteFile(filepath.Join(dir, "probe.bat"), []byte("@echo ok\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		shell = "sh"
		relArg = "./probe.sh"
		if err := os.WriteFile(filepath.Join(dir, "probe.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Move the process CWD somewhere that does NOT contain the relative script,
	// mimicking an MCP client that spawns mcp-arc from an unrelated directory.
	prev, err := os.Getwd()
	if err == nil {
		t.Cleanup(func() { _ = os.Chdir(prev) })
	}
	clientCWD := t.TempDir()
	if err := os.Chdir(clientCWD); err != nil {
		t.Fatal(err)
	}

	args := []string{shell}
	if runtime.GOOS == "windows" {
		args = append(args, "/c")
	}
	args = append(args, relArg)

	up, err := NewStdioUpstream(args, dir)
	if err != nil {
		t.Fatalf("NewStdioUpstream = %v", err)
	}
	if up.cmd.Dir != dir {
		t.Fatalf("cmd.Dir = %q, want %q", up.cmd.Dir, dir)
	}

	got := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = up.Run(ctx, func(b []byte) {
			select {
			case got <- string(b):
			default:
			}
		})
	}()

	select {
	case line := <-got:
		if !strings.Contains(line, "ok") {
			t.Fatalf("upstream stdout = %q, want it to contain %q", line, "ok")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream produced no output; relative arg likely did not resolve from the config dir")
	}
	_ = up.Close()
}
