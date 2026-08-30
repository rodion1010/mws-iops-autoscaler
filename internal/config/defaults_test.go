package config

import "testing"

// TestPresets_allSatisfyValidation guards the registry itself, not just what
// Phase 1 selects from it: performance and cost are not reachable from any
// config yet (docs/configuration.md §1), but they are laid out for Phase 5, and this is
// exactly the check that would have caught the first draft's performance
// (2m scale_up_window / 3m cooldown.scale_up) before it shipped anywhere.
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
