# Integration Tests and CI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `docs/design.md` §8.2's integration tests against an in-process fake MWS API, add a black-box smoke test proving the built binary runs on a GitHub runner, and add one Actions workflow.

**Architecture:** A new `internal/platformfake` package serves the MWS Compute API and the instance metadata service from two `httptest` servers. Integration tests live in `cmd/mws-iops-autoscaler` (package `main`, build tag `integration`) and build a real `Agent` whose `client` is the real `compute.NewSDKClient` pointed at the fake by environment variable. They drive `Agent.actuate` directly with a constructed `decider.Decision` — the pattern `agent_test.go` already uses — so no saturation has to be synthesised through the analyzer. A separate smoke test compiles the binary and runs it as a child process.

**Tech Stack:** Go 1.26, `go.mws.cloud/go-sdk v0.19.0`, `net/http/httptest`, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-08-30-integration-tests-ci-design.md`

## Global Constraints

- Branch: `integration-tests`. All work commits there.
- **No production code changes.** If a task appears to need one, stop and report instead of making it.
- The `//go:build integration` tag goes on the tests that drive the agent or the binary — everything
  in `cmd/mws-iops-autoscaler` added by this plan. `internal/platformfake`'s own tests (Tasks 1-2) are
  ordinary fast unit tests of the fake itself: **untagged**, run by plain `go test ./...`, and they are
  what keeps the fake's wire fidelity under continuous check. Task 1 and 2 commands therefore omit
  `-tags integration` deliberately.
- The fake must serve `Content-Type: application/json`; the SDK rejects anything else with
  `invalid Content-Type` (verified by probe, spec §4).
- The fake is reached over plain `http://` — verified working; do not reach for TLS.
- Error bodies must be copied verbatim from `docs/platform-reference.md`; they are marked `[V]`
  (verified against the live platform) and are the point of the exercise.
- Seeded topology mirrors `docs/design.md` §8.3's reference environment: VM `iops-test-vm`, VM type
  `base-2-8` (2 vCPU), dependent disk alias/device `bootdisk` (resource `iops-test-vm-boot`),
  independent disk alias/device `datadisk` (resource `iops-test-data`).
- Run `gofmt -l` on every changed Go file before each commit.

---

### Task 1: `platformfake` read paths, proven by round-trip

This task exists to settle the fixtures. The exact JSON the SDK's generated decoders accept is not
fully known from the documentation, so the deliverable is not "JSON I believe is right" but "JSON the
real SDK client demonstrably translates into the expected domain objects". If the fixture is wrong the
test in Step 2 fails with a decode or translation mismatch; fix the fixture against
`go.mws.cloud/go-sdk/service/compute/model` until it passes. Do not change any repository code to make
it pass.

**Files:**
- Create: `internal/platformfake/fake.go`
- Test: `internal/platformfake/fake_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func New(t *testing.T) *Fake`
  - `func (f *Fake) BaseURL() string`
  - `func (f *Fake) MetadataURL() string`
  - `func (f *Fake) SetEnv(t *testing.T)` — sets `MWS_BASE_ENDPOINT`, `MWS_TOKEN`, `MWS_PROJECT`, `MWS_COMPUTE_METADATA_HOST` via `t.Setenv`
  - `const Project = "iops-test-project"`, `const VMName = "iops-test-vm"`, `const DependentDisk = "iops-test-vm-boot"`, `const IndependentDisk = "iops-test-data"`

- [ ] **Step 1: Write the failing round-trip test**

