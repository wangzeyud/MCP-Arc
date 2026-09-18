package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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

func (s *Server) Start(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/api/stats", s.auth(s.handleStats))
	mux.HandleFunc("/api/replay", s.auth(s.handleReplay))
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

	return http.ListenAndServe(fmt.Sprintf(":%d", port), mux)
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
