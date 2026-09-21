package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// StreamableHTTPClient is an MCP client transporter that exposes the MCP
// 2026-07-28 Streamable HTTP transport: MCP clients POST JSON-RPC to a single
// endpoint (<listen><path>, default /mcp). Each request is answered on the same
// HTTP exchange; per D1 we always reply with a `text/event-stream` body so a
// response (JSON or streamed) is delivered as one SSE `data:` event — this
// avoids needing to know the upstream Content-Type before writing response
// headers, and keeps the respond([]byte, done) per-frame contract.
//
// Notifications (JSON-RPC frames without an "id") are answered with 202 Accepted
// and no body. Upstream-initiated notifications are delivered to all active
// client streams via Broadcast (request-scoped notification routing is handled
// by the proxy, which forwards scoped notifications straight to the owning
// client's respond, keeping its stream open via done=false).
type StreamableHTTPClient struct {
	listen string
	path   string
	srv    *http.Server

	mu     sync.Mutex
	active map[*streamHandle]struct{}
}

type streamHandle struct {
	w  http.ResponseWriter
	fl http.Flusher
	mu sync.Mutex // guards writes to w
}

type respondEvent struct {
	b    []byte
	done bool
}

func NewStreamableHTTPClient(listen, path string) *StreamableHTTPClient {
	if path == "" {
		path = "/mcp"
	}
	return &StreamableHTTPClient{
		listen: listen,
		path:   path,
		active: make(map[*streamHandle]struct{}),
	}
}

func (s *StreamableHTTPClient) Run(ctx context.Context, onMessage func([]byte, func([]byte, bool) error) func()) error {
	mux := http.NewServeMux()
	mux.HandleFunc(s.path, func(w http.ResponseWriter, r *http.Request) {
		s.handlePost(w, r, onMessage)
	})
	s.srv = &http.Server{Addr: s.listen, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = s.srv.Shutdown(context.Background())
	}()
	return s.srv.ListenAndServe()
}

func (s *StreamableHTTPClient) handlePost(w http.ResponseWriter, r *http.Request, onMessage func([]byte, func([]byte, bool) error) func()) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// A JSON-RPC frame without an "id" is a notification: forward it and answer
	// 202 Accepted with no body (Streamable HTTP semantics).
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &probe)
	if len(probe.ID) == 0 {
		cp := append([]byte(nil), body...)
		// No pending call to clean up for a notification.
		_ = onMessage(cp, func([]byte, bool) error { return nil })
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Request: stream the response back as SSE (D1). The proxy drives completion
	// via done=true on the terminal frame; request-scoped notifications arrive as
	// additional frames with done=false and keep the stream open.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	h := &streamHandle{w: w, fl: flusher}
	s.mu.Lock()
	s.active[h] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, h)
		s.mu.Unlock()
	}()

	respondCh := make(chan respondEvent, 8)
	closed := make(chan struct{})
	var once sync.Once
	closeClosed := func() { once.Do(func() { close(closed) }) }

	respond := func(b []byte, done bool) error {
		select {
		case respondCh <- respondEvent{b: b, done: done}:
			return nil
		case <-closed:
			return errors.New("stream closed")
		}
	}

	go func() {
		for {
			select {
			case ev := <-respondCh:
				h.mu.Lock()
				fmt.Fprintf(h.w, "event: message\ndata: %s\n\n", ev.b)
				h.fl.Flush()
				h.mu.Unlock()
				if ev.done {
					closeClosed()
					return
				}
			case <-r.Context().Done():
				closeClosed()
				return
			}
		}
	}()

	cp := append([]byte(nil), body...)
	// onMessage returns a cleanup (e.g. cancel the upstream request) that the
	// proxy wants invoked when this client connection closes — whether because the
	// client disconnected or because the stream completed normally.
	cleanup := onMessage(cp, respond)
	<-closed
	if cleanup != nil {
		cleanup()
	}
}

// Broadcast writes a message to every active client stream. Used for
// upstream-initiated broadcast notifications (request-scoped routing is handled
// by the proxy, not here).
func (s *StreamableHTTPClient) Broadcast(raw []byte) error {
	s.mu.Lock()
	handles := make([]*streamHandle, 0, len(s.active))
	for h := range s.active {
		handles = append(handles, h)
	}
	s.mu.Unlock()
	for _, h := range handles {
		h.mu.Lock()
		fmt.Fprintf(h.w, "event: message\ndata: %s\n\n", raw)
		h.fl.Flush()
		h.mu.Unlock()
	}
	return nil
}

func (s *StreamableHTTPClient) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}
