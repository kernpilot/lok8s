#!/usr/bin/env bash
# check-yq-pin.sh — the installed yq is the release that .bin/b.yaml pins.
#
# `b install` exits 0 when a binary fails to install, and the bats container
# then runs its own yq instead. So CI checks the binary after the install:
# `<bin>/yq --version` must name the version of the yq entry in <bin>/b.yaml.
# The pin is read without yq (awk), so a broken yq cannot pass the check.
#
#   hack/check-yq-pin.sh [<bin dir>]     default: the repository's .bin
set -euo pipefail

bin="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.bin}"
b_yaml="${bin}/b.yaml"
yq="${bin}/yq"

fail() {
  printf '::error::%s\n' "$*"
  exit 1
}

[[ -f "${b_yaml}" ]] || fail "${b_yaml} does not exist"
# The version of the top-level binaries.yq entry: the line `  yq:` and the
# first `    version:` line under it, before the next entry. Quotes and a
# trailing comment are removed.
pinned=$(awk '
  /^[^[:space:]#]/ { inbin = ($0 ~ /^binaries:/); inyq = 0; next }
  inbin && /^  [^[:space:]#]/ { inyq = ($0 ~ /^  yq:[[:space:]]*$/); next }
  inyq && /^    version:/ {
    sub(/^    version:[[:space:]]*/, "")
    sub(/[[:space:]]*(#.*)?$/, "")
    gsub(/["\047]/, "")
    print
    exit
  }
' "${b_yaml}")
[[ -n "${pinned}" ]] || fail "${b_yaml} pins no yq version (binaries.yq.version)"

[[ -x "${yq}" ]] || fail "${yq} is not installed: b install did not install yq (b exits 0 when a binary fails)"
out=$("${yq}" --version 2>&1) || fail "${yq} --version failed: ${out}"
re='version v?([0-9]+\.[0-9]+\.[0-9]+)'
[[ "${out}" =~ ${re} ]] || fail "${yq} --version names no version: ${out}"
installed="v${BASH_REMATCH[1]}"

[[ "${installed}" == "${pinned}" ]] \
  || fail "${yq} is ${installed}, but ${b_yaml} pins ${pinned}: b install did not install the pinned yq"
printf 'yq %s in %s is the pinned release\n' "${installed}" "${bin}"
