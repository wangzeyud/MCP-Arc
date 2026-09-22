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

// ReserveTCPPort must hold the socket (not release it after probing) so two
// instances launched at the same time cannot both grab the same port. The first
// reserve keeps its port; a second reserve of that same port must move away.
func TestReserveTCPPortHoldsAndMoves(t *testing.T) {
	port := freePort(t)
	ln, got, moved := ReserveTCPPort("127.0.0.1", port)
	if ln == nil || got != port || moved {
		t.Fatalf("ReserveTCPPort(%d) = (%v,%d,%v), want (listener,%d,false)", port, ln, got, moved, port)
	}
	defer ln.Close()
	if lp := ln.Addr().(*net.TCPAddr).Port; lp != port {
		t.Fatalf("listener bound on %d, reported %d", lp, port)
	}

	// Second instance spawned at the same time: must not collide with the first.
	ln2, port2, moved2 := ReserveTCPPort("127.0.0.1", port)
	if ln2 == nil {
		t.Fatalf("second ReserveTCPPort = nil, want a held socket on a different port")
	}
	defer ln2.Close()
	if !moved2 || port2 == port {
		t.Fatalf("second reserve: port=%d moved=%v, want moved && != %d", port2, moved2, port)
	}
}

// When the preferred port is genuinely free, ReserveTCPPort returns it without
// moving and the socket stays bound (a follow-up probe sees it occupied).
func TestReserveTCPPortKeepsPreferred(t *testing.T) {
	port := freePort(t)
	ln, got, moved := ReserveTCPPort("127.0.0.1", port)
	if ln == nil || got != port || moved {
		t.Fatalf("ReserveTCPPort(%d) = (%v,%d,%v), want (listener,%d,false)", port, ln, got, moved, port)
	}
	defer ln.Close()
	// The held socket now answers, so FreeTCPPort must treat it as taken.
	if free, _ := FreeTCPPort("127.0.0.1", port); free == port {
		t.Fatalf("held port %d should be reported taken by FreeTCPPort", port)
	}
}
