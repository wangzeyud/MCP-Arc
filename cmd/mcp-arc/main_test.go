package main

import (
	"net"
	"reflect"
	"testing"

	"github.com/wangzeyud/mcp-arc/internal/config"
)

// splitCommand must keep paths with spaces intact (the old strings.Fields
// broke any upstream path containing a space) while still separating ordinary
// tokens and ignoring surrounding whitespace.
func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in  string
		out []string
	}{
		{"", nil},
		{"node server.js", []string{"node", "server.js"}},
		{"  node   server.js  ", []string{"node", "server.js"}},
		{`npx -y @foo/server "C:\my path\with spaces"`, []string{"npx", "-y", "@foo/server", `C:\my path\with spaces`}},
		{`sh -c 'echo hello world'`, []string{"sh", "-c", "echo hello world"}},
		{`uvx -- "some tool" arg`, []string{"uvx", "--", "some tool", "arg"}},
	}
	for _, c := range cases {
		got := splitCommand(c.in)
		if !reflect.DeepEqual(got, c.out) {
			t.Fatalf("splitCommand(%q) = %#v, want %#v", c.in, got, c.out)
		}
	}
}

// resolvePorts must reserve the admin console port atomically: it returns a held
// listener (not a test-then-release probe) whose port matches the reported
// admin port, so the console URL is correct and a second instance cannot race
// for the same port.
func TestResolvePortsReservesAdminListener(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Port = 0 // let the OS pick a free port

	consoleURL, _, adminLn := resolvePorts(cfg)
	if adminLn == nil {
		t.Fatalf("resolvePorts returned nil admin listener, want a held socket")
	}
	defer adminLn.Close()
	if consoleURL == "" {
		t.Fatalf("consoleURL empty, want a console URL")
	}
	lp, ok := adminLn.Addr().(*net.TCPAddr)
	if !ok || lp.Port != cfg.Admin.Port {
		t.Fatalf("listener port = %v, cfg.Admin.Port = %d", adminLn.Addr(), cfg.Admin.Port)
	}
}
