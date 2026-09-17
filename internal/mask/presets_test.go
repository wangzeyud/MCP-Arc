package mask

import "testing"

func TestPresetSpecsKnownAndUnknown(t *testing.T) {
	specs, err := PresetSpecs([]string{"phone_cn", "ip"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 presets, got %d", len(specs))
	}
	if _, err := PresetSpecs([]string{"phone_cn", "nope"}); err == nil {
		t.Error("expected error for unknown preset name")
	}
}

func TestPresetsMaskRealSamples(t *testing.T) {
	m, err := New([]Spec{Presets["phone_cn"], Presets["ip"], Presets["bank_card_cn"]})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in, want string
	}{
		{"call me at 13812345678", "call me at [PHONE]"},
		{"host 10.0.0.1:8080", "host [IP]:8080"},
		{"card 6222021234567890 ok", "card [BANKCARD] ok"},
	}
	for _, c := range cases {
		out, err := m.Mask(map[string]any{"text": c.in})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := out["text"].(string)
		if got != c.want {
			t.Errorf("Mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPresetSpecsCompiles(t *testing.T) {
	// Guard against a hand-edited preset with an invalid regex.
	specs, err := PresetSpecs([]string{"phone_cn", "ip", "bank_card_cn", "passport", "mac"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(specs); err != nil {
		t.Fatalf("preset library failed to compile: %v", err)
	}
}
