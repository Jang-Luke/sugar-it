---
title: "WebDAV overwrite must not reach x/net's delete-first path"
author: Lujke555
date: 2026-11-03
branch: feat/sugar-it
cost: { minutes: 75, tokens: 440744 }
id: l-268b4d2c
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
- `golang.org/x/net/webdav` calls `RemoveAll(dst)` before `Rename` or copy, so a MOVE with a missing source deleted the destination. The preflight in `internal/davfs/davfs.go` stats the source fresh and returns 404, and refuses MOVE or COPY onto a related path. File onto file is staged through `Put` under `fileSystem`, then the source goes; directory overwrite returns 412.
## Tried and ruled out
- Staging in the HTTP handler: x/net's lock check is unexported, so that would overwrite locked files.
## Watch out
- A staged MOVE keeps mtime but not permission bits; a 0600 source can end 0664.
