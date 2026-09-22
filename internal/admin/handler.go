package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
)

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store not available"})
		return
	}
	q := r.URL.Query()
	opts := audit.QueryOpts{
		ClientID: q.Get("client_id"),
		ToolName: q.Get("tool"),
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}
	recs, err := s.store.Query(opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store not available"})
		return
	}
	stats, err := s.store.Stats(audit.StatsOpts{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleConfigReload triggers a runtime reload of the configuration file (v0.7).
// It is exposed to the console so an operator can apply config edits without
// restarting mcp-arc; file-watcher reloads happen automatically in the background.
func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}
	if s.reloader == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reload unavailable"})
		return
	}
	if err := s.reloader.ReloadConfig(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

// handleReplay re-issues a recorded tools/call to the upstream server and returns
// the raw upstream response. The recorded raw_params are used verbatim (unmasked)
// so the replay faithfully reproduces the original request. When `diff: true` is
// supplied and the call has a recorded raw_result, the response is compared against
// it (exact JSON equality) so operators can verify the upstream still behaves.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store not available"})
		return
	}
	start := time.Now()
	var body struct {
		CallID int64 `json:"call_id"`
		Diff   bool  `json:"diff"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CallID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid call_id"})
		return
	}
	rec, err := s.store.Get(body.CallID)
	if err != nil || rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "call not found"})
		return
	}
	if rec.RawParams == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "call not replayable (no recorded params)"})
		return
	}
	if s.replayer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "replay not available"})
		return
	}
	resp, err := s.replayer.Replay(r.Context(), rec.ToolName, []byte(rec.RawParams))
	if err != nil {
		s.recordReplayAudit(rec, nil, err, time.Since(start).Milliseconds())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.recordReplayAudit(rec, resp, nil, time.Since(start).Milliseconds())
	out := map[string]any{
		"tool":     rec.ToolName,
		"response": json.RawMessage(resp),
	}
	if body.Diff && rec.RawResult != "" {
		out["diff"] = computeDiff(resp, []byte(rec.RawResult))
		out["recorded"] = json.RawMessage(rec.RawResult)
	}
	writeJSON(w, http.StatusOK, out)
}

// maxBatchReplays caps how many calls a single batch replay may touch, so a
// filter that matches everything cannot pin the upstream indefinitely.
const maxBatchReplays = 1000

// recordReplayAudit writes a console replay into the audit log so operators can
// see replays next to live traffic. ReplayOf points at the original call, and the
// replayed upstream response becomes the new raw/result payload. The admin
// process has no masker, so replays are stored unmasked — acceptable for an
// operator action that explicitly re-issues a known call.
func (s *Server) recordReplayAudit(orig *audit.CallRecord, resp []byte, rerr error, latencyMs int64) {
	if s.store == nil {
		return
	}
	rec := &audit.CallRecord{
		ClientID:  orig.ClientID,
		ToolName:  orig.ToolName,
		Params:    orig.Params,
		RawParams: orig.RawParams,
		RawResult: string(resp),
		Result:    string(resp),
		LatencyMs: latencyMs,
		Timestamp: time.Now(),
		ReplayOf:  orig.ID,
	}
	if rerr != nil {
		rec.ErrorMsg = rerr.Error()
		rec.RawResult = ""
		rec.Result = ""
	}
	_ = s.store.Insert(rec)
}

// handleReplayBatch re-issues several recorded tools/call to the upstream. Call
// ids may be given explicitly via `call_ids`, or resolved from a `filter` (tool /
// client_id / since / until / limit). Replays run sequentially, reusing the same
// id-rewrite machinery as a live call; a single failure is recorded and the rest
// continue. Optional `diff: true` compares each response against its recorded
// raw_result (exact JSON equality).
func (s *Server) handleReplayBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store not available"})
		return
	}
	if s.replayer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "replay not available"})
		return
	}
	var body struct {
		CallIDs []int64 `json:"call_ids"`
		Filter  struct {
			ToolName string `json:"tool"`
			ClientID string `json:"client_id"`
			Since    string `json:"since"` // RFC3339, optional
			Until    string `json:"until"` // RFC3339, optional
			Limit    int    `json:"limit"`
		} `json:"filter"`
		Diff bool `json:"diff"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	ids, err := s.resolveReplayIDs(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"results": []ReplayItemResult{}})
		return
	}
	if len(ids) > maxBatchReplays {
		ids = ids[:maxBatchReplays]
	}

	results := make([]ReplayItemResult, 0, len(ids))
	for _, id := range ids {
		item := ReplayItemResult{CallID: id}
		rec, gerr := s.store.Get(id)
		if gerr != nil || rec == nil {
			item.Status = "error"
			item.Error = "call not found"
			results = append(results, item)
			continue
		}
		item.Tool = rec.ToolName
		if rec.RawParams == "" {
			item.Status = "skipped"
			item.Error = "no recorded params"
			results = append(results, item)
			continue
		}
		itemStart := time.Now()
		resp, rerr := s.replayer.Replay(r.Context(), rec.ToolName, []byte(rec.RawParams))
		if rerr != nil {
			s.recordReplayAudit(rec, nil, rerr, time.Since(itemStart).Milliseconds())
			item.Status = "error"
			item.Error = rerr.Error()
			results = append(results, item)
			continue
		}
		s.recordReplayAudit(rec, resp, nil, time.Since(itemStart).Milliseconds())
		item.Status = "ok"
		item.Response = resp
		if body.Diff && rec.RawResult != "" {
			d := computeDiff(resp, []byte(rec.RawResult))
			item.Diff = &d
		}
		results = append(results, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveReplayIDs returns the call ids to replay: either the explicitly given
// list, or the ids matched by the filter (applied via the store's Query).
func (s *Server) resolveReplayIDs(body struct {
	CallIDs []int64 `json:"call_ids"`
	Filter  struct {
		ToolName string `json:"tool"`
		ClientID string `json:"client_id"`
		Since    string `json:"since"`
		Until    string `json:"until"`
		Limit    int    `json:"limit"`
	} `json:"filter"`
	Diff bool `json:"diff"`
}) ([]int64, error) {
	if len(body.CallIDs) > 0 {
		return body.CallIDs, nil
	}
	opts := audit.QueryOpts{ToolName: body.Filter.ToolName, ClientID: body.Filter.ClientID, Limit: body.Filter.Limit}
	if body.Filter.Since != "" {
		t, err := time.Parse(time.RFC3339, body.Filter.Since)
		if err != nil {
			return nil, fmt.Errorf("invalid since: %w", err)
		}
		opts.Since = t
	}
	if body.Filter.Until != "" {
		t, err := time.Parse(time.RFC3339, body.Filter.Until)
		if err != nil {
			return nil, fmt.Errorf("invalid until: %w", err)
		}
		opts.Until = t
	}
	recs, err := s.store.Query(opts)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.ID)
	}
	return ids, nil
}

