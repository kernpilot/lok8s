#!/usr/bin/env bats
# lo_node_version_test.bats — spec.kubernetes.version → the kind node image.
#
# Why this exists
# ---------------
# A Lo spec without spec.kubernetes.version (the minimal form in the spec
# reference) made `lo provision` run kind with `image: "kindest/node:null"`:
# the driver read the version with a raw `yq -r`, which prints `null` for a
# missing key (and `~` for a tilde null). kind failed with "manifest for
# kindest/node:null not found" — AFTER the driver had created the docker
# network and the registry containers.
#
# The contract, pinned here and in the Go twin
# (internal/driver/lo/nodeimage_test.go):
#   - unset (missing, null, ~, "", "null") → no image line in the kind config,
#     so kind uses the default node image of its own release;
#   - an image tag (any OCI tag, with an optional @sha256 digest; aliases and
#     merge keys resolved) → that image on every node;
#   - a shape that cannot be read, or a value that is not an image tag → an
#     error BEFORE any docker, kind or provider call, never "unset".
# The reader cases are shared with the Go twin: tests/fixtures/kind-node-version/.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export KIND_EXPERIMENTAL_DOCKER_NETWORK="lok8s-test"
  export DOMAIN_NAME="test.dev"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  CALLS="${BATS_TEST_TMPDIR}/calls"
  : > "${CALLS}"
}

teardown() { teardown_tmpdir; }

DIGEST="@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2"

# _spec <version line> — a Lo spec; an empty argument leaves the key out.
_spec() {
  {
    printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: lotest\nspec:\n  runtime: kind\n'
    if [[ -n "${1}" ]]; then printf '  kubernetes:\n    %s\n' "${1}"; fi
  } > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
}

# _load — the driver with every collaborator before and after the kind config
# stubbed to SUCCEED and to log its call. The kind config render is REAL.
# `kind create` copies its --config file to ${BATS_TEST_TMPDIR}/kind-config.
_load() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  :args() { domain="test.dev"; }
  export LOK8S_CP_COUNT=1 LOK8S_WORKER_COUNT=1 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID

  lo::read_config()      { echo "lo::read_config" >> "${CALLS}"; }
  lo::validate_ips()     { echo "lo::validate_ips" >> "${CALLS}"; }
  lo::network()          { echo "lo::network" >> "${CALLS}"; }
  lo::registry_network() { echo "lo::registry_network" >> "${CALLS}"; }
  registry::is_shared()  { return 1; }
  lo::registries_tls_cert()  { echo "lo::registries_tls_cert" >> "${CALLS}"; }
  lo::registries()           { echo "lo::registries" >> "${CALLS}"; }
  lo::write_certs_d()        { echo "lo::write_certs_d" >> "${CALLS}"; }
  lo::write_oidc_auth_config() { :; }
  lo::registries_tls_nudge() { :; }
  _lo_extract_kubeconfig()   { echo "${PATH_BASE}/.kubeconfig/lotest.yaml"; }
  lo::coredns()              { :; }
  lo::apply_local_registry_hosting() { :; }
  lo::registry_configmap()   { :; }
  kapply::run() { local phase="${1}" fn="${2}"; shift 2; "${fn}" "${@}"; }
  docker() { echo "docker $*" >> "${CALLS}"; }
  kind() {
    echo "kind $*" >> "${CALLS}"
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

# ── spec::kind_node_version ──────────────────────────────────────────────────
# The shared cases: tests/fixtures/kind-node-version/<verdict>--<case>.yaml, the
# expected answer in <case>.want ("unset", "tag <tag>" or "error <message>").
# The Go twin (TestNodeVersionFixtures) reads the same files, and
# TestNodeVersionMatchesBash runs this reader on each of them.

# _verdict <spec file> — the reader's answer in the .want format.
_verdict() {
  local out
  if out=$(spec::kind_node_version "${1}"); then
    if [[ -n "${out}" ]]; then printf 'tag %s\n' "${out}"; else echo unset; fi
  else
    printf 'error %s\n' "${out}"
  fi
}

@test "spec::kind_node_version: each shared fixture gets its expected answer" {
  local f want got n=0 bad=0
  for f in "${_PROJECT_ROOT}"/tests/fixtures/kind-node-version/*.yaml; do
    want=$(<"${f%.yaml}.want")
    got=$(_verdict "${f}" 2>"${BATS_TEST_TMPDIR}/stderr")
    if [[ "${got}" != "${want}" ]]; then
      printf '%s:\n  got  %s\n  want %s\n' "${f##*/}" "${got}" "${want}" >&2
      bad=$(( bad + 1 ))
    fi
    if [[ -s "${BATS_TEST_TMPDIR}/stderr" ]]; then
      printf '%s: the reader wrote to stderr:\n' "${f##*/}" >&2
      cat "${BATS_TEST_TMPDIR}/stderr" >&2
      bad=$(( bad + 1 ))
    fi
    n=$(( n + 1 ))
  done
  (( n >= 50 )) || { echo "only ${n} fixtures found" >&2; return 1; }
  (( bad == 0 ))
}

