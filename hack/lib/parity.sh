#!/usr/bin/env bash
# hack/lib/parity.sh: the machinery shared by the parity harnesses.
#
# Every hack/parity-*.sh sources this file and keeps only its own fixtures
# and cases. The library owns the environment hygiene (parity::init), the
# differential runner (check, parity::run_pair, parity::compare), the
# exit-code contract check (expect_rc), the state and tree comparisons, the
# synthetic-project and stub builders, and the final report (report).
#
# The contract every harness relies on:
#   - The Go binary is ${LO_BIN}; the bash twin is the same binary with
#     LO_IMPL=bash forcing the argsh passthrough. Both run from a synthetic
#     project under ${WORK}, never from the developer's checkout.
#   - A case is green when exit code, stdout and stderr match. An allow
#     regex ("-" = none) drops matching lines from both streams before the
#     diff; that is how a documented divergence is tolerated per case.
#   - Results print as "ok: <label>" or "FAIL: <label> — <what>". The ok
#     lines are what hack/parity.sh counts per harness.
#
# Sourced, never executed. Usage in a harness:
#   source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
#   parity::init "${1:-}"
set -euo pipefail

PARITY_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── init / cleanup ───────────────────────────────────────────────────────────

# parity::init [path-to-go-lo]: resolve ROOT and LO_BIN (default: bin/lo),
# create WORK (removed on exit; PARITY_KEEP=1 leaves it behind for a
# post-mortem), unset the inherited project variables, pin the C locale.
#
# The harness must run against its synthetic project ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations, and every WRITE a harness
# makes, into that live repo instead of ${WORK}. Harnesses unset their own
# extra variables right after init.
#
# The C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists stores and dirs in byte order (= C collation). Under e.g. en_US.UTF-8
# the two orderings differ for mixed-case names, a cosmetic listing-order
# divergence a differential harness must not trip over.
parity::init() {
  ROOT="$(cd "${PARITY_LIB_DIR}/../.." && pwd)"
  LO_BIN="${1:-${ROOT}/bin/lo}"
  [[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

  WORK="$(mktemp -d)"
  PROJ="${WORK}/proj"
  if [[ -n "${PARITY_KEEP:-}" ]]; then
    echo "work dir: ${WORK}"
  else
    trap parity::cleanup EXIT
  fi

  unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
    DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG
  export LC_ALL=C

  failures=0
  go_rc=0
  bash_rc=0
}

# parity::cleanup: the EXIT trap. A harness-defined parity_cleanup hook
# (stop a stub server, …) runs first, then the work dir goes.
parity::cleanup() {
  if declare -F parity_cleanup >/dev/null; then parity_cleanup; fi
  rm -rf "${WORK}"
}

# parity::require_tools <tool...>: exit 2 unless every tool is on PATH.
parity::require_tools() {
  local tool
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 \
      || { echo "error: ${tool} required (the bash side uses it)" >&2; exit 2; }
  done
}

# ── the differential runner ──────────────────────────────────────────────────

# parity::run <impl> <dir> <argv...>: one implementation (go | bash), run
# from <dir> with stdin from PARITY_STDIN (empty = closed). Outputs land in
# ${WORK}/<impl>.out and ${WORK}/<impl>.err; the exit code is returned.
# Hooks: PARITY_PRE_EACH (a function name) runs before the command,
# PARITY_POST_EACH <impl> after it. Stateful harnesses use them to restore
# fixtures and to snapshot generated files per implementation.
parity::run() {
  local impl="${1}" dir="${2}"; shift 2
  local lo=("${LO_BIN}")
  [[ "${impl}" != bash ]] || lo=(env LO_IMPL=bash "${LO_BIN}")
  [[ -z "${PARITY_PRE_EACH:-}" ]] || "${PARITY_PRE_EACH}"
  local rc=0
  if [[ -n "${PARITY_STDIN:-}" ]]; then
    (cd "${dir}" && "${lo[@]}" "$@" <<<"${PARITY_STDIN}" >"${WORK}/${impl}.out" 2>"${WORK}/${impl}.err") || rc=$?
  else
    (cd "${dir}" && "${lo[@]}" "$@" </dev/null >"${WORK}/${impl}.out" 2>"${WORK}/${impl}.err") || rc=$?
  fi
  [[ -z "${PARITY_POST_EACH:-}" ]] || "${PARITY_POST_EACH}" "${impl}"
  return "${rc}"
}

# parity::run_pair <argv...>: both implementations, the Go binary first,
# then the LO_IMPL=bash twin, from PARITY_DIR_GO / PARITY_DIR_BASH (default:
# ${PROJ} for both; stateful sections give each implementation its own
# clone). Exit codes land in go_rc / bash_rc. The outputs are normalized:
# the project dir becomes PROJ and the work dir WORK (findings and usage
# lines embed absolute paths); a harness-defined parity_normalize
# <files...> hook runs after that.
parity::run_pair() {
  local dgo="${PARITY_DIR_GO:-${PROJ}}" dbash="${PARITY_DIR_BASH:-${PROJ}}"
  go_rc=0; bash_rc=0
  parity::run go "${dgo}" "$@" || go_rc=$?
  parity::run bash "${dbash}" "$@" || bash_rc=$?
  sed -i "s|${dgo}|PROJ|g; s|${WORK}|WORK|g" "${WORK}/go.out" "${WORK}/go.err"
  sed -i "s|${dbash}|PROJ|g; s|${WORK}|WORK|g" "${WORK}/bash.out" "${WORK}/bash.err"
  if declare -F parity_normalize >/dev/null; then
    parity_normalize "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"
  fi
}

# parity::rc_tolerated: the one documented rc divergence. argsh exits 2 on
# its own parse errors ("Error: too many arguments: …"), the Go binary exits
# 1 with the identical message (cli.argshErrorf; the cli-wide convention).
# PARITY_PARSE_RC=1 tolerates exactly that pair; PARITY_PARSE_RC=auto only
# when the bash stderr carries the argsh "Error: " line.
parity::rc_tolerated() {
  (( bash_rc == 2 && go_rc == 1 )) || return 1
  case "${PARITY_PARSE_RC:-0}" in
    1) return 0 ;;
    auto) grep -q '^Error: ' "${WORK}/bash.err" ;;
    *) return 1 ;;
  esac
}

