package compute

import (
	"context"
	"fmt"
	"strings"

	"go.mws.cloud/go-sdk/mws"
	mwserrors "go.mws.cloud/go-sdk/mws/errors"
	"go.mws.cloud/go-sdk/pkg/optional"
	commonmodel "go.mws.cloud/go-sdk/service/common/model"
	sdkclient "go.mws.cloud/go-sdk/service/compute/client"
	"go.mws.cloud/go-sdk/service/compute/model"
	computesdk "go.mws.cloud/go-sdk/service/compute/sdk"
)

// sdkClient implements Client against the real Compute API via
// go.mws.cloud/go-sdk. All SDK <-> domain translation lives here — nothing
// elsewhere in this package imports the SDK.
type sdkClient struct {
	project string
	vm      *computesdk.VirtualMachine
	vmType  *computesdk.VmType
	disk    *computesdk.Disk
}

// NewSDKClient builds a Client from an already-loaded SDK (mws.Load) and a
// project name.
func NewSDKClient(ctx context.Context, sdk *mws.SDK, project string) (Client, error) {
	vmClient, err := computesdk.NewVirtualMachine(ctx, sdk)
	if err != nil {
		return nil, fmt.Errorf("init VirtualMachine client: %w", err)
	}
	vmTypeClient, err := computesdk.NewVmType(ctx, sdk)
	if err != nil {
		return nil, fmt.Errorf("init VmType client: %w", err)
	}
	diskClient, err := computesdk.NewDisk(ctx, sdk)
	if err != nil {
		return nil, fmt.Errorf("init Disk client: %w", err)
	}
	return &sdkClient{project: project, vm: vmClient, vmType: vmTypeClient, disk: diskClient}, nil
}

func (c *sdkClient) GetVirtualMachine(ctx context.Context, name string) (*VirtualMachine, error) {
	resp, err := c.vm.GetVirtualMachine(ctx, sdkclient.GetVirtualMachineRequest{
		Project:        c.project,
		VirtualMachine: name,
	})
	if err != nil {
		return nil, err
	}
	return translateVMResponse(name, resp)
}

// translateVMResponse is shared by GetVirtualMachine and
// UpdateDependentDisk's response handling, so both paths produce the same
// shape for callers to verify an applied value against.
func translateVMResponse(name string, resp *model.VirtualMachineOptionalResponse) (*VirtualMachine, error) {
	out := &VirtualMachine{Name: name}
	vmType := resp.Spec.VmType
	out.VMType = vmType.GetVmType()
	if resp.Metadata.IsSet() {
		if etag := resp.Metadata.Value.GetEtag(); etag != nil {
			out.Etag = *etag
		}
	}

	statusByAlias := make(map[string]model.StorageDiskStatusResponse)
	if resp.Status != nil {
		for _, sd := range resp.Status.Storage.Disks {
			statusByAlias[sd.Name] = sd
		}
	}

	for _, specEntry := range resp.Spec.Storage.Disks {
		disk := StorageDisk{Alias: specEntry.Name}
		if b, ok := specEntry.Boot.Get(); ok {
			disk.Boot = b
		}

		if ref, ok := specEntry.Disk.Ref.Get(); ok {
			disk.Class = ClassIndependent
			disk.DiskName = ref.GetDisk()
		} else if spec, ok := specEntry.Disk.Spec.Get(); ok {
			disk.Class = ClassDependent
			if dt, ok2 := spec.DiskType.Get(); ok2 {
				disk.DiskType = dt.GetDiskType()
			}
			if sz, ok2 := spec.Size.Get(); ok2 {
				if raw := sz.RawValue(); raw != nil {
					disk.SizeRaw = *raw
				}
			}
			if iops, ok2 := spec.Iops.Get(); ok2 {
				disk.IOPS = int64(iops)
			}
		}

		// The status entry is the authoritative source for DeviceName
		// (required there, defaulted server-side if unset at attach time —
		// CLAUDE.md "Guest device mapping") and for the disk's own resource
		// name (Ref), needed to call GetDisk/UpdateDisk on either class.
		if status, ok := statusByAlias[specEntry.Name]; ok {
			disk.DeviceName = strings.TrimPrefix(status.DeviceName, "mws-")
			disk.DiskName = status.Ref.GetDisk()
		}

		out.Disks = append(out.Disks, disk)
	}

	return out, nil
}

func (c *sdkClient) GetVmType(ctx context.Context, name string) (*VmType, error) {
	resp, err := c.vmType.GetVmType(ctx, sdkclient.GetVmTypeRequest{VmType: name})
	if err != nil {
		return nil, err
	}
	out := &VmType{Name: name}
	if cpu, ok := resp.Spec.Cpu.Get(); ok {
		if vcpu, ok2 := cpu.VcpuCount.Get(); ok2 {
			out.VCPUCount = vcpu
		}
	}
	return out, nil
}

