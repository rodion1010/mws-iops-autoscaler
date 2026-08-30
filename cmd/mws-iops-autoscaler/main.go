// Command mws-iops-autoscaler is the MWS IOPS Autoscaler agent (Phase 1: local
// mode, scale-up — docs/prd-mws-iops-autoscaler.md §6).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.mws.cloud/go-sdk/mws"

	"github.com/rodion1010/mws-iops-autoscaler/internal/actuator"
	"github.com/rodion1010/mws-iops-autoscaler/internal/analyzer"
	"github.com/rodion1010/mws-iops-autoscaler/internal/compute"
	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
	"github.com/rodion1010/mws-iops-autoscaler/internal/decider"
	"github.com/rodion1010/mws-iops-autoscaler/internal/health"
	"github.com/rodion1010/mws-iops-autoscaler/internal/journal"
	"github.com/rodion1010/mws-iops-autoscaler/internal/metrics"
	"github.com/rodion1010/mws-iops-autoscaler/internal/state"
	"github.com/rodion1010/mws-iops-autoscaler/internal/telemetry"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("mws-iops-autoscaler", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the config file (required)")
	validateOnly := fs.Bool("validate-config", false, "parse and validate the config, then exit")
	dryRun := fs.Bool("dry-run", false, "discover, sample and decide, but apply no change (US-013)")
	stateDir := fs.String("state-dir", "/var/lib/mws-iops-autoscaler", "directory for the durable 24h change budget")
	journalPath := fs.String("journal", "", "decision journal output path (default: stdout)")
	listenAddr := fs.String("listen", ":9435", "address for /metrics, /healthz, /readyz")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "mws-iops-autoscaler: -config is required")
		return 2
	}

	if *validateOnly {
		return validateConfig(*configPath)
	}

	if err := runAgent(*configPath, *dryRun, *stateDir, *journalPath, *listenAddr); err != nil {
		fmt.Fprintf(os.Stderr, "mws-iops-autoscaler: %v\n", err)
		return 1
	}
	return 0
}

