package compute

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUpdateIndependentDiskIOPS_success(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000}

	got, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 3000, false, "idem-1")
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

func TestUpdateIndependentDiskIOPS_dryRunDoesNotVerifyOrMutate(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000}

	got, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 3000, true, "")
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

func TestUpdateIndependentDiskIOPS_appliedMismatchIsFailure(t *testing.T) {
	c := newFakeClient()
	c.disks["datadisk"] = &Disk{Name: "datadisk", SpecIOPS: 1000}
	wrong := int64(2000)
	c.independentApplied = &wrong

	_, err := UpdateIndependentDiskIOPS(context.Background(), c, "datadisk", 3000, false, "")
	if err == nil {
		t.Fatal("UpdateIndependentDiskIOPS succeeded despite the platform applying a different value than requested")
	}
	if !strings.Contains(err.Error(), "wanted 3000") {
		t.Errorf("error = %v, want it to name the mismatch", err)
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

	got, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 3000, "idem-1")
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

func TestUpdateDependentDiskIOPS_deviceNotFound(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	if _, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "no-such-disk", 3000, ""); err == nil {
		t.Fatal("UpdateDependentDiskIOPS succeeded for a device_name not present on the VM")
	}
}

func TestUpdateDependentDiskIOPS_refusesIndependentDisk(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = vmWithDependentDisk()

	_, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "datadisk", 3000, "")
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

	if _, err := UpdateDependentDiskIOPS(context.Background(), c, "iops-test-vm", "bootdisk", 3000, ""); err == nil {
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
	if _, err := UpdateDependentDiskIOPS(context.Background(), mc, "iops-test-vm", "bootdisk", 3000, ""); err == nil {
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
