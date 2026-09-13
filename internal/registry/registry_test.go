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
	managed, degraded, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
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

func TestSelect_autoDiscoveryExcludeTargetNotFoundIsDegradedNotFatal(t *testing.T) {
	// A typo in exclude must not be indistinguishable from the harmless
	// "nothing to exclude" case (docs/configuration.md §4.2) — but it also
	// must not block the rest of the managed set from being built.
	spec := config.DisksSpec{Mode: "auto-discovery", Exclude: []string{"bootdisk", "no-such-disk"}}
	managed, degraded, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 2 {
		t.Fatalf("managed = %+v, want 2 disks (only bootdisk actually excluded)", managed)
	}
	if len(degraded) != 1 || degraded[0].DeviceName != "no-such-disk" || degraded[0].Reason != ReasonExcludeTargetNotFound {
		t.Errorf("degraded = %+v, want exactly [{no-such-disk %s}]", degraded, ReasonExcludeTargetNotFound)
	}
}

func TestSelect_selectedReturnsOnlyNamedDisks(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "logdisk"},
	}}
	managed, degraded, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
	}
	if len(managed) != 2 {
		t.Fatalf("managed = %+v, want exactly the 2 selected disks", managed)
	}
}

func TestSelect_selectedOneMissingIsReportedAsDegradedButNotFatal(t *testing.T) {
	// Select itself never decides fatal-vs-degraded (see its doc comment) —
	// that needs discovery history only the caller has. It always reports
	// what it could resolve, plus what it couldn't.
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "no-such-disk"},
	}}
	managed, degraded, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 1 || managed[0].Disk.DeviceName != "datadisk" {
		t.Errorf("managed = %+v, want exactly [datadisk] — the one name that did resolve", managed)
	}
	if len(degraded) != 1 || degraded[0].DeviceName != "no-such-disk" || degraded[0].Reason != ReasonSelectedNotFound {
		t.Errorf("degraded = %+v, want exactly [{no-such-disk %s}]", degraded, ReasonSelectedNotFound)
	}
}

func TestSelect_selectedAllMissingReturnsEmptyManagedAndEveryNameDegraded(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "no-such-disk-1"},
		{DeviceName: "no-such-disk-2"},
	}}
	managed, degraded, err := Select(testVM(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 0 {
		t.Errorf("managed = %+v, want none", managed)
	}
	if len(degraded) != 2 {
		t.Errorf("degraded = %v, want both names", degraded)
	}
}

func TestSelect_selectedRecoversOnceVMInventoryIsConsistent(t *testing.T) {
	// The same selection resolves fully once the missing disk appears in a
	// later snapshot — degraded goes back to empty, not just non-fatal.
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{
		{DeviceName: "datadisk"},
		{DeviceName: "not-yet-attached"},
	}}
	if _, degraded, err := Select(testVM(), spec, testResolved()); err != nil || len(degraded) != 1 {
		t.Fatalf("test setup: first Select: degraded = %v, err = %v, want exactly one degraded name and no error", degraded, err)
	}

	recoveredVM := testVM()
	recoveredVM.Disks = append(recoveredVM.Disks, compute.StorageDisk{Alias: "new", DeviceName: "not-yet-attached"})
	managed, degraded, err := Select(recoveredVM, spec, testResolved())
	if err != nil {
		t.Fatalf("Select after recovery: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
	}
	if len(managed) != 2 {
		t.Errorf("managed = %+v, want both selected disks now that the VM inventory is consistent", managed)
	}
}

func TestSelect_selectedDuplicateLiveDeviceNameNeverSilentlyOverwrites(t *testing.T) {
	// Discover must remove duplicate live device names before Select builds its lookup map.
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

// vmWithDisks builds a VM whose disks carry the exact device_name VM
// status reports, the same value config selectors must match.
func vmWithDisks() *compute.VirtualMachine {
	return &compute.VirtualMachine{
		Name: "vm-1",
		Disks: []compute.StorageDisk{
			{Alias: "boot", DeviceName: "mws-bootdisk", DiskName: "res-boot"},
			{Alias: "data", DeviceName: "mws-datadisk", DiskName: "res-data"},
		},
	}
}

func TestResolveSelectedByDeviceName(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{{DeviceName: "mws-datadisk"}}}
	managed, degraded, err := Select(vmWithDisks(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
	}
	if len(managed) != 1 || managed[0].Disk.DeviceName != "mws-datadisk" {
		t.Fatalf("managed = %+v, want exactly [mws-datadisk]", managed)
	}
}

func TestResolveExcludeByDeviceName(t *testing.T) {
	spec := config.DisksSpec{Mode: "auto-discovery", Exclude: []string{"mws-bootdisk"}}
	managed, degraded, err := Select(vmWithDisks(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
	}
	if len(managed) != 1 || managed[0].Disk.DeviceName != "mws-datadisk" {
		t.Fatalf("managed = %+v, want exactly [mws-datadisk] (mws-bootdisk excluded)", managed)
	}
}

// TestResolveRawDeviceNameWithMWSPrefix covers a disk whose attach-time
// deviceName itself started with "mws-": VM status then doubles the
// prefix, and that doubled value is what config must name.
func TestResolveRawDeviceNameWithMWSPrefix(t *testing.T) {
	vm := &compute.VirtualMachine{
		Name:  "vm-1",
		Disks: []compute.StorageDisk{{Alias: "data", DeviceName: "mws-mws-data", DiskName: "res-data"}},
	}
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{{DeviceName: "mws-mws-data"}}}
	managed, degraded, err := Select(vm, spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(degraded) != 0 {
		t.Errorf("degraded = %v, want none", degraded)
	}
	if len(managed) != 1 || managed[0].Disk.DiskName != "res-data" {
		t.Fatalf("managed = %+v, want the disk whose attach-time deviceName was itself \"mws-data\"", managed)
	}
}

// TestResolveDeviceNameRequiresExactMatch confirms a config value that
// differs from the live device_name in any way — not just a missing
// prefix — is an unresolved name like any other typo, not specially
// handled.
func TestResolveDeviceNameRequiresExactMatch(t *testing.T) {
	spec := config.DisksSpec{Mode: "selected", Selected: []config.SelectedDiskSpec{{DeviceName: "datadisk"}}}
	managed, degraded, err := Select(vmWithDisks(), spec, testResolved())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(managed) != 0 {
		t.Fatalf("managed = %+v, want none — \"datadisk\" is not the exact device_name VM status reports", managed)
	}
	if len(degraded) != 1 || degraded[0].DeviceName != "datadisk" || degraded[0].Reason != ReasonSelectedNotFound {
		t.Fatalf("degraded = %+v, want exactly [{datadisk %s}]", degraded, ReasonSelectedNotFound)
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
