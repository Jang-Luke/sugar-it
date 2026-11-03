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
  (zipped) and a Windows NSIS installer.

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
only and need no firewall rule.

## Quickstart: two hosts

Host A shares `~/docs`; host B pairs with A and copies a file.

On host A:

```
sugar-it daemon                       # leave running; or: sugar-it service install
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
commands: `status`, `mkdir`, `rename`, `rm`, `peer remove`, `mount`, `unmount`,
`service install`. Every command has `--help`.

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

## Network drive (`sugar-it mount`)

`sugar-it mount` exposes the whole mesh through the local WebDAV server and
asks the OS to mount it. The WebDAV user is `sugar-it` and the password is the
control token, which the command prints when the OS asks for it.

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

**Not verified**:

- The GoReleaser snapshot, the six archives, the deb and rpm packages, the
  nfpm contents, and the docker `dpkg -i` install check have never been run.
  `.goreleaser.yaml` has never been parsed by GoReleaser.
- Mounting on macOS and on Windows, and on Linux (the build host has no
  `gvfs-backends`).
- The desktop app walk-through (no display on the build host). Its Go and
  front-end tests pass; the window has never been opened.
- The CI and release workflows: no remote exists, so they have never run on
  GitHub. In particular the macOS and Windows test jobs, the e2e suite on those
  OSes, `wails build -platform darwin/universal` and the Windows NSIS build are
  untested.

## Development

```
go test ./...                                 # root module, includes e2e (minutes)
go test -short ./...                          # skips the large-file kill tests
cd desktop/frontend && npm ci && npm run build && npm test -- --run
cd desktop && go test -tags webkit2_41 .      # Linux; needs the frontend built
goreleaser release --snapshot --clean         # local packaging test only
```
