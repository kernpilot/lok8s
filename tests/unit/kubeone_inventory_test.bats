#!/usr/bin/env bats
# kubeone_inventory_test.bats — unit tests for the KubeOne driver's
# _append_inventory: the DESCRIPTOR-ANCHORING contract (2026-07-10 incident:
# label-discovered machine-controller VMs were swept into staticWorkers.hosts
# and kubeone apply tried to SSH/adopt live MachineDeployment nodes).
#
#   1. controlPlane.hosts / staticWorkers.hosts contain ONLY servers declared
#      in the descriptor's server[] array (matched by name). VMs discovered by
#      the cluster label but NOT declared are skipped with a warning, and stay
#      out of the apiserver alternativeNames SANs.
#   2. A descriptor with no server[] is refused outright (no fallback to label
#      discovery).
#   3. Declared hosts keep their node.labels through the anchoring filter —
#      incl. a labeled CONTROL PLANE and the `key-` removal-marker entry
#      (KubeOne addRemoveKeyValues renders `"key-": ""`). The label RENDER
#      contract itself (sorting, @json quoting, injection safety) is pinned by
#      kubeone_labels_test.bats; the parse/validity guard by
#      hetzner_labels_test.bats.
#
# Uses the real jq/yq/envsubst from `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  command -v jq &>/dev/null || skip "jq required"
  command -v yq &>/dev/null || skip "yq required"
  command -v envsubst &>/dev/null || skip "envsubst required"

  import() { :; }; export -f import
  :usage() { :; }; export -f :usage
  :args() { shift; }; export -f :args

  # main imports drivers/kubeone/config (import is stubbed here): the edits
  # of kubeone.yaml run through its kubeone::_yq_replace.
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  # The builder verifies the ssh private key file exists.
  SSH_KEY="${BATS_TEST_TMPDIR}/id_test"
  touch "${SSH_KEY}"

  DESCRIPTOR="${BATS_TEST_TMPDIR}/hetzner.json"

  # Seed manifest as kubeone::generate_config would leave it (cloudProvider
  # set; the builder appends apiEndpoint + hosts to it).
  MANIFEST="${BATS_TEST_TMPDIR}/kubeone.yaml"
  cat > "${MANIFEST}" <<'YAML'
apiVersion: kubeone.k8c.io/v1beta2
kind: KubeOneCluster
cloudProvider:
  hetzner: {}
YAML
}

teardown() {
  # A failed case can stop before it gives the write access back.
  [[ ! -d "${WD:-}" ]] || chmod -R u+w "${WD}"
  teardown_tmpdir
}

# _node <name> <role> [<labels-json>] — one provider::output inventory row
_node() {
  jq -n --arg n "$1" --arg r "$2" --argjson l "${3:-{\}}" \
    '{name: $n, role: $r, group: "default", public_ip: "203.0.113.9",
      private_ip: "10.0.0.9", access: "default", ssh_user: "root",
      ssh_port: 22, labels: $l}'
}

# _stub_provider_output <nodes_json> — provider::output returns the standard
# envelope (api endpoint + access + the given nodes; empty network id).
_stub_provider_output() {
  _PO_JSON=$(jq -n --arg key "${SSH_KEY}" --argjson nodes "$1" \
    '{api: {endpoint: "198.51.100.1", privateEndpoint: "", port: 6443},
      access: [{id: "default", type: "ssh", user: "root", port: 22,
                privateKey: $key, publicKey: ""}],
      nodes: $nodes,
      network: {id: "", name: "", cidr: ""}}')
  provider::output() { echo "${_PO_JSON}"; }
}

