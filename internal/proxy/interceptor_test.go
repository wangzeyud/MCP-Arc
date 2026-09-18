package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
	"github.com/wangzeyud/mcp-arc/internal/mask"
	"github.com/wangzeyud/mcp-arc/internal/ratelimit"
	"github.com/wangzeyud/mcp-arc/internal/transport"
)

// capturingStore records audit inserts so the interception chain can be
// asserted without sqlite or postgres. It reuses fakeRuleStore for the rest of
// audit.Store.
type capturingStore struct {
	fakeRuleStore
	records []*audit.CallRecord
}

func (c *capturingStore) Insert(r *audit.CallRecord) error {
	c.records = append(c.records, r)
	return nil
}

// collector stands in for a client transport's respond function.
type collector struct {
	msgs [][]byte
}

func (c *collector) respond(b []byte) error {
	c.msgs = append(c.msgs, b)
	return nil
}

func newTestProxy() (*Proxy, *capturingStore, *collector) {
	store := &capturingStore{}
	c := &collector{}
	p := &Proxy{
		pending:         map[string]*pendingCall{},
		limiter:         ratelimit.NewTokenBucketManager(0, 0, false),
		auditWrites:     true,
		auditStore:      store,
		clientID:        "test-client",
		clientBroadcast: func([]byte) error { return nil },
	}
	return p, store, c
}

// numberAt returns the literal JSON number found at the given nested key path,
// e.g. numberAt(raw, "params", "arguments", "orderId").
func numberAt(t *testing.T, raw []byte, keys ...string) string {
	t.Helper()
	m, err := decodeJSONObject(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var cur any = m
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("expected an object at %q in %s", k, raw)
		}
		cur = obj[k]
	}
	n, ok := cur.(json.Number)
	if !ok {
		t.Fatalf("expected a number at %v in %s, got %#v", keys, raw, cur)
	}
	return n.String()
}

func stringAt(t *testing.T, raw []byte, keys ...string) string {
	t.Helper()
	m, err := decodeJSONObject(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var cur any = m
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("expected an object at %q in %s", k, raw)
		}
		cur = obj[k]
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("expected a string at %v in %s, got %#v", keys, raw, cur)
	}
	return s
}

func pendingCount(p *Proxy) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

