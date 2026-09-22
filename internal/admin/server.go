package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/web"
)

// Replayer re-issues a previously recorded tools/call to the upstream server.
// It is satisfied by *proxy.Proxy, keeping admin decoupled from the proxy package.
type Replayer interface {
	Replay(ctx context.Context, toolName string, rawParams []byte) ([]byte, error)
}

// Status is runtime information surfaced to the console so the user can see
// which address to paste into their MCP client. Ports may differ from the
// configured defaults when the preferred ones were already in use.
type Status struct {
	ClientTransport string `json:"client_transport"` // stdio | sse
	SSEURL          string `json:"sse_url"`          // endpoint for MCP clients, e.g. http://localhost:8081/sse
	ConsoleURL      string `json:"console_url"`      // this web console, e.g. http://localhost:8080/
	AdminPort       int    `json:"admin_port"`
	// PID / StartedAt identify the process actually serving this console. Several
	// mcp-arc instances can run side by side (each on its own port), so an
	// operator needs a way to tell a live instance from a stale browser tab or a
	// leftover process. The console prints both.
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"` // RFC3339
	// AuditDriver is the active audit backend: "sqlite" | "postgres" | "memory".
	// "memory" is non-persistent — records are gone as soon as the process exits,
	// which is the usual reason a console looks empty after a restart.
	AuditDriver string `json:"audit_driver"`
	// ConfigDir is the directory of the config file the process loaded, so the
	// operator can confirm which config (and therefore which upstream) is live.
	ConfigDir string `json:"config_dir,omitempty"`
}

type Server struct {
	store    audit.Store
	token    string
	replayer Replayer
	rules    RuleManager
	// Status is set by the caller before Start and exposed via /api/status.
	Status Status
}

func New(store audit.Store, token string, replayer Replayer, rules RuleManager) *Server {
	return &Server{store: store, token: token, replayer: replayer, rules: rules}
}

// Start binds port and serves the console. It is kept for tests and for the
// embedded fallback path; production callers should prefer StartListener so the
// port is reserved atomically before the server runs (see cmd/mcp-arc).
func (s *Server) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// StartListener serves the console on an already-bound listener. main.go reserves
// the port (holding the socket) before launching the proxy, so two instances can
// never race for the same admin port.
func (s *Server) StartListener(ln net.Listener) error {
	return s.Serve(ln)
}

// Serve handles requests on ln until it closes. The handler map is rebuilt per
// call so each server instance owns its own mux.
func (s *Server) Serve(ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/api/stats", s.auth(s.handleStats))
	mux.HandleFunc("/api/replay", s.auth(s.handleReplay))
	mux.HandleFunc("/api/replay/batch", s.auth(s.handleReplayBatch))
	mux.HandleFunc("/api/export", s.auth(s.handleExport))
	mux.HandleFunc("/api/rules", s.auth(s.handleRules))
	mux.HandleFunc("/api/rules/{id}", s.auth(s.handleRuleByID))
	mux.HandleFunc("/api/status", s.auth(s.handleStatus))

	// Serve the embedded web console (compiled into the binary by `npm run build`).
	sub, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		return fmt.Errorf("embed web dist: %w", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback: unknown non-asset routes return index.html.
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/api/") {
			if _, statErr := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); statErr != nil {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})

	return http.Serve(ln, mux)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			if r.Header.Get("Authorization") != "Bearer "+s.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// handleStatus reports the addresses the user needs: this console plus the SSE
// endpoint to paste into their MCP client. It is how the console tells the user
// what to fill in when a port had to move.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Status)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
