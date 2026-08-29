// Package compute wraps the MWS Compute API (go.mws.cloud/go-sdk) behind a
// narrow interface, translating its wire types into plain domain values so
// the rest of the agent never imports the SDK directly. Discovery and
// classification are docs/design.md §4.1; the two write paths are ADR-003.
package compute

// DiskClass is how a disk is attached to its VM (ADR-003) — it determines
// the write path, never whether a disk is in scope.
type DiskClass string

const (
	// ClassIndependent disks are created standalone and attached by
	// reference (spec.storage.disks[].disk.ref). Written via UpdateDisk.
	ClassIndependent DiskClass = "independent"
	// ClassDependent disks are created inline in the VM spec
	// (spec.storage.disks[].disk.spec) and owned by it. Written by
	// resending the VM's full storage.disks[] array (US-011a).
	ClassDependent DiskClass = "dependent"
)

// StorageDisk is one entry in a VM's spec.storage.disks[], translated from
// the SDK's wire-optional types into plain Go values. Once a read has
// succeeded every field here is populated for the disk's class — there is
// nothing left to make optional at this layer.
type StorageDisk struct {
	Alias      string // spec.storage.disks[].name — alias unique within the VM, not globally
	Boot       bool
	DeviceName string // unprefixed (ADR-004); empty if the platform has not assigned one
	Class      DiskClass
	DiskName   string // status.storage.disks[].ref — cloud resource name; GetDisk/UpdateDisk target

	// Populated for ClassDependent only — the fields DiskSpecRequest marks
	// immutable-but-must-be-resent (CLAUDE.md, "DiskSpecRequest mutability"),
	// carried so a write can resend every other disk's entry byte-identical.
	DiskType string // e.g. "nbs-pl2"
	SizeRaw  string // bytesize.ByteSize.RawValue(), reparsed unchanged on write

	// Populated by Discover (discovery.go) from a separate GetDisk call —
	// design.md §4.1 reads every disk's own spec/status this way, uniformly
	// for both classes, rather than trusting the VM spec's inline iops.
	IOPS                           int64 // spec.iops, current provisioned limit
	BaseIOPS, ExtraIOPS, TotalIOPS int64 // status.iops breakdown
}

// GuestPath is where this disk appears on the guest filesystem in `local`
// mode (docs/configuration.md §4.1).
func (d StorageDisk) GuestPath() string {
	return "/dev/disk/by-id/mws-" + d.DeviceName
}

// MetricLabel is the device_name label value on this disk's exported
// metrics (CLAUDE.md, "Prometheus export").
func (d StorageDisk) MetricLabel() string {
	return "mws-" + d.DeviceName
}

// VirtualMachine is a VM as read from the Compute API.
type VirtualMachine struct {
	Name   string
	Etag   string // metadata.etag — required for the dependent write path
	VMType string // spec.vmType — feeds GetVmType for the VM's IOPS pool (design.md §4.1)
	Disks  []StorageDisk

	// VCPUCount is populated by Discover (discovery.go) from a separate
	// GetVmType call — the VM ceiling is 3000 × VCPUCount and is not
	// enforced by the platform (CLAUDE.md "IOPS limits"). Zero until
	// Discover fills it in; GetVirtualMachine alone does not know it.
	VCPUCount int32
}

// VmType is a VM type as read from the Compute API — currently used only
// for its vCPU count (the VM aggregate IOPS pool is 3000 × vCPU, CLAUDE.md
// "IOPS limits"; spec.disks.totalIops exists in the schema but is null on
// every type observed, so it is not read here).
type VmType struct {
	Name      string
	VCPUCount int32
}

// Disk is a standalone disk resource as read from the Compute API.
type Disk struct {
	Name     string
	SpecIOPS int64 // spec.iops — current provisioned limit

	// status.iops breakdown — observability only, not load-bearing yet.
	BaseIOPS, ExtraIOPS, TotalIOPS int64
}
