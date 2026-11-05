# Manual mount checks

`sugar-it mount` runs an OS command, so the automated suites only check the
plan (`internal/mount`) and the WebDAV server (`e2e`, `internal/davfs`). The
real mount needs a desktop session, which CI and the build host do not have.

Setup on every OS: start a daemon (`sugar-it daemon` returns once it runs in the
background; `sugar-it stop` ends it; its output is in `daemon.log` in the config
directory), add a share
(`sugar-it share add docs <folder>`), and pair at least one other host so a
peer folder exists. The WebDAV user is `sugar-it`; the password is the control
token. On macOS `sugar-it mount` prints it when Finder asks; on Linux it is
passed to `gio` on stdin; on Windows it is passed to the API and never shown.

## Linux (checked on the build host: not possible there)

Needs a session bus and the GVFS WebDAV backend (`gvfs-backends`).

```
sugar-it mount
```

Expected: the command succeeds and `Network` in the file manager (or
`gio mount -l`) shows `dav://sugar-it@127.0.0.1:7445/` with the peers as
folders. `sugar-it unmount` removes it.

Result on the build host (Ubuntu, no display, session bus present at
`/run/user/1000/bus`, no `gvfs-backends`):

```
Mounting with gio; the folders appear in your file manager under Network.
gio: dav://sugar-it@127.0.0.1:47445/: volume doesn't implement mount
sugar-it: gio failed: exit status 2
```

`gio` is installed, but without the GVFS DAV backend it has no handler for
`dav://`, so the mount was not attempted further. Not verified: whether `gio`
takes the password from stdin when it is not a terminal. If it prompts on the
terminal instead, run `sugar-it mount` from a terminal and paste the token
(`cat <config-dir>/control.token`); report it so the plan can change.

## macOS (to be checked by the user)

```
sugar-it mount
```

1. Finder opens a connect dialog for `http://127.0.0.1:7445/`.
2. Choose "Registered User", Name `sugar-it`, Password the token printed in the
   terminal.
3. Expected: a volume appears in Finder with the peers as folders; copy a file
   in and out.
4. `sugar-it unmount` runs `diskutil unmount /Volumes/127.0.0.1`. If the volume
   has another name, eject it in Finder. Report the real mount-point name.

## Windows (to be checked by the user)

Run in a normal prompt first:

```
sugar-it mount --drive Z:
```

1. If `BasicAuthLevel` is below 2, the command stops and prints a `reg add`
   command. Run it in an administrator prompt together with
   `net stop webclient && net start webclient`, then repeat. (The desktop app
   shows the same commands in Korean as an explanation, not as an error.)
2. A file-size warning (`FileSizeLimitInBytes`, default about 50 MB) is only a
   note; raise it as printed to copy larger files, up to 4294967295 bytes.
3. No password prompt appears: the command calls `WNetUseConnectionW` (mpr.dll)
   with the remote name `\\127.0.0.1@7445\DavWWWRoot`, user `sugar-it` and the
   token as the password argument, and prints `Mounted on <drive>`. Run on
   Windows 11 over SSH, from an administrator session, with `BasicAuthLevel` 2:
   a chosen and an automatic drive connected, then listing, reading, writing,
   deleting and unmount worked. Not run: a normal (non-administrator) session
   and a desktop session. Check these failures give a clear message: WebClient
   stopped or disabled, a drive letter already in use (85), a wrong token (5,
   86, 1326). Error 1244 (no logon session that may connect a network drive)
   was not seen over SSH; it is the message for a session that cannot, if one
   is found.
4. Expected: drive `Z:` in Explorer shows the peers; copy a file in and out.
   Without `--drive` Windows picks a free letter and the command reports it.
   If this remote name form is rejected, the `http://127.0.0.1:7445/` form is
   the alternative to try.
5. `sugar-it unmount --drive Z:` runs `net use Z: /delete /y`.

# Desktop app (`sugar-it-desktop`)

