package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// SSEServer is an MCP client transporter: it exposes the MCP SSE transport so
// that MCP clients can connect to MCP Arc over HTTP. Clients open GET /sse to
// receive messages, and POST JSON-RPC to /messages?sessionId=... to send them.
type SSEServer struct {
	listen   string
	mu       sync.Mutex
	sessions map[string]*sseSession
	srv      *http.Server
}

type sseSession struct {
	ch   chan []byte
	done chan struct{} // closed when the SSE connection ends
	mu   sync.Mutex    // guards writes to this session's ResponseWriter
}

func NewSSEServer(listen string) *SSEServer {
	return &SSEServer{listen: listen, sessions: map[string]*sseSession{}}
}

func (s *SSEServer) Run(ctx context.Context, onMessage func([]byte, func([]byte, bool) error) func()) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		s.handleSSE(w, r, ctx, onMessage)
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		s.handleMessages(w, r, onMessage)
	})
	s.srv = &http.Server{Addr: s.listen, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = s.srv.Shutdown(context.Background())
	}()
	return s.srv.ListenAndServe()
}

func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request, ctx context.Context, onMessage func([]byte, func([]byte, bool) error) func()) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID := randString(16)
	sess := &sseSession{ch: make(chan []byte, 64), done: make(chan struct{})}
	s.mu.Lock()
	s.sessions[sessionID] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.mu.Unlock()
		close(sess.done)
	}()

	fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=%s\n\n", sessionID)
	flusher.Flush()

	// writer goroutine: drain the channel onto the SSE stream.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case b, ok := <-sess.ch:
				if !ok {
					return
				}
				sess.mu.Lock()
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				flusher.Flush()
				sess.mu.Unlock()
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				sess.mu.Lock()
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()
				sess.mu.Unlock()
			}
		}
	}()

	<-r.Context().Done()
}

func (s *SSEServer) handleMessages(w http.ResponseWriter, r *http.Request, onMessage func([]byte, func([]byte, bool) error) func()) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	// respond is bound to the SESSION lifetime (sess.done), NOT the POST request
	// context — upstream responses are delivered asynchronously, long after this
	// handler returns, so the POST request context would already be closed.
	respond := func(b []byte, _ bool) error {
		select {
		case sess.ch <- b:
			return nil
		case <-sess.done:
			return errors.New("session closed")
		case <-time.After(5 * time.Second):
			return errors.New("session write timeout")
		}
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	onMessage(cp, respond)
	w.WriteHeader(http.StatusAccepted)
}

// Broadcast writes a message to every connected SSE session.
func (s *SSEServer) Broadcast(raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		select {
		case sess.ch <- raw:
		default:
		}
	}
	return nil
}

func (s *SSEServer) Close() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

func randString(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