// runAgent builds every pipeline stage (design.md §3.1) and runs the
// control loop until an interrupt or SIGTERM. It stops short of two
// things deliberately: no systemd/.deb packaging (a deployment decision,
// not a code one — matches this project's own TODO.md rule that CI/CD and
// deployment infrastructure need an explicit decision before building) and
// no attempt to validate live against the platform (that's a step a human
// takes with -dry-run against a real environment, not something this
// function does on its own).
func runAgent(configPath string, dryRun bool, stateDir, journalPath, listenAddr string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.Mode != "local" {
		// Already rejected by Load/Validate today (fleet mode ships in
		// Phase 4), but the check stays here too as a direct statement of
		// this function's own scope, not a delegated one.
		return fmt.Errorf("mode %q not supported yet", cfg.Mode)
	}
	if cfg.Runtime.Metrics.Source == "platform" {
		return fmt.Errorf("runtime.metrics.source: \"platform\" requires PlatformSource, which ships in M7 — not implemented yet")
	}
	resolved := config.Resolve(cfg)

	project := os.Getenv("MWS_PROJECT")
	if project == "" {
		return fmt.Errorf("MWS_PROJECT must be set (go.mws.cloud/go-sdk's own project env var)")
	}

	// NFR-8: at least 30s, never the SDK's 5s default — endpoint discovery
	// alone was measured at 15s.
	sdk, err := mws.Load(ctx, mws.WithDefaultProject(project), mws.WithTimeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("load MWS SDK credentials: %w", err)
	}
	client, err := compute.NewSDKClient(ctx, sdk, project)
	if err != nil {
		return fmt.Errorf("build Compute API client: %w", err)
	}

	vmName, err := compute.SelfVMName(ctx)
	if err != nil {
		return fmt.Errorf("resolve own VM identity (US-002): %w", err)
	}

	source := metrics.NewGuestSource()
	sampler, err := metrics.NewSampler(source, resolved.SamplerCapacity())
	if err != nil {
		return fmt.Errorf("build sampler: %w", err)
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create state directory %s: %w", stateDir, err)
	}
	budget, err := state.LoadBudget(filepath.Join(stateDir, "budget.jsonl"))
	if err != nil {
		return fmt.Errorf("load 24h change budget: %w", err)
	}
	tracker := state.NewTracker(budget)

	var journalOut = os.Stdout
	if journalPath != "" {
		f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open journal file %s: %w", journalPath, err)
		}
		defer f.Close()
		journalOut = f
	}

	reg := telemetry.NewRegistry()
	readiness := &health.Readiness{}
	readiness.SetConfigValid(true) // LoadAndValidate above already proved this

	// Phase 1 (PRD §6) is local mode, scale-up only. This is the one place
	// that capability gets decided (issue #14) — Phase 3 flips it here and
	// nowhere else; both the Decider and the Agent's own defense-in-depth
	// check read the same value, not two independently maintained copies.
	const allowScaleDown = false

	// Freshness bounds (issue #15), derived from the same collection/
	// discovery cadence the pipeline already runs on rather than hardcoded
	// or independently configured: a threshold that isn't a multiple of
	// the interval it's bounding would either trip on ordinary jitter (too
	// tight) or defeat its own purpose (too loose). 3x tolerates two fully
	// missed cycles before failing closed; 2x on the gap bound means one
	// missed cycle inside a window is still tolerated, two is not.
	// Calibration-pending, same as every other constant of this shape in
	// this codebase (design.md §8.5).
	maxSampleAge := 3 * resolved.Runtime.CollectionInterval
	maxSampleGap := 2 * resolved.Runtime.CollectionInterval
	maxDiscoveryAge := 3 * resolved.Runtime.DiscoveryInterval

	agent := &Agent{
		cfg:               cfg,
		resolved:          resolved,
		vmName:            vmName,
		dryRun:            dryRun,
		allowScaleDown:    allowScaleDown,
		maxDiscoveryAge:   maxDiscoveryAge,
		maxSampleAge:      maxSampleAge,
		maxSampleGap:      maxSampleGap,
		guestPathVerifier: source.VerifyPath,
		client:            client,
		sampler:           sampler,
		analyzer:          analyzer.NewWindowAnalyzer(),
		decider:           decider.NewPolicyDecider(allowScaleDown),
		actuator:          actuator.NewComputeActuator(client),
		tracker:           tracker,
		telemetry:         reg,
		journal:           journal.NewWriter(journalOut),
		readiness:         readiness,
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.Handle("/healthz", health.HealthzHandler())
	mux.Handle("/readyz", readiness.ReadyzHandler())
	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server: %v", err)
		}
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if dryRun {
		log.Printf("mws-iops-autoscaler: starting in -dry-run mode for VM %q — no writes will be applied", vmName)
	} else {
		log.Printf("mws-iops-autoscaler: starting for VM %q", vmName)
	}
	return agent.Run(ctx)
}

func validateConfig(path string) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid: %v\n", err)
		return 1
	}
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid:\n")
		for _, e := range flattenJoined(err) {
			fmt.Fprintf(os.Stderr, "  - %v\n", e)
		}
		return 1
	}

	resolved := config.Resolve(cfg)

	fmt.Printf("valid: mode=%s limits=[%d,%d] disks.mode=%s\n",
		cfg.Mode, resolved.VMPolicy.MinIOPS, resolved.VMPolicy.MaxIOPS, cfg.Disks.Mode)
	return 0
}

// flattenJoined splits an errors.Join tree into its leaves for one-per-line
// printing. The standard library does not expose this directly.
func flattenJoined(err error) []error {
	type unwrapper interface{ Unwrap() []error }
	if u, ok := err.(unwrapper); ok {
		var out []error
		for _, e := range u.Unwrap() {
			out = append(out, flattenJoined(e)...)
		}
		return out
	}
	return []error{err}
}
