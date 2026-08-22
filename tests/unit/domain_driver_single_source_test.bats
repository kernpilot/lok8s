#!/usr/bin/env bats
# domain_driver_single_source_test.bats — the cluster driver is read in one place.
#
# Why this exists
# ---------------
# #88 made `utils/domain.sh` the single domain-resolution and driver-identity
# point. #89 listed the sites that never converged: thirteen private
# `yq -r '.kind' | tr` reads across `lo`, provision, bootstrap, status, audit,
# lint, doctor, use, addons, inventory and kubehz. They disagreed on the three
# things that matter, and each disagreement had already produced a bug:
#
#   - `// ""`. Without it yq prints the literal string "null" for a missing key.
#     Non-empty, not "lo" — which is how `lo down` routed a kind-less spec to
#     driver-destroy.
#   - the `^[a-z][a-z0-9]*$` shape guard. The value is interpolated into
#     `drivers/<kind>/main` and sourced. Two sites checked it. Eleven did not.
#   - `yq | tr`. Without pipefail, tr's rc 0 masks a yq failure and the caller
#     reads an empty driver instead of an error.
#
# `domain::spec_driver` owns all three now. The sweep below is what keeps the
# fourteenth copy from appearing, and it carries an explicit allowlist: every
# remaining `.kind` read is named with the reason it is not a driver read.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq not available"
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"
  SPEC="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
}

teardown() { teardown_tmpdir; }

# ── the reader ────────────────────────────────────────────

@test "domain::spec_driver lowercases the declared kind" {
  printf 'kind: KubeOne\nmetadata:\n  name: t\n' > "${SPEC}"
  run domain::spec_driver "${SPEC}"
  [ "${status}" -eq 0 ]
  [ "${output}" = "kubeone" ]
}

@test "domain::spec_driver reads a missing .kind as ABSENT, never as 'null'" {
  # The bug behind the whole issue. yq prints the string "null" without `// ""`.
  printf 'apiVersion: cluster.lok8s.dev/v1beta1\nmetadata:\n  name: t\n' > "${SPEC}"
  run domain::spec_driver "${SPEC}"
  [ "${status}" -eq 1 ]
  [ "${output}" = "" ]
}

@test "domain::spec_driver reads an explicit null the same way" {
  printf 'kind: null\nmetadata:\n  name: t\n' > "${SPEC}"
  run domain::spec_driver "${SPEC}"
  [ "${status}" -eq 1 ]
  [ "${output}" = "" ]
}

@test "domain::spec_driver reports a malformed kind as rc 2, not as absent" {
  # rc 1 and rc 2 must stay distinct: a caller with a fallback defaults on 1
  # and must NOT default on 2 — the value reaches a path we source.
  printf 'kind: ../../evil\nmetadata:\n  name: t\n' > "${SPEC}"
  run domain::spec_driver "${SPEC}"
  [ "${status}" -eq 2 ]
  [ "${output}" = "" ]
}

@test "domain::spec_driver never defaults a malformed kind" {
  printf 'kind: "a b"\nmetadata:\n  name: t\n' > "${SPEC}"
  run domain::spec_driver "${SPEC}" lo
  [ "${status}" -eq 2 ]
  [ "${output}" != "lo" ]
}

@test "domain::spec_driver applies the fallback only when there is nothing to read" {
  run domain::spec_driver "${BATS_TEST_TMPDIR}/absent.yaml" lo
  [ "${status}" -eq 0 ]
  [ "${output}" = "lo" ]
}

@test "domain::spec_driver returns 1 on a missing file with no fallback" {
  run domain::spec_driver "${BATS_TEST_TMPDIR}/absent.yaml"
  [ "${status}" -eq 1 ]
  [ "${output}" = "" ]
}

@test "domain::spec_driver does not report an unreadable spec as a driver" {
  # `yq | tr` used to mask this: tr exits 0 and the caller saw an empty driver.
  printf 'kind: [unclosed\n' > "${SPEC}"
  run domain::spec_driver "${SPEC}"
  [ "${status}" -ne 0 ]
  [ "${output}" = "" ]
}

# ── domain::driver still answers for a whole domain ───────

@test "domain::driver routes through the shared reader" {
  mkdir -p "${PATH_CLUSTERS}/a.dev"
  printf 'kind: Lo\nmetadata:\n  name: t\n' > "${PATH_CLUSTERS}/a.dev/cluster.lok8s.yaml"
  run domain::driver a.dev
  [ "${status}" -eq 0 ]
  [ "${output}" = "lo" ]
}

