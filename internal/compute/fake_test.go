package compute

import (
	"context"
	"fmt"
)

// fakeClient is a hand-written Client for unit tests — no mocking
// framework, matching internal/config's testing style. Production wiring
// (sdk_client.go) is exercised only by building against the real SDK; there
// are no credentials in this environment to run it live.
type fakeClient struct {
	vms     map[string]*VirtualMachine
	vmTypes map[string]*VmType
	disks   map[string]*Disk

	// dependentWriteErr, if set, is returned by UpdateDependentDisk instead
	// of performing the write — used to simulate an etag conflict.
	dependentWriteErr error
	// independentApplied overrides what UpdateIndependentDisk reports as
	// applied, to simulate the platform silently applying a different
	// value than requested.
	independentApplied *int64
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		vms:     make(map[string]*VirtualMachine),
		vmTypes: make(map[string]*VmType),
		disks:   make(map[string]*Disk),
	}
}

func (f *fakeClient) GetVirtualMachine(_ context.Context, name string) (*VirtualMachine, error) {
	vm, ok := f.vms[name]
	if !ok {
		return nil, fmt.Errorf("fake: VM %q not found", name)
	}
	cp := *vm
	cp.Disks = append([]StorageDisk(nil), vm.Disks...)
	return &cp, nil
}

func (f *fakeClient) GetVmType(_ context.Context, name string) (*VmType, error) {
	vt, ok := f.vmTypes[name]
	if !ok {
		return nil, fmt.Errorf("fake: VmType %q not found", name)
	}
	return vt, nil
}

func (f *fakeClient) GetDisk(_ context.Context, name string) (*Disk, error) {
	d, ok := f.disks[name]
	if !ok {
		return nil, fmt.Errorf("fake: disk %q not found", name)
	}
	return d, nil
}

func (f *fakeClient) UpdateIndependentDisk(_ context.Context, req IndependentUpdateRequest) (*Disk, error) {
	d, ok := f.disks[req.DiskName]
	if !ok {
		return nil, fmt.Errorf("fake: disk %q not found", req.DiskName)
	}
	if req.ValidateOnly {
		return d, nil // dry run: report current state, do not mutate
	}
	applied := req.IOPS
	if f.independentApplied != nil {
		applied = *f.independentApplied
	}
	d.SpecIOPS = applied
	return d, nil
}

func (f *fakeClient) UpdateDependentDisk(_ context.Context, req DependentUpdateRequest) (*VirtualMachine, error) {
	if f.dependentWriteErr != nil {
		return nil, f.dependentWriteErr
	}
	vm, ok := f.vms[req.VMName]
	if !ok {
		return nil, fmt.Errorf("fake: VM %q not found", req.VMName)
	}
	if req.Etag == "" {
		return nil, fmt.Errorf("fake: empty etag")
	}
	found := false
	for i, d := range vm.Disks {
		if d.Alias == req.TargetAlias {
			vm.Disks[i].IOPS = req.TargetIOPS
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("fake: target alias %q not found on VM %q", req.TargetAlias, req.VMName)
	}
	cp := *vm
	cp.Disks = append([]StorageDisk(nil), vm.Disks...)
	return &cp, nil
}
