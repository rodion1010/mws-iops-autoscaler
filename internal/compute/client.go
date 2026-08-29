package compute

import "context"

// Client is the narrow surface this package needs from the Compute API. The
// production implementation (sdkClient, in sdk_client.go) wraps
// go.mws.cloud/go-sdk; tests use a hand-written fake (fake_test.go) — no
// mocking framework, matching internal/config's testing style.
type Client interface {
	// GetVirtualMachine reads a VM by name, classifying each disk (ADR-003)
	// and resolving its device name (ADR-004) along the way.
	GetVirtualMachine(ctx context.Context, name string) (*VirtualMachine, error)
	// GetVmType reads a VM type — currently only for its vCPU count.
	GetVmType(ctx context.Context, name string) (*VmType, error)
	// GetDisk reads a standalone disk resource by its cloud name.
	GetDisk(ctx context.Context, name string) (*Disk, error)

	// UpdateIndependentDisk writes spec.iops on a standalone disk via
	// UpdateDisk (?updateOnly=true) — nothing else in the spec is touched.
	UpdateIndependentDisk(ctx context.Context, req IndependentUpdateRequest) (*Disk, error)
	// UpdateDependentDisk resends a VM's full storage.disks[] array with
	// exactly one entry's iops changed (US-011a) — the caller
	// (UpdateDependentDiskIOPS) is responsible for having just re-read the
	// VM fresh and mutated exactly one entry, so what reaches here is
	// already "everything byte-identical except one iops" (design.md §4.7).
	// This method does not fetch or diff on its own — that would risk
	// operating on stale data.
	UpdateDependentDisk(ctx context.Context, req DependentUpdateRequest) (*VirtualMachine, error)
}

// IndependentUpdateRequest is a partial update: only Iops is sent
// (docs/configuration.md; CLAUDE.md "DiskSpecRequest mutability" — iops has
// no immutability note, everything else does).
type IndependentUpdateRequest struct {
	DiskName       string
	IOPS           int64
	ValidateOnly   bool // US-013 dry-run — independent disks only; the VM update endpoint has no equivalent (see write.go)
	IdempotencyKey string
}

// DependentUpdateRequest carries the full storage.disks[] array to resend.
type DependentUpdateRequest struct {
	VMName         string
	Etag           string // optimistic concurrency; a conflict aborts, never forces (US-011a)
	Disks          []StorageDisk
	IdempotencyKey string
}
