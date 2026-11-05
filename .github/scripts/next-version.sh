#!/usr/bin/env bash
# Prints the next release tag. Stdin: existing tags, one per line. Arguments: the
# merged pull request's label names. release:major wins over release:minor; with
# neither, the patch number goes up. Tags that are not plain vX.Y.Z are ignored,
# and no such tag at all counts as v0.0.0.
set -euo pipefail

bump=patch
for label in "$@"; do
  case "$label" in
    release:major) bump=major ;;
    release:minor) [ "$bump" = major ] || bump=minor ;;
  esac
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
highest=$(bash "$here/highest-version.sh")
major=0 minor=0 patch=0
if [ -n "$highest" ]; then
  IFS=. read -r major minor patch <<<"${highest#v}"
fi

case "$bump" in
  major) major=$((major + 1)) minor=0 patch=0 ;;
  minor) minor=$((minor + 1)) patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
printf 'v%d.%d.%d\n' "$major" "$minor" "$patch"
