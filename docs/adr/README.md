# Architecture decision records

`docs/design.md` is the canonical description of the current architecture. This directory contains
only exceptional decision rationale that still meets the ADR criteria in `CLAUDE.md`; it must not
mirror implementation details, issue history, progress checklists or the current design.

There are currently no active ADR files. ADR-001 through ADR-019 were consolidated into
[`docs/design.md`](../design.md) on 2026-08-30. Their full historical text remains available in the
[pre-consolidation snapshot](https://github.com/rodion1010/mws-iops-autoscaler/tree/5b89e8efc4ffe480cb6f44230433eaef700ec861/docs/adr).

Add a new ADR only when all repository criteria are met. When an accepted decision changes the
current system, update `docs/design.md` in the same change; do not copy the design into the ADR.