@test "spec::kind_node_version: a file that does not parse is an error with the reason, never unset" {
  local spec="${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  printf 'kind: Lo\nspec: [\n' > "${spec}"
  run spec::kind_node_version "${spec}"
  assert_failure
  assert_output --regexp '^spec\.kubernetes\.version cannot be read: .+\. Correct the spec file\.$'
}

# _yq_stub <body> — a yq on PATH that runs <body> (bash) and stays in front
# of the real one for the rest of the test.
_yq_stub() {
  mkdir -p "${BATS_TEST_TMPDIR}/stub"
  printf '#!/usr/bin/env bash\n%s\n' "${1}" > "${BATS_TEST_TMPDIR}/stub/yq"
  chmod +x "${BATS_TEST_TMPDIR}/stub/yq"
  export PATH="${BATS_TEST_TMPDIR}/stub:${PATH}"
}

@test "spec::kind_node_version: a yq without --yaml-fix-merge-anchor-to-spec names the yq version to install" {
  _spec "version: v1.31.4"
  _yq_stub 'echo "Error: unknown flag: --yaml-fix-merge-anchor-to-spec" >&2; exit 1'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.54.3 or later (b install installs the pinned yq)."
}

@test "spec::kind_node_version: a yq that panics on the reader names its version, not the spec file" {
  _spec "version: v1.31.4"
  # The panic of yq v4.52.5 on the reader's expressions (exit 2), from a yq
  # that reports another version: the message reads the version.
  _yq_stub 'if [[ "${1:-}" == --version ]]; then echo "yq (https://github.com/mikefarah/yq/) version v4.50.1"; exit 0; fi
printf "panic: runtime error: index out of range [-1]\n\ngoroutine 1 [running]:\n" >&2
exit 2'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: the installed yq (v4.50.1) failed on the version reader (panic: runtime error: index out of range [-1]). Install yq v4.54.3 or later (b install installs the pinned yq)."
}

@test "spec::kind_node_version: a nil pointer panic of yq (it prints [signal SIGSEGV) names the yq, not the memory limit" {
  _spec "version: v1.31.4"
  _yq_stub 'if [[ "${1:-}" == --version ]]; then echo "yq (https://github.com/mikefarah/yq/) version v4.50.1"; exit 0; fi
printf "panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x0]\n\ngoroutine 1 [running]:\n" >&2
exit 2'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: the installed yq (v4.50.1) failed on the version reader (panic: runtime error: invalid memory address or nil pointer dereference). Install yq v4.54.3 or later (b install installs the pinned yq)."
}

@test "spec::kind_node_version: a panicking yq whose --version fails is named as of an unknown version" {
  _spec "version: v1.31.4"
  _yq_stub 'if [[ "${1:-}" == --version ]]; then exit 1; fi
printf "panic: runtime error: index out of range [-1]\n" >&2
exit 2'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: the installed yq (version unknown) failed on the version reader (panic: runtime error: index out of range [-1]). Install yq v4.54.3 or later (b install installs the pinned yq)."
}

@test "spec::kind_node_version: a panicking pre-release of the minimum is named with its suffix, so the advice is not the yq it has" {
  _spec "version: v1.31.4"
  # v4.54.3-rc1 is older than v4.54.3. Without the suffix the message would
  # read "the installed yq (v4.54.3) ... Install yq v4.54.3 or later".
  _yq_stub 'if [[ "${1:-}" == --version ]]; then echo "yq (https://github.com/mikefarah/yq/) version v4.54.3-rc1"; exit 0; fi
printf "panic: runtime error: index out of range [-1]\n" >&2
exit 2'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: the installed yq (v4.54.3-rc1) failed on the version reader (panic: runtime error: index out of range [-1]). Install yq v4.54.3 or later (b install installs the pinned yq)."
}

@test "spec::kind_node_version: a read past the time limit is named, without timeout(1)" {
  _spec "version: v1.31.4"
  # The time limit needs no GNU timeout (macOS has none): a timeout(1) call
  # fails the test.
  timeout() { echo "timeout(1) was called" >&2; return 99; }
  export -f timeout
  _yq_stub 'exec sleep 5'
  _SPEC_KVER_TIMEOUT=1
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq did not finish in 1 s. Simplify the spec file."
}

@test "spec::kind_node_version: yq runs under the data memory limit (ulimit -d)" {
  _spec "version: v1.31.4"
  _yq_stub "ulimit -d > '${BATS_TEST_TMPDIR}/limit'; exit 1"
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  [ "$(cat "${BATS_TEST_TMPDIR}/limit")" = "262144" ] || {
    echo "yq ran with ulimit -d $(cat "${BATS_TEST_TMPDIR}/limit"), want 262144" >&2; return 1
  }
}

@test "spec::kind_node_version: a yq that runs out of memory is named" {
  _spec "version: v1.31.4"
  _yq_stub 'echo "fatal error: runtime: cannot allocate memory" >&2; exit 2'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq stopped at the memory limit of 256 MiB, or a signal stopped it. Simplify the spec file."
}

@test "spec::kind_node_version: a yq stopped by SIGSEGV is named" {
  _spec "version: v1.31.4"
  _yq_stub 'ulimit -c 0; kill -SEGV $$'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq stopped at the memory limit of 256 MiB, or a signal stopped it. Simplify the spec file."
}

