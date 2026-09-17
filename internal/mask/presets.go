package mask

import "fmt"

// Presets is a built-in library of common PII masking templates. They are NOT
// enabled automatically — reference them by name from config.yaml
// (`masking.presets: [phone_cn, ip, ...]`). This keeps behaviour config-driven
// while sparing operators from re-deriving well-known patterns.
//
// Patterns are deliberately high-precision: mobile numbers, IPs, UnionPay cards,
// passports and MACs. Free-text PII such as names or street addresses is
// excluded on purpose — regex over-masks those and corrupts legitimate
// payloads; the optional LLM second pass exists precisely for that gap.
var Presets = map[string]Spec{
	"phone_cn": {
		Name:     "phone_cn",
		Patterns: []string{`\b1[3-9]\d{9}\b`},
		MaskChar: "[PHONE]",
		Enabled:  true,
	},
	"ip": {
		Name:     "ip",
		Patterns: []string{`\b(?:\d{1,3}\.){3}\d{1,3}\b`},
		MaskChar: "[IP]",
		Enabled:  true,
	},
	"bank_card_cn": {
		Name:     "bank_card_cn",
		Patterns: []string{`\b62\d{14,17}\b`},
		MaskChar: "[BANKCARD]",
		Enabled:  true,
	},
	"passport": {
		Name:     "passport",
		Patterns: []string{`\b[A-Za-z][0-9]{7,9}\b`},
		MaskChar: "[PASSPORT]",
		Enabled:  true,
	},
	"mac": {
		Name:     "mac",
		Patterns: []string{`\b(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}\b`},
		MaskChar: "[MAC]",
		Enabled:  true,
	},
}

// PresetSpecs returns specs for the named presets, in the given order. An unknown
// name is rejected so a typo fails fast instead of silently masking nothing.
func PresetSpecs(names []string) ([]Spec, error) {
	out := make([]Spec, 0, len(names))
	for _, n := range names {
		p, ok := Presets[n]
		if !ok {
			return nil, fmt.Errorf("unknown masking preset %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}