@test "undeclared label-discovered VMs (machine-controller) are excluded from staticWorkers with a warning" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "cp-0", "type": "cx43" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10" }
  ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane)" \
    --argjson b "$(_node worker-0 worker)" \
    --argjson c "$(_node base-fsn1-6c7bd8b74dxkzmzc-abcde worker)" \
    '[$a, $b, $c]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success
  assert_output --partial "skipping base-fsn1-6c7bd8b74dxkzmzc-abcde (role=worker)"
  assert_output --partial "not declared in the descriptor's server[]"

  run yq -r '[.staticWorkers.hosts[].hostname] | join(",")' "${MANIFEST}"
  assert_output "worker-0"
}

@test "control-plane selection is descriptor-anchored too" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [ { "name": "cp-0", "type": "cx43" } ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane)" \
    --argjson b "$(_node foreign-cp control-plane)" \
    '[$a, $b]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success
  assert_output --partial "skipping foreign-cp (role=control-plane)"

  run yq -r '[.controlPlane.hosts[].hostname] | join(",")' "${MANIFEST}"
  assert_output "cp-0"
  # the foreign CP's private IP must not leak into the apiserver SANs either
  run yq -r '.apiEndpoint.alternativeNames | length' "${MANIFEST}"
  assert_output "1"
}

@test "anchoring keeps node.labels on declared hosts — incl. a labeled CONTROL PLANE" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "cp-0", "type": "cx43" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10" }
  ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node cp-0 control-plane '{"role.kubehz.cloud/platform": "true"}')" \
    --argjson b "$(_node worker-0 worker '{"tier.kubehz.cloud/pro": "true"}')" \
    --argjson c "$(_node base-fsn1-6c7bd8b74dxkzmzc-abcde worker '{"tier.kubehz.cloud/pro": "true"}')" \
    '[$a, $b, $c]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success

  run yq -r '.controlPlane.hosts[0].labels["role.kubehz.cloud/platform"]' "${MANIFEST}"
  assert_output "true"
  run yq -r '.staticWorkers.hosts[0].labels["tier.kubehz.cloud/pro"]' "${MANIFEST}"
  assert_output "true"
}

@test "a key- removal marker (KubeOne addRemoveKeyValues) renders as an empty-valued label" {
  # A CP is declared+present — a manifest with zero control planes is refused
  # by the post-anchoring guard (round-1 fix), so worker-only fixtures are
  # invalid by design.
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [
    { "name": "cp-0", "type": "cx43" },
    { "name": "worker-0", "#cloud.root": "true", "#external-ip": "203.0.113.10" }
  ] }
JSON
  _stub_provider_output "$(jq -n \
    --argjson cp "$(_node cp-0 control-plane)" \
    --argjson a "$(_node worker-0 worker '{"tier.kubehz.cloud/pro": "true", "old.example.com/retired-": ""}')" \
    '[$cp, $a]')"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success

  run yq -r '.staticWorkers.hosts[0].labels | has("old.example.com/retired-")' "${MANIFEST}"
  assert_output "true"
  run yq -r '.staticWorkers.hosts[0].labels["old.example.com/retired-"]' "${MANIFEST}"
  assert_output ""
}

@test "a descriptor with no server[] refuses to build an inventory from labels alone" {
  echo '{ "cluster_name": "prod" }' > "${DESCRIPTOR}"
  _stub_provider_output "$(jq -n --argjson a "$(_node cp-0 control-plane)" '[$a]')"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_failure
  assert_output --partial "declares no server[]"
}

@test "YAML descriptor keeps envsubst expansion when extracting server[] (round-1 fix)" {
  # ${TEST_CP_NAME} must be expanded BEFORE the YAML→JSON conversion — the
  # regression re-read the raw file and anchored against the literal string.
  DESCRIPTOR="${BATS_TEST_TMPDIR}/hetzner.yaml"
  cat > "${DESCRIPTOR}" <<'YAML'
cluster_name: prod
server:
  - name: ${TEST_CP_NAME}
    type: cx43
YAML
  export TEST_CP_NAME="cp-0"
  local nodes
  nodes=$(jq -n --argjson a "$(_node cp-0 control-plane)" '[$a]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_success
  run yq -r '.controlPlane.hosts[0].hostname' "${MANIFEST}"
  assert_output "cp-0"
}

@test "anchoring that leaves zero control planes fails with the cause (round-1 fix)" {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod",
  "server": [ { "name": "worker-0", "#cloud.root": "true" } ] }
JSON
  local nodes
  nodes=$(jq -n \
    --argjson a "$(_node drifted-cp-name control-plane)" \
    --argjson b "$(_node worker-0 worker)" \
    '[$a, $b]')
  _stub_provider_output "${nodes}"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_failure
  assert_output --partial "ZERO control-plane hosts"
}

