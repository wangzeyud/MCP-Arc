package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wangzeyud/mcp-arc/internal/audit"
)

// fakeReplayer captures the args handed to Replay so tests can assert the
// handler forwards the recorded tool name and raw params verbatim. If respondFor
// maps a tool name to an error, that error is returned for that tool.
type fakeReplayer struct {
	tool      string
	params    string
	resp      []byte
	err       error
	respondFor map[string]error
}

func (f *fakeReplayer) Replay(_ context.Context, toolName string, rawParams []byte) ([]byte, error) {
	f.tool = toolName
	f.params = string(rawParams)
	if f.respondFor != nil {
		if e, ok := f.respondFor[toolName]; ok {
			return nil, e
		}
	}
	return f.resp, f.err
}

// fakeStore is an in-memory audit.Store so the handler can be exercised without a
// cgo-enabled SQLite (the go-sqlite3 driver is a no-op stub when CGO_ENABLED=0).
type fakeStore struct {
	mu    sync.Mutex
	seq   int64
	calls map[int64]*audit.CallRecord
}

func newFakeStore() *fakeStore {
	return &fakeStore{calls: map[int64]*audit.CallRecord{}}
}

func (f *fakeStore) Insert(r *audit.CallRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	r.ID = f.seq
	c := *r
	f.calls[r.ID] = &c
	return nil
}

func (f *fakeStore) Query(opts audit.QueryOpts) ([]audit.CallRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.CallRecord, 0, len(f.calls))
	for _, r := range f.calls {
		if opts.ToolName != "" && r.ToolName != opts.ToolName {
			continue
		}
		if opts.ClientID != "" && r.ClientID != opts.ClientID {
			continue
		}
		out = append(out, *r)
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (f *fakeStore) Get(id int64) (*audit.CallRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.calls[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}

func (f *fakeStore) Stats(audit.StatsOpts) (*audit.Stats, error) { return &audit.Stats{}, nil }
func (f *fakeStore) Close() error                                { return nil }

// RuleStore stubs (unused by the replay handler).
func (f *fakeStore) ListRules() ([]audit.MaskRule, error)   { return nil, nil }
func (f *fakeStore) GetRule(int64) (*audit.MaskRule, error) { return nil, errors.New("not found") }
func (f *fakeStore) CreateRule(*audit.MaskRule) error       { return nil }
func (f *fakeStore) UpdateRule(*audit.MaskRule) error       { return nil }
func (f *fakeStore) DeleteRule(int64) error                 { return nil }

func newTestServer(t *testing.T, rp Replayer) (*Server, func()) {
	t.Helper()
	s := New(newFakeStore(), "", rp, nil, nil)
	return s, func() {}
}

func doReplay(t *testing.T, s *Server, callID int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"call_id": callID})
	req := httptest.NewRequest(http.MethodPost, "/api/replay", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleReplay(rec, req)
	return rec
}

func insertAndID(t *testing.T, s *Server, rec *audit.CallRecord) int64 {
	t.Helper()
	if err := s.store.Insert(rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The store assigns the id on insert; return it directly rather than
	// re-querying, since the fake store's map iteration order is non-deterministic.
	if rec.ID == 0 {
		t.Fatalf("store did not assign an id")
	}
	return rec.ID
}

// The handler re-issues the recorded call with the original (unmasked) params and
// echoes the upstream response together with the tool name.
func TestHandleReplaySuccess(t *testing.T) {
	rp := &fakeReplayer{resp: []byte(`{"result":"ok"}`)}
	s, closeFn := newTestServer(t, rp)
	defer closeFn()

	id := insertAndID(t, s, &audit.CallRecord{
		ClientID:  "c",
		ToolName:  "my_tool",
		Params:    `{"x":1}`,
		RawParams: `{"x":1}`,
		Result:    `{"r":1}`,
		RawResult: `{"r":1}`,
	})

	rr := doReplay(t, s, id)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Tool     string          `json:"tool"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Tool != "my_tool" {
		t.Fatalf("tool = %q, want my_tool", out.Tool)
	}
	if string(out.Response) != `{"result":"ok"}` {
		t.Fatalf("response = %s", out.Response)
	}
	if rp.tool != "my_tool" || rp.params != `{"x":1}` {
		t.Fatalf("replayer received tool=%q params=%q", rp.tool, rp.params)
	}
}

func TestHandleReplayNotFound(t *testing.T) {
	s, closeFn := newTestServer(t, &fakeReplayer{})
	defer closeFn()
	if rr := doReplay(t, s, 99999); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// A recorded call with no raw params cannot be faithfully replayed; the handler
// must refuse rather than send a blank/garbage request upstream.
func TestHandleReplayNotReplayable(t *testing.T) {
	s, closeFn := newTestServer(t, &fakeReplayer{})
	defer closeFn()
	id := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "t", Params: `{}`, RawParams: ""})
	if rr := doReplay(t, s, id); rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestHandleReplayUpstreamError(t *testing.T) {
	rp := &fakeReplayer{err: errors.New("upstream down")}
	s, closeFn := newTestServer(t, rp)
	defer closeFn()
	id := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "t", RawParams: `{}`})
	if rr := doReplay(t, s, id); rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rr.Code, rr.Body.String())
	}
}

func doReplayBatch(t *testing.T, s *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/replay/batch", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	s.handleReplayBatch(rec, req)
	return rec
}

type batchItem struct {
	CallID   int64           `json:"call_id"`
	Tool     string          `json:"tool"`
	Status   string          `json:"status"`
	Response json.RawMessage `json:"response"`
	Error    string          `json:"error"`
	Diff     *struct {
		Mode  string `json:"mode"`
		Match bool   `json:"match"`
	} `json:"diff"`
}

func decodeBatch(t *testing.T, rr *httptest.ResponseRecorder) []batchItem {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Results []batchItem `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	return out.Results
}

// Batch replay by explicit ids reuses the same upstream path and returns one
// result per call.
func TestHandleReplayBatchByIDs(t *testing.T) {
	rp := &fakeReplayer{resp: []byte(`{"result":"ok"}`)}
	s, closeFn := newTestServer(t, rp)
	defer closeFn()
	id1 := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "a", RawParams: `{"x":1}`})
	id2 := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "b", RawParams: `{"y":2}`})

	rr := doReplayBatch(t, s, map[string]any{"call_ids": []int64{id1, id2}})
	items := decodeBatch(t, rr)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Status != "ok" || items[1].Status != "ok" {
		t.Fatalf("statuses = %q/%q, want ok/ok", items[0].Status, items[1].Status)
	}
	if items[0].Tool != "a" || items[1].Tool != "b" {
		t.Fatalf("tools = %q/%q", items[0].Tool, items[1].Tool)
	}
	if string(items[0].Response) != `{"result":"ok"}` {
		t.Fatalf("response[0] = %s", items[0].Response)
	}
}