# bash reports a job that a signal stopped on stderr ("line N: PID
# Segmentation fault ( ulimit -d ... )") at its next notify point: after a
# command, and after a DEBUG trap (bats sets one, with functrace). The read
# writes its own line for that cause, so its stderr stays empty. The
# watchdog delay (_SPEC_KVER_TEST_DELAY_WATCHDOG) holds the window open
# after the stub died. The read runs in a new bash, so the case does not
# depend on the traps of bats.
@test "spec::_kver_yq: bash puts no report of a yq that a signal stopped on the caller's stderr" {
  _spec "version: v1.31.4"
  local sig trace rc bad=0
  for sig in SEGV KILL; do
    _yq_stub 'ulimit -c 0; cd "'"${BATS_TEST_TMPDIR}"'" && kill -'"${sig}"' $$'
    for trace in none debug; do
      rc=0
      bash -c 'import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"; _SPEC_KVER_TEST_DELAY_WATCHDOG=1
        [[ "$3" == none ]] || { set -T; trap ":" DEBUG; }
        spec::_kver_yq "$2" -r .kind' \
        bash "${_PROJECT_ROOT}/.lok8s/utils" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" "${trace}" \
        > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
      if (( rc != 1 )) || [[ "$(<"${BATS_TEST_TMPDIR}/out")" != "spec.kubernetes.version cannot be read: yq stopped at the memory limit of 256 MiB, or a signal stopped it. Simplify the spec file." ]]; then
        echo "SIG${sig}, DEBUG trap ${trace}: status ${rc}, want 1, with: $(<"${BATS_TEST_TMPDIR}/out")" >&2; bad=1
      fi
      if [ -s "${BATS_TEST_TMPDIR}/err" ]; then
        echo "SIG${sig}, DEBUG trap ${trace}: the caller's stderr is not empty:" >&2
        cat "${BATS_TEST_TMPDIR}/err" >&2; bad=1
      fi
    done
  done
  if compgen -G "${BATS_TEST_TMPDIR}/core*" >/dev/null; then
    echo "a stub left a core file" >&2; bad=1
  fi
  (( bad == 0 ))
}

# With no free file descriptor for errfd (ulimit -n 10: errfd takes the
# lowest free one from 10 up), the read stops before yq and names the cause.
@test "spec::_kver_yq: no free file descriptor for errfd is a read error, and yq does not run" {
  _spec "version: v1.31.4"
  local rc=0
  _yq_stub 'echo ran > "'"${BATS_TEST_TMPDIR}"'/ran"; echo v1.31.4'
  bash -c 'import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"; ulimit -n 10; spec::_kver_yq "$2" -r .kind' \
    bash "${_PROJECT_ROOT}/.lok8s/utils" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" \
    > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -eq 1 ]
  [ "$(<"${BATS_TEST_TMPDIR}/out")" = "spec.kubernetes.version cannot be read: lo cannot open a file descriptor. Raise the open-file limit (ulimit -n)." ]
  [ ! -e "${BATS_TEST_TMPDIR}/ran" ]
}

@test "spec::kind_node_version: a yq that a signal stops after a panic line is named as stopped, not as a fault of the yq release" {
  _spec "version: v1.31.4"
  # A status above 128 is a signal (SIGKILL: 137), also when yq wrote a
  # "panic: " line before it. The panic message would advise another yq.
  _yq_stub 'if [[ "${1:-}" == --version ]]; then echo "yq (https://github.com/mikefarah/yq/) version v4.50.1"; exit 0; fi
printf "panic: runtime error: index out of range [-1]\n" >&2
kill -KILL $$'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq stopped at the memory limit of 256 MiB, or a signal stopped it. Simplify the spec file."
}

