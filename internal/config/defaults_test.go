package config

import "testing"

func TestDefaultAdvanced_satisfiesValidation(t *testing.T) {
	if errs := validateResolvedPolicy("default", defaultResolvedPolicy()); len(errs) > 0 {
		t.Errorf("the built-in parameter set fails its own validation: %v", errs)
	}
}
