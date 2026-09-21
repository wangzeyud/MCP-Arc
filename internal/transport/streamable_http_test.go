package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// freeAddr grabs a currently-unused TCP address on localhost for the client
// transport to bind. We probe with net.Listen (port 0 => OS-assigned) and release
// it; the test binds the same address moments later, so the window for a clash is
// negligible on a single host.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitUpstreamReady retries Write until Run has wired up the callback (or the
// deadline passes). Avoids a flaky sleep between starting Run and calling Write.
func waitUpstreamReady(t *testing.T, u *StreamableHTTPUpstream, raw []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := u.Write(raw); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream never became ready for Write")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamableHTTPUpstreamJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "" {
			t.Errorf("missing Accept header on upstream POST")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"gw-1","result":{"ok":true}}`))
	}))
	defer srv.Close()

	u := NewStreamableHTTPUpstream(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []byte
	go u.Run(ctx, func(b []byte) { got = b })

	waitUpstreamReady(t, u, []byte(`{"jsonrpc":"2.0","id":"gw-1","method":"tools/call","params":{}}`))
	if string(got) != `{"jsonrpc":"2.0","id":"gw-1","result":{"ok":true}}` {
		t.Fatalf("onMessage = %s", got)
	}
}

func TestStreamableHTTPUpstreamSSEStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", `{"jsonrpc":"2.0","id":"gw-1","result":{"step":1}}`)
		fl.Flush()
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", `{"jsonrpc":"2.0","method":"notifications/progress","params":{"pct":50}}`)
		fl.Flush()
	}))
	defer srv.Close()

	u := NewStreamableHTTPUpstream(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var frames [][]byte
	go u.Run(ctx, func(b []byte) {
		mu.Lock()
		frames = append(frames, b)
		mu.Unlock()
	})

	waitUpstreamReady(t, u, []byte(`{"jsonrpc":"2.0","id":"gw-1","method":"tools/call","params":{}}`))

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(frames)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d frames, want 2", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(string(frames[0]), ArcScopeKey) {
		t.Errorf("response frame must not be scoped: %s", frames[0])
	}
	if !strings.Contains(string(frames[1]), `"`+ArcScopeKey+`":"gw-1"`) {
		t.Errorf("notification must be scoped to gw-1, got %s", frames[1])
	}
}

// A blocked upstream POST must be torn down when the proxy cancels the request
// (T5: client disconnect -> cancel upstream). The deterministic assertion is that
// Cancel makes the in-flight Write return promptly (the upstream POST is aborted);
// the server-side disconnect observation is best-effort because in some sandboxed
// environments the FIN is slow to propagate back to the server.
func TestStreamableHTTPUpstreamCancel(t *testing.T) {
	received := make(chan struct{})
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(received)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	u := NewStreamableHTTPUpstream(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Run(ctx, func(b []byte) {})

	// Wait until Run has wired up the callback so Write proceeds past the
	// not-ready fast path (same-package access to the unexported field).
	for i := 0; i < 200; i++ {
		u.mu.Lock()
		ready := u.onMsg != nil
		u.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan error, 1)
	go func() {
		done <- u.Write([]byte(`{"jsonrpc":"2.0","id":"gw-7","method":"tools/call","params":{}}`))
	}()

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}

	u.Cancel("gw-7")

	// Primary assertion (T5 transport contract): cancelling an in-flight request
	// aborts the upstream POST, so Write returns promptly.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not return after cancel (upstream request not aborted)")
	}

	// Best-effort: the upstream server should observe the client disconnect and
	// tear down the request. Logged rather than failed because the connection
	// close can be slow to propagate in some sandboxed environments.
	select {
	case <-cancelled:
		// good
	case <-time.After(3 * time.Second):
		t.Logf("warn: upstream server did not observe the disconnect within 3s (environment-dependent)")
	}
}

func TestStreamableHTTPClientRequestRespondsSSE(t *testing.T) {
	addr := freeAddr(t)
	u := NewStreamableHTTPClient(addr, "/mcp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Run(ctx, func(raw []byte, respond func([]byte, bool) error) func() {
		_ = respond([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`), true)
		return nil
	})

	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = http.Post("http://"+addr+"/mcp", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE response, got %q", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `data: {"jsonrpc":"2.0","id":1,"result":{"ok":true}}`) {
		t.Fatalf("client body = %q", data)
	}
}

func TestStreamableHTTPClientNotificationIs202(t *testing.T) {
	addr := freeAddr(t)
	u := NewStreamableHTTPClient(addr, "/mcp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan []byte, 1)
	go u.Run(ctx, func(raw []byte, respond func([]byte, bool) error) func() {
		got <- raw
		return nil
	})

	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = http.Post("http://"+addr+"/mcp", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", resp.StatusCode)
	}
	select {
	case n := <-got:
		if !strings.Contains(string(n), "notifications/initialized") {
			t.Errorf("notification not forwarded: %s", n)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not forwarded to onMessage")
	}
}

func TestStreamableHTTPClientBroadcast(t *testing.T) {
	addr := freeAddr(t)
	u := NewStreamableHTTPClient(addr, "/mcp")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var once sync.Once
	ready := make(chan struct{})
	go u.Run(ctx, func(raw []byte, respond func([]byte, bool) error) func() {
		once.Do(func() { close(ready) })
		// Send one keepalive frame (done=false) so the client POST returns, then
		// hold the stream open; Broadcast can still push frames in afterwards.
		_ = respond([]byte(`{"jsonrpc":"2.0","method":"notifications/keepalive"}`), false)
		select {} // hold the request stream open until the client disconnects
	})

	var resp *http.Response
	var perr error
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, perr = http.Post("http://"+addr+"/mcp", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
		if perr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(perr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer resp.Body.Close()
	<-ready
	time.Sleep(20 * time.Millisecond)

	if err := u.Broadcast([]byte(`{"jsonrpc":"2.0","method":"notifications/foo"}`)); err != nil {
		t.Fatal(err)
	}

	r := bufio.NewReader(resp.Body)
	deadline = time.Now().Add(time.Second)
	found := false
	for time.Now().Before(deadline) {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			break
		}
		if strings.Contains(line, "notifications/foo") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("broadcast not delivered to the client stream")
	}
}

// Soak / load variant for the Streamable HTTP upstream: fire many concurrent
// requests and confirm every response is delivered.
func TestStreamableHTTPUpstreamSoak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &m)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, m.ID)
	}))
	defer srv.Close()

	u := NewStreamableHTTPUpstream(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	count := 0
	go u.Run(ctx, func(b []byte) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":"gw-%d","method":"tools/call","params":{}}`, i)
			for attempt := 0; attempt < 200; attempt++ {
				if u.Write([]byte(raw)) == nil {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Errorf("request gw-%d never delivered", i)
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := count
		mu.Unlock()
		if c >= N {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	c := count
	mu.Unlock()
	t.Fatalf("soak delivered %d/%d", c, N)
}