# parity::compare <label> [allow-regex|-]: diff the last run_pair. The exit
# codes first (PARITY_PARSE_RC is read HERE, not by run_pair), then stdout
# and stderr with the allowed lines removed from both sides. Prints the FAIL
# lines; returns 1 on any difference. Never counts: the caller records the
# case (parity::record) so one case is one failure.
parity::compare() {
  local label="${1}" allow="${2:--}"
  local ok=1
  if (( go_rc != bash_rc )) && ! parity::rc_tolerated; then
    echo "FAIL: ${label} — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  local stream diff_out
  for stream in out err; do
    if [[ "${allow}" == "-" ]]; then
      diff_out="$(diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" || true)"
    else
      diff_out="$(diff <(grep -vE "${allow}" "${WORK}/bash.${stream}") \
                       <(grep -vE "${allow}" "${WORK}/go.${stream}") || true)"
    fi
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: ${label} — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  (( ok ))
}

# parity::record <ok> <label>: "ok: <label>", or one more failure.
parity::record() {
  if (( ${1} )); then
    echo "ok: ${2}"
  else
    failures=$((failures + 1))
  fi
}

# parity::finish <label> [allow-regex|-] [ok-label]: compare the last
# run_pair and record it (the ok line defaults to the label).
parity::finish() {
  local ok=1
  parity::compare "${1}" "${2:--}" || ok=0
  parity::record "${ok}" "${3:-${1}}"
}

# check <allow-diff-regex|-> <argv...>: one differential case. Runs
# `lo <argv>` in both implementations, diffs, labels it "lo <argv>".
check() {
  local allow="${1}"; shift
  parity::run_pair "$@"
  parity::finish "lo $*" "${allow}"
}

# fail <text>: a standalone failure line (a guard outside a case).
fail() { echo "FAIL: ${*}"; failures=$((failures + 1)); }

# expect_rc <rc> <argv...>: the CONTRACT check on the Go binary alone:
# parity would also pass if both implementations drifted together, so the
# documented exit codes are pinned explicitly. Runs from PARITY_DIR
# (default: ${PROJ}) with stdin closed. A harness that takes the dir as an
# argument wraps parity::expect_rc under its own signature.
expect_rc() { parity::expect_rc "$@"; }
parity::expect_rc() {
  local want="${1}"; shift
  local rc=0
  (cd "${PARITY_DIR:-${PROJ}}" && "${LO_BIN}" "$@" </dev/null >/dev/null 2>&1) || rc=$?
  if (( rc == want )); then
    echo "ok: rc ${want}: lo $*"
  else
    echo "FAIL: lo $* — rc=${rc}, want ${want}"
    failures=$((failures + 1))
  fi
}

# ── generated state ──────────────────────────────────────────────────────────

# parity::state_same <bash-file> <go-file> <label>: the file must be
# byte-identical across the two clones. Prints "ok: state <label>" or the
# FAIL block; returns 1 on a difference (the caller counts it).
parity::state_same() {
  if diff -q "${1}" "${2}" >/dev/null 2>&1; then
    echo "ok: state ${3}"
  else
    echo "FAIL: state ${3} differs between implementations"
    diff "${1}" "${2}" | head -10 | sed 's/^/  /' || true
    return 1
  fi
}

