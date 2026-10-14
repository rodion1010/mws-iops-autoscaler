package compute

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUpdateIndependentDiskIOPS_success(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000, Etag: "etag-1"}

	got, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 1000, 3000, "", false, "idem-1")
	if err != nil {
		t.Fatalf("UpdateIndependentDiskIOPS: %v", err)
	}
	if got != 3000 {
		t.Errorf("applied = %d, want 3000", got)
	}
	if c.disks["datadisk"].SpecIOPS != 3000 {
		t.Errorf("fake disk SpecIOPS = %d, want 3000", c.disks["datadisk"].SpecIOPS)
	}
}

func TestUpdateIndependentDiskIOPS_decreaseSucceeds(t *testing.T) {
	// Every other test in this file exercises an increase end-to-end; a decrease must
	// clear the same re-read/etag/write/verify flow (design.md §4.7, issue #16's
	// direction-agnostic guard is unit-tested in isolation by TestDirectionPreserved,
	// but never previously exercised through the full path here).
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 3000, Etag: "etag-1"}

	got, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 3000, 1000, "", false, "idem-1")
	if err != nil {
		t.Fatalf("UpdateIndependentDiskIOPS (decrease): %v", err)
	}
	if got != 1000 {
		t.Errorf("applied = %d, want 1000", got)
	}
	if c.disks["datadisk"].SpecIOPS != 1000 {
		t.Errorf("fake disk SpecIOPS = %d, want 1000", c.disks["datadisk"].SpecIOPS)
	}
}

func TestUpdateIndependentDiskIOPS_dryRunVerifiesButDoesNotMutate(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000, Etag: "etag-1"}

	got, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 1000, 3000, "", true, "")
	if err != nil {
		t.Fatalf("UpdateIndependentDiskIOPS (dry-run): %v", err)
	}
	if got != 3000 {
		t.Errorf("dry-run reported = %d, want the requested 3000", got)
	}
	if c.disks["datadisk"].SpecIOPS != 1000 {
		t.Errorf("dry-run mutated the disk: SpecIOPS = %d, want unchanged 1000", c.disks["datadisk"].SpecIOPS)
	}
}

func TestUpdateIndependentDiskIOPS_dryRunCatchesConflict(t *testing.T) {
	// Dry-run enforces the same preconditions as a real write.
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 5000, Etag: "etag-1"} // live moved since discovery

	_, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 1000, 3000, "", true, "")
	if err == nil {
		t.Fatal("UpdateIndependentDiskIOPS (dry-run) succeeded despite live current differing from expected")
	}
	if !errors.Is(err, ErrConcurrentModification) {
		t.Errorf("error = %v, want it to wrap ErrConcurrentModification", err)
	}
	if c.disks["datadisk"].SpecIOPS != 5000 {
		t.Errorf("dry-run mutated the disk: SpecIOPS = %d, want unchanged 5000", c.disks["datadisk"].SpecIOPS)
	}
}

func TestUpdateIndependentDiskIOPS_appliedMismatchIsFailure(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000, Etag: "etag-1"}
	wrong := int64(2000)
	c.independentApplied = &wrong

	_, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 1000, 3000, "", false, "")
	if err == nil {
		t.Fatal("UpdateIndependentDiskIOPS succeeded despite the platform applying a different value than requested")
	}
	if !strings.Contains(err.Error(), "wanted 3000") {
		t.Errorf("error = %v, want it to name the mismatch", err)
	}
}

func TestUpdateIndependentDiskIOPS_emptyEtagOnReReadFailsLocally(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000} // no Etag

	_, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 1000, 3000, "", false, "")
	if err == nil {
		t.Fatal("UpdateIndependentDiskIOPS succeeded despite an empty etag on the pre-write re-read")
	}
	if !strings.Contains(err.Error(), "no etag on re-read") {
		t.Errorf("error = %v, want it to name the missing etag", err)
	}
	if c.disks["datadisk"].SpecIOPS != 1000 {
		t.Error("disk was mutated despite the empty-etag guard")
	}
}

func TestUpdateIndependentDiskIOPS_conflictWhenLiveDiffersFromExpected(t *testing.T) {
	c := newFakeClient()
	// An external actor raised the disk after discovery.
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 5000, Etag: "etag-1"}

	before := *c.disks["datadisk"]
	_, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 1000, 2000, "", false, "")
	if err == nil {
		t.Fatal("UpdateIndependentDiskIOPS succeeded despite live current (5000) differing from expected (1000)")
	}
	if !errors.Is(err, ErrConcurrentModification) {
		t.Errorf("error = %v, want it to wrap ErrConcurrentModification", err)
	}
	if *c.disks["datadisk"] != before {
		t.Errorf("disk mutated after a detected conflict: before=%+v after=%+v — a conflict must never force", before, *c.disks["datadisk"])
	}
}

