package registry

import (
	"testing"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
)

func testVM() *compute.VirtualMachine {
	return &compute.VirtualMachine{
		Name: "vm-1",
		Disks: []compute.StorageDisk{
			{Alias: "bootdisk", DeviceName: "bootdisk"},
			{Alias: "datadisk", DeviceName: "datadisk"},
			{Alias: "logdisk", DeviceName: "logdisk"},
		},
	}
}

func testResolved() *config.ResolvedConfig {
	vmPolicy := config.ResolvedPolicy{MinIOPS: 1000, MaxIOPS: 10000}
	diskPolicy := config.ResolvedPolicy{MinIOPS: 3000, MaxIOPS: 10000}
	return &config.ResolvedConfig{
		VMPolicy:     vmPolicy,
		DiskPolicies: map[string]config.ResolvedPolicy{"logdisk": diskPolicy},
	}
}

func TestSelect_autoDiscoveryReturnsEverythingNotExcluded(t *testing.T) {
	spec := config.DisksSpec{Mode: "auto-discovery", Exclude: []string{"bootdisk"}}
	managed, missing, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if len(managed) != 2 {
		t.Fatalf("managed = %+v, want 2 disks (bootdisk excluded)", managed)
	}
	names := map[string]bool{}
	for _, m := range managed {
		names[m.Disk.DeviceName] = true
	}
	if names["bootdisk"] {
		t.Error("bootdisk present in managed set despite being excluded")
	}
	if !names["datadisk"] || !names["logdisk"] {
		t.Errorf("managed set missing an expected disk: %v", names)
	}
}

func TestSelect_autoDiscoveryNoExcludeReturnsAll(t *testing.T) {
	spec := config.DisksSpec{Mode: "auto-discovery"}
	managed, _, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 3 {
		t.Errorf("managed = %d disks, want 3 (nothing excluded)", len(managed))
	}
}

func TestSelect_selectedReturnsOnlyNamedDisks(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "logdisk"},
	}}
	managed, missing, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if len(managed) != 2 {
		t.Fatalf("managed = %+v, want exactly the 2 selected disks", managed)
	}
}

func TestSelect_selectedReportsMissingWithoutFailingOthers(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "no-such-disk"},
	}}
	managed, missing, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 1 || managed[0].Disk.DeviceName != "datadisk" {
		t.Errorf("managed = %+v, want just datadisk", managed)
	}
	if len(missing) != 1 || missing[0] != "no-such-disk" {
		t.Errorf("missing = %v, want [no-such-disk]", missing)
	}
}

func TestSelect_policyFallsBackToVMPolicy(t *testing.T) {
	spec := config.DisksSpec{Mode: "auto-discovery"}
	managed, _, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for _, m := range managed {
		switch m.Disk.DeviceName {
		case "logdisk":
			if m.Policy.MinIOPS != 3000 {
				t.Errorf("logdisk policy = %+v, want the per-disk override (MinIOPS 3000)", m.Policy)
			}
		default:
			if m.Policy.MinIOPS != 1000 {
				t.Errorf("%s policy = %+v, want the VM-level fallback (MinIOPS 1000)", m.Disk.DeviceName, m.Policy)
			}
		}
	}
}

func TestSelect_unknownModeErrors(t *testing.T) {
	spec := config.DisksSpec{Mode: "bogus"}
	if _, _, err := Select(testVM(), spec, testResolved()); err == nil {
		t.Error("Select succeeded with an unknown disks.mode")
	}
}
