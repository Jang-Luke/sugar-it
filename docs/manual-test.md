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
