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
			{Alias: "bootdisk", DeviceName: "bootdisk", Class: ClassDependent, DiskName: "iops-test-vm-boot"},
			{Alias: "datadisk", DeviceName: "datadisk", Class: ClassIndependent, DiskName: "iops-test-vm-data"},
		},
	}
	c.vmTypes["base-2-8"] = &VmType{Name: "base-2-8", VCPUCount: 2}
	c.disks["iops-test-vm-boot"] = &Disk{Name: "iops-test-vm-boot", SpecIOPS: 1000, BaseIOPS: 1000, TotalIOPS: 1000}
	c.disks["iops-test-vm-data"] = &Disk{Name: "iops-test-vm-data", SpecIOPS: 3000, BaseIOPS: 1000, ExtraIOPS: 2000, TotalIOPS: 3000}

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
	// Deliberately no entry in c.disks for "does-not-exist".

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
