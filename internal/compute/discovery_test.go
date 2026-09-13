package compute

import (
	"context"
	"testing"
)

func TestDiscover_classifiesAndResolvesGuestPath(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = &VirtualMachine{
		Name:   "iops-test-vm",
		Etag:   "etag-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "bootdisk", DeviceName: "mws-bootdisk", Class: ClassDependent, DiskName: "iops-test-vm-boot"},
			{Alias: "datadisk", DeviceName: "mws-datadisk", Class: ClassIndependent, DiskName: "iops-test-vm-data"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["iops-test-vm-boot"] = &Disk{Name: "iops-test-vm-boot", SpecIOPS: 1000, BaseIOPS: 1000, TotalIOPS: 1000, IncarnationID: "boot-incarnation-1"}
	c.disks["iops-test-vm-data"] = &Disk{Name: "iops-test-vm-data", SpecIOPS: 3000, BaseIOPS: 1000, ExtraIOPS: 2000, TotalIOPS: 3000, IncarnationID: "data-incarnation-1"}

	vm, skipped, err := Discover(context.Background(), c, "iops-test-vm")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %+v, want none", skipped)
	}

	if vm.VCPUCount != 2 {
		t.Errorf("VCPUCount = %d, want 2", vm.VCPUCount)
	}
	if got := VMIOPSPool(vm); got != 6000 {
		t.Errorf("VMIOPSPool = %d, want 6000", got)
	}
	if len(vm.Disks) != 2 {
		t.Fatalf("len(Disks) = %d, want 2", len(vm.Disks))
	}

	boot, data := vm.Disks[0], vm.Disks[1]
	if boot.Class != ClassDependent {
		t.Errorf("bootdisk Class = %q, want dependent", boot.Class)
	}
	if data.Class != ClassIndependent {
		t.Errorf("datadisk Class = %q, want independent", data.Class)
	}
	if got, want := data.GuestPath(), "/dev/disk/by-id/mws-datadisk"; got != want {
		t.Errorf("GuestPath = %q, want %q", got, want)
	}
	if got, want := data.MetricLabel(), "mws-datadisk"; got != want {
		t.Errorf("MetricLabel = %q, want %q", got, want)
	}
	if data.IOPS != 3000 {
		t.Errorf("datadisk IOPS = %d, want 3000 (from GetDisk)", data.IOPS)
	}
	if data.TotalIOPS != 3000 || data.ExtraIOPS != 2000 {
		t.Errorf("datadisk status breakdown = %+v, want total=3000 extra=2000", data)
	}
	// Discover obtains both IOPS and incarnation from GetDisk for either attachment class.
	if boot.IncarnationID != "boot-incarnation-1" {
		t.Errorf("bootdisk IncarnationID = %q, want %q", boot.IncarnationID, "boot-incarnation-1")
	}
	if data.IncarnationID != "data-incarnation-1" {
		t.Errorf("datadisk IncarnationID = %q, want %q", data.IncarnationID, "data-incarnation-1")
	}
	if boot.IdentityKey() == data.IdentityKey() {
		t.Errorf("bootdisk and datadisk produced the same IdentityKey() (%q) despite different DiskName and IncarnationID", boot.IdentityKey())
	}
}

// TestDiscoverPreservesPlatformVisibleDeviceName confirms Discover never
// transforms DeviceName: it must stay exactly the value VM status reports.
func TestDiscoverPreservesPlatformVisibleDeviceName(t *testing.T) {
	c := newFakeClient()
	c.vms["iops-test-vm"] = &VirtualMachine{
		Name:   "iops-test-vm",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "data", DeviceName: "mws-data", Class: ClassIndependent, DiskName: "res-data"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["res-data"] = &Disk{Name: "res-data", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "iops-test-vm")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
	if len(vm.Disks) != 1 || vm.Disks[0].DeviceName != "mws-data" {
		t.Fatalf("Disks = %+v, want exactly one disk with DeviceName %q", vm.Disks, "mws-data")
	}
}

func TestDiscover_unresolvableDiskExcludedNotFatal(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "gone", DeviceName: "gone", Class: ClassIndependent, DiskName: "does-not-exist"},
			{Alias: "ok", DeviceName: "ok", Class: ClassIndependent, DiskName: "resolves-fine"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["resolves-fine"] = &Disk{Name: "resolves-fine", SpecIOPS: 2000}
	// No disk resource exists for this attachment.

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v, want no error — an unresolvable disk is skipped, not fatal (US-002)", err)
	}
	if len(vm.Disks) != 1 || vm.Disks[0].Alias != "ok" {
		t.Fatalf("Disks = %+v, want only the resolvable disk — an unresolvable one must be excluded from the managed set, not kept with IOPS==0", vm.Disks)
	}
	if len(skipped) != 1 || skipped[0].Alias != "gone" || skipped[0].DiskName != "does-not-exist" || skipped[0].Reason == nil {
		t.Fatalf("skipped = %+v, want one entry naming the gone disk and a reason", skipped)
	}
	if skipped[0].Category != SkipReasonGetDiskFailed {
		t.Errorf("Category = %q, want %q", skipped[0].Category, SkipReasonGetDiskFailed)
	}
}

func TestDiscover_ambiguousClassSkipped(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			// Neither ref nor inline spec resolves, so Class remains empty.
			{Alias: "weird", DeviceName: "weird", Class: "", DiskName: "vm-1-weird"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["vm-1-weird"] = &Disk{Name: "vm-1-weird", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — an ambiguous class must never enter the managed set", vm.Disks)
	}
	if len(skipped) != 1 || skipped[0].Category != SkipReasonAmbiguousClass {
		t.Fatalf("skipped = %+v, want one entry categorized %q", skipped, SkipReasonAmbiguousClass)
	}
}

func TestDiscover_emptyDiskNameSkipped(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "nameless", DeviceName: "nameless", Class: ClassIndependent, DiskName: ""},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — an empty resource identity must never enter the managed set", vm.Disks)
	}
	if len(skipped) != 1 || skipped[0].Category != SkipReasonEmptyIdentity {
		t.Fatalf("skipped = %+v, want one entry categorized %q", skipped, SkipReasonEmptyIdentity)
	}
}

