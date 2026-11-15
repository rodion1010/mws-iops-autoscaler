//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
)

// scriptedClient models stale reads, a racing writer, and delayed scale-up.
type scriptedClient struct {
	vmName     string
	deviceName string
	alias      string
	diskName   string
	class      compute.DiskClass

	trueIOPS int64

	staleCycles int
	staleLeft   int
	staleIOPS   int64
	cycleStale  bool

	orphanRaise int64

	raiseAfterCycles int
	raiseTo          int64
	cycles           int
}

func (f *scriptedClient) openCycle() {
	f.cycles++
	if f.raiseAfterCycles > 0 && f.cycles > f.raiseAfterCycles {
		f.trueIOPS = f.raiseTo
	}
	if f.staleLeft > 0 {
		f.cycleStale = true
		f.staleLeft--
	} else {
		f.cycleStale = false
	}
}

func (f *scriptedClient) readIOPS() int64 {
	if f.cycleStale {
		return f.staleIOPS
	}
	return f.trueIOPS
}

func (f *scriptedClient) virtualMachine(iops int64) *compute.VirtualMachine {
	return &compute.VirtualMachine{
		Name:   f.vmName,
		Etag:   "etag-1",
		VMType: "type-1",
		Disks: []compute.StorageDisk{{
			Alias:      f.alias,
			DeviceName: f.deviceName,
			Class:      f.class,
			DiskName:   f.diskName,
			DiskType:   "nbs-pl2",
			SizeRaw:    "10GiB",
			IOPS:       iops,
		}},
	}
}

func (f *scriptedClient) GetVirtualMachine(_ context.Context, name string) (*compute.VirtualMachine, error) {
	if name != f.vmName {
		return nil, errScriptedNotFound
	}
	f.openCycle()
	return f.virtualMachine(f.readIOPS()), nil
}

func (f *scriptedClient) GetVmType(_ context.Context, _ string) (*compute.VmType, error) {
	return &compute.VmType{Name: "type-1", VCPUCount: 2}, nil
}

func (f *scriptedClient) GetDisk(_ context.Context, name string) (*compute.Disk, error) {
	if name != f.diskName {
		return nil, errScriptedNotFound
	}
	return &compute.Disk{
		Name:          f.diskName,
		SpecIOPS:      f.readIOPS(),
		Etag:          "etag-1",
		IncarnationID: "inc-1",
	}, nil
}

func (f *scriptedClient) UpdateIndependentDisk(_ context.Context, req compute.IndependentUpdateRequest) (*compute.Disk, error) {
	f.recordWrite(req.IOPS)
	return &compute.Disk{Name: f.diskName, SpecIOPS: req.IOPS, Etag: "etag-2", IncarnationID: "inc-1"}, nil
}

func (f *scriptedClient) UpdateDependentDisk(_ context.Context, req compute.DependentUpdateRequest) (*compute.VirtualMachine, error) {
	f.recordWrite(req.TargetIOPS)
	return f.virtualMachine(req.TargetIOPS), nil
}

func (f *scriptedClient) recordWrite(target int64) {
	f.staleIOPS = f.trueIOPS
	f.trueIOPS = target
	f.staleLeft = f.staleCycles
	if f.orphanRaise > 0 {
		f.trueIOPS = f.orphanRaise
	}
}

type scriptedError string

func (e scriptedError) Error() string { return string(e) }

const errScriptedNotFound = scriptedError("scriptedClient: unknown resource")

func newScriptedTarget(class compute.DiskClass) Target {
	return Target{
		VMName:        "vm-1",
		DeviceName:    "datadisk",
		BaselineIOPS:  1000,
		ExpectedClass: class,
	}
}

func fastResetTiming() resetTiming {
	return resetTiming{
		confirmInterval: time.Millisecond,
		confirmDeadline: 250 * time.Millisecond,
		retryWait:       time.Millisecond,
	}
}