// ReplayItemResult is one entry of a batch replay response.
type ReplayItemResult struct {
	CallID   int64           `json:"call_id"`
	Tool     string          `json:"tool,omitempty"`
	Status   string          `json:"status"`            // ok | error | skipped
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
	Diff     *ReplayDiff     `json:"diff,omitempty"`
}

// ReplayDiff reports whether a replayed response still matches the originally
// recorded upstream result. v0.6 supports "exact" (JSON-equal) comparison.
type ReplayDiff struct {
	Mode  string `json:"mode"`  // "exact"
	Match bool   `json:"match"` // true when the responses are JSON-equal
}

// computeDiff compares a freshly replayed upstream response against the
// originally recorded raw result. The recorded RawResult holds only the inner
// MCP result payload (e.g. {"content":[...]}), whereas Replay returns the full
// JSON-RPC response envelope ({"jsonrpc":"2.0","id":...,"result":{...}}). To
// compare like-for-like we unwrap the envelope's result (or error) before the
// exact comparison; a payload that fails to parse is treated as a non-match.
func computeDiff(got, want []byte) ReplayDiff {
	return ReplayDiff{Mode: "exact", Match: jsonEqual(unwrapResult(got), want)}
}

// unwrapResult extracts the inner payload of a JSON-RPC response envelope so it
// can be compared against a recorded RawResult (which holds only that payload).
// Non-envelope JSON is returned unchanged, and a parse failure falls back to the
// raw bytes.
func unwrapResult(b []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(b, &msg); err != nil {
		return b
	}
	if v, ok := msg["result"]; ok {
		if rb, err := json.Marshal(v); err == nil {
			return rb
		}
	}
	if v, ok := msg["error"]; ok {
		if eb, err := json.Marshal(v); err == nil {
			return eb
		}
	}
	return b
}

func jsonEqual(a, b []byte) bool {
	var ja, jb any
	if err := json.Unmarshal(a, &ja); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &jb); err != nil {
		return false
	}
	return reflect.DeepEqual(ja, jb)
}
