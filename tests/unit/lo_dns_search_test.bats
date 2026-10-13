#!/usr/bin/env bats
# lo_dns_search_test.bats — networking.dnsSearch in the kind config and the
# spec.network.dnsSearch reader. The reason for the key is at
# lo::render_kind_config (.lok8s/drivers/lo/utils/render.sh).
#
# The bash render is compared byte for byte with the goldens the Go tests use
# (internal/driver/lo/testdata/kindconfig_*.golden), so both implementations
# render the same kind config. The reader's error lines are pinned with the
# same text as the Go twin. Go twin: internal/driver/lo/dnssearch_test.go.

setup() {
  load "../test_helper"
  setup_tmpdir
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  # The golden harness (internal/driver/lo/render_test.go: goldenDriver).
  export PATH_CLUSTERS="/test/clusters" DOMAIN_NAME="test.lok8s.dev" LOK8S_MAX_CONCURRENT_DOWNLOADS=3
  export LOK8S_CP_COUNT=1 LOK8S_WORKER_COUNT=0 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID LOK8S_NETWORK_DNS_SEARCH
  GOLDEN="${_PROJECT_ROOT}/internal/driver/lo/testdata"
}

teardown() { teardown_tmpdir; }

# _same_as_golden <golden> [spec] — the bash render equals the golden file.
_same_as_golden() {
  diff -u "${GOLDEN}/${1}" <(lo::render_kind_config test-shared v1.31.4 lok8s "${2:-}") || {
    echo "the bash render differs from ${1} (the Go render's golden)" >&2
    return 1
  }
}

# _dns_search <file> [name…] — networking.dnsSearch is a LIST that holds
# exactly the given names (none: the empty list). A missing key or a null
# keeps the host's search domains: kind passes KIND_DNS_SEARCH only for a
# set list.
_dns_search() {
  local file="${1}"; shift
  local want got
  want="$(printf '%s\n' "${@}")"
  got="$(yq -r '.networking.dnsSearch[]' "${file}")"
  [ "$(yq -r '.networking.dnsSearch | tag' "${file}")" == '!!seq' ] && [ "${got}" == "${want}" ] || {
    echo "networking.dnsSearch is not the list [${*}]:" >&2; cat "${file}" >&2; return 1
  }
}

# _spec <spec-body> — a cluster spec under the test dir; prints its path.
_spec() {
  printf 'kind: Lo\nmetadata:\n  name: dns\nspec:\n%s' "${1}" > "${BATS_TEST_TMPDIR}/spec.yaml"
  echo "${BATS_TEST_TMPDIR}/spec.yaml"
}

# _stub_tools — docker, kind and kubectl FIRST on PATH: each logs its argv and
# fails. A provision test calls _no_tool_ran at its end, so a real tool can
# never touch the host's live clusters, and a call that a stub catches fails
# the test.
_stub_tools() {
  local tool
  mkdir -p "${BATS_TEST_TMPDIR}/stub-bin"
  for tool in docker kind kubectl; do
    printf '#!/usr/bin/env bash\necho "%s $*" >> "%s"\nexit 97\n' \
      "${tool}" "${BATS_TEST_TMPDIR}/tools.log" > "${BATS_TEST_TMPDIR}/stub-bin/${tool}"
    chmod +x "${BATS_TEST_TMPDIR}/stub-bin/${tool}"
  done
  export PATH="${BATS_TEST_TMPDIR}/stub-bin:${PATH}"
}

_no_tool_ran() {
  [ ! -s "${BATS_TEST_TMPDIR}/tools.log" ] || {
    echo "a tool on PATH ran (only the function stubs may answer):" >&2
    cat "${BATS_TEST_TMPDIR}/tools.log" >&2
    return 1
  }
}

@test "render: the default config equals the golden and drops the host search domains" {
  _same_as_golden kindconfig_default.golden
  lo::render_kind_config test-shared v1.31.4 lok8s "" > "${BATS_TEST_TMPDIR}/kind.yaml"
  _dns_search "${BATS_TEST_TMPDIR}/kind.yaml"
}

