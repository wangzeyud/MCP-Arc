package audit

import (
	"path/filepath"
	"testing"
	"time"
)

// newTempSQLite builds a throwaway SQLite store under t.TempDir so retention can
// be exercised without touching a shared database. modernc.org/sqlite is pure-Go,
// so this works even with CGO_ENABLED=0.
func newTempSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLite(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLitePruneByAge(t *testing.T) {
	s := newTempSQLite(t)
	now := time.Now()
	// Records at 0,10,20,30,40,...,90 days old. MaxAgeDays=30 deletes those
	// strictly older than 30 days (i=4..9 => 6 records).
	for i := 0; i < 10; i++ {
		r := &CallRecord{ClientID: "c", ToolName: "t", Timestamp: now.Add(-time.Duration(i*10) * 24 * time.Hour)}
		if err := s.Insert(r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := s.Prune(Retention{MaxAgeDays: 30})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 6 {
		t.Fatalf("deleted=%d, want 6", n)
	}
	recs, err := s.Query(QueryOpts{Limit: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("remaining=%d, want 4", len(recs))
	}
}

func TestSQLitePruneByRows(t *testing.T) {
	s := newTempSQLite(t)
	for i := 0; i < 10; i++ {
		if err := s.Insert(&CallRecord{ClientID: "c", ToolName: "t"}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := s.Prune(Retention{MaxRows: 3})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 7 {
		t.Fatalf("deleted=%d, want 7", n)
	}
	recs, _ := s.Query(QueryOpts{Limit: 100})
	if len(recs) != 3 {
		t.Fatalf("remaining=%d, want 3", len(recs))
	}
}

func TestSQLitePruneDisabled(t *testing.T) {
	s := newTempSQLite(t)
	for i := 0; i < 5; i++ {
		_ = s.Insert(&CallRecord{ClientID: "c", ToolName: "t"})
	}
	n, err := s.Prune(Retention{}) // both policies disabled
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 0 {
		t.Fatalf("deleted=%d, want 0", n)
	}
}

func TestMemoryPruneByAge(t *testing.T) {
	s := NewMemory()
	now := time.Now()
	for i := 0; i < 10; i++ {
		r := &CallRecord{ClientID: "c", ToolName: "t", Timestamp: now.Add(-time.Duration(i*10) * 24 * time.Hour)}
		if err := s.Insert(r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := s.Prune(Retention{MaxAgeDays: 30})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 6 {
		t.Fatalf("deleted=%d, want 6", n)
	}
	recs, _ := s.Query(QueryOpts{Limit: 100})
	if len(recs) != 4 {
		t.Fatalf("remaining=%d, want 4", len(recs))
	}
}

func TestMemoryPruneByRows(t *testing.T) {
	s := NewMemory()
	for i := 0; i < 10; i++ {
		if err := s.Insert(&CallRecord{ClientID: "c", ToolName: "t"}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := s.Prune(Retention{MaxRows: 3})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 7 {
		t.Fatalf("deleted=%d, want 7", n)
	}
	recs, _ := s.Query(QueryOpts{Limit: 100})
	if len(recs) != 3 {
		t.Fatalf("remaining=%d, want 3", len(recs))
	}
}
