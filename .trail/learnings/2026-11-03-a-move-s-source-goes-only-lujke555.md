---
title: "A move's source goes only after the commit and its log entry are durable"
author: Lujke555
date: 2026-11-03
branch: feat/sugar-it
cost: { minutes: 245, tokens: 1000055 }
id: l-efe79a3a
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
- Order for a cross-share move: commit the part, fsync the destination directory, fsync `done.log` with its entry, then remove the source. `openDoneLog` syncs a non-empty log before any entry is trusted. Same-share item renames write a durable record in `renames.log` first.
- The per-job taken-name set that blocks case and NFC/NFD collisions is seeded from `done.log`, so a lost entry whose source is gone silently overwrote the only copy under `overwrite`.
## Tried and ruled out
- An unsynced `done.log`: truncating it after a source removal lost the first file while the job reported `done`.
## Watch out
- Directory fsync treats EINVAL and ENOTSUP as a no-op, so network and FAT mounts lack this guarantee. Cost is about 40 ms per file on ext4. In the code: `internal/transfer/manager.go`, `internal/transfer/store.go`, `internal/shares/part.go`.
