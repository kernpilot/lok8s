#!/usr/bin/env bats
# curl_user_config_test.bats — each curl call of the bash tree reads no
# config file of the user, and each call that carries a credential gets no
# TLS log or trace control.
#
# Unless -q is its FIRST argument, curl reads one config file of the user:
# $CURL_HOME/.curlrc, else $XDG_CONFIG_HOME/curlrc, else $HOME/.curlrc. A
# trace-ascii line there logs each header (the bearer, the Basic
# credential) and each answer, an output line writes the answer to another
# file. SSLKEYLOGFILE and the GnuTLS and NSS debug variables in the
# environment make curl or its TLS library write the session keys or a
# trace. So each call starts with `curl -q` and runs in a subshell after
# http::tls_log_vars_unset (utils/http.sh).
#
# Two kinds of check:
#   - the source gate (tests/lib/curl_gate.bash): every curl call in the
#     bash tree passes -q first, and every one outside the manifests runs
#     as the first command after http::tls_log_vars_unset in its subshell,
#     except the calls of the gate's allowlist (no credential, each named);
#   - for each caller family, the real curl against a local server
#     (tests/lib/http_stub.bash, 127.0.0.1 only): with a hostile config file
#     in each place curl looks, no trace or output file appears and the
#     result is the same as without one; with the TLS log and trace
#     variables set, curl gets none of them and writes no key log, and a
#     readonly one stops the call before curl runs, with a line that names
#     it.
# The list of the TLS variables is _HTTP_TLS_LOG_VARS of utils/http.sh: the
# tests read it there. The agent CronJob script has its cases in
# kubehz_heartbeat_test.bats: it runs in its own image, without
# utils/http.sh, and clears the same names itself.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  # argsh `:args` stub: `--name value` pairs become variables.
  :args() {
    shift
    while (( $# )); do
      case "$1" in
        --*)
          local _n="${1#--}"
          _n="${_n//-/_}"
          shift
          printf -v "${_n}" '%s' "${1:-}"
          ;;
      esac
      shift || true
    done
  }
  export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/tests/lib/curl_gate.bash"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}/d.example" "${BATS_TEST_TMPDIR}/home"
  # No config file, proxy or TLS variable of the host reaches a test.
  export HOME="${BATS_TEST_TMPDIR}/home"
  unset CURL_HOME XDG_CONFIG_HOME CURL_CA_BUNDLE SSL_CERT_FILE SSL_CERT_DIR \
    http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY no_proxy NO_PROXY \
    "${_HTTP_TLS_LOG_VARS[@]}"

  # One value for each credential.
  local hex
  hex=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  export KUBEHZ_TOKEN="khzt_rc_${hex}" HCLOUD_TOKEN="hcloud-rc-${hex}" KKP_TOKEN="kkp-rc-${hex}"
  export KUBEHZ_AGENT_BEARER="agent-rc-${hex}"
  export HROBOT_USER="robot-user" HROBOT_PASSWORD="robot-pw-${hex}"
  export KUBEHZ_AGENT_CLIENT_ID="client-rc" KUBEHZ_AGENT_CLIENT_SECRET="client-secret-${hex}"
  export AGENT_TOKEN_IN_CLUSTER="khz_agt_rc_${hex}"
  export STUB_BEARER="${KUBEHZ_TOKEN} ${HCLOUD_TOKEN} ${KKP_TOKEN} ${KUBEHZ_AGENT_BEARER}"
  # A synthetic bind secret (64 hex, as the api mints it): the register
  # bodies carry it, and the stub logs only whether it arrived.
  export STUB_BIND_SECRET="${hex}${hex}${hex}${hex}"
  SECRETS=("${KUBEHZ_TOKEN}" "${HCLOUD_TOKEN}" "${KKP_TOKEN}" "${KUBEHZ_AGENT_BEARER}"
    "${HROBOT_PASSWORD}" "${KUBEHZ_AGENT_CLIENT_SECRET}" "${AGENT_TOKEN_IN_CLUSTER}" "${STUB_BIND_SECRET}")
}

teardown() {
  http_stub_stop
  teardown_tmpdir
}

# ── the source gate ──────────────────────────────────────

# The detection itself: the curl calls that the gate finds in the tree. The
# count is the number of calls today, and the second count the number of
# them with a credential (a new call raises one: then give it -q and the
# helper, and a family case below when it carries a credential). A broken
# pattern would list fewer calls and let the gates below pass on nothing.
@test "curl gate: the detection finds each curl call in the bash tree" {
  local -a files=()
  mapfile -t files < <(curl_gate_files)
  run curl_gate list "${files[@]}"
  assert_success
  local f
  for f in libs/kubehz/main libs/kubehz/hosted libs/kubehz/agent libs/kubehz/shared libs/kubehz/handover \
    drivers/kubeone/config drivers/kkp/api providers/hetzner/main libs/kubehz/manifests/agent/cronjob.yaml \
    libs/image drivers/lo/libs/registry; do
    assert_output --partial "/.lok8s/${f}:"
  done
  [ "$(grep -c "/.lok8s/libs/kubehz/main:[0-9]* credential$" <<<"${output}")" -eq 14 ]
  [ "$(grep -c "/.lok8s/libs/kubehz/hosted:[0-9]* credential$" <<<"${output}")" -eq 4 ]
  [ "$(grep -c "/.lok8s/drivers/kubeone/config:[0-9]* credential$" <<<"${output}")" -eq 2 ]
  [ "$(grep -c "/.lok8s/libs/kubehz/manifests/agent/cronjob.yaml:[0-9]* credential$" <<<"${output}")" -eq 4 ]
  [ "$(grep -c "/.lok8s/providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh:[0-9]*$" <<<"${output}")" -eq 6 ]
  [ "$(grep -c "/.lok8s/libs/image:[0-9]*$" <<<"${output}")" -eq 2 ]
  [ "$(grep -c ' credential$' <<<"${output}")" -eq 28 ]
  [ "${#lines[@]}" -eq 40 ]
}

@test "curl gate: each curl call in the bash tree passes -q first" {
  local -a files=()
  mapfile -t files < <(curl_gate_files)
  run curl_gate q "${files[@]}"
  assert_success
  assert_output ""
}

# The agent CronJob runs its script in its own image, without utils/http.sh:
# the gate leaves the files under manifests/ out of this rule, and the
# script clears the same names itself (the drift case below). Each other
# exception is an entry of the allowlist, which must name exactly one call
# without a credential.
@test "curl gate: each curl call outside the manifests runs after http::tls_log_vars_unset, or is in the allowlist" {
  local -a files=()
  mapfile -t files < <(curl_gate_files)
  run curl_gate tls "${files[@]}"
  assert_success
  assert_output ""
  [ "${#CURL_GATE_ALLOW[@]}" -eq 11 ]
}

# The discovery: curl_gate_files gives every file of the tree, so the
# scanner sees a file whose only call is a split name (no word curl in it).
# The fixture tree also holds a file without curl and the two parts that
# discovery leaves out. A floor above the count of files fails loudly.
@test "curl gate: discovery gives each file of the tree, so a split name is found" {
  local root="${BATS_TEST_TMPDIR}/tree"
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  mkdir -p "${root}/libs" "${root}/init.d" "${root}/providers/hetzner/cloud-init"
  printf '%s\n' 'cu\' 'rl -u "${U}:${P}" "${url}"' > "${root}/libs/split"
  printf '%s\n' 'echo hello' > "${root}/libs/plain"
  printf '%s\n' 'curl -sS https://h.example/i' > "${root}/init.d/scaffold.ts"
  printf '%s\n' 'curl' > "${root}/providers/hetzner/cloud-init/packages"
  run curl_gate_files "${root}" 2
  assert_success
  assert_output "${root}/libs/plain
${root}/libs/split"
  local -a files=()
  mapfile -t files < <(curl_gate_files "${root}" 2)
  CURL_GATE_ALLOW=()
  run curl_gate q "${files[@]}"
  assert_failure 1
  assert_output "${root}/libs/split:1: ${q}"
  run curl_gate tls "${files[@]}"
  assert_failure 1
  assert_output "${root}/libs/split:1: ${t}"
  run curl_gate list "${files[@]}"
  assert_success
  assert_output "${root}/libs/split:1 credential"
  run curl_gate_files "${root}" 3
  assert_failure 1
  assert_output "curl_gate_files found only 2 file(s) under ${root}: the discovery is broken, not the tree"
}