@test "an unparseable descriptor fails with a parse error, not 'declares no server[]' (round-2 fix)" {
  printf '{{ this is neither JSON nor YAML: [unclosed\n' > "${DESCRIPTOR}"
  _stub_provider_output "$(jq -n --argjson a "$(_node cp-0 control-plane)" '[$a]')"

  run _append_inventory "${DESCRIPTOR}" "${MANIFEST}"
  assert_failure
  assert_output --partial "neither valid JSON nor valid YAML"
  refute_output --partial "declares no server[]"
}

# ── The edits of kubeone.yaml after the host append ──────────────────────
# cloudProvider.hetzner.networkID (when the provider has a network) and the
# addons path (on Hetzner) go through kubeone::_yq_replace: a temp file
# beside kubeone.yaml, renamed over it. kubeone reset and kubeone status read
# kubeone.yaml as it is, also after a failed step, so a failed edit must
# leave the whole old file. A raw `yq -i` keeps its temp copy of the
# manifest in TMPDIR when its last write fails. Each case runs the step on
# kubeone.yaml in its own directory WD, with a fresh TMPDIR (TMPD). No case
# prints the content of a file.

# _edit_setup <network id>: one declared control plane, the network id ("" =
# no network), and the seed manifest in WD.
_edit_setup() {
  cat > "${DESCRIPTOR}" <<'JSON'
{ "cluster_name": "prod", "server": [ { "name": "cp-0", "type": "cx43" } ] }
JSON
  _stub_provider_output "$(jq -n --argjson a "$(_node cp-0 control-plane)" '[$a]')"
  _PO_JSON=$(printf '%s' "${_PO_JSON}" | jq --arg id "${1}" '.network.id = $id')
  WD="${BATS_TEST_TMPDIR}/wd"
  TMPD="${BATS_TEST_TMPDIR}/tmpdir"
  mkdir -p "${WD}" "${TMPD}"
  cp "${MANIFEST}" "${WD}/kubeone.yaml"
  chmod 600 "${WD}/kubeone.yaml"
  REAL_YQ=$(command -v yq)
  [[ -n "${REAL_YQ}" ]] || { echo "no yq" >&2; return 1; }
  export REAL_YQ
  export SNAP="${BATS_TEST_TMPDIR}/before-edit.yaml"
}

# _fake_yq <pattern> <action>: a yq on PATH in front of the real one. When
# its arguments hold <pattern>, it runs <action> first, with F set to its
# last argument (the file that it reads).
_fake_yq() {
  local dir="${BATS_TEST_TMPDIR}/fakeyq"
  mkdir -p "${dir}"
  {
    printf '#!/usr/bin/env bash\n'
    printf '[[ -n "${REAL_YQ:-}" ]] || exit 3\n'
    printf 'for F in "$@"; do :; done\n'
    printf 'if [[ "$*" == *%q* ]]; then\n%s\nfi\n' "${1}" "${2}"
    printf 'exec "${REAL_YQ}" "$@"\n'
  } > "${dir}/yq"
  chmod +x "${dir}/yq"
}

# _run_step: _append_inventory on WD/kubeone.yaml, with the fake yq first on
# PATH and TMPDIR=TMPD.
_run_step() {
  local saved_path="${PATH}"
  PATH="${BATS_TEST_TMPDIR}/fakeyq:${PATH}"
  TMPDIR="${TMPD}" run _append_inventory "${DESCRIPTOR}" "${WD}/kubeone.yaml"
  PATH="${saved_path}"
}

