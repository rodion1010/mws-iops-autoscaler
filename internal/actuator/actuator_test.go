package actuator

import (
	"context"
	"errors"
	"testing"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
)

type fakeClient struct {
	vms   map[string]*compute.VirtualMachine
	disks map[string]*compute.Disk

	independentApplied *int64 // overrides what UpdateIndependentDisk reports, to simulate a mismatch
	dependentErr       error
}

func (f *fakeClient) GetVirtualMachine(_ context.Context, name string) (*compute.VirtualMachine, error) {
	vm, ok := f.vms[name]
	if !ok {
		return nil, errors.New("fake: vm not found")
	}
	cp := *vm
	cp.Disks = append([]compute.StorageDisk(nil), vm.Disks...)
	return &cp, nil
}

func (f *fakeClient) GetVmType(context.Context, string) (*compute.VmType, error) {
	return nil, errors.New("unused")
}

func (f *fakeClient) GetDisk(_ context.Context, name string) (*compute.Disk, error) {
	d, ok := f.disks[name]
	if !ok {
		return nil, errors.New("fake: disk not found")
	}
	return d, nil
}

func (f *fakeClient) UpdateIndependentDisk(_ context.Context, req compute.IndependentUpdateRequest) (*compute.Disk, error) {
	d, ok := f.disks[req.DiskName]
	if !ok {
		return nil, errors.New("fake: disk not found")
	}
	if req.Etag == "" {
		return nil, errors.New("fake: empty etag")
	}
	if req.ValidateOnly {
		return d, nil
	}
	applied := req.IOPS
	if f.independentApplied != nil {
		applied = *f.independentApplied
	}
	d.SpecIOPS = applied
	return d, nil
}

func (f *fakeClient) UpdateDependentDisk(_ context.Context, req compute.DependentUpdateRequest) (*compute.VirtualMachine, error) {
	if f.dependentErr != nil {
		return nil, f.dependentErr
	}
	vm, ok := f.vms[req.VMName]
	if !ok {
		return nil, errors.New("fake: vm not found")
	}
	found := false
	for i, d := range vm.Disks {
		if d.Alias == req.TargetAlias {
			vm.Disks[i].IOPS = req.TargetIOPS
			found = true
		}
	}
	if !found {
		return nil, errors.New("fake: target alias not found")
	}
	cp := *vm
	cp.Disks = append([]compute.StorageDisk(nil), vm.Disks...)
	return &cp, nil
}

func TestApply_independentDiskWritesAndVerifies(t *testing.T) {
	c := &fakeClient{disks: map[string]*compute.Disk{
		"datadisk-res": {Name: "datadisk-res", SpecIOPS: 1000, Etag: "etag-1"},
	}}
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "datadisk", Action: decider.ActionScaleUp, Current: 1000, Target: 3000}
	target := Target{DiskName: "datadisk-res", Class: compute.ClassIndependent}

	got, err := a.Apply(context.Background(), d, target, false, "idem-1")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.IOPS != 3000 {
		t.Errorf("Applied.IOPS = %d, want 3000", got.IOPS)
	}
	if c.disks["datadisk-res"].SpecIOPS != 3000 {
		t.Errorf("fake disk SpecIOPS = %d, want 3000", c.disks["datadisk-res"].SpecIOPS)
	}
}

func TestApply_independentDiskScaleDownWritesAndVerifies(t *testing.T) {
	// Every other independent-disk test here exercises ActionScaleUp only.
	c := &fakeClient{disks: map[string]*compute.Disk{
		"datadisk-res": {Name: "datadisk-res", SpecIOPS: 3000, Etag: "etag-1"},
	}}
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "datadisk", Action: decider.ActionScaleDown, Current: 3000, Target: 1000}
	target := Target{DiskName: "datadisk-res", Class: compute.ClassIndependent}

	got, err := a.Apply(context.Background(), d, target, false, "idem-1")
	if err != nil {
		t.Fatalf("Apply (scale down): %v", err)
	}
	if got.IOPS != 1000 {
		t.Errorf("Applied.IOPS = %d, want 1000", got.IOPS)
	}
	if c.disks["datadisk-res"].SpecIOPS != 1000 {
		t.Errorf("fake disk SpecIOPS = %d, want 1000", c.disks["datadisk-res"].SpecIOPS)
	}
}

