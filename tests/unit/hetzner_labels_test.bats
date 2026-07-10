#!/usr/bin/env bats
# hetzner_labels_test.bats — the bare-metal #labels PARSE + the k8s-label
# validity guard in the Hetzner provider.
#
# Two contracts:
#   1. The comma-separated "k=v,k=v" #labels string on a #cloud.root server is
#      parsed into a node-label map that (a) preserves "=" inside a value
#      (k=a=b => a=b), (b) drops a leading "="/empty key (=orphan), (c) drops the
#      lok8s.dev/{cluster,role,group} orchestration keys AND the bare `role` key
#      (role=control-plane is a SELECTOR, not node taxonomy).
#   2. _validate_node_labels drops (with a warn) any key/value that violates the
#      k8s label syntax / 63-char limit, so a bad label fails EARLY in lok8s
#      rather than late inside `kubeone apply`.
#
# jq/yq are MANDATORY here (this is a parse suite). We FAIL-HARD rather than
# skip: a silent skip would ship a false-green "1..N ok" that validated nothing.
# Run via `PATH_BIN=$PWD/.bin ./.bin/argsh test …` (CI does; mounts the toolchain).

setup() {
  load "../test_helper"
  setup_tmpdir

  command -v jq &>/dev/null || { echo "FATAL: jq required — run with PATH_BIN=\$PWD/.bin" >&2; return 1; }
  command -v yq &>/dev/null || { echo "FATAL: yq required — run with PATH_BIN=\$PWD/.bin" >&2; return 1; }

  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  # Neutralize the argsh framework glue the provider sources at load time.
  import() { :; }; export -f import

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
}

teardown() { teardown_tmpdir; }

# The exact jq parse expression the bare-metal path uses on the #labels string.
_parse_labels() {
  echo "$1" | jq -R -c 'split(",") | map(select(length>0) | split("=") | select((.[0]|length) > 0) | {(.[0]): (.[1:]|join("="))}) | add // {}
    | del(.["lok8s.dev/cluster"], .["lok8s.dev/role"], .["lok8s.dev/group"], .["role"])'
}

@test "#labels parse: a value containing '=' is preserved intact (k=a=b => a=b)" {
  run _parse_labels 'x.kubehz.cloud/token=a=b=c'
  assert_success
  assert_output '{"x.kubehz.cloud/token":"a=b=c"}'
}

@test "#labels parse: a leading '='/empty key is dropped (=orphan)" {
  run _parse_labels '=orphan,tier.kubehz.cloud/free=true'
  assert_success
  assert_output '{"tier.kubehz.cloud/free":"true"}'
}

@test "#labels parse: the bare 'role' selector is NOT carried as a Node label" {
  run _parse_labels 'role=control-plane,tier.kubehz.cloud/free=true'
  assert_success
  assert_output '{"tier.kubehz.cloud/free":"true"}'
}

@test "#labels parse: lok8s.dev/{cluster,role,group} orchestration keys are dropped" {
  run _parse_labels 'lok8s.dev/cluster=c,lok8s.dev/role=worker,lok8s.dev/group=g,tier.kubehz.cloud/pro=true'
  assert_success
  assert_output '{"tier.kubehz.cloud/pro":"true"}'
}

@test "#labels parse: a bare key with no '=' becomes an empty-valued label" {
  run _parse_labels 'x.kubehz.cloud/flag'
  assert_success
  assert_output '{"x.kubehz.cloud/flag":""}'
}

# ── _validate_node_labels guard ─────────────────────────────

@test "validity guard: a well-formed labels map passes through unchanged" {
  run _validate_node_labels '{"tier.kubehz.cloud/free":"true","capability.kubehz.cloud/monitoring":"true"}' node-a
  assert_success
  assert_output '{"tier.kubehz.cloud/free":"true","capability.kubehz.cloud/monitoring":"true"}'
}

# The helper writes the warn to stderr and the filtered JSON to stdout; assert
# on the JSON only (the warn line legitimately NAMES the dropped key). We capture
# stdout separately so the warn text can't satisfy/spoil a JSON assertion.
_validated_json() { _validate_node_labels "$1" "${2:-node}" 2>/dev/null; }

@test "validity guard: a value over 63 chars is dropped with a warn" {
  local long; long=$(printf 'a%.0s' {1..64})
  run _validated_json "{\"x.kubehz.cloud/big\":\"${long}\",\"ok.kubehz.cloud/k\":\"v\"}" node-b
  assert_success
  # The valid label survives; the over-long one is gone from the JSON.
  assert_output --partial '"ok.kubehz.cloud/k":"v"'
  refute_output --partial 'x.kubehz.cloud/big'
}

@test "validity guard: a value with an illegal char (quote) is dropped" {
  run _validated_json '{"x.kubehz.cloud/inject":"a\"bad","ok/k":"v"}' node-c
  assert_success
  refute_output --partial 'x.kubehz.cloud/inject'
  assert_output --partial '"ok/k":"v"'
}

@test "validity guard: an empty value is legal (kept)" {
  run _validate_node_labels '{"x.kubehz.cloud/flag":""}' node-d
  assert_success
  assert_output '{"x.kubehz.cloud/flag":""}'
}

@test "validity guard: a key name segment over 63 chars is dropped" {
  local longname; longname=$(printf 'k%.0s' {1..64})
  run _validated_json "{\"kubehz.cloud/${longname}\":\"v\",\"ok/k\":\"v\"}" node-e
  assert_success
  refute_output --partial "${longname}"
  assert_output --partial '"ok/k":"v"'
}

@test "validity guard: a dropped label emits a [warn] on stderr" {
  run _validate_node_labels '{"bad key":"v"}' node-f
  assert_success
  assert_output --partial 'dropping invalid k8s label'
}

# ── addRemoveKeyValues removal marker ───────────────────────
# A single trailing "-" on a key is KubeOne's label REMOVAL convention
# ("key-": "" deletes the Node label on the next apply, nodes.go
# addRemoveKeyValues) — documented on the descriptor's #node-tiers, so the
# marker must survive both the #labels parse and the validity guard.

@test "#labels parse: a bare 'key-' removal entry becomes an empty-valued label (marker intact)" {
  run _parse_labels 'old.example.com/retired-'
  assert_success
  assert_output '{"old.example.com/retired-":""}'
}

@test "validity guard: a single trailing '-' (KubeOne removal marker) is kept" {
  run _validate_node_labels '{"old.example.com/retired-":""}' node-g
  assert_success
  assert_output '{"old.example.com/retired-":""}'
}

@test "validity guard: only ONE trailing '-' is tolerated (a double dash is still invalid)" {
  run _validated_json '{"x.kubehz.cloud/bad--":"","ok/k":"v"}' node-h
  assert_success
  refute_output --partial 'bad--'
  assert_output --partial '"ok/k":"v"'
}