# _now_ms — the wall clock in milliseconds (bash 5 EPOCHREALTIME: no fork,
# and macOS date has no %N).
_now_ms() {
  local t="${EPOCHREALTIME/[.,]/}"
  echo $(( 10#${t} / 1000 ))
}

# _gone <pattern> — true when no process matches <pattern> (_pgrep_f of
# test_helper: the CI bats container has no pgrep) within 2 s. A process
# that a signal stops can stay visible for a moment. The 2 s are a time,
# not a count of scans: under bats, one scan of /proc can take more than
# a second on a host with many processes, and a process that runs on until
# a time limit must still run when the check ends. Status 2 (with a
# message): _pgrep_f failed, so nothing is known.
_gone() {
  local found end=$(( $(_now_ms) + 2000 ))
  while :; do
    found=$(_pgrep_f "${1}") || { echo "_gone: _pgrep_f failed for '${1}'" >&2; return 2; }
    [[ -n "${found}" ]] || return 0
    (( $(_now_ms) < end )) || return 1
    sleep 0.1
  done
}

# _kill_left <pattern> — stop the processes that a failed test left behind.
_kill_left() {
  local p
  for p in $(_pgrep_f "${1}"); do kill "${p}" 2>/dev/null || :; done
}

@test "spec::_kver_yq: the watchdog does not stop a read that ends before the time limit" {
  _spec "version: v1.31.4"
  local real
  real=$(command -v yq)
  # Each yq call takes 0.5 s, the limit is 3 s. A watchdog that does not wait
  # (a `read -t` on a process substitution on macOS ends at once) stops yq.
  _yq_stub "sleep 0.5; exec '${real}' \"\$@\""
  _SPEC_KVER_TIMEOUT=3
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_success
  assert_output "v1.31.4"
}

@test "spec::_kver_yq: a read that ends in time leaves no watchdog sleep behind" {
  _spec "version: v1.31.4"
  # A time limit that no other process uses, so _gone finds only this sleep.
  _SPEC_KVER_TIMEOUT=$(( 7000 + RANDOM % 1000 ))
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_success
  assert_output "v1.31.4"
  _gone "^sleep ${_SPEC_KVER_TIMEOUT}\$" || {
    _kill_left "^sleep ${_SPEC_KVER_TIMEOUT}\$"
    echo "the watchdog left 'sleep ${_SPEC_KVER_TIMEOUT}' running" >&2; return 1
  }
}

# The read's body dies (SIGKILL) while yq runs. The caller's $(...) must
# return then, not at the time limit: no job of the read may hold its pipe.
# The second case reads stderr in the $(...) too: while yq runs, the read
# keeps the caller's stderr in errfd, and no job may keep that copy.
@test "spec::_kver_yq: a SIGKILLed read does not hold the caller's \$(...) open until the time limit" {
  _spec "version: v1.31.4"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  local reads caller i yq body start took limit
  # A time limit that no other process uses, so _kill_left finds only this
  # watchdog's sleep.
  limit="8.$(printf '%04d' $(( RANDOM % 10000 )))"
  for reads in stdout stderr; do
    rm -f "${BATS_TEST_TMPDIR}/yq.pid" "${BATS_TEST_TMPDIR}/done"
    _yq_stub 'echo $$ > "'"${BATS_TEST_TMPDIR}"'/yq.pid"; exec sleep 30'
    # `|| :`: the killed body ends the $(...) with 137, and bats runs with set -e.
    if [[ "${reads}" == stdout ]]; then
      ( _SPEC_KVER_TIMEOUT=${limit}; out="$(spec::_kver_yq "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" -r .kind)" || :
        : "${out}"; echo done > "${BATS_TEST_TMPDIR}/done" ) &
    else
      ( _SPEC_KVER_TIMEOUT=${limit}; out="$(spec::_kver_yq "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" -r .kind 2>&1)" || :
        : "${out}"; echo done > "${BATS_TEST_TMPDIR}/done" ) &
    fi
    caller=$!
    for (( i = 0; i < 50; i++ )); do [ -s "${BATS_TEST_TMPDIR}/yq.pid" ] && break; sleep 0.1; done
    [ -s "${BATS_TEST_TMPDIR}/yq.pid" ]
    yq="$(cat "${BATS_TEST_TMPDIR}/yq.pid")"
    body="$(_ppid "${yq}")"
    [ -n "${body}" ]
    start=${SECONDS}
    kill -KILL "${body}"
    for (( i = 0; i < 60; i++ )); do [ -e "${BATS_TEST_TMPDIR}/done" ] && break; sleep 0.1; done
    took=$(( SECONDS - start ))
    kill -TERM "${yq}" 2>/dev/null || :
    _kill_left "^sleep ${limit//./\\.}\$"
    wait "${caller}" 2>/dev/null || :
    [ -e "${BATS_TEST_TMPDIR}/done" ] || { echo "${reads}: the caller's \$(...) did not return within 6 s" >&2; return 1; }
    (( took < 4 )) || { echo "${reads}: the caller's \$(...) returned after ${took} s" >&2; return 1; }
  done
}

# The CI bats container has no ps and no pgrep: a test that calls one sees
# "command not found", and a check built on it passes without checking. The
# /proc helpers of test_helper (_ppid, _children, _pgrep_f) call them only
# where /proc does not exist. The guard reads lines, not the shell: it
# misses a call in a quoted string (bash -c 'pgrep -f x'), a quoted name
# ("pgrep"), `ps ax` or a bare `ps`, a one-line @test, and a call in a
# heredoc line that starts with #. It finds none of these in the tree today.
@test "the unit tests call ps and pgrep only in the fallbacks of test_helper" {
  run grep -rnE "(^|[^[:alnum:]_.'-])((pgrep|pkill|pidof|killall)[[:space:]]+[^[:space:]]|ps[[:space:]]+(-|aux))" \
    "${_PROJECT_ROOT}/tests/unit" "${_PROJECT_ROOT}/tests/operator" "${_PROJECT_ROOT}/tests/lib" \
    "${_PROJECT_ROOT}/tests/test_helper.bash"
  # A comment or a test name can name the tools: only code lines count.
  local line text bad="" n=0 skip='^[[:space:]]*(#|@test[[:space:]])'
  while IFS= read -r line; do
    [[ -n "${line}" ]] || continue
    text="${line#*:*:}"
    [[ ! "${text}" =~ ${skip} ]] || continue
    if [[ "${line}" == "${_PROJECT_ROOT}/tests/test_helper.bash:"* ]]; then
      n=$(( n + 1 ))
    else
      bad+="${line}"$'\n'
    fi
  done <<< "${output}"
  [ -z "${bad}" ] || { echo "calls outside the fallbacks: ${bad}" >&2; return 1; }
  # The three fallbacks (_ppid, _children, _pgrep_f). A count of 0 means that
  # the grep itself failed.
  [ "${n}" -eq 3 ] || { echo "test_helper has ${n} calls, want 3: ${output}" >&2; return 1; }
}

# _pgrep_f must find a process, not only report none: every other caller
# checks only that a process is gone.
@test "_pgrep_f finds a running process by its command line" {
  local limit pid found="" i
  # A duration that no other process uses.
  limit=$(( 6000 + RANDOM % 1000 ))
  # 3>&-: a sleep left behind must not hold bats' output open.
  sleep "${limit}" 3>&- &
  pid=$!
  # Until the child has run exec, its command line is the shell's: wait for
  # it, as _gone waits for an end. The kill runs on every path.
  local rc=0
  for i in $(seq 1 20); do
    rc=0
    found=$(_pgrep_f "^sleep ${limit}\$") || rc=$?
    [[ -z "${found}" && "${rc}" -eq 0 ]] || break
    sleep 0.1
  done
  kill "${pid}" 2>/dev/null || :
  [ "${rc}" -eq 0 ] || { echo "_pgrep_f failed with status ${rc}" >&2; return 1; }
  [ "${found}" = "${pid}" ] || { echo "_pgrep_f gave '${found}', want '${pid}'" >&2; return 1; }
}

# A `read -t N` on a process substitution opened read-write (`<> <(:)`)
# waits on Linux only: on macOS it ends at once, and every read timed out.
@test "the bash tree has no read on a process substitution opened read-write" {
  run grep -rnE '<>[[:space:]]*<\(' "${_PROJECT_ROOT}/.lok8s" "${_PROJECT_ROOT}/internal/assets/lok8s"
  assert_failure 1
  assert_output ""
}

# An interrupt of the read (TERM, or INT from Ctrl-C) stops yq and the
# watchdog, and the EXIT trap removes the temporary files. The read runs
# outside `run`, so a yq or a sleep that the traps miss stays visible.
@test "spec::_kver_yq: an interrupt stops yq and the watchdog and removes the temporary files" {
  _spec "version: v1.31.4"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  local sig want rc limit stub_sleep bad=0
  for sig in TERM INT; do
    want=143
    [[ "${sig}" == INT ]] && want=130
    # Durations that no other process uses, so _gone finds only these.
    limit=$(( 8000 + RANDOM % 1000 ))
    stub_sleep=$(( 9000 + RANDOM % 1000 ))
    # The yq stub stops the read it runs in (its parent) with the signal, as
    # an interrupt of `lo` would, then waits: only a trap stops it sooner.
    _yq_stub "kill -${sig} \"\${PPID}\"; exec sleep ${stub_sleep}"
    rc=0
    _SPEC_KVER_TIMEOUT="${limit}" bash -c \
      'import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"; _SPEC_KVER_TIMEOUT=$3; spec::_kver_yq "$2" -r .kind' \
      bash "${_PROJECT_ROOT}/.lok8s/utils" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" "${limit}" \
      >"${BATS_TEST_TMPDIR}/out" 2>&1 || rc=$?
    if (( rc != want )); then
      echo "${sig}: status ${rc}, want ${want}" >&2; bad=1
    fi
    if ! _gone "^sleep ${stub_sleep}\$"; then
      echo "${sig}: the trap did not stop yq" >&2; bad=1
    fi
    if ! _gone "^sleep ${limit}\$"; then
      echo "${sig}: the trap did not stop the watchdog" >&2; bad=1
    fi
    if [[ -n "$(ls -A "${TMPDIR}")" ]]; then
      echo "${sig}: temporary files remain: $(ls -A "${TMPDIR}")" >&2; bad=1
    fi
    _kill_left "^sleep ${stub_sleep}\$"
    _kill_left "^sleep ${limit}\$"
    rm -rf "${TMPDIR:?}"/*
  done
  (( bad == 0 ))
}

# A signal that comes before spec::_kver_yq keeps the pid of yq, or of the
# watchdog, is only noted until it keeps both. Then it stops the read as at
# any other time, so neither runs on. A forced delay of 1.5 s holds each
# window open (_SPEC_KVER_TEST_DELAY_YQ, _WATCHDOG; empty in a normal run).
# The yq stub sends the signal to the read (its parent): at once in the yq
# window, after 0.5 s in the watchdog window. The Go reader has no such
# window: it reads the spec in its own process (internal/yqsem), with no yq.
@test "spec::_kver_yq: a signal before the read keeps the pids of yq and the watchdog stops both" {
  _spec "version: v1.31.4"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  local window sig want rc limit stub_sleep pre dyq dwd start took bad=0
  for window in yq watchdog; do
    for sig in INT TERM; do
      want=143
      [[ "${sig}" == INT ]] && want=130
      # A time limit (20.NNNN s) and a stub sleep that no other process
      # uses, so _gone finds only these. The limit also ends a read whose
      # stop does not run. It is long: a watchdog that runs on must still
      # run when the checks below end (each scan of /proc can take a second
      # or more).
      limit="20.$(printf '%04d' $(( RANDOM % 10000 )))"
      stub_sleep=$(( 9000 + RANDOM % 1000 ))
      if [[ "${window}" == yq ]]; then
        dyq=1.5 dwd="" pre=""
      else
        dyq="" dwd=1.5 pre="sleep 0.5; "
      fi
      _yq_stub "${pre}kill -${sig} \"\${PPID}\"; exec sleep ${stub_sleep}"
      rc=0
      start=$(_now_ms)
      bash -c \
        'import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"; _SPEC_KVER_TIMEOUT=$3 _SPEC_KVER_TEST_DELAY_YQ=$4 _SPEC_KVER_TEST_DELAY_WATCHDOG=$5; spec::_kver_yq "$2" -r .kind' \
        bash "${_PROJECT_ROOT}/.lok8s/utils" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" "${limit}" "${dyq}" "${dwd}" \
        >"${BATS_TEST_TMPDIR}/out" 2>&1 || rc=$?
      took=$(( $(_now_ms) - start ))
      if (( rc != want )); then
        echo "${window} window, ${sig}: status ${rc}, want ${want}: $(<"${BATS_TEST_TMPDIR}/out")" >&2; bad=1
      fi
      # The floor: the read ends after the delay (the signal waits for the
      # end of the delay's sleep), so the window was open. The ceiling is
      # far below the time limit: the stop, not the watchdog, ended the read.
      if (( took < 1400 || took > 8000 )); then
        echo "${window} window, ${sig}: the read took ${took} ms, want the delay of 1500 ms and the stop" >&2; bad=1
      fi
      if ! _gone "^sleep ${stub_sleep}\$"; then
        echo "${window} window, ${sig}: yq runs on after the read" >&2; bad=1
        _kill_left "^sleep ${stub_sleep}\$"
      fi
      if ! _gone "^sleep ${limit//./\\.}\$"; then
        echo "${window} window, ${sig}: the watchdog runs on after the read" >&2; bad=1
        _kill_left "^sleep ${limit//./\\.}\$"
      fi
      if [[ -n "$(ls -A "${TMPDIR}")" ]]; then
        echo "${window} window, ${sig}: temporary files remain: $(ls -A "${TMPDIR}")" >&2; bad=1
      fi
      rm -rf "${TMPDIR:?}"/*
    done
  done
  (( bad == 0 ))
}

# After the read's wait, yq is reaped, and its pid can be another
# process's: a stop sends no signal to a job of the read that has ended,
# waited for or not (spec::_running_job). A job that runs gets TERM, and the
# stop returns after the job ends. The stops run in a job of their own
# (set -m) with a time limit: a stop that does not return fails the test,
# and a KILL to the job's group leaves no process behind.
@test "spec::_kver_yq: a stop sends no signal to a job of the read that has ended" {
  local log="${BATS_TEST_TMPDIR}/kills" sub i
  : > "${log}"
  set -m
  (
    kill() { printf '%s\n' "$*" >> "${log}"; builtin kill "$@"; }
    sleep 0.1 3>&- &
    waited=$!
    sleep 0.1 3>&- &
    ended=$!
    wait "${waited}"
    # The shell reaps the other one when it ends, with no wait.
    sleep 0.5
    spec::_kver_stop "${waited}" "${ended}"
    [[ ! -s "${log}" ]] || { echo "a stop sent a signal to a job that had ended: $(<"${log}")"; exit 1; }
    sleep 5 3>&- &
    running=$!
    spec::_kver_stop "${running}" ""
    [[ "$(<"${log}")" == "-TERM ${running}" ]] || { echo "a stop did not send TERM to the running job ${running}: $(<"${log}")"; exit 1; }
    ! builtin kill -0 "${running}" 2>/dev/null || { echo "the stop returned while the job ${running} ran"; exit 1; }
  ) 3>&- &
  sub=$!
  set +m
  for (( i = 0; i < 100; i++ )); do kill -0 "${sub}" 2>/dev/null || break; sleep 0.1; done
  if kill -0 "${sub}" 2>/dev/null; then
    kill -KILL -- -"${sub}" 2>/dev/null || true
    wait "${sub}" 2>/dev/null || true
    echo "a stop did not return in 10 s; signals from the stops: $(tr '\n' ' ' < "${log}")"
    return 1
  fi
  wait "${sub}" || return 1
}

# A second signal during a stop does nothing: the stop that runs sends yq
# one TERM and returns after yq ends. Without that, bash runs a second stop
# inside the first (between its commands), and yq gets TERM twice; or the
# second signal ends the stop's wait, and the read returns while yq runs.
# The yq stub ends 1 s after its TERM. A helper of the stub sends two TERMs
# to the read (the stub's parent), 0.3 s apart.
@test "spec::_kver_yq: a second signal during a stop does not start a second stop" {
  _spec "version: v1.31.4"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  local log="${BATS_TEST_TMPDIR}/kills" m="${BATS_TEST_TMPDIR}/m" rc=0 yq n
  mkdir -p "${m}"
  : > "${log}"
  _yq_stub "echo \$\$ > '${m}/pid'
trap 'sleep 1; exit 1' TERM
( sleep 0.2; kill -TERM \"\${PPID}\"; sleep 0.3; kill -TERM \"\${PPID}\" ) &
while :; do sleep 0.1; done"
  bash -c \
    'log=$3; kill() { printf "%s\n" "$*" >> "${log}"; builtin kill "$@"; }; import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"; _SPEC_KVER_TIMEOUT=20; spec::_kver_yq "$2" -r .kind' \
    bash "${_PROJECT_ROOT}/.lok8s/utils" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" "${log}" \
    >"${BATS_TEST_TMPDIR}/out" 2>&1 || rc=$?
  yq=$(<"${m}/pid")
  if kill -0 "${yq}" 2>/dev/null; then
    kill -KILL "${yq}" 2>/dev/null || :
    echo "the read returned while yq (${yq}) ran; signals from the read: $(tr '\n' ' ' < "${log}")" >&2
    return 1
  fi
  [ "${rc}" -eq 143 ] || { echo "status ${rc}, want 143: $(<"${BATS_TEST_TMPDIR}/out")" >&2; return 1; }
  n=$(grep -cx -- "-TERM ${yq}" "${log}" || true)
  [ "${n}" = 1 ] || { echo "yq got TERM ${n} times from the read, want once: $(tr '\n' ' ' < "${log}")" >&2; return 1; }
  [ -z "$(ls -A "${TMPDIR}")" ] || { echo "temporary files remain: $(ls -A "${TMPDIR}")" >&2; return 1; }
}

@test "spec::kind_node_version: yq stderr stays out of the shape checks" {
  _spec "version: v1.31.4"
  local real
  real=$(command -v yq)
  _yq_stub "echo 'level=WARN msg=\"a warning\"' >&2; exec '${real}' \"\$@\""
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_success
  assert_output "v1.31.4"
}

# The `local LC_ALL=C` of spec::_is_image_tag only matters in a locale where
# bash matches 'é' with [A-Za-z] (en_US.UTF-8; not C.UTF-8). The Go twin runs
# every fixture in that locale (TestNodeVersionMatchesBash) and fails under
# CI without it; here the test skips when the locale is missing.
@test "spec::kind_node_version: the shared fixtures in the en_US.UTF-8 locale" {
  locale -a 2>/dev/null | grep -qiE '^en_US\.utf-?8$' || skip "no en_US.UTF-8 locale"
  local f want got bad=0
  for f in "${_PROJECT_ROOT}"/tests/fixtures/kind-node-version/error--non-ascii.yaml \
           "${_PROJECT_ROOT}"/tests/fixtures/kind-node-version/tag--plain.yaml; do
    want=$(<"${f%.yaml}.want")
    got=$(LC_ALL=en_US.UTF-8 _verdict "${f}")
    [[ "${got}" == "${want}" ]] || { printf '%s: got %s\n' "${f##*/}" "${got}" >&2; bad=1; }
  done
  (( bad == 0 ))
}

# ── lo::render_nodes ─────────────────────────────────────────────────────────

@test "lo::render_nodes: an unset version names no image on any node" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  export LOK8S_CP_COUNT=3 LOK8S_WORKER_COUNT=2 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  local v out
  for v in "" null; do
    out=$(lo::render_nodes "${v}" "")
    [ "$(grep -c 'role: ' <<<"${out}")" -eq 5 ] || { echo "want 5 nodes:" >&2; echo "${out}" >&2; return 1; }
    if grep -q 'image:' <<<"${out}"; then
      echo "version '${v}' still names an image:" >&2; echo "${out}" >&2; return 1
    fi
  done
}

