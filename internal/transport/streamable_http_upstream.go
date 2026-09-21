package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// errStreamableUpstreamNotReady is returned by Write before Run has wired up
// the upstream message callback (i.e. before the supervisor has started it).
var errStreamableUpstreamNotReady = errors.New("streamable-http upstream not ready")

// ArcScopeKey is an internal marker injected into upstream notifications that
// arrive without an id inside a request's SSE stream. It tells the proxy which
// pending request that notification belongs to, so it can be routed to that
// client's stream instead of broadcast to every client (MCP 2026-07-28: a
// request's SSE stream carries request-scoped notifications). The proxy strips
// the marker before forwarding, so it never reaches the client.
const ArcScopeKey = "__arc_scope__"

// StreamableHTTPUpstream connects to a remote MCP server that speaks the MCP
// 2026-07-28 Streamable HTTP transport: every client request is forwarded as a
// single HTTP POST, and the response comes back in the same exchange as either
// `application/json` or a `text/event-stream` SSE stream. There is no persistent
// GET connection and no session id.
//
// Because the transport is request/response rather than a long-lived stream,
// Run simply parks until ctx is cancelled (the supervisor treats a non-nil Run
// error as "upstream down"); the real I/O happens in Write, which performs the
// POST and feeds each decoded JSON-RPC payload to onMessage.
type StreamableHTTPUpstream struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	onMsg   func([]byte)
	cancels map[string]context.CancelFunc
}

func NewStreamableHTTPUpstream(rawURL string) *StreamableHTTPUpstream {
	return &StreamableHTTPUpstream{
		url:    rawURL,
		client: &http.Client{},
		cancels: make(map[string]context.CancelFunc),
	}
}

func (u *StreamableHTTPUpstream) Run(ctx context.Context, onMessage func([]byte)) error {
	u.mu.Lock()
	u.onMsg = onMessage
	u.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// Cancel aborts an in-flight request's upstream POST (used to propagate a client
// disconnect / stream close upstream). It is a no-op for unknown ids.
func (u *StreamableHTTPUpstream) Cancel(id string) {
	u.mu.Lock()
	cancel, ok := u.cancels[id]
	u.mu.Unlock()
	if ok {
		cancel()
	}
}

// Write forwards one client request to the upstream Streamable HTTP endpoint and
// reads the whole response. A JSON response becomes a single onMessage call; an
// SSE response becomes one onMessage call per `data:` event (this is how
// long-running / streaming results are delivered). The upstream's response
// Content-Type selects the mode.
//
// Notifications that arrive inside a request's stream (no JSON-RPC id) are tagged
// with that request's id so the proxy can scope them to the right client.
func (u *StreamableHTTPUpstream) Write(raw []byte) error {
	u.mu.Lock()
	onMsg := u.onMsg
	u.mu.Unlock()
	if onMsg == nil {
		return errStreamableUpstreamNotReady
	}

	ownerID := extractMsgIDString(raw)

	ctx, cancel := context.WithCancel(context.Background())
	if ownerID != "" {
		u.mu.Lock()
		u.cancels[ownerID] = cancel
		u.mu.Unlock()
	}
	defer func() {
		if ownerID != "" {
			u.mu.Lock()
			delete(u.cancels, ownerID)
			u.mu.Unlock()
		}
		cancel()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	// Streamable HTTP requires these headers. Per-request Mcp-* / Origin header
	// threading from the client is deferred to a later pass (needs interceptor
	// plumbing); the response Content-Type still selects JSON vs SSE handling.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return u.readSSE(resp.Body, onMsg, ownerID)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// A bare JSON notification inside a request stream is also request-scoped.
	if !hasID(body) {
		cp := scopeNotification(ownerID, body)
		onMsg(cp)
		return nil
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	onMsg(cp)
	return nil
}

func (u *StreamableHTTPUpstream) readSSE(body io.Reader, onMsg func([]byte), ownerID string) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data != "" {
				payload := data
				if !hasID([]byte(data)) {
					payload = string(scopeNotification(ownerID, []byte(data)))
				}
				cp := make([]byte, len(payload))
				copy(cp, payload)
				onMsg(cp)
				data = ""
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	return scanner.Err()
}

func (u *StreamableHTTPUpstream) Close() error {
	u.client.CloseIdleConnections()
	u.mu.Lock()
	u.onMsg = nil
	u.cancels = make(map[string]context.CancelFunc)
	u.mu.Unlock()
	return nil
}

// hasID reports whether the JSON object in raw carries a non-empty "id" field.
func hasID(raw []byte) bool {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return len(m.ID) > 0 && string(m.ID) != "null"
}

// scopeNotification wraps a request-scoped upstream notification (no id) with the
// owning request id, so the proxy can route it to the right client stream.
func scopeNotification(ownerID string, raw []byte) []byte {
	if ownerID == "" {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	m[ArcScopeKey] = ownerID
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// extractMsgIDString returns the JSON-RPC id of raw as a string (for routing
// keys), or "" if absent.
func extractMsgIDString(raw []byte) string {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || len(m.ID) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.ID, &s); err != nil {
		return ""
	}
	return s
}
