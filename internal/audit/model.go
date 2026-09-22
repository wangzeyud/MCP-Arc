package audit

import "time"

type CallRecord struct {
	ID        int64     `db:"id" json:"id"`
	ClientID  string    `db:"client_id" json:"client_id"`
	ToolName  string    `db:"tool_name" json:"tool_name"`
	Params    string    `db:"params" json:"params"`         // 脱敏后的参数 JSON
	RawParams string    `db:"raw_params" json:"raw_params"` // 原始（未脱敏）请求参数，用于回放
	Result    string    `db:"result" json:"result"`         // 脱敏后的结果 JSON
	RawResult string    `db:"raw_result" json:"raw_result"` // 原始（未脱敏）上游返回，用于回放/核对
	ErrorMsg  string    `db:"error_msg" json:"error_msg"`
	LatencyMs int64     `db:"latency_ms" json:"latency_ms"`
	Timestamp time.Time `db:"timestamp" json:"timestamp"`
	// ReplayOf is non-zero when this record was produced by a console replay,
	// and points at the original call it re-issued. It lets the audit log show
	// replays distinctly and trace them back to their source.
	ReplayOf int64 `db:"replay_of" json:"replay_of"`
}

type QueryOpts struct {
	ClientID string
	ToolName string
	Since    time.Time
	Until    time.Time
	Limit    int
}

type StatsOpts struct {
	Since time.Time
	Until time.Time
}

type Stats struct {
	TotalCalls   int64            `json:"total_calls"`
	ErrorCount   int64            `json:"error_count"`
	AvgLatencyMs float64          `json:"avg_latency_ms"`
	ToolCounts   map[string]int64 `json:"tool_counts"`
}

// Retention bounds how large the audit log may grow. Zero values disable the
// corresponding policy. It is consumed by Store.Prune (sqlite / postgres /
// memory) and the proxy's background pruner.
type Retention struct {
	MaxAgeDays int // delete records older than this many days (0 = disabled)
	MaxRows    int // keep at most this many newest records (0 = disabled)
}

// MaskRule is a masking rule as persisted in the `mask_rules` table.
//
// Rules are no longer config-only: the console can create, edit, enable/disable
// and delete them at runtime, and the masker hot-reloads. Rules seeded from
// config.yaml are marked Source="config" so operators can tell them apart from
// rules created in the UI (Source="ui") or suggested by the LLM pass.
type MaskRule struct {
	ID        int64     `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	Patterns  []string  `db:"patterns" json:"patterns"` // regex, JSON-encoded in the column
	Fields    []string  `db:"fields" json:"fields"`     // field-name match, JSON-encoded
	MaskChar  string    `db:"mask_char" json:"mask_char"`
	Enabled   bool      `db:"enabled" json:"enabled"`
	Source    string    `db:"source" json:"source"` // config | ui | llm
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// RuleStore persists masking rules. It is implemented by the same backends as
// CallStore so both share a single connection (and, for SQLite, one file lock).
type RuleStore interface {
	ListRules() ([]MaskRule, error)
	GetRule(id int64) (*MaskRule, error)
	CreateRule(r *MaskRule) error
	UpdateRule(r *MaskRule) error
	DeleteRule(id int64) error
}

// CallStore persists audit records.
type CallStore interface {
	Insert(r *CallRecord) error
	Query(opts QueryOpts) ([]CallRecord, error)
	Get(id int64) (*CallRecord, error)
	Stats(opts StatsOpts) (*Stats, error)
}

// Store is the persistence interface for audit records and masking rules.
type Store interface {
	CallStore
	RuleStore
	Close() error
}
