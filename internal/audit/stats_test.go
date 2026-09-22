package audit

import (
	"testing"
	"time"
)

func TestStatsPercentilesAndDimensions(t *testing.T) {
	s := NewMemory()
	base := time.Now().Add(-time.Hour).Truncate(time.Hour)
	recs := []CallRecord{
		{ToolName: "echo", ClientID: "c1", ErrorMsg: "", LatencyUs: 100, Timestamp: base},
		{ToolName: "echo", ClientID: "c1", ErrorMsg: "", LatencyUs: 200, Timestamp: base.Add(time.Minute)},
		{ToolName: "db", ClientID: "c2", ErrorMsg: "boom", LatencyUs: 1000, Timestamp: base.Add(2 * time.Minute)},
		{ToolName: "db", ClientID: "c2", ErrorMsg: "", LatencyUs: 50, Timestamp: base.Add(26 * time.Hour)},
	}
	for i := range recs {
		if err := s.Insert(&recs[i]); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.Stats(StatsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalCalls != 4 {
		t.Fatalf("total = %d, want 4", stats.TotalCalls)
	}
	if stats.ErrorCount != 1 {
		t.Fatalf("errors = %d, want 1", stats.ErrorCount)
	}
	if stats.ErrorRate < 0.24 || stats.ErrorRate > 0.26 {
		t.Fatalf("error rate = %v, want ~0.25", stats.ErrorRate)
	}
	if stats.LatencyP50Us != 100 {
		t.Fatalf("P50 = %d, want 100", stats.LatencyP50Us)
	}
	if stats.LatencyP95Us != 1000 {
		t.Fatalf("P95 = %d, want 1000", stats.LatencyP95Us)
	}
	if stats.LatencyP99Us != 1000 {
		t.Fatalf("P99 = %d, want 1000", stats.LatencyP99Us)
	}
	if stats.ToolCounts["echo"] != 2 || stats.ToolCounts["db"] != 2 {
		t.Fatalf("tool counts wrong: %v", stats.ToolCounts)
	}
	if stats.ClientCounts["c1"] != 2 || stats.ClientCounts["c2"] != 2 {
		t.Fatalf("client counts wrong: %v", stats.ClientCounts)
	}
	if len(stats.Series) != 2 {
		t.Fatalf("series buckets = %d, want 2 days", len(stats.Series))
	}
}