@test "domain::driver rejects a malformed kind instead of passing it on" {
  # Behaviour CHANGE, deliberate: it used to echo whatever the file said.
  mkdir -p "${PATH_CLUSTERS}/b.dev"
  printf 'kind: ../../evil\nmetadata:\n  name: t\n' > "${PATH_CLUSTERS}/b.dev/cluster.lok8s.yaml"
  run domain::driver b.dev
  [ "${status}" -ne 0 ]
  [ "${output}" = "" ]
}

@test "domain::driver still answers 'deploy' for a deploy-only domain" {
  mkdir -p "${PATH_CLUSTERS}/c.dev"
  printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: a.dev\n' \
    > "${PATH_CLUSTERS}/c.dev/deploy.lok8s.yaml"
  run domain::driver c.dev
  [ "${status}" -eq 0 ]
  [ "${output}" = "deploy" ]
}

# ── the drift gate ────────────────────────────────────────

# Every yq read whose PROGRAM starts at `.kind` (or `.spec.kind`). Anything
# else — a `select(.kind == …)` over a manifest stream, a `[.kind, …] | @tsv` —
# is a Kubernetes object kind, not a cluster driver, and is not what this gates.
#
# What the pattern must NOT do is anchor on the assignment. The first version
# of this sweep required `=$(yq`, so the quoted spelling
#
#     k="$(yq -r '.kind' "${spec}")"
#
# evaded it — and that is the PRE-FIX spelling of libs/use:74, one of the
# thirteen sites #89 converged. The gate could not have caught a regression of
# the very thing it guards. Two allowlist rows proved it independently: the
# `libs/hooks` row (quoted) and the `libs/lint` spec_runtime row (`.spec.kind`)
# matched nothing the sweep ever produced, so both sat there dead. Matching on
# the yq PROGRAM instead of on the shell syntax around it covers every
# assignment spelling there is — `x=$(…)`, `x="$(…)"`, `local x=$(…)`,
# backticks, a bare argument, `< <(…)` — and it is the yq program, not the
# assignment, that says "this is a driver read".
#
# Comment lines are dropped: prose that quotes the pattern (utils/domain.sh's
# own docstring does) is not a read.
_kind_reads() {
  find "${_PROJECT_ROOT}/.lok8s" -type f ! -path '*/init.d/*' \
    ! -name '*.yaml' ! -name '*.yml' ! -name '*.json' ! -name '*.ts' -print0 \
    | xargs -0 grep -lE '^#!/usr/bin/env (argsh|bash)|^# shellcheck shell=bash' 2>/dev/null \
    | xargs grep -nE 'yq [^)]*'"'"'(\.spec)?\.kind' 2>/dev/null \
    | grep -vE ':[0-9]+:[[:space:]]*#' \
    | sed "s|^${_PROJECT_ROOT}/||"
}

