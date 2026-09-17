// Package mask redacts sensitive values in tool-call params and results.
//
// Two passes:
//  1. Static rules — regex patterns and/or field-name matches, configurable in
//     config.yaml or editable at runtime from the console.
//  2. An optional Detector (see SetDetector) — typically an LLM that catches
//     what the static rules missed (names, addresses, free-text secrets).
//
// The second pass is best-effort and fails open: if the detector errors or is
// slow, the statically masked value is used and the call proceeds.
package mask

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const DefaultMaskChar = "****"

// Spec is the serialisable form of a rule: it comes from config.yaml or from the
// `mask_rules` table, and is compiled into a Rule.
type Spec struct {
	Name     string
	Patterns []string
	Fields   []string
	MaskChar string
	Enabled  bool
}

// Rule is a compiled masking rule.
type Rule struct {
	Name     string
	Pattern  []*regexp.Regexp
	Fields   []string
	MaskChar string
	Enabled  bool
}

// Finding marks one path inside a params object as sensitive. Path is a dotted
// JSON path relative to the object root, with numeric indices for arrays, e.g.
// "user.email" or "items[1].note".
type Finding struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// Detector is the second-pass hook used by LLM-assisted masking. It receives the
// already statically-masked object and returns the paths still worth redacting.
type Detector interface {
	Detect(value map[string]any) ([]Finding, error)
}

// Masker applies rules to a params/result object. Rules can be swapped at
// runtime (Update) without disturbing in-flight calls.
type Masker struct {
	mu           sync.RWMutex
	rules        []Rule
	detector     Detector
	detectResult bool
}

// New builds a Masker from rule specs.
func New(specs []Spec) (*Masker, error) {
	m := &Masker{}
	if err := m.Update(specs); err != nil {
		return nil, err
	}
	return m, nil
}

// Update compiles specs and atomically swaps them in. Disabled rules are dropped
// so they cost nothing at mask time.
func (m *Masker) Update(specs []Spec) error {
	compiled := make([]Rule, 0, len(specs))
	for _, s := range specs {
		if !s.Enabled {
			continue
		}
		rule := Rule{Name: s.Name, Fields: s.Fields, MaskChar: s.MaskChar, Enabled: true}
		if rule.MaskChar == "" {
			rule.MaskChar = DefaultMaskChar
		}
		for _, p := range s.Patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				return fmt.Errorf("rule %q: compile pattern %q: %w", s.Name, p, err)
			}
			rule.Pattern = append(rule.Pattern, re)
		}
		compiled = append(compiled, rule)
	}
	m.mu.Lock()
	m.rules = compiled
	m.mu.Unlock()
	return nil
}

// SetDetector installs (or clears, with nil) the second-pass detector.
// applyToResult controls whether upstream results are also sent to it.
func (m *Masker) SetDetector(d Detector, applyToResult bool) {
	m.mu.Lock()
	m.detector = d
	m.detectResult = applyToResult
	m.mu.Unlock()
}

// Mask returns a deep-copied, masked version of params, running the detector
// second pass when configured.
func (m *Masker) Mask(params map[string]any) (map[string]any, error) {
	return m.mask(params, true)
}

// MaskResult masks an upstream result. The second pass is skipped unless it was
// explicitly enabled for results (results are bulkier than arguments).
func (m *Masker) MaskResult(result map[string]any) (map[string]any, error) {
	return m.mask(result, false)
}

func (m *Masker) mask(v map[string]any, isParams bool) (map[string]any, error) {
	out := deepCopy(v)
	if out == nil {
		return map[string]any{}, nil
	}
	m.mu.RLock()
	rules := m.rules
	det := m.detector
	wantDetect := m.detector != nil && (isParams || m.detectResult)
	m.mu.RUnlock()

	m.maskValue(out, "", rules)

	if wantDetect {
		// Best-effort: a detector failure must never block or unmask a call.
		if findings, err := det.Detect(out); err == nil {
			for _, f := range findings {
				if f.Path == "" {
					continue
				}
				setPath(out, f.Path, DefaultMaskChar)
			}
		}
	}
	return out, nil
}

func (m *Masker) maskValue(v any, path string, rules []Rule) any {
	switch val := v.(type) {
	case map[string]any:
		for k, v2 := range val {
			if matchField(rules, k) {
				val[k] = maskFieldValue(rules, v2)
			} else {
				val[k] = m.maskValue(v2, path+"."+k, rules)
			}
		}
		return val
	case []any:
		for i, item := range val {
			val[i] = m.maskValue(item, fmt.Sprintf("%s[%d]", path, i), rules)
		}
		return val
	case string:
		return maskString(rules, val)
	default:
		return v
	}
}

func matchField(rules []Rule, name string) bool {
	for _, rule := range rules {
		for _, f := range rule.Fields {
			if f == name {
				return true
			}
		}
	}
	return false
}

func maskFieldValue(rules []Rule, v any) any {
	if s, ok := v.(string); ok {
		if masked := maskString(rules, s); masked != s {
			return masked
		}
	}
	return DefaultMaskChar
}

func maskString(rules []Rule, s string) string {
	for _, rule := range rules {
		for _, re := range rule.Pattern {
			if re.MatchString(s) {
				return re.ReplaceAllString(s, rule.MaskChar)
			}
		}
	}
	return s
}

// setPath replaces the value at a dotted path (e.g. "user.email", "a[0].b").
// The path must resolve to something that already exists: a model may
// hallucinate one, and inventing a "ghost": "****" entry in the audit record
// would be worse than missing the redaction.
func setPath(root map[string]any, path string, val any) bool {
	toks := tokenizePath(path)
	if len(toks) == 0 {
		return false
	}
	var cur any = root
	for i, t := range toks {
		last := i == len(toks)-1
		switch node := cur.(type) {
		case map[string]any:
			if last {
				if _, ok := node[t]; !ok {
					return false
				}
				node[t] = val
				return true
			}
			cur = node[t]
		case []any:
			idx, err := strconv.Atoi(t)
			if err != nil || idx < 0 || idx >= len(node) {
				return false
			}
			if last {
				node[idx] = val
				return true
			}
			cur = node[idx]
		default:
			return false
		}
		if cur == nil {
			return false
		}
	}
	return false
}

// tokenizePath turns "a.b[0].c" into ["a","b","0","c"].
func tokenizePath(path string) []string {
	path = strings.TrimPrefix(path, "$")
	var out []string
	var cur strings.Builder
	for _, r := range path {
		switch r {
		case '.', '[', ']':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// deepCopy clones src via JSON. Numbers are kept in their literal form so the
// masked copy stored in the audit record stays numerically faithful to what the
// client sent — a float64 round trip would rewrite large integers.
func deepCopy(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	data, err := json.Marshal(src)
	if err != nil {
		return src
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var dst map[string]any
	if err := dec.Decode(&dst); err != nil {
		return src
	}
	return dst
}
