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

# highest-version.sh: the highest plain tag, or nothing.
for c in $'v0.9.0\nv0.10.0\nv1.0.0-rc1\n=v0.10.0' $'nightly\n='; do
  got=$(printf '%s\n' "${c%=*}" | bash "$here/highest-version.sh")
  if [ "$got" != "${c##*=}" ]; then
    echo "FAIL highest-version: got '$got', want '${c##*=}'"
    fail=1
  else
    echo "ok   highest-version '${c##*=}'"
  fi
done

# release-guard.sh in a scratch repository: main is A-B-C, side branches off A.
git_() {
  git -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false -c tag.gpgsign=false "$@"
}
repo=$tmp/repo
git_ init -q -b main "$repo"
cd "$repo" || exit 1
git_ commit -q --allow-empty -m A
a=$(git rev-parse HEAD)
git_ commit -q --allow-empty -m B
b=$(git rev-parse HEAD)
git_ commit -q --allow-empty -m C
c=$(git rev-parse HEAD)
git_ checkout -q -b side "$a"
git_ commit -q --allow-empty -m S
s=$(git rev-parse HEAD)
git_ checkout -q main

# guard NAME COMMIT WANT[ok|fail] [TEXT]: TEXT must be in the error.
guard() {
  local out rc=0
  git checkout -q "$2"
  out=$(bash "$here/release-guard.sh" main 2>&1) || rc=$?
  if { [ "$3" = ok ] && [ "$rc" != 0 ]; } || { [ "$3" = fail ] && [ "$rc" = 0 ]; } ||
    { [ -n "${4:-}" ] && [[ "$out" != *"$4"* ]]; }; then
    echo "FAIL guard $1: exit $rc, output: $out"
    fail=1
  else
    echo "ok   guard $1"
  fi
}

guard "no tags at all"                 "$a" ok
guard "a commit that is not on main"   "$s" fail "not on"
git_ tag -a v0.1.4 -m v0.1.4 "$b"
guard "re-run of an older commit"      "$a" fail "v0.1.4"
guard "the commit already has a tag"   "$b" fail "already tagged"
guard "a newer commit"                 "$c" ok
git_ tag nightly "$c"
git_ tag v9.9.9-rc1 "$s"
guard "other tags do not count"        "$c" ok
git_ tag v0.2.0 "$s"
guard "highest tag not in the history" "$c" fail "v0.2.0"
cd "$here" || exit 1

exit "$fail"