@test "render: host ports, multinode, OIDC and extra mounts equal their goldens" {
  LOK8S_HOST_PORTS=true _same_as_golden kindconfig_hostports.golden
  LOK8S_CP_COUNT=3 LOK8S_WORKER_COUNT=2 _same_as_golden kindconfig_multinode.golden
  LOK8S_HOST_PORTS=true LOK8S_SPEC_OIDC_ISSUER="https://id.kubehz.dev" LOK8S_SPEC_OIDC_CLIENTID="kubectl-client" \
    _same_as_golden kindconfig_oidc.golden
  cat > "${BATS_TEST_TMPDIR}/mounts.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-shared
spec:
  nodes:
    extraMounts:
      - hostPath: /src/app
        containerPath: /app
        readOnly: true
      - hostPath: /data
        containerPath: /data
YAML
  LOK8S_EXTRA_MOUNTS_COUNT=2 _same_as_golden kindconfig_extramounts.golden "${BATS_TEST_TMPDIR}/mounts.yaml"
}

@test "render: spec.network.dnsSearch replaces the empty list (the shared golden)" {
  export LOK8S_NETWORK_DNS_SEARCH="corp.example.com example.ts.net"
  _same_as_golden kindconfig_dnssearch.golden
  lo::render_kind_config test-shared v1.31.4 lok8s "" > "${BATS_TEST_TMPDIR}/kind.yaml"
  _dns_search "${BATS_TEST_TMPDIR}/kind.yaml" corp.example.com example.ts.net
}

@test "read_dns_search_config: no key, a null, [] and a non-map spec.network export the empty list" {
  local body
  for body in '  cluster:\n    domain: lok8s.dev\n' '  network: lok8s\n' '  network:\n    name: n\n' \
    '  network:\n    dnsSearch: ~\n' '  network:\n    dnsSearch:\n' '  network:\n    dnsSearch: []\n'; do
    export LOK8S_NETWORK_DNS_SEARCH="stale.example"
    # shellcheck disable=SC2059 # the bodies carry \n escapes on purpose
    lo::read_dns_search_config "$(_spec "$(printf "${body}")")" 2> "${BATS_TEST_TMPDIR}/err"
    [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "body ${body}:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
    [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ] || {
      echo "body ${body}: exported '${LOK8S_NETWORK_DNS_SEARCH}', want ''" >&2; return 1
    }
  done
}

@test "read_dns_search_config: DNS names are exported as written, space-separated" {
  local label63 name253
  label63="$(printf 'a%.0s' {1..63})"
  name253="${label63}.${label63}.${label63}.$(printf 'b%.0s' {1..61})"
  [ "${#name253}" -eq 253 ]
  lo::read_dns_search_config "$(_spec "  network:
    dnsSearch:
      - corp.example.com
      - \"fritz.box\"
      - 'lan'
      - 1and1.example
      - a-b.example
      - xn--bcher-kva.example
      - Corp.Example.com
      - corp.example.com.
      - ${label63}.example
      - ${name253}
      - ${name253}.
")"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "corp.example.com fritz.box lan 1and1.example a-b.example xn--bcher-kva.example Corp.Example.com corp.example.com. ${label63}.example ${name253} ${name253}." ]
}

@test "read_dns_search_config: aliases read like their value (an anchored list, aliased entries, an anchored spec.network)" {
  printf 'kind: Lo\nshared: &s [corp.example.com, x.example]\nspec:\n  network:\n    dnsSearch: *s\n' > "${BATS_TEST_TMPDIR}/a.yaml"
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/a.yaml"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "corp.example.com x.example" ]

  printf 'kind: Lo\none: &o corp.example.com\nspec:\n  network:\n    dnsSearch: [*o, y.example, *o]\n' > "${BATS_TEST_TMPDIR}/b.yaml"
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/b.yaml"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "corp.example.com y.example corp.example.com" ]

  printf 'kind: Lo\nnet: &n\n  name: x\n  dnsSearch: [a.example]\nspec:\n  network: *n\n' > "${BATS_TEST_TMPDIR}/c.yaml"
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/c.yaml"
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "a.example" ]
}

