// Package compute isolates the MWS Compute SDK behind domain types and a narrow client interface.
package compute

// DiskClass selects the platform write path for an attachment.
type DiskClass string

const (
	// ClassIndependent is a standalone disk attached by reference and updated through the disk API.
	ClassIndependent DiskClass = "independent"
	// ClassDependent is owned inline by a VM and updated through the VM attachment collection.
	ClassDependent DiskClass = "dependent"
)

// StorageDisk is a VM attachment returned raw by GetVirtualMachine or enriched by Discover.
type StorageDisk struct {
	Alias string // spec.storage.disks[].name — alias unique within the VM, not globally
	Boot  bool
	// DeviceName is the exact value VM status reports —
	// status.storage.disks[].deviceName — the same identifier used to
	// address the device inside Linux; empty if the
	// platform has not assigned one. Config selectors must match this
	// value exactly.
	DeviceName string
	Class      DiskClass
	DiskName   string // status.storage.disks[].ref — cloud resource name; GetDisk/UpdateDisk target

	// Dependent-only immutable fields that must be resent unchanged by VM updates.
	DiskType string // e.g. "nbs-pl2"
	SizeRaw  string // bytesize.ByteSize.RawValue(), reparsed unchanged on write

	// IOPS comes from the inline VM spec on raw dependent attachments.
	// Discover overwrites it from GetDisk for every managed disk.
	IOPS                           int64 // spec.iops, current provisioned limit
	BaseIOPS, ExtraIOPS, TotalIOPS int64 // status.iops breakdown

	// IncarnationID is metadata.id and distinguishes same-name replacement resources.
	// GetVirtualMachine never populates it; Discover obtains it from GetDisk.
	IncarnationID string
}

// GuestPath is the local-mode device path.
func (d StorageDisk) GuestPath() string {
	return "/dev/disk/by-id/" + d.DeviceName
}

// MetricLabel is the exported device_name label.
func (d StorageDisk) MetricLabel() string {
	return d.DeviceName
}

// IdentityKey is the incarnation-aware key for samples, cooldowns, and change budget.
// It retains the disk name for diagnostics and degrades to name-only when identity is unavailable.
func (d StorageDisk) IdentityKey() string {
	return d.DiskName + "\x00" + d.IncarnationID
}

// VirtualMachine is a VM translated from the Compute API.
type VirtualMachine struct {
	Name   string
	Etag   string // metadata.etag — required for the dependent write path
	VMType string // spec.vmType — feeds GetVmType for the VM's IOPS pool
	Disks  []StorageDisk

	// VCPUCount comes from a separate VM-type read and determines aggregate IOPS capacity.
	VCPUCount int32
}

// VmType is the VM-type capacity data needed by discovery.
type VmType struct {
	Name      string
	VCPUCount int32
}

// Disk is a standalone disk translated from the Compute API.
type Disk struct {
	Name     string
	SpecIOPS int64  // spec.iops — current provisioned limit
	Etag     string // metadata.etag — optimistic-concurrency precondition for UpdateDisk

	// Live status IOPS breakdown, currently used only for observability.
	BaseIOPS, ExtraIOPS, TotalIOPS int64

	// IncarnationID is the disk resource metadata.id returned by GetDisk.
	IncarnationID string
}
