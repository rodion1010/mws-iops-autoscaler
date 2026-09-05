//go:build integration

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mws.cloud/go-sdk/mws"

	"github.com/rodion1010/mws-iops-autoscaler/internal/actuator"
	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/health"
	"github.com/rodion1010/mws-iops-autoscaler/internal/journal"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
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
	sampler, err := metrics.NewSampler(emptySource{}, 10)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	return &Agent{
		vmName: platformfake.VMName,
		// cfg, resolved, sampler and readiness are not exercised by any test
		// today (actuate never reaches discover — see the §8.5 note in
		// docs/superpowers/specs/2026-08-30-integration-tests-ci-design.md),
		// but a nil-pointer panic there would be an illegible failure if that
		// changes. Populated the same way as newDiscoverableTestAgent in
		// agent_test.go so discover fails with a real error instead.
		cfg:       &config.Config{Disks: config.DisksSpec{Mode: "auto-discovery"}},
		resolved:  &config.ResolvedConfig{},
		client:    client,
		sampler:   sampler,
		actuator:  actuator.NewComputeActuator(client),
		tracker:   state.NewTracker(budget),
		telemetry: telemetry.NewRegistry(),
		journal:   journal.NewWriter(nopWriter{}),
		readiness: &health.Readiness{},
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
		`{"code":"PERMISSION_DENIED","message":"Resource 'compute/.../virtualMachines/iops-test-vm' is forbidden"}`)

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

// No injection: the target really is out of bounds, and the fake rejects it with
// the platform's own recorded message the way the real API does.
func TestIntegration_OutOfBoundsIOPSSurfacesPlatform400AndAppliesNothing(t *testing.T) {
	f := platformfake.New(t)
	a := newIntegrationAgent(t, f)

	path := "/compute/v1/projects/" + platformfake.Project + "/disks/" + platformfake.IndependentDisk
	d := decider.Decision{
		DiskID: platformfake.IndependentDisk, Action: decider.ActionScaleUp,
		Current: 1000, Target: 20000, // above the platform maximum of 10000
	}
	a.actuate(context.Background(), independentManaged(), d, time.Now())

	// A write that never reached the fake at all (e.g. a budget reservation
	// failure before Apply is ever called) has the exact same DiskIOPS/budget
	// signature as one the platform rejected — confirm the write was actually
	// attempted before trusting the assertions below. A 404, a 403 or an
	// undecodable body would satisfy those same signatures too, so also
	// confirm the fake actually answered with the platform's 400: nothing
	// else observes the status this test's name promises.
	var posts int
	var statuses []int
	for _, r := range f.Requests() {
		if r.Method == "POST" && r.Path == path {
			posts++
			statuses = append(statuses, r.Status)
		}
	}
	if posts == 0 {
		t.Fatal("no write reached the fake — the assertions below would also pass if actuate never attempted a write at all")
	}
	for _, s := range statuses {
		if s != http.StatusBadRequest {
			t.Errorf("POST status = %d, want %d — the out-of-bounds IOPS must be rejected with the platform's 400", s, http.StatusBadRequest)
		}
	}

	if got := f.DiskIOPS(platformfake.IndependentDisk); got != 1000 {
		t.Errorf("disk IOPS = %d, want 1000 — a rejected write must not mutate", got)
	}
	if applies := f.Applies(platformfake.IndependentDisk); applies != 0 {
		t.Errorf("disk was mutated %d time(s), want 0 — a rejected write must not apply", applies)
	}
	// The write failed and live state proves it never landed, so the budget slot is released.
	// Tracker.State returns decider.GuardrailState; ChangesInLast24h is its budget counter.
	if used := a.tracker.State(independentManaged().Disk.IdentityKey(), time.Now()).ChangesInLast24h; used != 0 {
		t.Errorf("budget changes in last 24h = %d, want 0 after a write the platform rejected", used)
	}
}

func TestIntegration_EtagConflictMutatesNothing(t *testing.T) {
	f := platformfake.New(t)
	a := newIntegrationAgent(t, f)

	path := "/compute/v1/projects/" + platformfake.Project + "/disks/" + platformfake.IndependentDisk
	// The SDK retries 409, so queue enough rejections to outlast its attempts.
	// The code must be "INVALID_ETAG_KEY" — the SDK's decoder requires a "code"
	// field on every error body, and the IsAPIErrorInvalidEtagKeyStatus branch
	// in UpdateIndependentDisk (internal/compute/sdk_client.go) keys on that
	// exact string to recognize an etag conflict.
	for i := 0; i < 6; i++ {
		f.FailNext("POST", path, 409, `{"code":"INVALID_ETAG_KEY","message":"invalid etag"}`)
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

	var posts int
	for _, r := range f.Requests() {
		if r.Method == "POST" && r.Path == path {
			posts++
		}
	}
	// "Nothing mutated" is also true if the write was never attempted, or if
	// the injected body failed to decode into a classified APIError at all
	// (a decode error is not retryable, so the SDK stops after exactly one
	// POST). A genuinely classified 409/INVALID_ETAG_KEY is retryable and
	// produces several — so posts > 1 is what actually proves the SDK
	// recognized this as an etag conflict, without depending on any
	// particular log or error-message wording.
	if posts <= 1 {
		t.Fatalf("only %d POST(s) reached the fake, want more than 1 — a single attempt means the error was never classified as a retryable etag conflict (e.g. the injected body failed to decode), so the disk was never really at risk of mutating", posts)
	}
}

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
	// The fake's write is value-idempotent and the retried request reuses the
	// same idempotency key, so the final IOPS value and key-repetition count
	// alone cannot distinguish "abort, then apply once" from "apply, then
	// apply again". Applies counts only actual state mutations, not replayed
	// keys, so this is what actually asserts §8.2's single-effective-change
	// half of the contract.
	if applies := f.Applies(platformfake.IndependentDisk); applies != 1 {
		t.Errorf("disk was mutated %d time(s), want 1 — a retried write must apply exactly once", applies)
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
