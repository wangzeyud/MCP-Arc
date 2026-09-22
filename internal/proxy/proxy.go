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
	"github.com/wangzeyud/mcp-arc/internal/alerting"
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
	Config      *config.Config // initial snapshot (tests / fallback)
	ConfigMgr   *config.Manager // hot-reloadable source of truth (v0.7)
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
	alertSender *alerting.Sender
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

	// restartFP records the restart fingerprint of the last applied config so a
	// reload that touches restart-required fields can be flagged.
	restartFP string
}

func New(opts Options) *Proxy {
	cfg := opts.Config
	if opts.ConfigMgr != nil {
		cfg = opts.ConfigMgr.Get()
	}
	p := &Proxy{
		opts:          opts,
		clientID:      cfg.Server.ClientID,
		auditWrites:   cfg.Audit.Enabled,
		pending:       make(map[string]*pendingCall),
		upstreamReady: make(chan struct{}),
	}
	// Config guarantees a positive default; guard anyway against a 0 that would
	// make the first-connect wait instant.
	uct := cfg.Server.UpstreamConnectTimeoutMs
	if uct <= 0 {
		uct = 30000
	}
	p.firstUpstreamConnectWait = time.Duration(uct) * time.Millisecond

	// One store backs both call records and masking rules, so the console can
	// edit rules without a second connection (or a second SQLite file lock).
	if cfg.Audit.Enabled || cfg.Masking.Enabled {
		store, err := audit.NewStore(cfg.Audit.Driver, cfg.Audit.DSN)
		if err != nil {
			log.Printf("warn: store init failed: %v", err)
		} else {
			p.auditStore = store
		}
	}

	if cfg.Masking.Enabled {
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
			m.SetDetectHook(func(findings []mask.Finding) {
				if p.alertSender == nil {
					return
				}
				items := make([]map[string]string, 0, len(findings))
				for _, f := range findings {
					items = append(items, map[string]string{"path": f.Path, "type": f.Type})
				}
				p.alertSender.Send(alerting.EventSensitiveDetected, map[string]any{
					"findings": items,
				})
			})
		}
	}

	p.limiter = ratelimit.NewTokenBucketManager(
		cfg.RateLimit.QPS,
		cfg.RateLimit.DailyQuota,
		cfg.RateLimit.Enabled,
	)
	p.alertSender = alerting.New(cfg.Alerting)
	// Config hot-reload (v0.7): re-apply live config to the rate limiter, masker
	// and retention worker whenever the file changes (watcher or POST /api/config/reload).
	if p.opts.ConfigMgr != nil {
		p.restartFP = config.RestartFingerprint(p.cfg())
		p.opts.ConfigMgr.OnReload(p.onConfigReload)
	}
	return p
}

