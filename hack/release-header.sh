#!/usr/bin/env bash
# release-header.sh — print the hand-written notes of a tag as the release
# header: docs/releases/<tag>.md without its leading YAML front matter.
#
# The notes are also a docs page, so they start with a VitePress front
# matter block (`---`, `title: ...`, `---`). goreleaser's --release-header
# copies the file as it is, so the v0.13.0 and v0.14.1 release bodies started
# with that block. The release workflow passes the output of this script to
# --release-header instead of the notes file.
#
# Front matter is a first line `---` and the next line `---` that closes it.
# The block and the blank lines after it are removed. A file without a first
# line `---`, or with no closing `---`, is printed unchanged. A UTF-8 byte
# order mark before the first `---` does not hide the block.
#
#   hack/release-header.sh docs/releases/v0.14.1.md > "${RUNNER_TEMP}/release-header.md"
set -euo pipefail

notes="${1:?usage: release-header.sh <notes file, e.g. docs/releases/v0.14.1.md>}"
[[ -f "${notes}" ]] || { echo "release-header: no such file: ${notes}" >&2; exit 1; }

# delimiter LINE: the line is `---`, with trailing whitespace or a CR allowed.
delimiter() { [[ "${1}" =~ ^---[[:space:]]*$ ]]; }

mapfile -t lines < "${notes}"
(( ${#lines[@]} == 0 )) || lines[0]="${lines[0]#$'\xef\xbb\xbf'}"
start=0
if (( ${#lines[@]} > 0 )) && delimiter "${lines[0]}"; then
  for (( i = 1; i < ${#lines[@]}; i++ )); do
    if delimiter "${lines[i]}"; then
      start=$(( i + 1 ))
      while (( start < ${#lines[@]} )) && [[ "${lines[start]}" =~ ^[[:space:]]*$ ]]; do
        start=$(( start + 1 ))
      done
      break
    fi
  done
fi

if (( start == 0 )); then
  cat -- "${notes}"
elif (( start < ${#lines[@]} )); then
  printf '%s\n' "${lines[@]:start}"
fi