func TestDecodeJSONObjectKeepsNumbersLiteral(t *testing.T) {
	m, err := decodeJSONObject([]byte(`{"big":1234567890123456789,"f":1.50,"s":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m["big"].(json.Number).String(); got != "1234567890123456789" {
		t.Errorf("big = %s, want 1234567890123456789", got)
	}
	if got := m["f"].(json.Number).String(); got != "1.50" {
		t.Errorf("f = %s, want 1.50 (formatting must survive the round trip)", got)
	}
}

func TestDecodeJSONObjectRejectsNonSingleObject(t *testing.T) {
	cases := map[string]string{
		"batch":         `[{"jsonrpc":"2.0","id":1}]`,
		"trailing data": `{"jsonrpc":"2.0","id":1} trailing`,
		"truncated":     `{"jsonrpc":"2.0","id":1`,
		"not json":      `hello`,
		"number":        `42`,
	}
	for name, in := range cases {
		if _, err := decodeJSONObject([]byte(in)); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
	// `null` decodes to a nil map without error, matching json.Unmarshal.
	m, err := decodeJSONObject([]byte(`null`))
	if err != nil || m != nil {
		t.Errorf("null: got (%v, %v), want (nil, nil)", m, err)
	}
}

// A float64 round trip rewrites any integer beyond 2^53, so the upstream would
// silently receive different arguments than the client sent.
func TestProcessClientMessagePreservesLargeIntegers(t *testing.T) {
	p, _, c := newTestProxy()
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pay","arguments":{"orderId":1234567890123456789,"cents":100}}}`)

	forward, synthetic := p.processClientMessage(raw, c.respond)
	if synthetic != nil {
		t.Fatalf("unexpected synthetic response: %s", synthetic)
	}
	if forward == nil {
		t.Fatal("expected the call to be forwarded")
	}
	if got := numberAt(t, forward, "params", "arguments", "orderId"); got != "1234567890123456789" {
		t.Errorf("orderId = %s, want 1234567890123456789", got)
	}
	if got := numberAt(t, forward, "params", "arguments", "cents"); got != "100" {
		t.Errorf("cents = %s, want 100", got)
	}
	if got := stringAt(t, forward, "id"); got != "gw-1" {
		t.Errorf("id = %q, want gw-1 (rewritten for correlation)", got)
	}
}

func TestProcessClientMessagePreservesNumberFormatting(t *testing.T) {
	p, _, c := newTestProxy()
	raw := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"calc","arguments":{"ratio":1.0,"exp":1e3}}}`)

	forward, _ := p.processClientMessage(raw, c.respond)
	if got := numberAt(t, forward, "params", "arguments", "ratio"); got != "1.0" {
		t.Errorf("ratio = %s, want 1.0", got)
	}
	if got := numberAt(t, forward, "params", "arguments", "exp"); got != "1e3" {
		t.Errorf("exp = %s, want 1e3", got)
	}
}

// Anything the gateway does not own must reach the upstream byte-identical.
func TestProcessClientMessagePassesThroughUntouched(t *testing.T) {
	cases := map[string]string{
		"not json":      `not json at all`,
		"notification":  `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		"batch request": `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}]`,
		"malformed":     `{"jsonrpc":"2.0","id":1,"method":"ping"} oops`,
	}
	for name, in := range cases {
		p, _, c := newTestProxy()
		forward, synthetic := p.processClientMessage([]byte(in), c.respond)
		if synthetic != nil {
			t.Errorf("%s: unexpected synthetic response", name)
		}
		if string(forward) != in {
			t.Errorf("%s: forward = %s, want it passed through unchanged", name, forward)
		}
		if n := pendingCount(p); n != 0 {
			t.Errorf("%s: pending = %d, want 0", name, n)
		}
	}
}

func TestProcessClientMessageRateLimitsToolCallsOnly(t *testing.T) {
	p, _, c := newTestProxy()
	p.limiter = ratelimit.NewTokenBucketManager(0, 0, true) // one token, never refills

	first := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{}}}`)
	forward, synthetic := p.processClientMessage(first, c.respond)
	if forward == nil || synthetic != nil {
		t.Fatalf("first tool call should pass: forward=%s synthetic=%s", forward, synthetic)
	}

	second := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x","arguments":{}}}`)
	forward, synthetic = p.processClientMessage(second, c.respond)
	if forward != nil {
		t.Errorf("rate-limited call must not reach the upstream, got %s", forward)
	}
	if synthetic == nil {
		t.Fatal("expected a synthetic error for the client")
	}
	if got := numberAt(t, synthetic, "id"); got != "2" {
		t.Errorf("synthetic id = %s, want the client's original id 2", got)
	}
	if got := numberAt(t, synthetic, "error", "code"); got != "-32000" {
		t.Errorf("error code = %s, want -32000", got)
	}
	if n := pendingCount(p); n != 1 {
		t.Errorf("pending = %d, want 1 (the rejected call must not be tracked)", n)
	}

	// Non-tool traffic is never rate limited.
	ping := []byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if forward, synthetic := p.processClientMessage(ping, c.respond); forward == nil || synthetic != nil {
		t.Errorf("ping should not be rate limited: forward=%s synthetic=%s", forward, synthetic)
	}
}

func TestProcessClientMessageNormalisesMissingArguments(t *testing.T) {
	cases := map[string]string{
		"params without arguments": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ping"}}`,
		"no params at all":         `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`,
	}
	for name, in := range cases {
		p, _, c := newTestProxy()
		p.processClientMessage([]byte(in), c.respond)
		if got := p.pending["gw-1"].rawParams; got != "{}" {
			t.Errorf("%s: rawParams = %s, want {}", name, got)
		}
	}
}

