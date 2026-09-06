package platformfake_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mws.cloud/go-sdk/mws"
	mwserrors "go.mws.cloud/go-sdk/mws/errors"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/platformfake"
)

// newClient builds the production Compute client against the fake, proving the
// fake speaks the wire protocol the generated SDK decoders expect.
func newClient(t *testing.T, f *platformfake.Fake) compute.Client {
	t.Helper()
	f.SetEnv(t)
	ctx := context.Background()
	sdk, err := mws.Load(ctx, mws.WithDefaultProject(platformfake.Project), mws.WithTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("mws.Load against the fake: %v", err)
	}
	c, err := compute.NewSDKClient(ctx, sdk, platformfake.Project)
	if err != nil {
		t.Fatalf("compute.NewSDKClient: %v", err)
	}
	return c
}

func TestFake_GetVirtualMachine_translatesBothDiskClasses(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	vm, err := c.GetVirtualMachine(context.Background(), platformfake.VMName)
	if err != nil {
		t.Fatalf("GetVirtualMachine: %v", err)
	}
	if vm.Etag == "" {
		t.Error("VM etag is empty — the dependent write path refuses to write without one")
	}
	if vm.VMType != "base-2-8" {
		t.Errorf("VMType = %q, want base-2-8", vm.VMType)
	}
	if len(vm.Disks) != 2 {
		t.Fatalf("got %d disks, want 2", len(vm.Disks))
	}

	byDevice := map[string]compute.StorageDisk{}
	for _, d := range vm.Disks {
		byDevice[d.DeviceName] = d
	}
	boot, ok := byDevice["bootdisk"]
	if !ok {
		t.Fatalf("no disk with device_name bootdisk; got %v", byDevice)
	}
	if boot.Class != compute.ClassDependent {
		t.Errorf("bootdisk class = %q, want dependent (inline spec)", boot.Class)
	}
	if boot.DiskName != platformfake.DependentDisk {
		t.Errorf("bootdisk DiskName = %q, want %q", boot.DiskName, platformfake.DependentDisk)
	}
	data, ok := byDevice["datadisk"]
	if !ok {
		t.Fatalf("no disk with device_name datadisk; got %v", byDevice)
	}
	if data.Class != compute.ClassIndependent {
		t.Errorf("datadisk class = %q, want independent (ref)", data.Class)
	}
	if data.DiskName != platformfake.IndependentDisk {
		t.Errorf("datadisk DiskName = %q, want %q", data.DiskName, platformfake.IndependentDisk)
	}
}

func TestFake_GetDisk_carriesEtagAndIncarnation(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	d, err := c.GetDisk(context.Background(), platformfake.IndependentDisk)
	if err != nil {
		t.Fatalf("GetDisk: %v", err)
	}
	if d.Etag == "" {
		t.Error("disk etag is empty — the independent write path refuses to write without one")
	}
	if d.IncarnationID == "" {
		t.Error("disk metadata.id is empty — identity checks degrade to IOPS-only without it")
	}
	if d.SpecIOPS != 1000 {
		t.Errorf("SpecIOPS = %d, want 1000", d.SpecIOPS)
	}
}

func TestFake_GetDisk_unknownDiskIsCleanNotFound(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	_, err := c.GetDisk(context.Background(), "no-such-disk")
	if err == nil {
		t.Fatal("GetDisk: expected an error for an unknown disk name, got nil")
	}
	if strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("GetDisk error mentions Content-Type — the 404 body isn't wire-compatible: %v", err)
	}
	if !mwserrors.IsAPIErrorNotFoundStatus(err) {
		t.Fatalf("GetDisk error is not recognized as a NotFound API error: %v", err)
	}
}

func TestFake_GetVmType_returnsVCPUCount(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	vt, err := c.GetVmType(context.Background(), "base-2-8")
	if err != nil {
		t.Fatalf("GetVmType: %v", err)
	}
	if vt.VCPUCount != 2 {
		t.Errorf("VCPUCount = %d, want 2", vt.VCPUCount)
	}
}