func TestRegression_Preflight_ResetConfirmsBaselineAcrossStaleReads(t *testing.T) {
	for _, class := range []compute.DiskClass{compute.ClassDependent, compute.ClassIndependent} {
		t.Run(string(class), func(t *testing.T) {
			f := &scriptedClient{
				vmName: "vm-1", deviceName: "datadisk", alias: "data", diskName: "disk-1",
				class:       class,
				trueIOPS:    1500,
				staleCycles: 2,
			}
			ctx := context.Background()
			if err := resetDiskToBaseline(ctx, f, newScriptedTarget(class), fastResetTiming()); err != nil {
				t.Fatalf("reset must ride out stale reads and confirm baseline, got: %v", err)
			}
			if f.trueIOPS != 1000 {
				t.Fatalf("disk left at %d, want baseline 1000", f.trueIOPS)
			}
		})
	}
}

func TestRegression_Preflight_ResetFailsWhenOrphanRaces(t *testing.T) {
	for _, class := range []compute.DiskClass{compute.ClassDependent, compute.ClassIndependent} {
		t.Run(string(class), func(t *testing.T) {
			f := &scriptedClient{
				vmName: "vm-1", deviceName: "datadisk", alias: "data", diskName: "disk-1",
				class:       class,
				trueIOPS:    1500,
				orphanRaise: 1500,
			}
			ctx := context.Background()
			err := resetDiskToBaseline(ctx, f, newScriptedTarget(class), fastResetTiming())
			if err == nil {
				t.Fatalf("reset must fail closed when a racing writer keeps re-raising the disk, but returned nil — the disk would read as reset while actually elevated")
			}
			if !strings.Contains(err.Error(), "confirm baseline") {
				t.Fatalf("failure should come from baseline confirmation, got: %v", err)
			}
		})
	}
}

func TestRegression_WaitForScaleUp_RejectsAlreadyElevated(t *testing.T) {
	f := &scriptedClient{
		vmName: "vm-1", deviceName: "datadisk", alias: "data", diskName: "disk-1",
		class:    compute.ClassDependent,
		trueIOPS: 1500,
	}
	ctx := context.Background()
	_, err := waitForScaleUp(ctx, f, newScriptedTarget(compute.ClassDependent), 50*time.Millisecond, time.Hour, time.Millisecond, time.Now())
	if err == nil {
		t.Fatalf("an already-elevated first observation must be a precondition failure, not success")
	}
	if !strings.Contains(err.Error(), "precondition") {
		t.Fatalf("error should name the precondition violation, got: %v", err)
	}
}

func TestRegression_WaitForScaleUp_RejectsEarlyElevation(t *testing.T) {
	f := &scriptedClient{
		vmName: "vm-1", deviceName: "datadisk", alias: "data", diskName: "disk-1",
		class:            compute.ClassDependent,
		trueIOPS:         1000,
		raiseAfterCycles: 1,
		raiseTo:          1200,
	}
	ctx := context.Background()
	agentStarted := time.Now()
	_, err := waitForScaleUp(ctx, f, newScriptedTarget(compute.ClassDependent), 2*time.Second, 500*time.Millisecond, 5*time.Millisecond, agentStarted)
	if err == nil {
		t.Fatalf("a rise before the scale-up window could elapse must fail closed, not pass")
	}
	if !strings.Contains(err.Error(), "scale-up window") {
		t.Fatalf("error should name the scale-up window violation, got: %v", err)
	}
}

func TestRegression_WaitForScaleUp_AcceptsObservedBaselineThenRise(t *testing.T) {
	f := &scriptedClient{
		vmName: "vm-1", deviceName: "datadisk", alias: "data", diskName: "disk-1",
		class:            compute.ClassDependent,
		trueIOPS:         1000,
		raiseAfterCycles: 7,
		raiseTo:          1200,
	}
	ctx := context.Background()
	agentStarted := time.Now()
	final, err := waitForScaleUp(ctx, f, newScriptedTarget(compute.ClassDependent), 2*time.Second, 20*time.Millisecond, 5*time.Millisecond, agentStarted)
	if err != nil {
		t.Fatalf("a rise at/after the scale-up window above an observed baseline must succeed, got: %v", err)
	}
	if final != 1200 {
		t.Fatalf("final IOPS = %d, want 1200", final)
	}
}

