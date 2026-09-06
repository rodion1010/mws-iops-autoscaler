# Agent instructions

Read [CLAUDE.md](CLAUDE.md) in the repo root before changing anything — it is canonical for this
repository, and this file is only a pointer to it.

CLAUDE.md defines:

- the documentation map and the single canonical home of each fact (PRD, design, configuration,
  platform reference, platform dependencies, development, testing, CI/CD, TODO, ADR policy);
- change discipline (scope, fail-closed invariants, functional/doc-only commit separation);
- code-comment policy;
- verification requirements (gofmt, `go test`/`go vet`/`go test -race`, regression tests for bug
  fixes);
- credential rules (never commit keys, tokens, or live environment identifiers).

If this file and CLAUDE.md ever disagree, CLAUDE.md wins.
