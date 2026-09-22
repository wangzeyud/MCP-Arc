package audit

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
)

// compile-time check: both backends satisfy the full Store contract.
var _ Store = (*PostgresStore)(nil)

// PostgresStore persists audit records in PostgreSQL. It implements the same
// Store interface as the SQLite backend.
type PostgresStore struct {
	db *sql.DB
}

func NewPostgres(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &PostgresStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PostgresStore) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS calls (
		id          BIGSERIAL PRIMARY KEY,
		client_id   TEXT    NOT NULL,
		tool_name   TEXT    NOT NULL,
		params      TEXT,
		raw_params  TEXT,
		raw_result  TEXT,
		result      TEXT,
		error_msg   TEXT,
		latency_ms  BIGINT,
		timestamp   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_calls_client ON calls(client_id);
	CREATE INDEX IF NOT EXISTS idx_calls_tool   ON calls(tool_name);
	CREATE INDEX IF NOT EXISTS idx_calls_time   ON calls(timestamp);`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS mask_rules (
		id          BIGSERIAL PRIMARY KEY,
		name        TEXT    NOT NULL,
		patterns    TEXT,
		fields      TEXT,
		mask_char   TEXT,
		enabled     BOOLEAN NOT NULL DEFAULT TRUE,
		source      TEXT    NOT NULL DEFAULT 'ui',
		created_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		updated_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_mask_rules_name ON mask_rules(name);`); err != nil {
		return err
	}
	for _, ddl := range []string{
		`ALTER TABLE calls ADD COLUMN IF NOT EXISTS raw_params TEXT`,
		`ALTER TABLE calls ADD COLUMN IF NOT EXISTS raw_result TEXT`,
		`ALTER TABLE calls ADD COLUMN IF NOT EXISTS replay_of BIGINT`,
		`ALTER TABLE calls ADD COLUMN IF NOT EXISTS latency_us BIGINT`,
	} {
		if _, err := s.db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) Insert(r *CallRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO calls (client_id, tool_name, params, raw_params, raw_result, result, error_msg, latency_ms, latency_us, timestamp, replay_of)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.ClientID, r.ToolName, r.Params, r.RawParams, r.RawResult, r.Result, r.ErrorMsg, r.LatencyMs, r.LatencyUs, r.Timestamp, r.ReplayOf,
	)
	return err
}

func (s *PostgresStore) Query(opts QueryOpts) ([]CallRecord, error) {
	query := `SELECT id, client_id, tool_name, params, raw_params, raw_result, result, error_msg, latency_ms, latency_us, timestamp, replay_of
	          FROM calls WHERE 1=1`
	var args []any
	n := 0
	add := func(cond string, val any) {
		n++
		query += " " + strings.Replace(cond, "?", fmt.Sprintf("$%d", n), 1)
		args = append(args, val)
	}
	if opts.ClientID != "" {
		add("AND client_id = ?", opts.ClientID)
	}
	if opts.ToolName != "" {
		add("AND tool_name = ?", opts.ToolName)
	}
	if !opts.Since.IsZero() {
		add("AND timestamp >= ?", opts.Since)
	}
	if !opts.Until.IsZero() {
		add("AND timestamp <= ?", opts.Until)
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
			&r.Result, &r.ErrorMsg, &r.LatencyMs, &r.LatencyUs, &r.Timestamp, &r.ReplayOf); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(id int64) (*CallRecord, error) {
	var r CallRecord
	err := s.db.QueryRow(
		`SELECT id, client_id, tool_name, params, raw_params, raw_result, result, error_msg, latency_ms, latency_us, timestamp, replay_of
		 FROM calls WHERE id = $1`, id,
	).Scan(&r.ID, &r.ClientID, &r.ToolName, &r.Params, &r.RawParams, &r.RawResult,
		&r.Result, &r.ErrorMsg, &r.LatencyMs, &r.LatencyUs, &r.Timestamp, &r.ReplayOf)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PostgresStore) Stats(opts StatsOpts) (*Stats, error) {
	query := `SELECT latency_us, client_id, tool_name, error_msg, timestamp
	          FROM calls WHERE 1=1`
	var args []any
	n := 0
	add := func(cond string, val any) {
		n++
		query += " " + strings.Replace(cond, "?", fmt.Sprintf("$%d", n), 1)
		args = append(args, val)
	}
	if !opts.Since.IsZero() {
		add("AND timestamp >= ?", opts.Since)
	}
	if !opts.Until.IsZero() {
		add("AND timestamp <= ?", opts.Until)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recs := make([]statRow, 0)
	for rows.Next() {
		var r statRow
		var errMsg string
		if err := rows.Scan(&r.LatencyUs, &r.ClientID, &r.ToolName, &errMsg, &r.Timestamp); err != nil {
			return nil, err
		}
		r.Error = errMsg != ""
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return computeStats(recs), nil
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// Prune deletes old / excess audit records according to the retention policy.
// Both policies are best-effort and never block the request path. It returns the
// number of rows removed.
func (s *PostgresStore) Prune(r Retention) (int64, error) {
	var deleted int64
	if r.MaxAgeDays > 0 {
		res, err := s.db.Exec(`DELETE FROM calls WHERE timestamp < NOW() - ($1 || ' days')::interval`, r.MaxAgeDays)
		if err != nil {
			return deleted, err
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += n
		}
	}
	if r.MaxRows > 0 {
		res, err := s.db.Exec(`DELETE FROM calls WHERE id NOT IN (SELECT id FROM calls ORDER BY id DESC LIMIT $1)`, r.MaxRows)
		if err != nil {
			return deleted, err
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += n
		}
	}
	return deleted, nil
}

// --- masking rules ---------------------------------------------------------

func (s *PostgresStore) ListRules() ([]MaskRule, error) {
	rows, err := s.db.Query(`SELECT ` + ruleColumns + ` FROM mask_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRules(rows)
}

func (s *PostgresStore) GetRule(id int64) (*MaskRule, error) {
	rows, err := s.db.Query(`SELECT `+ruleColumns+` FROM mask_rules WHERE id = $1`, id)
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

func (s *PostgresStore) CreateRule(r *MaskRule) error {
	if r.Source == "" {
		r.Source = "ui"
	}
	return s.db.QueryRow(
		`INSERT INTO mask_rules (name, patterns, fields, mask_char, enabled, source)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		r.Name, encodeStrings(r.Patterns), encodeStrings(r.Fields), r.MaskChar, r.Enabled, r.Source,
	).Scan(&r.ID)
}

func (s *PostgresStore) UpdateRule(r *MaskRule) error {
	_, err := s.db.Exec(
		`UPDATE mask_rules SET name = $1, patterns = $2, fields = $3, mask_char = $4, enabled = $5, updated_at = CURRENT_TIMESTAMP
		 WHERE id = $6`,
		r.Name, encodeStrings(r.Patterns), encodeStrings(r.Fields), r.MaskChar, r.Enabled, r.ID,
	)
	return err
}

func (s *PostgresStore) DeleteRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM mask_rules WHERE id = $1`, id)
	return err
}