# Every read that is NOT a cluster-driver read, with the reason. One entry per
# line: <path>|<substring that identifies the line>.
#
# Adding an entry here is a decision, not a formality — if the line reads a
# CLUSTER SPEC's .kind to pick a driver, it belongs in domain::spec_driver.
_allowed_kind_reads() {
  cat <<'ALLOW'
.lok8s/utils/domain.sh|kind=$(yq -r '.kind // ""' "${spec}"
.lok8s/libs/crds|kind=$(yq -r '.kind | downcase' "${schema}")
.lok8s/libs/addons|kind=$(yq -r '.kind // ""' "${dir}/chart.yaml"
.lok8s/libs/build|doc_count=$(yq -N '.kind' "${artifact}"
.lok8s/libs/hooks|kind="$(yq -r '.kind // ""' <<< "${doc}")"
.lok8s/libs/lint|spec_kind=$(yq -r '.kind // ""' "${spec_file}")
.lok8s/libs/lint|spec_runtime=$(yq -r '.spec.kind // .kind // ""' "${spec_file}")
.lok8s/libs/kubehz/hosted|kind=$(yq -r '.kind' "${cluster_yaml}")
ALLOW
}
# Reasons, in the same order:
#   utils/domain.sh   — the reader itself.
#   libs/crds         — the kind of a CRD SCHEMA source, not a cluster spec.
#   libs/addons       — a khelm chart.yaml's kind (ChartRenderer), not a driver.
#   libs/build        — counts documents in a rendered artifact.
#   libs/hooks        — a Kubernetes manifest document's kind, read per document
#                       out of a `select(documentIndex == …)` split and handed to
#                       hooks::_live_images to query the live object. Nothing to
#                       do with a cluster driver.
#   libs/lint (×2)    — VALIDATES the kind field, so it must see the raw value
#                       exactly as written, including a malformed one. lint::schema
#                       reports a MISSING `kind` / `spec.kind`; routing it through
#                       domain::spec_driver would collapse "absent" and
#                       "malformed" into one answer, which is the opposite of
#                       what a linter is for.
#   libs/kubehz/hosted — the wire field of the hosted-cluster payload. It goes
#                       to the kubehz API in the spec's original case
#                       ("KubeOne", not "kubeone"); lowercasing it here would
#                       change a cross-repo contract, so it stays until the API
#                       side is checked. This is the one item of #89 that this
#                       change does not close.

@test "every remaining .kind read is either the shared reader or allowlisted" {
  local hits; hits="$(_kind_reads)"
  # ANTI-VACUITY 1: the sweep must find the reader itself, or the pattern is
  # broken and this gate is measuring an empty set.
  [[ "${hits}" == *"utils/domain.sh"* ]] || {
    echo "the sweep did not even find domain::spec_driver's own read — the" >&2
    echo "grep in _kind_reads is stale, not the code." >&2
    echo "${hits}" >&2
    return 1
  }
  # ANTI-VACUITY 2: it must also find the QUOTED spelling. libs/hooks:130 is
  # `kind="$(yq -r '.kind // ""' …)"` — a `"` between the `=` and the `$(`.
  # The first version of this sweep required `=$(yq` and so missed exactly
  # that form, which is how the pre-fix libs/use:74 was written. This keeps a
  # live canary for it in the tree instead of trusting the pattern by reading.
  [[ "${hits}" == *"libs/hooks"* ]] || {
    echo "the sweep missed the QUOTED read in libs/hooks — the pattern is" >&2
    echo "anchored on the assignment again, so k=\"\$(yq -r '.kind' …)\"" >&2
    echo "evades it. Match the yq PROGRAM, not the shell syntax." >&2
    echo "${hits}" >&2
    return 1
  }

  local -a unexpected=()
  local line path allowed ok
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    path="${line%%:*}"
    ok=0
    while IFS='|' read -r a_path a_frag; do
      [[ -n "${a_path}" ]] || continue
      [[ "${path}" == "${a_path}" ]] || continue
      [[ "${line}" == *"${a_frag}"* ]] || continue
      ok=1
      break
    done < <(_allowed_kind_reads)
    (( ok )) || unexpected+=("${line}")
  done <<< "${hits}"

  [ "${#unexpected[@]}" -eq 0 ] || {
    echo "a private read of .kind reappeared:" >&2
    printf '  %s\n' "${unexpected[@]}" >&2
    echo "Use domain::spec_driver (issue #89). If the line reads a Kubernetes" >&2
    echo "object kind rather than a cluster driver, add it to" >&2
    echo "_allowed_kind_reads with a reason." >&2
    return 1
  }
}

@test "the allowlist has no stale entries" {
  # The other half of the gate. Without this, a converged site could leave its
  # exemption behind and the next copy in that file would sail through.
  #
  # The comparison is against the SWEEP OUTPUT, not against the file. An
  # earlier version grepped the file directly, which let a row survive that the
  # sweep could never produce: two rows here — libs/hooks (quoted) and
  # libs/lint's spec_runtime (`.spec.kind`) — matched their files fine while
  # being invisible to the sweep, so they exempted nothing and hid the fact
  # that the pattern was too narrow. Matching the sweep makes a row that
  # exempts nothing fail as loudly as a row whose code is gone.
  local hits; hits="$(_kind_reads)"
  local -a stale=()
  local a_path a_frag line found
  while IFS='|' read -r a_path a_frag; do
    [[ -n "${a_path}" ]] || continue
    found=0
    while IFS= read -r line; do
      [[ -n "${line}" ]] || continue
      [[ "${line%%:*}" == "${a_path}" ]] || continue
      [[ "${line}" == *"${a_frag}"* ]] || continue
      found=1
      break
    done <<< "${hits}"
    (( found )) || stale+=("${a_path} | ${a_frag}")
  done < <(_allowed_kind_reads)

  [ "${#stale[@]}" -eq 0 ] || {
    echo "these allowlist entries exempt nothing the sweep finds — either the" >&2
    echo "code is gone (remove the row) or the sweep no longer sees it (fix" >&2
    echo "the pattern in _kind_reads):" >&2
    printf '  %s\n' "${stale[@]}" >&2
    return 1
  }
}

@test "no lib lowercases a driver with the maskable yq-into-tr pipeline" {
  # The third disagreement. `yq … | tr` cannot fail: tr returns 0 and the
  # caller reads an empty driver. domain::spec_driver uses bash's `,,`.
  local hits
  hits=$(find "${_PROJECT_ROOT}/.lok8s" -type f ! -path '*/init.d/*' -print0 \
    | xargs -0 grep -nE "yq [^)]*\.kind.*\| *tr " 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a yq-into-tr driver read reappeared:" >&2
    echo "${hits}" >&2
    return 1
  }
}