# _read_quiet <file> <want-list> — the read exports <want-list>, prints nothing
# on stderr (no yq merge-key WARN line either) and takes under 5 s.
_read_quiet() {
  local start=${SECONDS}
  lo::read_dns_search_config "${1}" 2> "${BATS_TEST_TMPDIR}/err" || { echo "read failed:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  (( SECONDS - start < 5 )) || { echo "read took $(( SECONDS - start )) s" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "stderr is not empty:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "${2}" ] || { echo "exported '${LOK8S_NETWORK_DNS_SEARCH}', want '${2}'" >&2; return 1; }
}

@test "read_dns_search_config: nested aliases and merge keys resolve, the YAML spec way (Go: yqsem.LookupMerged)" {
  printf 'kind: Lo\none: &one corp.example.com\ninner: &inner [*one, two.example]\nnet: &net {name: x, dnsSearch: *inner}\nspec:\n  network: *net\n' > "${BATS_TEST_TMPDIR}/nested.yaml"
  _read_quiet "${BATS_TEST_TMPDIR}/nested.yaml" "corp.example.com two.example"
  printf 'kind: Lo\nbase: &base {dnsSearch: [m.example]}\nspec:\n  network:\n    <<: *base\n    name: x\n' > "${BATS_TEST_TMPDIR}/merge.yaml"
  _read_quiet "${BATS_TEST_TMPDIR}/merge.yaml" "m.example"
  printf 'kind: Lo\nbase: &base {dnsSearch: [m.example]}\nspec:\n  network:\n    dnsSearch: [local.example]\n    <<: *base\n' > "${BATS_TEST_TMPDIR}/local.yaml"
  _read_quiet "${BATS_TEST_TMPDIR}/local.yaml" "local.example"
  printf 'kind: Lo\na: &a {dnsSearch: [a.example]}\nb: &b {dnsSearch: [b.example]}\nspec:\n  network: {<<: [*a, *b]}\n' > "${BATS_TEST_TMPDIR}/first.yaml"
  _read_quiet "${BATS_TEST_TMPDIR}/first.yaml" "a.example"
  printf 'kind: Lo\ns: &s {network: {dnsSearch: [s.example]}}\nspec:\n  <<: *s\n' > "${BATS_TEST_TMPDIR}/spec.yaml"
  _read_quiet "${BATS_TEST_TMPDIR}/spec.yaml" "s.example"
}

@test "read_dns_search_config: merge keys, alias cycles and a large anchor tree outside spec.network cost nothing" {
  # A self-merging map and a 9^9-entry anchor tree: a whole-document
  # explode(.) runs out of memory on either (bounded here by dnssearch::_read).
  {
    printf 'kind: Lo\nbase: &base {x: 1}\nother: {<<: *base, y: 2}\ncyc: &cyc [x.example, *cyc]\nmore: *cyc\nselfm: &selfm {<<: *selfm}\n'
    printf 'l0: &l0 [x.example, x.example, x.example, x.example, x.example, x.example, x.example, x.example, x.example]\n'
    local i
    for i in 1 2 3 4 5 6 7 8; do
      printf 'l%d: &l%d [*l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d]\n' "${i}" "${i}" $(( i - 1 )) $(( i - 1 )) $(( i - 1 )) $(( i - 1 )) $(( i - 1 )) $(( i - 1 )) $(( i - 1 )) $(( i - 1 )) $(( i - 1 ))
    done
    printf 'big: *l8\nspec:\n  network:\n    dnsSearch: [a.example]\n'
  } > "${BATS_TEST_TMPDIR}/outside.yaml"
  _read_quiet "${BATS_TEST_TMPDIR}/outside.yaml" "a.example"
}

