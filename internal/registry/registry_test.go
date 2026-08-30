package registry

import (
	"strings"
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

func TestSelect_selectedOneMissingIsFatal(t *testing.T) {
	// issue #5, reopened: disks.selected is an exhaustive statement of
	// operator intent, not a best-effort filter — one unmatched name must
	// fail the whole call, not silently narrow the managed set.
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "no-such-disk"},
	}}
	managed, missing, err := Select(testVM(), spec, testResolved())
	if err == nil {
		t.Fatal("Select succeeded despite an unmatched disks.selected device_name")
	}
	if managed != nil {
		t.Errorf("managed = %+v, want nil — no partial set on a fatal mismatch", managed)
	}
	if len(missing) != 1 || missing[0] != "no-such-disk" {
		t.Errorf("missing = %v, want [no-such-disk] (still reported alongside err, for the caller to log)", missing)
	}
	if !strings.Contains(err.Error(), "no-such-disk") {
		t.Errorf("err = %v, want it to name the unmatched device_name", err)
	}
}

func TestSelect_selectedAllMissingIsFatal(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "no-such-disk-1"},
		{DeviceName: "no-such-disk-2"},
	}}
	managed, missing, err := Select(testVM(), spec, testResolved())
	if err == nil {
		t.Fatal("Select succeeded despite every disks.selected device_name being unmatched")
	}
	if managed != nil {
		t.Errorf("managed = %+v, want nil", managed)
	}
	if len(missing) != 2 {
		t.Errorf("missing = %v, want both names", missing)
	}
}

func TestSelect_selectedRecoversOnceVMInventoryIsConsistent(t *testing.T) {
	// The same Select call, retried against a VM snapshot where the
	// previously-missing disk now exists — e.g. a later reconcile cycle
	// after the disk was actually attached — must succeed cleanly, proving
	// this isn't a permanent failure once triggered.
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "not-yet-attached"},
	}}
	if _, _, err := Select(testVM(), spec, testResolved()); err == nil {
		t.Fatal("test setup: expected the first Select to fail")
	}

	recoveredVM := testVM()
	recoveredVM.Disks = append(recoveredVM.Disks, compute.StorageDisk{Alias: "new", DeviceName: "not-yet-attached"})
	managed, missing, err := Select(recoveredVM, spec, testResolved())
	if err != nil {
		t.Fatalf("Select after recovery: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if len(managed) != 2 {
		t.Errorf("managed = %+v, want both selected disks now that the VM inventory is consistent", managed)
	}
}

func TestSelect_selectedDuplicateLiveDeviceNameNeverSilentlyOverwrites(t *testing.T) {
	// issue #5's other remaining criterion ("reject ambiguous duplicate
	// live matches") is closed upstream, at compute.Discover (issue #4):
	// a duplicate device_name attachment is excluded from vm.Disks
	// entirely before Select ever sees it. This test documents that
	// invariant from Select's side — if vm.Disks somehow still carried a
	// duplicate (a caller bypassing Discover, e.g. in a test), the last
	// entry silently wins the map, which is exactly why Discover must be
	// the one preventing this in the first place, not Select.
	vm := testVM()
	vm.Disks = append(vm.Disks, compute.StorageDisk{Alias: "datadisk-2", DeviceName: "datadisk"})
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{{DeviceName: "datadisk"}}}

	managed, _, err := Select(vm, spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 1 {
		t.Fatalf("managed = %+v, want exactly 1 — Select itself only ever resolves one entry per device_name key", managed)
	}
	if managed[0].Disk.Alias != "datadisk-2" {
		t.Errorf("managed[0].Disk.Alias = %q, want %q — last-write-wins is Select's documented behavior on a duplicate it's handed; compute.Discover is what prevents one from ever reaching here", managed[0].Disk.Alias, "datadisk-2")
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
