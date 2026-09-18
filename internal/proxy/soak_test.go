package proxy

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// TestProxySoakStability drives many tool-call round trips through the full
// interception chain (mask -> forward -> audit -> correlate -> respond) and
// asserts the proxy stays correct and leak-free.
//
// It is the v0.4 stability/soak gate: run the whole suite with `go test -race`
// (requires a C compiler / CGO) to additionally surface concurrency bugs, since
// the client and upstream transports operate as independent concurrent streams
// in production.
func TestProxySoakStability(t *testing.T) {
	const rounds = 20000

	p, store, c := newTestProxy()

	// Settled baseline goroutine count (no in-flight audit writes yet).
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 1; i <= rounds; i++ {
		req := []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"pay","arguments":{"orderId":1234567890123456789,"note":"soak-%d"}}}`,
			i, i))
		forward, synthetic := p.processClientMessage(req, c.respond)
		if synthetic != nil {
			t.Fatalf("call %d: unexpected synthetic response: %s", i, synthetic)
		}
		if forward == nil {
			t.Fatalf("call %d: not forwarded to upstream", i)
		}
		gwID := jsonID(t, forward)
		resp := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"ok":true}}`, gwID))
		if err := p.processUpstreamMessage(resp); err != nil {
			t.Fatalf("call %d: upstream error: %v", i, err)
		}
	}

	// Let any in-flight audit goroutines settle before measuring.
	time.Sleep(200 * time.Millisecond)

	if n := pendingCount(p); n != 0 {
		t.Errorf("pending = %d after soak, want 0 (leaked correlation state)", n)
	}
	if want := rounds; len(store.records) != want {
		t.Errorf("audit records = %d, want %d", len(store.records), want)
	}
	if got := runtime.NumGoroutine(); got > base+10 {
		t.Errorf("goroutines = %d, base = %d (possible goroutine leak)", got, base)
	}
}

// jsonID extracts the JSON "id" field as a string from a JSON-RPC message.
func jsonID(t *testing.T, raw []byte) string {
	t.Helper()
	m, err := decodeJSONObject(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	switch v := m["id"].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		t.Fatalf("unexpected id type %T in %s", m["id"], raw)
		return ""
	}
}