func TestUpstreamResponseRestoresOriginalIDAndAudits(t *testing.T) {
	p, store, c := newTestProxy()
	p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search","arguments":{"q":"hi"}}}`), c.respond)

	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-1","result":{"content":[{"type":"text","text":"ok"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if len(c.msgs) != 1 {
		t.Fatalf("client received %d messages, want 1", len(c.msgs))
	}
	if got := numberAt(t, c.msgs[0], "id"); got != "7" {
		t.Errorf("client id = %s, want 7", got)
	}
	if n := pendingCount(p); n != 0 {
		t.Errorf("pending = %d, want 0 after the response is routed", n)
	}
	if len(store.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(store.records))
	}
	rec := store.records[0]
	if rec.ToolName != "search" {
		t.Errorf("ToolName = %q, want search", rec.ToolName)
	}
	if rec.RawParams != `{"q":"hi"}` {
		t.Errorf("RawParams = %s, want {\"q\":\"hi\"}", rec.RawParams)
	}
	if !strings.Contains(rec.RawResult, "ok") {
		t.Errorf("RawResult = %s, want it to carry the upstream payload", rec.RawResult)
	}
	if rec.ErrorMsg != "" {
		t.Errorf("ErrorMsg = %q, want empty", rec.ErrorMsg)
	}
}

// The id is rewritten on both legs, so a float64 round trip would corrupt it
// invisibly: the client would still match the response, just to the wrong id.
func TestUpstreamResponsePreservesLargeIntegerID(t *testing.T) {
	p, _, c := newTestProxy()
	p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":9007199254740993,"method":"ping"}`), c.respond)

	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-1","result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if len(c.msgs) != 1 {
		t.Fatalf("client received %d messages, want 1", len(c.msgs))
	}
	if got := numberAt(t, c.msgs[0], "id"); got != "9007199254740993" {
		t.Errorf("client id = %s, want 9007199254740993", got)
	}
}

func TestUpstreamErrorResponseIsAudited(t *testing.T) {
	p, store, c := newTestProxy()
	p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}`), c.respond)

	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-1","error":{"code":-32603,"message":"internal error"}}`)); err != nil {
		t.Fatal(err)
	}
	if len(store.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(store.records))
	}
	rec := store.records[0]
	if !strings.Contains(rec.ErrorMsg, "internal error") {
		t.Errorf("ErrorMsg = %q, want it to carry the upstream error", rec.ErrorMsg)
	}
	if rec.Result != "null" {
		t.Errorf("Result = %q, want null when the call errored", rec.Result)
	}
	if len(c.msgs) != 1 || !strings.Contains(string(c.msgs[0]), "internal error") {
		t.Errorf("the client should still receive the error, got %q", c.msgs)
	}
}

func TestUpstreamNotificationIsBroadcast(t *testing.T) {
	p, _, _ := newTestProxy()
	notif := []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
	var got []byte
	p.clientBroadcast = func(b []byte) error { got = b; return nil }

	if err := p.processUpstreamMessage(notif); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(notif) {
		t.Errorf("broadcast = %s, want the original bytes", got)
	}
}

func TestUpstreamMessageWithoutPendingCallIsIgnored(t *testing.T) {
	p, store, c := newTestProxy()
	// An id the gateway never issued, and something that is not JSON at all.
	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-404","result":{}}`)); err != nil {
		t.Errorf("unknown id: %v", err)
	}
	if err := p.processUpstreamMessage([]byte(`definitely not json`)); err != nil {
		t.Errorf("malformed: %v", err)
	}
	if len(c.msgs) != 0 {
		t.Errorf("client received %d messages, want 0", len(c.msgs))
	}
	if len(store.records) != 0 {
		t.Errorf("audit records = %d, want 0", len(store.records))
	}
}