Not run on the build host: it has no display (no `DISPLAY`, no `xvfb-run`), so
the window itself was never opened. What the automated suites cover is the Go
side (`desktop/app_test.go`, against a real in-process daemon) and the front
end logic (`desktop/frontend/src/lib/*.test.ts`). Everything below is for a
person with a desktop session.

Build (Linux needs `libgtk-3-dev` and `libwebkit2gtk-4.1-dev`; the frontend must
be built before `go test` or `go vet` because `main.go` embeds
`frontend/dist`):

```
cd desktop
(cd frontend && npm install && npm run build && npm test -- --run)
go test -tags webkit2_41 .
wails build -tags webkit2_41      # output: desktop/build/bin/sugar-it-desktop
```

The service button installs a unit that runs the `sugar-it` command if one sits
next to `sugar-it-desktop`, otherwise `sugar-it-desktop daemon` itself, which
opens no window and needs no display.

Use two config directories so one machine acts as two hosts. Host A is the app,
host B a plain daemon on other ports:

```
sugar-it --config-dir /tmp/b daemon --listen :8443 --control 127.0.0.1:8444 --dav 127.0.0.1:8445 --advertise 127.0.0.1:8443
./desktop/build/bin/sugar-it-desktop --config-dir /tmp/a
```

1. With no daemon on `/tmp/a`, the window shows the banner with the service button. Linux: "앱을 닫으면 공유가
   중단됩니다". Windows with a working tray, and macOS: "창을 닫아도 공유는
   계속됩니다. …" (Windows: "트레이 아이콘의 종료를 누르면", macOS: "앱을
   종료하면"), which is true only if step 14 behaves as written. Start `sugar-it --config-dir /tmp/a
   daemon` first and relaunch: the banner is gone (the app uses that daemon).
2. Hosts tab, "공유 추가": pick a folder with a few files, name it `docs`.
   It appears in the list. Try a name with capitals: an error message in Korean.
3. Hosts tab, "초대 만들기" in the app; run `sugar-it --config-dir /tmp/b peer
   add '<invite>'`. Host B appears online. Make a share on B
   (`sugar-it --config-dir /tmp/b share add data <folder>`).
4. Files tab: left pane `이 컴퓨터` / `docs`, right pane host B / `data`. Select
   a file, "복사 →". A notice appears; the Transfers tab shows the progress bar,
   speed and a final "완료". Compare hashes of source and result with
   `sha256sum`.
5. Drag a file from one pane to the other (a copy when the panes show different shares or hosts, a move within one share); see 14 for the modifier keys.
   Choose "이름 바꿔 저장" before copying a name that already exists: a `(1)`
   copy appears. With "실패 처리", the transfer ends "일부 실패"/"실패" and the
   Transfers tab lists the item with a Korean reason; "재시도" works.
6. Create a folder with 100,000 empty files (`for i in $(seq 100000); do :>
   f$i; done`), share it and open it in a pane: scrolling stays smooth and the
   status line reads "100000개 항목".
7. Delete: the dialog names every item; cancelling deletes nothing. Rename and
   "새 폴더" work, and a duplicate name shows the "exists" message.
8. Stop host B (`kill`): the right pane shows "호스트가 오프라인입니다." with a
   "재시도" button and stays selectable; after restarting B, "재시도" lists it.
9. Hosts tab, "이 컴퓨터" card: rename this host. The new name shows at once and
   host B sees it after gossip; an invalid name shows the error banner.
   Then remove host B: the dialog shows its name; confirm removes it.
10. Mount (Hosts tab). Linux: needs `gvfs-backends` and a session bus; the
    result box shows `gio`'s output. macOS: Finder asks for credentials, use
    "토큰 보기" for the password (a native dialog; the token appears nowhere
    else in the window). Windows: no token is shown or typed; the app calls
    `WNetUseConnectionW` itself and the result box names the assigned drive.
    The same call was run from the CLI on Windows 11 (see Windows above); the
    app's button and the Hosts card have not been confirmed since the last fixes.
    The result box shows the output decoded from the console code page (Korean
    Windows writes CP949), not raw bytes. With `BasicAuthLevel` below 2 the card explains in
    Korean and lists the `reg add` commands (needing an administrator prompt)
    instead of showing an error; other failures show "마운트하지 못했습니다."
    with the detail.
11. Windows: "서비스 설치" runs `reg add HKCU\...\Run /v sugar-it`; no
    elevation is needed and the daemon does not start now, only at the next
    logon (the card says so in Korean). Check the value with `reg query
    HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v sugar-it`, log
    out and in, and see whether the daemon runs and whether a console window
    appears (expected only for `sugar-it.exe`). The Run value was written with
    correct quoting and removed again from an administrator session over SSH;
    the logon start, a non-administrator and the app's button are not run.
    "서비스 제거" removes the value and, if the v0.1.0 scheduled task `sugar-it`
    exists, deletes it (`schtasks /Query /TN sugar-it`, `/Delete /TN sugar-it
    /F`); without elevation that fails and the result shows the command to run
    as administrator. It then asks a daemon answering for this config directory
    to stop and says so; a daemon inside the app is left running until the app
    quits. Run it twice: the second run says nothing was installed and must
    not fail. A failed install shows "서비스를 설치하거나 제거하지 못했습니다."
    plus the detail. Not yet run on Windows: the scheduled-task removal and the
    daemon stop.
    Other systems: "서비스 설치" installs the per-user service (Linux: `systemctl --user status
    sugar-it`). While the app itself runs the daemon, the service fails to take
    the config-directory lock and retries every 5 seconds until the app quits.
    "서비스 제거" asks first, then stops and removes the service (systemctl
    disable --now on Linux, launchctl unload on macOS).
12. Kill the in-process daemon's port by starting another process on it before
    launch: the window shows the start error banner instead of crashing.
13. Automatic refresh (two panes, two hosts or two shares; not yet rendered).
    a. Create a file in a folder shown in a pane from outside the app (a shell
    or another host): within about 3 seconds it appears with no click on
    "새로 고침", the scroll position is unchanged and nothing blanks or shows
    "불러오는 중".
    b. Select a file, then delete another one outside: the selection stays on
    the first. Delete the selected file outside: it leaves the list and the
    selection and the "선택 n개" count follow.
    c. Minimize the window or switch to another tab and change files: nothing
    is requested (watch the daemon log or WebDAV access log); on return the
    list updates within a few seconds. Open a delete dialog or a context menu:
    no refresh happens until it closes.
    d. Start a copy between the panes: the destination pane shows files as they
    finish, without waiting for the job to leave the active set, and a small
    job that finishes within one second still shows up.
    e. In a folder of 100,000 files, listing takes seconds: the app does not
    refresh again until ten times that duration has passed.
    f. Navigate into a folder while a refresh is slow (stop the other host
    briefly): the old folder's rows never reappear. Stop host B while viewing
    its folder: the rows stay and a "목록을 새로 고치지 못했습니다" note shows;
    navigating to another folder on B instead clears the rows and shows the
    error.
14. Drag rules (not yet rendered). Without a key: dropping inside the same
    host and share (the other pane, or a folder row) moves; dropping on another
    host or share copies. The pointer and the small label beside it say
    "이동 → ..." or "복사 → ..." and match what happens. Shift always moves,
    Ctrl always copies (Option on macOS, where Ctrl+click is a context click),
    both held copies. The "이름이 겹치면" selector still
    applies.
15. Drop onto a folder (not yet rendered). Hovering a folder row outlines it;
    dropping puts the entries inside that folder, not in the pane's folder.
    Dropping on empty space or on a file row targets the pane's own folder.
    Also drag onto a folder row in the same pane. These are refused and start
    no job: the drop is not accepted (the pointer shows it is refused) and no
    message appears, because `dragover` rejects them: a folder onto itself or into its own subfolder
    (also through the other pane), and entries dropped on the folder they
    already sit in. Change folders in the middle of starting a large drop: the
    jobs still go to the folder dropped on.
    Refresh during a drag: while dragging, and while a dialog or menu is open,
    a listing that changed meanwhile is not applied; the hint and the drop go to
    the same folder, and the list updates after the drag ends. Dragging a file
    from Explorer into a pane does nothing (only drags started inside the app
    are accepted). Start a drag, scroll the source row out of view, release
    outside the pane: polling resumes within a few seconds and no stale move is
    armed.
16. Context menu (not yet rendered). Right click a row: the native menu does
    not appear; "열기" (folders only, one item), "다른 패널로 복사", "다른 패널로
    이동", "이름 변경" (one item only) and "삭제". Right click an unselected row
    selects only it; right click inside a selection keeps it. Delete still
    asks and names the items. Right click empty space: "새 폴더" and
    "새로 고침". Items are greyed out when the matching toolbar button is.
    The menu closes on a click outside it, Escape and scrolling the list, and
    stays inside the window when opened near the right or bottom edge.

    Starting a copy or move shows no notice. Make one fail, for example a move
    onto an existing name with the conflict policy `fail`: a notice with a
    button to the transfers tab appears once.

14. Window close, tray and second launch (none of this has been run on a
    display; the code was only built for Windows from Linux).
    a. Windows, config dir with no daemon so the app hosts it: click the close
    button. The window disappears, the tray icon stays (tooltip `sugar-it`),
    `Get-Process sugar-it-desktop` still lists it, and `sugar-it status` still
    answers. Left-click the icon: the window returns and has focus. Close it
    again and double-click the icon: same. Minimize the window, then click the
    icon: it is restored.
    b. Right-click the icon: the menu has 열기 and 종료. 열기 shows the window.
    종료 ends the process, `sugar-it status` stops answering (graceful stop: the
    ports are free and `runtime.json` is gone), and the icon is gone at once.
    Hover where the icon was: no ghost icon remains. Click 종료 twice quickly:
    no error, no second dialog. Also quit while the window is hidden.
    c. Windows, second launch: with the app running (window visible, then
    hidden, then minimized), start `sugar-it-desktop.exe` again. No second
    window or tray icon appears; the existing window is shown and focused each
    time. `sugar-it-desktop.exe daemon` is not affected and shows no icon.
    d. Windows, tray failure: hard to provoke. If the notification area is
    unavailable (for example, no Explorer shell), the app must quit when its
    window is closed, never hide with no icon. The log line is "tray
    unavailable, closing the window will quit". If Explorer restarts while the
    app runs, the icon should return.
    e. macOS: the close button hides the app and a Dock click brings the window
    back (`HideWindowOnClose`; from Wails' source, not run). Cmd+Q quits and
    stops an in-process daemon. No menu bar or tray icon is expected. A second
    launch shows the existing window. This path is unverified.
    f. Linux: the close button quits the app and an in-process daemon; there is
    no tray. A second launch from another terminal shows the existing window
    and exits.
    g. The banner text matches the platform as in step 1; the service texts say
    "종료" where they used to say "닫"; `sugar-it stop` against an in-process
    daemon says to quit the app.

## Background daemon (`sugar-it daemon`)

Linux was run by hand with a temporary config directory (start, `status`, a
second start refused, `stop`, `stop` again). The rest has not been run.

1. `sugar-it --config-dir /tmp/c daemon`: returns within a few seconds with one
   line (addresses, log path, how to stop); `status` works from a new terminal;
   close the starting terminal and `status` still works.
2. `daemon.log` exists with mode 0600 and holds "daemon running". A second
   `daemon` on the same directory exits 1 and leaves the first running.
3. Start with a busy `--control` port: exit 1 and the end of the log on stderr.
4. `sugar-it --config-dir /tmp/c stop` prints "stopped"; `status` then exits 1,
   the ports are free and `runtime.json` is gone. `stop` again exits 1.
5. With the desktop app running its own daemon, `sugar-it stop` says to quit
   the app (on Windows closing the window only hides it).
6. Windows: `sugar-it.exe daemon` from a console returns and leaves no console
   window for the daemon; `Get-Process sugar-it` shows it; `stop` ends it. The
   same with `sugar-it-desktop.exe daemon` started from Explorer or the Run key
   (a GUI-subsystem parent). After `service install` and a logon, see whether a
   console window flashes for `sugar-it.exe`. Not yet run on Windows.
7. macOS: the same as Linux; the LaunchAgent uses `daemon --foreground`.

## Desktop update

None of this has been run. It needs a release newer than the build under test, so
build an old version with `wails build -ldflags "-X main.version=0.0.1"`, or install
an older release. The tests in `internal/update` and `desktop/update_test.go` cover
the logic with fakes; only these steps touch GitHub, UAC and the installer.

1. A build without the `-ldflags` shows `dev` in the Hosts view, 지금 확인 says
   "개발 빌드는 업데이트를 확인하지 않습니다", and no connection to `api.github.com` is
   made.
2. With an old version: the banner names the new version shortly after startup. 나중에
   hides it; a restart shows it again. 지금 확인 says a newer version exists and brings
   the banner back. With the newest version it says "최신 버전입니다".
3. No network, or `api.github.com` blocked: no banner and no error at startup, a line
   "update check" in the log. 지금 확인 shows the error beside the button.
4. macOS and Linux: the banner says the app does not install in place. The button
   opens `https://github.com/Jang-Luke/sugar-it/releases/tag/v...` in the browser.
