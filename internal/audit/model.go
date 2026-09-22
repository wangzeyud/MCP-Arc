package audit

import (
	"math"
	"sort"
	"time"
)

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
	LatencyUs int64     `db:"latency_us" json:"latency_us"`
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
	ErrorRate    float64          `json:"error_rate"`
	AvgLatencyMs float64          `json:"avg_latency_ms"`
	AvgLatencyUs float64          `json:"avg_latency_us"`
	LatencyP50Us int64            `json:"latency_p50_us"`
	LatencyP95Us int64            `json:"latency_p95_us"`
	LatencyP99Us int64            `json:"latency_p99_us"`
	ToolCounts   map[string]int64 `json:"tool_counts"`
	ClientCounts map[string]int64 `json:"client_counts,omitempty"`
	Series       []TimeBucket     `json:"series,omitempty"`
}

// TimeBucket is one point in the per-day call-count time series.
type TimeBucket struct {
	Bucket time.Time `db:"bucket" json:"bucket"`
	Count  int64     `json:"count"`
}

// statRow is the minimal per-record projection Stats needs; backends scan into it
// and hand it to computeStats so percentile/client/series math lives in one place.
type statRow struct {
	LatencyUs int64
	ClientID  string
	ToolName  string
	Error     bool
	Timestamp time.Time
}

// computeStats aggregates a window of audit records into summary statistics. It is
// backend-agnostic: sqlite, postgres and memory all feed it the same projection.
func computeStats(rows []statRow) *Stats {
	stats := &Stats{ToolCounts: map[string]int64{}, ClientCounts: map[string]int64{}}
	total := int64(len(rows))
	if total == 0 {
		return stats
	}
	lats := make([]int64, total)
	var sumUs int64
	for i, r := range rows {
		lats[i] = r.LatencyUs
		sumUs += r.LatencyUs
		if r.Error {
			stats.ErrorCount++
		}
		stats.ToolCounts[r.ToolName]++
		stats.ClientCounts[r.ClientID]++
	}
	stats.TotalCalls = total
	stats.ErrorRate = float64(stats.ErrorCount) / float64(total)
	stats.AvgLatencyUs = float64(sumUs) / float64(total)
	stats.AvgLatencyMs = stats.AvgLatencyUs / 1000
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	stats.LatencyP50Us = percentile(lats, 50)
	stats.LatencyP95Us = percentile(lats, 95)
	stats.LatencyP99Us = percentile(lats, 99)
	stats.Series = bucketByDay(rows)
	return stats
}

// percentile returns the p-th percentile (0..100) of an ascending-sorted slice
// using nearest-rank. p must be within (0,100]; returns 0 for empty input.
func percentile(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// bucketByDay groups records into per-day call counts for the time-series widget.
func bucketByDay(rows []statRow) []TimeBucket {
	m := map[string]*struct {
		t time.Time
		c int64
	}{}
	order := make([]string, 0)
	for _, r := range rows {
		day := r.Timestamp.Truncate(24 * time.Hour).UTC()
		key := day.Format("2006-01-02")
		if m[key] == nil {
			m[key] = &struct {
				t time.Time
				c int64
			}{t: day}
			order = append(order, key)
		}
		m[key].c++
	}
	out := make([]TimeBucket, 0, len(order))
	for _, k := range order {
		out = append(out, TimeBucket{Bucket: m[k].t, Count: m[k].c})
	}
	return out
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