// cfg returns the active configuration, preferring the hot-reloadable Manager when
// present and falling back to the initial snapshot otherwise.
func (p *Proxy) cfg() *config.Config {
	if p.opts.ConfigMgr != nil {
		return p.opts.ConfigMgr.Get()
	}
	return p.opts.Config
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

	// Config hot-reload poller (v0.7): watches the config file and swaps in new
	// values without a restart. Stops with the process.
	if p.opts.ConfigMgr != nil {
		p.opts.ConfigMgr.Start(ctx)
	}

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
	switch p.cfg().Transport.Upstream {
	case "sse":
		if p.cfg().Transport.UpstreamURL == "" {
			return errors.New("transport.upstream_url is required when upstream = sse")
		}
		url := p.cfg().Transport.UpstreamURL
		upstreamFactory = func() (transport.UpstreamTransporter, error) {
			return transport.NewSSEUpstream(url), nil
		}
	case "streamable-http":
		if p.cfg().Transport.UpstreamURL == "" {
			return errors.New("transport.upstream_url is required when upstream = streamable-http")
		}
		url := p.cfg().Transport.UpstreamURL
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
	switch p.cfg().Transport.Client {
	case "sse":
		client = transport.NewSSEServer(p.cfg().Transport.Listen)
		log.Printf("mcp-arc: SSE client transport listening on %s", p.cfg().Transport.Listen)
	case "streamable-http":
		client = transport.NewStreamableHTTPClient(p.cfg().Transport.Listen, p.cfg().Transport.StreamableHTTPPath)
		log.Printf("mcp-arc: Streamable HTTP client transport listening on %s%s", p.cfg().Transport.Listen, p.cfg().Transport.StreamableHTTPPath)
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

	if p.cfg().Admin.Enabled {
		go func() {
			srv := admin.New(p.auditStore, p.cfg().Admin.Token, p, p, p)
			srv.Status = admin.Status{
				ClientTransport: p.cfg().Transport.Client,
				SSEURL:          p.opts.SSEURL,
				ConsoleURL:      p.opts.ConsoleURL,
				AdminPort:       p.cfg().Admin.Port,
				PID:             os.Getpid(),
				StartedAt:       procStartedAt.Format(time.RFC3339),
				AuditDriver:     p.cfg().Audit.Driver,
				ConfigDir:       p.cfg().ConfigDir,
			}
			var e error
			if p.opts.AdminListener != nil {
				e = srv.StartListener(p.opts.AdminListener)
			} else {
				e = srv.Start(p.cfg().Admin.Port)
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

	log.Printf("mcp-arc: running (client=%s, upstream=%s)", p.cfg().Transport.Client, p.cfg().Transport.Upstream)
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
// never block the request path or abort shutdown. The policy is read live from the
// active config each pass, so retention changes apply without a restart (v0.7).
func (p *Proxy) startRetention(ctx context.Context) {
	if p.auditStore == nil {
		return
	}
	p.pruneNow()
	go func() {
		ticker := time.NewTicker(retentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.pruneNow()
			}
		}
	}()
}

// pruneNow trims audit records according to the currently active retention policy.
// No-op when no policy is set or the store does not support pruning.
func (p *Proxy) pruneNow() {
	if p.auditStore == nil {
		return
	}
	cfg := p.cfg().Audit.Retention
	if cfg.MaxAgeDays <= 0 && cfg.MaxRows <= 0 {
		return
	}
	pruner, ok := p.auditStore.(retentionPruner)
	if !ok {
		return
	}
	policy := audit.Retention{MaxAgeDays: cfg.MaxAgeDays, MaxRows: cfg.MaxRows}
	if n, err := pruner.Prune(policy); err != nil {
		log.Printf("warn: audit prune failed: %v", err)
	} else if n > 0 {
		log.Printf("audit: pruned %d records", n)
	}
}

// onConfigReload applies a freshly loaded configuration to the live components. It
// is invoked by config.Manager.Reload (file watch or POST /api/config/reload).
func (p *Proxy) onConfigReload(c *config.Config) {
	p.limiter.Reload(c.RateLimit.QPS, c.RateLimit.DailyQuota, c.RateLimit.Enabled)
	if c.Masking.Enabled && p.masker != nil {
		if err := p.reloadRules(); err != nil {
			log.Printf("warn: masker reload on config change failed: %v", err)
		}
	}
	p.pruneNow()

	if p.alertSender != nil {
		p.alertSender.Reload(c.Alerting)
	}
	restartNeeded := false
	fp := config.RestartFingerprint(c)
	if p.restartFP != "" && fp != p.restartFP {
		restartNeeded = true
		log.Printf("warn: config changed in fields that require a restart (transport / admin port / audit driver or DSN / upstream); restart mcp-arc to apply")
	}
	p.restartFP = fp

	if p.alertSender != nil {
		if restartNeeded {
			p.alertSender.Send(alerting.EventRestartRequired, map[string]any{
				"note": "restart-required fields changed; restart mcp-arc to apply",
			})
		} else {
			p.alertSender.Send(alerting.EventConfigReloaded, map[string]any{
				"rate_limit_qps": c.RateLimit.QPS,
			})
		}
	}
}

// ReloadConfig triggers a runtime reload of the configuration file. It is exposed to
// the console via POST /api/config/reload.
func (p *Proxy) ReloadConfig() error {
	if p.opts.ConfigMgr == nil {
		return errors.New("config manager unavailable")
	}
	return p.opts.ConfigMgr.Reload()
}

// retentionInterval is how often the background audit pruner runs.
const retentionInterval = time.Hour