```go
package platformfake_test

import (
	"context"
	"testing"
	"time"

	"go.mws.cloud/go-sdk/mws"

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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/platformfake/ -run TestFake_ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Implement the fake's read paths**

Create `internal/platformfake/fake.go`. Serve every response with
`w.Header().Set("Content-Type", "application/json")`. Two servers: one for Compute plus discovery,
one for the metadata service. `New` registers `t.Cleanup` to close both.

Routes on the Compute server:
- `GET /endpoint` → `[{"id":"compute","address":"<compute server URL>"}]`
- `GET /compute/v1/projects/{project}/virtualMachines/{vm}` → the VM fixture
- `GET /compute/v1/projects/{project}/disks/{disk}` → the disk fixture for that name
- `GET /compute/v1/vmTypes/{vmType}` → `{"spec":{"cpu":{"vcpuCount":2}}}`

Route on the metadata server:
- `GET /computeMetadata/v1/instance/name` → `iops-test-vm` as a plain body

Seeded state, held under a mutex: per-disk `iops`, `etag`, `incarnation`. Start `bootdisk` and
`datadisk` both at 1000 IOPS.

The VM fixture must produce one dependent attachment (inline `disk.spec`) and one independent
attachment (`disk.ref`), with `status.storage.disks[]` carrying `deviceName` prefixed `mws-` and the
`ref` naming the disk resource — `translateVMResponse` in `internal/compute/sdk_client.go` reads
`deviceName` and `ref` from the status entry and the class from the spec entry. Start from this shape
and correct it against the SDK model types until Step 4 passes:

```json
{
  "metadata": {"etag": "vm-etag-1"},
  "spec": {
    "vmType": "base-2-8",
    "storage": {"disks": [
      {"name": "bootdisk", "boot": true,
       "disk": {"spec": {"diskType": "nbs-pl2", "size": "10Gi", "iops": 1000}}},
      {"name": "datadisk",
       "disk": {"ref": {"disk": "iops-test-data"}}}
    ]}
  },
  "status": {
    "storage": {"disks": [
      {"name": "bootdisk", "deviceName": "mws-bootdisk", "ref": {"disk": "iops-test-vm-boot"}},
      {"name": "datadisk", "deviceName": "mws-datadisk", "ref": {"disk": "iops-test-data"}}
    ]}
  }
}
```

Disk fixture shape, same treatment:

```json
{
  "metadata": {"etag": "disk-etag-1", "id": "01234567-89ab-cdef-0123-456789abcdef"},
  "spec": {"iops": 1000},
  "status": {"iops": {"base": 1000, "extra": 0, "total": 1000}}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/platformfake/ -run TestFake_ -v`
Expected: PASS, all three tests.

If a test fails on a missing or misnamed field, correct the fixture — never the repository's
translation code.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/platformfake/
git add internal/platformfake/
git commit -m "test: add MWS platform fake with round-trip-verified read paths"
```

---

### Task 2: `platformfake` write paths, request log, failure injection

**Files:**
- Modify: `internal/platformfake/fake.go`
- Test: `internal/platformfake/fake_test.go`

**Interfaces:**
- Consumes: Task 1's `New`, `SetEnv`, constants.
- Produces:
  - `type Request struct { Method, Path, Query, IdempotencyKey string }`
  - `func (f *Fake) Requests() []Request`
  - `func (f *Fake) DiskIOPS(name string) int64`
  - `func (f *Fake) FailNext(method, path string, status int, body string)`
  - `func (f *Fake) AbortNext(method, path string)`

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/platformfake/ -run 'TestFake_(UpdateDisk|IdempotencyKeyReplay|FailNext)' -v`
Expected: FAIL — `Requests`, `DiskIOPS`, `FailNext` undefined.

- [ ] **Step 3: Implement the write paths**

Add to `internal/platformfake/fake.go`:
- `POST /compute/v1/projects/{project}/disks/{disk}` — read `spec.iops` from the body, store it,
  return the disk fixture carrying the new value and a bumped etag.
- `POST /compute/v1/projects/{project}/virtualMachines/{vm}` — read
  `spec.storage.disks[]`, apply `spec.iops` for whichever alias carries one, return the VM fixture.
- Record every request into a slice before doing anything else, capturing the **`Idempotency-Key`**
  header — no `X-` prefix. Verified in the generated SDK: `req.Header.Add("Idempotency-Key", …)` in
  `service/compute/client/http/Disk.methods.gen.go` and the VM equivalent.
- **Enforce the platform's real IOPS bounds** so a bad request is rejected by the fake the way the
  platform rejects it, rather than by injection. From `docs/platform-reference.md` §"Server-side
  validation, measured", all `[V]`, returned as 400 with the body verbatim:
  - `> 10000` → `Requested iops can't be greater than maximum iops value: 10000`
  - `< 1000` → `Requested iops can't be less than minimum iops value: 1000`
  - not divisible by 100 → `iops count <N> must be divisible by 100`
  A rejected write must not mutate seeded state.
- Keep `map[string]int64` of applied idempotency keys to their resulting IOPS; on replay, return the
  stored result without mutating.
- `FailNext` and `AbortNext` push onto per-`method+path` queues consumed one request at a time.
  `AbortNext` must close the connection without a response — use
  `http.NewResponseController(w).EnableFullDuplex()` if needed, or panic with
  `http.ErrAbortHandler`, which `net/http` turns into a dropped connection.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/platformfake/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/platformfake/
git add internal/platformfake/
git commit -m "test: add platform fake write paths, request log and failure injection"
```

---

### Task 3: Scenario — a dependent disk never touches the disk-update endpoint

`docs/design.md` §8.2 words this as "403 → agent routes to VM path". Routing is actually decided by
`DiskClass` at discovery (`internal/actuator/actuator.go`), so the 403 is a trap: it proves the agent
never takes the forbidden path. The 403 body is the one recorded in `docs/platform-reference.md`.

**Files:**
- Create: `cmd/mws-iops-autoscaler/integration_test.go`

**Interfaces:**
- Consumes: Task 2's full `platformfake` API.
- Produces: `func newIntegrationAgent(t *testing.T, f *platformfake.Fake) *Agent` — an `Agent` wired
  with the real SDK client and real `actuator.NewComputeActuator`, reused by Tasks 4-7.

- [ ] **Step 1: Write the failing test**

```go
//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mws.cloud/go-sdk/mws"

	"github.com/rodion1010/mws-iops-autoscaler/internal/actuator"
	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/journal"
	"github.com/rodion1010/mws-iops-autoscaler/internal/platformfake"
	"github.com/rodion1010/mws-iops-autoscaler/internal/registry"
	"github.com/rodion1010/mws-iops-autoscaler/internal/state"
	"github.com/rodion1010/mws-iops-autoscaler/internal/telemetry"
)

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func newIntegrationAgent(t *testing.T, f *platformfake.Fake) *Agent {
	t.Helper()
	f.SetEnv(t)
	ctx := context.Background()
	sdk, err := mws.Load(ctx, mws.WithDefaultProject(platformfake.Project), mws.WithTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("mws.Load: %v", err)
	}
	client, err := compute.NewSDKClient(ctx, sdk, platformfake.Project)
	if err != nil {
		t.Fatalf("compute.NewSDKClient: %v", err)
	}
	budget, err := state.LoadBudget(t.TempDir() + "/budget.jsonl")
	if err != nil {
		t.Fatalf("LoadBudget: %v", err)
	}
	return &Agent{
		vmName:    platformfake.VMName,
		client:    client,
		actuator:  actuator.NewComputeActuator(client),
		tracker:   state.NewTracker(budget),
		telemetry: telemetry.NewRegistry(),
		journal:   journal.NewWriter(nopWriter{}),
	}
}