# The anti-vacuity half: each canary breaks a rule in one way, and the gate
# names its file and line. The compliant forms give no line, and so do the
# forms that are not a curl call: in quotes, in a comment, or on a line
# that opts out of the backstop with "# curl-gate: not a call". The
# allowlist of the tree is cleared here: its entries name files that this
# run does not read.
@test "curl gate: each canary breaks the rule it shows, and the compliant forms pass" {
  local bad="${BATS_TEST_TMPDIR}/bad.sh" good="${BATS_TEST_TMPDIR}/good.sh"
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
curl -K - https://h.example/a
x=$(printf '%s' "${c}" | curl -sS --config - https://h.example/b)
curl -sS https://h.example/c
args=(-u "${U}:${P}")
curl "${args[@]}" "${url}"
c=curl
"${c}" -K - https://h.example/d
timeout 5 curl -K - https://h.example/e
FOO=1 curl -sS https://h.example/f
\curl -sS https://h.example/g
/usr/bin/curl -sS https://h.example/h
command curl -K - https://h.example/i
env -u X curl -sS https://h.example/j
printf '%s\n' https://h.example/k | xargs curl -sS
exec curl -sS https://h.example/l
local tool="/usr/bin/curl"
"${CURL:-curl}" -sS https://h.example/m
      - curl -fsSL https://h.example/n
true | http::tls_log_vars_unset && curl -q -K - "${url}"
( http::tls_log_vars_unset; curl -q -K - "${url}" )
http::tls_log_vars_unset && ( curl -q -K - "${url}" )
( true && http::tls_log_vars_unset && curl -q -K - "${url}" )
x=$( http::tls_log_vars_unset && curl -q -K - "${url}" )
( http::tls_log_vars_unset || true && curl -q -K - "${url}" )
( http::tls_log_vars_unset && foo | curl -q -sS "${url}" )
cu\
rl -K - https://h.example/o
eval 'curl -u "${U}:${P}" "${url}"'
eval "${c} -sS https://h.example/p"
( http::tls_log_vars_unset && env SSLKEYLOGFILE=/tmp/k curl -q -K - "${url}" )
( http::tls_log_vars_unset && SSLKEYLOGFILE=/tmp/k curl -q -K - "${url}" )
( http::tls_log_vars_unset && command curl -q -sS https://h.example/n6 )
( http::tls_log_vars_unset && exec curl -q -K - "${url}" )
( http::tls_log_vars_unset && sudo curl -q -K - "${url}" )
( http::tls_log_vars_unset && timeout -s KILL 5 curl -q -K - "${url}" )
( http::tls_log_vars_unset && nice curl -q -K - "${url}" )
( http::tls_log_vars_unset && umask 077 && umask 022 && curl -q -K - "${url}" )
SSLKEYLOGFILE="/tmp/my keys" curl -K - https://h.example/r1
SSLKEYLOGFILE='/tmp/keys' curl -K - https://h.example/r2
SSLKEYLOGFILE= curl -K - https://h.example/r3
SSLKEYLOGFILE=$(printf '%s' /tmp/k) curl -K - https://h.example/r4
( http::tls_log_vars_unset && SSLKEYLOGFILE="/tmp/keys" curl -q -K - "${url}" )
( http::tls_log_vars_unset && SSLKEYLOGFILE='/tmp/keys' curl -q -K - "${url}" )
( http::tls_log_vars_unset && SSLKEYLOGFILE= curl -q -K - "${url}" )
( http::tls_log_vars_unset && SSLKEYLOGFILE=$(printf '%s' /tmp/k) curl -q -K - "${url}" )
sudo -u "${user}" curl -K - https://h.example/r5
cl=$(command -v curl)
local w="$(which curl)"
t=`type -P curl`
eval "${cl} -sS https://h.example/r6"
SH
  cat > "${good}" <<'SH'
# curl -K - https://h.example/n1
echo "  curl -X POST https://h.example/n2 -H 'Authorization: Bearer <token>' \\"
echo "heartbeat failed (curl rc=${rc}); retrying" >&2
local -a curl_args=(
  --config -
)
error "a value for the curl option holds a control character"
command -v curl >/dev/null # curl-gate: not a call
apt-get -y install jq curl gnupg # curl-gate: not a call
x=$(http::curl_config header "Authorization: Bearer ${T}" | (
  http::tls_log_vars_unset &&
    curl -q -K - -fsSL \
      https://h.example/n4
) ) || x=""
( http::tls_log_vars_unset && umask 077 && curl -q -sS -u "${U}:${P}" https://h.example/n5 )
( http::tls_log_vars_unset && timeout 5 curl -q -sS https://h.example/n7 )
if out=$(printf '%s' "${b}" | ( http::tls_log_vars_unset && curl -q -sS --data-binary @- https://h.example/n8 )); then :; fi
( http::tls_log_vars_unset && umask 077 && timeout 30 curl -q -sS https://h.example/n9 )
( http::tls_log_vars_unset && cu\
rl -q -sS https://h.example/n10 )
eval "${saved_traps}"
x=$(echo curl is a word, not a call) # curl-gate: not a call
printf '%s\n' "${a}" | tr -d '\n' | ( http::tls_log_vars_unset && curl -q -sS https://h.example/n11 )
y="$(echo curl is a word here too)" # curl-gate: not a call
SH
  local q="a curl call: -q is not its first argument"
  local v="curl named through a variable: the gate cannot check that call"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local e="an eval of a command that holds curl: the gate cannot check that call"
  local ev="an eval of a variable that names curl: the gate cannot check that call"
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${bad}:1: ${q}
${bad}:2: ${q}
${bad}:3: ${q}
${bad}:5: ${q}
${bad}:6: ${v}
${bad}:8: ${q}
${bad}:9: ${q}
${bad}:10: ${q}
${bad}:11: ${q}
${bad}:12: ${q}
${bad}:13: ${q}
${bad}:14: ${q}
${bad}:15: ${q}
${bad}:16: ${v}
${bad}:17: ${v}
${bad}:18: ${q}
${bad}:26: ${q}
${bad}:28: ${e}
${bad}:38: ${q}
${bad}:39: ${q}
${bad}:40: ${q}
${bad}:41: ${q}
${bad}:46: ${q}
${bad}:47: ${v}
${bad}:48: ${v}
${bad}:49: ${v}
${bad}:29: ${ev}
${bad}:50: ${ev}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${bad}:1: ${t}
${bad}:2: ${t}
${bad}:3: ${t}
${bad}:5: ${t}
${bad}:6: ${v}
${bad}:8: ${t}
${bad}:9: ${t}
${bad}:10: ${t}
${bad}:11: ${t}
${bad}:12: ${t}
${bad}:13: ${t}
${bad}:14: ${t}
${bad}:15: ${t}
${bad}:16: ${v}
${bad}:17: ${v}
${bad}:18: ${t}
${bad}:19: ${t}
${bad}:20: ${t}
${bad}:21: ${t}
${bad}:22: ${t}
${bad}:23: ${t}
${bad}:24: ${t}
${bad}:25: ${t}
${bad}:26: ${t}
${bad}:28: ${e}
${bad}:30: ${t}
${bad}:31: ${t}
${bad}:32: ${t}
${bad}:33: ${t}
${bad}:34: ${t}
${bad}:35: ${t}
${bad}:36: ${t}
${bad}:37: ${t}
${bad}:38: ${t}
${bad}:39: ${t}
${bad}:40: ${t}
${bad}:41: ${t}
${bad}:42: ${t}
${bad}:43: ${t}
${bad}:44: ${t}
${bad}:45: ${t}
${bad}:46: ${t}
${bad}:47: ${v}
${bad}:48: ${v}
${bad}:49: ${v}
${bad}:29: ${ev}
${bad}:50: ${ev}"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate tls "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_output "${good}:12 credential
${good}:15 credential
${good}:16
${good}:17 credential
${good}:18
${good}:19
${good}:23"
  run curl_gate x "${good}"
  assert_failure 2
}

# A shell operator ends a word: in "echo x=y; curl …" the word before the
# ; is x=y, not the assignment word "x=y;" that the wrapper step dropped (it
# then saw "echo curl", a curl that is no command, and both modes passed).
# Each canary is a credential call in both modes. In lines 1 to 9, a word
# comes before the assignment, and no space comes before the operator. In
# line 10, the assignment comes just after the ( of a subshell. In lines 11
# to 13, an operator in a ${ } is no operator (X=${a//;/} stays one
# assignment word), and the ${ } ends at its first }, so the ; after it
# ends the word. In line 14, a ${ with no } before the curl skips nothing:
# the X=1 in its $( ) is an assignment word, as before. The controls keep
# their verdicts: a real assignment prefix (also with an operator in
# quotes) is still a call with -q, a quoted operator is no operator, and
# the & of a redirection (>&x=y, a file) ends no word, so the curl after
# it stays an argument (its line opts out of the backstop).
@test "curl gate: a shell operator ends a word, so an assignment before it hides no curl call" {
  local bad="${BATS_TEST_TMPDIR}/ops-bad.sh" good="${BATS_TEST_TMPDIR}/ops-good.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
echo x=y; curl -u "${U}:${P}" "${url}"
echo x=y&& curl -u "${U}:${P}" "${url}"
echo x=y|| curl -u "${U}:${P}" "${url}"
echo x=y|curl -u "${U}:${P}" "${url}"
echo x=y& curl -u "${U}:${P}" "${url}"
(echo x=y; curl -u "${U}:${P}" "${url}")
out=$(echo x=y; curl -u "${U}:${P}" "${url}")
(echo x=y)&& curl -u "${U}:${P}" "${url}"
{ echo x=y;}&& curl -u "${U}:${P}" "${url}"
(X=1 curl -u "${U}:${P}" "${url}")
X=${a//;/} curl -u "${U}:${P}" "${url}"
X=${a//&/+} curl -u "${U}:${P}" "${url}"
echo X=${x:-a}; curl -u "${U}:${P}" "${url}"
echo ${y:-$(X=1 curl -u "${U}:${P}" "${url}")}
SH
  cat > "${good}" <<'SH'
X=1 curl -q -sS https://h.example/c1
X="a;b" curl -q -sS https://h.example/c2
X='a|b' Y="c&&d" curl -q -sS https://h.example/c3
echo "x=y; curl -u ${U}:${P} ${url}"
echo 'x=y&& curl -u u:p https://h.example/c4'
( http::tls_log_vars_unset && curl -q -sS "https://h.example/c5?a=1;b=2" )
x=$(X=1 curl -q -sS https://h.example/c6)
echo x=y 2>&1 | curl -q -sS https://h.example/c7
echo a >&x=y curl -sS https://h.example/c8 # curl-gate: not a call
SH
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local want_q="" want_t="" want_l=""
  for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14; do
    want_q+="${bad}:${n}: ${q}"$'\n'
    want_t+="${bad}:${n}: ${t}"$'\n'
    want_l+="${bad}:${n} credential"$'\n'
  done
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${want_q%$'\n'}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${want_t%$'\n'}"
  run curl_gate list "${bad}"
  assert_success
  assert_output "${want_l%$'\n'}"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_success
  assert_output "${good}:1
${good}:2
${good}:3
${good}:6
${good}:7
${good}:8"
  # The assignment prefixes are outside the TLS form (as before), the
  # helper form with a quoted ; is not.
  run curl_gate tls "${good}"
  assert_failure 1
  assert_output "${good}:1: ${t}
${good}:2: ${t}
${good}:3: ${t}
${good}:7: ${t}
${good}:8: ${t}"
}

# A ${ } nests: a ${ in it opens one more, quotes in it are quotes, and
# only its own } ends it. A $( ) or a `…` in a ${ } or in double quotes is
# a command, and the gate finds a call there as at the top level. A call
# also starts at curl and a redirection (curl</dev/null). Each canary is a
# credential call in both modes. The controls: curl in the text of a ${ }
# is no call, and a redirection is no argument, so a -q after it is the
# first argument.
@test "curl gate: an expansion nests, a command in it is a command, and a call starts at curl and a redirection" {
  local bad="${BATS_TEST_TMPDIR}/nest-bad.sh" good="${BATS_TEST_TMPDIR}/nest-good.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
X=${a:-${b};} curl -u "${U}:${P}" "${url}"
echo ${y:-$(X=${z} curl -u "${U}:${P}" "${url}")}
echo ${y:-`curl -u "${U}:${P}" "${url}"`}
echo "x `curl -u u:p https://h.example/t4`"
X=${a:-"}"} curl -u "${U}:${P}" "${url}"
curl</dev/null -u "${U}:${P}" "${url}"
curl>/tmp/out -u "${U}:${P}" "${url}"
SH
  cat > "${good}" <<'SH'
echo ${a:-${b};curl -u u:p https://h.example/n1}
echo ${a:-see curl docs}
curl</dev/null -q -sS https://h.example/n3
curl 2>/dev/null -q -sS https://h.example/n4
SH
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local want_q="" want_t="" want_l=""
  for n in 1 2 3 4 5 6 7; do
    want_q+="${bad}:${n}: ${q}"$'\n'
    want_t+="${bad}:${n}: ${t}"$'\n'
    want_l+="${bad}:${n} credential"$'\n'
  done
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${want_q%$'\n'}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${want_t%$'\n'}"
  run curl_gate list "${bad}"
  assert_success
  assert_output "${want_l%$'\n'}"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_success
  assert_output "${good}:3
${good}:4"
  run curl_gate tls "${good}"
  assert_failure 1
  assert_output "${good}:3: ${t}
${good}:4: ${t}"
}

# The backstop: a curl word in code that the gate cannot place as a call
# is a finding in the q and tls modes. The canaries: a case arm, a
# redirection before the command (also before \curl and a path), a lookup,
# an argument, and six lines that do not opt out: more text after the
# marker, no space after its #, other text of the same length, no white
# space before its #, the marker text in a string before a comment of
# the same length, and a marker in a string that goes on to the next
# line (no comment). A line opts out only with the trailing comment
# "# curl-gate: not a call" after white space. The controls hold no curl
# word in code: in quotes, in a comment, in a ${ }, in a longer word, and
# on a line that opts out.
@test "curl gate: a curl word that the gate cannot place is a finding, unless its line opts out" {
  local bad="${BATS_TEST_TMPDIR}/word-bad.sh" good="${BATS_TEST_TMPDIR}/word-good.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
case "${x}" in a) curl -u "${U}:${P}" "${url}" ;; esac
2>&1 curl -u "${U}:${P}" "${url}"
2>/dev/null \curl -u "${U}:${P}" "${url}"
2>/dev/null /usr/bin/curl -u "${U}:${P}" "${url}"
command -v curl >/dev/null
x=$(echo curl is a word, not a call)
cmd -x curl # curl-gate: not a call here
cmd -x curl #curl-gate: not a call
cmd -x curl # curl-gate: not a CALL
cmd -x curl;# curl-gate: not a call
echo "# curl-gate: not a call" curl # curl-gate: not a CALL
echo curl "x # curl-gate: not a call
y"
SH
  cat > "${good}" <<'SH'
echo "curl -u ${U}:${P} ${url}"
# curl -u "${U}:${P}" "${url}"
cmd # curl -u "${U}:${P}" "${url}"
echo ${a:-see curl docs}
curl_gate q x.sh
source lib/curl.sh
cmd -x curl # curl-gate: not a call
SH
  local c="a curl word the gate cannot place" want=""
  for n in 1 2 3 4 5 6 7 8 9 10 11 12; do
    want+="${bad}:${n}: ${c}"$'\n'
  done
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${want%$'\n'}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${want%$'\n'}"
  run curl_gate list "${bad}"
  assert_success
  assert_output ""
  local m
  for m in q tls list; do
    run curl_gate "${m}" "${good}"
    assert_success
    assert_output ""
  done
}

# The state of a line goes on at the next line: a quote that is still
# open (the lines are one command), a $( ) that is still open (the next
# command starts in it), and a here-document (its body is no command).
# The calls (q): a single quote in a ${ } in double quotes is text (line
# 1), a quote that a line before opened ends at its ' (line 3), a call
# after the end of a "$( )" over more lines (lines 8 and 9), and a call
# after a body (lines 20 and 26), after a shift in (( )) (line 22). The
# findings that the gate cannot place: a curl word in a string over more
# lines, also on a line that starts with # (line 5); a curl word in an
# unquoted body, where the shell runs the $( ) (lines 11 and 14, a split
# name); a curl word in quotes on a line that starts in a $( ) of a line
# before (line 28: the ) of the case arm ends the $( ) for the gate). In
# amb.sh, a single quote in a ${ } in double quotes comes before the curl
# word in quotes: in its default mode bash reads that quote as a quote. In
# open.sh, a quoted body has no end in the file. The controls: a quoted
# body is text (also with <<-), libcurl and curl_config are no curl words,
# and the calls after a string or a $( ) over more lines are calls.
@test "curl gate: a quote, a \$( ) and a here-document over more lines keep their state, and a line that the gate cannot read is a finding" {
  local bad="${BATS_TEST_TMPDIR}/multi-bad.sh" amb="${BATS_TEST_TMPDIR}/amb.sh"
  local open="${BATS_TEST_TMPDIR}/open.sh" good="${BATS_TEST_TMPDIR}/multi-good.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
X="${a:-'}" curl -u u:p https://h.example/a1
x='
'; curl -u u:p https://h.example/a2
echo "a
# curl -u u:p https://h.example/a3
b"
x="$(
  printf '%s' "${y}" | curl -u u:p https://h.example/a4
)"; curl -u u:p https://h.example/a5
cat <<EOF
'$(curl -u u:p https://h.example/a6)'
EOF
cat <<EOF
$(cu\
rl -u u:p https://h.example/a7)
EOF
cat <<'EOF'
x
EOF
curl -u u:p https://h.example/a8
y=$(( 1 << n ))
curl -u u:p https://h.example/a9
cat <<EOF &&
body
EOF
curl -u u:p https://h.example/a10
z="$(f
case "${y}" in a) curl -u u:p https://h.example/a11 ;; esac
)"
SH
  printf '%s\n' 'X="${a:-'"'"'"'"'"'}" curl -u u:p https://h.example/b1' > "${amb}"
  printf '%s\n' "cat <<'EOF'" 'curl -u u:p https://h.example/c1' 'EOF-not' > "${open}"
  cat > "${good}" <<'SH'
y="${a:-'x'}"; ( http::tls_log_vars_unset && curl -q -sS https://h.example/g1 )
cat <<'EOF'
curl -u u:p https://h.example/g2
EOF
cat <<-"EOF"
	$(curl -u u:p https://h.example/g3)
	EOF
cat <<EOF
libcurl and curl_config are no words of the gate
EOF
echo "x
y" && ( http::tls_log_vars_unset && curl -q -sS https://h.example/g4 )
z=$(
  echo a
) && ( http::tls_log_vars_unset && curl -q -sS https://h.example/g5 )
echo "a ' b" && ( http::tls_log_vars_unset && curl -q -sS https://h.example/g6 )
SH
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local c="a curl word the gate cannot place"
  local want_q="" want_t="" want_l=""
  for n in 1 3 5 8 9 11 14 20 22 26 28; do
    case "${n}" in
      5 | 11 | 14 | 28) want_q+="${bad}:${n}: ${c}"$'\n'; want_t+="${bad}:${n}: ${c}"$'\n' ;;
      *) want_q+="${bad}:${n}: ${q}"$'\n'; want_t+="${bad}:${n}: ${t}"$'\n'; want_l+="${bad}:${n} credential"$'\n' ;;
    esac
  done
  run curl_gate q "${bad}" "${amb}" "${open}"
  assert_failure 1
  assert_output "${want_q}${amb}:1: ${c}
${open}:2: ${c}"
  run curl_gate tls "${bad}" "${amb}" "${open}"
  assert_failure 1
  assert_output "${want_t}${amb}:1: ${c}
${open}:2: ${c}"
  run curl_gate list "${bad}" "${amb}" "${open}"
  assert_success
  assert_output "${want_l%$'\n'}"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate tls "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_success
  assert_output "${good}:1
${good}:12
${good}:15
${good}:16"
}

# A here-document body ends only at the line that is its delimiter,
# compared exactly, as bash does: " EOF" and a tab before EOF are body text
# with << (lines 7 and 11), and <<- strips only the leading tabs (line 16).
# In an unquoted body, a line that ends in a backslash joins the next line
# before the test: "x \" then "EOF" is body text (line 19), and "E\" then
# "OF" ends the body (line 24). In an unquoted body, a curl name in quotes
# after a $( or a backtick of the body is a call too (lines 2 to 4, and
# line 29 after a $( on the line before). The controls: the same lines in a
# quoted body, a curl name in quotes with no $( or backtick before it in its
# body (also after a body with a $( that ended, and after a file whose body
# has no end), and a YAML block, whose indentation the shell does not see.
@test "curl gate: a here-document body ends at its exact delimiter after the line join, and a quoted curl name in a \$( ) of an unquoted body is a finding" {
  local bad="${BATS_TEST_TMPDIR}/hd-bad.sh" good="${BATS_TEST_TMPDIR}/hd-good.sh"
  local yaml="${BATS_TEST_TMPDIR}/hd-good.yaml" open="${BATS_TEST_TMPDIR}/hd-open.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
cat <<EOF
$("curl" -u u:p https://h.example/d1)
$('curl' -u u:p https://h.example/d2)
`"/usr/bin/curl" -u u:p https://h.example/d3`
EOF
cat <<EOF
 EOF
'$(curl -u u:p https://h.example/d4)'
EOF
cat <<EOF
	EOF
'$(curl -u u:p https://h.example/d5)'
EOF
cat <<-'EOF'
x
	EOF
curl -u u:p https://h.example/d6
cat <<EOF
x \
EOF
'$(curl -u u:p https://h.example/d7)'
EOF
cat <<EOF
E\
OF
curl -u u:p https://h.example/d8
cat <<EOF
$(f
"curl" -u u:p https://h.example/d9)
EOF
SH
  printf '%s\n' 'cat <<EOF' '$(date)' > "${open}"
  cat > "${good}" <<'SH'
cat <<EOF
"curl" is a word of this text
EOF
cat <<EOF
$(date)
EOF
cat <<EOF
"curl" is a word of this text
EOF
cat <<'EOF'
	EOF
curl -u u:p https://h.example/g1
EOF
cat <<'EOF'
 EOF
curl -u u:p https://h.example/g2
x \
EOF
SH
  cat > "${yaml}" <<'YAML'
spec:
  args:
    - |
      cat <<'EOF'
      curl -u u:p https://h.example/y1
      EOF
      ( http::tls_log_vars_unset && curl -q -sS https://h.example/y2 )
YAML
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local c="a curl word the gate cannot place"
  local want_q="" want_t="" want_l=""
  for n in 2 3 4 8 12 17 21 26 29; do
    case "${n}" in
      17 | 26) want_q+="${bad}:${n}: ${q}"$'\n'; want_t+="${bad}:${n}: ${t}"$'\n'; want_l+="${bad}:${n} credential"$'\n' ;;
      *) want_q+="${bad}:${n}: ${c}"$'\n'; want_t+="${bad}:${n}: ${c}"$'\n' ;;
    esac
  done
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${want_q%$'\n'}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${want_t%$'\n'}"
  run curl_gate list "${bad}"
  assert_success
  assert_output "${want_l%$'\n'}"
  run curl_gate q "${open}" "${good}" "${yaml}"
  assert_success
  assert_output ""
  run curl_gate tls "${open}" "${good}" "${yaml}"
  assert_success
  assert_output ""
  run curl_gate list "${open}" "${good}" "${yaml}"
  assert_success
  assert_output "${yaml}:7"
}

# For <<-, bash joins the lines of an unquoted body first and then strips
# the leading tabs of the joined line: "E\" then "<tab>OF" is body text
# (line 3), and "<tab>E\" then "OF" ends the body (line 8). In a YAML
# file, YAML removes the indentation of the block scalar (that of its
# first line) before the shell sees the text: a delimiter with one more
# space (line 5 of the YAML file) or with the indentation of a command in
# the block (line 11) is body text for bash, and the gate reports it. A
# new block sets its own indentation (lines 15 to 18), and in a quoted
# body the same holds (line 20). A block with an indentation indicator
# (|2) has no delimiter that the gate can place (line 24). The control: a
# here-document whose opener has more indentation than its delimiter,
# with the delimiter at the indentation of the block, after a comment line
# that ends in "|" (a comment starts no block).
@test "curl gate: <<- strips the tabs of the joined line, and a YAML body ends only at the indentation of its block" {
  local sh="${BATS_TEST_TMPDIR}/tabs-bad.sh" yaml="${BATS_TEST_TMPDIR}/block-bad.yaml"
  local good="${BATS_TEST_TMPDIR}/block-good.yaml"
  CURL_GATE_ALLOW=()
  cat > "${sh}" <<'SH'
cat <<-EOF
E\
	OF
'$(curl -u u:p https://h.example/t1)'
EOF
cat <<-EOF
	E\
OF
curl -u u:p https://h.example/t2
SH
  cat > "${yaml}" <<'YAML'
spec:
  args:
    - |
      cat <<EOF
       EOF
      '$(curl -u u:p https://h.example/y1)'
      EOF
      if true; then
        cat <<EOF
        '$(curl -u u:p https://h.example/y2)'
        EOF
      '$(curl -u u:p https://h.example/y3)'
      EOF
      fi
    - |
          cat <<EOF
          EOF
          curl -u u:p https://h.example/y4
          cat <<'EOF'
           EOF
          EOF
    - |2
        cat <<EOF
        EOF
        echo the end
YAML
  cat > "${good}" <<'YAML'
spec:
  # the args: |
    args:
      - |
        if true; then
          cat <<'EOF'
        curl -u u:p https://h.example/g1
        EOF
        fi
        ( http::tls_log_vars_unset && curl -q -sS https://h.example/g2 )
YAML
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local c="a curl word the gate cannot place" d="a here-document delimiter the gate cannot place"
  run curl_gate q "${sh}" "${yaml}"
  assert_failure 1
  assert_output "${sh}:4: ${c}
${sh}:9: ${q}
${yaml}:5: ${d}
${yaml}:6: ${c}
${yaml}:10: ${c}
${yaml}:11: ${d}
${yaml}:12: ${c}
${yaml}:18: ${q}
${yaml}:20: ${d}
${yaml}:24: ${d}"
  run curl_gate tls "${sh}" "${yaml}"
  assert_failure 1
  assert_output "${sh}:4: ${c}
${sh}:9: ${t}
${yaml}:5: ${d}
${yaml}:6: ${c}
${yaml}:10: ${c}
${yaml}:11: ${d}
${yaml}:12: ${c}
${yaml}:18: ${t}
${yaml}:20: ${d}
${yaml}:24: ${d}"
  run curl_gate list "${sh}" "${yaml}"
  assert_success
  assert_output "${sh}:9 credential
${yaml}:18 credential"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate tls "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_success
  assert_output "${good}:10"
}

# A folded block (">") joins its lines with a space, and an empty line
# becomes the newline: YAML gives "cat <<EOF\nEOF '$(curl …)'\nEOF\n", and
# bash runs the $( ) in the unquoted body. The gate does not decode the
# folding, so each line with a << in a folded block is a finding in the q
# and tls modes (fold.yaml:2), also a << whose delimiter is on the next
# line (fold.yaml:9). The controls: a literal block ("|"), a tab after the
# indentation, and a <<< in a folded block.
@test "curl gate: a here-document in a folded YAML block is a finding" {
  local fold="${BATS_TEST_TMPDIR}/fold.yaml" good="${BATS_TEST_TMPDIR}/fold-good.yaml"
  CURL_GATE_ALLOW=()
  cat > "${fold}" <<'YAML'
run: >
  cat <<EOF

  EOF
  '$(curl -u u:p https://h.example/f1)'

  EOF
also: >-
  cat <<
  EOF
  echo x
YAML
  printf '%b' "lit: |\n  cat <<EOF\n\n  EOF\n  '\$(curl -u u:p https://h.example/g1)'\ntabbed: |\n    cat <<-EOF\n    \tbody\n    \tEOF\nstr: >\n  cat <<<\"x\"\n" > "${good}"
  local f="a here-document in a folded YAML block (>) the gate cannot place"
  local mode
  for mode in q tls; do
    run curl_gate "${mode}" "${fold}"
    assert_failure 1
    assert_output "${fold}:2: ${f}
${fold}:9: ${f}"
    run curl_gate "${mode}" "${good}"
    assert_success
    assert_output ""
  done
  run curl_gate list "${fold}" "${good}"
  assert_success
  assert_output ""
}

# YAML takes only spaces as indentation. In first.yaml, the first line of
# the block is two spaces and a tab: the parsers do not agree on it (YAML
# takes indentation two and the tab as content, a parser can also stop
# with an error). With indentation two, bash keeps two spaces before the
# first EOF and runs the $( ); the last EOF ends the body. The gate took
# the line as blank and indentation four. Now a tab in the leading white
# space of the first line of a block (also a line of white space only),
# or after fewer spaces than the indentation of the block (in.yaml:3), is
# a finding in the q and tls modes. From that line on the gate does not
# know the indentation of the block, so each delimiter of a body is a
# finding too. The control: a tab after the indentation (fold-good.yaml
# in the case above).
@test "curl gate: a tab in the indentation of a YAML block is a finding" {
  local first="${BATS_TEST_TMPDIR}/first.yaml" in="${BATS_TEST_TMPDIR}/in.yaml"
  CURL_GATE_ALLOW=()
  printf '%b' "run: |\n  \t\n    cat <<EOF\n    EOF\n    '\$(curl -u u:p https://h.example/t1)'\n  EOF\n" > "${first}"
  printf '%b' "run: |\n    cat <<EOF\n  \tEOF\n    EOF\n" > "${in}"
  local b="a YAML block line the gate cannot place: a tab in its indentation"
  local c="a curl word the gate cannot place" d="a here-document delimiter the gate cannot place"
  local mode
  for mode in q tls; do
    run curl_gate "${mode}" "${first}" "${in}"
    assert_failure 1
    assert_output "${first}:2: ${b}
${first}:4: ${d}
${first}:5: ${c}
${first}:6: ${d}
${in}:3: ${b}
${in}:3: ${d}
${in}:4: ${d}"
  done
  run curl_gate list "${first}" "${in}"
  assert_success
  assert_output ""
}

# The operand of a redirection is one shell word, with its quotes and
# escapes: the -q in it is no argument (lines 1 to 3). A keyword starts a
# command only in a command position: in "echo if curl" the curl is an
# argument, no call, and so a finding of the backstop in the q and tls
# modes (lines 4 to 6). The controls: a -q after an operand with white
# space in quotes or in a $( ), and curl after if, then, ! and time in a
# command position (also after ; with no space).
@test "curl gate: a redirection operand is one shell word, and a keyword starts a command only in a command position" {
  local bad="${BATS_TEST_TMPDIR}/redir-bad.sh" good="${BATS_TEST_TMPDIR}/redir-good.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
curl >"/tmp/log -q " -u u:p https://h.example/r1
curl 2>'/tmp/a -q ' -u u:p https://h.example/r2
curl >/tmp/a\ -q -u u:p https://h.example/r3
echo if curl -u u:p https://h.example/r4
echo then curl -u u:p https://h.example/r5
echo ! curl -u u:p https://h.example/r6
SH
  cat > "${good}" <<'SH'
curl >"/tmp/a b" -q -sS https://h.example/k1
curl 2>"$(printf '%s' x) y" -q -sS https://h.example/k2
if curl -q -sS https://h.example/k3; then :; fi
if true; then curl -q -sS https://h.example/k4; fi
while ! curl -q -sS https://h.example/k5; do :; done
x;then curl -q -sS https://h.example/k6
time curl -q -sS https://h.example/k7
SH
  local q="a curl call: -q is not its first argument"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local c="a curl word the gate cannot place"
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${bad}:1: ${q}
${bad}:2: ${q}
${bad}:3: ${q}
${bad}:4: ${c}
${bad}:5: ${c}
${bad}:6: ${c}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${bad}:1: ${t}
${bad}:2: ${t}
${bad}:3: ${t}
${bad}:4: ${c}
${bad}:5: ${c}
${bad}:6: ${c}"
  run curl_gate list "${bad}"
  assert_success
  assert_output "${bad}:1 credential
${bad}:2 credential
${bad}:3 credential"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  local want=""
  for n in 1 2 3 4 5 6 7; do want+="${good}:${n}"$'\n'; done
  run curl_gate list "${good}"
  assert_success
  assert_output "${want%$'\n'}"
}

# An alias or a function named curl: the gate checks the source text, not
# the command that a curl call runs, so each definition is a finding in
# the q mode. The calls in a function body stay calls. The controls name
# curl in no definition: a function that calls curl, and the words alias
# and function as arguments.
@test "curl gate: an alias or a function named curl is a finding in the q mode" {
  local bad="${BATS_TEST_TMPDIR}/shadow-bad.sh" good="${BATS_TEST_TMPDIR}/shadow-good.sh" n
  CURL_GATE_ALLOW=()
  cat > "${bad}" <<'SH'
alias curl='command curl -s'
alias ll='ls -l' curl=wget
alias 'curl=command curl -s'
curl() { command curl -q "$@"; }
curl () { command curl -q "$@"; }
function curl { command curl -q "$@"; }
function curl() { :; }
SH
  cat > "${good}" <<'SH'
x() { curl -q -sS https://h.example/s1; }
echo function curl # curl-gate: not a call
echo alias curl=x # curl-gate: not a call
curl_alias=1
SH
  local s="an alias or a function named curl: the gate cannot check what a curl call runs"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local want=""
  for n in 1 2 3 4 5 6 7; do
    want+="${bad}:${n}: ${s}"$'\n'
  done
  run curl_gate q "${bad}"
  assert_failure 1
  assert_output "${want%$'\n'}"
  run curl_gate tls "${bad}"
  assert_failure 1
  assert_output "${bad}:4: ${t}
${bad}:5: ${t}
${bad}:6: ${t}"
  run curl_gate list "${bad}"
  assert_success
  assert_output "${bad}:4
${bad}:5
${bad}:6"
  run curl_gate q "${good}"
  assert_success
  assert_output ""
  run curl_gate list "${good}"
  assert_success
  assert_output "${good}:1"
}

# The credential forms: each call of cred.sh carries one form, each call
# of plain.sh none (a short option of another command of the pipeline, as
# head -n or grep -E, does not count). The list mode shows the
# classification that the allowlist rule uses.
@test "curl gate: each credential form counts, and an option of another command does not" {
  local cred="${BATS_TEST_TMPDIR}/cred.sh" plain="${BATS_TEST_TMPDIR}/plain.sh" i want=""
  cat > "${cred}" <<'SH'
curl -q --netrc https://h.example/c1
curl -q --netrc-file "${f}" https://h.example/c2
curl -q --netrc-optional https://h.example/c3
curl -q -n https://h.example/c4
curl -q -sn https://h.example/c5
curl -q -E "${crt}" https://h.example/c6
curl -q --cert "${crt}" https://h.example/c7
curl -q --key "${k}" https://h.example/c8
curl -q -H @"${h}" https://h.example/c9
curl -q --header @headers.txt https://h.example/c10
curl -q -H "X-API-Key: ${TOKEN}" https://h.example/c11
curl -q -sK "${cfg}" https://h.example/c12
curl -q --config "${cfg}" https://h.example/c13
curl -q --oauth2-bearer "${t}" https://h.example/c14
curl -q -b "${jar}" https://h.example/c15
curl -q --cookie "s=${s}" https://h.example/c16
curl -q -d @body.json https://h.example/c17
curl -q --data-ascii "${b}" https://h.example/c18
curl -q -F "f=@${p}" https://h.example/c19
curl -q -T "${p}" https://h.example/c20
curl -q --upload-file "${p}" https://h.example/c21
curl -q -sS https://h.example/c22 2>&1 --netrc
curl -q --variable "t@${f}" https://h.example/c23
curl -q --form-string "k=${v}" https://h.example/c24
printf 'header = "Authorization: Bearer %s"\n' "${T}" | curl -q -K - https://h.example/c25
SH
  cat > "${plain}" <<'SH'
curl -q -fsSL https://h.example/p1 | head -n 1
curl -q -sS https://h.example/p2 | grep -E 'x|y'
curl -q -H "Accept: application/json" https://h.example/p3
curl -q -fLo /tmp/p4 https://h.example/p4
x=$(curl -q -sS https://h.example/p5 2>&1 | sed -n 1p)
curl -q -sS https://h.example/p6 | grep -F x
curl -q -sS https://h.example/p7 | tr -d '\r'
curl -q -sS https://h.example/p8 && grep -F y /tmp/f
curl -q -sS https://h.example/p9 || tr -d x
curl -q -sS https://h.example/p10 | grep -F 'Authorization: Bearer'
SH
  for i in $(seq 1 25); do
    want+="${cred}:${i} credential"$'\n'
  done
  run curl_gate list "${cred}"
  assert_success
  assert_output "${want%$'\n'}"
  run curl_gate list "${plain}"
  assert_success
  assert_output "${plain}:1
${plain}:2
${plain}:3
${plain}:4
${plain}:5
${plain}:6
${plain}:7
${plain}:8
${plain}:9
${plain}:10"
}

# The allowlist: an entry exempts exactly one call of its file from the
# helper rule, and it pins the full text of that command (runs of white
# space as one space). An added argument (an array expansion here) breaks
# the match: the call is then a finding. A call in the allowlist that
# carries a credential is a finding (-K, a header with a variable,
# --netrc: each form must count), and so is an entry that names no call or
# more than one, and an entry whose file the run does not read.
@test "curl gate: an allowlist entry pins the text of exactly one call without a credential, in a file of the run" {
  local f="${BATS_TEST_TMPDIR}/allow.sh"
  local t="a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)"
  local c="a curl call in the allowlist carries a credential"
  cat > "${f}" <<'SH'
curl -q -sS https://h.example/a1
curl -q -K "${cfg}" https://h.example/a2
curl -q -sS https://h.example/a3
curl -q -sS https://h.example/a3
curl -q -sS https://h.example/a5 "${args[@]}"
curl -q -sS -H "X-API-Key: ${TOKEN}" https://h.example/a6
curl -q --netrc https://h.example/a7
  curl   -q -sS    https://h.example/a8
SH
  CURL_GATE_ALLOW=()
  curl_gate_allow allow.sh '@@curl -q -sS https://h.example/a1' "a canary"
  curl_gate_allow allow.sh '@@curl -q -K "${cfg}" https://h.example/a2' "a canary"
  curl_gate_allow allow.sh '@@curl -q -sS https://h.example/a3' "a canary"
  curl_gate_allow allow.sh '@@curl -q -sS https://h.example/a5' "a canary"
  curl_gate_allow allow.sh '@@curl -q -sS -H "X-API-Key: ${TOKEN}" https://h.example/a6' "a canary"
  curl_gate_allow allow.sh '@@curl -q --netrc https://h.example/a7' "a canary"
  curl_gate_allow allow.sh '@@curl -q -sS https://h.example/a8' "a canary"
  curl_gate_allow allow.sh '@@curl -q -sS https://h.example/gone' "a canary"
  curl_gate_allow other.sh '@@curl -q -sS https://h.example/a1' "a canary"
  run curl_gate tls "${f}"
  assert_failure 1
  assert_output "${f}:2: ${c}
${f}:5: ${t}
${f}:6: ${c}
${f}:7: ${c}
allow.sh: the allowlist entry <@@curl -q -sS https://h.example/a3> matches 2 curl calls, want 1
allow.sh: the allowlist entry <@@curl -q -sS https://h.example/a5> matches 0 curl calls, want 1
allow.sh: the allowlist entry <@@curl -q -sS https://h.example/gone> matches 0 curl calls, want 1
other.sh: the allowlist entry names a file that this run does not read (a stale entry)"
  run curl_gate_allow allow.sh 'x'
  assert_failure 2
}

# The TLS list holds the six names: a pin with the literal names, apart
# from the list itself (the drift cases compare the copies with the list,
# so a name dropped from all copies would pass them).
@test "the TLS list: _HTTP_TLS_LOG_VARS holds the six names" {
  [ "${_HTTP_TLS_LOG_VARS[*]}" = "SSLKEYLOGFILE GNUTLS_KEYLOGFILE GNUTLS_DEBUG_LEVEL SSLTRACE SSLDEBUG SSLDEBUGFILE" ]
}

# An empty or an unset list would clear nothing: both helpers refuse it
# with one line, status 1, and the command after && does not run.
@test "the TLS helper: an empty or an unset list stops the call" {
  local fn form rc o="${BATS_TEST_TMPDIR}/list.out" e="${BATS_TEST_TMPDIR}/list.err"
  for fn in http::tls_log_vars_unset http::tls_log_vars_ok; do
    for form in empty unset; do
      rc=0
      (
        if [[ "${form}" == empty ]]; then _HTTP_TLS_LOG_VARS=(); else unset _HTTP_TLS_LOG_VARS; fi
        set -u
        export SSLKEYLOGFILE="${BATS_TEST_TMPDIR}/keys"
        "${fn}" && echo "the command after the helper ran"
      ) > "${o}" 2> "${e}" || rc=$?
      [ "${rc}" -eq 1 ]
      [ ! -s "${o}" ]
      [ "$(wc -l < "${e}")" -eq 1 ]
      grep -qF 'the list of TLS log and trace variables (_HTTP_TLS_LOG_VARS) is empty or not set: the request does not run' "${e}"
    done
  done
  # The probe of the readonly names: status 2 and the same line, so a
  # caller cannot read an empty list as "no name is readonly".
  for form in empty unset; do
    rc=0
    (
      if [[ "${form}" == empty ]]; then _HTTP_TLS_LOG_VARS=(); else unset _HTTP_TLS_LOG_VARS; fi
      set -u
      http::tls_log_vars_readonly
    ) > "${o}" 2> "${e}" || rc=$?
    [ "${rc}" -eq 2 ]
    [ ! -s "${o}" ]
    [ "$(wc -l < "${e}")" -eq 1 ]
    grep -qF 'the list of TLS log and trace variables (_HTTP_TLS_LOG_VARS) is empty or not set: the request does not run' "${e}"
  done
}

# The text of the readonly messages reads right for one name and for more
# names, given as separate arguments or as one line with a space between
# two names (the output of http::tls_log_vars_readonly).
@test "the TLS helper: the readonly text names one name, or more names in the plural" {
  [ "$(http::tls_log_var_readonly_text SSLKEYLOGFILE)" = "SSLKEYLOGFILE is readonly, so lo cannot clear it for curl" ]
  [ "$(http::tls_log_var_readonly_text "SSLKEYLOGFILE SSLTRACE")" = "SSLKEYLOGFILE and SSLTRACE are readonly, so lo cannot clear them for curl" ]
  [ "$(http::tls_log_var_readonly_text SSLKEYLOGFILE SSLTRACE SSLDEBUG)" = "SSLKEYLOGFILE, SSLTRACE and SSLDEBUG are readonly, so lo cannot clear them for curl" ]
  (
    readonly SSLKEYLOGFILE SSLTRACE
    [ "$(http::tls_log_vars_readonly)" = "SSLKEYLOGFILE SSLTRACE" ]
    [ "$(http::tls_log_var_readonly_text "$(http::tls_log_vars_readonly)")" = "SSLKEYLOGFILE and SSLTRACE are readonly, so lo cannot clear them for curl" ]
  )
}

# One list of the TLS log and trace variables: _HTTP_TLS_LOG_VARS of
# utils/http.sh. The agent CronJob cannot read that file, so its script
# holds a copy: the same names in the same order, cleared before its first
# curl call. (The Go copy is checked by TestTLSLogVarsMatchBash.)
@test "the TLS list: the agent CronJob clears the names of utils/http.sh before its first curl call" {
  local yaml="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent/cronjob.yaml" list first_unset first_curl
  (( ${#_HTTP_TLS_LOG_VARS[@]} >= 6 ))
  list=$(sed -n 's/^ *TLS_LOG_VARS="\([^"]*\)"$/\1/p' "${yaml}")
  [ "${list}" = "${_HTTP_TLS_LOG_VARS[*]}" ]
  first_unset=$(grep -n '^ *unset \${TLS_LOG_VARS}' "${yaml}" | head -1 | cut -d: -f1)
  first_curl=$(grep -nE '^ *(bounded [0-9]+ )?curl -q ' "${yaml}" | head -1 | cut -d: -f1)
  [ -n "${first_unset}" ] && [ -n "${first_curl}" ]
  (( first_unset < first_curl ))
}

# ── the family cases: helpers ────────────────────────────

_need_real_curl() {
  command -v python3 >/dev/null || skip "no python3"
  [[ -n "$(type -P curl)" ]] || skip "no curl"
  [[ -n "$(command -v openssl)" ]] || skip "no openssl: the https stub needs a self-signed certificate"
}

# _routes: the answers of the stub for every family (http_stub.py, the
# route table). The claim key label is the first 32 hex characters of the
# SHA-256 of d.example.
_routes() {
  export STUB_ROUTES="${BATS_TEST_TMPDIR}/routes.json"
  cat > "${STUB_ROUTES}" <<'JSON'
{
  "POST /api/clusters/register": {"code": 201, "bind_secret": true, "body": {"id": "cl-1", "registered": true, "claimed": true,
    "claimKey": {"publicKey": "ssh-ed25519 AAAA k", "fingerprint": "aa:bb", "name": "kubehz-claim-d.example"}}},
  "POST /api/credentials": {"code": 200, "body": {"data": {"validation": {"writable": true}}}},
  "GET /api/clusters?perPage=500": {"code": 200, "body": {"ok": true, "data": [{"id": "cl-1", "domain": "d.example",
    "createdAt": "2026-01-01T00:00:00Z", "status": "active", "connected": true}]}},
  "GET /api/clusters/cl-1/assessment": {"code": 200, "body": {"data": {}}},
  "POST /api/clusters/cl-1/agent-token": {"code": 200, "body": {"rotated": true, "clusterId": "cl-1"}},
  "DELETE /api/clusters/cl-1": {"code": 200, "body": {"ok": true}},
  "POST /api/clusters": {"code": 201, "body": {"id": "cl-1"}},
  "GET /api/clusters/cl-1": {"code": 200, "body": {"data": {"status": "Running"}}},
  "GET /api/clusters/cl-1/kubeconfig": {"code": 200, "body": "apiVersion: v1\nkind: Config\n"},
  "GET /v1/ssh_keys?name=kubehz-claim-d.example": {"code": 200, "body": {"ssh_keys": [{"id": 7,
    "labels": {"lok8s.dev/kubehz-claim": "4832b62ed035fb411ffefd346ab61e77"}}]}},
  "DELETE /v1/ssh_keys/7": {"code": 204},
  "POST /v1/ssh_keys": {"code": 201, "body": {"ssh_key": {"id": 8}}},
  "POST /oauth/v2/token": {"code": 200, "body": {"access_token": "at-stub", "token_type": "Bearer", "expires_in": 3600}},
  "GET /server": {"code": 200, "body": [{"server": {"server_name": "worker-0", "server_number": 12345,
    "server_ip": "203.0.113.10"}}]},
  "POST /server/12345": {"code": 200, "body": {"server": {"server_number": 12345, "server_name": "worker-0"}}},
  "GET /api/v2/dc": {"code": 200, "body": [{"metadata": {"name": "fsn1"}}]},
  "POST /api/v2/projects": {"code": 201, "body": {"id": "p-1"}}
}
JSON
}

# _https_stub: the https stub with a certificate for 127.0.0.1. API is its
# base URL, CONTROL_URL the URL of the controls.
_https_stub() {
  _routes
  stub_cert
  export STUB_BASIC="${STUB_BASIC:-}"
  http_stub_start "${STUB_CERT}" "${STUB_KEY}"
  export CURL_CA_BUNDLE="${STUB_CERT}" API="https://127.0.0.1:${STUB_PORT}"
  CONTROL_URL="${API}/control"
}

# _tunnel_stub: the stub as an http proxy on 127.0.0.1 for a URL with a
# fixed host (the Robot API in the kubeone driver): curl sends CONNECT to
# the stub, which speaks TLS in the tunnel with a certificate for that host.
# curl does not resolve the host itself, so no request leaves 127.0.0.1.
_tunnel_stub() {
  _routes
  stub_cert DNS:robot-ws.your-server.de
  export STUB_TUNNEL_CERT="${STUB_CERT}" STUB_TUNNEL_KEY="${STUB_KEY}" STUB_BASIC="${STUB_BASIC:-}"
  http_stub_start
  export CURL_CA_BUNDLE="${STUB_CERT}" https_proxy="http://127.0.0.1:${STUB_PORT}"
  CONTROL_URL="https://robot-ws.your-server.de/control"
}

# _step <label> <command...>: run the command and print its status.
_step() {
  local label="${1}" rc=0
  shift
  "$@" || rc=$?
  printf '== %s: rc=%s\n' "${label}" "${rc}"
}

# _no_secret <file...>: none of SECRETS is in the files.
_no_secret() {
  local s
  for s in "${SECRETS[@]}"; do
    if grep -qF -e "${s}" "$@"; then
      echo "a credential is in $*" >&2
      return 1
    fi
  done
}

# _curlrc_case <flow> <steps>: run <flow> once without a config file of the
# user (the reference) and once with a hostile config file in each place
# curl looks: CURL_HOME, XDG_CONFIG_HOME, HOME (curlrc_dir: a trace-ascii
# and an output line into the test dir). The checks of the config file come
# first: a run that fails because the file took its answer must still show
# the file as the cause. Each run must write no trace and no output file,
# print the same stdout and stderr and end with the same status as the
# reference, and show no credential. The reference must have <steps> steps,
# each with rc 0. The control in each place: curl without -q reads the file
# there, so the case can fail. curl older than 7.73 does not look in
# XDG_CONFIG_HOME: that control is left out, with a note. Sets CONTROLS.
_curlrc_case() {
  local flow="${1}" steps="${2}" var o e rc ref_rc=0
  local ref="${BATS_TEST_TMPDIR}/ref"
  mkdir -p "${BATS_TEST_TMPDIR}/curlrc-none"
  ( unset CURL_HOME XDG_CONFIG_HOME && export HOME="${BATS_TEST_TMPDIR}/curlrc-none" && "${flow}" ) \
    > "${ref}.out" 2> "${ref}.err" || ref_rc=$?
  [ "${ref_rc}" -eq 0 ]
  [ "$(grep -c '^== .*: rc=0$' "${ref}.out")" -eq "${steps}" ]
  [ "$(grep -c '^== ' "${ref}.out")" -eq "${steps}" ]
  _no_secret "${ref}.out" "${ref}.err"
  CONTROLS=0
  for var in CURL_HOME XDG_CONFIG_HOME HOME; do
    echo "# config file in ${var}"
    curlrc_dir "${var}"
    o="${RC_DIR}.stdout" e="${RC_DIR}.stderr" rc=0
    ( curlrc_env "${var}" && "${flow}" ) > "${o}" 2> "${e}" || rc=$?
    [ ! -e "${RC_TRACE}" ]
    [ ! -e "${RC_OUT}" ]
    diff "${ref}.out" "${o}"
    diff "${ref}.err" "${e}"
    [ "${rc}" -eq "${ref_rc}" ]
    _no_secret "${o}" "${e}"
    if [[ "${var}" == XDG_CONFIG_HOME ]] && ! curl_reads_xdg; then
      echo "# no control for XDG_CONFIG_HOME: this curl is older than 7.73 and does not look there" >&3
    else
      curlrc_control "${var}" "${CONTROL_URL}"
      CONTROLS=$((CONTROLS + 1))
    fi
  done
}

# _tls_value <name>: the value of a TLS variable in the cases below: a
# level for the debug levels, else a file in the test dir (no case writes
# it unless curl gets the variable).
_tls_value() {
  case "${1}" in
    GNUTLS_DEBUG_LEVEL) echo 9 ;;
    SSLTRACE | SSLDEBUG) echo 99 ;;
    *) echo "${BATS_TEST_TMPDIR}/tls.${1}" ;;
  esac
}

# _tls_case <flow> <calls> <one>: the TLS log and trace controls, for each
# name of _HTTP_TLS_LOG_VARS (utils/http.sh). The key log control first
# (keylog_control): this curl writes a key log for a request to the same
# server when SSLKEYLOGFILE reaches it. A curl that writes none leaves only
# the record check below, which needs no key log. A control request that
# does not get to the stub fails the case. Then <flow> runs with each name
# set and a spy for curl (tls_spy_on: a record of names, never a value,
# then the real curl): <calls> records, none with a name of the list, each
# with the control (an empty record would pass otherwise), no file of a
# name written, and the caller keeps each name with its value. Then <one>
# (a single call) runs twice: as it is, where it must succeed and run curl
# (so the next failure has the stop as its cause), then with a readonly
# SSLKEYLOGFILE: the call fails with the line that names the variable,
# curl does not run and no request reaches the stub.
_tls_case() {
  local flow="${1}" calls="${2}" one="${3}" kc=0 rc=0 before name f
  local o="${BATS_TEST_TMPDIR}/tls.stdout" e="${BATS_TEST_TMPDIR}/tls.stderr"
  (( ${#_HTTP_TLS_LOG_VARS[@]} >= 6 ))
  keylog_control "${CONTROL_URL}" || kc=$?
  # 2: the control request did not get to the stub. That is a fault, not a skip.
  [ "${kc}" -ne 2 ]
  if (( kc != 0 )); then
    echo "# this curl writes no TLS key log (its TLS backend ignores SSLKEYLOGFILE): the record check alone" >&3
  fi
  local spy="${BATS_TEST_TMPDIR}/curl-env"
  tls_spy_on "${spy}"
  for name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    export "${name}=$(_tls_value "${name}")"
  done
  export TLS_ENV_CONTROL=on
  "${flow}" > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 0 ]
  [ "$(grep -c '^== .*: rc=[^0]' "${o}")" -eq 0 ]
  for name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    [ ! -e "${BATS_TEST_TMPDIR}/tls.${name}" ]
  done
  _no_secret "${o}" "${e}"
  [ "$(find "${spy}" -type f | wc -l)" -eq "${calls}" ]
  for f in "${spy}"/env.*; do
    [ "$(head -1 "${f}")" = record ]
    grep -qx control "${f}"
    [ "$(wc -l < "${f}")" -eq 2 ]
  done
  # The caller keeps each one, with its value, as an export.
  for name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    [[ "$(declare -p "${name}")" == "declare -x ${name}="* ]]
    [ "${!name}" = "$(_tls_value "${name}")" ]
  done
  find "${spy}" -type f -name 'env.*' -delete
  rc=0
  ( "${one}" ) > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 0 ]
  [ "$(find "${spy}" -type f | wc -l)" -ge 1 ]
  find "${spy}" -type f -name 'env.*' -delete
  before=$(wc -l < "${STUB_LOG}")
  rc=0
  ( readonly SSLKEYLOGFILE && "${one}" ) > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -ne 0 ]
  grep -qF 'SSLKEYLOGFILE is readonly, so lo cannot clear it for curl: the request does not run' "${e}"
  [ "$(find "${spy}" -type f | wc -l)" -eq 0 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq "${before}" ]
  [ ! -e "${SSLKEYLOGFILE}" ]
}

# ── kubehz api (libs/kubehz/main, the KUBEHZ_TOKEN bearer) ──

_kubehz_api_setup() {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  kubehz::get_ssh_fingerprint() { echo "lo:d.example"; }
  kubectl() {
    case "$*" in
      *"get secret kubehz-agent"*) printf '%s' "${AGENT_TOKEN_IN_CLUSTER}" | base64 | tr -d '\n' ;;
    esac
  }
  export -f kubectl
  _https_stub
  YAML="${PATH_CLUSTERS}/d.example/cluster.lok8s.yaml"
  cat > "${YAML}" <<YAML
kind: Lo
metadata:
  name: d
spec:
  cluster:
    domain: d.example
  kubehz:
    hosting: self
    access: registered
    apiUrl: ${API}
YAML
  export domain="d.example" DOMAIN_NAME="d.example" LOK8S_KUBEHZ_API_URL="${API}"
}

# The register calls (bearer and anonymous), the direct claim with the
# token connect, status, assess, re-enroll and deregister: every call of
# the family. HCLOUD_TOKEN only for the token connect, so deregister leaves
# the claim key alone (the Hetzner Cloud family).
_kubehz_api_flow() {
  _register_body | _step "register (bearer)" kubehz::register_post d.example "${API}" "lo next" bearer
  _register_body | _step "register" kubehz::register_post d.example "${API}" "lo next"
  ( export HCLOUD_TOKEN LOK8S_KUBEHZ_CONNECT_TOKEN=true &&
    _step "direct claim" kubehz::direct_claim d.example "${YAML}" "${API}" )
  ( unset HCLOUD_TOKEN &&
    _step status kubehz::status &&
    _step assess kubehz::assess &&
    _step re-enroll kubehz::re-enroll &&
    _step deregister kubehz::deregister_cluster d.example "${YAML}" )
}

_kubehz_api_one() {
  _register_body | kubehz::register_post d.example "${API}" "lo next" bearer
}

# The body of a register with the synthetic bind secret: the stub logs
# bind=yes only when it arrives (the route has "bind_secret": true).
_register_body() {
  printf '{"domain":"d.example","bindSecret":"%s"}' "${STUB_BIND_SECRET}"
}

@test "kubehz api: curl reads no config file of the user" {
  _need_real_curl
  _kubehz_api_setup
  _curlrc_case _kubehz_api_flow 7
  # Each run: 3 registers (2 with the bearer; the two of the fixture carry
  # the bind secret, the direct claim has none), the token connect, 4
  # lists, the assessment, the agent token and the delete.
  [ "$(grep -c '^POST /api/clusters/register bearer bind=yes$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/clusters/register bearer bind=no$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/clusters/register none bind=yes$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/credentials bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /api/clusters?perPage=500 bearer$' "${STUB_LOG}")" -eq 16 ]
  [ "$(grep -c '^GET /api/clusters/cl-1/assessment bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/clusters/cl-1/agent-token bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^DELETE /api/clusters/cl-1 bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${CONTROLS}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 11 + CONTROLS)) ]
}

@test "kubehz api: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kubehz_api_setup
  _tls_case _kubehz_api_flow 11 _kubehz_api_one
}

# ── Hetzner Cloud (libs/kubehz/main, the HCLOUD_TOKEN bearer) ──

# The claim key (the anonymous register, the key lookup, its delete and the
# upload) and the claim key cleanup of deregister (lookup, delete).
_hcloud_flow() {
  _step "claim key" kubehz::ensure_claim_key d.example "${API}"
  _step deregister kubehz::deregister_cluster d.example "${YAML}"
}

# The Hetzner Cloud calls of the claim key alone: the register answer is
# given (kubehz::register_post is replaced in the subshell of the case), so
# the first curl call is the key lookup. Its status is the status of the
# upload, the last call: without the stop it succeeds.
_hcloud_one() {
  kubehz::register_post() {
    cat > /dev/null
    printf '%s' '{"id":"cl-1","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-d.example"}}'
  }
  kubehz::ensure_claim_key d.example "${API}"
}

@test "hetzner cloud: curl reads no config file of the user" {
  _need_real_curl
  _kubehz_api_setup
  export HCLOUD_API_BASE="${API}"
  _curlrc_case _hcloud_flow 2
  [ "$(grep -c '^GET /v1/ssh_keys?name=kubehz-claim-d.example bearer$' "${STUB_LOG}")" -eq 8 ]
  [ "$(grep -c '^DELETE /v1/ssh_keys/7 bearer$' "${STUB_LOG}")" -eq 8 ]
  [ "$(grep -c '^POST /v1/ssh_keys bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${CONTROLS}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 8 + CONTROLS)) ]
}

@test "hetzner cloud: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kubehz_api_setup
  export HCLOUD_API_BASE="${API}"
  _tls_case _hcloud_flow 8 _hcloud_one
}

# ── the agent key token (libs/kubehz/main, client_id:secret) ──

_token_flow() {
  local KUBEHZ_TOKEN_ACCESS="" KUBEHZ_TOKEN_EXPIRES=""
  _step token kubehz::token_access "${API}/oauth/v2/token" openid "" 1
  [[ "${KUBEHZ_TOKEN_ACCESS}" == at-stub ]] && echo "access token: at-stub"
  (( KUBEHZ_TOKEN_EXPIRES > 0 )) && echo "expires: set"
}

_token_one() {
  kubehz::token_access "${API}/oauth/v2/token" openid "" 1
}

@test "kubehz token: curl reads no config file of the user" {
  _need_real_curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export STUB_BASIC="${KUBEHZ_AGENT_CLIENT_ID}:${KUBEHZ_AGENT_CLIENT_SECRET}"
  _https_stub
  _curlrc_case _token_flow 1
  grep -qx 'access token: at-stub' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^POST /oauth/v2/token basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 + CONTROLS)) ]
}

@test "kubehz token: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  export STUB_BASIC="${KUBEHZ_AGENT_CLIENT_ID}:${KUBEHZ_AGENT_CLIENT_SECRET}"
  _https_stub
  _tls_case _token_flow 1 _token_one
}

# ── the agent api (libs/kubehz/agent, the access token) ──

_agent_flow() {
  _step "agent api" kubehz::agent_api list GET /api/spaces/sp-a
  printf '%s|%s\n' "${KUBEHZ_AGENT_STATUS}" "${KUBEHZ_AGENT_BODY}"
}

_agent_one() {
  kubehz::agent_api list GET /api/spaces/sp-b
}

_agent_setup() {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/agent"
  _https_stub
  export KUBEHZ_AGENT_BASE="${API}"
}

@test "kubehz agent: curl reads no config file of the user" {
  _need_real_curl
  _agent_setup
  _curlrc_case _agent_flow 1
  grep -qx '200|{"ok":true,"data":{"id":"sp-a"}}' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^GET /api/spaces/sp-a bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 + CONTROLS)) ]
}

@test "kubehz agent: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _agent_setup
  _tls_case _agent_flow 1 _agent_one
}

# ── hosted (libs/kubehz/hosted, the KUBEHZ_TOKEN bearer) ──

# The create, the wait, the kubeconfig and the destroy (with the lookup).
_hosted_flow() {
  _step provision kubehz::provision_hosted d.example "${YAML}"
  cat "${PATH_BASE}/.kubeconfig/d.example.yaml"
  _step destroy kubehz::destroy_hosted d.example "${YAML}"
}

_hosted_one() {
  kubehz::provision_hosted d.example "${YAML}"
}

_hosted_setup() {
  _kubehz_api_setup
  # A hosted create needs a version (provision_hosted refuses a spec without one).
  printf '  kubernetes:\n    version: v1.31.10\n' >> "${YAML}"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"
}

@test "kubehz hosted: curl reads no config file of the user" {
  _need_real_curl
  _hosted_setup
  _curlrc_case _hosted_flow 2
  grep -qx 'kind: Config' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^POST /api/clusters bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /api/clusters/cl-1 bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /api/clusters/cl-1/kubeconfig bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^DELETE /api/clusters/cl-1 bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 5 + CONTROLS)) ]
}

@test "kubehz hosted: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _hosted_setup
  _tls_case _hosted_flow 5 _hosted_one
}

# The kubeconfig download goes to a temp file beside the kubeconfig, which
# is 0600 from its creation (also under umask 022), and replaces the
# kubeconfig only after curl succeeded. A spy for curl records the mode of
# its stdout when that is a file (the download; the other calls write to a
# pipe). A failed download (HTTP 500) keeps the old kubeconfig and leaves
# no temp file.
@test "kubehz hosted: the kubeconfig is 0600 while curl writes it, and a failed download keeps the old one" {
  _need_real_curl
  [[ -e "/proc/${BASHPID}/fd/1" ]] || skip "no /proc/<pid>/fd"
  _hosted_setup
  local kc="${PATH_BASE}/.kubeconfig/d.example.yaml" o="${BATS_TEST_TMPDIR}/hk.out" e="${BATS_TEST_TMPDIR}/hk.err" rc=0
  export CURL_MODES="${BATS_TEST_TMPDIR}/modes"
  curl() {
    if [[ -f "/proc/${BASHPID}/fd/1" ]]; then
      stat -L -c %a "/proc/${BASHPID}/fd/1" >> "${CURL_MODES}"
    fi
    command curl "$@"
  }
  export -f curl
  mkdir -p "${PATH_BASE}/.kubeconfig"
  printf 'old\n' > "${kc}"
  jq '."GET /api/clusters/cl-1/kubeconfig" = {"code": 500, "body": "no"}' "${STUB_ROUTES}" > "${STUB_ROUTES}.new"
  mv -- "${STUB_ROUTES}.new" "${STUB_ROUTES}"
  ( umask 022 && kubehz::provision_hosted d.example "${YAML}" ) > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -ne 0 ]
  grep -qF 'Failed to download kubeconfig for hosted cluster cl-1' "${e}"
  [ "$(cat "${kc}")" = old ]
  [ -z "$(find "${PATH_BASE}/.kubeconfig" -name 'd.example.yaml.*')" ]
  [ "$(cat "${CURL_MODES}")" = 600 ]
  _routes
  : > "${CURL_MODES}"
  rc=0
  ( umask 022 && kubehz::provision_hosted d.example "${YAML}" ) > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 0 ]
  grep -qx 'kind: Config' "${kc}"
  [ "$(stat -c %a "${kc}")" = 600 ]
  [ "$(cat "${CURL_MODES}")" = 600 ]
  [ -z "$(find "${PATH_BASE}/.kubeconfig" -name 'd.example.yaml.*')" ]
  [ "$(grep -c '^GET /api/clusters/cl-1/kubeconfig bearer$' "${STUB_LOG}")" -eq 2 ]
}

# ── kubeone Robot naming (drivers/kubeone/config, user and password) ──

# The driver calls the Robot API at its fixed host, so the stub is the
# proxy (_tunnel_stub): each call is a CONNECT and the request in it.
_kubeone_setup() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
  export STUB_BASIC="${HROBOT_USER}:${HROBOT_PASSWORD}"
  _tunnel_stub
  MANIFEST="${BATS_TEST_TMPDIR}/kubeone.yaml"
  cat > "${MANIFEST}" <<'YAML'
staticWorkers:
  hosts:
    - hostname: worker-0
      publicAddress: 203.0.113.10
YAML
}

_kubeone_flow() {
  _step "name robot workers" kubeone::_name_robot_workers "${MANIFEST}"
}

_kubeone_one() {
  kubeone::_name_robot_workers "${MANIFEST}"
}

@test "kubeone robot: curl reads no config file of the user" {
  _need_real_curl
  _kubeone_setup
  _curlrc_case _kubeone_flow 1
  [ "$(grep -c '^CONNECT robot-ws.your-server.de:443 none$' "${STUB_LOG}")" -eq $((8 + CONTROLS)) ]
  [ "$(grep -c '^GET /server basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /server/12345 basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${CONTROLS}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 4 + 2 * CONTROLS)) ]
}

@test "kubeone robot: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kubeone_setup
  _tls_case _kubeone_flow 2 _kubeone_one
}

# ── hetzner provider Robot calls (providers/hetzner/main, user and password) ──

_provider_setup() {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
  export STUB_BASIC="${HROBOT_USER}:${HROBOT_PASSWORD}"
  _https_stub
  export HROBOT_API="${API}"
}

_provider_flow() {
  _step "robot lookup" hetzner::_robot_server_number_by_ip 203.0.113.10
}

# The wrapper itself: the lookup sends its stderr to /dev/null, which would
# hide the line of bash.
_provider_one() {
  hetzner::_robot_curl -f -sS "${HROBOT_API}/server"
}

@test "hetzner provider robot: curl reads no config file of the user" {
  _need_real_curl
  _provider_setup
  _curlrc_case _provider_flow 1
  grep -qx 12345 "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^GET /server basic$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 + CONTROLS)) ]
}

@test "hetzner provider robot: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _provider_setup
  _tls_case _provider_flow 1 _provider_one
}

