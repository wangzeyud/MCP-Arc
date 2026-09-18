package proxy

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/audit"
	"github.com/wangzeyud/mcp-arc/internal/config"
	"github.com/wangzeyud/mcp-arc/internal/llm"
	"github.com/wangzeyud/mcp-arc/internal/mask"
)

// ErrNoRuleStore is returned by the rule CRUD API when no database is available.
var ErrNoRuleStore = errors.New("no rule store: masking rules are read-only from config.yaml unless a sqlite/postgres backend is configured")

// specsFromConfig converts config.yaml rules into masking specs.
func specsFromConfig(rules []config.MaskRule) []mask.Spec {
	out := make([]mask.Spec, 0, len(rules))
	for _, r := range rules {
		out = append(out, mask.Spec{
			Name:     r.Name,
			Patterns: r.Patterns,
			Fields:   r.Fields,
			MaskChar: r.MaskChar,
			Enabled:  r.IsEnabled(),
		})
	}
	return out
}

// configSpecs merges config.yaml rules with any referenced presets into masking
// specs. User rules come first so they win on any overlap with a preset.
func (p *Proxy) configSpecs() []mask.Spec {
	specs := specsFromConfig(p.opts.Config.Masking.Rules)
	if len(p.opts.Config.Masking.Presets) == 0 {
		return specs
	}
	presets, err := mask.PresetSpecs(p.opts.Config.Masking.Presets)
	if err != nil {
		log.Printf("warn: %v", err)
		return specs
	}
	return append(specs, presets...)
}

func specOf(r audit.MaskRule) mask.Spec {
	return mask.Spec{
		Name:     r.Name,
		Patterns: r.Patterns,
		Fields:   r.Fields,
		MaskChar: r.MaskChar,
		Enabled:  r.Enabled,
	}
}

// seedConfigRules copies config.yaml rules into the rules table the first time
// the database is used, so the console starts from what the operator already
// wrote and can then take over from there.
func (p *Proxy) seedConfigRules() {
	if p.auditStore == nil {
		return
	}
	specs := p.configSpecs()
	if len(specs) == 0 {
		return
	}
	existing, err := p.auditStore.ListRules()
	if err != nil {
		log.Printf("warn: could not read masking rules: %v", err)
		return
	}
	if len(existing) > 0 {
		return // already seeded; config no longer owns the rules
	}
	for _, s := range specs {
		if !s.Enabled {
			continue
		}
		rule := &audit.MaskRule{
			Name:     s.Name,
			Patterns: s.Patterns,
			Fields:   s.Fields,
			MaskChar: s.MaskChar,
			Enabled:  true,
			Source:   "config",
		}
		if err := p.auditStore.CreateRule(rule); err != nil {
			log.Printf("warn: seed rule %q: %v", s.Name, err)
		}
	}
}

// reloadRules re-reads the rules table into the live masker. Called at startup
// and after every console edit.
func (p *Proxy) reloadRules() error {
	if p.masker == nil {
		return nil
	}
	if p.auditStore == nil {
		return p.masker.Update(p.configSpecs())
	}
	rules, err := p.auditStore.ListRules()
	if err != nil {
		return err
	}
	specs := make([]mask.Spec, 0, len(rules))
	for _, r := range rules {
		specs = append(specs, specOf(r))
	}
	return p.masker.Update(specs)
}

// initDetector wires the optional LLM second pass. A broken configuration only
// disables the second pass — static masking keeps working.
func (p *Proxy) initDetector(m *mask.Masker) {
	cfg := p.opts.Config.LLM
	if !cfg.Enabled {
		return
	}
	client, err := llm.New(llm.Config{
		Endpoint:      cfg.Endpoint,
		APIKey:        cfg.APIKey,
		Model:         cfg.Model,
		Timeout:       time.Duration(cfg.TimeoutMs) * time.Millisecond,
		MaxBytes:      cfg.MaxBytes,
		CacheTTL:      time.Duration(cfg.CacheTTLSecs) * time.Second,
		CacheMax:      cfg.CacheEntries,
		ApplyToResult: cfg.ApplyToResult,
	})
	if err != nil {
		log.Printf("warn: llm masking disabled: %v", err)
		return
	}
	m.SetDetector(client, cfg.ApplyToResult)
	log.Printf("mcp-arc: llm-assisted masking enabled (model=%s)", cfg.Model)
}

// --- RuleManager (admin API + console) --------------------------------------

// ListRules returns every rule in the rules table.
func (p *Proxy) ListRules() ([]audit.MaskRule, error) {
	if p.auditStore == nil {
		return nil, ErrNoRuleStore
	}
	return p.auditStore.ListRules()
}

// GetRule returns a single rule by id.
func (p *Proxy) GetRule(id int64) (*audit.MaskRule, error) {
	if p.auditStore == nil {
		return nil, ErrNoRuleStore
	}
	return p.auditStore.GetRule(id)
}

// CreateRule validates, persists and activates a new rule.
func (p *Proxy) CreateRule(r *audit.MaskRule) error {
	if p.auditStore == nil {
		return ErrNoRuleStore
	}
	if err := validateRule(r); err != nil {
		return err
	}
	if r.Source == "" {
		r.Source = "ui"
	}
	normalizeRule(r)
	if err := p.auditStore.CreateRule(r); err != nil {
		return err
	}
	if err := p.reloadRules(); err != nil {
		return err
	}
	return p.refreshRule(r)
}

// UpdateRule validates, persists and re-activates an edited rule.
func (p *Proxy) UpdateRule(r *audit.MaskRule) error {
	if p.auditStore == nil {
		return ErrNoRuleStore
	}
	if r.ID == 0 {
		return errors.New("rule id is required")
	}
	if err := validateRule(r); err != nil {
		return err
	}
	normalizeRule(r)
	if err := p.auditStore.UpdateRule(r); err != nil {
		return err
	}
	if err := p.reloadRules(); err != nil {
		return err
	}
	return p.refreshRule(r)
}

// refreshRule re-reads a saved rule so API responses carry the persisted
// timestamps and source rather than the caller's echo.
func (p *Proxy) refreshRule(r *audit.MaskRule) error {
	saved, err := p.auditStore.GetRule(r.ID)
	if err != nil {
		return nil // saved fine, just couldn't read back
	}
	*r = *saved
	return nil
}

// normalizeRule keeps list fields non-nil so JSON responses render [] not null.
func normalizeRule(r *audit.MaskRule) {
	if r.Patterns == nil {
		r.Patterns = []string{}
	}
	if r.Fields == nil {
		r.Fields = []string{}
	}
}

// DeleteRule removes a rule and reloads.
func (p *Proxy) DeleteRule(id int64) error {
	if p.auditStore == nil {
		return ErrNoRuleStore
	}
	if err := p.auditStore.DeleteRule(id); err != nil {
		return err
	}
	return p.reloadRules()
}

// validateRule rejects rules that cannot do anything useful or would fail to
// compile at mask time. Compilation is checked by building a throwaway masker.
func validateRule(r *audit.MaskRule) error {
	if r.Name == "" {
		return errors.New("rule name is required")
	}
	if len(r.Patterns) == 0 && len(r.Fields) == 0 {
		return fmt.Errorf("rule %q needs at least one pattern or field", r.Name)
	}
	spec := specOf(*r)
	spec.Enabled = true // validate the regexes even if the rule is saved disabled
	if _, err := mask.New([]mask.Spec{spec}); err != nil {
		return err
	}
	return nil
}
