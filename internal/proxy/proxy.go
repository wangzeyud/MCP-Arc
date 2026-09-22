// Package proxy implements an MCP-agnostic relay that sits between an MCP client
// and an MCP server. It deliberately lives at the transport layer: the only
// MCP-specific knowledge it relies on is the "tools/call" JSON-RPC method name
// and its params shape. It does NOT parse or depend on MCP protocol internals, so
// the governance features (masking, audit, rate limit, replay) keep working even
// as the MCP spec evolves.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/admin"
	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
	"github.com/wangzeyud/mcp-arc/internal/mask"
	"github.com/wangzeyud/mcp-arc/internal/ratelimit"
	"github.com/wangzeyud/mcp-arc/internal/transport"
)

// procStartedAt is when this process came up. Together with the PID it is
// reported through /api/status so the console can show which instance the
// operator is actually looking at (a stale tab or a leftover process otherwise
// looks exactly like a quiet one).
var procStartedAt = time.Now()

type Options struct {
	UpstreamCmd []string
	Config      *config.Config
	// SSEURL / ConsoleURL are the effective (possibly auto-adjusted) addresses
	// surfaced in the console. SSEURL holds the client endpoint (SSE or
	// Streamable HTTP); it is empty only when the client transport is "stdio".
	SSEURL     string
	ConsoleURL string
	// ConfigDir anchors relative paths in server.upstream to the directory of the
	// loaded config file, so the upstream runs with a predictable working directory
	// regardless of the CWD the MCP client spawned mcp-arc with. Empty when no
	// config file was loaded (built-in defaults).
	ConfigDir string
	// AdminListener, when non-nil, is the already-reserved (bound, held) socket
	// for the web console. main.go reserves it atomically so two instances never
	// collide on the same admin port. When nil the server falls back to binding
	// Config.Admin.Port itself (used by tests and embedded scenarios).
	AdminListener net.Listener
}

type Proxy struct {
	opts        Options
	auditStore  audit.Store            // also holds the masking rules table
	auditWrites bool                   // audit.enabled: whether call records are persisted
	auditCh     chan *audit.CallRecord // async write buffer; nil → synchronous insert
	masker      *mask.Masker
	limiter     *ratelimit.TokenBucketManager
	clientID    string

	upstreamWriter   func([]byte) error
	upstreamInstance  transport.UpstreamTransporter // live upstream, for per-request cancel
	clientBroadcast   func([]byte) error

	// upstreamEverUp is true once the upstream has connected at least once;
	// upstreamReady is closed at that first connect. Together they let a client
	// request arriving during startup wait for the initial connect instead of
	// being rejected, while still failing fast during a later reconnect gap.
	upstreamEverUp    atomic.Bool
	upstreamReady     chan struct{}
	upstreamReadyOnce sync.Once

	// firstUpstreamConnectWait bounds how long a client request waits for the
	// upstream's very first connection (see writeUpstream). Config-driven via
	// server.upstream_connect_timeout_ms.
	firstUpstreamConnectWait time.Duration

	mu         sync.Mutex
	upstreamMu sync.RWMutex
	seq        atomic.Int64
	pending    map[string]*pendingCall
}

func New(opts Options) *Proxy {
	p := &Proxy{
		opts:          opts,
		clientID:      opts.Config.Server.ClientID,
		auditWrites:   opts.Config.Audit.Enabled,
		pending:       make(map[string]*pendingCall),
		upstreamReady: make(chan struct{}),
	}
	// Config guarantees a positive default; guard anyway against a 0 that would
	// make the first-connect wait instant.
	uct := opts.Config.Server.UpstreamConnectTimeoutMs
	if uct <= 0 {
		uct = 30000
	}
	p.firstUpstreamConnectWait = time.Duration(uct) * time.Millisecond

	// One store backs both call records and masking rules, so the console can
	// edit rules without a second connection (or a second SQLite file lock).
	if opts.Config.Audit.Enabled || opts.Config.Masking.Enabled {
		store, err := audit.NewStore(opts.Config.Audit.Driver, opts.Config.Audit.DSN)
		if err != nil {
			log.Printf("warn: store init failed: %v", err)
		} else {
			p.auditStore = store
		}
	}

	if opts.Config.Masking.Enabled {
		m, err := mask.New(nil)
		if err != nil {
			log.Printf("warn: masker init failed: %v", err)
		} else {
			p.masker = m
			p.seedConfigRules()
			if err := p.reloadRules(); err != nil {
				log.Printf("warn: rule load failed, using config rules: %v", err)
				_ = m.Update(p.configSpecs())
			}
			p.initDetector(m)
		}
	}

	p.limiter = ratelimit.NewTokenBucketManager(
		opts.Config.RateLimit.QPS,
		opts.Config.RateLimit.DailyQuota,
		opts.Config.RateLimit.Enabled,
	)
	return p
}

