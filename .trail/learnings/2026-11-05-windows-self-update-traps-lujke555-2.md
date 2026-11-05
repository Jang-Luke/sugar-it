---
title: Windows self-update traps
author: Lujke555
date: 2026-11-05
branch: main
cost: { minutes: 196, tokens: 2259999 }
id: l-de45201d
scope: repo
status: active
replaces: l-1abea17a
src: agent
touches:
  - internal/update/helper.go
  - internal/detach/detach_windows.go
  - ".github/workflows/release.yml"
---

## Decided
- The silent installer gets `/S /D=<app folder>` (`InstallerParams`), because without `/D` NSIS uses its default folder and relaunching an app from another folder starts the old version, so the banner returns forever.
- After installing, `RunHelper` compares the app's SHA-256 with its own exe, a byte copy of the old program, because silent NSIS skips a locked file and still exits 0.
- The helper restarts a stopped daemon before the app, because an app started first hosts its own daemon and the service is then refused. It skips the relaunch if the old app outlived the wait, because the single-instance branch would just exit.
- `detach.StartGUI` exists because `HideWindow` passes SW_HIDE in STARTUPINFO, which Windows applies to a program's first ShowWindow, so a relaunched app would stay hidden.
## Watch out
- In a `workflow_call` run from auto-release, `GITHUB_REF_NAME` is main, so `release.yml` jobs use `TAG`.
- v0.1.0 to v0.1.2 are pre-releases because the controller marked them by hand with `gh release edit`, not because of GoReleaser. `/releases/latest` is therefore 404 until the first full release. `release.yml` also fails if a plain version is not a full release.
