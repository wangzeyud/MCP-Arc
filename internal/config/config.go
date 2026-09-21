package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Transport TransportConfig `yaml:"transport"`
	Audit     AuditConfig     `yaml:"audit"`
	Masking   MaskingConfig   `yaml:"masking"`
	LLM       LLMConfig       `yaml:"llm"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Admin     AdminConfig     `yaml:"admin"`
}

type ServerConfig struct {
	ClientID string   `yaml:"client_id"`
	Upstream []string `yaml:"upstream"` // stdio upstream command + args
}

// TransportConfig selects how MCP Arc talks to the MCP client and to the upstream
// MCP server. "stdio" spawns/uses a subprocess; "sse" uses HTTP Server-Sent
// Events (the legacy MCP remote transport); "streamable-http" uses the MCP
// 2026-07-28 Streamable HTTP transport (single POST endpoint).
type TransportConfig struct {
	Client      string `yaml:"client"`       // stdio | sse | streamable-http  (how clients connect to MCP Arc)
	Listen      string `yaml:"listen"`       // address for the HTTP client transports, e.g. ":8081"
	Upstream    string `yaml:"upstream"`     // stdio | sse | streamable-http  (how MCP Arc connects to the real server)
	UpstreamURL string `yaml:"upstream_url"` // the upstream /sse or /mcp endpoint, when upstream = sse | streamable-http
	StreamableHTTPPath string `yaml:"streamable_http.path"` // client endpoint path for streamable-http, default /mcp
}

type AuditConfig struct {
	Enabled                bool   `yaml:"enabled"`
	Driver                 string `yaml:"driver"`                    // sqlite | postgres | memory
	DSN                    string `yaml:"dsn"`                       // ./mcp-arc.db
	QueueSize              int    `yaml:"queue_size"`                // audit write buffer; 0 → default 1024
	WriteTimeoutMs         int    `yaml:"write_timeout_ms"`          // per-insert wall-clock timeout; 0 → 2s
	ShutdownFlushTimeoutMs int    `yaml:"shutdown_flush_timeout_ms"` // max time to drain the queue on shutdown; 0 → 5s
}

type MaskingConfig struct {
	Enabled bool       `yaml:"enabled"`
	Rules   []MaskRule `yaml:"rules"`
	Presets []string   `yaml:"presets"` // names from the built-in preset library
}

type MaskRule struct {
	Name     string   `yaml:"name"`
	Patterns []string `yaml:"patterns"` // regex
	Fields   []string `yaml:"fields"`   // field-name match
	MaskChar string   `yaml:"mask_char"`
	// Enabled is a pointer so that an absent `enabled:` key means "on"; only an
	// explicit `enabled: false` disables a rule.
	Enabled *bool `yaml:"enabled"`
}

// IsEnabled reports whether the rule is active. Rules default to enabled.
func (r MaskRule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// LLMConfig drives the optional LLM-assisted masking pass. It is a second-pass
// net: static rules (regex + field names) run first, and the model is only asked
// to catch what they missed. Everything fails open — a slow or broken endpoint
// never blocks a tool call.
type LLMConfig struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint is an OpenAI-compatible chat completions URL. If it does not end
	// in /chat/completions, that path is appended.
	Endpoint string `yaml:"endpoint"`
	APIKey   string `yaml:"api_key"` // prefer MCP_ARC_LLM_API_KEY
	Model    string `yaml:"model"`
	// TimeoutMs bounds how long a tool call may wait for the model. Default 3000.
	TimeoutMs int `yaml:"timeout_ms"`
	// MaxBytes skips the LLM pass for params larger than this. Default 8192.
	MaxBytes int `yaml:"max_bytes"`
	// CacheTTLSecs / CacheEntries bound the in-memory result cache, keyed by a
	// hash of the payload, so repeated identical calls cost nothing.
	CacheTTLSecs int `yaml:"cache_ttl_seconds"`
	CacheEntries int `yaml:"cache_entries"`
	// ApplyToResult also runs the LLM pass over upstream results. Off by
	// default: results are larger and usually less sensitive than arguments.
	ApplyToResult bool `yaml:"apply_to_result"`
}

type RateLimitConfig struct {
	Enabled    bool    `yaml:"enabled"`
	QPS        float64 `yaml:"qps"`
	DailyQuota int     `yaml:"daily_quota"`
}

type AdminConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Port        int    `yaml:"port"`
	Token       string `yaml:"token"`
	OpenBrowser bool   `yaml:"open_browser"` // auto-open the web console in the default browser on startup
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&c)
	return &c, nil
}

func Default() *Config {
	c := Config{}
	applyDefaults(&c)
	return &c
}

func applyDefaults(c *Config) {
	if c.Audit.Driver == "" {
		c.Audit.Driver = "sqlite"
	}
	if c.Audit.DSN == "" {
		c.Audit.DSN = "./mcp-arc.db"
	}
	if c.Audit.QueueSize <= 0 {
		c.Audit.QueueSize = 1024
	}
	if c.Audit.WriteTimeoutMs <= 0 {
		c.Audit.WriteTimeoutMs = 2000
	}
	if c.Audit.ShutdownFlushTimeoutMs <= 0 {
		c.Audit.ShutdownFlushTimeoutMs = 5000
	}
	if c.Server.ClientID == "" {
		if v := os.Getenv("MCP_ARC_CLIENT_ID"); v != "" {
			c.Server.ClientID = v
		} else {
			c.Server.ClientID = "default"
		}
	}
	if c.Transport.Client == "" {
		c.Transport.Client = "stdio"
	}
	if c.Transport.Listen == "" {
		c.Transport.Listen = ":8081"
	}
	if c.Transport.StreamableHTTPPath == "" {
		c.Transport.StreamableHTTPPath = "/mcp"
	}
	if c.Transport.Upstream == "" {
		c.Transport.Upstream = "stdio"
	}
	if c.Admin.Port == 0 {
		c.Admin.Port = 8080
	}
	if c.RateLimit.QPS == 0 {
		c.RateLimit.QPS = 10
	}
	applyLLMDefaults(&c.LLM)
}

func applyLLMDefaults(l *LLMConfig) {
	if l.Endpoint == "" {
		l.Endpoint = os.Getenv("MCP_ARC_LLM_ENDPOINT")
	}
	if l.APIKey == "" {
		l.APIKey = os.Getenv("MCP_ARC_LLM_API_KEY")
	}
	if l.Model == "" {
		l.Model = "gpt-4o-mini"
	}
	if l.TimeoutMs <= 0 {
		l.TimeoutMs = 3000
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 8192
	}
	if l.CacheTTLSecs <= 0 {
		l.CacheTTLSecs = 300
	}
	if l.CacheEntries <= 0 {
		l.CacheEntries = 512
	}
}