func TestFake_UpdateDisk_appliesIOPSAndRecordsRequest(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	got, err := c.UpdateIndependentDisk(context.Background(), compute.IndependentUpdateRequest{
		DiskName:       platformfake.IndependentDisk,
		IOPS:           2000,
		Etag:           "disk-etag-1",
		IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("UpdateIndependentDisk: %v", err)
	}
	if got.SpecIOPS != 2000 {
		t.Errorf("response SpecIOPS = %d, want 2000", got.SpecIOPS)
	}
	if iops := f.DiskIOPS(platformfake.IndependentDisk); iops != 2000 {
		t.Errorf("fake state IOPS = %d, want 2000", iops)
	}

	var updates int
	for _, r := range f.Requests() {
		if r.Method == "POST" && r.IdempotencyKey == "key-1" {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("recorded %d updates carrying key-1, want 1", updates)
	}
}

func TestFake_IdempotencyKeyReplay_mutatesOnce(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)
	ctx := context.Background()

	req := compute.IndependentUpdateRequest{
		DiskName: platformfake.IndependentDisk, IOPS: 2000,
		Etag: "disk-etag-1", IdempotencyKey: "key-replay",
	}
	if _, err := c.UpdateIndependentDisk(ctx, req); err != nil {
		t.Fatalf("first update: %v", err)
	}
	// A replay of the same key must be answered from the first result, not applied again.
	req.IOPS = 9000
	if _, err := c.UpdateIndependentDisk(ctx, req); err != nil {
		t.Fatalf("replayed update: %v", err)
	}
	if iops := f.DiskIOPS(platformfake.IndependentDisk); iops != 2000 {
		t.Errorf("fake state IOPS = %d, want 2000 — the replay must not apply a second change", iops)
	}
}

func TestFake_UpdateDependentDisk_resolvesAliasToResourceName(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	got, err := c.UpdateDependentDisk(context.Background(), compute.DependentUpdateRequest{
		VMName:         platformfake.VMName,
		Etag:           "vm-etag-1",
		Aliases:        []string{"bootdisk", "datadisk"},
		TargetAlias:    "bootdisk",
		TargetIOPS:     3000,
		IdempotencyKey: "key-vm-1",
	})
	if err != nil {
		t.Fatalf("UpdateDependentDisk: %v", err)
	}

	var boot compute.StorageDisk
	var foundBoot bool
	for _, d := range got.Disks {
		if d.DeviceName == "bootdisk" {
			boot, foundBoot = d, true
		}
	}
	if !foundBoot {
		t.Fatalf("response has no bootdisk attachment; got %+v", got.Disks)
	}
	if boot.IOPS != 3000 {
		t.Errorf("response bootdisk IOPS = %d, want 3000", boot.IOPS)
	}

	if iops := f.DiskIOPS(platformfake.DependentDisk); iops != 3000 {
		t.Errorf("fake state IOPS (keyed by resource name %q) = %d, want 3000 — "+
			"the VM write must resolve alias %q to the resource name before applying",
			platformfake.DependentDisk, iops, "bootdisk")
	}
}

func TestFake_UpdateDisk_rejectsOutOfBoundsIOPS(t *testing.T) {
	tests := []struct {
		name    string
		iops    int64
		wantMsg string
	}{
		{"above max", 10100, "Requested iops can't be greater than maximum iops value: 10000"},
		{"below min", 900, "Requested iops can't be less than minimum iops value: 1000"},
		{"not divisible by 100", 1050, "iops count 1050 must be divisible by 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := platformfake.New(t)
			c := newClient(t, f)

			_, err := c.UpdateIndependentDisk(context.Background(), compute.IndependentUpdateRequest{
				DiskName: platformfake.IndependentDisk, IOPS: tt.iops,
				Etag: "disk-etag-1", IdempotencyKey: "key-" + tt.name,
			})
			if err == nil {
				t.Fatal("UpdateIndependentDisk succeeded, want a validation error")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want containing %q", err, tt.wantMsg)
			}
			if iops := f.DiskIOPS(platformfake.IndependentDisk); iops != 1000 {
				t.Errorf("fake state IOPS = %d, want 1000 — a rejected write must not mutate", iops)
			}
		})
	}
}

func TestFake_AbortNext_dropsConnectionThenSDKRetrySucceeds(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	path := "/compute/v1/projects/" + platformfake.Project + "/disks/" + platformfake.IndependentDisk
	f.AbortNext("POST", path)

	got, err := c.UpdateIndependentDisk(context.Background(), compute.IndependentUpdateRequest{
		DiskName: platformfake.IndependentDisk, IOPS: 2000,
		Etag: "disk-etag-1", IdempotencyKey: "key-abort",
	})
	if err != nil {
		t.Fatalf("UpdateIndependentDisk: %v", err)
	}
	if got.SpecIOPS != 2000 {
		t.Errorf("response SpecIOPS = %d, want 2000", got.SpecIOPS)
	}

	var attempts int
	for _, r := range f.Requests() {
		if r.Method == "POST" && r.Path == path && r.IdempotencyKey == "key-abort" {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("recorded %d attempts carrying key-abort, want 2 (the aborted attempt plus the SDK's automatic retry)", attempts)
	}
}

func TestFake_FailNext_returnsInjectedStatusOnce(t *testing.T) {
	f := platformfake.New(t)
	c := newClient(t, f)

	f.FailNext("POST", "/compute/v1/projects/"+platformfake.Project+"/disks/"+platformfake.IndependentDisk,
		400, `{"message":"Requested iops can't be greater than maximum iops value: 10000"}`)

	// A valid IOPS value, so this isolates the injection mechanism from the
	// fake's own bounds enforcement. Injection is consulted before validation.
	_, err := c.UpdateIndependentDisk(context.Background(), compute.IndependentUpdateRequest{
		DiskName: platformfake.IndependentDisk, IOPS: 2000,
		Etag: "disk-etag-1", IdempotencyKey: "key-400",
	})
	if err == nil {
		t.Fatal("UpdateIndependentDisk succeeded, want the injected 400")
	}
	if iops := f.DiskIOPS(platformfake.IndependentDisk); iops != 1000 {
		t.Errorf("fake state IOPS = %d, want 1000 — a rejected write must not mutate", iops)
	}
}