func TestRegression_WaitForScaleUp_TimesOutAtBaseline(t *testing.T) {
	f := &scriptedClient{
		vmName: "vm-1", deviceName: "datadisk", alias: "data", diskName: "disk-1",
		class:    compute.ClassDependent,
		trueIOPS: 1000,
	}
	ctx := context.Background()
	_, err := waitForScaleUp(ctx, f, newScriptedTarget(compute.ClassDependent), 30*time.Millisecond, time.Hour, 5*time.Millisecond, time.Now())
	if err == nil {
		t.Fatalf("a disk that never rises above an observed baseline must time out")
	}
	if !strings.Contains(err.Error(), "still at") {
		t.Fatalf("error should be the timeout, got: %v", err)
	}
}

// sshWarning exercises marker parsing with prefixed stderr noise.
const sshWarning = "Warning: Permanently added '203.0.113.7' (ED25519) to the list of known hosts.\n"

type fakeRunner struct {
	absentAnswers []string
	absentCalls   int
	killSignals   []string
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, "pkill"):
		sig := "TERM"
		if strings.Contains(cmd, "-KILL") {
			sig = "KILL"
		}
		f.killSignals = append(f.killSignals, sig)
		return "", nil
	case strings.Contains(cmd, "pgrep"):
		idx := f.absentCalls
		if idx >= len(f.absentAnswers) {
			idx = len(f.absentAnswers) - 1
		}
		f.absentCalls++
		return f.absentAnswers[idx], nil
	default:
		return "", nil
	}
}

func fastAgentStopTiming() agentStopTiming {
	return agentStopTiming{termAttempts: 2, killAttempts: 2, interval: time.Millisecond}
}

func TestRegression_Preflight_StopStaleAgentVerifiesAbsence(t *testing.T) {
	f := &fakeRunner{absentAnswers: []string{sshWarning + agentPresentMarker}}
	timing := fastAgentStopTiming()
	ctx := context.Background()
	err := stopStaleAgent(ctx, f, timing)
	if err == nil {
		t.Fatalf("an agent that keeps showing present after kills must fail the stop, not pass it")
	}
	if !strings.Contains(err.Error(), "SIGTERM") || !strings.Contains(err.Error(), "SIGKILL") {
		t.Fatalf("error should name both escalation stages, got: %v", err)
	}
	wantKills := timing.termAttempts + timing.killAttempts
	if len(f.killSignals) != wantKills {
		t.Fatalf("kill rounds = %d, want %d", len(f.killSignals), wantKills)
	}
	if f.killSignals[len(f.killSignals)-1] != "KILL" {
		t.Fatalf("escalation must end with SIGKILL, got signals %v", f.killSignals)
	}
}

func TestRegression_Preflight_StopStaleAgentSucceedsOnConvergedAbsence(t *testing.T) {
	f := &fakeRunner{absentAnswers: []string{
		sshWarning + agentPresentMarker,
		sshWarning + agentAbsentMarker,
	}}
	ctx := context.Background()
	if err := stopStaleAgent(ctx, f, fastAgentStopTiming()); err != nil {
		t.Fatalf("an agent gone by the second check must succeed, got: %v", err)
	}
	if len(f.killSignals) != 2 || f.killSignals[0] != "TERM" || f.killSignals[1] != "TERM" {
		t.Fatalf("a graceful-path stop must use only SIGTERM, got signals %v", f.killSignals)
	}
}

func TestRegression_Preflight_StopStaleAgentEscalatesToKill(t *testing.T) {
	f := &fakeRunner{absentAnswers: []string{
		sshWarning + agentPresentMarker,
		sshWarning + agentPresentMarker,
		sshWarning + agentAbsentMarker,
	}}
	ctx := context.Background()
	if err := stopStaleAgent(ctx, f, fastAgentStopTiming()); err != nil {
		t.Fatalf("an agent that outlives SIGTERM must be stopped by SIGKILL escalation, got: %v", err)
	}
	want := []string{"TERM", "TERM", "KILL"}
	if len(f.killSignals) != len(want) {
		t.Fatalf("kill rounds = %v, want %v", f.killSignals, want)
	}
	for i, sig := range want {
		if f.killSignals[i] != sig {
			t.Fatalf("kill signals = %v, want %v", f.killSignals, want)
		}
	}
}