func TestDiscover_duplicateDiskNameBothSkipped(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "a", DeviceName: "a", Class: ClassIndependent, DiskName: "shared-res"},
			{Alias: "b", DeviceName: "b", Class: ClassIndependent, DiskName: "shared-res"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["shared-res"] = &Disk{Name: "shared-res", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — an ambiguous shared identity must exclude both attachments, not pick one", vm.Disks)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want both entries excluded", skipped)
	}
	for _, s := range skipped {
		if s.Category != SkipReasonDuplicateDiskName {
			t.Errorf("skipped[%s].Category = %q, want %q", s.Alias, s.Category, SkipReasonDuplicateDiskName)
		}
	}
}

func TestDiscover_duplicateDeviceNameBothSkipped(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "a", DeviceName: "shared-dev", Class: ClassIndependent, DiskName: "res-a"},
			{Alias: "b", DeviceName: "shared-dev", Class: ClassIndependent, DiskName: "res-b"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["res-a"] = &Disk{Name: "res-a", SpecIOPS: 1000}
	c.disks["res-b"] = &Disk{Name: "res-b", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — registry.Select joins by device_name (docs/design.md §4.1); a collision must exclude both, not let one silently win", vm.Disks)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want both entries excluded", skipped)
	}
	for _, s := range skipped {
		if s.Category != SkipReasonDuplicateDevice {
			t.Errorf("skipped[%s].Category = %q, want %q", s.Alias, s.Category, SkipReasonDuplicateDevice)
		}
	}
}

func TestDiscover_duplicateAliasBothSkipped(t *testing.T) {
	// Aliases are dependent-write join keys and must be unique before selection.
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "shared-alias", DeviceName: "a", Class: ClassIndependent, DiskName: "res-a"},
			{Alias: "shared-alias", DeviceName: "b", Class: ClassIndependent, DiskName: "res-b"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["res-a"] = &Disk{Name: "res-a", SpecIOPS: 1000}
	c.disks["res-b"] = &Disk{Name: "res-b", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — a colliding alias must exclude both attachments, not pick one", vm.Disks)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want both entries excluded", skipped)
	}
	for _, s := range skipped {
		if s.Category != SkipReasonDuplicateAlias {
			t.Errorf("skipped[%s].Category = %q, want %q", s.DiskName, s.Category, SkipReasonDuplicateAlias)
		}
	}
}

func TestDiscover_emptyAliasSkipped(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "", DeviceName: "a", Class: ClassIndependent, DiskName: "res-a"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["res-a"] = &Disk{Name: "res-a", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — an empty alias must never enter the managed set", vm.Disks)
	}
	if len(skipped) != 1 || skipped[0].Category != SkipReasonEmptyAlias {
		t.Fatalf("skipped = %+v, want one entry categorized %q", skipped, SkipReasonEmptyAlias)
	}
}

func TestDiscover_emptyDeviceNamesAreNotTreatedAsDuplicates(t *testing.T) {
	// Empty device names are expected before platform assignment and do not collide.
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "a", DeviceName: "", Class: ClassIndependent, DiskName: "res-a"},
			{Alias: "b", DeviceName: "", Class: ClassIndependent, DiskName: "res-b"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["res-a"] = &Disk{Name: "res-a", SpecIOPS: 1000}
	c.disks["res-b"] = &Disk{Name: "res-b", SpecIOPS: 1000}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none — an empty device_name is not itself a duplicate-identity signal", skipped)
	}
	if len(vm.Disks) != 2 {
		t.Fatalf("Disks = %+v, want both", vm.Disks)
	}
}

func TestDiscover_invalidIOPSSkipped(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{
		Name:   "vm-1",
		VMType: "base-2-8",
		Disks: []StorageDisk{
			{Alias: "zero", DeviceName: "zero", Class: ClassIndependent, DiskName: "res-zero"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["res-zero"] = &Disk{Name: "res-zero", SpecIOPS: 0}

	vm, skipped, err := Discover(context.Background(), c, "vm-1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(vm.Disks) != 0 {
		t.Fatalf("Disks = %+v, want none — a disk with IOPS<=0 must never enter the managed set with an impossible current limit", vm.Disks)
	}
	if len(skipped) != 1 || skipped[0].Category != SkipReasonInvalidIOPS {
		t.Fatalf("skipped = %+v, want one entry categorized %q", skipped, SkipReasonInvalidIOPS)
	}
}

func TestDiscover_missingVMErrors(t *testing.T) {
	c := newFakeClient()
	if _, _, err := Discover(context.Background(), c, "does-not-exist"); err == nil {
		t.Fatal("Discover succeeded for a VM the client does not know about")
	}
}

func TestDiscover_missingVmTypeErrors(t *testing.T) {
	c := newFakeClient()
	c.vms["vm-1"] = &VirtualMachine{Name: "vm-1", VMType: "unknown-type"}
	if _, _, err := Discover(context.Background(), c, "vm-1"); err == nil {
		t.Fatal("Discover succeeded with an unresolvable VM type")
	}
}