// Masking is an audit-side concern: the upstream must still receive the real
// arguments, or the proxy changes what the tool actually does.
func TestMaskingChangesTheAuditRecordOnly(t *testing.T) {
	p, store, c := newTestProxy()
	m, err := mask.New([]mask.Spec{{
		Name:     "password",
		Patterns: []string{`^s3cret$`},
		MaskChar: "[REDACTED]",
		Enabled:  true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	p.masker = m

	forward, _ := p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"login","arguments":{"user":"u","password":"s3cret"}}}`), c.respond)
	if !strings.Contains(string(forward), "s3cret") {
		t.Errorf("the upstream must receive unmasked arguments, got %s", forward)
	}
	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-1","result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if len(store.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(store.records))
	}
	rec := store.records[0]
	if !strings.Contains(rec.Params, "[REDACTED]") || strings.Contains(rec.Params, "s3cret") {
		t.Errorf("Params must be masked, got %s", rec.Params)
	}
	if !strings.Contains(rec.RawParams, "s3cret") {
		t.Errorf("RawParams must stay unmasked for replay, got %s", rec.RawParams)
	}
}

// An upstream that never answers must not grow p.pending forever.
func TestReapPendingReleasesUnansweredCalls(t *testing.T) {
	p, _, c := newTestProxy()
	p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`), c.respond)

	p.reapPending(time.Now())
	if n := pendingCount(p); n != 1 {
		t.Fatalf("a call within its deadline was reaped: pending = %d", n)
	}
	if len(c.msgs) != 0 {
		t.Fatalf("client received %d messages, want 0", len(c.msgs))
	}

	p.mu.Lock()
	p.pending["gw-1"].deadline = time.Now().Add(-time.Second)
	p.pending["gw-2"] = &pendingCall{toolName: "orphan", deadline: time.Now().Add(-time.Second)} // no respond: must not panic
	p.mu.Unlock()

	p.reapPending(time.Now())
	if n := pendingCount(p); n != 0 {
		t.Errorf("pending = %d, want 0 after the deadline passed", n)
	}
	if len(c.msgs) != 1 {
		t.Fatalf("client received %d messages, want 1 timeout error", len(c.msgs))
	}
	if got := numberAt(t, c.msgs[0], "id"); got != "1" {
		t.Errorf("timeout id = %s, want the client's original id 1", got)
	}
	if got := numberAt(t, c.msgs[0], "error", "code"); got != "-32001" {
		t.Errorf("error code = %s, want -32001", got)
	}
}

// The sweeper must stop when the run context is cancelled, or every Run()
// would leak a goroutine.
func TestSweepPendingStopsWithContext(t *testing.T) {
	p, _, _ := newTestProxy()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		p.sweepPending(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweepPending did not return after the context was cancelled")
	}
}

// slowInsertStore makes only its first Insert block (200ms), then returns fast —
// used to prove the audit worker doesn't stall on a slow write. The "first" call
// is counted atomically at entry, before the sleep, so a later call is never
// mistaken for the slow one.
type slowInsertStore struct {
	fakeRuleStore
	mu      sync.Mutex
	records []*audit.CallRecord
	calls   atomic.Int64
}

func (s *slowInsertStore) Insert(r *audit.CallRecord) error {
	if s.calls.Add(1) == 1 {
		time.Sleep(200 * time.Millisecond)
	}
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	return nil
}

