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
	// UpdateDependentDisk resends the VM's storage attachment collection by
	// alias, in order — every alias present, so none is dropped from the
	// collection (Upsert treats an omitted existing element as a delete) —
	// but carrying no field for any of them except TargetAlias's new IOPS
	// (US-011a). name/boot/deviceName are immutable on update and the API
	// accepts them omitted; sending back a value resent through a lossy
	// response→domain→request translation risks a mismatch the platform
	// would reject or, worse, silently accept as a change. The caller
	// (UpdateDependentDiskIOPS) is responsible for having just re-read the
	// VM fresh to build Aliases/Etag — this method does not fetch or diff
	// on its own, which would risk operating on stale data.
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

// DependentUpdateRequest is a minimal partial VM update: every current
// attachment alias, in order, so the collection stays intact, with only
// TargetAlias carrying a change — spec.iops on TargetAlias, nothing else.
type DependentUpdateRequest struct {
	VMName         string
	Etag           string // mandatory: never write without optimistic concurrency (US-011a)
	Aliases        []string
	TargetAlias    string
	TargetIOPS     int64
	IdempotencyKey string
}
