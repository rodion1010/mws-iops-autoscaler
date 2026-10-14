//go:build !e2e_scaledown

package main

// allowScaleDown is Phase 1/2's production value (PRD §6): scale-up only.
// This is the one place that capability gets decided (issue #14) — Phase 3
// flips it here and nowhere else, and only by editing this file's build
// constraint away, never by a runtime flag or config field (docs/design.md:
// "a constructor-only capability, not a per-call argument ... must not
// depend on any user-tunable threshold"). The e2e_scaledown-tagged sibling
// file in this package defines the same identifier as true, for a
// separately-built test binary only (docs/testing.md) — every other build,
// including every other e2e scenario's binary, links this file and gets
// false. Both the Decider and the Agent's own defense-in-depth check in
// agent.go read this single value, not two independently maintained copies.
const allowScaleDown = false
