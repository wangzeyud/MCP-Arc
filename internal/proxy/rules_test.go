package proxy

import (
	"errors"
	"testing"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
	"github.com/wangzeyud/mcp-arc/internal/mask"
)

// fakeRuleStore implements the parts of audit.Store the rule code touches, so
// hot reload can be tested without sqlite or postgres.
type fakeRuleStore struct {
	rules  []audit.MaskRule
	nextID int64
	err    error
}

func (f *fakeRuleStore) ListRules() ([]audit.MaskRule, error) { return f.rules, f.err }

func (f *fakeRuleStore) GetRule(id int64) (*audit.MaskRule, error) {
	for i := range f.rules {
		if f.rules[i].ID == id {
			r := f.rules[i]
			return &r, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeRuleStore) CreateRule(r *audit.MaskRule) error {
	if f.err != nil {
		return f.err
	}
	f.nextID++
	r.ID = f.nextID
	r.CreatedAt = time.Now()
	r.UpdatedAt = r.CreatedAt
	f.rules = append(f.rules, *r)
	return nil
}

func (f *fakeRuleStore) UpdateRule(r *audit.MaskRule) error {
	for i := range f.rules {
		if f.rules[i].ID == r.ID {
			r.UpdatedAt = time.Now()
			f.rules[i] = *r
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeRuleStore) DeleteRule(id int64) error {
	for i := range f.rules {
		if f.rules[i].ID == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeRuleStore) Insert(*audit.CallRecord) error                    { return nil }
func (f *fakeRuleStore) Query(audit.QueryOpts) ([]audit.CallRecord, error) { return nil, nil }
func (f *fakeRuleStore) Get(int64) (*audit.CallRecord, error)              { return nil, nil }
func (f *fakeRuleStore) Stats(audit.StatsOpts) (*audit.Stats, error)       { return nil, nil }
func (f *fakeRuleStore) Close() error                                      { return nil }

func TestSpecsFromConfigHonoursEnabledFlag(t *testing.T) {
	off := false
	specs := specsFromConfig([]config.MaskRule{
		{Name: "disabled", Fields: []string{"pwd"}, Enabled: &off},
		{Name: "default", Patterns: []string{`\d`}},
	})
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if specs[0].Enabled {
		t.Error("explicit enabled: false must produce a disabled spec")
	}
	if !specs[1].Enabled {
		t.Error("an absent enabled key must default to on")
	}
}

func TestValidateRule(t *testing.T) {
	cases := []struct {
		name    string
		rule    audit.MaskRule
		wantErr bool
	}{
		{"pattern", audit.MaskRule{Name: "a", Patterns: []string{`\d+`}}, false},
		{"field", audit.MaskRule{Name: "a", Fields: []string{"pwd"}}, false},
		{"missing name", audit.MaskRule{Patterns: []string{`\d+`}}, true},
		{"no pattern or field", audit.MaskRule{Name: "a"}, true},
		{"uncompilable regex", audit.MaskRule{Name: "a", Patterns: []string{"([unclosed"}}, true},
		{"uncompilable regex while disabled", audit.MaskRule{Name: "a", Patterns: []string{"([unclosed"}, Enabled: false}, true},
	}
	for _, c := range cases {
		rule := c.rule
		err := validateRule(&rule)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

func TestRuleCRUDHotReloads(t *testing.T) {
	m, err := mask.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRuleStore{}
	cfg := &config.Config{}
	cfg.Masking.Rules = []config.MaskRule{
		{Name: "email", Patterns: []string{`\S+@\S+`}, MaskChar: "***"},
	}
	p := &Proxy{opts: Options{Config: cfg}, auditStore: store, masker: m}

	// First run seeds config rules into the store.
	p.seedConfigRules()
	if len(store.rules) != 1 {
		t.Fatalf("expected 1 seeded rule, got %d", len(store.rules))
	}
	if store.rules[0].Source != "config" {
		t.Errorf("seeded rule should be marked source=config, got %q", store.rules[0].Source)
	}
	p.seedConfigRules() // must be idempotent
	if len(store.rules) != 1 {
		t.Fatalf("re-seeding duplicated rules: %d", len(store.rules))
	}

	if err := p.reloadRules(); err != nil {
		t.Fatal(err)
	}
	if out, _ := m.Mask(map[string]any{"mail": "a@b.com"}); out["mail"] != "***" {
		t.Errorf("seeded rule not active: %#v", out)
	}

	// Create through the API: persisted, then live.
	rule := &audit.MaskRule{
		Name:     "phone_cn",
		Patterns: []string{`\b1[3-9]\d{9}\b`},
		MaskChar: "[PHONE]",
		Enabled:  true,
	}
	if err := p.CreateRule(rule); err != nil {
		t.Fatal(err)
	}
	if rule.ID == 0 || rule.Source != "ui" {
		t.Errorf("created rule should carry an id and source=ui, got id=%d source=%q", rule.ID, rule.Source)
	}
	if rule.Fields == nil {
		t.Error("nil list fields should normalise to an empty slice, not null")
	}
	out, _ := m.Mask(map[string]any{"phone": "13800138000"})
	if out["phone"] != "[PHONE]" {
		t.Errorf("new rule did not hot-reload: %#v", out)
	}

	// Disable: gone on the next call, still in the store.
	rule.Enabled = false
	if err := p.UpdateRule(rule); err != nil {
		t.Fatal(err)
	}
	if out, _ := m.Mask(map[string]any{"phone": "13800138000"}); out["phone"] == "[PHONE]" {
		t.Error("a disabled rule must not apply after reload")
	}
	if len(store.rules) != 2 {
		t.Errorf("disabling should not delete the rule, store has %d", len(store.rules))
	}

	// Delete: gone from both.
	if err := p.DeleteRule(rule.ID); err != nil {
		t.Fatal(err)
	}
	if len(store.rules) != 1 {
		t.Errorf("expected 1 rule left, got %d", len(store.rules))
	}
}

func TestRuleAPIWithoutStore(t *testing.T) {
	p := &Proxy{}
	if _, err := p.ListRules(); !errors.Is(err, ErrNoRuleStore) {
		t.Errorf("expected ErrNoRuleStore, got %v", err)
	}
	if err := p.CreateRule(&audit.MaskRule{Name: "a", Fields: []string{"pwd"}}); !errors.Is(err, ErrNoRuleStore) {
		t.Errorf("expected ErrNoRuleStore, got %v", err)
	}
}

func TestUpdateRuleRequiresID(t *testing.T) {
	p := &Proxy{auditStore: &fakeRuleStore{}}
	if err := p.UpdateRule(&audit.MaskRule{Name: "a", Fields: []string{"pwd"}}); err == nil {
		t.Error("updating without an id should be rejected")
	}
}
