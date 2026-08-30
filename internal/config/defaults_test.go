package config

import "testing"

func TestPresets_allSatisfyValidation(t *testing.T) {
	for name, advanced := range presets {
		t.Run(string(name), func(t *testing.T) {
			rp := ResolvedPolicy{MinIOPS: DefaultMinIOPS, MaxIOPS: DefaultMaxIOPS, Advanced: advanced}
			if errs := validateResolvedPolicy("presets["+string(name)+"]", rp); len(errs) > 0 {
				t.Errorf("preset %q fails its own validation: %v", name, errs)
			}
		})
	}
}

func TestDefaultAdvanced_isBalancedPreset(t *testing.T) {
	if defaultAdvanced != presets[PresetBalanced] {
		t.Errorf("defaultAdvanced = %+v, want presets[balanced] %+v", defaultAdvanced, presets[PresetBalanced])
	}
}
