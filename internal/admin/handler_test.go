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

	"github.com/dodoyu-sama/mcp-arc/internal/audit"
)

// fakeReplayer captures the args handed to Replay so tests can assert the
// handler forwards the recorded tool name and raw params verbatim.
type fakeReplayer struct {
	tool   string
	params string
	resp   []byte
	err    error
}

func (f *fakeReplayer) Replay(_ context.Context, toolName string, rawParams []byte) ([]byte, error) {
	f.tool = toolName
	f.params = string(rawParams)
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
	s := New(newFakeStore(), "", rp, nil)
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
	recs, err := s.store.Query(audit.QueryOpts{Limit: 1})
	if err != nil || len(recs) == 0 {
		t.Fatalf("query after insert: %v", err)
	}
	return recs[0].ID
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