func TestApply_independentDryRunDoesNotMutate(t *testing.T) {
	c := &fakeClient{disks: map[string]*compute.Disk{
		"datadisk-res": {Name: "datadisk-res", SpecIOPS: 1000, Etag: "etag-1"},
	}}
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "datadisk", Action: decider.ActionScaleUp, Current: 1000, Target: 3000}
	target := Target{DiskName: "datadisk-res", Class: compute.ClassIndependent}

	got, err := a.Apply(context.Background(), d, target, true, "")
	if err != nil {
		t.Fatalf("Apply (dry-run): %v", err)
	}
	if got.IOPS != 3000 {
		t.Errorf("dry-run reported IOPS = %d, want 3000 (the requested value)", got.IOPS)
	}
	if c.disks["datadisk-res"].SpecIOPS != 1000 {
		t.Errorf("dry-run mutated the disk: SpecIOPS = %d, want unchanged 1000", c.disks["datadisk-res"].SpecIOPS)
	}
}

func TestApply_dependentDiskWrites(t *testing.T) {
	c := &fakeClient{vms: map[string]*compute.VirtualMachine{
		"vm-1": {
			Name: "vm-1", Etag: "etag-1",
			Disks: []compute.StorageDisk{
				{Alias: "bootdisk", DeviceName: "bootdisk", Class: compute.ClassDependent, IOPS: 1000},
			},
		},
	}}
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "bootdisk", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	target := Target{VMName: "vm-1", DeviceName: "bootdisk", Class: compute.ClassDependent}

	got, err := a.Apply(context.Background(), d, target, false, "idem-1")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.IOPS != 2000 {
		t.Errorf("Applied.IOPS = %d, want 2000", got.IOPS)
	}
}

func TestApply_dependentDiskScaleDownWrites(t *testing.T) {
	c := &fakeClient{vms: map[string]*compute.VirtualMachine{
		"vm-1": {
			Name: "vm-1", Etag: "etag-1",
			Disks: []compute.StorageDisk{
				{Alias: "bootdisk", DeviceName: "bootdisk", Class: compute.ClassDependent, IOPS: 3000},
			},
		},
	}}
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "bootdisk", Action: decider.ActionScaleDown, Current: 3000, Target: 1000}
	target := Target{VMName: "vm-1", DeviceName: "bootdisk", Class: compute.ClassDependent}

	got, err := a.Apply(context.Background(), d, target, false, "idem-1")
	if err != nil {
		t.Fatalf("Apply (scale down): %v", err)
	}
	if got.IOPS != 1000 {
		t.Errorf("Applied.IOPS = %d, want 1000", got.IOPS)
	}
}

func TestApply_dependentDryRunNeverCallsWriteEndpoint(t *testing.T) {
	// Dependent dry-run performs local checks and must never call the VM write endpoint.
	c := &fakeClient{
		vms: map[string]*compute.VirtualMachine{"vm-1": {
			Name: "vm-1", Etag: "etag-1",
			Disks: []compute.StorageDisk{
				{Alias: "bootdisk", DeviceName: "bootdisk", Class: compute.ClassDependent, IOPS: 1000},
			},
		}},
		dependentErr: errors.New("fake: the write endpoint must not be called during a dry run"),
	}
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "bootdisk", Action: decider.ActionScaleUp, Current: 1000, Target: 2000}
	target := Target{VMName: "vm-1", DeviceName: "bootdisk", Class: compute.ClassDependent}

	got, err := a.Apply(context.Background(), d, target, true, "")
	if err != nil {
		t.Fatalf("Apply (dry-run): %v", err)
	}
	if got.IOPS != 2000 {
		t.Errorf("dry-run reported IOPS = %d, want 2000 (intent only)", got.IOPS)
	}
}

func TestApply_refusesNonScaleActions(t *testing.T) {
	c := &fakeClient{}
	a := NewComputeActuator(c)

	for _, action := range []decider.Action{decider.ActionNoAction, decider.ActionVMCapReached} {
		t.Run(string(action), func(t *testing.T) {
			d := decider.Decision{DiskID: "d1", Action: action}
			if _, err := a.Apply(context.Background(), d, Target{Class: compute.ClassIndependent}, false, ""); err == nil {
				t.Errorf("Apply succeeded for action %q — only scale_up/scale_down should ever write", action)
			}
		})
	}
}

func TestApply_appliedMismatchIsFailure(t *testing.T) {
	c := &fakeClient{disks: map[string]*compute.Disk{"d1-res": {Name: "d1-res", SpecIOPS: 1000, Etag: "etag-1"}}}
	wrong := int64(2500)
	c.independentApplied = &wrong
	a := NewComputeActuator(c)

	d := decider.Decision{DiskID: "d1", Action: decider.ActionScaleUp, Current: 1000, Target: 3000}
	target := Target{DiskName: "d1-res", Class: compute.ClassIndependent}

	if _, err := a.Apply(context.Background(), d, target, false, ""); err == nil {
		t.Error("Apply succeeded despite the platform applying a different value than requested")
	}
}
