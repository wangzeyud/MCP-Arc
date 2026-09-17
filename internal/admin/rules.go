package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/dodoyu-sama/mcp-arc/internal/audit"
)

// RuleManager is the rule CRUD surface the console talks to. It is satisfied by
// *proxy.Proxy, keeping admin decoupled from the proxy package.
type RuleManager interface {
	ListRules() ([]audit.MaskRule, error)
	GetRule(id int64) (*audit.MaskRule, error)
	CreateRule(r *audit.MaskRule) error
	UpdateRule(r *audit.MaskRule) error
	DeleteRule(id int64) error
}

// rulePayload is the request body for create/update. Enabled is a pointer so an
// omitted field means "on" rather than Go's zero value.
type rulePayload struct {
	Name     string   `json:"name"`
	Patterns []string `json:"patterns"`
	Fields   []string `json:"fields"`
	MaskChar string   `json:"mask_char"`
	Enabled  *bool    `json:"enabled"`
}

func (p rulePayload) toRule(id int64) *audit.MaskRule {
	r := &audit.MaskRule{
		ID:       id,
		Name:     p.Name,
		Patterns: p.Patterns,
		Fields:   p.Fields,
		MaskChar: p.MaskChar,
		Enabled:  p.Enabled == nil || *p.Enabled,
	}
	if r.MaskChar == "" {
		r.MaskChar = "****"
	}
	return r
}

func decodeRule(r *http.Request) (*audit.MaskRule, error) {
	var p rulePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return nil, err
	}
	return p.toRule(0), nil
}

// handleRules serves GET (list) and POST (create) on /api/rules.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	if s.rules == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rule store not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules, err := s.rules.ListRules()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if rules == nil {
			rules = []audit.MaskRule{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
	case http.MethodPost:
		rule, err := decodeRule(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
			return
		}
		if err := s.rules.CreateRule(rule); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, rule)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET or POST"})
	}
}

// handleRuleByID serves GET / PUT / DELETE on /api/rules/{id}.
func (s *Server) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	if s.rules == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "rule store not available"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid rule id"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		rule, err := s.rules.GetRule(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule not found"})
			return
		}
		writeJSON(w, http.StatusOK, rule)

	case http.MethodPut:
		rule, err := decodeRule(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
			return
		}
		rule.ID = id
		if err := s.rules.UpdateRule(rule); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rule)

	case http.MethodDelete:
		if err := s.rules.DeleteRule(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET, PUT or DELETE"})
	}
}