@test "lo::render_nodes: a version names its image on every node" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  export LOK8S_CP_COUNT=3 LOK8S_WORKER_COUNT=2 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  local out
  out=$(lo::render_nodes "v1.35.0" "")
  [ "$(grep -c '^    image: "kindest/node:v1.35.0"$' <<<"${out}")" -eq 5 ] || {
    echo "want the image on all 5 nodes:" >&2; echo "${out}" >&2; return 1
  }
}

# ── driver::provision ────────────────────────────────────────────────────────

@test "lo provision: no version → kind create gets no --image and a config without an image" {
  local line
  for line in "" "version: null" "version: ~" 'version: "null"' 'version: ""'; do
    _spec "${line}"
    _load
    rm -f "${BATS_TEST_TMPDIR}/kind-config"
    : > "${CALLS}"

    local rc=0
    driver::provision test.dev || rc=$?
    [ "${rc}" -eq 0 ] || { echo "'${line}': provision failed (rc ${rc})" >&2; return 1; }
    grep -q '^kind create cluster ' "${CALLS}" || { echo "'${line}': kind create never ran" >&2; return 1; }
    if grep '^kind create' "${CALLS}" | grep -q -- '--image'; then
      echo "'${line}': kind create carries --image:" >&2; grep '^kind create' "${CALLS}" >&2; return 1
    fi
    # The image check below reads the captured config: it must be the real
    # cluster config (one control plane, one worker), not an empty capture.
    grep '^kind create' "${CALLS}" | grep -q -- ' --config ' || {
      echo "'${line}': kind create carries no --config:" >&2; grep '^kind create' "${CALLS}" >&2; return 1
    }
    if ! grep -q '^kind: Cluster$' "${BATS_TEST_TMPDIR}/kind-config" \
      || [ "$(grep -c '^  - role: ' "${BATS_TEST_TMPDIR}/kind-config")" -ne 2 ]; then
      echo "'${line}': the captured kind config is not the cluster config:" >&2
      cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    fi
    if grep -q 'image:' "${BATS_TEST_TMPDIR}/kind-config"; then
      echo "'${line}': the kind config names an image:" >&2; cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    fi
  done
}

