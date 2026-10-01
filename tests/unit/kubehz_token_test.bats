#!/usr/bin/env bats
# kubehz_token_test.bats — `lo kubehz token`, the kubectl exec credential
# plugin for a kubehz agent key (bash twin of internal/kubehz/token.go).
#
# Pins the parts the Go side pins with the same values: the ExecCredential
# bytes, the cache file (name, shape, mode) and every refusal message. Also
# the secret containment: the client secret reaches curl on STDIN (a config
# line), never on argv, and never lands in the cache or on a stream.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # argsh `:args` stub: `--name value` pairs, and the one boolean flag.
  :args() {
    shift
    while (( $# )); do
      case "$1" in
        --no-cache) no_cache=1 ;;
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

  # A fixed clock (the Go harness's 1700000000) for every `date -u +%s`.
  date() {
    if [[ "$*" == "-u +%s" ]]; then echo "${STUB_NOW:-1700000000}"; else command date "$@"; fi
  }
  export -f date

  export XDG_CACHE_HOME="${BATS_TEST_TMPDIR}/cache"
  export KUBEHZ_AGENT_CLIENT_ID="cid"
  export KUBEHZ_AGENT_CLIENT_SECRET="s3cr3t-value"
  export CURL_ARGS="${BATS_TEST_TMPDIR}/curl.args"
  export CURL_STDIN="${BATS_TEST_TMPDIR}/curl.stdin"
  export CURL_CALLS="${BATS_TEST_TMPDIR}/curl.calls"
  export STUB_CODE="200"
  export STUB_BODY='{"access_token":"jwt-1","token_type":"Bearer","expires_in":43199}'

  # curl stub: records argv and stdin, answers STUB_BODY + "\n<code>".
  curl() {
    printf '%s\n' "$*" >> "${CURL_ARGS}"
    cat >> "${CURL_STDIN}"
    echo x >> "${CURL_CALLS}"
    printf '%s\n%s' "${STUB_BODY}" "${STUB_CODE}"
  }
  export -f curl

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  URL="https://id.example/oauth/v2/token"
}

teardown() {
  teardown_tmpdir
}

calls() { [[ -f "${CURL_CALLS}" ]] && wc -l < "${CURL_CALLS}" | tr -d ' ' || echo 0; }

@test "token: prints the ExecCredential bytes the Go twin prints" {
  run kubehz::token --token-url "${URL}" --scope scope
  assert_success
  assert_output '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"jwt-1","expirationTimestamp":"2023-11-15T10:13:19Z"}}'
}

@test "token: the secret reaches curl on stdin only, over https only" {
  run kubehz::token --token-url "${URL}" --scope scope
  assert_success
  run cat "${CURL_STDIN}"
  assert_output 'user = "cid:s3cr3t-value"'
  run cat "${CURL_ARGS}"
  refute_output --partial 's3cr3t-value'
  assert_output --partial '--proto =https'
  assert_output --partial 'grant_type=client_credentials'
  assert_output --partial 'scope=scope'
}

@test "token: the cache file has the Go twin's name, shape and mode, and is reused" {
  run kubehz::token --token-url "${URL}" --scope scope
  assert_success
  local f="${XDG_CACHE_HOME}/lok8s/kubehz-token/307dcc769135510a4f46243554b6d3b1.json"
  run cat "${f}"
  assert_output '{"access_token":"jwt-1","expires_at":1700043199}'
  run stat -c '%a' "${f}"
  assert_output "600"
  run stat -c '%a' "${XDG_CACHE_HOME}/lok8s/kubehz-token"
  assert_output "700"
  run kubehz::token --token-url "${URL}" --scope scope
  assert_success
  assert_output --partial '"token":"jwt-1"'
  [ "$(calls)" -eq 1 ]
}

@test "token: a token within five minutes of expiry is refreshed" {
  run kubehz::token --token-url "${URL}" --scope scope
  STUB_NOW=$(( 1700000000 + 43199 - 240 ))
  STUB_BODY='{"access_token":"jwt-2","expires_in":43199}'
  run kubehz::token --token-url "${URL}" --scope scope
  assert_success
  assert_output --partial '"token":"jwt-2"'
  [ "$(calls)" -eq 2 ]
}

@test "token: a cache file others can read is ignored" {
  local d="${XDG_CACHE_HOME}/lok8s/kubehz-token"
  mkdir -p "${d}"
  echo '{"access_token":"planted","expires_at":9999999999}' > "${d}/307dcc769135510a4f46243554b6d3b1.json"
  chmod 644 "${d}/307dcc769135510a4f46243554b6d3b1.json"
  run kubehz::token --token-url "${URL}" --scope scope
  assert_success
  refute_output --partial 'planted'
  [ "$(calls)" -eq 1 ]
}

@test "token: --format token prints the bare token; kubectl's apiVersion is honoured" {
  run kubehz::token --token-url "${URL}" --scope scope --format token
  assert_success
  assert_output 'jwt-1'
  KUBERNETES_EXEC_INFO='{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential"}' \
    run kubehz::token --token-url "${URL}" --scope scope
  assert_output --partial '"apiVersion":"client.authentication.k8s.io/v1beta1"'
}

@test "token: the refusals print the Go twin's messages and reach no endpoint" {
  run kubehz::token --scope scope
  assert_failure
  assert_output --partial 'token url and scope are required: pass --token-url and --scope, or set KUBEHZ_AGENT_TOKEN_URL and KUBEHZ_AGENT_SCOPE'
  run kubehz::token --token-url http://id.example/oauth/v2/token --scope scope
  assert_failure
  assert_output --partial 'Token URL must use HTTPS: http://id.example/oauth/v2/token'
  KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::token --token-url "${URL}" --scope scope
  assert_failure
  assert_output --partial 'no agent key: set KUBEHZ_AGENT_CLIENT_ID and KUBEHZ_AGENT_CLIENT_SECRET (or pass --secret-file)'
  run kubehz::token --token-url "${URL}" --scope scope --format yaml
  assert_failure
  assert_output --partial 'unknown --format yaml (want exec-credential or token)'
  run kubehz::token --token-url "${URL}" --scope scope --secret-file /nonexistent/secret
  assert_failure
  assert_output --partial 'cannot read the secret file /nonexistent/secret'
  [ "$(calls)" -eq 0 ]
}

@test "token: an endpoint refusal names the reason on one line and caches nothing" {
  STUB_CODE=401
  STUB_BODY='{"error":"invalid_client","error_description":"client authentication failed\nfor s3cr3t"}'
  run kubehz::token --token-url "${URL}" --scope scope
  assert_failure
  assert_output --partial 'token request refused: HTTP 401: invalid_client client authentication failed for s3cr3t'
  refute_output --partial 's3cr3t-value'
  [ ! -e "${XDG_CACHE_HOME}/lok8s/kubehz-token/307dcc769135510a4f46243554b6d3b1.json" ]
}

@test "token: --secret-file is trimmed and --no-cache writes nothing" {
  printf 'from-file\n' > "${BATS_TEST_TMPDIR}/secret"
  KUBEHZ_AGENT_CLIENT_SECRET="" run kubehz::token --token-url "${URL}" --scope scope --secret-file "${BATS_TEST_TMPDIR}/secret" --no-cache
  assert_success
  run cat "${CURL_STDIN}"
  assert_output 'user = "cid:from-file"'
  [ ! -e "${XDG_CACHE_HOME}/lok8s" ]
}
