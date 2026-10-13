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

# Every yq read of a `.kind`-rooted accessor. Two rounds of review went into
# this pattern and the lesson of both is the same: STOP ANCHORING ON HOW THE
# CALL IS WRITTEN.
#
#   round 1 anchored on the shell ASSIGNMENT (`=$(yq`), so `k="$(yq -r '.kind'
#           …)"` — the pre-fix spelling of libs/use:74, one of the thirteen
#           sites #89 converged — walked straight through it;
#   round 2 anchored on the QUOTING (a literal `'` before `.kind`), so
#           `kind=$(yq -r ".kind // \"\"" "${spec}")` walked through instead.
#           Not an exotic spelling: .lok8s holds dozens of double-quoted yq
#           programs, and this PR's own new reader (utils/spec.sh) is one.
#
# Both anchors were incidental syntax. What identifies a driver read is the
# ACCESSOR — a path expression rooted at the document's `kind` — so that is
# what the pattern matches, in whatever notation:
#
#   '.kind'   ".kind"   .kind   '.spec.kind'   '.["kind"]'   ".[\"kind\"]"
#   '.spec["kind"]'     '.["spec"].kind'       "${root}.kind"
#
# and, separately, the same accessor written into a VARIABLE that is handed to
# yq later (`q='.kind'; yq -r "${q}" …`) — caught where the program is written
# rather than where it is run, because that is the only place it exists as text.
#
# The leading-delimiter class (quote, whitespace, `}`, `|`) is what keeps
# `.kind` at the ROOT: `.metadata.kind` and `.items[].kind` are preceded by a
# word character or `]` and do not match. `|` is in the class (#143): a
# pipeline stage — `select(documentIndex == 0)|.kind` — starts a new
# expression. (Its `.kind` is root-anchored when the stage input is the
# document root; `.items[]|.kind` also matches, which is deliberate parity
# with the spaced `.items[] | .kind` the class already caught.) `(` is deliberately NOT in
# the class, so a bare `select(.kind == …)` — a Kubernetes object kind, not a
# driver — stays out; the grouping shape that costs is item 8 below. Flags no
# longer break the match either: round 2's `yq [^)]*` span was defeated by any
# `)` in between, e.g. `yq --arg d "$(date)" -r '.kind'`.
#
# WHAT THIS PATTERN CANNOT CATCH — the honest boundary, because a third false
# anchor dressed up as completeness would be worse than a named gap:
#
#   1. a yq call whose program sits on a DIFFERENT LINE (backslash
#      continuation, or a heredoc body), unless that program is also written as
#      a quoted assignment somewhere;
#   2. a program in which the word `kind` itself is interpolated —
#      `f=kind; yq -r ".${f}"`, `yq -r ".$(echo kind)"`;
#   3. a program assembled by printf/concatenation into a variable that is not
#      a single quoted literal — `q="."; q+="kind"`;
#   4. yq reached through an alias or a variable rather than the literal token
#      (`${YQ} -r '.kind'`, a `_yq()` wrapper) — Part A keys on `yq`;
#   5. a driver read that never uses yq at all: `grep '^kind:' | cut -d: -f2`,
#      jq over converted JSON, python;
#   6. a root `kind` reached indirectly — `.. | select(…)`, `to_entries`,
#      `with(.; .kind)`;
#   7. anything outside `.lok8s`, or in a file without a bash/argsh header
#      (see shell_sources in tests/test_helper.bash);
#   8. a grouped root read — `kind=$(yq -r '(.kind) // ""' …)` — because
#      admitting `(` to the delimiter class would also match
#      `select(.kind == …)`, a Kubernetes object kind (#143);
#   9. no-space operator siblings of the pipe shape whose leading char is not
#      in the class — `//.kind` (alternative), `,.kind` (comma sequence) —
#      and quoted/indirect spellings: `."kind"`, `getpath(["kind"])`,
#      `.[$k]`. Same trade as item 8: each candidate delimiter (`/`, `,`)
#      also appears mid-expression in non-driver programs.
#
# 1–3, 6, 8 and 9 are reachable by a determined author. None of them is the shape a
# fourteenth copy actually takes — every one of the thirteen #89 converged was
# a plain one-line quoted read — so the gate is aimed at drift, not at an
# adversary. The anti-vacuity canaries below are what keep it aimed at all.
#
# Comment lines are dropped: prose that quotes the pattern (utils/domain.sh's
# own docstring does) is not a read.
_kind_accessor_re() {
  printf '%s' '\.(spec|\[[^]]*spec[^]]*\])?\.?(\[[^]]*kind[^]]*\]|kind([^[:alnum:]_]|$))'
}

# Part A — the accessor handed to yq on the same line.
_kind_read_re() {
  local q=\'
  printf '%s' "yq.*[[:space:]\"${q}}|]$(_kind_accessor_re)"
}

# Part B — the accessor written into a variable, for a yq call further down.
_kind_query_re() {
  local q=\'
  printf '%s' "=[\"${q}|]$(_kind_accessor_re)"
}

