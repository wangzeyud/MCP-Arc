package netutil

import (
	"net"
	"strconv"
	"testing"
)

// freePort grabs an OS-assigned free port, releases it, and returns it. A
// subsequent bind of the same port is almost always successful, which is enough
// for these tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestFreeTCPPortKeepsPreferred(t *testing.T) {
	port := freePort(t)
	got, changed := FreeTCPPort("127.0.0.1", port)
	if got != port || changed {
		t.Fatalf("FreeTCPPort(%d) = (%d,%v), want (%d,false)", port, got, changed, port)
	}
}

func TestFreeTCPPortMovesWhenTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	got, changed := FreeTCPPort("127.0.0.1", port)
	if !changed || got == port {
		t.Fatalf("FreeTCPPort(%d) = (%d,%v), want a different, free port", port, got, changed)
	}
	// The reported port must actually be bindable right now.
	l2, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(got)))
	if err != nil {
		t.Fatalf("reported free port %d is not bindable: %v", got, err)
	}
	_ = l2.Close()
}

func TestFreeListenAddrMoves(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))

	got, changed := FreeListenAddr(addr)
	if !changed || got == addr {
		t.Fatalf("FreeListenAddr(%q) = (%q,%v), want a moved address", addr, got, changed)
	}
	if _, _, err := SplitListen(got); err != nil {
		t.Fatalf("moved address %q is not a valid listen addr: %v", got, err)
	}
}

func TestURLForListen(t *testing.T) {
	cases := map[string]string{
		":8081":          "http://localhost:8081/sse",
		"0.0.0.0:9000":   "http://localhost:9000/sse",
		"127.0.0.1:9000": "http://127.0.0.1:9000/sse",
		"[::]:9000":      "http://localhost:9000/sse",
	}
	for in, want := range cases {
		if got := URLForListen(in, "/sse"); got != want {
			t.Errorf("URLForListen(%q) = %q, want %q", in, got, want)
		}
	}
	if got := URLForListen("bogus", "/sse"); got != "" {
		t.Errorf("URLForListen(bogus) = %q, want empty", got)
	}
}