func vmWithDependentDisk() *VirtualMachine {
	return &VirtualMachine{
		Name: "iops-test-vm",
		Etag: "etag-1",
		Disks: []StorageDisk{
			{Alias: "bootdisk", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 1000},
			{Alias: "datadisk", DeviceName: "datadisk", Class: ClassIndependent, DiskName: "iops-test-vm-data"},
		},
	}
}

func TestUpdateDependentDiskIOPS_success(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, "idem-1")
	if err != nil {
		t.Fatalf("UpdateDependentDiskIOPS: %v", err)
	}
	if got != 3000 {
		t.Errorf("applied = %d, want 3000", got)
	}

	// Every non-target attachment must reach the client unchanged.
	updated := c.vms["iops-test-vm"].Disks
	for _, d := range updated {
		if d.DeviceName == "datadisk" && d.Class != ClassIndependent {
			t.Errorf("independent disk entry was mutated: %+v", d)
		}
	}
}

func TestUpdateDependentDiskIOPS_decreaseSucceeds(t *testing.T) {
	c := newFakeClient()
	vm := vmWithDependentDisk()
	vm.Disks[0].IOPS = 3000 // bootdisk starts above the target this time
	c.vms["iops-test-vm"] = vm

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 3000, 1000, "", false, "idem-1")
	if err != nil {
		t.Fatalf("UpdateDependentDiskIOPS (decrease): %v", err)
	}
	if got != 1000 {
		t.Errorf("applied = %d, want 1000", got)
	}

	// Every non-target attachment must still reach the client unchanged.
	updated := c.vms["iops-test-vm"].Disks
	for _, d := range updated {
		if d.DeviceName == "datadisk" && d.Class != ClassIndependent {
			t.Errorf("independent disk entry was mutated: %+v", d)
		}
	}
}

func TestUpdateDependentDiskIOPS_dryRunVerifiesButDoesNotMutate(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", true, "")
	if err != nil {
		t.Fatalf("UpdateDependentDiskIOPS (dry-run): %v", err)
	}
	if got != 3000 {
		t.Errorf("dry-run reported = %d, want the requested 3000", got)
	}
	if c.vms["iops-test-vm"].Disks[0].IOPS != 1000 {
		t.Errorf("dry-run mutated the VM: bootdisk IOPS = %d, want unchanged 1000", c.vms["iops-test-vm"].Disks[0].IOPS)
	}
}

func TestUpdateDependentDiskIOPS_dryRunCatchesConflict(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk() // bootdisk live IOPS is 1000

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 5000, 6000, "", true, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS (dry-run) succeeded despite live current differing from expected")
	}
	if !errors.Is(err, ErrConcurrentModification) {
		t.Errorf("error = %v, want it to wrap ErrConcurrentModification", err)
	}
}

func TestUpdateDependentDiskIOPS_conflictWhenLiveDiffersFromExpected(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk() // bootdisk live IOPS is 1000

	before := append([]StorageDisk(nil), c.vms["iops-test-vm"].Disks...)
	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 5000, 6000, "", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite live current (1000) differing from expected (5000)")
	}
	if !errors.Is(err, ErrConcurrentModification) {
		t.Errorf("error = %v, want it to wrap ErrConcurrentModification", err)
	}
	after := c.vms["iops-test-vm"].Disks
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("disk[%d] changed after a detected conflict: before=%+v after=%+v — a conflict must never force", i, before[i], after[i])
		}
	}
}

func TestUpdateDependentDiskIOPS_deviceNotFound(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	if _, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "no-such-disk", 1000, 3000, "", false, ""); err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded for a device_name not present on the VM")
	}
}

func TestUpdateDependentDiskIOPS_duplicateAliasRefusedWithoutWriting(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = &VirtualMachine{
		Name: "iops-test-vm",
		Etag: "etag-1",
		Disks: []StorageDisk{
			{Alias: "disk", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 1000},
			{Alias: "disk", DeviceName: "datadisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "10GB", IOPS: 1000},
		},
	}
	c.dependentWriteErr = errors.New("must never be reached")

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite two attachments sharing one alias")
	}
	if !strings.Contains(err.Error(), "sharing alias") {
		t.Errorf("error = %v, want it to name the duplicate alias", err)
	}
	if errors.Is(err, c.dependentWriteErr) {
		t.Error("UpdateDependentDisk was called — validation must refuse before ever reaching the write")
	}
}