func integrationPolicy() config.ResolvedPolicy {
	return config.ResolvedPolicy{
		MinIOPS: 1000,
		MaxIOPS: 10000,
		Advanced: config.ResolvedAdvanced{
			MinChangeIOPS:    100,
			MaxScaleUpRatio:  2.0,
			MaxChangesPer24h: 4,
		},
	}
}

func dependentManaged() registry.ManagedDisk {
	return registry.ManagedDisk{
		Disk: compute.StorageDisk{
			Alias: "bootdisk", DeviceName: "bootdisk",
			Class: compute.ClassDependent, DiskName: platformfake.DependentDisk,
			IOPS: 1000,
		},
		Policy: integrationPolicy(),
	}
}

func independentManaged() registry.ManagedDisk {
	return registry.ManagedDisk{
		Disk: compute.StorageDisk{
			Alias: "datadisk", DeviceName: "datadisk",
			Class: compute.ClassIndependent, DiskName: platformfake.IndependentDisk,
			IOPS: 1000,
		},
		Policy: integrationPolicy(),
	}
}

// The disk-update endpoint 403s for a VM-owned disk (docs/platform-reference.md).
// A dependent scale-up must never go near it.
func TestIntegration_DependentWriteNeverCallsDiskUpdateEndpoint(t *testing.T) {
	f := platformfake.New(t)
	a := newIntegrationAgent(t, f)

	diskUpdatePath := "/compute/v1/projects/" + platformfake.Project + "/disks/" + platformfake.DependentDisk
	f.FailNext("POST", diskUpdatePath, 403,
		`{"message":"Resource 'compute/.../virtualMachines/iops-test-vm' is forbidden"}`)

	d := decider.Decision{
		DiskID: platformfake.DependentDisk, Action: decider.ActionScaleUp,
		Current: 1000, Target: 2000,
	}
	a.actuate(context.Background(), dependentManaged(), d, time.Now())

	for _, r := range f.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/disks/"+platformfake.DependentDisk) {
			t.Fatalf("dependent write called the disk-update endpoint %s — that path 403s on the real platform", r.Path)
		}
	}

	var vmUpdates int
	for _, r := range f.Requests() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/virtualMachines/"+platformfake.VMName) {
			vmUpdates++
		}
	}
	if vmUpdates != 1 {
		t.Errorf("VM-path updates = %d, want 1", vmUpdates)
	}
	if got := f.DiskIOPS(platformfake.DependentDisk); got != 2000 {
		t.Errorf("dependent disk IOPS = %d, want 2000", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_DependentWrite -v`
Expected: FAIL — the file does not compile yet, then FAIL on assertions until the fake's VM update
path applies IOPS correctly.

- [ ] **Step 3: Make it pass**

No repository code changes. Fix only the test file and, if the VM update path mishandles the alias
collection, `internal/platformfake/fake.go`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_DependentWrite -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/mws-iops-autoscaler/ internal/platformfake/
git add cmd/mws-iops-autoscaler/integration_test.go internal/platformfake/
git commit -m "test: assert a dependent write never calls the 403 disk endpoint"
```

---

### Task 4: Scenario — `iops` out of bounds returns the platform's own 400

Assert what exists: no mutation, and the reservation resolved. Do **not** assert a journal record for
the failure — the journal is written before actuation and has no write-outcome field (spec §6.2).

**Files:**
- Modify: `cmd/mws-iops-autoscaler/integration_test.go`

- [ ] **Step 1: Write the failing test**

```go
// No injection: the target really is out of bounds, and the fake rejects it with
// the platform's own recorded message the way the real API does.
func TestIntegration_OutOfBoundsIOPSSurfacesPlatform400AndAppliesNothing(t *testing.T) {
	f := platformfake.New(t)
	a := newIntegrationAgent(t, f)

	d := decider.Decision{
		DiskID: platformfake.IndependentDisk, Action: decider.ActionScaleUp,
		Current: 1000, Target: 20000, // above the platform maximum of 10000
	}
	a.actuate(context.Background(), independentManaged(), d, time.Now())

	if got := f.DiskIOPS(platformfake.IndependentDisk); got != 1000 {
		t.Errorf("disk IOPS = %d, want 1000 — a rejected write must not mutate", got)
	}
	// The write failed and live state proves it never landed, so the budget slot is released.
	// Tracker.State returns decider.GuardrailState; ChangesInLast24h is its budget counter.
	if used := a.tracker.State(independentManaged().Disk.IdentityKey(), time.Now()).ChangesInLast24h; used != 0 {
		t.Errorf("budget changes in last 24h = %d, want 0 after a write the platform rejected", used)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_OutOfBounds -v`
Expected: FAIL.

- [ ] **Step 3: Make it pass**

Test-file changes only.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_OutOfBounds -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/mws-iops-autoscaler/
git add cmd/mws-iops-autoscaler/integration_test.go
git commit -m "test: assert the platform's 400 on out-of-bounds iops applies nothing"
```

---

### Task 5: Scenario — an etag conflict mutates nothing

Assert only what design and implementation agree on. Spec §8.5 records that "abort the cycle" does not
hold today; do not assert it in either direction.

**Files:**
- Modify: `cmd/mws-iops-autoscaler/integration_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestIntegration_EtagConflictMutatesNothing(t *testing.T) {
	f := platformfake.New(t)
	a := newIntegrationAgent(t, f)

	path := "/compute/v1/projects/" + platformfake.Project + "/disks/" + platformfake.IndependentDisk
	// The SDK retries 409, so queue enough rejections to outlast its attempts.
	for i := 0; i < 6; i++ {
		f.FailNext("POST", path, 409, `{"message":"invalid etag"}`)
	}

	d := decider.Decision{
		DiskID: platformfake.IndependentDisk, Action: decider.ActionScaleUp,
		Current: 1000, Target: 2000,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.actuate(ctx, independentManaged(), d, time.Now())

	if got := f.DiskIOPS(platformfake.IndependentDisk); got != 1000 {
		t.Errorf("disk IOPS = %d, want 1000 — an etag conflict must not mutate", got)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_EtagConflict -v`
Expected: FAIL.

- [ ] **Step 3: Make it pass**

Test-file changes only. If the test takes longer than about 30 seconds, the SDK's retry backoff is the
cause; record the measured duration in the commit message rather than disabling retries.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_EtagConflict -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/mws-iops-autoscaler/
git add cmd/mws-iops-autoscaler/integration_test.go
git commit -m "test: assert an etag conflict leaves disk state untouched"
```

---

### Task 6: Scenario — every write carries an idempotency key

**Files:**
- Modify: `cmd/mws-iops-autoscaler/integration_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestIntegration_WritesCarryAnIdempotencyKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed registry.ManagedDisk
		diskID  string
	}{
		{"independent", independentManaged(), platformfake.IndependentDisk},
		{"dependent", dependentManaged(), platformfake.DependentDisk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := platformfake.New(t)
			a := newIntegrationAgent(t, f)

			d := decider.Decision{
				DiskID: tc.diskID, Action: decider.ActionScaleUp,
				Current: 1000, Target: 2000,
			}
			a.actuate(context.Background(), tc.managed, d, time.Now())

			var posts, keyed int
			for _, r := range f.Requests() {
				if r.Method != "POST" {
					continue
				}
				posts++
				if r.IdempotencyKey != "" {
					keyed++
				}
			}
			if posts == 0 {
				t.Fatal("no write reached the fake")
			}
			if keyed != posts {
				t.Errorf("%d of %d writes carried an idempotency key, want all of them", keyed, posts)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_WritesCarry -v`
Expected: FAIL.

- [ ] **Step 3: Make it pass**

If `keyed` is zero, the header name captured in Task 2 is wrong. Dump `r.Header` in the fake, find the
real header, and fix `platformfake`. No repository code changes.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_WritesCarry -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/mws-iops-autoscaler/ internal/platformfake/
git add cmd/mws-iops-autoscaler/integration_test.go internal/platformfake/
git commit -m "test: assert both write paths carry an idempotency key"
```

---

### Task 7: Scenario — a dropped connection mid-write yields exactly one applied change

**Files:**
- Modify: `cmd/mws-iops-autoscaler/integration_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestIntegration_AbortedWriteRetriesAndAppliesExactlyOneChange(t *testing.T) {
	f := platformfake.New(t)
	a := newIntegrationAgent(t, f)

	path := "/compute/v1/projects/" + platformfake.Project + "/disks/" + platformfake.IndependentDisk
	f.AbortNext("POST", path)

	d := decider.Decision{
		DiskID: platformfake.IndependentDisk, Action: decider.ActionScaleUp,
		Current: 1000, Target: 2000,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.actuate(ctx, independentManaged(), d, time.Now())

	if got := f.DiskIOPS(platformfake.IndependentDisk); got != 2000 {
		t.Errorf("disk IOPS = %d, want 2000 — the SDK retries transport errors", got)
	}

	keys := map[string]int{}
	for _, r := range f.Requests() {
		if r.Method == "POST" && r.IdempotencyKey != "" {
			keys[r.IdempotencyKey]++
		}
	}
	if len(keys) != 1 {
		t.Fatalf("writes used %d distinct idempotency keys, want 1 — a retry must reuse the reservation's key", len(keys))
	}
	for k, n := range keys {
		if n < 2 {
			t.Errorf("key %s was sent %d time(s); the aborted attempt should have been retried", k, n)
		}
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_AbortedWrite -v`
Expected: FAIL.

- [ ] **Step 3: Make it pass**

If the abort does not produce a retry, verify `AbortNext` really drops the connection —
`panic(http.ErrAbortHandler)` inside the handler is the reliable way. If the aborted request is not
recorded, record it before aborting.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestIntegration_AbortedWrite -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/mws-iops-autoscaler/ internal/platformfake/
git add cmd/mws-iops-autoscaler/integration_test.go internal/platformfake/
git commit -m "test: assert a dropped connection retries into exactly one applied change"
```

---

### Task 8: Black-box smoke test of the built binary

**Files:**
- Create: `cmd/mws-iops-autoscaler/smoke_test.go`
- Create: `cmd/mws-iops-autoscaler/testdata/smoke-config.yaml`

**Interfaces:**
- Consumes: Task 2's `platformfake` API.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Write the config fixture**

Write `cmd/mws-iops-autoscaler/testdata/smoke-config.yaml` exactly as below. This is the minimal
document `internal/config/load_test.go` treats as valid; `runtime: {}` and both `policy.limits` bounds
are required, unknown top-level keys are rejected, and `apiVersion` must match verbatim.

```yaml
apiVersion: autoscaling.mws.cloud/v1alpha1
kind: IOPSAutoScaler
mode: local
policy:
  limits:
    min_iops: 1000
    max_iops: 10000
disks:
  mode: auto-discovery
runtime: {}
```

- [ ] **Step 2: Write the failing test**

```go
//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/platformfake"
)

// freePort reserves a port and releases it for the child to bind. Racy in
// principle, acceptable for a single-job CI (spec §8.3).
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestSmoke_BinaryStartsServesHealthAndShutsDownCleanly(t *testing.T) {
	f := platformfake.New(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "mws-iops-autoscaler")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	addr := freePort(t)
	cmd := exec.Command(bin,
		"-config", "testdata/smoke-config.yaml",
		"-state-dir", filepath.Join(dir, "state"),
		"-journal", filepath.Join(dir, "journal.jsonl"),
		"-listen", addr,
	)
	cmd.Env = append(os.Environ(),
		"MWS_BASE_ENDPOINT="+f.BaseURL(),
		"MWS_COMPUTE_METADATA_HOST="+f.MetadataURL(),
		"MWS_TOKEN=smoke-token",
		"MWS_PROJECT="+platformfake.Project,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the listener rather than sleeping a fixed interval.
	deadline := time.Now().Add(30 * time.Second)
	var healthz *http.Response
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			healthz = resp
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if healthz == nil {
		t.Fatal("binary never served /healthz")
	}
	defer healthz.Body.Close()
	if healthz.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", healthz.StatusCode)
	}

	// main.go starts the HTTP listener (main.go:186) BEFORE agent.Run performs
	// initial discovery (main.go:201), so a 200 from /healthz does not mean
	// discovery has run yet. Poll /readyz until discovery_succeeded flips rather
	// than reading it once and racing the agent.
	type readyBody struct {
		Ready              bool `json:"ready"`
		ConfigValid        bool `json:"config_valid"`
		DiscoverySucceeded bool `json:"discovery_succeeded"`
		SourceReachable    bool `json:"source_reachable"`
	}
	var body readyBody
	var status int
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		var b readyBody
		decodeErr := json.NewDecoder(resp.Body).Decode(&b)
		status = resp.StatusCode
		resp.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode /readyz: %v", decodeErr)
		}
		body = b
		if b.DiscoverySucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !body.DiscoverySucceeded {
		t.Fatal("discovery_succeeded never became true — the binary never reached the fake Compute API")
	}
	if !body.ConfigValid {
		t.Error("config_valid = false, want true")
	}
	// No /dev/disk/by-id/mws-* exists on a runner, so every disk fails guest-path
	// verification, the managed set is empty and readiness must say so.
	if body.SourceReachable {
		t.Error("source_reachable = true, but no /dev/disk/by-id/mws-* exists on a runner")
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 while source_reachable is false", status)
	}

	metrics, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", metrics.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("binary exited with %v, want a clean exit on SIGTERM", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("binary did not exit within 15s of SIGTERM")
	}
	_ = context.Background()
}
```

- [ ] **Step 3: Validate the config fixture, then run the test**

Run: `go run . -config testdata/smoke-config.yaml -validate-config`
Expected: `valid: mode=local limits=[...] disks.mode=...`. Fix the fixture until it validates.

Then run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestSmoke -v`
Expected: FAIL initially.

- [ ] **Step 4: Make it pass**

Test and fixture changes only. If `discovery_succeeded` is false, the fake's VM name or project does
not match what the binary asks for — reconcile the constants, not the binary.

- [ ] **Step 5: Run it and confirm it passes**

Run: `go test -tags integration ./cmd/mws-iops-autoscaler/ -run TestSmoke -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l cmd/mws-iops-autoscaler/
git add cmd/mws-iops-autoscaler/smoke_test.go cmd/mws-iops-autoscaler/testdata/
git commit -m "test: add a black-box smoke test of the built binary"
```

---

### Task 9: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Write the workflow**

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

permissions:
  contents: read

jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: gofmt
        run: |
          unformatted=$(gofmt -l .)
          if [ -n "$unformatted" ]; then
            echo "These files are not gofmt-formatted:"
            echo "$unformatted"
            exit 1
          fi

      - name: go vet
        run: go vet ./...

      - name: go test
        run: go test ./...

      - name: go test -race
        run: go test -race ./...

      - name: integration tests
        run: go test -tags integration ./...
```

- [ ] **Step 2: Verify every command locally first**

Run each in order and confirm all pass before committing:

```bash
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
go test -tags integration ./...
```

- [ ] **Step 3: Commit and push the branch**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: add the verification workflow"
git push -u origin integration-tests
```

- [ ] **Step 4: Confirm the run is green**

Open the branch's Actions run on GitHub and confirm every step passed. If `go test -race` or the
integration job is slow, record the measured wall-clock time — the spec budgets roughly four minutes
per run against 2000 free minutes per month.

---

### Task 10: Documentation

**Files:**
- Modify: `docs/platform-reference.md`
- Modify: `docs/development.md`
- Modify: `docs/superpowers/specs/2026-08-30-integration-tests-ci-design.md`

- [ ] **Step 1: Move the verified SDK facts to their canonical home**

Add to `docs/platform-reference.md` under the existing `## Go SDK` section, marked `[V]` because each
was verified in this work:

- the environment seams `MWS_BASE_ENDPOINT`, `MWS_TOKEN`, `MWS_PROJECT`, `MWS_COMPUTE_METADATA_HOST`;
- endpoint discovery: one `GET <base>/endpoint` returning `[{"id":"compute","address":"..."}]`;
- a plain `http://` base endpoint is accepted, and responses must carry
  `Content-Type: application/json`;
- the default retryer retries any transport error plus 408, 409, 423, 424, 429, 500, 502, 503, 504,
  521, 522 and 524, with exponential jitter from 20 ms to 10 s.

- [ ] **Step 2: Shrink the spec's fact table to a link**

Replace §4 of the spec with a pointer to the `docs/platform-reference.md` sections just added, keeping
only the CI-build fact (that `proxy.golang.org` serves the SDK), which is about this repository's CI
rather than about the platform.

- [ ] **Step 3: Document how to run the integration tests**

Add to `docs/development.md`: integration tests are behind the `integration` build tag and run with
`go test -tags integration ./...`; they need no credentials and no network.

- [ ] **Step 4: Verify links and commit**

Check that every relative link in the changed files resolves.

```bash
git add docs/
git commit -m "docs: record verified SDK facts and how to run integration tests"
```

- [ ] **Step 5: Open the two issues the spec identified**

1. §8.2's metrics-credential scenario is unreachable until M7.
2. A server-side etag conflict is not classified as `ErrConcurrentModification`, so the reconcile
   cycle does not abort as `docs/design.md` §8.2 states (spec §8.5).

Report both to the Product Lead rather than editing `docs/design.md`.