// Run wires up the client and upstream transports and pumps messages between
// them, applying the interception chain (rate limit, mask, audit) on the way.
// It blocks until the client transport closes or a SIGINT/SIGTERM is received.
func (p *Proxy) Run() error {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return p.run(sigCtx)
}

// run is the shared implementation behind Run.
func (p *Proxy) run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Audit writes are offloaded to a buffered channel + background worker so a
	// slow or hung database can never stall the response path.
	var auditDone chan struct{}
	if p.auditStore != nil && p.auditWrites {
		p.auditCh = make(chan *audit.CallRecord, p.auditQueueSize())
		auditDone = make(chan struct{})
		go p.auditWorker(ctx, auditDone)
	}

	// Audit retention: trim old/excess records at startup and periodically.
	// Best-effort and fail-open — a prune failure never blocks or aborts the proxy.
	p.startRetention(ctx)

	// --- upstream transport factory ---
	// The upstream may die at any time; upstreamSupervisor recreates it on every
	// failure, so we keep a factory rather than a single live connection.
	var upstreamFactory func() (transport.UpstreamTransporter, error)
	switch p.opts.Config.Transport.Upstream {
	case "sse":
		if p.opts.Config.Transport.UpstreamURL == "" {
			return errors.New("transport.upstream_url is required when upstream = sse")
		}
		url := p.opts.Config.Transport.UpstreamURL
		upstreamFactory = func() (transport.UpstreamTransporter, error) {
			return transport.NewSSEUpstream(url), nil
		}
	case "streamable-http":
		if p.opts.Config.Transport.UpstreamURL == "" {
			return errors.New("transport.upstream_url is required when upstream = streamable-http")
		}
		url := p.opts.Config.Transport.UpstreamURL
		upstreamFactory = func() (transport.UpstreamTransporter, error) {
			return transport.NewStreamableHTTPUpstream(url), nil
		}
	default: // stdio
		if len(p.opts.UpstreamCmd) == 0 {
			return errors.New("no upstream command provided")
		}
		cmd := p.opts.UpstreamCmd
		upstreamFactory = func() (transport.UpstreamTransporter, error) {
			return transport.NewStdioUpstream(cmd, p.opts.ConfigDir)
		}
	}

	// --- client transport ---
	var client transport.ClientTransporter
	switch p.opts.Config.Transport.Client {
	case "sse":
		client = transport.NewSSEServer(p.opts.Config.Transport.Listen)
		log.Printf("mcp-arc: SSE client transport listening on %s", p.opts.Config.Transport.Listen)
	case "streamable-http":
		client = transport.NewStreamableHTTPClient(p.opts.Config.Transport.Listen, p.opts.Config.Transport.StreamableHTTPPath)
		log.Printf("mcp-arc: Streamable HTTP client transport listening on %s%s", p.opts.Config.Transport.Listen, p.opts.Config.Transport.StreamableHTTPPath)
	default: // stdio
		client = transport.StdioClient{}
	}

	// broadcast target for upstream-initiated notifications
	switch c := client.(type) {
	case *transport.SSEServer:
		p.clientBroadcast = c.Broadcast
	case *transport.StreamableHTTPClient:
		p.clientBroadcast = c.Broadcast
	default:
		p.clientBroadcast = transport.StdioClient{}.Broadcast
	}

	if p.opts.Config.Admin.Enabled {
		go func() {
			srv := admin.New(p.auditStore, p.opts.Config.Admin.Token, p, p)
			srv.Status = admin.Status{
				ClientTransport: p.opts.Config.Transport.Client,
				SSEURL:          p.opts.SSEURL,
				ConsoleURL:      p.opts.ConsoleURL,
				AdminPort:       p.opts.Config.Admin.Port,
				PID:             os.Getpid(),
				StartedAt:       procStartedAt.Format(time.RFC3339),
				AuditDriver:     p.opts.Config.Audit.Driver,
				ConfigDir:       p.opts.Config.ConfigDir,
			}
			var e error
			if p.opts.AdminListener != nil {
				e = srv.StartListener(p.opts.AdminListener)
			} else {
				e = srv.Start(p.opts.Config.Admin.Port)
			}
			if e != nil {
				log.Printf("warn: admin server stopped: %v", e)
			}
		}()
	}

	// Keep the upstream alive: reconnect/restart it on any failure instead of
	// taking the whole proxy down with it.
	go p.upstreamSupervisor(ctx, upstreamFactory)

	errCh := make(chan error, 1)
	go func() {
		if e := client.Run(ctx, func(raw []byte, respond func([]byte, bool) error) func() {
			return p.handleClientMessage(raw, respond)
		}); e != nil {
			errCh <- e
		} else {
			errCh <- fmt.Errorf("client closed")
		}
	}()

	log.Printf("mcp-arc: running (client=%s, upstream=%s)", p.opts.Config.Transport.Client, p.opts.Config.Transport.Upstream)
	// Reap requests the upstream never answered, so a dead or hung upstream
	// cannot grow p.pending without bound.
	go p.sweepPending(ctx)

	var err error
	select {
	case err = <-errCh:
		log.Printf("mcp-arc: stopping (client transport: %v)", err)
	case <-ctx.Done():
		log.Printf("mcp-arc: stopping (signal received)")
	}
	cancel()
	// Fail any in-flight requests up front so connected clients get a clear error
	// instead of hanging until their own timeout while we tear the proxy down.
	p.failPendingUpstreamDown()
	_ = client.Close()
	// Flush the audit queue so buffered records are not lost on shutdown. The
	// worker drains under a bounded deadline; we wait for it (with our own
	// timeout guard) before closing the store so nothing is dropped mid-write.
	if auditDone != nil {
		waitAudit := make(chan struct{})
		go func() { <-auditDone; close(waitAudit) }()
		select {
		case <-waitAudit:
		case <-time.After(p.auditShutdownTimeout()):
			log.Printf("warn: audit flush timed out; closing store anyway")
		}
	}
	if p.auditStore != nil {
		_ = p.auditStore.Close()
	}
	return nil
}

