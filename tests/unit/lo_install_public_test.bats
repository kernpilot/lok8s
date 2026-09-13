#!/usr/bin/env bats
# lo_install_public_test.bats: the installer the docs site serves is the
# installer the release verifies.
#
# `docs/public/lo-install.sh` is a committed copy of `install/lo-install.sh`.
# VitePress copies docs/public/ into the site root, so it is served at
# https://lok8s.io/lo-install.sh next to the legacy `lo-up`. The release
# asset of the same name is what checksums.txt covers; a copy that drifts
# from it would hand a script to every new user that no checksum vouches
# for. Nothing rebuilds the copy, so this gate holds the two byte-identical
# (the same rule loup_bundle_sync_test.bats applies to lo-up).

setup() {
  load "../test_helper"
  SOURCE="${_PROJECT_ROOT}/install/lo-install.sh"
  PUBLIC="${_PROJECT_ROOT}/docs/public/lo-install.sh"
}

@test "the source installer is a real script, not a stub" {
  # ANTI-VACUITY: the byte comparison below would pass over two empty files.
  [ -f "${SOURCE}" ] || { echo "missing ${SOURCE}" >&2; return 1; }
  local size; size=$(wc -c < "${SOURCE}")
  [ "${size}" -gt 4000 ] || {
    echo "${SOURCE} is only ${size} bytes; that is not the installer." >&2
    return 1
  }
  grep -q '^set -euo pipefail$' "${SOURCE}"
  grep -q 'checksums.txt' "${SOURCE}"
}

@test "docs/public/lo-install.sh is byte-identical to install/lo-install.sh" {
  [ -f "${PUBLIC}" ] || {
    echo "missing ${PUBLIC}: the docs site would serve a 404 at /lo-install.sh." >&2
    echo "Copy it:  cp install/lo-install.sh docs/public/lo-install.sh" >&2
    return 1
  }
  cmp -s "${SOURCE}" "${PUBLIC}" || {
    echo "the published installer is STALE: docs/public/lo-install.sh differs from" >&2
    echo "install/lo-install.sh (the copy checksums.txt covers):" >&2
    diff -u "${SOURCE}" "${PUBLIC}" | head -40 >&2
    echo "Refresh it:  cp install/lo-install.sh docs/public/lo-install.sh" >&2
    return 1
  }
}

@test "the published copy parses (bash -n)" {
  run bash -n "${PUBLIC}"
  [ "${status}" -eq 0 ]
}
