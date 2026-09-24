package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
	"github.com/wangzeyud/mcp-arc/internal/ratelimit"
	"github.com/wangzeyud/mcp-arc/internal/transport"
	"gopkg.in/yaml.v3"
)

// soak flags turn TestProxySoakStability into a configurable long-run harness
// (v0.8). With no flags it reproduces the original v0.4 fast gate: 20000 fixed
// rounds, no reload, no fault injection — so `go test ./...` keeps its quick
// stability check.
//
// Configure via flags (where `go test` forwards them) OR environment variables
// (always reliable):
//
//	# 7×24 run with hot-reload churn + a memory ceiling
//	go test -run TestProxySoakStability ./internal/proxy/ \
//	  -soak.dur=24h -soak.reload -soak.reloadEvery=30s -soak.memLimitMB=128
//	# equivalent, via env (works even when go test does not forward custom flags)
//	SOAK_DUR=24h SOAK_RELOAD=1 SOAK_RELOAD_EVERY=30s SOAK_MEM_LIMIT_MB=128 \
//	  go test -run TestProxySoakStability ./internal/proxy/
//
// Pair with `-race` to also surface concurrency bugs, since the client and
// upstream legs operate as independent concurrent streams in production.
var (
	soakDur          = flag.Duration("soak.dur", 0, "soak run duration; 0 = fixed-rounds mode (-soak.rounds)")
	soakRounds       = flag.Int("soak.rounds", 20000, "rounds driven when -soak.dur==0")
	soakReload       = flag.Bool("soak.reload", false, "enable the config hot-reload churn loop during the soak")
	soakReloadEvery  = flag.Duration("soak.reloadEvery", 0, "config reload interval when -soak.reload (default 5s)")
	soakFailRate     = flag.Float64("soak.failRate", 0, "fraction of calls answered with an injected upstream error (0..1)")
	soakMaxGorGrowth = flag.Int("soak.maxGoroutineGrowth", 0, "allowed goroutine growth over the settled baseline (default 10)")
	soakMemLimitMB   = flag.Int("soak.memLimitMB", 0, "if >0, fail when max heap alloc exceeds baseline+this MB (0 = report only)")
	soakSampleEvery  = flag.Duration("soak.sampleEvery", 0, "memory/goroutine sample interval (0 = auto)")
)

// effSoakCfg is the resolved soak configuration: flags win, environment
// variables fill in anything the flag left at its zero value (so the env form
// is a reliable fallback when `go test` does not forward custom flags).
type effSoakCfg struct {
	dur          time.Duration
	rounds       int
	reload       bool
	reloadEvery  time.Duration
	failRate     float64
	maxGorGrowth int
	memLimitMB   int
	sampleEvery  time.Duration
}

func resolveSoakCfg() effSoakCfg {
	c := effSoakCfg{
		dur:          *soakDur,
		rounds:       *soakRounds,
		reload:       *soakReload,
		reloadEvery:  *soakReloadEvery,
		failRate:     *soakFailRate,
		maxGorGrowth: *soakMaxGorGrowth,
		memLimitMB:   *soakMemLimitMB,
		sampleEvery:  *soakSampleEvery,
	}
	if s := os.Getenv("SOAK_DUR"); s != "" && c.dur == 0 {
		if d, err := time.ParseDuration(s); err == nil {
			c.dur = d
		}
	}
	if os.Getenv("SOAK_RELOAD") == "1" {
		c.reload = true
	}
	if s := os.Getenv("SOAK_RELOAD_EVERY"); s != "" && c.reloadEvery == 0 {
		if d, err := time.ParseDuration(s); err == nil {
			c.reloadEvery = d
		}
	}
	if s := os.Getenv("SOAK_FAIL_RATE"); s != "" && c.failRate == 0 {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			c.failRate = f
		}
	}
	if s := os.Getenv("SOAK_MAX_GOROUTINE_GROWTH"); s != "" && c.maxGorGrowth == 0 {
		if n, err := strconv.Atoi(s); err == nil {
			c.maxGorGrowth = n
		}
	}
	if s := os.Getenv("SOAK_MEM_LIMIT_MB"); s != "" && c.memLimitMB == 0 {
		if n, err := strconv.Atoi(s); err == nil {
			c.memLimitMB = n
		}
	}
	if s := os.Getenv("SOAK_SAMPLE_EVERY"); s != "" && c.sampleEvery == 0 {
		if d, err := time.ParseDuration(s); err == nil {
			c.sampleEvery = d
		}
	}
	// Internal defaults for flags whose zero value is meaningful (so env vars can
	// override them via the zero-value guard above).
	if c.reloadEvery == 0 {
		c.reloadEvery = 5 * time.Second
	}
	if c.maxGorGrowth == 0 {
		c.maxGorGrowth = 10
	}
	return c
}

