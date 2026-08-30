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

// StorageDisk is one translated VM disk attachment with class-specific fields populated.
type StorageDisk struct {
	Alias      string // spec.storage.disks[].name — alias unique within the VM, not globally
	Boot       bool
	DeviceName string // unprefixed (docs/design.md §4.1); empty if the platform has not assigned one
	Class      DiskClass
	DiskName   string // status.storage.disks[].ref — cloud resource name; GetDisk/UpdateDisk target

	// Dependent-only immutable fields that must be resent unchanged by VM updates.
	DiskType string // e.g. "nbs-pl2"
	SizeRaw  string // bytesize.ByteSize.RawValue(), reparsed unchanged on write

	// Provisioned IOPS from a separate live disk read performed during discovery.
	IOPS                           int64 // spec.iops, current provisioned limit
	BaseIOPS, ExtraIOPS, TotalIOPS int64 // status.iops breakdown

	// IncarnationID is metadata.id, which distinguishes same-name replacement resources.
	// It may be empty only when the platform response omits identity.
	IncarnationID string
}

// GuestPath is the local-mode device path.
func (d StorageDisk) GuestPath() string {
	return "/dev/disk/by-id/mws-" + d.DeviceName
}

// MetricLabel is the exported device_name label.
func (d StorageDisk) MetricLabel() string {
	return "mws-" + d.DeviceName
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
	VMType string // spec.vmType — feeds GetVmType for the VM's IOPS pool (design.md §4.1)
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
	Etag     string // metadata.etag — optimistic-concurrency precondition for UpdateDisk (issue #16)

	// Live status IOPS breakdown, currently used only for observability.
	BaseIOPS, ExtraIOPS, TotalIOPS int64

	// IncarnationID is the disk resource metadata.id returned by GetDisk.
	IncarnationID string
}
