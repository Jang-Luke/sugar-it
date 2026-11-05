#!/usr/bin/env bash
# Refuses to tag HEAD unless it can honestly be the newest release. $1 is the branch
# releases come from. Run after a full fetch of commits and tags.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
base=$1

if ! git merge-base --is-ancestor HEAD "$base"; then
  echo "::error::$(git rev-parse HEAD) is not on $base; nothing was tagged"
  exit 1
fi
# Re-running every job after a release must not give the commit a second version.
existing=$(git tag --points-at HEAD | bash "$here/highest-version.sh")
if [ -n "$existing" ]; then
  echo "::error::this commit is already tagged ($existing); re-run only the failed jobs, or rebuild by hand"
  exit 1
fi
# Re-running an old failed run must not publish older code as the newest version.
highest=$(git tag --list | bash "$here/highest-version.sh")
if [ -n "$highest" ] && ! git merge-base --is-ancestor "$highest" HEAD; then
  echo "::error::the highest tag $highest is not in this commit's history, so this would release older code as the newest; nothing was tagged"
  exit 1
fi