func TestUpdateDependentDiskIOPS_duplicateTargetDeviceNameRefusedWithoutWriting(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = &VirtualMachine{
		Name: "iops-test-vm",
		Etag: "etag-1",
		Disks: []StorageDisk{
			{Alias: "boot1", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 1000},
			{Alias: "boot2", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 2000},
		},
	}
	c.dependentWriteErr = errors.New("must never be reached")

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite two attachments sharing the target device_name")
	}
	if !strings.Contains(err.Error(), "attached 2 times") {
		t.Errorf("error = %v, want it to name the ambiguous device_name count", err)
	}
	if errors.Is(err, c.dependentWriteErr) {
		t.Error("UpdateDependentDisk was called — validation must refuse before ever reaching the write")
	}
}

func TestUpdateDependentDiskIOPS_emptyTargetAliasRefusedWithoutWriting(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = &VirtualMachine{
		Name: "iops-test-vm",
		Etag: "etag-1",
		Disks: []StorageDisk{
			{Alias: "", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 1000},
		},
	}
	c.dependentWriteErr = errors.New("must never be reached")

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite the target attachment having an empty alias")
	}
	if !strings.Contains(err.Error(), "empty alias") {
		t.Errorf("error = %v, want it to name the empty alias", err)
	}
	if errors.Is(err, c.dependentWriteErr) {
		t.Error("UpdateDependentDisk was called — validation must refuse before ever reaching the write")
	}
}

func TestUpdateDependentDiskIOPS_validMultiDiskVMSucceeds(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = &VirtualMachine{
		Name: "iops-test-vm",
		Etag: "etag-1",
		Disks: []StorageDisk{
			{Alias: "boot", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 1000},
			{Alias: "data1", DeviceName: "datadisk1", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "10GB", IOPS: 2000},
			{Alias: "data2", DeviceName: "datadisk2", Class: ClassIndependent, DiskName: "iops-test-vm-data2"},
		},
	}

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, "idem-1")
	if err != nil {
		t.Fatalf("UpdateDependentDiskIOPS on a valid multi-disk VM: %v", err)
	}
	if got != 3000 {
		t.Errorf("applied = %d, want 3000", got)
	}
	updated := c.vms["iops-test-vm"].Disks
	if len(updated) != 3 {
		t.Fatalf("disk count after write = %d, want 3 — every attachment must survive the round trip", len(updated))
	}
}

func TestUpdateDependentDiskIOPS_refusesIndependentDisk(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "datadisk", 1000, 3000, "", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded on an independent disk — it must be routed through UpdateIndependentDiskIOPS instead (docs/design.md §4.7)")
	}
	if !strings.Contains(err.Error(), "independent, not dependent") {
		t.Errorf("error = %v, want it to name the class mismatch", err)
	}
}

func TestUpdateDependentDiskIOPS_etagConflictAbortsNeverForces(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()
	c.dependentWriteErr = errors.New("fake: etag conflict")

	before := append([]StorageDisk(nil), c.vms["iops-test-vm"].Disks...)

	if _, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, ""); err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite a simulated etag conflict")
	}

	after := c.vms["iops-test-vm"].Disks
	if len(before) != len(after) {
		t.Fatalf("disk count changed after an aborted write: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("disk[%d] changed after an aborted write: before=%+v after=%+v — a conflict must never force", i, before[i], after[i])
		}
	}
}

func TestUpdateDependentDiskIOPS_appliedMismatchIsFailure(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	// The wrapper simulates a platform response with a different applied value.
	mc := &mismatchDependentClient{fakeClient: c, wrongIOPS: 4000}
	if _, err := UpdateDependentDiskIOPS(context.Background(), mc, "iops-test-vm", "bootdisk", 1000, 3000, "", false, ""); err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite the platform applying a different value than requested")
	}
}

type mismatchDependentClient struct {
	*fakeClient
	wrongIOPS int64
}

func (m *mismatchDependentClient) UpdateDependentDisk(ctx context.Context, req DependentUpdateRequest) (*VirtualMachine, error) {
	vm, err := m.fakeClient.UpdateDependentDisk(ctx, req)
	if err != nil {
		return nil, err
	}
	for i := range vm.Disks {
		if vm.Disks[i].Class == ClassDependent {
			vm.Disks[i].IOPS = m.wrongIOPS
		}
	}
	return vm, nil
}