// Audit writes must not block the response path, and a slow write must not stall
// the worker from processing the next one.
func TestAuditWriteOffloadedAndNonBlocking(t *testing.T) {
	store := &slowInsertStore{}
	p := &Proxy{
		opts:            Options{Config: &config.Config{}},
		pending:         map[string]*pendingCall{},
		limiter:         ratelimit.NewTokenBucketManager(0, 0, false),
		auditWrites:     true,
		auditStore:      store,
		auditCh:         make(chan *audit.CallRecord, 8),
		clientID:        "test-client",
		clientBroadcast: func([]byte) error { return nil },
	}
	p.opts.Config.Audit.WriteTimeoutMs = 50 // force a quick timeout on the slow write

	ctx, cancel := context.WithCancel(context.Background())
	go p.auditWorker(ctx, make(chan struct{}))
	defer cancel()

	noOp := func([]byte) error { return nil }

	// First call: slow insert. The response path must return immediately.
	p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`), noOp)
	start := time.Now()
	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-1","result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("hot path blocked for %s; expected it to enqueue and return immediately", elapsed)
	}

	// Second call: fast insert. It must be audited even while the first is still
	// sleeping — proving the worker moved past the slow one.
	p.processClientMessage([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast","arguments":{}}}`), noOp)
	if err := p.processUpstreamMessage([]byte(`{"jsonrpc":"2.0","id":"gw-2","result":{}}`)); err != nil {
		t.Fatal(err)
	}

	// Wait past the slow insert (200ms); the fast one should already be recorded.
	time.Sleep(120 * time.Millisecond)
	store.mu.Lock()
	sawFast := false
	for _, r := range store.records {
		if r.ToolName == "fast" {
			sawFast = true
		}
	}
	store.mu.Unlock()
	if !sawFast {
		t.Fatalf("fast call was not audited while a slow write was pending; records=%d", len(store.records))
	}
}

// fakeUpstream is a transport.UpstreamTransporter whose Run returns quickly (to
// simulate a crash) and whose Write can be made to fail, for exercising the
// supervisor reconnect loop and the write-failure error path.
type fakeUpstream struct {
	mu       sync.Mutex
	runs     int
	writeErr error
	runErr   error
}

func (f *fakeUpstream) Run(ctx context.Context, onMessage func([]byte)) error {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
	}
	return f.runErr
}

func (f *fakeUpstream) Write(raw []byte) error { return f.writeErr }
func (f *fakeUpstream) Close() error           { return nil }

// The supervisor must reconnect the upstream after it dies, rather than giving up.
func TestUpstreamSupervisorReconnects(t *testing.T) {
	p := &Proxy{pending: map[string]*pendingCall{}}
	f := &fakeUpstream{}
	factory := func() (transport.UpstreamTransporter, error) { return f, nil }

	ctx := t.Context()
	go p.upstreamSupervisor(ctx, factory)

	time.Sleep(1200 * time.Millisecond)
	f.mu.Lock()
	runs := f.runs
	f.mu.Unlock()

	if runs < 2 {
		t.Fatalf("expected at least 2 upstream (re)connects, got %d", runs)
	}
}

// A failed forward to the upstream must surface a clear JSON-RPC error to the
// client (with the original request id), not be silently dropped.
func TestClientGetsClearErrorOnUpstreamWriteFailure(t *testing.T) {
	p := &Proxy{
		pending:     map[string]*pendingCall{},
		limiter:     ratelimit.NewTokenBucketManager(0, 0, false),
		auditWrites: false,
	}
	p.setUpstreamWriter(func(b []byte) error { return errors.New("connection refused") })

	var got []byte
	respond := func(b []byte) error { got = b; return nil }
	raw := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"x","arguments":{}}}`
	p.handleClientMessage([]byte(raw), respond)

	if got == nil {
		t.Fatal("expected a client error response when the upstream write fails")
	}
	var r struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(got, &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != 7 {
		t.Fatalf("error response id = %d, want 7 (raw=%s)", r.ID, got)
	}
	if r.Error.Code != upstreamWriteFailedCode {
		t.Fatalf("error code = %d, want %d (raw=%s)", r.Error.Code, upstreamWriteFailedCode, got)
	}

	// The failed request never left the proxy, so its pending entry is dropped.
	p.mu.Lock()
	n := len(p.pending)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected pending to be empty after a failed write, got %d", n)
	}
}
