package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads and strictly parses the config file at path. Unknown keys at any
// nesting level are rejected (docs/configuration.md §8): this is what retires
// the superseded `device:` / `disk_id:` selectors (ADR-004) and the platform-
// fixed `billing_step_iops` (§2 field notes) as a side effect of strict
// decoding, without a dedicated check for either.
//
// Load does not call Validate — callers that need a fully checked config call
// both, in order, as LoadAndValidate does.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var peek modePeek
	if err := yaml.Unmarshal(data, &peek); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if peek.Mode != "local" {
		return nil, fmt.Errorf(
			"%s: mode %q is not supported in this build — fleet mode ships in Phase 4 (docs/prd-mws-iops-autoscaler.md §6)",
			path, peek.Mode)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	return &cfg, nil
}

// LoadAndValidate reads, parses and validates the config file at path.
func LoadAndValidate(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
