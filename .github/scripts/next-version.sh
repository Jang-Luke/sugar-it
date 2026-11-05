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

# At most nine digits: anything longer is not a version and would overflow.
num='(0|[1-9][0-9]{0,8})'
major=0 minor=0 patch=0
while IFS= read -r tag || [ -n "$tag" ]; do
  tag=${tag%$'\r'}
  [[ "$tag" =~ ^v$num\.$num\.$num$ ]] || continue
  a=${BASH_REMATCH[1]} b=${BASH_REMATCH[2]} c=${BASH_REMATCH[3]}
  if ((a > major || (a == major && (b > minor || (b == minor && c > patch))))); then
    major=$a minor=$b patch=$c
  fi
done

case "$bump" in
  major) major=$((major + 1)) minor=0 patch=0 ;;
  minor) minor=$((minor + 1)) patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
printf 'v%d.%d.%d\n' "$major" "$minor" "$patch"
