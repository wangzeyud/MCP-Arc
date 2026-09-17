package audit

import (
	"database/sql"
	"fmt"

	// modernc.org/sqlite is a pure-Go SQLite driver (no cgo), so the sidecar
	// builds and runs with CGO_ENABLED=0 — keeping audit/replay persistence
	// working in a single portable binary. It registers the "sqlite3" driver
	// name, so sql.Open below is unchanged.
	_ "modernc.org/sqlite"
)

// compile-time check: both backends satisfy the full Store contract.
var _ Store = (*SQLiteStore)(nil)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLite(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS calls (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		client_id   TEXT    NOT NULL,
		tool_name   TEXT    NOT NULL,
		params      TEXT,
		raw_params  TEXT,
		raw_result  TEXT,
		result      TEXT,
		error_msg   TEXT,
		latency_ms  INTEGER,
		timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_calls_client ON calls(client_id);
	CREATE INDEX IF NOT EXISTS idx_calls_tool   ON calls(tool_name);
	CREATE INDEX IF NOT EXISTS idx_calls_time   ON calls(timestamp);`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS mask_rules (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT    NOT NULL,
		patterns    TEXT,
		fields      TEXT,
		mask_char   TEXT,
		enabled     INTEGER NOT NULL DEFAULT 1,
		source      TEXT    NOT NULL DEFAULT 'ui',
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_mask_rules_name ON mask_rules(name);`); err != nil {
		return err
	}
	// Best-effort schema evolution for existing databases (column may already exist).
	for _, ddl := range []string{
		`ALTER TABLE calls ADD COLUMN raw_params TEXT`,
		`ALTER TABLE calls ADD COLUMN raw_result TEXT`,
	} {
		_, _ = s.db.Exec(ddl)
	}
	return nil
}

func (s *SQLiteStore) Insert(r *CallRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO calls (client_id, tool_name, params, raw_params, raw_result, result, error_msg, latency_ms, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ClientID, r.ToolName, r.Params, r.RawParams, r.RawResult, r.Result, r.ErrorMsg, r.LatencyMs, r.Timestamp,
	)
	return err
}

func (s *SQLiteStore) Query(opts QueryOpts) ([]CallRecord, error) {
	query := `SELECT id, client_id, tool_name, params, raw_params, raw_result, result, error_msg, latency_ms, timestamp
	          FROM calls WHERE 1=1`
	var args []any
	if opts.ClientID != "" {
		query += " AND client_id = ?"
		args = append(args, opts.ClientID)
	}
	if opts.ToolName != "" {
		query += " AND tool_name = ?"
		args = append(args, opts.ToolName)
	}
	if !opts.Since.IsZero() {
		query += " AND timestamp >= ?"
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += " AND timestamp <= ?"
		args = append(args, opts.Until)
	}
	query += " ORDER BY id DESC"
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CallRecord, 0)
	for rows.Next() {
		var r CallRecord
		if err := rows.Scan(&r.ID, &r.ClientID, &r.ToolName, &r.Params, &r.RawParams, &r.RawResult,
			&r.Result, &r.ErrorMsg, &r.LatencyMs, &r.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Get(id int64) (*CallRecord, error) {
	var r CallRecord
	err := s.db.QueryRow(
		`SELECT id, client_id, tool_name, params, raw_params, raw_result, result, error_msg, latency_ms, timestamp
		 FROM calls WHERE id = ?`, id,
	).Scan(&r.ID, &r.ClientID, &r.ToolName, &r.Params, &r.RawParams, &r.RawResult,
		&r.Result, &r.ErrorMsg, &r.LatencyMs, &r.Timestamp)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *SQLiteStore) Stats(opts StatsOpts) (*Stats, error) {
	stats := &Stats{ToolCounts: map[string]int64{}}
	query := `SELECT COUNT(*),
	                 COALESCE(SUM(CASE WHEN error_msg != '' THEN 1 ELSE 0 END), 0),
	                 COALESCE(AVG(latency_ms), 0)
	          FROM calls WHERE 1=1`
	var args []any
	if !opts.Since.IsZero() {
		query += " AND timestamp >= ?"
		args = append(args, opts.Since)
	}
	if !opts.Until.IsZero() {
		query += " AND timestamp <= ?"
		args = append(args, opts.Until)
	}
	if err := s.db.QueryRow(query, args...).Scan(&stats.TotalCalls, &stats.ErrorCount, &stats.AvgLatencyMs); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT tool_name, COUNT(*) FROM calls GROUP BY tool_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var cnt int64
		if err := rows.Scan(&name, &cnt); err != nil {
			return nil, err
		}
		stats.ToolCounts[name] = cnt
	}
	return stats, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// --- masking rules ---------------------------------------------------------

func (s *SQLiteStore) ListRules() ([]MaskRule, error) {
	rows, err := s.db.Query(`SELECT ` + ruleColumns + ` FROM mask_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRules(rows)
}

func (s *SQLiteStore) GetRule(id int64) (*MaskRule, error) {
	rows, err := s.db.Query(`SELECT `+ruleColumns+` FROM mask_rules WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	rules, err := scanRules(rows)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, sql.ErrNoRows
	}
	return &rules[0], nil
}

func (s *SQLiteStore) CreateRule(r *MaskRule) error {
	if r.Source == "" {
		r.Source = "ui"
	}
	res, err := s.db.Exec(
		`INSERT INTO mask_rules (name, patterns, fields, mask_char, enabled, source)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.Name, encodeStrings(r.Patterns), encodeStrings(r.Fields), r.MaskChar, boolToInt(r.Enabled), r.Source,
	)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *SQLiteStore) UpdateRule(r *MaskRule) error {
	_, err := s.db.Exec(
		`UPDATE mask_rules SET name = ?, patterns = ?, fields = ?, mask_char = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		r.Name, encodeStrings(r.Patterns), encodeStrings(r.Fields), r.MaskChar, boolToInt(r.Enabled), r.ID,
	)
	return err
}

func (s *SQLiteStore) DeleteRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM mask_rules WHERE id = ?`, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
