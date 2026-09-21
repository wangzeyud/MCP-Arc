package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/transport"
)

const (
	// upstreamDownCode is returned to a client whose in-flight request is failed
	// when the upstream disconnects (see failPendingUpstreamDown).
	upstreamDownCode = -32002
	// upstreamWriteFailedCode is returned when forwarding a client request to the
	// upstream fails (broken pipe, not connected, ...).
	upstreamWriteFailedCode = -32003

	reconnectBackoffInit = 500 * time.Millisecond
	reconnectBackoffMax  = 30 * time.Second
)

// errUpstreamDown is returned by writeUpstream when no upstream connection is
// currently established (between reconnects, or before the first connect).
var errUpstreamDown = errors.New("upstream not connected")

// setUpstreamWriter swaps the active upstream write function under lock. Only the
// supervisor touches this; handleClientMessage/Replay read it via writeUpstream.
func (p *Proxy) setUpstreamWriter(w func([]byte) error) {
	p.upstreamMu.Lock()
	p.upstreamWriter = w
	p.upstreamMu.Unlock()
}

// setUpstreamInstance records the live upstream transporter so per-request
// cancellation (e.g. cancelling the upstream POST when a client disconnects) can
// reach transport-specific methods like StreamableHTTPUpstream.Cancel.
func (p *Proxy) setUpstreamInstance(u transport.UpstreamTransporter) {
	p.upstreamMu.Lock()
	p.upstreamInstance = u
	p.upstreamMu.Unlock()
}

func (p *Proxy) getUpstreamInstance() transport.UpstreamTransporter {
	p.upstreamMu.RLock()
	u := p.upstreamInstance
	p.upstreamMu.RUnlock()
	return u
}

// cancelUpstream aborts an in-flight upstream request if the current upstream
// supports per-request cancellation. Used to propagate a client disconnect
// (respond failure) upstream so a hung server request is torn down promptly.
func (p *Proxy) cancelUpstream(id string) {
	u := p.getUpstreamInstance()
	if u == nil {
		return
	}
	if c, ok := u.(interface{ Cancel(string) }); ok {
		c.Cancel(id)
	}
}

// getUpstreamWriter returns the current upstream writer (or nil) under lock.
func (p *Proxy) getUpstreamWriter() func([]byte) error {
	p.upstreamMu.RLock()
	w := p.upstreamWriter
	p.upstreamMu.RUnlock()
	return w
}

// writeUpstream forwards a frame to the upstream, returning a clear error when no
// connection is established. It never blocks on a dead upstream: during a reconnect
// gap the writer is nil and this returns immediately.
func (p *Proxy) writeUpstream(b []byte) error {
	w := p.getUpstreamWriter()
	if w == nil {
		return errUpstreamDown
	}
	return w(b)
}

// upstreamSupervisor keeps the upstream connected for the lifetime of ctx. On any
// failure (process crash, connection drop, start error) it waits with exponential
// backoff and tries again, so a flaky upstream degrades to transient errors rather
// than taking the proxy down. In-flight requests are failed promptly on disconnect
// so clients are not left hanging until the 5m sweep.
func (p *Proxy) upstreamSupervisor(ctx context.Context, factory func() (transport.UpstreamTransporter, error)) {
	backoff := reconnectBackoffInit
	for {
		if ctx.Err() != nil {
			return
		}
		up, err := factory()
		if err != nil {
			log.Printf("warn: upstream start failed: %v (retry in %s)", err, backoff)
		} else {
			p.setUpstreamWriter(up.Write)
			p.setUpstreamInstance(up)
			log.Printf("mcp-arc: upstream connected")
			runErr := up.Run(ctx, p.onUpstreamMessage)
			_ = up.Close()
			p.setUpstreamWriter(nil)
			p.setUpstreamInstance(nil)
			// The upstream went away: fail any requests it was handling so the
			// client gets a clear error instead of a 5-minute hang.
			p.failPendingUpstreamDown()
			if runErr != nil && ctx.Err() == nil {
				log.Printf("warn: upstream disconnected: %v (retry in %s)", runErr, backoff)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < reconnectBackoffMax {
			backoff *= 2
		}
	}
}

// onUpstreamMessage adapts transport's message callback to the interception chain.
func (p *Proxy) onUpstreamMessage(raw []byte) {
	if err := p.processUpstreamMessage(raw); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("warn: upstream message: %v", err)
	}
}

// failPendingUpstreamDown fails every in-flight request with a clear JSON-RPC error
// when the upstream connection is lost. They will never be answered by a dead
// upstream, so failing them fast is the correct, fail-closed behaviour.
func (p *Proxy) failPendingUpstreamDown() {
	p.mu.Lock()
	expired := make([]*pendingCall, 0, len(p.pending))
	for _, pc := range p.pending {
		expired = append(expired, pc)
	}
	p.pending = make(map[string]*pendingCall)
	p.mu.Unlock()

	for _, pc := range expired {
		if pc.respond == nil {
			continue
		}
		_ = pc.respond(jsonRPCError(pc.origIDRaw, upstreamDownCode, "upstream disconnected"), true)
	}
}

// jsonRPCError builds a JSON-RPC error frame carrying the given id (raw JSON).
func jsonRPCError(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	return b
}

// extractMsgID returns the raw JSON-RPC id of a client frame, if present.
func extractMsgID(raw []byte) (json.RawMessage, bool) {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m.ID, len(m.ID) > 0
}

// pendingKey extracts the gateway-unique correlation id ("gw-N") from a forwarded
// client frame, for dropping a pending entry whose write failed.
func pendingKey(forward []byte) (string, bool) {
	id, ok := extractMsgID(forward)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(id, &s); err != nil {
		return "", false
	}
	return s, true
}