// vmWithDependentDiskNamed matches a real VM read: DiskName set, IncarnationID empty.
func vmWithDependentDiskNamed(diskName string) *VirtualMachine {
	return &VirtualMachine{
		Name: "iops-test-vm",
		Etag: "etag-1",
		Disks: []StorageDisk{
			{Alias: "bootdisk", DeviceName: "bootdisk", Class: ClassDependent, DiskType: "nbs-pl2", SizeRaw: "20GB", IOPS: 1000, DiskName: diskName},
		},
	}
}

func TestUpdateDependentDiskIOPS_incarnationMismatchRefusedViaFreshDiskRead(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDiskNamed("iops-test-vm-boot")
	// A replacement incarnation now exists under the same disk name.
	c.disks["iops-test-vm-boot"] = &Disk{Name: "iops-test-vm-boot", IncarnationID: "incarnation-b"}

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "incarnation-a", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite a fresh GetDisk read reporting a different incarnation than expected")
	}
	if !errors.Is(err, ErrIncarnationMismatch) {
		t.Errorf("error = %v, want it to wrap ErrIncarnationMismatch", err)
	}
	if c.vms["iops-test-vm"].Disks[0].IOPS != 1000 {
		t.Errorf("bootdisk IOPS = %d, want unchanged 1000 — a mismatch must refuse before writing", c.vms["iops-test-vm"].Disks[0].IOPS)
	}
}

func TestUpdateDependentDiskIOPS_incarnationMatchViaFreshDiskReadSucceeds(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDiskNamed("iops-test-vm-boot")
	c.disks["iops-test-vm-boot"] = &Disk{Name: "iops-test-vm-boot", IncarnationID: "incarnation-a"}

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "incarnation-a", false, "")
	if err != nil {
		t.Fatalf("UpdateDependentDiskIOPS: %v", err)
	}
	if got != 3000 {
		t.Errorf("applied = %d, want 3000", got)
	}
}

func TestUpdateDependentDiskIOPS_emptyLiveIncarnationFailsClosedWhenExpectedIsKnown(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDiskNamed("iops-test-vm-boot")
	// Expected identity exists, but the fresh disk read cannot provide a live ID.
	c.disks["iops-test-vm-boot"] = &Disk{Name: "iops-test-vm-boot"}

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "incarnation-a", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite an empty live incarnation when one was expected")
	}
	if !errors.Is(err, ErrIncarnationMismatch) {
		t.Errorf("error = %v, want it to wrap ErrIncarnationMismatch", err)
	}
}

func TestUpdateDependentDiskIOPS_incarnationCheckSkippedWhenExpectedUnknown(t *testing.T) {
	// Empty expected identity degrades to IOPS-only checks and skips the extra GetDisk.
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDiskNamed("iops-test-vm-boot")

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "", false, "")
	if err != nil {
		t.Fatalf("UpdateDependentDiskIOPS: %v", err)
	}
	if got != 3000 {
		t.Errorf("applied = %d, want 3000", got)
	}
}

func TestUpdateDependentDiskIOPS_incarnationCheckReadFailurePropagates(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDiskNamed("iops-test-vm-boot")
	// The required fresh GetDisk fails and must fail the write closed.

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 1000, 3000, "incarnation-a", false, "")
	if err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite the incarnation-check GetDisk read failing")
	}
	if c.vms["iops-test-vm"].Disks[0].IOPS != 1000 {
		t.Error("bootdisk was mutated despite a failed incarnation-check read")
	}
}

func TestDirectionPreserved(t *testing.T) {
	// Direction safety remains independently enforced even if equality checks change.
	cases := []struct {
		name                          string
		expectedCurrent, target, live int64
		want                          bool
	}{
		{"scale up, live matches expected", 1000, 2000, 1000, true},
		{"scale up, live already at or above target", 1000, 2000, 2000, true},
		{"scale up, target would drop below live", 1000, 2000, 3000, false},
		{"scale down, live matches expected", 3000, 1000, 3000, true},
		{"scale down, live already at or below target", 3000, 1000, 1000, true},
		{"scale down, target would rise above live", 3000, 1000, 500, false},
		{"no-op target equals expected", 1000, 1000, 1000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := directionPreserved(tc.expectedCurrent, tc.target, tc.live); got != tc.want {
				t.Errorf("directionPreserved(%d, %d, %d) = %v, want %v", tc.expectedCurrent, tc.target, tc.live, got, tc.want)
			}
		})
	}
}
