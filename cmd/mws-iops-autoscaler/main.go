// Command mws-iops-autoscaler is the MWS IOPS Autoscaler agent (local mode,
// bidirectional scaling).
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
	dryRun := fs.Bool("dry-run", false, "discover, sample and decide, but apply no change")
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

// runAgent builds every pipeline stage and runs the control loop until an
// interrupt or SIGTERM.
func runAgent(configPath string, dryRun bool, stateDir, journalPath, listenAddr string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.Mode != "local" {
		// Validation already rejects other modes; this states runAgent's own scope.
		return fmt.Errorf("mode %q not supported yet", cfg.Mode)
	}
	if cfg.Runtime.Metrics.Source == "platform" {
		return fmt.Errorf("runtime.metrics.source: \"platform\" is not implemented yet")
	}
	resolved := config.Resolve(cfg)

	identity, err := compute.ResolveSelfIdentity(ctx)
	if err != nil {
		return fmt.Errorf("resolve own VM identity: %w", err)
	}
	vmName := identity.VMName

	// At least 30s, never the SDK's 5s default: endpoint discovery alone
	// can take 15s.
	sdk, err := mws.Load(ctx, mws.WithDefaultProject(identity.Project), mws.WithDefaultZone(identity.Zone), mws.WithTimeout(30*time.Second))
	if err != nil {
		return fmt.Errorf("load MWS SDK credentials: %w", err)
	}
	client, err := compute.NewSDKClient(ctx, sdk, identity.Project)
	if err != nil {
		return fmt.Errorf("build Compute API client: %w", err)
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

	// Freshness bounds are multiples of the interval they bound, so ordinary
	// jitter never trips them. 3x tolerates two missed cycles before failing
	// closed; 2x on the gap bound tolerates one missed cycle inside a window.
	// The multipliers are not yet calibrated.
	maxSampleAge := 3 * resolved.Runtime.CollectionInterval
	maxSampleGap := 2 * resolved.Runtime.CollectionInterval
	maxDiscoveryAge := 3 * resolved.Runtime.DiscoveryInterval

	agent := &Agent{
		cfg:               cfg,
		resolved:          resolved,
		vmName:            vmName,
		dryRun:            dryRun,
		maxDiscoveryAge:   maxDiscoveryAge,
		maxSampleAge:      maxSampleAge,
		maxSampleGap:      maxSampleGap,
		guestPathVerifier: source.VerifyPath,
		client:            client,
		sampler:           sampler,
		analyzer:          analyzer.NewWindowAnalyzer(),
		decider:           decider.NewPolicyDecider(),
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