// handleClientMessage processes one client frame (rate limit, mask, audit, id
// rewrite, forward) and returns a cleanup function. The client transport invokes
// cleanup when this client connection closes (disconnect or stream completion).
// On a real request the cleanup cancels the in-flight upstream request so a
// client that walks away mid-call does not leave the upstream server hanging
// (T5: cancellation propagation).
//
// The upstream write is performed asynchronously: it can block for the full
// duration of a long-running tool, and we must return promptly so the client
// transport can observe a client disconnect (and invoke cleanup) instead of
// being stuck behind the in-flight upstream request.
func (p *Proxy) handleClientMessage(raw []byte, respond func([]byte, bool) error) func() {
	forward, synthetic := p.processClientMessage(raw, respond)
	if synthetic != nil {
		_ = respond(synthetic, true)
		return func() {}
	}

	// Recover the gateway-unique correlation id from the forwarded frame so we can
	// cancel the matching upstream request if the client disconnects. A nil/empty
	// id means this was a notification or a failed-rate-limit reply — nothing to
	// cancel.
	upID := ""
	if id, ok := extractMsgID(forward); ok {
		var s string
		if err := json.Unmarshal(id, &s); err == nil {
			upID = s
		}
	}
	cleanup := func() {
		if upID == "" {
			return
		}
		p.cancelUpstream(upID)
		p.dropPending(upID)
	}

	if forward != nil {
		go func() {
			if err := p.writeUpstream(forward); err != nil {
				log.Printf("warn: upstream write failed: %v", err)
				// Tell the client exactly what went wrong instead of leaving it to
				// hang until the 5-minute sweep.
				if id, ok := extractMsgID(raw); ok {
					_ = respond(jsonRPCError(id, upstreamWriteFailedCode, "upstream write failed: "+err.Error()), true)
				}
				// The request never left the proxy, so drop its correlation entry
				// rather than let it linger for the sweep and double-respond later.
				if key, ok := pendingKey(forward); ok {
					p.dropPending(key)
				}
			}
		}()
	}
	return cleanup
}