5. Windows, installed with the installer, with the app hosting the daemon and a
   transfer running. 업데이트 shows a dialog with the restart, the pause and the number
   of transfers. Clicking 취소 changes nothing. After confirming: a percentage, the app
   quits with no tray icon left, a UAC prompt. Accept: the new version starts
   unhidden and unelevated (Task Manager, Elevated: No), the Hosts view shows the new
   version, no installer window appeared, the transfer resumes, and
   `%TEMP%\sugar-it-update-*` holds at most the helper. A start an hour later removes it.
6. Windows, refuse the UAC prompt: the old version starts again and says the install
   was not allowed, once. Sharing works.
7. Windows, installed to a folder other than the default: the update stays in that
   folder. Note whether Defender or SmartScreen objects to the helper or the installer
   in `%TEMP%`; Defender flagged an earlier background start.
8. Windows, attached to a logon daemon: press the service button, log on again, start
   the app (no in-process banner). The update stops the daemon, and afterwards
   `sugar-it-desktop.exe daemon` runs again from the new file and the app is attached
   to it. With `--config-dir`, the same directory is used again. Repeat with
   `sugar-it.exe daemon`: that process is not touched and keeps its version.
9. Windows, keep the program locked by something else: a second user running the app,
   a daemon of another config directory, or reopen the app during the UAC prompt. The
   installer exits 0 without replacing the file. The app must say the install failed
   and offer the release page (a reopened app shows it within seconds).