// Filter mode resolves the id list from the store before replaying.
func TestHandleReplayBatchFilter(t *testing.T) {
	rp := &fakeReplayer{resp: []byte(`{"ok":true}`)}
	s, closeFn := newTestServer(t, rp)
	defer closeFn()
	insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "a", RawParams: `{}`})
	insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "a", RawParams: `{}`})
	insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "other", RawParams: `{}`})

	rr := doReplayBatch(t, s, map[string]any{"filter": map[string]any{"tool": "a"}})
	items := decodeBatch(t, rr)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (filter tool=a)", len(items))
	}
	for _, it := range items {
		if it.Tool != "a" {
			t.Fatalf("filtered tool = %q, want a", it.Tool)
		}
	}
}

// Diff reports whether the replayed response still matches the recorded raw_result.
func TestHandleReplayBatchDiff(t *testing.T) {
	// Replay returns the full JSON-RPC envelope in production; mirror that here so
	// we exercise the unwrap-to-result comparison.
	rp := &fakeReplayer{resp: []byte(`{"jsonrpc":"2.0","id":"gw-1","result":{"r":1}}`)}
	s, closeFn := newTestServer(t, rp)
	defer closeFn()
	// matches recorded raw_result -> diff.match true
	idMatch := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "a", RawParams: `{}`, RawResult: `{"r":1}`})
	// differs -> diff.match false
	idDiff := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "b", RawParams: `{}`, RawResult: `{"r":2}`})

	rr := doReplayBatch(t, s, map[string]any{"call_ids": []int64{idMatch, idDiff}, "diff": true})
	items := decodeBatch(t, rr)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Diff == nil || !items[0].Diff.Match {
		t.Fatalf("item0 diff = %+v, want match=true", items[0].Diff)
	}
	if items[1].Diff == nil || items[1].Diff.Match {
		t.Fatalf("item1 diff = %+v, want match=false", items[1].Diff)
	}
}

// computeDiff must compare the inner result payload, not the JSON-RPC envelope:
// Replay returns {"jsonrpc","id","result":...} but RawResult stores only the
// result object. Regression test for "diff always shows changed".
func TestComputeDiffUnwrapsEnvelope(t *testing.T) {
	cases := []struct {
		name string
		got  string // what Replay returns (full envelope)
		want string // recorded RawResult (result object)
		match bool
	}{
		{"matching result object", `{"jsonrpc":"2.0","id":"gw-1","result":{"content":[{"type":"text","text":"ok"}]}}`, `{"content":[{"type":"text","text":"ok"}]}`, true},
		{"id differs but result same -> match", `{"jsonrpc":"2.0","id":"gw-9","result":{"x":1}}`, `{"x":1}`, true},
		{"result differs -> no match", `{"jsonrpc":"2.0","id":"gw-1","result":{"x":2}}`, `{"x":1}`, false},
		{"error payload compared", `{"jsonrpc":"2.0","id":"gw-1","error":{"code":-32000,"message":"boom"}}`, `{"code":-32000,"message":"boom"}`, true},
		{"non-envelope passthrough", `{"foo":"bar"}`, `{"foo":"bar"}`, true},
	}
	for _, c := range cases {
		d := computeDiff([]byte(c.got), []byte(c.want))
		if d.Mode != "exact" {
			t.Fatalf("%s: mode = %q, want exact", c.name, d.Mode)
		}
		if d.Match != c.match {
			t.Fatalf("%s: match = %v, want %v", c.name, d.Match, c.match)
		}
	}
}

// A call with no recorded params is skipped (not errored); an upstream failure on
// one call does not abort the others.
func TestHandleReplayBatchPartialFailure(t *testing.T) {
	rp := &fakeReplayer{resp: []byte(`{"ok":true}`), err: errors.New("boom")}
	s, closeFn := newTestServer(t, rp)
	defer closeFn()
	idNoParams := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "a", RawParams: ""})
	idFail := insertAndID(t, s, &audit.CallRecord{ClientID: "c", ToolName: "b", RawParams: `{}`})

	// Make the replayer return an error for "b" specifically.
	rp.respondFor = map[string]error{"b": errors.New("boom")}
	rr := doReplayBatch(t, s, map[string]any{"call_ids": []int64{idNoParams, idFail}})
	items := decodeBatch(t, rr)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Status != "skipped" {
		t.Fatalf("item0 status = %q, want skipped", items[0].Status)
	}
	if items[1].Status != "error" || items[1].Error != "boom" {
		t.Fatalf("item1 = %+v, want error/boom", items[1])
	}
}
