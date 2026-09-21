package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/transport"
)

// e2eFreeAddr grabs an unused TCP address on localhost for the client transport.
func e2eFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestE2EStreamableHTTPProxyRoundTrip wires the real transport objects to the
// proxy exactly like proxy.run (Streamable HTTP on both sides) and drives a full
// client -> proxy -> upstream -> proxy -> client round trip. It asserts the
// response is correlated back, that an upstream-initiated scoped notification is
// forwarded (with the scope marker stripped) instead of broadcast, and that the
// call is captured by the audit store.
func TestE2EStreamableHTTPProxyRoundTrip(t *testing.T) {
	// Fake upstream MCP server speaking Streamable HTTP.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		if len(m.ID) == 0 { // notification -> 202 Accepted
			w.WriteHeader(http.StatusAccepted)
			return
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fl.Flush()
		// An in-flight, request-scoped progress notification (scope == the
		// gateway id the proxy assigned upstream).
		fmt.Fprintf(w, "event: message\ndata: %s\n\n",
			fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"pct":50},"__arc_scope__":%s}`, m.ID))
		fl.Flush()
		// The tool result, echoed with the same id.
		fmt.Fprintf(w, "event: message\ndata: %s\n\n",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, m.ID))
		fl.Flush()
	}))
	defer upstreamSrv.Close()

	// Build the proxy. newTestProxy wires a capturing audit store and a no-op
	// broadcast; we override the upstream writer and the client broadcast with the
	// real transports below.
	p, store, _ := newTestProxy()

	upstream := transport.NewStreamableHTTPUpstream(upstreamSrv.URL)
	p.setUpstreamWriter(func(b []byte) error { return upstream.Write(b) })
	p.setUpstreamInstance(upstream)

	clientAddr := e2eFreeAddr(t)
	client := transport.NewStreamableHTTPClient(clientAddr, "/mcp")
	p.clientBroadcast = client.Broadcast

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go upstream.Run(ctx, p.onUpstreamMessage)
	go client.Run(ctx, func(raw []byte, respond func([]byte, bool) error) func() {
		return p.handleClientMessage(raw, respond)
	})

	clientURL := "http://" + clientAddr + "/mcp"

	// Wait for the client transport to be listening.
	var ready *http.Response
	var rerr error
	deadline := time.Now().Add(3 * time.Second)
	for {
		var req *http.Request
		req, rerr = http.NewRequestWithContext(ctx, "POST", clientURL,
			bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{}}}`)))
		if rerr == nil {
			req.Header.Set("Content-Type", "application/json")
			ready, rerr = http.DefaultClient.Do(req)
		}
		if rerr == nil {
			_ = ready.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client transport never became ready: %v", rerr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Scenario A: a tools/call request that yields a scoped notification + result.
	reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
	defer reqCancel()
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"password":"topsecret"}}}`
	req, _ := http.NewRequestWithContext(reqCtx, "POST", clientURL, bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	frames := parseSSEFrames(t, data)
	if len(frames) < 2 {
		t.Fatalf("client received %d SSE frames, want >=2 (raw=%s)", len(frames), data)
	}
	var gotNotif, gotResult bool
	for _, f := range frames {
		if strings.Contains(string(f), "notifications/progress") {
			gotNotif = true
			if strings.Contains(string(f), "__arc_scope__") {
				t.Errorf("scope marker must be stripped before forwarding: %s", f)
			}
		}
		if strings.Contains(string(f), `"id":1`) && strings.Contains(string(f), `"result"`) {
			gotResult = true
		}
	}
	if !gotNotif {
		t.Errorf("scoped upstream notification not delivered to client (raw=%s)", data)
	}
	if !gotResult {
		t.Errorf("tool result not delivered back to client (raw=%s)", data)
	}

	// The call must be audited under its tool name.
	var audited bool
	for _, rec := range store.records {
		if rec.ToolName == "x" {
			audited = true
		}
	}
	if !audited {
		t.Errorf("audit store did not capture the call (records=%d)", len(store.records))
	}

	// Scenario B: a client notification must get a 202 Accepted from the proxy.
	noteCtx, noteCancel := context.WithTimeout(ctx, 5*time.Second)
	defer noteCancel()
	note, _ := http.NewRequestWithContext(noteCtx, "POST", clientURL,
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)))
	note.Header.Set("Content-Type", "application/json")
	noteResp, err := http.DefaultClient.Do(note)
	if err != nil {
		t.Fatal(err)
	}
	defer noteResp.Body.Close()
	if noteResp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", noteResp.StatusCode)
	}
}

// TestE2EStreamableHTTPProxyCancelOnClientDisconnect wires real Streamable HTTP
// transports to the proxy and verifies T5's cancellation propagation: when the
// client closes its connection mid-call, the proxy tears down the still-running
// upstream request (instead of leaving it hanging until the 5-minute sweep).
//
// It drives a real HTTP client that POSTs a tools/call, waits for the upstream to
// have received it (so the upstream request is genuinely in flight), then cancels
// the client's request context — simulating a client disconnect. The upstream
// server blocks on its own request context; it must observe that context being
// cancelled, which proves the proxy called cancelUpstream through the real wiring.
func TestE2EStreamableHTTPProxyCancelOnClientDisconnect(t *testing.T) {
	// Upstream MCP server: on a request, record arrival and then block until the
	// client (proxy) disconnects. Closing cancelled proves the proxy cancelled the
	// in-flight upstream POST.
	received := make(chan struct{})
	cancelled := make(chan struct{})
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &m)
		if len(m.ID) == 0 { // notification -> 202 Accepted
			w.WriteHeader(http.StatusAccepted)
			return
		}
		close(received)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstreamSrv.Close()

	p, _, _ := newTestProxy()

	upstream := transport.NewStreamableHTTPUpstream(upstreamSrv.URL)
	p.setUpstreamWriter(func(b []byte) error { return upstream.Write(b) })
	p.setUpstreamInstance(upstream)

	clientAddr := e2eFreeAddr(t)
	client := transport.NewStreamableHTTPClient(clientAddr, "/mcp")
	p.clientBroadcast = client.Broadcast

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go upstream.Run(ctx, p.onUpstreamMessage)
	go client.Run(ctx, func(raw []byte, respond func([]byte, bool) error) func() {
		return p.handleClientMessage(raw, respond)
	})

	clientURL := "http://" + clientAddr + "/mcp"

	// Wait for the client transport to be listening (TCP-level probe only — a full
	// HTTP POST would be forwarded to the blocking upstream and never return).
	readyDeadline := time.Now().Add(3 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", clientAddr, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("client transport never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Start the client call in the background. The proxy forwards it to the
	// upstream, which blocks, so this POST never completes until the upstream
	// responds (it won't) or the client disconnects.
	reqCtx, reqCancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(reqCtx, "POST", clientURL,
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{}}}`)))
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		close(done)
	}()

	// Wait until the upstream has actually received the forwarded request, so the
	// upstream POST is in flight when we disconnect the client.
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received the forwarded request")
	}

	// Client disconnect: cancel the request context, which closes the client's
	// HTTP connection. The proxy must detect this and cancel the upstream request.
	reqCancel()

	// The upstream's blocking request must observe the disconnect (r.Context()
	// cancelled) because the proxy tore down its upstream POST.
	select {
	case <-cancelled:
		// good: proxy propagated the client disconnect upstream
	case <-time.After(4 * time.Second):
		t.Fatal("upstream request was not cancelled after client disconnect")
	}

	// The client's POST should have returned (connection closed by proxy).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client POST did not return after disconnect")
	}
}

// parseSSEFrames splits an SSE body into the JSON payloads of each `data:` line.
func parseSSEFrames(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			t.Fatalf("invalid SSE data frame %q: %v", payload, err)
		}
		out = append(out, raw)
	}
	return out
}
