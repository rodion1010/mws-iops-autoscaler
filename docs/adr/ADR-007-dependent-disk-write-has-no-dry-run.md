# ADR-007: The dependent-disk write path has no server-side dry-run

**Status:** Accepted
**Date:** 2026-08-29
**Deciders:** Product Lead (NBS)
**Supersedes:** US-013's assumption that `ValidateOnly=true` applies uniformly to "each decision"

## Context

US-013 (Dry-run mode) states: "Each decision is sent with `ValidateOnly=true`, so the platform runs
its real validation (bounds, the 100-IOPS step, permissions) without mutating — a dry run that
catches genuine rejections rather than only printing intent." This was written before either write
path had been implemented against the real SDK, on the assumption that `ValidateOnly` is a property
of "writing a decision," not of which endpoint that write happens to use.

Implementing M1's `internal/compute` package against `go.mws.cloud/go-sdk@v0.19.0` found this is not
the case. The generated request structs differ:

```go
// service/compute/client/Disk.gen.go
type UpdateDiskRequest struct {
    ...
    ValidateOnly   *bool   // query: "validateOnly"
    IdempotencyKey *string
    ...
}

// service/compute/client/VirtualMachine.gen.go
type UpdateVirtualMachineRequest struct {
    ...
    IdempotencyKey *string
    // no ValidateOnly field — grepped the whole file, zero hits
}
```

The SDK is generated directly from MWS's own OpenAPI spec (CLAUDE.md, "API" §), so an absent field
here reflects an absent query parameter on that operation in the spec itself, not an SDK authoring
gap. This has not been confirmed by a live call in this session (no credentials available) — treat it
as strong evidence from a primary source, not yet a `[V]` measured fact in CLAUDE.md's terms.

## Decision

**Dry-run behaves differently by disk class, and US-013 now says so:**

- **Independent disks** — unchanged. `UpdateDisk` is called with `ValidateOnly=true`; the platform
  runs its real validation (bounds, the 100-IOPS step, permissions) and the dry-run report reflects a
  genuine server verdict.
- **Dependent disks** — `UpdateVirtualMachine` has no equivalent. A "dry run" here can only report the
  computed decision (target IOPS, delta, the reasoning behind it) plus whatever the agent can check
  client-side (range, step, the VM IOPS pool guard — all of which the agent already enforces itself
  regardless of dry-run, per `docs/configuration.md` §7). It cannot confirm permissions or an etag
  precondition without actually writing. The dry-run report must say so explicitly per disk — printed
  intent for a dependent disk is not the same claim as a server-confirmed verdict for an independent
  one, and US-013's own stated goal ("rather than only printing intent") means the difference has to
  be visible to the operator, not silently smoothed over.

## Options Considered

### Option A: Replicate every server check client-side for dependent disks

Would close the gap for range/step, which the agent already validates anyway. Does not close it for
permissions or etag/concurrency — those genuinely require a round-trip. Rejected as solving only part
of the problem while implying (via a uniform "validated" label) that the rest was covered too.

### Option B: Chosen — report the asymmetry honestly

No new capability invented; the dry-run report is simply accurate about what was and wasn't confirmed
by the platform, per disk.

### Option C: Send `?validateOnly=true` on the VM endpoint anyway, hoping it is accepted and ignored

Rejected, untested: CLAUDE.md's own measured section already shows this platform returning confusing
errors for unsupported request shapes elsewhere (the misleading `503` for a wrong credential type on
metrics, rather than a clean `403`). Sending an undeclared query parameter risks a similarly opaque
failure instead of a clean no-op, and this has not been tested live to know which.

## Consequences

**Easier**
- No new endpoint behavior to build or maintain — the two write paths (write.go, already implemented)
  are unchanged by this decision.

**Harder**
- A dependent-disk dry run is weaker evidence than an independent-disk one. An operator relying on
  `--dry-run` to fully vet a config before enabling live writes gets a partial answer for VM-owned
  disks specifically.
- The dry-run report (US-014, observability) needs a field or label distinguishing "server-confirmed"
  from "computed, not server-checked" per decision — not just a flat pass/fail.

**Revisit when**
- A live call against the test environment confirms (or overturns) that `UpdateVirtualMachine`
  genuinely has no dry-run path — promote this from "strong evidence" to `[V]` in CLAUDE.md either
  way.
- If the platform ever adds `validateOnly` to the VM update operation, this ADR's asymmetry goes away
  and US-013 can drop the per-class distinction.