func TestRegression_Preflight_StopStaleAgentFailsClosedOnBrokenAbsenceCheck(t *testing.T) {
	for name, output := range map[string]string{
		"pgrep error":  sshWarning + agentPgrepErrorPrefix + "2\n",
		"unrecognized": sshWarning + "something else entirely\n",
		"no output":    "",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{absentAnswers: []string{output}}
			ctx := context.Background()
			err := stopStaleAgent(ctx, f, fastAgentStopTiming())
			if err == nil {
				t.Fatalf("a broken absence check must fail closed, not pass — output %q would leave the stop verdict guessed", output)
			}
			if !strings.Contains(err.Error(), "verify absence") {
				t.Fatalf("failure should come from the absence check, got: %v", err)
			}
		})
	}
}

// TestRegression_Config_SelectedDisksRendersEveryDeviceName guards the
// disks: selected block InstallAgent's default path relies on — a
// regression here would silently change what every existing scenario's
// agent manages.
func TestRegression_Config_SelectedDisksRendersEveryDeviceName(t *testing.T) {
	got := disksBlockYAML(SelectedDisksSpec("boot", "data"))
	want := "disks:\n  mode: selected\n  selected:\n    - device_name: boot\n    - device_name: data\n"
	if got != want {
		t.Fatalf("disksBlockYAML(selected) =\n%s\nwant:\n%s", got, want)
	}
}

// TestRegression_Config_AutoDiscoveryOmitsExcludeWhenEmpty checks that
// mode: auto-discovery with no excluded names renders no exclude: key at
// all, rather than an empty list — internal/config's YAML decoder treats an
// absent key and an empty list the same way, but an empty-list rendering
// would be easy to mistake for a real (if unlikely) exclude-list-of-nothing.
func TestRegression_Config_AutoDiscoveryOmitsExcludeWhenEmpty(t *testing.T) {
	got := disksBlockYAML(AutoDiscoveryDisksSpec())
	want := "disks:\n  mode: auto-discovery\n"
	if got != want {
		t.Fatalf("disksBlockYAML(auto-discovery, no exclude) =\n%s\nwant:\n%s", got, want)
	}
}

// TestRegression_Config_AutoDiscoveryRendersExcludedNames guards the
// exclude: list shape — internal/config.DisksSpec.Exclude is []string, a
// bare name per line, not a device_name: key like Selected.
func TestRegression_Config_AutoDiscoveryRendersExcludedNames(t *testing.T) {
	got := disksBlockYAML(AutoDiscoveryDisksSpec("noname"))
	want := "disks:\n  mode: auto-discovery\n  exclude:\n    - noname\n"
	if got != want {
		t.Fatalf("disksBlockYAML(auto-discovery, exclude) =\n%s\nwant:\n%s", got, want)
	}
}

// TestRegression_Config_DefaultDiscoveryIntervalUnchangedByDisksSpec pins
// agentConfigYAML's discovery_interval at defaultDiscoveryInterval — the
// value main.go's maxDiscoveryAge is derived from for every existing
// scenario. See discoveryScenarioInterval's comment for why a scenario
// needing faster rediscovery must opt in via agentConfigYAMLWithDisks
// instead of this constant changing.
func TestRegression_Config_DefaultDiscoveryIntervalUnchangedByDisksSpec(t *testing.T) {
	target := Target{DeviceName: "data"}
	rendered := agentConfigYAML(target)
	want := "discovery_interval: " + defaultDiscoveryInterval.String()
	if !strings.Contains(rendered, want) {
		t.Fatalf("agentConfigYAML must render %q, got:\n%s", want, rendered)
	}
	if defaultDiscoveryInterval != 5*time.Minute {
		t.Fatalf("defaultDiscoveryInterval = %s, want unchanged at 5m — every existing scenario's maxDiscoveryAge margin depends on this value", defaultDiscoveryInterval)
	}
}

