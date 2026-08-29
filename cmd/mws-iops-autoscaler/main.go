// Command mws-iops-autoscaler is the MWS IOPS Autoscaler agent (Phase 1: local
// mode, scale-up — docs/prd-mws-iops-autoscaler.md §6).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rodion1010/mws-iops-autoscaler/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("mws-iops-autoscaler", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the config file (required)")
	validateOnly := fs.Bool("validate-config", false, "parse and validate the config, then exit")
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

	// The control loop (discovery, sampling, decide, actuate) lands in later
	// milestones (design.md §9.1, M1-M6); Phase 1's first slice is the config
	// path only.
	fmt.Fprintln(os.Stderr, "mws-iops-autoscaler: the control loop is not implemented yet; run with -validate-config")
	return 1
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
