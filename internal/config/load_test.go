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
	if len(cfg.Disks.Selected) != 3 {
		t.Fatalf("len(Disks.Selected) = %d, want 3", len(cfg.Disks.Selected))
	}
	if cfg.Disks.Selected[0].DeviceName != "datadisk" || cfg.Disks.Selected[0].Policy != nil {
		t.Errorf("Selected[0] = %+v, want device_name=datadisk with no override", cfg.Disks.Selected[0])
	}
	logdisk := cfg.Disks.Selected[1]
	if logdisk.DeviceName != "logdisk" || logdisk.Policy == nil || logdisk.Policy.Limits == nil || *logdisk.Policy.Limits.MinIOPS != 3000 {
		t.Errorf("Selected[1] = %+v, want logdisk overriding limits.min_iops to 3000", logdisk)
	}
}

func TestLoad_unknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
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
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local
policy:
  limits: {min_iops: 1000, max_iops: 10000}
  advanced:
    enabled: true
    evaluation: {collection_interval: 10s, scale_up_window: 5m, scale_down_window: 30m}
disks: {mode: auto-discovery}
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with evaluation.collection_interval set — it must be rejected as an unknown key (docs/design.md §4.2)")
	}
}

func TestLoad_wrongAPIVersionRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
apiVersion: autoscaling.mws.cloud/v1
kind: IOPSAutoScaler
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with an unsupported apiVersion")
	}
}

func TestLoad_wrongKindRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScalerFleet
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with kind: IOPSAutoScalerFleet under mode: local")
	}
}

func TestLoad_multiDocumentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
---
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local
policy: {limits: {min_iops: 3000, max_iops: 10000}}
disks: {mode: auto-discovery}
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded on a file with two YAML documents — the second was silently dropped")
	}
}

func TestLoad_supersededDeviceSelector(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local
policy: {limits: {min_iops: 1000, max_iops: 10000}}
disks:
  mode: selected
  selected:
    - device: /dev/vdb
runtime: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded on the superseded device: selector")
	}
}

func TestLoad_fleetModeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScalerFleet
mode: fleet
project: {}
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded on mode: fleet, which Phase 1 does not support")
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
