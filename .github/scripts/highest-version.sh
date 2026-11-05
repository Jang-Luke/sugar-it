#!/usr/bin/env bash
# Prints the highest plain vX.Y.Z of the tags on stdin, one per line, or nothing.
set -euo pipefail

# At most nine digits: anything longer is not a version and would overflow.
num='(0|[1-9][0-9]{0,8})'
highest='' major=0 minor=0 patch=0
while IFS= read -r tag || [ -n "$tag" ]; do
  tag=${tag%$'\r'}
  [[ "$tag" =~ ^v$num\.$num\.$num$ ]] || continue
  a=${BASH_REMATCH[1]} b=${BASH_REMATCH[2]} c=${BASH_REMATCH[3]}
  if [ -z "$highest" ] || ((a > major || (a == major && (b > minor || (b == minor && c > patch))))); then
    highest=$tag major=$a minor=$b patch=$c
  fi
done
[ -z "$highest" ] || printf '%s\n' "$highest"