# ── KKP api (drivers/kkp/api, the KKP_TOKEN bearer and the body) ──

_kkp_setup() {
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kkp/api"
  _https_stub
  export KKP_API_URL="${API}"
}

_kkp_flow() {
  _step "kkp get" kkp::api GET /api/v2/dc
  _step "kkp post" kkp::api POST /api/v2/projects '{"name":"p"}'
}

_kkp_one() {
  kkp::api GET /api/v2/dc
}

@test "kkp api: curl reads no config file of the user" {
  _need_real_curl
  _kkp_setup
  _curlrc_case _kkp_flow 2
  grep -qxF '[{"metadata":{"name":"fsn1"}}]' "${BATS_TEST_TMPDIR}/ref.out"
  grep -qxF '{"id":"p-1"}' "${BATS_TEST_TMPDIR}/ref.out"
  [ "$(grep -c '^GET /api/v2/dc bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/v2/projects bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((4 * 2 + CONTROLS)) ]
}

# The stop line is the only line of the readonly run: the call reports no
# HTTP status (it has none, as curl did not run).
@test "kkp api: curl gets no TLS log or trace variable and writes no TLS key log" {
  _need_real_curl
  _kkp_setup
  _tls_case _kkp_flow 2 _kkp_one
  [ "$(wc -l < "${BATS_TEST_TMPDIR}/tls.stderr")" -eq 1 ]
  [ "$(grep -c 'HTTP' "${BATS_TEST_TMPDIR}/tls.stderr")" -eq 0 ]
}
