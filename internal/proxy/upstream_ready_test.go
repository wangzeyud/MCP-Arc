package proxy

import (
	"testing"
	"time"
)

// A client (Cherry Studio, Claude Desktop, Cursor, …) spawns the proxy and sends
// `initialize` immediately. If the upstream child is still starting, the request
// must wait for the first connect instead of being rejected with an error.
func TestWriteUpstreamWaitsForFirstConnect(t *testing.T) {
	p := &Proxy{upstreamReady: make(chan struct{})}
	// The first-connect wait is now config-driven (server.upstream_connect_timeout_ms);
	// set it explicitly here since the test bypasses New().
	p.firstUpstreamConnectWait = 5 * time.Second

	var got []byte
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.setUpstreamWriter(func(b []byte) error { got = b; return nil })
		p.markUpstreamReady()
	}()

	start := time.Now()
	if err := p.writeUpstream([]byte("initialize")); err != nil {
		t.Fatalf("writeUpstream = %v, want nil after waiting for the first connect", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("writeUpstream returned after %s, before the upstream connected", elapsed)
	}
	if string(got) != "initialize" {
		t.Fatalf("upstream received %q, want %q", got, "initialize")
	}
}

// Once the upstream has connected, a later disconnect (reconnect gap) must still
// fail fast — the proxy must never block on a dead upstream.
func TestWriteUpstreamFailsFastInReconnectGap(t *testing.T) {
	p := &Proxy{upstreamReady: make(chan struct{})}
	p.markUpstreamReady() // upstream connected once, now the writer is nil

	start := time.Now()
	if err := p.writeUpstream([]byte("x")); err != errUpstreamDown {
		t.Fatalf("writeUpstream = %v, want errUpstreamDown", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("writeUpstream blocked for %s, want fail-fast", elapsed)
	}
}

// A proxy built without a readiness channel (as several unit tests do) must keep
// the old fail-fast behaviour rather than block on a nil channel.
func TestWriteUpstreamFailFastWithoutReadyChannel(t *testing.T) {
	p := &Proxy{}

	start := time.Now()
	if err := p.writeUpstream([]byte("x")); err != errUpstreamDown {
		t.Fatalf("writeUpstream = %v, want errUpstreamDown", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("writeUpstream blocked for %s, want fail-fast", elapsed)
	}
}

// The first-connect wait must be driven by the (configurable) field, not a
// hardcoded 10s constant: a slow npx cold start should be tunable. With a tiny
// timeout it must return errUpstreamDown quickly, not after 10s.
func TestWriteUpstreamFirstConnectTimeoutConfigurable(t *testing.T) {
	p := &Proxy{upstreamReady: make(chan struct{})}
	p.firstUpstreamConnectWait = 30 * time.Millisecond

	start := time.Now()
	if err := p.writeUpstream([]byte("initialize")); err != errUpstreamDown {
		t.Fatalf("writeUpstream = %v, want errUpstreamDown", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("writeUpstream returned after %s, want ~30ms configurable wait", elapsed)
	}
}
