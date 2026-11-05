#!/usr/bin/env bash
# Writes <file>.sha256 beside <file> in sha256sum format, with the bare file name
# and two spaces, so that it verifies on any OS. Git Bash's sha256sum marks files
# with "*" and macOS may only have shasum, so the line is written here.
set -euo pipefail

file=$1
if command -v sha256sum >/dev/null; then
  sum=$(sha256sum "$file")
else
  sum=$(shasum -a 256 "$file")
fi
printf '%s  %s\n' "${sum%% *}" "$(basename "$file")" > "$file.sha256"
