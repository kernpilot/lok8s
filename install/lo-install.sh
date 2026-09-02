#!/usr/bin/env bash
# lo-install.sh — install the `lo` binary from a lok8s GitHub release.
#
# This is NOT a `curl | sh` script. Download it, read it, then run it:
#
#   curl -fsSLO https://github.com/kernpilot/lok8s/releases/latest/download/lo-install.sh
#   curl -fsSLO https://github.com/kernpilot/lok8s/releases/latest/download/checksums.txt
#   sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c
#   less lo-install.sh
#   bash lo-install.sh
#
# What it does, in order:
#   1. picks the release (--version, LO_VERSION, or the latest published tag),
#   2. downloads lo-<os>-<arch>.tar.gz AND checksums.txt from that release,
#   3. verifies the archive's SHA-256 against checksums.txt — a mismatch aborts
#      before anything is extracted,
#   4. extracts `lo` and installs it to ~/.local/bin (or --dir),
#   5. prints the next steps.
#
# `--dry-run` performs step 1 and prints what steps 2–4 WOULD do, then exits.
#
# The Go `lo` is the single entrypoint of a lok8s project. Commands that are
# not ported yet pass through to the framework tree (.lok8s/lo), which each
# project still carries — see docs/reference/go-migration.md.

set -euo pipefail

# Upstream + endpoints. Overridable for forks and for tests (a file:// base
# URL works — the tests point one at a fixture tree).
: "${LO_INSTALL_REPO:=kernpilot/lok8s}"
: "${LO_INSTALL_BASE_URL:=https://github.com/${LO_INSTALL_REPO}}"
: "${LO_INSTALL_DIR:=${HOME}/.local/bin}"
: "${LO_VERSION:=}"

# ── output ───────────────────────────────────────────────────────────────────
say()  { printf '%b\n' "${*}" >&2; }
info() { say "  \033[36m•\033[0m ${*}"; }
ok()   { say "  \033[32m✓\033[0m ${*}"; }
warn() { say "  \033[33m!\033[0m ${*}"; }
die()  { say "  \033[31m✗\033[0m ${*}"; exit 1; }

