package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/dodoyu-sama/mcp-arc/internal/audit"
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

// handleReplay re-issues a recorded tools/call to the upstream server and returns
// the raw upstream response. The recorded raw_params are used verbatim (unmasked)
// so the replay faithfully reproduces the original request.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "audit store not available"})
		return
	}
	var body struct {
		CallID int64 `json:"call_id"`
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
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tool":     rec.ToolName,
		"response": json.RawMessage(resp),
	})
}
