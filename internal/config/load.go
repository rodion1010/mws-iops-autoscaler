package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// supportedAPIVersion / supportedKindLocal are the only apiVersion/kind
// values Load accepts for a mode: local document (docs/configuration.md
// §6.1, §6.2 examples). kind: IOPSAutoScalerFleet exists for mode: fleet
// (§6.3) but is unreachable here — the mode check above already rejects
// anything but local — so a mismatched kind on a local-mode document (e.g.
// mode: local left in place under a copy-pasted fleet kind) is its own,
// distinct error rather than being caught only incidentally by the mode
// check.
const (
	supportedAPIVersion = "autoscaling.mws.cloud/v1alpha1"
	supportedKindLocal  = "IOPSAutoScaler"
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

	// A second `---`-separated document is silently dropped by a single
	// Decode call — Decode only ever reads the first one. Reject it rather
	// than acting on a config that isn't the whole file.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s: contains more than one YAML document — only one is supported", path)
		}
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if cfg.APIVersion != supportedAPIVersion {
		return nil, fmt.Errorf("%s: apiVersion %q is not supported — expected %q", path, cfg.APIVersion, supportedAPIVersion)
	}
	if cfg.Kind != supportedKindLocal {
		return nil, fmt.Errorf("%s: kind %q is not supported for mode: local — expected %q", path, cfg.Kind, supportedKindLocal)
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