// Replay re-issues a previously recorded tools/call to the upstream server using
// the original (unmasked) request parameters, and returns the upstream's raw
// response. It rides the same pending/id-rewrite machinery as a live call so the
// response is correlated and audited normally. This is intentionally decoupled
// from MCP semantics: it only knows the "tools/call" method name and the params
// shape — it does not parse or depend on the protocol internals.
func (p *Proxy) Replay(ctx context.Context, toolName string, rawParams []byte) ([]byte, error) {
	if p.getUpstreamWriter() == nil {
		return nil, errors.New("replay unavailable: upstream transport not connected")
	}
	// Numbers stay in their literal form so a replayed call carries byte-identical
	// arguments to the original (see decodeJSONObject).
	args, err := decodeJSONObject(rawParams)
	if err != nil || args == nil {
		args = map[string]any{}
	}
	upID := fmt.Sprintf("gw-%d", p.seq.Add(1))
	ch := make(chan []byte, 1)
	now := time.Now()
	pc := &pendingCall{
		toolName:  toolName,
		start:     now,
		deadline:  now.Add(pendingTTL),
		respond:   func(b []byte, _ bool) error { ch <- b; return nil },
		origIDRaw: json.RawMessage(fmt.Sprintf("%q", upID)),
	}
	if p.masker != nil {
		if m, err := p.masker.Mask(args); err == nil && m != nil {
			if mb, err := json.Marshal(m); err == nil {
				pc.maskedParams = string(mb)
			}
		}
	}
	pc.rawParams = string(rawParams)
	p.mu.Lock()
	p.pending[upID] = pc
	p.mu.Unlock()
	// Replay is synchronous: once it returns, nobody is left reading ch, so the
	// entry is dead weight until the sweeper would find it.
	defer p.dropPending(upID)

	b, err := newToolCallRequest(upID, toolName, args)
	if err != nil {
		return nil, err
	}
	if err := p.writeUpstream(b); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		return nil, errors.New("replay timeout")
	}
}

// retentionPruner is implemented by the concrete audit stores that support
// pruning. It is matched via type assertion so the Store interface (and the test
// fakes that implement it) stay untouched.
type retentionPruner interface {
	Prune(audit.Retention) (int64, error)
}

// startRetention trims audit records at startup and then on a fixed interval
// according to the configured retention policy. It is a no-op when no policy is
// set or the active store does not support pruning. Prune failures are logged and
// never block the request path or abort shutdown.
func (p *Proxy) startRetention(ctx context.Context) {
	cfg := p.opts.Config.Audit.Retention
	if cfg.MaxAgeDays <= 0 && cfg.MaxRows <= 0 {
		return
	}
	pruner, ok := p.auditStore.(retentionPruner)
	if !ok {
		return
	}
	policy := audit.Retention{MaxAgeDays: cfg.MaxAgeDays, MaxRows: cfg.MaxRows}
	if n, err := pruner.Prune(policy); err != nil {
		log.Printf("warn: initial audit prune failed: %v", err)
	} else if n > 0 {
		log.Printf("audit: pruned %d records at startup", n)
	}
	go func() {
		ticker := time.NewTicker(retentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := pruner.Prune(policy); err != nil {
					log.Printf("warn: audit prune failed: %v", err)
				} else if n > 0 {
					log.Printf("audit: pruned %d records", n)
				}
			}
		}
	}()
}

// retentionInterval is how often the background audit pruner runs.
const retentionInterval = time.Hour