@test "read_dns_search_config: an alias cycle inside the list resolves one level (the entry is a list)" {
  printf 'kind: Lo\ncyc: &cyc [x.example, *cyc]\nspec:\n  network:\n    dnsSearch: *cyc\n' > "${BATS_TEST_TMPDIR}/cycle.yaml"
  local rc=0 start=${SECONDS}
  lo::read_dns_search_config "${BATS_TEST_TMPDIR}/cycle.yaml" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ]
  (( SECONDS - start < 5 ))
  [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "error: spec.network.dnsSearch[1] must be a DNS name. Found a list. Write the entry as text, for example corp.example.com." ] || {
    cat "${BATS_TEST_TMPDIR}/err" >&2; return 1
  }
}

# _slow_yq <script-body> — a yq stub first on PATH (yqbound::run runs the yq
# BINARY in a background subshell, so a function stub would not reach it).
_slow_yq() {
  mkdir -p "${BATS_TEST_TMPDIR}/yq-bin"
  printf '#!/usr/bin/env bash\n%s\n' "${1}" > "${BATS_TEST_TMPDIR}/yq-bin/yq"
  chmod +x "${BATS_TEST_TMPDIR}/yq-bin/yq"
  PATH="${BATS_TEST_TMPDIR}/yq-bin:${PATH}"
}

# The limit cases of yqbound::run itself are in tests/unit/yqbound_test.bats;
# these pin the problem line each one becomes.
# _limit_line <stub> <want> — with the yq stub, the read fails with exactly
# the line <want> (one cause, one next action).
_limit_line() {
  ( _slow_yq "${1}"
    local rc=0
    lo::read_dns_search_config "${LIMIT_SPEC}" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
    [ "${rc}" -ne 0 ] && [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "${2}" ] ) || {
    echo "stub: ${1}" >&2; echo "want: ${2}" >&2; echo "got:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1
  }
}

@test "read_dns_search_config: a yq call at a limit is one error line per cause, not a hang" {
  LIMIT_SPEC="$(_spec "  network:
    dnsSearch: [a.example]
")"
  local prefix="error: spec.network.dnsSearch could not be read:"
  DNSSEARCH_YQ_SECONDS=1
  _limit_line 'exec sleep 30' \
    "${prefix} yq did not finish in 1 s. Write spec.network.dnsSearch without YAML aliases."
  _limit_line 'echo "fatal error: runtime: out of memory" >&2; echo "goroutine 1 [running]:" >&2; exit 2' \
    "${prefix} yq stopped at the memory limit of 1024 MiB. Write spec.network.dnsSearch without YAML aliases."
  _limit_line 'ulimit -c 0; cd "'"${BATS_TEST_TMPDIR}"'" && kill -SEGV $$' \
    "${prefix} the signal SIGSEGV stopped yq. Run the command again."
  # An old yq (before v4.47.1): the version it reports is in the line.
  _limit_line 'if [[ "${1:-}" == --version ]]; then echo "yq (https://github.com/mikefarah/yq/) version v4.46.1"; exit 0; fi
echo "Error: unknown flag: --yaml-fix-merge-anchor-to-spec" >&2; echo "Usage:" >&2; exit 1' \
    "${prefix} the installed yq (v4.46.1) does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.47.1 or later (lo toolchain install, or b install, installs the pinned yq)."
  ( TMPDIR="${BATS_TEST_TMPDIR}/no-such-dir"
    _limit_line 'echo unused' "${prefix} the temporary directory ${BATS_TEST_TMPDIR}/no-such-dir does not exist. Set TMPDIR to a directory that exists." )
  mkdir -p "${BATS_TEST_TMPDIR}/read-only"
  chmod 555 "${BATS_TEST_TMPDIR}/read-only"
  if [[ ! -w "${BATS_TEST_TMPDIR}/read-only" ]]; then  # root can write anyway
    ( TMPDIR="${BATS_TEST_TMPDIR}/read-only"
      _limit_line 'echo unused' "${prefix} the temporary directory ${BATS_TEST_TMPDIR}/read-only is not writable. Set TMPDIR to a writable directory." )
  fi
  chmod 755 "${BATS_TEST_TMPDIR}/read-only"
  # The data-memory cap is set on the yq process itself (DNSSEARCH_YQ_KIB).
  ( _slow_yq 'echo "data memory: $(ulimit -d)" >&2; exit 1'
    lo::read_dns_search_config "${LIMIT_SPEC}" 2> "${BATS_TEST_TMPDIR}/err"
    [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "data memory: 1048576" ] ) || {
    echo "ulimit:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1
  }
  # A yq error of its own (not a limit) reaches stderr as it is, and the
  # read finds no list, like the Go twin on an unparsable file.
  ( _slow_yq 'echo "Error: bad file: yaml: line 3: mapping values are not allowed" >&2; exit 1'
    local rc=0
    lo::read_dns_search_config "${LIMIT_SPEC}" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
    [ "${rc}" -eq 0 ] && [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ] &&
      [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "Error: bad file: yaml: line 3: mapping values are not allowed" ] ) || {
    echo "own error:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1
  }
}

