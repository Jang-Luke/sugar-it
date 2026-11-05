---
title: Windows self-update traps
author: Lujke555
date: 2026-11-05
branch: main
cost: { minutes: 187, tokens: 2241315 }
id: l-1abea17a
scope: repo
status: active
src: agent
touches:
  - internal/detach/detach_windows.go
  - ".github/workflows/release.yml"
---

## Decided
- The silent installer gets `/S /D=<app folder>` (`InstallerParams`), because without `/D` NSIS uses its default folder and relaunching an app from another folder starts the old version, so the banner returns forever.
- The helper restarts a stopped daemon before the app (`RunHelper`), because an app started first hosts its own daemon and the service is then refused.
- `detach.StartGUI` exists because `HideWindow` passes SW_HIDE in STARTUPINFO, which Windows applies to a program's first ShowWindow, so a relaunched app would stay hidden.
## Watch out
- In a `workflow_call` run from auto-release, `GITHUB_REF_NAME` is main, so `release.yml` jobs use `TAG`. v0.1.0 to v0.1.2 are pre-releases, so `/releases/latest` is 404 until the first full release; the cause of that marking was not found, so `release.yml` now fails if a plain version is not a full release.
