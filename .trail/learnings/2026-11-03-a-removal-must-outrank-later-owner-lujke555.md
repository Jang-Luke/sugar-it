---
title: "A removal must outrank later owner changes, so membership records carry an epoch"
author: Lujke555
date: 2026-11-03
branch: feat/sugar-it
cost: { minutes: 245, tokens: 1000055 }
id: l-88706f47
scope: repo
status: active
src: agent
touches:
  - README.md
  - docs/superpowers/specs/2026-11-02-sugar-it-design.md
  - internal/daemon/daemon.go
  - internal/daemon/daemon_test.go
  - internal/cli/cli_test.go
  - internal/cli/fs.go
  - desktop/frontend/src/lib/api.ts
  - desktop/frontend/src/lib/transfers.test.ts
  - desktop/frontend/src/views/Transfers.svelte
  - internal/cli/jobs.go
---

## Decided
- `mesh.Member` has `Epoch`, and `mesh.Merge` orders by (Epoch, Generation, existing tie rules). Only removal and re-pairing a removed record raise Epoch. A Generation-only rule let a removed host return with no invite after two renames, because owner bumps and tombstones shared one counter.
- Re-pairing an active host does not raise Epoch, so active epochs stay even and tombstones odd, and a tombstone never ties an active record.
## Tried and ruled out
- Blocking revival by dropping the supersede step: the revival still reproduced; the shared counter was the cause.
## Watch out
- A re-pairing newer than what the remover has seen beats that removal; repeat the removal after views converge. In the code: `internal/mesh/member.go`, `internal/node/pair.go`, `internal/node/sync.go`, spec section 4.