@test "dnssearch::_stop: an exit status with no own cause is still a failure with its number" {
  local rc=0
  dnssearch::_stop 130 || rc=$?
  [ "${rc}" -eq 1 ]
  [ "${#DNSSEARCH_PROBLEMS[@]}" -eq 1 ]
  [ "${DNSSEARCH_PROBLEMS[0]}" == "spec.network.dnsSearch could not be read: yq stopped with the exit status 130. Run the command again." ]
  rc=0
  dnssearch::_stop 1 || rc=$?
  [ "${rc}" -eq 0 ] && [ "${#DNSSEARCH_PROBLEMS[@]}" -eq 0 ]
}

# LOK8S_TEST_BASH: a bash < 4.4 to run the set -u case in (there an empty
# "${a[@]}" is an unbound variable); the default bash runs it too.
@test "read_dns_search_config under set -u: no list, an empty list and a list export without an unbound variable" {
  local sh="${LOK8S_TEST_BASH:-bash}" body
  for body in '  cluster: {domain: lok8s.dev}\n' '  network:\n    dnsSearch: []\n' '  network:\n    dnsSearch: [a.example, b.example]\n' '  network:\n    dnsSearch: [bad_entry]\n'; do
    # shellcheck disable=SC2059 # the bodies carry \n escapes on purpose
    printf "kind: Lo\nspec:\n${body}" > "${BATS_TEST_TMPDIR}/u.yaml"
    run "${sh}" -uc 'import() { :; }
      source "$1/utils/verbose.sh"; source "$1/utils/yqbound.sh"; source "$1/utils/dnssearch.sh"; source "$1/drivers/lo/utils/config.sh"
      rc=0; lo::read_dns_search_config "$2" || rc=$?
      echo "rc=${rc} list=[${LOK8S_NETWORK_DNS_SEARCH}]"' _ "${_PROJECT_ROOT}/.lok8s" "${BATS_TEST_TMPDIR}/u.yaml"
    [ "${status}" -eq 0 ] || { echo "${sh}: status ${status}: ${output}" >&2; return 1; }
    case "${body}" in
      *a.example*) [ "${lines[-1]}" == "rc=0 list=[a.example b.example]" ] ;;
      *bad_entry*) [ "${lines[-1]}" == "rc=1 list=[]" ] ;;
      *) [ "${lines[-1]}" == "rc=0 list=[]" ] ;;
    esac || { echo "${sh}: ${output}" >&2; return 1; }
    if [[ "${output}" == *"unbound variable"* ]]; then echo "${sh}: ${output}" >&2; return 1; fi
  done
}

