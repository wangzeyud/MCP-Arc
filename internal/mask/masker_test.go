package mask

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTokenizePath(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a", []string{"a"}},
		{"a.b", []string{"a", "b"}},
		{"a.b[0].c", []string{"a", "b", "0", "c"}},
		{"items[12]", []string{"items", "12"}},
		{"$.user.email", []string{"user", "email"}}, // model likes "$." prefixes
		{"", nil},
		{"...", nil},
	}
	for _, c := range cases {
		if got := tokenizePath(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenizePath(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestSetPath(t *testing.T) {
	newRoot := func() map[string]any {
		return map[string]any{
			"user":  map[string]any{"email": "a@b.com"},
			"items": []any{map[string]any{"note": "x"}, "y"},
		}
	}

	root := newRoot()
	if !setPath(root, "user.email", "****") {
		t.Fatal("setPath on an existing key should succeed")
	}
	if root["user"].(map[string]any)["email"] != "****" {
		t.Errorf("user.email not replaced: %#v", root["user"])
	}

	if !setPath(root, "items[0].note", "****") {
		t.Error("setPath should walk into arrays")
	}
	if !setPath(root, "items[1]", "****") {
		t.Error("setPath should replace array elements")
	}
	if root["items"].([]any)[1] != "****" {
		t.Errorf("items[1] not replaced: %#v", root["items"])
	}

	// A hallucinated or overshooting path must be ignored, not panic or create
	// keys — an invented entry would pollute the audit record.
	for _, bad := range []string{"", "ghost", "nope.nope", "items[9]", "items[a]", "user.email.deeper"} {
		root := newRoot()
		if setPath(root, bad, "****") {
			t.Errorf("setPath(%q) should have been rejected", bad)
		}
		if _, invented := root["ghost"]; invented {
			t.Errorf("setPath(%q) created a key that was not there", bad)
		}
	}
}

func TestUpdateDropsDisabledRulesAndKeepsOldOnesOnError(t *testing.T) {
	m, err := New([]Spec{
		{Name: "on", Patterns: []string{`\d+`}, MaskChar: "X", Enabled: true},
		{Name: "off", Fields: []string{"pwd"}, Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := m.Mask(map[string]any{"n": "a1b", "pwd": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if out["n"] != "aXb" {
		t.Errorf("pattern rule not applied: %#v", out)
	}
	if out["pwd"] != "secret" {
		t.Errorf("disabled rule must not apply, got %#v", out["pwd"])
	}

	if err := m.Update([]Spec{{Name: "bad", Patterns: []string{"([unclosed"}, Enabled: true}}); err == nil {
		t.Fatal("expected a regex compile error")
	}
	out, _ = m.Mask(map[string]any{"n": "a1b"})
	if out["n"] != "aXb" {
		t.Error("a failed Update must leave the previous rules intact")
	}
}

// fakeDetector records what it was shown — the privacy property that matters is
// that the second pass only ever sees already-masked values.
type fakeDetector struct {
	findings []Finding
	err      error
	seen     map[string]any
	calls    int
}

func (f *fakeDetector) Detect(v map[string]any) ([]Finding, error) {
	f.calls++
	f.seen = v
	return f.findings, f.err
}

func TestDetectorRunsAfterStaticRules(t *testing.T) {
	m, err := New([]Spec{{Name: "fields", Fields: []string{"pwd"}, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	det := &fakeDetector{findings: []Finding{{Path: "note", Type: "person"}}}
	m.SetDetector(det, false)

	out, err := m.Mask(map[string]any{"pwd": "hunter2", "note": "call 张三"})
	if err != nil {
		t.Fatal(err)
	}
	if out["note"] != DefaultMaskChar {
		t.Errorf("detector finding not applied: %#v", out)
	}
	if det.seen["pwd"] != DefaultMaskChar {
		t.Errorf("detector must see the masked value, got %#v", det.seen["pwd"])
	}
}

func TestDetectorFailureFailsOpen(t *testing.T) {
	m, err := New([]Spec{{Name: "fields", Fields: []string{"pwd"}, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	det := &fakeDetector{err: errors.New("endpoint down")}
	m.SetDetector(det, false)

	out, err := m.Mask(map[string]any{"pwd": "hunter2", "note": "keep me"})
	if err != nil {
		t.Fatalf("detector failure must not surface as an error: %v", err)
	}
	if out["pwd"] != DefaultMaskChar {
		t.Errorf("static masking must survive a detector failure: %#v", out)
	}
	if out["note"] != "keep me" {
		t.Errorf("unexpected change to unrelated field: %#v", out)
	}
}

func TestDetectorIgnoresHallucinatedPaths(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetDetector(&fakeDetector{findings: []Finding{{Path: "ghost"}, {Path: ""}}}, false)

	out, err := m.Mask(map[string]any{"a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := out["ghost"]; leaked {
		t.Error("a hallucinated path must not create keys")
	}
	if len(out) != 1 {
		t.Errorf("unexpected keys: %#v", out)
	}
}

func TestDetectorSkippedForResultsUnlessEnabled(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	det := &fakeDetector{}
	m.SetDetector(det, false)
	if _, err := m.MaskResult(map[string]any{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if det.calls != 0 {
		t.Errorf("results should skip the second pass by default, got %d calls", det.calls)
	}

	m.SetDetector(det, true)
	if _, err := m.MaskResult(map[string]any{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if det.calls != 1 {
		t.Errorf("applyToResult should enable the second pass, got %d calls", det.calls)
	}
}

func TestMaskDeepCopies(t *testing.T) {
	m, err := New([]Spec{{Name: "fields", Fields: []string{"pwd"}, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"pwd": "hunter2"}
	out, err := m.Mask(in)
	if err != nil {
		t.Fatal(err)
	}
	if in["pwd"] != "hunter2" {
		t.Error("Mask must not mutate its input")
	}
	if out["pwd"] == "hunter2" {
		t.Error("Mask must redact in the copy")
	}
}

// Regression: maskString used to return after the first matching rule, so a
// single value holding several kinds of PII was only partially redacted — the
// earliest rule (email, first in config) won and the card number / API key next
// to it leaked verbatim into the audit record.
func TestMaskAppliesEveryMatchingRule(t *testing.T) {
	m, err := New([]Spec{
		{Name: "email", Patterns: []string{`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`}, MaskChar: "***@***.com", Enabled: true},
		{Name: "credit_card", Patterns: []string{`\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b`}, MaskChar: "****-****-****-****", Enabled: true},
		{Name: "api_key", Patterns: []string{`\b(?:sk-|pk-|api[_-]?key|token)[-_A-Za-z0-9]{10,}\b`}, MaskChar: "****", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	const raw = "联系方式 a@b.com，卡号 4111111111111111，api_key sk-abcdefghij123"
	out, err := m.Mask(map[string]any{"message": raw})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out["message"].(string)

	for _, leak := range []string{"a@b.com", "4111111111111111", "sk-abcdefghij123"} {
		if strings.Contains(got, leak) {
			t.Errorf("raw value %q leaked through masking: %q", leak, got)
		}
	}
	for _, want := range []string{"***@***.com", "****-****-****-****"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in masked output, got %q", want, got)
		}
	}
}

// Rule order must not decide which rules fire. With the old early-return bug only
// the first matching rule applied; here the same PII is fed with the rules
// reversed (api_key, credit_card, email) and all three must still be redacted.
func TestMaskOrderDoesNotSkipRules(t *testing.T) {
	m, err := New([]Spec{
		{Name: "api_key", Patterns: []string{`\b(?:sk-|pk-|api[_-]?key|token)[-_A-Za-z0-9]{10,}\b`}, MaskChar: "****", Enabled: true},
		{Name: "credit_card", Patterns: []string{`\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b`}, MaskChar: "****-****-****-****", Enabled: true},
		{Name: "email", Patterns: []string{`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`}, MaskChar: "***@***.com", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	const raw = "联系方式 a@b.com，卡号 4111111111111111，api_key sk-abcdefghij123"
	out, err := m.Mask(map[string]any{"message": raw})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := out["message"].(string)
	for _, leak := range []string{"a@b.com", "4111111111111111", "sk-abcdefghij123"} {
		if strings.Contains(got, leak) {
			t.Errorf("raw value %q leaked through masking: %q", leak, got)
		}
	}
}