# _assert_only <dir> <entry>...: the directory holds exactly these entries.
_assert_only() {
  local dir="${1}" want got
  shift
  want=$(printf '%s\n' "$@" | sort)
  got=$(find "${dir}" -mindepth 1 -printf '%P\n' | sort)
  [ "${got}" = "${want}" ] || { echo "entries of ${dir}: ${got//$'\n'/ } (want: ${want//$'\n'/ })" >&2; return 1; }
}

# _assert_tmpdir_empty: nothing is left in TMPD (names and modes only).
_assert_tmpdir_empty() {
  local left
  left=$(find "${TMPD}" -mindepth 1 -printf '%P (mode %m)\n')
  [ -z "${left}" ] || { echo "left in TMPDIR: ${left}" >&2; return 1; }
}

# _ref <expression>...: what yq -i (the old form) writes from SNAP, the input
# of the first edit, with these expressions in turn.
_ref() {
  local ref="${BATS_TEST_TMPDIR}/ref.yaml" e
  cp "${SNAP}" "${ref}"
  for e in "$@"; do
    TMPDIR="${BATS_TEST_TMPDIR}" "${REAL_YQ}" -i "${e}" "${ref}" || return 1
  done
  printf '%s' "${ref}"
}

@test "networkID edit: kubeone.yaml holds what yq -i writes from the same input" {
  _edit_setup net-7
  _fake_yq 'networkID =' '[[ -e "${SNAP}" ]] || cp -- "${F}" "${SNAP}"'
  _run_step
  assert_success
  [ -f "${SNAP}" ] || { echo "the networkID edit did not run" >&2; return 1; }
  # On Hetzner the addons edit follows.
  local ref
  ref=$(_ref '.cloudProvider.hetzner.networkID = "net-7"' '.addons.enable = true | .addons.path = "./addons"')
  cmp "${ref}" "${WD}/kubeone.yaml"
  run yq -r '.cloudProvider.hetzner.networkID' "${WD}/kubeone.yaml"
  assert_output "net-7"
  _assert_only "${WD}" kubeone.yaml
  _assert_tmpdir_empty
}

@test "networkID edit: a failed write leaves the old kubeone.yaml, no temp file and an empty TMPDIR, and stops the step" {
  _edit_setup net-7
  # A write that fails part way: this yq saves the file that it reads, then
  # empties it and fails.
  _fake_yq 'networkID =' 'cp -- "${F}" "${SNAP}"; : > "${F}"; exit 1'
  _run_step
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1" >&2; return 1; }
  assert_output --partial "kubeone inventory: cannot write cloudProvider.hetzner.networkID into ${WD}/kubeone.yaml"
  [ -f "${SNAP}" ] || { echo "the networkID edit did not run" >&2; return 1; }
  cmp "${SNAP}" "${WD}/kubeone.yaml" || { echo "kubeone.yaml is not the old file" >&2; return 1; }
  _assert_only "${WD}" kubeone.yaml
  _assert_tmpdir_empty
  # The step stopped: the addons edit did not run.
  run yq -r '.addons.enable // "unset"' "${WD}/kubeone.yaml"
  assert_output "unset"
}

@test "networkID edit: a kubeone.yaml that cannot be written stops the step and leaves TMPDIR empty" {
  [[ "$(id -u)" != 0 ]] || skip "root writes a read-only file"
  _edit_setup net-7
  # After the host append, at the cloudProvider read before the edit, this yq
  # makes kubeone.yaml and its directory read-only. In that setup a raw
  # `yq -i` keeps its temp copy of the manifest in TMPDIR.
  _fake_yq 'has("hetzner")' 'chmod 400 "${F}"; chmod 500 "${F%/*}"'
  _run_step
  chmod 700 "${WD}"
  chmod 600 "${WD}/kubeone.yaml"
  _assert_tmpdir_empty
  _assert_only "${WD}" kubeone.yaml
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1" >&2; return 1; }
  assert_output --partial "kubeone inventory: cannot write cloudProvider.hetzner.networkID into ${WD}/kubeone.yaml"
}