@test "the dnsSearch arrays are expanded with the + guard (bash < 4.4 under set -u)" {
  local hits
  hits="$(grep -nE '(^|[^+])"\$\{DNSSEARCH_(NAMES|PROBLEMS)\[@\]\}"' \
    "${_PROJECT_ROOT}/.lok8s/utils/dnssearch.sh" "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/config.sh" \
    "${_PROJECT_ROOT}/.lok8s/libs/lint" || true)"
  [ -z "${hits}" ] || { echo "an unguarded expansion:" >&2; echo "${hits}" >&2; return 1; }
}

@test "read_dns_search_config: 32 entries pass, more fail with the count to remove" {
  local n body i
  for n in 32 33 34; do
    body="  network:
    dnsSearch:
"
    for (( i = 0; i < n; i++ )); do body+="      - d${i}.example
"; done
    local rc=0 want=""
    lo::read_dns_search_config "$(_spec "${body}")" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
    case "${n}" in
      33) want="error: spec.network.dnsSearch has 33 entries. The limit is 32. Remove 1 entry." ;;
      34) want="error: spec.network.dnsSearch has 34 entries. The limit is 32. Remove 2 entries." ;;
    esac
    if [[ -z "${want}" ]]; then
      [ "${rc}" -eq 0 ] || { echo "32 entries rejected:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
      [ "$(wc -w <<< "${LOK8S_NETWORK_DNS_SEARCH}")" -eq 32 ] || { echo "32 entries: exported '${LOK8S_NETWORK_DNS_SEARCH}'" >&2; return 1; }
    else
      [ "${rc}" -ne 0 ] || { echo "${n} entries accepted" >&2; return 1; }
      [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "${want}" ] || { echo "${n} entries:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
    fi
  done
}

@test "read_dns_search_config: a value that is not a list fails, the value in plain words" {
  local value observed
  for value in 'corp.example.com|the text '"'"'corp.example.com'"'"'' '{a: b}|a map' 'true|the boolean true' '5|the number 5'; do
    observed="${value#*|}"; value="${value%%|*}"
    local rc=0
    lo::read_dns_search_config "$(_spec "  network:
    dnsSearch: ${value}
")" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
    [ "${rc}" -ne 0 ]
    [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "error: spec.network.dnsSearch must be a list of DNS names. Found ${observed}. Write a YAML list, for example [corp.example.com]." ] || {
      echo "dnsSearch: ${value}:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1
    }
    [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ]
  done
}

@test "read_dns_search_config: every bad entry gets the line of its cause (the Go golden, byte for byte)" {
  # The shared fixture holds one entry per failure cause, an aliased entry,
  # a "…\n" string and a | block scalar (a newline the value keeps).
  local rc=0
  lo::read_dns_search_config "${GOLDEN}/dnssearch_bad.yaml" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ]
  [ "${LOK8S_NETWORK_DNS_SEARCH}" == "" ]
  diff -u "${GOLDEN}/dnssearch_bad.golden" "${BATS_TEST_TMPDIR}/err"
}

@test "read_config and driver::export stop at a failed read (errexit off, as provision runs them)" {
  local spec rc
  spec="$(_spec "  network:
    dnsSearch: [corp_example]
")"
  lo::read_network_config() { return 0; }
  lo::read_node_config()    { return 0; }
  lo::read_lb_config()      { return 0; }
  rc=0; lo::read_config "${spec}" 2>/dev/null || rc=$?
  [ "${rc}" -ne 0 ] || { echo "read_config returned 0 for a rejected dnsSearch" >&2; return 1; }

  # A failed EARLIER reader stops the read too: the dnsSearch reader runs
  # last, and its success must not become the verdict.
  lo::read_dns_search_config() { return 0; }
  lo::read_network_config() { return 1; }
  rc=0; lo::read_config "${spec}" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "read_config returned 0 after a failed network read" >&2; return 1; }
  lo::read_network_config() { return 0; }
  lo::read_node_config() { return 1; }
  rc=0; lo::read_config "${spec}" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "read_config returned 0 after a failed node read" >&2; return 1; }

  # driver::export: a failed read must not hand over to export_spec_envs.
  :args() { domain="test.dev"; }
  lo::read_config() { return 1; }
  lo::export_spec_envs() { echo "export_spec_envs ran" >&2; return 0; }
  rc=0; driver::export test.dev 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "driver::export returned 0 after a failed read" >&2; return 1; }
  if grep -q "export_spec_envs ran" "${BATS_TEST_TMPDIR}/err"; then
    echo "driver::export went on to export_spec_envs after a failed read" >&2; return 1
  fi
}