_kind_reads() {
  local -a files=()
  mapfile -t files < <(shell_sources)
  # shell_sources has already said why on stderr. Returning early matters:
  # `grep` with no file arguments would read stdin and hang the suite.
  (( ${#files[@]} )) || return 1
  { grep -nE "$(_kind_read_re)"  "${files[@]}" 2>/dev/null
    grep -nE "$(_kind_query_re)" "${files[@]}" 2>/dev/null
  } | grep -vE ':[0-9]+:[[:space:]]*#' \
    | sed "s|^${_PROJECT_ROOT}/||" \
    | sort -u
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
.lok8s/utils/kapply.sh|yq -r 'select(.kind == "Deployment"
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
#   utils/kapply.sh   — `select(.kind == "Deployment" or .kind == …)` over a
#                       manifest stream: a Kubernetes object kind. It is listed
#                       because the widened accessor class matches the SECOND
#                       and third `.kind` in that select (each preceded by a
#                       space, not by `(`). The row is the price of catching
#                       `yq -r .kind` and `| .kind`, and it is a cheap one.

# Every spelling the sweep is claimed to cover, as a line it must match. This
# is the gate's real anti-vacuity check: it does not depend on any particular
# file still being written a particular way.
#
# Round 2's canary pinned the FILE (libs/hooks) rather than the FORM, so it
# only worked while libs/hooks:130 happened to be the sole quoted read in the
# tree. Re-spell that one line and the canary stays green while it stops
# measuring anything. These do not rot: each row IS the spelling.
_kind_read_canaries() {
  cat <<'CANARY'
single-quoted|  kind=$(yq -r '.kind // ""' "${spec}")
double-quoted|  kind=$(yq -r ".kind // \"\"" "${spec}")
quoted assignment|  kind="$(yq -r '.kind' "${spec}")"
bare program|  kind=$(yq -r .kind "${spec}")
flag with a subshell|  kind=$(yq --arg d "$(date)" -r '.kind' "${spec}")
bracket accessor|  kind=$(yq -r '.["kind"]' "${spec}")
escaped bracket accessor|  kind=$(yq -r ".[\"kind\"]" "${spec}")
spec-rooted|  kind=$(yq -r '.spec.kind' "${spec}")
spec-rooted bracket|  kind=$(yq -r '.spec["kind"]' "${spec}")
interpolated prefix|  kind=$(yq -r "${root}.kind" "${spec}")
pipe stage|  kind=$(yq -r "select(documentIndex == 0)|.kind" "${spec}")
program held in a variable|  local q='.kind'
CANARY
}

# ...and the spellings it must NOT match, so widening the accessor class does
# not quietly turn every Kubernetes object-kind read into an allowlist chore.
_kind_read_non_canaries() {
  cat <<'NOPE'
select over a manifest stream|  k=$(yq -r 'select(.kind == "Deployment")' "${f}")
a nested kind|  n=$(yq -r '.metadata.kind' "${f}")
a kind inside a list|  r=$(yq -r '[.kind, .metadata.name] | @tsv' "${f}")
NOPE
}

@test "the .kind sweep still recognises every spelling it claims to cover" {
  # ANTI-VACUITY for the two gates below, both of which assert an ABSENCE and
  # are therefore green when the pattern is broken.
  local re_a re_b label line
  re_a="$(_kind_read_re)"; re_b="$(_kind_query_re)"
  while IFS='|' read -r label line; do
    [[ -n "${label}" ]] || continue
    printf '%s\n' "${line}" | grep -qE "${re_a}|${re_b}" || {
      echo "the sweep no longer matches the ${label} form:" >&2
      echo "  ${line}" >&2
      echo "Part A: ${re_a}" >&2
      echo "Part B: ${re_b}" >&2
      return 1
    }
  done < <(_kind_read_canaries)

  while IFS='|' read -r label line; do
    [[ -n "${label}" ]] || continue
    if printf '%s\n' "${line}" | grep -qE "${re_a}|${re_b}"; then
      echo "the sweep now matches ${label}, which is a Kubernetes object kind" >&2
      echo "and not a driver read:" >&2
      echo "  ${line}" >&2
      return 1
    fi
  done < <(_kind_read_non_canaries)
}

@test "every remaining .kind read is either the shared reader or allowlisted" {
  local hits; hits="$(_kind_reads)"
  # ANTI-VACUITY: the sweep must find the reader itself. The canary test above
  # proves the PATTERN; this proves it is still being pointed at the tree.
  [[ "${hits}" == *"utils/domain.sh"* ]] || {
    echo "the sweep did not even find domain::spec_driver's own read — the" >&2
    echo "grep in _kind_reads is stale, not the code." >&2
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
  #
  # Built from the same accessor as the sweep above, so the two cannot drift —
  # this gate used to carry its own `yq [^)]*\.kind` copy and inherited every
  # evasion the sweep had.
  local re; re="$(_kind_read_re).*\| *tr "
  assert_pattern_matches "yq-into-tr gate" "${re}" \
    'd=$(yq -r ".kind" "${f}" | tr "[:upper:]" "[:lower:]")'
  # ...and that the file sweep still has a tree to look at. `|| true` below
  # would otherwise swallow shell_sources' own failure.
  shell_sources >/dev/null

  local hits
  hits=$(shell_sources | xargs grep -nE "${re}" 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a yq-into-tr driver read reappeared:" >&2
    echo "${hits}" >&2
    return 1
  }
}
