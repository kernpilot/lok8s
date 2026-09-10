#!/usr/bin/env bats
# lo_install_test.bats — install/lo-install.sh against a file:// fixture release.
#
# The installer's whole job is "fetch, VERIFY, then install". These tests pin
# the three outcomes that matter: a matching checksum installs a working `lo`;
# a mismatching one installs NOTHING; `--dry-run` fetches nothing. The fixture
# is a release tree on disk (releases/download/<tag>/{asset,checksums.txt})
# reached through LO_INSTALL_BASE_URL=file://…, so no network is involved and
# the exact URL layout GitHub serves is what gets exercised.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v curl >/dev/null 2>&1 || skip "curl not available"

  INSTALLER="${_PROJECT_ROOT}/install/lo-install.sh"
  TAG="v9.9.9"
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) skip "unsupported arch for the fixture" ;;
  esac
  ASSET="lo-${OS}-${ARCH}.tar.gz"

  # A fake release: an executable `lo` (prints a recognisable version) in the
  # archive layout goreleaser produces (lo + LICENSE at the top level).
  RELEASE="${BATS_TEST_TMPDIR}/repo/releases/download/${TAG}"
  mkdir -p "${RELEASE}" "${BATS_TEST_TMPDIR}/pack"
  printf '#!/bin/sh\necho "lo version %s (fixture)"\n' "${TAG}" > "${BATS_TEST_TMPDIR}/pack/lo"
  chmod +x "${BATS_TEST_TMPDIR}/pack/lo"
  echo "MIT" > "${BATS_TEST_TMPDIR}/pack/LICENSE"
  tar -czf "${RELEASE}/${ASSET}" -C "${BATS_TEST_TMPDIR}/pack" lo LICENSE
  # The lo-full archive: same layout, the member is named after the build
  # (goreleaser `binary: lo-full`); the installer renames it to `lo`.
  FULL_ASSET="lo-full-${OS}-${ARCH}.tar.gz"
  printf '#!/bin/sh\necho "lo version %s (full fixture)"\n' "${TAG}" > "${BATS_TEST_TMPDIR}/pack/lo-full"
  chmod +x "${BATS_TEST_TMPDIR}/pack/lo-full"
  tar -czf "${RELEASE}/${FULL_ASSET}" -C "${BATS_TEST_TMPDIR}/pack" lo-full LICENSE
  ( cd "${RELEASE}" && sha256sum "${ASSET}" "${FULL_ASSET}" > checksums.txt )

  export LO_INSTALL_BASE_URL="file://${BATS_TEST_TMPDIR}/repo"
  export LO_INSTALL_REPO="fixture/lok8s"
  DEST="${BATS_TEST_TMPDIR}/bin"
}

teardown() { teardown_tmpdir; }

@test "installer parses (bash -n)" {
  run bash -n "${INSTALLER}"
  [ "${status}" -eq 0 ]
}

@test "a verified archive installs a working lo into --dir" {
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}"
  echo "${output}"
  [ "${status}" -eq 0 ]
  [ -x "${DEST}/lo" ]
  [[ "${output}" == *"checksum verified"* ]]
  run "${DEST}/lo" --version
  [ "${output}" = "lo version ${TAG} (fixture)" ]
}

@test "--full installs the lo-full build as lo, verified against its own checksum" {
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}" --full
  echo "${output}"
  [ "${status}" -eq 0 ]
  [ -x "${DEST}/lo" ]
  [ ! -e "${DEST}/lo-full" ]
  [[ "${output}" == *"checksum verified"* ]]
  [[ "${output}" == *"installed ${DEST}/lo (full)"* ]]
  run "${DEST}/lo" --version
  [ "${output}" = "lo version ${TAG} (full fixture)" ]
  # A corrupt lo-full sum must abort the --full install, while the core
  # archive's sum is untouched — verification is per asset.
  sed -i "s/^[0-9a-f]\{8\}\(.*${FULL_ASSET}\)$/00000000\1/" "${RELEASE}/checksums.txt"
  rm -f "${DEST}/lo"
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}" --full
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"MISMATCH"* ]]
  [ ! -e "${DEST}/lo" ]
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}"
  [ "${status}" -eq 0 ]
}

@test "a checksum mismatch aborts and installs nothing" {
  # Corrupt the recorded hash, keep the archive intact: the download itself
  # succeeds, only verification can catch it.
  sed -i 's/^[0-9a-f]\{8\}/00000000/' "${RELEASE}/checksums.txt"
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}"
  echo "${output}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"MISMATCH"* ]]
  [ ! -e "${DEST}/lo" ]
}

@test "an asset missing from checksums.txt is a failure, not a pass" {
  : > "${RELEASE}/checksums.txt"
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"no entry for ${ASSET}"* ]]
  [ ! -e "${DEST}/lo" ]
}

@test "--dry-run prints the plan and fetches nothing" {
  # Remove the fixture assets: a dry run that reaches for them would fail.
  rm -f "${RELEASE}/${ASSET}" "${RELEASE}/checksums.txt"
  run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}" --dry-run
  echo "${output}"
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"would download  ${LO_INSTALL_BASE_URL}/releases/download/${TAG}/${ASSET}"* ]]
  [[ "${output}" == *"would download  ${LO_INSTALL_BASE_URL}/releases/download/${TAG}/checksums.txt"* ]]
  [[ "${output}" == *"would install   ${DEST}/lo"* ]]
  [ ! -e "${DEST}" ]
}

@test "LO_VERSION and a bare version number both select the tag" {
  LO_VERSION="9.9.9" run bash "${INSTALLER}" --dir "${DEST}" --dry-run
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"/releases/download/${TAG}/${ASSET}"* ]]
}

@test "a plain-http release URL is refused before anything is written" {
  # The installer pins curl to https (and file:// for this fixture). A
  # regression to plain http must fail at the download, not silently
  # fetch an unverified archive over the network.
  LO_INSTALL_BASE_URL="http://127.0.0.1:9/lok8s" run bash "${INSTALLER}" --version "${TAG}" --dir "${DEST}"
  [ "${status}" -ne 0 ]
  # The scheme guard, not a connection error: port 9 refuses connections
  # too, and "download failed" would pass with the guard deleted.
  [[ "${output}" == *"must be https://"* ]]
  [ ! -e "${DEST}/lo" ]
}

@test "an unknown argument exits non-zero with usage" {
  run bash "${INSTALLER}" --bogus
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"Usage:"* ]]
  [[ "${output}" == *"unknown argument: --bogus"* ]]
}

@test "--help exits 0 and documents the flags" {
  run bash "${INSTALLER}" --help
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"--dry-run"* ]]
  [[ "${output}" == *"--version"* ]]
  [[ "${output}" == *"--dir"* ]]
}