usage() {
  cat <<EOF
lo-install.sh — install the lok8s \`lo\` binary from a GitHub release

Usage: bash lo-install.sh [--version <tag>] [--dir <path>] [--dry-run]

  -v, --version <tag>   Release tag to install (e.g. v0.3.0). Default: latest.
  -d, --dir <path>      Install directory. Default: ${LO_INSTALL_DIR}
  -n, --dry-run         Resolve the release and print what would be fetched
                        and installed, without downloading or writing anything.
  -h, --help            This text.

Environment: LO_VERSION, LO_INSTALL_DIR, LO_INSTALL_REPO (owner/repo),
             LO_INSTALL_BASE_URL (forks / mirrors / tests).

Every download is verified against the release's checksums.txt before use.
EOF
}

# ── host detection ───────────────────────────────────────────────────────────
# Release matrix: linux/darwin × amd64/arm64 (see .goreleaser.yaml).
detect_os() {
  case "$(uname -s)" in
    Linux)  echo linux ;;
    Darwin) echo darwin ;;
    *)      die "unsupported OS: $(uname -s) (releases cover linux and darwin)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo amd64 ;;
    aarch64|arm64)  echo arm64 ;;
    *)              die "unsupported CPU architecture: $(uname -m) (releases cover amd64 and arm64)" ;;
  esac
}

# ── fetching ─────────────────────────────────────────────────────────────────
# fetch <url> <dest> — https only for real downloads; file:// is allowed so
# the tests can serve a fixture tree. A redirect may not downgrade to http.
fetch() {
  curl -fsSL --proto '=https,file' --proto-redir '=https' "${1}" -o "${2}"
}

# resolve_latest — the tag GitHub's releases/latest redirects to. A redirect
# follow, not an API call: the REST API is rate-limited per IP when
# unauthenticated and has failed CI runners before; the redirect is not.
resolve_latest() {
  local effective
  effective="$(curl -fsSLI --proto '=https,file' --proto-redir '=https' \
    -o /dev/null -w '%{url_effective}' "${LO_INSTALL_BASE_URL}/releases/latest")" \
    || die "could not resolve the latest release at ${LO_INSTALL_BASE_URL}/releases/latest (pass --version <tag>)"
  local tag="${effective##*/}"
  [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]] \
    || die "latest release resolved to '${tag}', which is not a version tag (pass --version <tag>)"
  echo "${tag}"
}

# sha256_of <file> — GNU coreutils or macOS shasum.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${1}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${1}" | awk '{print $1}'
  else
    die "neither sha256sum nor shasum is available — cannot verify the download"
  fi
}

# expected_sum <checksums-file> <asset-name> — the recorded hash for one asset.
expected_sum() {
  awk -v name="${2}" '$2 == name || $2 == "*" name {print $1; exit}' "${1}"
}

main() {
  local version="${LO_VERSION}" dir="${LO_INSTALL_DIR}" dry_run=0
  while (( $# )); do
    case "${1}" in
      -v|--version) [[ -n "${2:-}" ]] || die "--version needs a tag"; version="${2}"; shift 2 ;;
      -d|--dir)     [[ -n "${2:-}" ]] || die "--dir needs a path"; dir="${2}"; shift 2 ;;
      -n|--dry-run) dry_run=1; shift ;;
      -h|--help)    usage; exit 0 ;;
      *)            usage >&2; die "unknown argument: ${1}" ;;
    esac
  done

  command -v curl >/dev/null 2>&1 || die "curl is required"
  command -v tar  >/dev/null 2>&1 || die "tar is required"

  local os arch
  os="$(detect_os)"; arch="$(detect_arch)"

  [[ -n "${version}" ]] || version="$(resolve_latest)"
  [[ "${version}" == v* ]] || version="v${version}"

  local asset="lo-${os}-${arch}.tar.gz"
  local release_url="${LO_INSTALL_BASE_URL}/releases/download/${version}"
  local dest="${dir}/lo"

  say ""
  say "  \033[1;36mlo-install\033[0m  \033[2m${LO_INSTALL_REPO} ${version} · ${os}/${arch}\033[0m"
  if (( dry_run )); then
    info "would download  ${release_url}/${asset}"
    info "would download  ${release_url}/checksums.txt"
    info "would verify    sha256(${asset}) against checksums.txt"
    info "would install   ${dest}"
    say ""
    ok "dry run — nothing was downloaded or written"
    return 0
  fi

  local tmp stage
  tmp="$(mktemp -d)"
  stage="${dest}.tmp.$$"
  # Expand now (SC2064): both paths are gone from scope at exit. The staging
  # file lives next to the destination, so it needs its own rm.
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'; rm -f '${stage}'" EXIT

  info "downloading ${asset}"
  fetch "${release_url}/${asset}" "${tmp}/${asset}" \
    || die "download failed: ${release_url}/${asset} (no such release, or no build for ${os}/${arch})"
  fetch "${release_url}/checksums.txt" "${tmp}/checksums.txt" \
    || die "download failed: ${release_url}/checksums.txt"

  # Verify BEFORE extracting. The checksums file must name this asset; an
  # absent entry is a failure, not a pass (a truncated checksums.txt would
  # otherwise verify anything).
  local want have
  want="$(expected_sum "${tmp}/checksums.txt" "${asset}")"
  [[ -n "${want}" ]] || die "checksums.txt from ${version} has no entry for ${asset}"
  have="$(sha256_of "${tmp}/${asset}")"
  [[ "${want}" == "${have}" ]] || die "checksum MISMATCH for ${asset}
      expected ${want}
      got      ${have}
    The download is corrupt or tampered with — nothing was installed."
  ok "checksum verified (sha256 ${have:0:12}…)"

  tar -xzf "${tmp}/${asset}" -C "${tmp}" lo \
    || die "${asset} does not contain a 'lo' binary"
  [[ -f "${tmp}/lo" ]] || die "${asset} does not contain a 'lo' binary"

  mkdir -p "${dir}" || die "cannot create ${dir}"
  # Stage next to the destination and rename: an interrupted copy never leaves
  # a half-written `lo` on PATH.
  if ! { cp "${tmp}/lo" "${stage}" && chmod 0755 "${stage}" && mv -f "${stage}" "${dest}"; }; then
    die "cannot write ${dest}"
  fi
  ok "installed ${dest}"

  local reported
  reported="$("${dest}" --version 2>/dev/null || true)"
  [[ -n "${reported}" ]] && info "${reported}"

  say ""
  say "  Next:"
  case ":${PATH}:" in
    *":${dir}:"*) ;;
    *) say "    \033[2mexport PATH=\"${dir}:\${PATH}\"\033[0m   \033[2m# ${dir} is not on your PATH yet\033[0m" ;;
  esac
  say "    \033[2mlo --help\033[0m"
  say "    \033[2mcd <your-project> && lo use <domain> && lo up\033[0m"
  say ""
  say "  A project still carries the framework tree (.lok8s/) — bootstrap a new"
  say "  one with 'b env add github.com/kernpilot/lok8s#local && b install'."
  say "  Details: https://lok8s.io/reference/go-migration"
  say ""
}

main "${@}"
