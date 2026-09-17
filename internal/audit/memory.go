package audit

import (
	"slices"
	"sync"
	"time"
)

// MemoryStore is an in-process implementation of Store. It backs demos and CI
// runs where a real database is unavailable: sqlite needs cgo (unavailable when
// CGO_ENABLED=0) and postgres needs a running server. All data lives only for
// the lifetime of the process and is lost on exit — fine for a local preview,
// but never use it for production audit retention.
type MemoryStore struct {
	mu      sync.Mutex
	callSeq int64
	calls   []CallRecord
	ruleSeq int64
	rules   []MaskRule
}

// NewMemory returns an empty in-memory store.
func NewMemory() *MemoryStore { return &MemoryStore{} }

// compile-time check: MemoryStore satisfies the full Store contract.
var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) Insert(r *CallRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callSeq++
	r.ID = s.callSeq
	s.calls = append(s.calls, *r)
	return nil
}

func (s *MemoryStore) Query(opts QueryOpts) ([]CallRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CallRecord, 0, len(s.calls))
	// newest first, mirroring the sqlite backend's ORDER BY id DESC.
	for _, r := range slices.Backward(s.calls) {

		if opts.ClientID != "" && r.ClientID != opts.ClientID {
			continue
		}
		if opts.ToolName != "" && r.ToolName != opts.ToolName {
			continue
		}
		if !opts.Since.IsZero() && r.Timestamp.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && r.Timestamp.After(opts.Until) {
			continue
		}
		out = append(out, r)
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) Get(id int64) (*CallRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.calls {
		if s.calls[i].ID == id {
			r := s.calls[i]
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

func (s *MemoryStore) Stats(opts StatsOpts) (*Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := &Stats{ToolCounts: map[string]int64{}}
	for i := range s.calls {
		r := s.calls[i]
		if !opts.Since.IsZero() && r.Timestamp.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && r.Timestamp.After(opts.Until) {
			continue
		}
		stats.TotalCalls++
		if r.ErrorMsg != "" {
			stats.ErrorCount++
		}
		stats.AvgLatencyMs += float64(r.LatencyMs)
		stats.ToolCounts[r.ToolName]++
	}
	if stats.TotalCalls > 0 {
		stats.AvgLatencyMs /= float64(stats.TotalCalls)
	}
	return stats, nil
}

func (s *MemoryStore) ListRules() ([]MaskRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MaskRule, len(s.rules))
	copy(out, s.rules)
	return out, nil
}

func (s *MemoryStore) GetRule(id int64) (*MaskRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			r := s.rules[i]
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

func (s *MemoryStore) CreateRule(r *MaskRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ruleSeq++
	r.ID = s.ruleSeq
	now := time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	if r.Source == "" {
		r.Source = "ui"
	}
	s.rules = append(s.rules, *r)
	return nil
}

func (s *MemoryStore) UpdateRule(r *MaskRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == r.ID {
			r.UpdatedAt = time.Now()
			s.rules[i] = *r
			return nil
		}
	}
	return ErrNotFound
}

func (s *MemoryStore) DeleteRule(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (s *MemoryStore) Close() error { return nil }