// soakCountingStore is a lightweight audit.Store for the soak: it counts inserts
// without retaining them, so a 24h run neither OOMs on audit records nor
// conflates test-harness memory with proxy leaks. It embeds fakeRuleStore for
// the rule methods the masking code touches.
type soakCountingStore struct {
	fakeRuleStore
	total int64
}

// Insert records one audited call without keeping it.
func (s *soakCountingStore) Insert(*audit.CallRecord) error {
	atomic.AddInt64(&s.total, 1)
	return nil
}

// noopRespond discards client-bound messages; the soak never inspects them, and
// retaining them would grow unbounded over a long run.
var noopRespond = func([]byte, bool) error { return nil }

// TestProxySoakStability is the soak gate (v0.4+), now configurable (v0.8). It
// drives many tool-call round trips through the full interception chain
// (mask -> forward -> audit -> correlate -> respond) and asserts the proxy stays
// correct and leak-free under duration, hot-reload churn, and fault injection.
func TestProxySoakStability(t *testing.T) {
	cfg := resolveSoakCfg()
	t.Logf("soak cfg: dur=%v rounds=%d reload=%v reloadEvery=%v failRate=%v maxGorGrowth=%d memLimitMB=%d sampleEvery=%v",
		cfg.dur, cfg.rounds, cfg.reload, cfg.reloadEvery, cfg.failRate, cfg.maxGorGrowth, cfg.memLimitMB, cfg.sampleEvery)
	if cfg.failRate < 0 || cfg.failRate > 1 {
		t.Fatalf("failRate must be in [0,1], got %v", cfg.failRate)
	}

	// Pick the proxy builder: reload mode wires a file-backed config.Manager so
	// the hot-reload path (onConfigReload) is actually exercised; otherwise we
	// keep the original hand-rolled proxy with no manager (identical to v0.4).
	var (
		p        *Proxy
		store    *soakCountingStore
		reloadFn func(int64) error
	)
	if cfg.reload {
		p, store, reloadFn = newTestProxyWithReload(t)
	} else {
		p, store = newSoakProxy()
	}

	// Settled baseline goroutine count and heap size (no in-flight work yet).
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	baseAlloc := ms0.Alloc

	// --- background loops -------------------------------------------------
	stop := make(chan struct{})
	var wg sync.WaitGroup

	var reloads int64
	if cfg.reload {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.reloadEvery)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					n := atomic.AddInt64(&reloads, 1)
					if err := reloadFn(1000 + n%50); err != nil {
						t.Errorf("soak reload %d failed: %v", n, err)
					}
				}
			}
		}()
	}

	sampleEvery := cfg.sampleEvery
	switch {
	case sampleEvery > 0:
		// explicit override
	case cfg.dur > 0:
		sampleEvery = time.Second
	default:
		sampleEvery = 20 * time.Millisecond
	}
	var maxG int32 = int32(base)
	var maxAlloc uint64 = baseAlloc
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(sampleEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g := int32(runtime.NumGoroutine())
				if g > atomic.LoadInt32(&maxG) {
					atomic.StoreInt32(&maxG, g)
				}
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				for {
					cur := atomic.LoadUint64(&maxAlloc)
					if ms.Alloc <= cur {
						break
					}
					if atomic.CompareAndSwapUint64(&maxAlloc, cur, ms.Alloc) {
						break
					}
				}
			}
		}
	}()

	// --- drive the soak ----------------------------------------------------
	var calls int64
	deadline := time.Now().Add(cfg.dur)
	faultEvery := int64(cfg.failRate * 100)
	for i := int64(1); ; i++ {
		if cfg.dur > 0 {
			if time.Now().After(deadline) {
				break
			}
		} else if i > int64(cfg.rounds) {
			break
		}
		req := []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"pay","arguments":{"orderId":1234567890123456789,"note":"soak-%d"}}}`,
			i, i))
		forward, synthetic := p.processClientMessage(req, noopRespond)
		if synthetic != nil {
			t.Fatalf("call %d: unexpected synthetic response: %s", i, synthetic)
		}
		if forward == nil {
			t.Fatalf("call %d: not forwarded to upstream", i)
		}
		gwID := jsonID(t, forward)
		var resp []byte
		if faultEvery > 0 && i%100 < faultEvery {
			resp = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-32099,"message":"injected fault"}}`, gwID))
		} else {
			resp = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"ok":true}}`, gwID))
		}
		if err := p.processUpstreamMessage(resp); err != nil {
			t.Fatalf("call %d: upstream error: %v", i, err)
		}
		calls++
	}

	close(stop)
	wg.Wait()

	// Let any trailing async work settle before measuring.
	time.Sleep(200 * time.Millisecond)

	if n := pendingCount(p); n != 0 {
		t.Errorf("pending = %d after soak, want 0 (leaked correlation state)", n)
	}
	if want := int(calls); int(atomic.LoadInt64(&store.total)) != want {
		t.Errorf("audit records = %d, want %d", atomic.LoadInt64(&store.total), want)
	}
	if got := runtime.NumGoroutine(); got > base+cfg.maxGorGrowth {
		t.Errorf("goroutines = %d, base = %d (growth %d > limit %d, possible goroutine leak)", got, base, got-base, cfg.maxGorGrowth)
	}
	maxAllocFinal := atomic.LoadUint64(&maxAlloc)
	if cfg.memLimitMB > 0 {
		limit := uint64(cfg.memLimitMB) * 1024 * 1024
		if maxAllocFinal-baseAlloc > limit {
			t.Errorf("max heap alloc grew %d MB (baseline %d MB) > limit %d MB (possible memory leak)",
				(maxAllocFinal-baseAlloc)/1024/1024, baseAlloc/1024/1024, cfg.memLimitMB)
		}
	}
	t.Logf("soak done: calls=%d reloads=%d maxGoroutines=%d (base %d) maxHeapMB=%d (base %d)",
		calls, atomic.LoadInt64(&reloads), atomic.LoadInt32(&maxG), base,
		maxAllocFinal/1024/1024, baseAlloc/1024/1024)
}

// TestProxySoakStreamableHTTP is the Streamable HTTP long-connection soak variant
// (v0.8). Unlike TestProxySoakStability, which drives the interception chain
// directly, this wires the REAL transport objects exactly like proxy.run — a
// StreamableHTTPClient serving the client on a port and a StreamableHTTPUpstream
// forwarding to a mock upstream MCP server — and drives a real keep-alive HTTP
// client against the proxy for the whole soak window. This exercises the transport
// layer (HTTP server, per-request SSE streams, TCP connection reuse, request
// correlation) that the direct variant cannot reach. It reuses resolveSoakCfg, so
// all the same knobs (duration, fault injection, reload churn, goroutine/heap
// ceilings) apply, plus the soakCountingStore so long runs do not retain audit
// records. The mock upstream counts every forwarded request so we can assert no
// silent drops.
func TestProxySoakStreamableHTTP(t *testing.T) {
	cfg := resolveSoakCfg()
	t.Logf("soak[http] cfg: dur=%v rounds=%d reload=%v reloadEvery=%v failRate=%v maxGorGrowth=%d memLimitMB=%d sampleEvery=%v",
		cfg.dur, cfg.rounds, cfg.reload, cfg.reloadEvery, cfg.failRate, cfg.maxGorGrowth, cfg.memLimitMB, cfg.sampleEvery)
	if cfg.failRate < 0 || cfg.failRate > 1 {
		t.Fatalf("failRate must be in [0,1], got %v", cfg.failRate)
	}

	// Mock upstream MCP server speaking Streamable HTTP.
	var upstreamHits int64
	var reqSeq int64
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
		atomic.AddInt64(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		faultEvery := int64(cfg.failRate * 100)
		n := atomic.AddInt64(&reqSeq, 1)
		if faultEvery > 0 && n%100 < faultEvery {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32099,"message":"injected fault"}}`, m.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, m.ID)
	}))
	defer upstreamSrv.Close()

	// Build the proxy: reload mode wires a file-backed config.Manager so the
	// hot-reload path is exercised; otherwise the hand-rolled proxy.
	var (
		p        *Proxy
		store    *soakCountingStore
		reloadFn func(int64) error
	)
	if cfg.reload {
		p, store, reloadFn = newTestProxyWithReload(t)
	} else {
		p, store = newSoakProxy()
	}

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

	// Wait for the client transport to be listening (TCP probe; mirrors the e2e
	// cancel test so we don't pollute the audit count with a warmup POST).
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

	// Settled baseline goroutine count and heap size (no in-flight work yet).
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	baseAlloc := ms0.Alloc

	// --- background loops -------------------------------------------------
	stop := make(chan struct{})
	var wg sync.WaitGroup

	var reloads int64
	if cfg.reload {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(cfg.reloadEvery)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					n := atomic.AddInt64(&reloads, 1)
					if err := reloadFn(1000 + n%50); err != nil {
						t.Errorf("soak reload %d failed: %v", n, err)
					}
				}
			}
		}()
	}

	sampleEvery := cfg.sampleEvery
	switch {
	case sampleEvery > 0:
		// explicit override
	case cfg.dur > 0:
		sampleEvery = time.Second
	default:
		sampleEvery = 20 * time.Millisecond
	}
	var maxG int32 = int32(base)
	var maxAlloc uint64 = baseAlloc
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(sampleEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g := int32(runtime.NumGoroutine())
				if g > atomic.LoadInt32(&maxG) {
					atomic.StoreInt32(&maxG, g)
				}
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				for {
					cur := atomic.LoadUint64(&maxAlloc)
					if ms.Alloc <= cur {
						break
					}
					if atomic.CompareAndSwapUint64(&maxAlloc, cur, ms.Alloc) {
						break
					}
				}
			}
		}
	}()

	// --- drive the soak over a real keep-alive HTTP client ----------------
	hc := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			DisableKeepAlives:   false,
		},
	}
	var calls int64
	deadline := time.Now().Add(cfg.dur)
	for i := int64(1); ; i++ {
		if cfg.dur > 0 {
			if time.Now().After(deadline) {
				break
			}
		} else if i > int64(cfg.rounds) {
			break
		}
		reqBody := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"pay","arguments":{"orderId":1234567890123456789,"note":"soak-%d"}}}`,
			i, i)
		reqCtx, reqCancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, clientURL, bytes.NewReader([]byte(reqBody)))
		if err != nil {
			reqCancel()
			t.Fatalf("call %d: build request: %v", i, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			reqCancel()
			t.Fatalf("call %d: http: %v", i, err)
		}
		// Consume the full SSE body so the connection returns to the idle pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		reqCancel()
		calls++
	}

	close(stop)
	wg.Wait()

	// Let any trailing async work settle before measuring.
	time.Sleep(200 * time.Millisecond)

	if n := pendingCount(p); n != 0 {
		t.Errorf("pending = %d after soak, want 0 (leaked correlation state)", n)
	}
	if want := int(calls); int(atomic.LoadInt64(&store.total)) != want {
		t.Errorf("audit records = %d, want %d", atomic.LoadInt64(&store.total), want)
	}
	if got := int(atomic.LoadInt64(&upstreamHits)); got != int(calls) {
		t.Errorf("upstream received %d requests, want %d (silent drop?)", got, int(calls))
	}
	if got := runtime.NumGoroutine(); got > base+cfg.maxGorGrowth {
		t.Errorf("goroutines = %d, base = %d (growth %d > limit %d, possible goroutine leak)", got, base, got-base, cfg.maxGorGrowth)
	}
	maxAllocFinal := atomic.LoadUint64(&maxAlloc)
	if cfg.memLimitMB > 0 {
		limit := uint64(cfg.memLimitMB) * 1024 * 1024
		if maxAllocFinal-baseAlloc > limit {
			t.Errorf("max heap alloc grew %d MB (baseline %d MB) > limit %d MB (possible memory leak)",
				(maxAllocFinal-baseAlloc)/1024/1024, baseAlloc/1024/1024, cfg.memLimitMB)
		}
	}
	t.Logf("soak[http] done: calls=%d upstreamHits=%d reloads=%d maxGoroutines=%d (base %d) maxHeapMB=%d (base %d)",
		calls, atomic.LoadInt64(&upstreamHits), atomic.LoadInt64(&reloads), atomic.LoadInt32(&maxG), base,
		maxAllocFinal/1024/1024, baseAlloc/1024/1024)
}

// newSoakProxy builds the hand-rolled proxy used by the default (no-reload) soak
// gate. It is identical in spirit to the v0.4 newTestProxy but uses a
// soakCountingStore so long runs do not retain audit records.
func newSoakProxy() (*Proxy, *soakCountingStore) {
	store := &soakCountingStore{}
	p := &Proxy{
		pending:         map[string]*pendingCall{},
		limiter:         ratelimit.NewTokenBucketManager(0, 0, false),
		auditWrites:     true,
		auditStore:      store,
		clientID:        "test-client",
		clientBroadcast: func([]byte) error { return nil },
	}
	return p, store
}

// newTestProxyWithReload builds a proxy whose config is backed by a temp-file
// config.Manager, so the hot-reload path is exercised. It returns a reloadFn
// that rewrites the config (mutating QPS to prove the limiter actually
// reconfigures) and triggers ReloadConfig. The audit store is overridden with a
// soakCountingStore so no real DB is touched and record counts stay assertable.
func newTestProxyWithReload(t *testing.T) (*Proxy, *soakCountingStore, func(int64) error) {
	t.Helper()
	def := config.Default()
	def.Audit.Enabled = false    // replaced by a soakCountingStore below; avoid a real DB
	def.Masking.Enabled = true   // exercise masker reload on hot-reload
	def.RateLimit.Enabled = false
	path := filepath.Join(t.TempDir(), "soak.yaml")
	writeCfgToFile(t, path, def, int64(def.RateLimit.QPS))
	// Normalize through the same Load/applyDefaults path the file watcher uses,
	// so the initial fingerprint matches the first reload (no spurious restart).
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	mgr := config.NewWatcherFromFile(path, cfg)
	p := New(Options{Config: cfg, ConfigMgr: mgr})

	// New() opened a real audit store (masking enabled). Close it so the temp
	// sqlite file is released before t.TempDir cleanup; audits are driven through
	// the lightweight soakCountingStore below.
	if err := p.auditStore.Close(); err != nil {
		t.Logf("closing seed audit store: %v", err)
	}

	store := &soakCountingStore{}
	p.auditStore = store // override the store New() may have opened
	p.auditWrites = true
	p.clientID = "test-client"

	reloadFn := func(qps int64) error {
		writeCfgToFile(t, path, cfg, qps)
		return p.ReloadConfig()
	}
	return p, store, reloadFn
}

// writeCfgToFile marshals cfg (with the given QPS) to path so ReloadConfig can
// re-read it. Mutating QPS each reload proves the live limiter reconfigures.
func writeCfgToFile(t *testing.T, path string, cfg *config.Config, qps int64) {
	t.Helper()
	cfg.RateLimit.QPS = float64(qps)
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config %s: %v", path, err)
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