# _provision_stubs — every collaborator of driver::provision stubbed to
# SUCCEED, except the render and the DNS search reader (the real ones).
# `kind create` copies its config file to ${BATS_TEST_TMPDIR}/kind-config.
_provision_stubs() {
  _stub_tools
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters" PATH_BASE="${BATS_TEST_TMPDIR}/base"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: lotest\nspec:\n  runtime: kind\n  kubernetes:\n    version: v1.31.4\n%s' \
    "${1:-}" > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  :args() { domain="test.dev"; }
  # The real reader for spec.network.dnsSearch; the network/registry readers
  # would write .registries.json, which this test does not need.
  lo::read_config() { lo::read_dns_search_config "${1}"; }
  lo::validate_ips() { :; }
  lo::network() { :; }
  registry::is_shared() { return 1; }
  lo::registries_tls_cert() { :; }
  lo::registries() { :; }
  lo::write_certs_d() { :; }
  lo::write_oidc_auth_config() { :; }
  lo::registries_tls_nudge() { :; }
  _lo_extract_kubeconfig() { echo "${PATH_BASE}/.kubeconfig/lotest.yaml"; }
  # The node-ip heal gate runs `docker network inspect`; no node has a
  # registry NIC here.
  lo::nodes_on_registry_network() { return 1; }
  lo::coredns() { :; }
  lo::apply_local_registry_hosting() { :; }
  lo::registry_configmap() { :; }
  kapply::run() { local phase="${1}" fn="${2}"; shift 2; "${fn}" "${@}"; }
  kind() {
    if [[ "${1:-} ${2:-}" == "create cluster" ]]; then
      local prev="" arg
      for arg in "${@}"; do
        [[ "${prev}" == "--config" ]] && cat "${arg}" > "${BATS_TEST_TMPDIR}/kind-config"
        prev="${arg}"
      done
    fi
    return 0
  }
}

@test "lo provision: kind create gets a config without the host search domains" {
  _provision_stubs
  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -eq 0 ]
  [ -s "${BATS_TEST_TMPDIR}/kind-config" ] || { echo "kind create got no config" >&2; return 1; }
  _dns_search "${BATS_TEST_TMPDIR}/kind-config"
  _no_tool_ran
}

@test "lo provision: kind create gets the spec.network.dnsSearch list" {
  _provision_stubs "  network:
    dnsSearch: [corp.example.com, example.ts.net]
"
  local rc=0
  driver::provision test.dev || rc=$?
  [ "${rc}" -eq 0 ]
  _dns_search "${BATS_TEST_TMPDIR}/kind-config" corp.example.com example.ts.net
  _no_tool_ran
}

@test "lo provision: a bad dnsSearch entry stops before kind create" {
  _provision_stubs "  network:
    dnsSearch: [corp_example]
"
  lo::network() { echo "lo::network ran" >&2; }
  local rc=0
  driver::provision test.dev 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ] || { echo "driver::provision returned 0 for a rejected dnsSearch" >&2; return 1; }
  grep -qF "error: spec.network.dnsSearch[0] has a character that is not valid in a DNS name: 'corp_example'." "${BATS_TEST_TMPDIR}/err"
  if grep -q "lo::network ran" "${BATS_TEST_TMPDIR}/err"; then
    echo "the provision went on to the docker network after the rejected read" >&2; return 1
  fi
  [ ! -e "${BATS_TEST_TMPDIR}/kind-config" ]
  _no_tool_ran
}
