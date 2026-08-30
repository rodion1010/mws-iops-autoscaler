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
	// issue #16 acceptance criterion: dry-run exercises the same
	// precondition checks as a real write, not a weaker subset of them.
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
	// An operator raised the disk from 1000 to 5000 after discovery ran —
	// the scenario from issue #16's own example.
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

	// Every other disk's entry must have reached the fake untouched.
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

// --- issue #20: reject ambiguous attachment identities before writing ---

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
		t.Fatal("UpdateDependentDiskIOPS succeeded on an independent disk — it must be routed through UpdateIndependentDiskIOPS instead (ADR-003)")
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

	// The fake echoes back req.Disks as-is (always a match), so the
	// mismatch case needs a wrapper that wants a different value than
	// UpdateDependentDiskIOPS asked for — simulating the platform silently
	// applying something else.
	mc := &mismatchDependentClient{fakeClient: c, wrongIOPS: 4000}
	if _, err := UpdateDependentDiskIOPS(context.Background(), mc, "iops-test-vm", "bootdisk", 1000, 3000, "", false, ""); err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded despite the platform applying a different value than requested")
	}
}

// mismatchDependentClient wraps fakeClient to make UpdateDependentDisk
// report an applied value different from what was requested.
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

func TestDirectionPreserved(t *testing.T) {
	// issue #16's explicit acceptance criterion, tested directly rather
	// than only indirectly through the equality check it's meant to keep
	// holding even if that check is ever loosened: "a scale-up request can
	// never write a value below the live current; future scale-down can
	// never write above it."
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
