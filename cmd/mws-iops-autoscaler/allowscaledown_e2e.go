//go:build e2e_scaledown

package main

// allowScaleDown is true only in a binary built with -tags e2e_scaledown —
// a separate build used exclusively by the "scale-down after quiet" and "no
// flapping" e2e scenarios (docs/testing.md, design.md §8.3), never by the
// production build or by any other e2e scenario's binary (see the
// !e2e_scaledown-tagged sibling file for the production value and the
// rationale for keeping this a compile-time, not a runtime, decision).
const allowScaleDown = true