# tree_check <dir-go> <dir-bash> <label>: every file under the two clones
# (the linked framework/toolchain dirs excluded) must be byte-identical, and
# the file LISTS must match. Symlinks are compared by target. One case.
tree_check() {
  local dgo="${1}" dbash="${2}" label="${3}"
  local list_go list_bash
  list_go="$(cd "${dgo}" && find . -path ./.lok8s -prune -o -path ./.bin -prune -o \( -type f -o -type l \) -print | sort)"
  list_bash="$(cd "${dbash}" && find . -path ./.lok8s -prune -o -path ./.bin -prune -o \( -type f -o -type l \) -print | sort)"
  if [[ "${list_go}" != "${list_bash}" ]]; then
    echo "FAIL: tree ${label} — file lists differ:"
    diff <(echo "${list_bash}") <(echo "${list_go}") | head -20 | sed 's/^/  /' || true
    failures=$((failures + 1))
    return
  fi
  local f bad=0
  while IFS= read -r f; do
    [[ -n "${f}" ]] || continue
    if [[ -L "${dgo}/${f}" || -L "${dbash}/${f}" ]]; then
      local tgo tbash
      tgo="$(readlink "${dgo}/${f}" | sed "s|${dgo}|PROJ|")"
      tbash="$(readlink "${dbash}/${f}" | sed "s|${dbash}|PROJ|")"
      if [[ "${tgo}" != "${tbash}" ]]; then
        echo "FAIL: tree ${label} — symlink ${f}: bash=${tbash} go=${tgo}"
        bad=1
      fi
    elif ! cmp -s "${dbash}/${f}" "${dgo}/${f}"; then
      echo "FAIL: tree ${label} — ${f} differs between implementations:"
      diff "${dbash}/${f}" "${dgo}/${f}" | head -10 | sed 's/^/  /' || true
      bad=1
    fi
  done <<< "${list_go}"
  if (( bad )); then
    failures=$((failures + 1))
  else
    echo "ok: tree ${label} ($(echo "${list_go}" | grep -c . ) files identical)"
  fi
}

# ── synthetic projects and stubs ─────────────────────────────────────────────

# parity::new_project <dir> [copy-lok8s]: a synthetic project. clusters/,
# the framework tree linked (or COPIED when a section writes into it), the
# toolchain linked.
parity::new_project() {
  local dir="${1}" copy="${2:-0}"
  mkdir -p "${dir}/clusters"
  if (( copy )); then
    cp -R "${ROOT}/.lok8s" "${dir}/.lok8s"
  else
    ln -s "${ROOT}/.lok8s" "${dir}/.lok8s"
  fi
  ln -s "${ROOT}/.bin" "${dir}/.bin"
}

# parity::own_bin <dir>: give the project its OWN .bin, the real toolchain
# entry by entry (symlinks), so single tools can be replaced by stubs. Both
# implementations resolve tools through that directory first. A linked
# .bin (parity::new_project) is replaced.
parity::own_bin() {
  local dir="${1}" entry
  if [[ -L "${dir}/.bin" ]]; then rm "${dir}/.bin"; fi
  mkdir -p "${dir}/.bin"
  for entry in "${ROOT}"/.bin/*; do
    ln -s "${entry}" "${dir}/.bin/$(basename "${entry}")"
  done
}

# parity::stub <dir> <tool>: replace <dir>/.bin/<tool> with the script on
# stdin (a heredoc), executable. The linked real tool goes first.
parity::stub() {
  rm -f "${1}/.bin/${2}"
  cat > "${1}/.bin/${2}"
  chmod +x "${1}/.bin/${2}"
}

# parity::stub_refuse <dir> <tool>: a stub that fails loudly. The tool must
# never be reached from a harness.
parity::stub_refuse() {
  parity::stub "${1}" "${2}" <<SH
#!/usr/bin/env bash
echo "parity stub: ${2} must not be reached" >&2
exit 1
SH
}

# parity::stub_kubectl_fail <dir>: a kubectl that fails silently. No live
# cluster may be reached, and every caller must cope with an absent one.
parity::stub_kubectl_fail() {
  parity::stub "${1}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub: every kubectl fails silently (no live cluster may be reached).
exit 1
SH
}

# ── report ───────────────────────────────────────────────────────────────────

# report: the failure summary; exit 1 on any failure.
report() {
  if (( failures )); then
    echo; echo "${failures} parity failure(s)"
    exit 1
  fi
  echo; echo "parity: all checks passed"
}
