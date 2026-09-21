package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/transport"
)

const (
	// pendingTTL is how long a request waits for its upstream response before
	// being reaped. An upstream that dies or hangs mid-call would otherwise
	// leak one entry per call, without bound, over a long-lived process.
	pendingTTL = 5 * time.Minute
	// pendingSweepInterval is how often expired pending calls are reaped.
	pendingSweepInterval = time.Minute

	// upstreamTimeoutCode is the JSON-RPC error code handed back to a client
	// whose request the upstream never answered (see reapPending).
	upstreamTimeoutCode = -32001
)

type pendingCall struct {
	toolName     string
	maskedParams string
	rawParams    string
	start        time.Time
	deadline     time.Time
	respond      func([]byte, bool) error
	origIDRaw    json.RawMessage
}

// decodeJSONObject decodes raw into a generic map, keeping every number in its
// original literal form (json.Number) rather than the default float64.
//
// This is what keeps the proxy byte-transparent: intercepted messages are
// re-serialised on the way through, and a float64 round trip silently rewrites
// any integer beyond 2^53 — the upstream would receive a different argument
// than the client sent, and nobody would notice, because the JSON-RPC id is
// rewritten the same way on both legs.
//
// It rejects anything that is not exactly one JSON object, so malformed or
// batched payloads keep taking the pass-through path.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil {
		return nil, err
	}
	// json.Unmarshal rejects trailing data, Decode does not — restore that.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after JSON object")
	}
	return msg, nil
}

// processClientMessage intercepts a message from an MCP client. It applies rate
// limiting and (for tools/call) masking + audit prep, rewrites the JSON-RPC id
// to a gateway-unique value for correlation/routing across concurrent sessions,
// and returns the bytes to forward upstream. On rate limiting it returns a
// synthetic error to send back to the client instead.
func (p *Proxy) processClientMessage(raw []byte, respond func([]byte, bool) error) (forward []byte, synthetic []byte) {
	msg, err := decodeJSONObject(raw)
	if err != nil {
		return raw, nil // not JSON-RPC, pass through untouched
	}
	id, hasID := msg["id"]
	if !hasID {
		// notification: no response expected, pass through
		return raw, nil
	}

	// The only MCP knowledge used here is "is this a tools/call" (see protocol.go).
	toolName, arguments, isToolCall := toolCall(msg)

	// rate limit only tool calls
	if isToolCall && !p.limiter.Allow(p.clientID) {
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error":   map[string]any{"code": -32000, "message": "rate limit exceeded"},
		}
		b, _ := json.Marshal(resp)
		return nil, b
	}

	// gateway-unique id for correlation across concurrent client sessions
	upID := fmt.Sprintf("gw-%d", p.seq.Add(1))
	now := time.Now()
	origIDRaw, _ := json.Marshal(id)
	pc := &pendingCall{respond: respond, origIDRaw: origIDRaw, start: now, deadline: now.Add(pendingTTL)}

	if isToolCall {
		if arguments == nil {
			arguments = map[string]any{}
		}
		pc.toolName = toolName
		rawParams, _ := json.Marshal(arguments)
		maskedParams := rawParams
		if p.masker != nil {
			if m, err := p.masker.Mask(arguments); err == nil && m != nil {
				maskedParams, _ = json.Marshal(m)
			}
		}
		pc.rawParams = string(rawParams)
		pc.maskedParams = string(maskedParams)
	}

	p.mu.Lock()
	p.pending[upID] = pc
	p.mu.Unlock()

	msg["id"] = upID
	out, _ := json.Marshal(msg)
	return out, nil
}

