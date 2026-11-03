# Manual mount checks

`sugar-it mount` runs an OS command, so the automated suites only check the
plan (`internal/mount`) and the WebDAV server (`e2e`, `internal/davfs`). The
real mount needs a desktop session, which CI and the build host do not have.

Setup on every OS: start a daemon (`sugar-it daemon`), add a share
(`sugar-it share add docs <folder>`), and pair at least one other host so a
peer folder exists. The WebDAV user is `sugar-it`; the password is the control
token, which `sugar-it mount` prints when the OS asks for it.

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
   `net stop webclient && net start webclient`, then repeat.
2. A file-size warning (`FileSizeLimitInBytes`, default about 50 MB) is only a
   note; raise it as printed to copy larger files, up to 4294967295 bytes.
3. `net use` asks for the password: user `sugar-it`, password the token printed
   in the terminal.
4. Expected: drive `Z:` in Explorer shows the peers; copy a file in and out.
   Without `--drive` Windows picks a free letter.
5. `sugar-it unmount --drive Z:` disconnects it.

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
sugar-it --config-dir /tmp/b daemon --listen :8443 --control 127.0.0.1:8444 --dav 127.0.0.1:8445 --advertise 127.0.0.1:8443 &
./desktop/build/bin/sugar-it-desktop --config-dir /tmp/a
```

1. With no daemon on `/tmp/a`, the window shows the banner "앱을 닫으면 공유가
   중단됩니다" with the service button. Start `sugar-it --config-dir /tmp/a
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
5. Drag a file from one pane to the other (copy); drag with Shift held (move).
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
9. Hosts tab, remove host B: the dialog shows its name; confirm removes it.
10. Mount (Hosts tab). Linux: needs `gvfs-backends` and a session bus; the
    result box shows `gio`'s output. macOS: Finder asks for credentials, use
    "토큰 보기" for the password (a native dialog; the token appears nowhere
    else in the window). Windows: report whether `net use ... *` can prompt
    for the password when started from the GUI app, which has no console. If
    not, the mount cannot be completed from the app on Windows.
11. "서비스 설치" installs the per-user service (Linux: `systemctl --user status
    sugar-it`). While the app itself runs the daemon, the service fails to take
    the config-directory lock and retries every 5 seconds until the app closes.
12. Kill the in-process daemon's port by starting another process on it before
    launch: the window shows the start error banner instead of crashing.
