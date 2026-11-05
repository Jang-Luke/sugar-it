#!/usr/bin/env bash
# Tests next-version.sh and write-sha256.sh. Run by ci.yml; needs only bash.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fail=0

# check NAME WANT TAGS [LABEL...]: TAGS is what next-version.sh reads on stdin.
check() {
  local name=$1 want=$2 tags=$3
  shift 3
  local got
  got=$(printf '%s' "$tags" | bash "$here/next-version.sh" "$@") || got="exit $?"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: got '$got', want '$want'"
    fail=1
  else
    echo "ok   $name"
  fi
}

check "no tags at all"          v0.0.1 ""
check "patch by default"        v0.1.3 $'v0.1.0\nv0.1.1\nv0.1.2\n'
check "minor label"             v0.2.0 $'v0.1.2\n' release:minor
check "major label"             v1.0.0 $'v0.1.2\n' release:major
check "major wins over minor"   v1.0.0 $'v0.1.2\n' release:minor release:major
check "major wins, other order" v1.0.0 $'v0.1.2\n' release:major release:minor
check "unrelated labels"        v0.1.3 $'v0.1.2\n' bug "needs review"
check "no-tag major"            v1.0.0 "" release:major
check "no-tag minor"            v0.1.0 "" release:minor
check "numeric, not text, order" v0.10.1 $'v0.9.9\nv0.10.0\nv0.2.0\n'
check "highest wherever listed" v2.0.1 $'v2.0.0\nv0.1.0\nv1.9.9\n'
check "ignores non-semver"      v0.1.3 $'v0.1.2\nv9.9.9-rc1\nv9.9\nvendor-x\n9.9.9\nv9.9.9.9\nrelease-9\nv09.1.1\n'
check "only non-semver tags"    v0.0.1 $'nightly\nv1\n'
check "last line without newline" v1.2.4 'v1.2.3'
check "carriage returns"        v1.2.4 $'v1.2.3\r\n'
check "a huge number is not a version" v0.1.3 $'v0.1.2\nv99999999999999999999.0.0\n'

# write-sha256.sh: the file holds only the name, and verifies from its directory.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/sub"
printf 'hello\n' > "$tmp/sub/asset.bin"
if ! bash "$here/write-sha256.sh" "$tmp/sub/asset.bin"; then
  echo "FAIL write-sha256: exit status"
  fail=1
fi
want="5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  asset.bin"
if [ "$(cat "$tmp/sub/asset.bin.sha256" 2>/dev/null)" != "$want" ]; then
  echo "FAIL write-sha256: got '$(cat "$tmp/sub/asset.bin.sha256" 2>/dev/null)', want '$want'"
  fail=1
elif command -v sha256sum >/dev/null && ! (cd "$tmp/sub" && sha256sum -c asset.bin.sha256 >/dev/null); then
  echo "FAIL write-sha256: sha256sum -c refused it"
  fail=1
else
  echo "ok   write-sha256"
fi

exit "$fail"