@test "lo provision: a given version → that image on every node" {
  local line want
  # <version line, \n for a line break>;<the tag of the node image>
  while IFS=";" read -r line want; do
    _spec "${line//\\n/$'\n'}"
    _load
    rm -f "${BATS_TEST_TMPDIR}/kind-config"

    local rc=0
    driver::provision test.dev || rc=$?
    [ "${rc}" -eq 0 ] || { echo "'${line}': provision failed (rc ${rc})" >&2; return 1; }
    [ "$(grep -c "^    image: \"kindest/node:${want}\"$" "${BATS_TEST_TMPDIR}/kind-config")" -eq 2 ] || {
      echo "'${line}': want kindest/node:${want} on both nodes:" >&2; cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    }
  done <<VERSIONS
version: v1.35.0;v1.35.0
version: v1.31.12${DIGEST};v1.31.12${DIGEST}
version: latest;latest
<<: {version: v1.31.4};v1.31.4
version: |\n      v1.31.4;v1.31.4
VERSIONS
}

@test "lo provision: a version that cannot be read or is not a tag fails before any network, container or VM" {
  local name want
  for name in error--image-reference error--version-is-a-map error--kubernetes-is-a-list error--merge-of-a-scalar error--merge-cycle-under-kubernetes error--nul-byte; do
    cp "${_PROJECT_ROOT}/tests/fixtures/kind-node-version/${name}.yaml" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
    want=$(<"${_PROJECT_ROOT}/tests/fixtures/kind-node-version/${name}.want")
    want="[error] ${want#error }"
    _load
    export PROVIDER_NAME="hetzner"
    provider::provision() { echo "provider::provision" >> "${CALLS}"; }
    lo::provision_remote() { echo "lo::provision_remote" >> "${CALLS}"; }
    : > "${CALLS}"

    run driver::provision test.dev
    [ "${status}" -ne 0 ] || { echo "${name}: provision accepted it" >&2; return 1; }
    grep -qxF -- "${want}" <<<"${output}" || {
      echo "${name}: want the line '${want}':" >&2; echo "${output}" >&2; return 1
    }
    if grep -qE '^\[error\] *$' <<<"${output}"; then
      echo "${name}: an empty [error] line:" >&2; echo "${output}" >&2; return 1
    fi
    [ ! -s "${CALLS}" ] || {
      echo "${name}: steps ran before the version was rejected:" >&2; cat "${CALLS}" >&2; return 1
    }
    unset PROVIDER_NAME
  done
}