// TestRegression_Config_GeneratedYAMLPassesLoadAndValidate runs every
// DisksSpec shape a scenario in this package actually renders through the
// real internal/config.LoadAndValidate — the same call the agent itself
// makes at startup (cmd/mws-iops-autoscaler/main.go). A config rejected
// there exits before the process ever reaches discovery, which would fail a
// scenario relying on it for a reason unrelated to what it's testing —
// discovery_test.go's disks.exclude cases in particular have never gone
// through the real validator before this test existed.
func TestRegression_Config_GeneratedYAMLPassesLoadAndValidate(t *testing.T) {
	cases := map[string]struct {
		disks             DisksSpec
		discoveryInterval time.Duration
	}{
		// InstallAgent's actual default: agentConfigYAML at
		// defaultDiscoveryInterval, the config every existing scenario
		// ships — not a stand-in built from the *WithDisks primitives.
		"selected (InstallAgent's default)":              {SelectedDisksSpec("mws-data"), defaultDiscoveryInterval},
		"auto-discovery, no exclude (A1's shape)":        {AutoDiscoveryDisksSpec(), discoveryScenarioInterval},
		"auto-discovery, unresolved exclude name (E6's)": {AutoDiscoveryDisksSpec("mws-e2e-exclude-ghost"), discoveryScenarioInterval},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(agentConfigYAMLWithDisks(tc.disks, tc.discoveryInterval)), 0o600); err != nil {
				t.Fatalf("write generated config: %v", err)
			}
			if _, err := config.LoadAndValidate(path); err != nil {
				t.Fatalf("generated config failed LoadAndValidate: %v", err)
			}
		})
	}
}

func TestRegression_Load_UsesAsyncEngineSoIodepthTakesEffect(t *testing.T) {
	for _, bs := range []string{"4k", "64k"} {
		cmd := deepLoadCmd("/dev/disk/by-id/mws-datadisk", bs)
		if !strings.Contains(cmd, "--ioengine=libaio") || !strings.Contains(cmd, "--bs="+bs+" ") {
			t.Errorf("load command %q must select an async engine at block size %s; fio's default ignores --iodepth", cmd, bs)
		}
	}
}

// TestRegression_VMCap_RecordMatchesOnlyPoolBoundCap guards the VM cap
// scenario's predicate: a disk pinned at policy.max_iops reports the same
// vm_cap_reached action, but with VM headroom still above its current limit.
func TestRegression_VMCap_RecordMatchesOnlyPoolBoundCap(t *testing.T) {
	cases := map[string]struct {
		line string
		want bool
	}{
		"pool exhausted":                    {`{"disk":"d1","action":"vm_cap_reached","reason":"vm_headroom_insufficient","current_iops":3200,"vm_headroom_iops":2900}`, true},
		"headroom equals current":           {`{"disk":"d1","action":"vm_cap_reached","reason":"vm_headroom_insufficient","current_iops":3200,"vm_headroom_iops":3200}`, true},
		"pinned at max_iops, pool has room": {`{"disk":"d1","action":"vm_cap_reached","reason":"vm_headroom_insufficient","current_iops":10000,"vm_headroom_iops":12000}`, false},
		"zero headroom (hand-built record)": {`{"disk":"d1","action":"vm_cap_reached","reason":"vm_headroom_insufficient","current_iops":3200,"vm_headroom_iops":0}`, false},
		"scale-up":                          {`{"disk":"d1","action":"scale_up","reason":"saturated","current_iops":1000,"vm_headroom_iops":900}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var rec journalRecord
			if err := json.Unmarshal([]byte(tc.line), &rec); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if rec.Disk != "d1" {
				t.Fatalf("Disk = %q, want d1 — the scenario maps the record back to a target by it", rec.Disk)
			}
			if got := isVMPoolCapRecord(rec); got != tc.want {
				t.Fatalf("isVMPoolCapRecord = %v, want %v", got, tc.want)
			}
		})
	}
}
