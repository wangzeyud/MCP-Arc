package admin

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
)

// exportRecord is the projection written out by /api/export. Raw (unmasked)
// params and results are only included when explicitly asked for — an audit
// export should not be the thing that leaks the secrets it was meant to govern.
type exportRecord struct {
	ID        int64     `json:"id"`
	ClientID  string    `json:"client_id"`
	ToolName  string    `json:"tool_name"`
	Params    string    `json:"params"`
	Result    string    `json:"result"`
	ErrorMsg  string    `json:"error_msg"`
	LatencyMs int64     `json:"latency_ms"`
	Timestamp time.Time `json:"timestamp"`
	ReplayOf  int64     `json:"replay_of,omitempty"`
	RawParams *string   `json:"raw_params,omitempty"`
	RawResult *string   `json:"raw_result,omitempty"`
}

const maxExportLimit = 10000

// handleExport streams the audit log as JSON or CSV.
//
//	GET /api/export?format=json|csv&client_id=&tool=&limit=&raw=1
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store not available"})
		return
	}
	q := r.URL.Query()
	opts := audit.QueryOpts{ClientID: q.Get("client_id"), ToolName: q.Get("tool")}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}
	if opts.Limit <= 0 {
		opts.Limit = 1000
	}
	if opts.Limit > maxExportLimit {
		opts.Limit = maxExportLimit
	}
	includeRaw := q.Get("raw") == "1" || q.Get("raw") == "true"

	recs, err := s.store.Query(opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows := make([]exportRecord, 0, len(recs))
	for _, rec := range recs {
		e := exportRecord{
			ID:        rec.ID,
			ClientID:  rec.ClientID,
			ToolName:  rec.ToolName,
			Params:    rec.Params,
			Result:    rec.Result,
			ErrorMsg:  rec.ErrorMsg,
			LatencyMs: rec.LatencyMs,
			Timestamp: rec.Timestamp,
			ReplayOf:  rec.ReplayOf,
		}
		if includeRaw {
			e.RawParams = &rec.RawParams
			e.RawResult = &rec.RawResult
		}
		rows = append(rows, e)
	}

	stamp := time.Now().Format("20060102-150405")
	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="mcp-arc-calls-`+stamp+`.csv"`)
		writeCSV(w, rows, includeRaw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="mcp-arc-calls-`+stamp+`.json"`)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"exported_at": time.Now(),
		"count":       len(rows),
		"records":     rows,
	})
}

func writeCSV(w http.ResponseWriter, rows []exportRecord, includeRaw bool) {
	writer := csv.NewWriter(w)
	defer writer.Flush()
	header := []string{"id", "client_id", "tool_name", "params", "result", "error_msg", "latency_ms", "timestamp", "replay_of"}
	if includeRaw {
		header = append(header, "raw_params", "raw_result")
	}
	_ = writer.Write(header)
	for _, r := range rows {
		line := []string{
			strconv.FormatInt(r.ID, 10),
			r.ClientID,
			r.ToolName,
			r.Params,
			r.Result,
			r.ErrorMsg,
			strconv.FormatInt(r.LatencyMs, 10),
			r.Timestamp.Format(time.RFC3339),
			strconv.FormatInt(r.ReplayOf, 10),
		}
		if includeRaw {
			line = append(line, deref(r.RawParams), deref(r.RawResult))
		}
		_ = writer.Write(line)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
