//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
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

func TestRegression_ResetConfirmsBaselineAcrossStaleReads(t *testing.T) {
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

func TestRegression_ResetFailsWhenOrphanRaces(t *testing.T) {
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

func TestRegression_WaitForScaleUpRejectsAlreadyElevated(t *testing.T) {
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

func TestRegression_WaitForScaleUpRejectsEarlyElevation(t *testing.T) {
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

func TestRegression_WaitForScaleUpAcceptsObservedBaselineThenRise(t *testing.T) {
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

func TestRegression_WaitForScaleUpTimesOutAtBaseline(t *testing.T) {
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

func TestRegression_StopStaleAgentVerifiesAbsence(t *testing.T) {
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

func TestRegression_StopStaleAgentSucceedsOnConvergedAbsence(t *testing.T) {
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

func TestRegression_StopStaleAgentEscalatesToKill(t *testing.T) {
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

func TestRegression_StopStaleAgentFailsClosedOnBrokenAbsenceCheck(t *testing.T) {
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