func (c *sdkClient) GetDisk(ctx context.Context, name string) (*Disk, error) {
	resp, err := c.disk.GetDisk(ctx, sdkclient.GetDiskRequest{Project: c.project, Disk: name})
	if err != nil {
		return nil, err
	}
	out := &Disk{Name: name}
	if iops, ok := resp.Spec.Iops.Get(); ok {
		out.SpecIOPS = int64(iops)
	}
	if resp.Status != nil && resp.Status.Iops != nil {
		out.BaseIOPS = int64(resp.Status.Iops.Base)
		out.ExtraIOPS = int64(resp.Status.Iops.Extra)
		out.TotalIOPS = int64(resp.Status.Iops.Total)
	}
	return out, nil
}

func (c *sdkClient) UpdateIndependentDisk(ctx context.Context, req IndependentUpdateRequest) (*Disk, error) {
	var validateOnly *bool
	if req.ValidateOnly {
		v := true
		validateOnly = &v
	}
	var idemKey *string
	if req.IdempotencyKey != "" {
		idemKey = &req.IdempotencyKey
	}

	body := model.UpdateDiskRequest{
		Spec: optional.NewOptional(model.UpdateDiskSpecRequest{
			Iops: optional.NewOptional(model.Iops(req.IOPS)),
		}),
	}

	resp, err := c.disk.UpdateDisk(ctx, sdkclient.UpdateDiskRequest{
		Project:        c.project,
		Disk:           req.DiskName,
		ValidateOnly:   validateOnly,
		IdempotencyKey: idemKey,
		Body:           body,
	})
	if err != nil {
		return nil, err
	}

	out := &Disk{Name: req.DiskName}
	if iops, ok := resp.Spec.Iops.Get(); ok {
		out.SpecIOPS = int64(iops)
	}
	return out, nil
}

// UpdateDependentDisk sends the minimal safe partial update: every current
// attachment alias, in the current order (Upsert treats a missing existing
// collection element as a delete — MWS API openapi.gen.yaml's complex-
// collection merge rules), with no field on any of them except
// TargetAlias's spec.iops. name/boot/deviceName are immutable on update and
// the API accepts them omitted entirely; only the collection key (name) is
// sent, never a value round-tripped through a response→domain→request
// translation that could drift from what the platform considers current.
func (c *sdkClient) UpdateDependentDisk(ctx context.Context, req DependentUpdateRequest) (*VirtualMachine, error) {
	if req.Etag == "" {
		return nil, fmt.Errorf("update dependent disk %q: empty etag — refusing to write without optimistic concurrency", req.VMName)
	}

	body, err := buildDependentUpdateBody(req)
	if err != nil {
		return nil, fmt.Errorf("update dependent disk %q: %w", req.VMName, err)
	}

	var idemKey *string
	if req.IdempotencyKey != "" {
		idemKey = &req.IdempotencyKey
	}

	resp, err := c.vm.UpdateVirtualMachine(ctx, sdkclient.UpdateVirtualMachineRequest{
		Project:        c.project,
		VirtualMachine: req.VMName,
		IdempotencyKey: idemKey,
		Body:           body,
	})
	if err != nil {
		if mwserrors.IsAPIErrorInvalidEtagKeyStatus(err) {
			return nil, fmt.Errorf("etag conflict — VM changed since it was read, retry next cycle: %w", err)
		}
		return nil, err
	}

	// Re-translate the response the same way GetVirtualMachine does, so
	// callers verify the applied value against the same shape either path
	// returns.
	return translateVMResponse(req.VMName, resp)
}

// buildDependentUpdateBody is the pure part of UpdateDependentDisk — no
// network, no *sdkClient — so the wire shape (issue #3's actual safety
// claim) can be asserted directly against a marshaled body in tests instead
// of only by code review.
func buildDependentUpdateBody(req DependentUpdateRequest) (model.UpdateVirtualMachineRequest, error) {
	disks := make([]model.UpdateStorageDiskSpecOrRefWithAttachmentsRequest, len(req.Aliases))
	found := false
	for i, alias := range req.Aliases {
		entry := model.UpdateStorageDiskSpecOrRefWithAttachmentsRequest{
			Name: optional.NewOptional(alias),
		}
		if alias == req.TargetAlias {
			found = true
			entry.Disk = optional.NewOptional(model.UpdateStorageDiskSpecOrRefRequest{
				Spec: optional.NewOptionalNil(model.UpdateStorageDiskSpecRequest{
					Iops: optional.NewOptional(model.Iops(req.TargetIOPS)),
				}),
			})
		}
		disks[i] = entry
	}
	if !found {
		return model.UpdateVirtualMachineRequest{}, fmt.Errorf("target alias %q not present in Aliases", req.TargetAlias)
	}

	return model.UpdateVirtualMachineRequest{
		Metadata: optional.NewOptionalNil(model.UpdateVirtualMachineMetadataRequest{
			UpdateTypedResourceMetadataRequest: commonmodel.UpdateTypedResourceMetadataRequest{
				Etag: optional.NewOptional(req.Etag),
			},
		}),
		Spec: optional.NewOptional(model.UpdateVirtualMachineSpecRequest{
			Storage: optional.NewOptional(model.UpdateStorageSpecRequest{
				Disks: optional.NewOptional(disks),
			}),
		}),
	}, nil
}
