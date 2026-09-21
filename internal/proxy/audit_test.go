package proxy

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
)

// fakeAuditStore captures inserted records and tracks in-flight inserts so tests
// can deterministically wait for the async audit worker to finish.
type fakeAuditStore struct {
	mu   sync.Mutex
	recs []*audit.CallRecord
	wg   sync.WaitGroup
}

func (f *fakeAuditStore) Insert(r *audit.CallRecord) error {
	f.wg.Add(1)
	defer f.wg.Done()
	f.mu.Lock()
	f.recs = append(f.recs, r)
	f.mu.Unlock()
	return nil
}
func (f *fakeAuditStore) Query(audit.QueryOpts) ([]audit.CallRecord, error) { return nil, nil }
func (f *fakeAuditStore) Get(int64) (*audit.CallRecord, error)              { return nil, nil }
func (f *fakeAuditStore) Stats(audit.StatsOpts) (*audit.Stats, error)       { return &audit.Stats{}, nil }
func (f *fakeAuditStore) ListRules() ([]audit.MaskRule, error)              { return nil, nil }
func (f *fakeAuditStore) GetRule(int64) (*audit.MaskRule, error)            { return nil, nil }
func (f *fakeAuditStore) CreateRule(*audit.MaskRule) error                  { return nil }
func (f *fakeAuditStore) UpdateRule(*audit.MaskRule) error                  { return nil }
func (f *fakeAuditStore) DeleteRule(int64) error                            { return nil }
func (f *fakeAuditStore) Close() error                                      { return nil }

// On a graceful stop the worker must drain buffered audit records rather than
// dropping them when ctx is cancelled.
func TestDrainAuditFlushesBufferedRecords(t *testing.T) {
	store := &fakeAuditStore{}
	p := &Proxy{auditStore: store, opts: Options{Config: config.Default()}}

	const n = 5
	p.auditCh = make(chan *audit.CallRecord, n)
	for range n {
		p.auditCh <- &audit.CallRecord{ToolName: "t"}
	}

	p.drainAudit(50 * time.Millisecond)
	store.wg.Wait() // wait for the async inserts kicked off by drainAudit

	store.mu.Lock()
	got := len(store.recs)
	store.mu.Unlock()
	if got != n {
		t.Fatalf("drained %d records, want %d", got, n)
	}
}

// A lost upstream connection must fail in-flight requests with a clear error so
// clients are not left hanging.
func TestFailPendingUpstreamDownNotifiesClients(t *testing.T) {
	p := &Proxy{pending: map[string]*pendingCall{}}
	var got []byte
	p.pending["gw-1"] = &pendingCall{
		respond:   func(b []byte, _ bool) error { got = b; return nil },
		origIDRaw: json.RawMessage("7"),
	}

	p.failPendingUpstreamDown()

	if got == nil {
		t.Fatal("expected in-flight request to be failed on upstream down")
	}
	if !strings.Contains(string(got), "upstream disconnected") {
		t.Fatalf("unexpected error body: %s", got)
	}
	p.mu.Lock()
	n := len(p.pending)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending should be cleared after fail, got %d", n)
	}
}
