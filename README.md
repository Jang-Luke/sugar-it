# sugar-it

Share folders between your own computers over a peer-to-peer mesh. Each host
runs a daemon, shares some folders, and pairs with your other hosts. Files move
directly between hosts with `sugar-it cp` and `sugar-it mv`, from the desktop
app, or through a network drive (WebDAV) mounted on the local machine.

Status of this document: commands below were checked against the built
binary's `--help`, and the two-host quickstart was run with two local daemons
on Linux. What was not run is listed under [Verification status](#verification-status).

## Install

There is no published release yet. Build from source (Go 1.27 or newer):

```
go build -o sugar-it ./cmd/sugar-it
```

Tagged releases are meant to provide the following. The first two items are built by GoReleaser (`release.yml`, job `cli`); the desktop assets are built by a separate `wails build` job (`release.yml`, job `desktop`):

- `sugar-it` archives for linux, darwin and windows on amd64 and arm64
  (`.tar.gz`, `.zip` on Windows) and a `checksums.txt`.
- `.deb` and `.rpm` packages for Linux. They install `/usr/bin/sugar-it` only:
  they do not enable or start any service. Run `sugar-it service install` as
  your own user when you want the background service.
- `sugar-it-desktop` (Wails GUI, not part of GoReleaser or `checksums.txt`): a Linux binary, a macOS universal `.app`
  (zipped) and a Windows NSIS installer. These packages do not contain the
  `sugar-it` command. The app's service button runs the app's own binary as the
  daemon (`sugar-it-desktop daemon`, no window; it detaches like `sugar-it daemon`), or a `sugar-it` beside it.

Desktop window behaviour: only one desktop app runs at a time; starting it a
second time shows (and focuses) the existing window, also when it is hidden. On
Windows, the close button hides the window and the app keeps running, with the
daemon if the app hosts it, behind a notification-area (tray) icon: a left
click or double click shows the window, and the menu has 열기 and 종료. 종료
quits the app, stops an in-process daemon gracefully and removes the icon. If the
tray cannot start, closing the window quits. On macOS, closing hides the app
(Wails' `HideWindowOnClose`) and a Dock click shows it; there is no tray, and
Cmd+Q quits. On Linux, closing the window quits. None of this has been seen on a
display yet; see `docs/manual-test.md`.

Build the desktop app yourself (Linux needs `libgtk-3-dev` and
`libwebkit2gtk-4.1-dev`; the frontend must be built first because `main.go`
embeds `frontend/dist`; `wails build` creates `frontend/wailsjs` itself):

```
go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1
cd desktop/frontend && npm ci && npm run build && cd ..
wails build -tags webkit2_41        # -tags webkit2_41 is for Linux only
```

### Unsigned binaries

Nothing is code signed or notarized. Expect warnings:

- macOS: Gatekeeper blocks the `.app` and the CLI binary downloaded from a
  browser. Remove the quarantine flag (`xattr -d com.apple.quarantine <file>`)
  or allow it in System Settings, Privacy and Security.
- Windows: SmartScreen shows "Windows protected your PC" for the installer and
  `sugar-it.exe`. Choose "More info", then "Run anyway".

Check the `sugar-it` archives and packages against `checksums.txt` instead of
trusting the warning dialog either way. `checksums.txt` does not cover the
`sugar-it-desktop` assets, which have no checksum file.

## Firewall

Hosts connect to each other on **TCP 7443** (the Peer API, TLS 1.3). Allow
inbound TCP 7443 on every host that other hosts must reach. The Control API
(`127.0.0.1:7444`) and the WebDAV server (`127.0.0.1:7445`) listen on loopback
only and need no firewall rule; the daemon refuses to start with any other
address for them.

## Quickstart: two hosts

Host A shares `~/docs`; host B pairs with A and copies a file.

On host A:

```
sugar-it daemon                       # starts in the background and returns; or: sugar-it service install
sugar-it peer rename alpha            # name this host (lowercase letters, digits, -)
sugar-it share add docs ~/docs
sugar-it invite                       # prints sugar-it1:... (valid 10 minutes)
```

On host B:

```
sugar-it daemon
sugar-it peer rename beta
sugar-it share add data ~/data
sugar-it peer add 'sugar-it1:...'     # paste the invitation from A
sugar-it peer list
sugar-it ls alpha                     # shares of A
sugar-it ls alpha:docs                # contents of A's share
sugar-it cp alpha:docs/hello.txt beta:data/
sugar-it mv alpha:docs/big.iso beta:data/ --detach   # prints a job id
sugar-it jobs --help                  # follow and control transfers
```

Paths are `<peer>:<share>/<path>`; `local` always means this host. Other
commands: `status`, `stop`, `mkdir`, `rename`, `rm`, `peer remove`, `mount`,
`unmount`, `service install`, `service uninstall`. Every command has `--help`.

`sugar-it daemon` detaches: it starts the daemon in the background, waits until
it answers (up to 15 seconds), prints its addresses and the log path, and
returns. The daemon's output goes to `daemon.log` in the config directory
(mode 0600, appended on every start, moved to `daemon.log.1` when it is over
10 MiB at the next start). If the daemon fails to start, the command prints the
end of the log and exits 1. `sugar-it stop` stops it gracefully through the
Control API and waits until it is gone. `sugar-it daemon --foreground` keeps the
old behaviour (runs in the terminal until Ctrl-C or SIGTERM); the systemd and
launchd units use it. A daemon hosted by the desktop app cannot be stopped with
`stop`: quit the app (on Windows, closing the window only hides it to the tray).

A daemon started before this change runs in a terminal and has no `stop`
endpoint: stop it with Ctrl-C (or restart it once), then start the new one.

Run on two local daemons on Linux, the sequence above printed `paired with
alpha`, then the share listing, then
`<job id>: done, 1 of 1 files`, and the copied file had the same content.

## Trust model

There is one owner and the hosts are all that owner's. A host that has been
paired is a full member: **any member can create invitations and add hosts, and
any member can remove any other member**. There are no roles and no per-share
permissions between members. Anyone who holds an unexpired invitation can join,
so treat an invitation like a password: it is short lived (`--ttl`, default 10
minutes), but do not post it anywhere public. Non-members are refused on every
Peer API endpoint except pairing.

### Removal caveat

Membership is replicated between hosts, with no central ordering. A host that
has been removed and then paired again with a newer invitation than the remover
had seen wins over that removal. If you remove a host and it reappears once the
hosts have exchanged state, repeat `sugar-it peer remove <name>` after the
views have converged. A removed host that has not yet learned it was removed
can still create an invitation, and running `peer add` with it re-admits that
host; do not use invitations from a host you removed.

Removal takes effect on what is already running, whether you removed the host
or the news arrived from another one. Its open downloads, uploads and hashes on
this host stop at once. A job reading from it fails with `peer_not_found`,
keeps its sources, and can be retried after pairing again. A job it requested is
canceled; files already moved stay moved.

## Network drive (`sugar-it mount`)

`sugar-it mount` exposes the whole mesh through the local WebDAV server and
asks the OS to mount it. The WebDAV user is `sugar-it` and the password is the
control token. On macOS the command prints it for Finder's prompt; on Linux
it goes to `gio` on stdin; on Windows it is passed to the API and never shown.

Moving or copying onto an existing file replaces it atomically, keeping the old
file if anything fails. Moving or copying a folder onto something that exists,
or anything onto an existing folder, is refused (HTTP 412): delete the
destination first.

Windows limits (from the Windows WebClient service, not from sugar-it):

- Basic authentication over plain HTTP is disabled by default
  (`BasicAuthLevel`). `sugar-it mount` stops and prints the `reg add` command
  and the `net stop webclient && net start webclient` to run in an
  administrator prompt.
- WebClient refuses files larger than `FileSizeLimitInBytes` (default about
  50 MB) and cannot go above 4294967295 bytes (4 GB). For large files use
  `sugar-it cp` or the desktop app instead of the drive.
- The drive letter is chosen with `--drive Z:`, otherwise Windows picks a
  free one.

See [docs/manual-test.md](docs/manual-test.md) for the step-by-step checks.

## Verification status

Verified on the Linux build host:

- Unit and integration tests and the three-daemon e2e suite
  (`go test -race ./...`; in one full run `TestGracefulShutdownKeepsJobs`
  timed out once while other commands ran at the same time, and it passed when
  run alone and in the second full run), cross-compilation for windows and darwin with
  `CGO_ENABLED=0`, and the two-host quickstart above.

**Verified in CI**: `go test -race ./...` passes on `ubuntu-24.04`, `macos-14`
and `windows-2022`, and the Linux desktop job builds the app.

**Verified between Linux and Windows 11**, over a Tailscale network with real
daemons: pairing in both directions, a 256 MiB copy with matching SHA-256, a
Korean file name, a move where `a:b` and `CON` fail with `invalid_name` and a
case-only twin fails with `conflict` while all three sources stay, a copy from
Windows to Linux, and a 512 MiB copy resumed to a matching SHA-256 after the
destination daemon was killed. `sugar-it mount` at the default `BasicAuthLevel`
refuses with the administrator commands. The tests and e2e suite also pass on
that Windows machine.

**Verified release**: tag `v0.1.0` ran the release workflow on all jobs. The Linux
archive and deb from that release match `checksums.txt` and report their version.

**Verified packaging**: `goreleaser release --snapshot --clean` builds the six
archives and the deb and rpm packages, and `checksums.txt` matches them.

**Not verified**:

- The docker `dpkg -i` install check has never been run. The amd64 deb was
  unpacked instead: it holds only `/usr/bin/sugar-it`, has no maintainer
  scripts, and that binary prints its version. The rpm packages were built but
  never unpacked or installed, and the macOS binaries were built but never run.
- Mounting on macOS and on Linux (the build host has no `gvfs-backends`).
- Mounting from the Windows app's button, in a desktop session. The same
  `WNetUseConnectionW` path was run from the CLI over SSH on Windows 11 with
  `BasicAuthLevel` 2: it connected a chosen and an automatic drive, then listed,
  read, wrote and deleted, and disconnected. The `\\127.0.0.1@port\DavWWWRoot`
  form is what that run used.
- The background start (`sugar-it daemon` without `--foreground`) on macOS, and
  from the GUI-subsystem `sugar-it-desktop.exe`, and whether a console flashes
  at logon on Windows. On Windows 11 over SSH, `sugar-it.exe daemon` outlived the
  session, a second start was refused, `stop` removed `runtime.json`, and
  `service uninstall` removed the Run value and stopped the daemon.
- `sugar-it service uninstall` on Windows removing a v0.1.0 scheduled task, and
  the error when that needs an administrator. Uninstall stops the service on Linux and
  macOS. On Windows it asks a running daemon to stop, and a daemon inside the
  desktop app keeps running until the app quits.
- `sugar-it service install` on Windows actually starting the daemon at logon.
  The Run key value was written with correct quoting and removed again, from an
  administrator session. A non-administrator has not been tried.
- The macOS desktop asset has been built by the release workflow but never
  opened. The Windows app has been opened and used: the file panes work within
  one host, with the buttons and drag copy working. Dragging by the Explorer
  rules (move or copy, drops onto folder rows), the right-click menu and the
  automatic refresh were added after that and have not been rendered or run
  in any window.
- On Windows, `sugar-it.exe` is a console program, so registering it to run at
  logon (`sugar-it service install`) shows a console window each time, whereas
  `sugar-it-desktop.exe` is a GUI-subsystem program and shows none. The install
  only writes the Run key value; it takes effect at the next logon, not now.

**Known limit on Windows**: a share folder cannot be renamed or deleted while
the daemon has it open, because `os.Root` holds the directory without
`FILE_SHARE_DELETE`. `sugar-it share remove` or stopping the daemon releases it.
Windows also has no directory fsync, so after a power cut a new name's survival
depends on NTFS, where Linux and macOS sync the directory first.

**Windows Defender**: in one test, Defender reported `Behavior:Win32/Execution.A!ml`
and removed `sugar-it.exe` and its scheduled task shortly after
`sugar-it service install` and `schtasks /Run`. That observation stands as
recorded for the scheduled-task version; the service now uses a Run key and no
`schtasks /Run`, and whether Defender detects that is unknown. That binary was
unsigned, in `%TEMP%`, and run over SSH, so an installed copy may behave
differently; this has not been tested. If it happens to you, Windows Security
shows the detection.

**Upgrading the Windows app**: from v0.1.2, closing the window keeps the app in
the tray, which keeps `sugar-it-desktop.exe` locked. Choose 종료 from the tray
menu before running a new installer.

**Upgrading from v0.1.0**: `sugar-it daemon` now detaches, so a service
installed by v0.1.0 stops working: run `sugar-it service install` again to
rewrite it, which adds `--foreground` on Linux and macOS. On Windows, run
`sugar-it service uninstall` to remove the old scheduled task; it may need an
administrator prompt. A daemon already running from v0.1.0 has no `stop`: use
Ctrl-C or restart it once.

## Development

```
go test ./...                                 # root module, includes e2e (minutes)
go test -short ./...                          # skips the large-file kill tests
cd desktop/frontend && npm ci && npm run build && npm test -- --run
cd desktop && go test -tags webkit2_41 .      # Linux; needs the frontend built
goreleaser release --snapshot --clean         # local packaging test only
```