// processUpstreamMessage intercepts a response from the upstream server, writes
// an audit record for tools/call responses, restores the original client id, and
// routes the message back to the correct client session.
func (p *Proxy) processUpstreamMessage(raw []byte) error {
	msg, err := decodeJSONObject(raw)
	if err != nil {
		return nil
	}

	// Request-scoped upstream notification (Streamable HTTP): the StreamableHTTP
	// upstream tagged it with the owning request id so we can route it to that
	// client's stream instead of broadcasting. Strip the marker before forwarding;
	// it must never reach the client.
	if scope, ok := msg[transport.ArcScopeKey].(string); ok && scope != "" {
		p.mu.Lock()
		pc, ok := p.pending[scope]
		p.mu.Unlock()
		if !ok {
			return nil
		}
		delete(msg, transport.ArcScopeKey)
		scoped, err := json.Marshal(msg)
		if err != nil {
			return nil
		}
		if err := pc.respond(scoped, false); err != nil {
			// client stream gone: propagate cancellation upstream
			p.cancelUpstream(scope)
		}
		return nil
	}

	id, ok := msg["id"]
	if !ok {
		// upstream-initiated broadcast notification (legacy SSE/stdio paths)
		return p.clientBroadcast(raw)
	}
	key := fmt.Sprintf("%v", id)

	p.mu.Lock()
	pc, ok := p.pending[key]
	if !ok {
		p.mu.Unlock()
		return nil
	}
	delete(p.pending, key)
	p.mu.Unlock()

	// restore the original client id
	msg["id"] = pc.origIDRaw
	outRaw, _ := json.Marshal(msg)

	if pc.toolName != "" {
		latency := time.Since(pc.start).Milliseconds()
		errMsg := ""
		var result any
		if e, ok := msg["error"].(map[string]any); ok {
			b, _ := json.Marshal(e)
			errMsg = string(b)
		} else {
			result = msg["result"]
		}
		rawResultBytes := []byte("null")
		if result != nil {
			rawResultBytes, _ = json.Marshal(result)
		}
		resultBytes := []byte("null")
		if result != nil {
			if p.masker != nil {
				// An InputRequiredResult (MCP 2026-07-28 MRTR) embeds
				// inputRequests (sampling/elicitation params) the client must
				// answer. Per the v0.5 plan we audit it but do NOT apply nested
				// masking to those params, so we leave the result intact.
				if rm, ok := result.(map[string]any); ok && !isInputRequiredResult(rm) {
					if masked, err := p.masker.MaskResult(rm); err == nil && masked != nil {
						result = masked
					}
				}
			}
			resultBytes, _ = json.Marshal(result)
		}
		rec := &audit.CallRecord{
			ClientID:  p.clientID,
			ToolName:  pc.toolName,
			Params:    pc.maskedParams,
			RawParams: pc.rawParams,
			Result:    string(resultBytes),
			RawResult: string(rawResultBytes),
			ErrorMsg:  errMsg,
			LatencyMs: latency,
			Timestamp: time.Now(),
		}
		if p.auditWrites {
			p.enqueueAudit(rec)
		}
	}

	if err := pc.respond(outRaw, true); err != nil {
		// client stream gone: propagate cancellation upstream
		p.cancelUpstream(key)
	}
	return nil
}

// isInputRequiredResult reports whether a tool result is an MCP 2026-07-28
// InputRequiredResult (MRTR): it carries inputRequests the client must answer.
// When true, the proxy audits the result but skips nested masking of the
// embedded sampling/elicitation params (v0.5 plan: audit-only for now).
func isInputRequiredResult(result map[string]any) bool {
	if _, ok := result["inputRequests"]; ok {
		return true
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		if t, ok := sc["type"].(string); ok && t == "InputRequiredResult" {
			return true
		}
	}
	return false
}

// sweepPending reaps pending calls whose upstream never answered, until ctx is
// done. Without it an upstream that dies or hangs mid-call leaks one entry per
// call for the lifetime of the process.
func (p *Proxy) sweepPending(ctx context.Context) {
	t := time.NewTicker(pendingSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.reapPending(time.Now())
		}
	}
}

// reapPending drops every pending call past its deadline and, when the caller
// is still waiting, answers it with a JSON-RPC error so no client is left
// hanging on a request that will never complete.
func (p *Proxy) reapPending(now time.Time) {
	p.mu.Lock()
	expired := make([]*pendingCall, 0)
	for id, pc := range p.pending {
		if now.After(pc.deadline) {
			expired = append(expired, pc)
			delete(p.pending, id)
		}
	}
	p.mu.Unlock()

	for _, pc := range expired {
		log.Printf("warn: pending call %q timed out after %s with no upstream response", pc.toolName, pendingTTL)
		if pc.respond == nil {
			continue
		}
		resp, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      pc.origIDRaw,
			"error": map[string]any{
				"code":    upstreamTimeoutCode,
				"message": "upstream timeout",
			},
		})
		if err != nil {
			continue
		}
		_ = pc.respond(resp, true)
	}
}

// dropPending removes a pending call without notifying anyone. Used when the
// caller has already given up on the response (e.g. replay returning early).
func (p *Proxy) dropPending(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}
