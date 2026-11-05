# sugar-it 후속 항목

SDD 작업 기록에서 옮긴 목록이다. 작업 기록은 브랜치 완료 후 지운다. 리뷰에서 나온 원문이라 영어로 두었다. 일부 항목은 이후 수정으로 이미 해결됐을 수 있으니, 고치기 전에 코드를 확인한다.

## 내린 판단 (Rulings)

- R1 Ruling: rename everywhere as sugar-it: binary, cmd/sugar-it, sugar-it-desktop, config dir, `.sugar-it-part`, invite prefix `sugar-it1:`, WebDAV user, service unit. Go module path stays `sugarintegration` (plan Global Constraints; it is the directory name) — cost if wrong: mechanical module rename.
- R2 Ruling: controller already ran `git init -b main`, committed docs and ignore files on main, and created feat/sugar-it; Task 1 skips `git init` and all work happens on this branch, in place, no worktree — user asked not to pause; session configured to work in place — cost if wrong: branch rename.
- R3 Ruling: Task 1 appends plan's ignore entries to the existing `.gitignore` instead of creating it; keep `.ignore` — both exist already — cost: none.
- R4 Ruling: commit steps stage only paths that exist (go.sum may not exist early); every commit message ends with the Co-Authored-By line — plan's literal `git add go.sum` fails when no deps — cost: none.
- R5 Ruling: `vfs.Clean` rejects the part dir name case-insensitively and in components split on both `/` and `\` — spec 5 says the path is unreachable through the API; on Windows/macOS `.SUGAR-IT-PART` or `.sugar-it-part\x` would otherwise reach it; plan says Clean does not reject backslashes in general and this keeps that — cost: Linux files literally named that way are unreachable.
- R6 Ruling: `CheckOverlap` falls back to lexical cleaning when EvalSymlinks fails (foreign-OS test paths, not-yet-existing paths); it compares case-insensitively for windows and darwin — plan's Windows test runs on Linux with paths that do not exist; spec 5 demands case-insensitive on both — cost: an unresolved symlink overlap is missed only for paths that do not exist, which AddShare rejects anyway.
- R7 Ruling: self Generation can lag remote views because Merge ignores remote self entries (rejoin sets gen = tombstone+1 on the inviter). Merge stays as specified, but Node tracks the highest generation seen for self in any incoming list (Pair response, sync) and every owner bump uses max(own, seen)+1 — otherwise a rejoined host's address or name change loses to its own older record, breaking spec 4 — cost: one extra field.
- R8 Ruling: crash window between CommitPart and done.log. On a recovered job, if the destination exists with same size and mtime as the source and the SHA-256 matches, count the file done (then delete the source if move) without applying the conflict policy. done.log is written after commit; for a move, a done.log entry whose source still exists has that source removed on resume — without this, a kill at that instant turns a correct transfer into `conflict`/`partial` or a `(1)` duplicate; Review Focus 1 — cost: a pre-existing identical file on a recovered job counts as transferred, harmless.
- R9 Ruling: peer names are local aliases, so Control converts names to member IDs before delegating; Spec.Src.Peer and Dst.Peer on the wire are always IDs; Node.SourceFS(selfID, share) returns the local FS — spec 4 says aliases are local, Options.Source takes peerID — cost: none.
- R10 Ruling: "Unavailable" in control.Discover is exported `control.ErrUnavailable`, built on `vfs.E(CodeInternal, ...)` and matched by errors.Is — Global Constraints allow only spec codes — cost: frontend shows generic text plus message.
- R11 Ruling: Daemon binds Control first, then Peer, then WebDAV, so a second daemon fails on Control as the plan's test requires and touches nothing. TestSecondDaemonRefused reuses the first daemon's real control address. Control tests that need daemons use external package `control_test` to avoid an import cycle — cost: none.
- R12 Ruling: `sugar-it daemon` gets `--listen`, `--control`, `--dav`, `--advertise` flags in Task 9 — e2e runs three local processes and DetectAddrs excludes loopback, so otherwise they cannot find each other — cost: small extra CLI surface.
- R13 Ruling: Task 11 also exports `mount.Run(Plan)` and `service.Install(Unit)`/`service.Uninstall(Unit)`, used by CLI and Task 12 desktop — both consumers need them and duplication is worse — cost: none.
- R14 Ruling: `canceled` final state exits 1 from waiting `cp`/`mv`; RemovePeer on self returns invalid_request — spec silent; a tombstone on self would be ignored by Merge anyway — cost: trivial to change.
- R15 Ruling: GTK and WebKit dev packages are already installed (dpkg verified), so Task 12 does not ask the user; this host has no display, so Step 6 is reported as unverified and handed to the user; docker exists for Task 13 — cost: none.
- R16 Ruling: Node gets unexported interval fields settable from same-package tests rather than a New signature change — plan requires injection and fixes the signature — cost: none.
- Task 3: Ruling: reclassify ControlToken crash window from Minor to Important and fix in round 1 — an empty token file makes every later daemon start fail forever, and it is on the same helper — cost: one small extra change
- Task 4: Ruling: progress callback gets a negative delta on restart or part reset, resumed bytes are reported once as a positive delta at start — Task 5 aggregates progress and would exceed 100% otherwise; load-bearing so fixed now rather than deferred — cost: small contract addition
- Task 5: Ruling: add `Manager.ActivePart(key string) bool` covering all non-terminal jobs and `failed` jobs that can still Retry, for Task 8's GCParts — spec 5 says only parts with no active job are collected and brief's interface offers no query — cost: one small method
- Task 5: Ruling: accept implementer's stricter-than-R8 source removal (ETag and destination re-checked; otherwise changed and source stays) and refusal of case-only same-share rename on windows/darwin — both only ever keep data — cost: a case-only rename fails instead of succeeding on those OSes
- Task 5: Ruling: CommitPart fsyncs the destination parent directory after its rename (no-op on Windows), and Put does the same — spec 7 deletes a move's source only after commit, and a non-durable rename is not committed; resolves Task 2 deferred minor on parent dir fsync — cost: one more fsync per file, Task 11 e2e timeouts need minutes
- Task 5: Ruling: directory fsync treats EINVAL and ENOTSUP as no-op — network and FAT-style mounts reject directory fsync and every commit would otherwise fail; those filesystems simply lack the guarantee — cost: weaker durability on them only
- Task 6: Ruling: server validates every component of a destination path for PUT, mkdir, rename with shares.ValidName(runtime.GOOS, c), returning invalid_name — closes Task 2 open cross-task check; spec 7 demands the code and LocalFS never checks — cost: none
- Task 6: Ruling: custom download handler replaces the plan's http.ServeContent — If-Range etag mismatch must return `changed` per spec 6 and ServeContent sends the full body — cost: Range handling is ours to maintain
- Task 7: Ruling: peerapi.Backend is implemented by adapter n.Backend() because Backend.Shares() clashes with the plan's Node.Shares(ctx, peer) — the plan says Node implements Backend directly but both signatures are mandated — cost: later tasks call n.Backend()
- Task 7: Ruling: add Member.Epoch and order records by (Epoch, Generation, then existing tie rules). Only removal and re-pairing raise Epoch; owner bumps and supersede raise only Generation, so they can never pass a tombstone. A host adopts a higher Epoch for itself only from an active record (Pair response or gossip), never from a tombstone. Spec 4 updated to match — spec 4 says re-registration only by new pairing, and its Generation-only merge rule cannot express that; removal is the only revocation in the system — cost: wire and config field added, mesh.Merge changed
- Task 7: Ruling: keep implementer's supersede, bounded to the same Epoch — with Epoch it can no longer undo a removal and it covers restart losing seen generation — cost: cloned identity bump loop remains, see minor
- Task 7: Ruling: Pair raises Epoch only when the existing record is Removed; active epochs stay even and tombstones odd, so a tombstone never ties an active record — narrows the spec's re-pairing wording, re-reviewer tested it in a copy — cost: none known
- Task 7: Ruling: accept and document in spec 4 that a re-pairing newer than what the remover has seen beats that removal; the operator repeats the removal once views converge — the single-owner trust model has no ordering between hosts' views and only the remover's own stale view is affected — cost: a removal issued from a badly lagging host may need repeating
- Task 7: Ruling: accept that a removed host unaware of its removal can still issue an invite; a member's operator running peer add on it revives it — both an invite and an operator action are involved, matching "any member can invite" in spec 3; documented in spec 4 — cost: an operator can re-admit a removed host by mistake
- Task 8: Ruling: take an OS file lock on <dir>/daemon.lock (flock on Unix, LockFileEx on Windows) before touching jobs or GC; a second daemon fails on the lock, superseding plan's "fails on Control port" and R11's test — a port conflict cannot detect different addresses and a lock survives kill -9 without stale files — cost: lock may not work on some network config dirs
- Task 9: Ruling: reclassify `--advertise` written before the lock from Minor to fix-now — a refused second daemon must not change a live daemon's config — pass through daemon.Options — cost: one Options field
- Task 10: Ruling: bump golang.org/x/net to current v0.62.0 — implementer used the module cache's v0.42.0, this is a network-facing library — cost: none expected
- Task 10: Ruling: related-path guard always uses the darwin key (case-fold plus NFC) because the destination peer's OS is unknown — refusing only ever keeps data — cost: on Linux a MOVE or COPY between paths differing only by case or Unicode form is refused over WebDAV; the app still does it
- Task 12: Ruling: reclassify dialog-reads-live-selection from Minor to fix-now — same class as the stale selection: Rename throws and Delete silently loses names when a refresh lands mid-dialog — cost: small
- Task 12: Ruling: fix the reported Daemon.Shutdown random context.Canceled in this round with a regression test — Task 8 defect reported by this implementer; desktop shutdown path depends on it — cost: small
- Final: Ruling: WebDAV overwrite — preflight stats the source fresh and returns 404 if missing; file onto file is staged by Put with Overwrite then source removal for MOVE, never deleting first; any overwrite involving a directory with an existing destination is refused 412 — x/net deletes the target before the replacement exists, and spec 7 forbids loss; atomic-save MOVE file onto file must keep working — cost: replacing a directory via WebDAV MOVE or COPY is refused; the app still does it
- Final: Ruling: removal cancels the removed caller's in-flight peer API requests through a per-caller context, including download, upload, and hash — removal is the only revocation — cost: small hook between node and peerapi
- Final: Ruling: on removal, jobs whose source is the removed host stop as failed with peer_not_found, sources kept, retryable after re-pairing; jobs whose Origin is the removed host are canceled, parts removed, already moved files stay — a removed host must not keep directing work or reading — cost: legitimately requested work stops on removal
- Final: Ruling: desktop binary runs the daemon headless when its first argument is `daemon`, through cli.Execute, and service install targets the desktop binary when no CLI sits beside it — spec 1 one package, spec 10 service install, and Finder lacks PATH — cost: binary still links GTK on Linux
- Final: Ruling: add RenameSelf and service uninstall to the GUI — spec 1 says same features — cost: small
- Final: Ruling: control and DAV listeners refuse non-loopback addresses — spec 2 lists them loopback; the token would otherwise travel cleartext — cost: no remote control API, which the spec does not offer
- Final: Ruling: fix in this wave Minors 1 path leak, 2 WebDAV ValidName, 3 Shutdown select, 4 skipped_symlink recorded, 5 ls symlink display, 10 above; defer Important 6 and Minors 6-9, 11-13 and report them — the former are spec statements or one-liners — cost: listed items stay open

## 미검증

- Task 11: unverified: Linux gio mount (gvfs-backends absent; stdin password unverified), macOS osascript and diskutil name, Windows net use, registry, schtasks. To report to user
- Task 12: unverified: no display, GUI never rendered; windows net use prompt from GUI may not work; drag and drop unverified
- Task 13: unverified: GoReleaser snapshot denied twice by auto-mode classifier as external code, not worked around, user decision needed; docker dpkg check not run; workflows never ran (no remote)

## 미룬 항목

- Task 1: minor (deferred): vfs/path.go part-dir check misses Windows trailing dot or space variants (`.sugar-it-part.`); os.Root still confines
- Task 1: minor (deferred): vfs_test bad loop does not assert empty result; EntryType and Entry lack doc comments
- Task 2: minor (deferred): ELOOP maps to internal, should be invalid_path; escape detection matches a Go error string and tests assert only err != nil
- Task 2: minor (deferred): name.go accepts COM¹ and CONIN$, no length check so 300-byte names give internal not invalid_name
- Task 2: minor (deferred): Rename SameFile shortcut passes for hard links (a remains); needs EqualFold. Self-or-child check is case-sensitive
- Task 2: minor (deferred): GC ages .part and .etag independently; etag removal failure after rename reports failure for a committed file
- Task 2: minor (deferred): no parent dir fsync after rename; power loss after move source delete could lose the file. Decide in final review
- Task 2: minor (deferred): only root part dir hidden though Clean rejects it at any depth; disk_full mapping and write-path escapes untested
- Task 2: minor (deferred): noLinks check-then-use race (os.Root still confines); restoreWrite ignores chmod error; symlinked `.sugar-it-part` itself would be traversed by part methods; overlapByIdentity tested only with fake same
- Task 3: minor (deferred): mergeOne live tie with different names is order-dependent; deterministic tie-break would harden it
- Task 3: minor (deferred): ControlToken stale-file replace can race to different tokens; os.Link fails on FAT/exFAT config dirs with no fallback
- Task 4: minor (deferred): name check runs before source stat so a symlink named a:b reports invalid_name not skipped_symlink
- Task 4: minor (deferred): no test for destination appearing mid-transfer (fetch.go:89) or oversized part with matching etag (fetch.go:177); weak assertions fetch_test.go:228-231, 330-339, 353
- Task 4: minor (deferred): RemovePart errors dropped at fetch.go:91,232; small conflict window before CommitPart; Stat errors skip ctxOr; dead `final` value fetch.go:65; interface{io.ReadSeeker} fetch.go:260
- Task 5: minor (deferred): backoff can stay at RetryMin when a peer answers Stat then drops every stream (manager.go:906,923); no backoff growth test
- Task 5: minor (deferred): DoneBytes overcounts after a failed source removal (manager.go:960-969)
- Task 5: minor (deferred): Retry cannot recover a logged item whose destination vanished; stays partial with source as only copy (manager.go:850,974)
- Task 5: minor (deferred): done.log not fsynced, power cut can make a finished move report not_found (manager.go:582)
- Task 5: minor (deferred): plan.go:152 message repeats code; Retry returns error on failed save while already enqueued (manager.go:303); manager.go 1078 lines, jobRun could move to its own file; walk depth unbounded from a peer
- Task 5: minor (deferred): stale rename record makes vanished source report done total=0 (manager.go:638); ensureDir treats existing dir as durable (manager.go:912-916); o.Dir not synced after jobs/ creation
- Task 5: minor (deferred): failed fsync on Linux may mark pages clean so a later sync proves little; darwin EOPNOTSUPP not filtered; after a failed done.log sync, Retry ends failed internal, not partial
- Task 6: minor (deferred): RemoteFS.Put blocks on stalled source until ctx cancel, document it; SetAddrs resets pref even when unchanged (client.go:90), node should call it only on change
- Task 6: minor (deferred): quoted ETag vs raw If-Range compare (server.go:281,296); persistent server read error surfaces as peer_offline and retries forever (server.go:312-314); Open does not check offset+ContentLength==Size (remotefs.go:63-68)
- Task 6: minor (deferred): no committed test for non-member unknown paths; TestOfflineMapsToPeerOffline only exercises connection refused
- Task 6: minor (deferred): decode ignores SetReadDeadline error; TestHandlerPanicIsLogged leftover no-op; FS panic still reaches client as peer_offline (retry), log is the only trace
- Task 7: minor (deferred): generation wraps at MaxUint64 (node.go:157,277); cloned identity causes unbounded bump loop with fsync, no rate limit or log (sync.go:70-76)
- Task 7: minor (deferred): selfAddrs returns nil on ifaces error and propagates empty addrs (node.go:182-185); refreshSelf error dropped (node.go:424); localFS race can leak an open FS after RemoveShare (node.go:333-352,375-400)
- Task 7: minor (deferred): sanitize does not apply validID (sync.go:27-37); TestRunStopsAndRestarts last block cannot fail (node_test.go:798-802); doc comment names ensureActive for activePeer (sync.go:185); localFS mutex can let a hung mount delay other shares; remote FS() costs an extra GET /v1/shares
- Task 7: minor (deferred): rejoin through an inviter that has not heard of the removal is silently lost, fails closed (pair.go:85); stale comment member.go:105; 10s callTimeout with 5s per address lets two black-holed addresses use up the call so pref never advances
- Task 8: minor (deferred): Discover sends token to whatever answers at runtime.json address after a crash; Discover creates a token as a side effect; Shutdown second caller ignores its ctx; n.Close not called if Start fails late
- Task 8: minor (deferred): windows lock compile-checked only; config.Open and ControlToken run before the lock
- Task 9: minor (deferred): cobra and pflag marked indirect in go.mod (fix by go mod tidy in this round); isTTY true for /dev/null; jobs cancel, retry, watch, Ctrl-C and share add --read-only untested; jobs watch Ctrl-C exits 1
- Task 9: minor (deferred): CLI exit-1 path through Failed() untested at CLI level; fix tests lack recorded RED
- Task 10: minor (deferred): PUT to a hidden symlink name replaces the link invisibly; loopbackHost duplicated from control; GET drops list cache; LOCK on missing path in writable share creates empty file (x/net); x/net Overwrite T removes destination before Rename so a failed rename loses the overwritten target
- Task 10: minor (deferred): daemon test flake, TestLockReleasedAfterKill free-port race "address already in use"
- Task 10: minor (deferred): reqState first recorded status wins over later errors; Rename guard maps to 405 though unreachable
- Task 11: minor (deferred): TestKillSource exists(src) after kill is trivially true; kill-dest asserts neither resume-not-restart nor dst absence; TestRemovedPeerLosesAccess accepts any non-zero exit; e2e slow tests not skipped under -short
- Task 11: minor (deferred): systemdEscape ignores newlines; launchctl load -w not idempotent; Windows ONLOGON no /RU; Linux uninstall no daemon-reload; manual-test.md Linux wording; startHelper leak on fatal ERR
- Task 12: minor (deferred): job finishing within one poll never refreshes panes; startTransfer partial failure shows no count; 100,000 selected gives 100,000 jobs; raw English daemon message beside Korean text; StartError no Korean explanation; tree glyphs and English bindings error outside ko.ts; inviteHelp 10 minutes vs invite(0) default unconfirmed; Mount failure loses vfs code
- Task 12: minor (deferred): refresh blanks list and shows loading before refill; Svelte wiring in PaneView and Files checked by reading only
- Task 13: minor (deferred): TestGracefulShutdownKeepsJobs timed out once under load, then passed 5/5 alone
- Task 13: minor (deferred): release.yml v* tag filter matches vendor-x, no main check; desktop jobs need cli; nfpm maintainer lacks Name <email>; newRoot assigns global control.Version; README lists go test -race as verified without flake caveat
- Final: minor (deferred): queued jobs of a removed Origin can start before their Cancel arrives (manager.go:358); MOVE overwrite widens permissions 0600 to 0664 (davfs.go:715); parts retained for failed jobs of a removed Origin (manager.go:270); RemoteFS.Put client stalls with a stalled reader; WebDAV GET open to a just-removed host not stopped
- Final: minor (deferred): Important 6 persistent source read error leaves job waiting_peer forever; review Minors 6-9, 11-13: remote recursive delete timeout reports failure while continuing; jobs show omits hosts; finished jobs never pruned; possible duplicate job on submit timeout race; Windows backslash in noLinks; desktop start edge cases; no arm64 Linux or Windows desktop asset; x/net 412 on a lock holder's MOVE onto its own locked file; x/net treats MOVE with no Overwrite header as F

## 미룬 항목 (v0.1.1 리뷰)

- `WNetGetLastError` expansion: the extended error text a failed `WNetUseConnectionW` can carry is not read.
- Connect ignores ctx: the Windows connection call cannot be canceled by the context.
- The Run value length limit is not checked when the command line is long.
- Go notes and command output are English in the Korean GUI.
- Pre-existing: a pane keeps its old path when the share changes in the share list.
- Windows uninstall cannot tell "scheduled task absent" from "query failed" (the schtasks message is localized); a failed query is treated as absent.
- v0.1.2 tray (deferred): reopening from the tray un-maximizes a maximized window because `showWindow` unminimises first; `startTray` runs before the daemon and can delay it 5s; the tray icon stays up to 10s during shutdown; a failed icon load is undetected and shows a blank but clickable slot; a second app with another `--config-dir` is blocked by the single-instance lock; Windows 11 may put the icon in the overflow area, with no first-hide notice; the installer has no running-process check.

## 미룬 항목 (앱 내 업데이트와 자동 릴리스)

- macOS and Linux one-click update: both only open the release page now, because their asset formats and the missing signing make in-place replacement a separate job.
- Code signing and notarization: builds are unsigned, so the updater's checksum guards only against corruption, not a compromised repository, and SmartScreen and Defender may object.
- An updater for the `sugar-it` command.
- The restarted daemon gets only `daemon` and `--config-dir`: `--listen`, `--control`, `--dav` and `--advertise` given by hand are lost. The Run key value has none of them. Control has no way to report them.
- A daemon of this same `sugar-it-desktop.exe` for another config directory is not found, so it still locks the file. What the silent NSIS installer then does has not been seen: if it ignores the locked file and exits 0, the relaunched app stays on the old version and the banner returns with no message.
- A renamed copy of the program: the installer writes `sugar-it-desktop.exe` into the same folder, and the relaunch starts the old copy.
- The NSIS version metadata stays at the default 1.0.0: `wails.json` has no `info.productVersion`, so Add/Remove Programs does not show the release version.
- Wait for the release to be complete before it becomes latest: the release is visible while the desktop jobs still run, and the banner points to the page until the Windows assets arrive. A draft published by a last job would close that window.
- GitHub keeps one pending run per concurrency group: three quick merges skip the middle run, and its labels are not applied.
- A release whose desktop job failed stays published: rerun the failed job. No automatic retry.
- The installer writes the Start menu and desktop shortcuts on every run, so an update brings back a shortcut the user deleted.
- The status file holds only the last failure, and the helper does not report a successful update to the new app.
