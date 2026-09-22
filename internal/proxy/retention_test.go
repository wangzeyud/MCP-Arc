package proxy

import (
	"context"
	"testing"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
)

// TestProxyRetentionStartupPrune verifies that startRetention trims records at
// startup according to the configured policy, using the in-memory store (no cgo /
// external database required). The periodic ticker is irrelevant here because the
// startup prune is synchronous.
func TestProxyRetentionStartupPrune(t *testing.T) {
	cfg := config.Default()
	cfg.Audit.Enabled = true
	cfg.Audit.Driver = "memory"
	cfg.Audit.Retention = config.RetentionConfig{MaxRows: 2}

	p := New(Options{Config: cfg})
	if p.auditStore == nil {
		t.Fatal("audit store not initialised")
	}
	for i := 0; i < 5; i++ {
		if err := p.auditStore.Insert(&audit.CallRecord{ClientID: "c", ToolName: "t"}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.startRetention(ctx)

	recs, err := p.auditStore.Query(audit.QueryOpts{Limit: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("after retention len=%d, want 2", len(recs))
	}
}

// TestProxyRetentionDisabled is a no-op when the policy is empty, so existing
// (unbounded) behaviour is preserved for operators who do not opt in.
func TestProxyRetentionDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Audit.Enabled = true
	cfg.Audit.Driver = "memory"

	p := New(Options{Config: cfg})
	for i := 0; i < 5; i++ {
		_ = p.auditStore.Insert(&audit.CallRecord{ClientID: "c", ToolName: "t"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.startRetention(ctx) // should not prune anything

	recs, _ := p.auditStore.Query(audit.QueryOpts{Limit: 100})
	if len(recs) != 5 {
		t.Fatalf("after no-op retention len=%d, want 5", len(recs))
	}
}