10. Corrupt path: serve a wrong `.sha256` through a test release. The install is
    refused, the app keeps running, and nothing stays in `%TEMP%`.
11. Change the installer in `%TEMP%` during the UAC wait or before it. It must not run,
    or a write must be refused while the helper holds it. Note whether the installer
    starts at all with the handle held; if the first attempt fails with a sharing
    violation, the helper tries again unheld, which shows as a second UAC prompt.
12. Hold the app past 60 seconds (`--pid` of another process through a hand-started
    helper): nothing is installed or relaunched, and the running app shows the notice.
13. Make Defender or a rename remove the helper as it starts: within 15 seconds the
    update fails, the app keeps running and a stopped daemon is back.

## Automatic release

Not yet run. In a pull request, merge once with each of: no label (patch),
`release:minor`, `release:minor` and `release:major` (major), `no-release` (nothing
runs). Check that CI ran on the merge commit before the tag appeared, that the release
is not a pre-release, and that each desktop asset has a `.sha256` that verifies:
`sha256sum -c sugar-it-desktop_linux_amd64.sha256`. Also check that
`sugar-it-desktop` shows the version of the tag. Pushing a tag by hand must still
release. A failing test must leave no tag. Re-run an old failed run after a newer release: it
must fail at the tag step. Close a pull request unmerged during a queued release: the
pending release must still run.