@test "lo::export_spec_envs: the literal string null exports an empty version" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  oidc::load_spec() { :; }
  local line want
  while IFS='|' read -r line want; do
    _spec "${line}"
    lo::export_spec_envs "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" >/dev/null 2>&1 || true
    [ "${LOK8S_SPEC_KUBERNETES_VERSION}" == "${want}" ] || {
      echo "'${line}': LOK8S_SPEC_KUBERNETES_VERSION='${LOK8S_SPEC_KUBERNETES_VERSION}', want '${want}'" >&2; return 1
    }
  done <<'EOF'
version: "null"|
version: ~|
|
version: v1.35.0|v1.35.0
EOF
}

# ── the run header of lo up ──────────────────────────────────────────────────
# Go twin: TestRunHeaderShapes (internal/cli/cmd_up_test.go).

@test "_lo_header: a spec file with a second document shows no driver and no version" {
  eval "$(sed -n '/^_lo_header()/,/^}/p' "${_PROJECT_ROOT}/.lok8s/lo")"
  declare -f _lo_header >/dev/null || { echo "could not extract _lo_header from .lok8s/lo" >&2; return 1; }
  export KUBECONFIG="${PATH_BASE}/.kubeconfig/lotest.yaml"
  _spec "version: v1.31.4"
  run _lo_header test.dev
  assert_success
  assert_line --index 0 $'  \033[1;36mtest.dev\033[0m  \033[2mlo · kind · v1.31.4\033[0m'
  printf -- '---\nkind: KubeOne\nspec:\n  kubernetes:\n    version: v1.20.0\n' >> "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  run _lo_header test.dev
  assert_success
  assert_line --index 0 $'  \033[1;36mtest.dev\033[0m  \033[2m\033[0m'
  refute_output --partial "registries"
}
