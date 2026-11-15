package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_examples(t *testing.T) {
	// These fixtures are copied from the documented configuration examples.
	for _, name := range []string{"local-auto-discovery.yaml", "local-per-disk-policies.yaml"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("Load(%s): %v", name, err)
			}
			if cfg.Mode != "local" {
				t.Errorf("Mode = %q, want local", cfg.Mode)
			}
			if err := Validate(cfg); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestLoad_perDiskPolicies(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "local-per-disk-policies.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Disks.PolicyNames(); len(got) != 3 {
		t.Fatalf("Disks.PolicyNames() = %v, want 3 disks", got)
	}
	if p, ok := cfg.Disks.Policies["mws-datadisk"]; !ok || p != nil {
		t.Errorf("Policies[mws-datadisk] = %+v (present %v), want present with no override", p, ok)
	}
	logdisk := cfg.Disks.Policies["mws-logdisk"]
	if logdisk == nil || logdisk.Limits == nil || *logdisk.Limits.MinIOPS != 3000 {
		t.Errorf("Policies[mws-logdisk] = %+v, want limits.min_iops overridden to 3000", logdisk)
	}
}

func TestLoad_unknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
version: 1
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
totally_unknown_field: true
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded on a document with an unknown top-level key")
	}
}

func TestLoad_evaluationCollectionIntervalRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
version: 1
mode: local
policy:
  limits: {min_iops: 1000, max_iops: 10000}
  advanced:
    evaluation: {collection_interval: 10s, scale_up_window: 5m, scale_down_window: 30m}
disks: {mode: auto-discovery}
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with evaluation.collection_interval set — it must be rejected as an unknown key")
	}
}

func TestLoad_versionAndModeRequired(t *testing.T) {
	for name, header := range map[string]string{
		"missing version":     "mode: local\n",
		"unsupported version": "version: 2\nmode: local\n",
		"fractional version":  "version: 1.9\nmode: local\n",
		"version 1.0":         "version: 1.0\nmode: local\n",
		"quoted version":      "version: \"1\"\nmode: local\n",
		"missing mode":        "version: 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, path, header+"policy: {limits: {min_iops: 1000, max_iops: 10000}}\ndisks: {mode: auto-discovery}\n")
			if _, err := Load(path); err == nil {
				t.Fatalf("Load succeeded with %s", name)
			}
		})
	}
}

func TestLoad_repeatedPolicyKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
version: 1
mode: local
disks:
  mode: selected
  policies:
    mws-datadisk: {limits: {min_iops: 2000}}
    mws-datadisk: {limits: {min_iops: 3000}}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with the same disks.policies key twice — one override would be silently dropped")
	}
}

func TestLoad_multiDocumentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
version: 1
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
---
version: 1
mode: local
policy: {limits: {min_iops: 3000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded on a file with two YAML documents — the second was silently dropped")
	}
}

func TestLoad_supersededSelectedList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
version: 1
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks:
  mode: selected
  selected:
    - device_name: mws-datadisk
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted disks.selected, which is not part of the schema")
	}
}

func TestLoad_fleetModeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
version: 1
mode: fleet
project: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded on mode: fleet, which this build does not support")
	}
}

func TestLoad_missingFile(t *testing.T) {
	if _, err := Load("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("Load succeeded on a missing file")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