@test "addons edit: kubeone.yaml holds what yq -i writes from the same input" {
  _edit_setup ""
  _fake_yq '.addons.enable = true' '[[ -e "${SNAP}" ]] || cp -- "${F}" "${SNAP}"'
  _run_step
  assert_success
  [ -f "${SNAP}" ] || { echo "the addons edit did not run" >&2; return 1; }
  local ref
  ref=$(_ref '.addons.enable = true | .addons.path = "./addons"')
  cmp "${ref}" "${WD}/kubeone.yaml"
  run yq -r '.addons.path' "${WD}/kubeone.yaml"
  assert_output "./addons"
  _assert_only "${WD}" kubeone.yaml
  _assert_tmpdir_empty
}

@test "addons edit: a failed write leaves the old kubeone.yaml, no temp file and an empty TMPDIR, and stops the step" {
  _edit_setup ""
  _fake_yq '.addons.enable = true' 'cp -- "${F}" "${SNAP}"; : > "${F}"; exit 1'
  _run_step
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1" >&2; return 1; }
  assert_output --partial "kubeone inventory: cannot write the addons path into ${WD}/kubeone.yaml"
  [ -f "${SNAP}" ] || { echo "the addons edit did not run" >&2; return 1; }
  cmp "${SNAP}" "${WD}/kubeone.yaml" || { echo "kubeone.yaml is not the old file" >&2; return 1; }
  _assert_only "${WD}" kubeone.yaml
  _assert_tmpdir_empty
}

@test "addons edit: a kubeone.yaml that cannot be written stops the step and leaves TMPDIR empty" {
  [[ "$(id -u)" != 0 ]] || skip "root writes a read-only file"
  _edit_setup ""
  _fake_yq 'has("hetzner")' 'chmod 400 "${F}"; chmod 500 "${F%/*}"'
  _run_step
  chmod 700 "${WD}"
  chmod 600 "${WD}/kubeone.yaml"
  _assert_tmpdir_empty
  _assert_only "${WD}" kubeone.yaml
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1" >&2; return 1; }
  assert_output --partial "kubeone inventory: cannot write the addons path into ${WD}/kubeone.yaml"
}

@test "networkID edit: a write back that fails after yq succeeds leaves the old kubeone.yaml, no temp file and an empty TMPDIR, and stops the step" {
  [[ "$(id -u)" != 0 ]] || skip "root writes a read-only file"
  _edit_setup net-7
  # This yq saves the staging file that it reads, runs the real yq, then
  # makes the staging file read-only, so the write of the result back into
  # it fails. It records the exit status of the real yq.
  export YQ_RC="${BATS_TEST_TMPDIR}/yq.rc"
  _fake_yq 'networkID =' 'cp -- "${F}" "${SNAP}"; "${REAL_YQ}" "$@"; rc=$?; chmod 400 "${F}"; printf "%s\n" "${rc}" > "${YQ_RC}"; exit "${rc}"'
  _run_step
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1" >&2; return 1; }
  assert_output --partial "kubeone inventory: cannot write cloudProvider.hetzner.networkID into ${WD}/kubeone.yaml"
  [ -f "${SNAP}" ] || { echo "the networkID edit did not run" >&2; return 1; }
  cmp "${SNAP}" "${WD}/kubeone.yaml" || { echo "kubeone.yaml is not the old file" >&2; return 1; }
  _assert_only "${WD}" kubeone.yaml
  _assert_tmpdir_empty
  run cat "${YQ_RC}"
  assert_output "0"
  # The step stopped: the addons edit did not run.
  run yq -r '.addons.enable // "unset"' "${WD}/kubeone.yaml"
  assert_output "unset"
}
