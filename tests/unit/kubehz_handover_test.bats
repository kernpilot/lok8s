#!/usr/bin/env bats
# kubehz_handover_test.bats — `lo kubehz handover` (eject target side).
#
# Guards .lok8s/libs/kubehz/handover against the cross-repo handover
# contract (§2 bundle keys, §4 lok8s scope):
#   - bundle validation names the missing key;
#   - receive (kubeadm path) runs the steps IN ORDER — snapshot restore →
#     kubeadm init (with the documented DirAvailable--var-lib-etcd ignore) →
#     verify — against a sandboxed /etc/kubernetes + /var/lib/etcd
#     (KUBEHZ_HANDOVER_K8S_DIR / _ETCD_DIR test seams);
#   - the generated kubeadm config wires the endpoint DNS (certSANs +
#     controlPlaneEndpoint) and the encryption-provider-config;
#   - the CA-hash verify FAILS when kubeadm re-minted the CA (identity lost);
#   - preseed places exactly the six PKI files over scp and nothing else.
#
# Mock idiom mirrors kubehz_register_test.bats: bash function mocks
# (export -f) over a sourced lib, with a shared CALLS log for order asserts.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/handover"

  # The argsh :args/:usage builtins are absent when a lib is sourced in bats —
  # shim them to parse exactly as argsh does (the recover_test.bats idiom),
  # including the ':!' required-flag refusal BEFORE the body runs.
  :args() {
    shift  # description
    while (( $# )); do
      case "$1" in
        --bundle|-b)   bundle="$2"; shift ;;
        --snapshot|-s) snapshot="$2"; shift ;;
        --single-node) single_node=1 ;;
        --force|-f)    force=1 ;;
        --node|-n)     node="$2"; shift ;;
        --user|-u)     user="$2"; shift ;;
        --port|-p)     port="$2"; shift ;;
        --ssh-key|-i)  ssh_key="$2"; shift ;;
      esac
      shift
    done
    local spec name var
    for spec in "${args[@]}"; do
      [[ "${spec}" == *':!'* ]] || continue
      name="${spec%%|*}"
      var="${name//-/_}"
      [[ -n "${!var:-}" ]] || { echo "Error: missing required flag: ${name}" >&2; exit 2; }
    done
  }
  # :usage resolves the subcommand to handover::<cmd> and stashes the tail —
  # mirrors the real dispatch (kubehz::handover → handover::receive).
  :usage() {
    shift  # description
    local cmd="$1"; shift
    usage=("handover::${cmd}" "$@")
  }
  export -f :args :usage

  export CALLS="${BATS_TEST_TMPDIR}/calls.log"
  export KUBEHZ_HANDOVER_K8S_DIR="${BATS_TEST_TMPDIR}/etc-kubernetes"
  export KUBEHZ_HANDOVER_ETCD_DIR="${BATS_TEST_TMPDIR}/var-lib-etcd"
  export KUBEHZ_HANDOVER_WAIT=0
}

teardown() {
  teardown_tmpdir
}

# A complete contract §2 bundle. The snapshot-location deliberately points at
# s3:// so tests prove --snapshot short-circuits the remote fetch.
_make_bundle() {
  BUNDLE="${BATS_TEST_TMPDIR}/bundle"
  mkdir -p "${BUNDLE}"
  printf -- '-----BEGIN CERTIFICATE-----\ntest-cluster-ca\n-----END CERTIFICATE-----\n' > "${BUNDLE}/ca.crt"
  printf -- '-----BEGIN RSA PRIVATE KEY-----\ntest-ca-key\n-----END RSA PRIVATE KEY-----\n' > "${BUNDLE}/ca.key"
  printf -- '-----BEGIN PUBLIC KEY-----\ntest-sa-pub\n-----END PUBLIC KEY-----\n' > "${BUNDLE}/sa.pub"
  printf -- '-----BEGIN RSA PRIVATE KEY-----\ntest-sa-key\n-----END RSA PRIVATE KEY-----\n' > "${BUNDLE}/sa.key"
  printf -- '-----BEGIN CERTIFICATE-----\ntest-fpca\n-----END CERTIFICATE-----\n' > "${BUNDLE}/front-proxy-ca.crt"
  printf -- '-----BEGIN RSA PRIVATE KEY-----\ntest-fpca-key\n-----END RSA PRIVATE KEY-----\n' > "${BUNDLE}/front-proxy-ca.key"
  printf 'dGVzdC1lbmNyeXB0aW9uLWtleQ==' > "${BUNDLE}/encryption-key"
  printf 's3://kubehz-backups/handover/cl-001/snap.db' > "${BUNDLE}/snapshot-location"
  printf 'cl-001.kubermatic.kkp.kubehz.in.net' > "${BUNDLE}/endpoint-dns"

  SNAPSHOT="${BATS_TEST_TMPDIR}/snap.db"
  printf 'fake-etcd-snapshot' > "${SNAPSHOT}"
}

# Happy-path mocks: every external binary logs into CALLS.
_mock_node_binaries() {
  etcdutl() { echo "etcdutl $*" >> "${CALLS}"; }
  kubeadm() { echo "kubeadm $*" >> "${CALLS}"; }
  kubectl() { echo "kubectl $*" >> "${CALLS}"; return 0; }
  export -f etcdutl kubeadm kubectl
}

# ── Bundle validation ────────────────────────────────────

@test "handover: validate_bundle passes on a complete bundle" {
  _make_bundle
  run handover::validate_bundle "${BUNDLE}"
  assert_success
}

@test "handover: validate_bundle NAMES the missing key" {
  _make_bundle
  rm "${BUNDLE}/sa.key"
  run handover::validate_bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "missing or empty key: sa.key"
}

@test "handover: validate_bundle rejects an empty key file too" {
  _make_bundle
  : > "${BUNDLE}/encryption-key"
  run handover::validate_bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "missing or empty key: encryption-key"
}

@test "handover: resolve_bundle unpacks a .tar.gz into a private dir" {
  _make_bundle
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  tar -czf "${tarball}" -C "${BUNDLE}" .
  run handover::resolve_bundle "${tarball}"
  assert_success
  [ -s "${output}/ca.crt" ]
  [ -s "${output}/endpoint-dns" ]
}

# ── receive: the full kubeadm path against a sandbox ─────

@test "handover receive: restores, inits and verifies IN ORDER with the seeded PKI" {
  _make_bundle
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  # Verify ran and reported the matching CA hash (output is re-used below).
  assert_output --partial "handover: verified"
  assert_output --partial "handover: receive complete"

  # Call order: snapshot restore BEFORE kubeadm init BEFORE the verify probe.
  run cat "${CALLS}"
  assert_success
  local restore_line init_line verify_line
  restore_line=$(grep -n '^etcdutl snapshot restore' "${CALLS}" | head -1 | cut -d: -f1)
  init_line=$(grep -n '^kubeadm init' "${CALLS}" | head -1 | cut -d: -f1)
  verify_line=$(grep -n '^kubectl --kubeconfig' "${CALLS}" | head -1 | cut -d: -f1)
  [ -n "${restore_line}" ] && [ -n "${init_line}" ] && [ -n "${verify_line}" ]
  [ "${restore_line}" -lt "${init_line}" ]
  [ "${init_line}" -lt "${verify_line}" ]

  # The restore targeted the sandboxed etcd dir with the LOCAL snapshot
  # (--snapshot short-circuited the s3:// fetch).
  run grep -F "etcdutl snapshot restore ${SNAPSHOT} --data-dir ${KUBEHZ_HANDOVER_ETCD_DIR}" "${CALLS}"
  assert_success

  # kubeadm init got the config AND the documented preflight ignore.
  run grep -F "kubeadm init --config ${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml --ignore-preflight-errors=DirAvailable--var-lib-etcd" "${CALLS}"
  assert_success

  # PKI was seeded byte-identical (kubeadm reuses it — the identity anchor).
  run cmp "${BUNDLE}/ca.crt" "${KUBEHZ_HANDOVER_K8S_DIR}/pki/ca.crt"
  assert_success
  run cmp "${BUNDLE}/sa.key" "${KUBEHZ_HANDOVER_K8S_DIR}/pki/sa.key"
  assert_success
  run cmp "${BUNDLE}/front-proxy-ca.key" "${KUBEHZ_HANDOVER_K8S_DIR}/pki/front-proxy-ca.key"
  assert_success
}

@test "handover receive: the generated kubeadm config carries endpoint DNS + encryption wiring" {
  _make_bundle
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success

  local cfg="${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml"
  [ -s "${cfg}" ]
  run cat "${cfg}"
  assert_output --partial "apiVersion: kubeadm.k8s.io/v1beta4"
  assert_output --partial 'controlPlaneEndpoint: "cl-001.kubermatic.kkp.kubehz.in.net:6443"'
  assert_output --partial '- "cl-001.kubermatic.kkp.kubehz.in.net"'
  assert_output --partial "name: encryption-provider-config"
  assert_output --partial "value: ${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"

  # The EncryptionConfiguration embeds the bundle key, mode 600.
  local enc="${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  run cat "${enc}"
  assert_output --partial "kind: EncryptionConfiguration"
  assert_output --partial "secret: dGVzdC1lbmNyeXB0aW9uLWtleQ=="
  run stat -c '%a' "${enc}"
  assert_output "600"
}

@test "handover receive: a CA re-minted by kubeadm init FAILS the verify (identity lost)" {
  _make_bundle
  # kubeadm clobbers the seeded CA — exactly what the verify must catch.
  etcdutl() { echo "etcdutl $*" >> "${CALLS}"; }
  kubeadm() {
    echo "kubeadm $*" >> "${CALLS}"
    printf 'freshly-minted-DIFFERENT-ca\n' > "${KUBEHZ_HANDOVER_K8S_DIR}/pki/ca.crt"
  }
  kubectl() { return 0; }
  export -f etcdutl kubeadm kubectl

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "CA hash mismatch"
  assert_output --partial "do NOT cut over"
}

@test "handover receive: refuses a non-empty /etc/kubernetes without --force" {
  _make_bundle
  _mock_node_binaries
  mkdir -p "${KUBEHZ_HANDOVER_K8S_DIR}"
  touch "${KUBEHZ_HANDOVER_K8S_DIR}/kubelet.conf"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "is not empty"
  assert_output --partial "--force"
  # Nothing ran against the node.
  [ ! -s "${CALLS}" ]
}

@test "handover receive: an incomplete bundle stops BEFORE any node mutation" {
  _make_bundle
  rm "${BUNDLE}/front-proxy-ca.crt"
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "missing or empty key: front-proxy-ca.crt"
  [ ! -s "${CALLS}" ]
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

@test "handover receive: s3 snapshot-location without an aws CLI errors with the --snapshot escape" {
  _make_bundle
  _mock_node_binaries
  # No --snapshot and no aws binary in the sandbox PATH.
  aws() { return 127; }
  unset -f aws 2>/dev/null || true
  PATH="/usr/bin:/bin" run handover::receive --bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "s3://kubehz-backups/handover/cl-001/snap.db"
  assert_output --partial "--snapshot"
}

@test "handover receive: etcdctl is the fallback when etcdutl is absent" {
  _make_bundle
  # Only etcdctl exists (older node) — plus the rest of the happy path.
  etcdctl() { echo "etcdctl $*" >> "${CALLS}"; }
  kubeadm() { echo "kubeadm $*" >> "${CALLS}"; }
  kubectl() { echo "kubectl $*" >> "${CALLS}"; return 0; }
  export -f etcdctl kubeadm kubectl

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  run grep -F "etcdctl snapshot restore ${SNAPSHOT} --data-dir ${KUBEHZ_HANDOVER_ETCD_DIR}" "${CALLS}"
  assert_success
}

@test "handover: write_kubeadm_config rejects a non-hostname endpoint-dns (injection guard)" {
  _make_bundle
  printf 'evil.example.com"\n  extraArgs:\n    - name: pwn' > "${BUNDLE}/endpoint-dns"
  run handover::write_kubeadm_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}" "${BATS_TEST_TMPDIR}/out.yaml"
  assert_failure
  assert_output --partial "not a plain hostname"
  [ ! -s "${BATS_TEST_TMPDIR}/out.yaml" ]
}

@test "handover: write_encryption_config rejects a non-base64 key (injection guard)" {
  _make_bundle
  printf 'k3y"\ninjected: yaml' > "${BUNDLE}/encryption-key"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_failure
  assert_output --partial "not base64"
}

# ── preseed: the thin kubeone variant ────────────────────

@test "handover preseed: scps exactly the six PKI files to the node's /etc/kubernetes/pki" {
  _make_bundle
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp

  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_success
  assert_output --partial "PKI pre-seeded on 203.0.113.7"

  # mkdir first, then ONE scp with all six PKI files, then the chmod.
  run grep -c '^scp ' "${CALLS}"
  assert_output "1"
  local scp_line
  scp_line=$(grep '^scp ' "${CALLS}")
  local key
  for key in ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key; do
    [[ "${scp_line}" == *"${BUNDLE}/${key}"* ]] || {
      echo "scp is missing ${key}: ${scp_line}" >&2
      return 1
    }
  done
  # …and NEVER the restore-only artifacts (the encryption key stays off the
  # wire until kubeone's own config delivery handles it).
  [[ "${scp_line}" != *encryption-key* ]]
  [[ "${scp_line}" != *snapshot-location* ]]
  [[ "${scp_line}" == *"root@203.0.113.7:/etc/kubernetes/pki/"* ]]

  # Key modes are tightened on the node afterwards.
  run grep -F "chmod 600 /etc/kubernetes/pki/ca.key" "${CALLS}"
  assert_success
}

@test "handover preseed: --node is required" {
  _make_bundle
  run handover::preseed --bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "node"
}

@test "handover preseed: an unreachable node fails before any scp" {
  _make_bundle
  ssh() { echo "ssh $*" >> "${CALLS}"; return 255; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp

  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_failure
  assert_output --partial "cannot reach"
  run grep -c '^scp ' "${CALLS}"
  assert_output "0"
}

# ── `lo kubehz assess` pretty-printer ────────────────────
# kubehz::render_assessment lives in libs/kubehz/main (it shares the status
# command's auth/config plumbing); the fetch itself is plain curl — the
# printer is the testable surface.

@test "assess: render_assessment prints aligned probes + feasibility" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  local response='{"ok":true,"data":{"assessedAt":"2026-07-29T09:00:00Z","assessment":{"collectedAt":"2026-07-29T08:58:00Z","k8sVersion":"v1.33.2","datastore":"etcd","etcdReachable":true,"capiManaged":true,"cni":"cilium","podCidr":"10.244.0.0/16","serviceCidr":"10.96.0.0/12","storageClasses":[{"name":"hcloud-volumes","provisioner":"csi.hetzner.cloud","isDefault":true}],"pvSummary":{"count":2,"totalGi":40,"byProvisioner":{"csi.hetzner.cloud":{"count":2,"totalGi":40}}},"loadBalancers":1,"webhooks":{"validating":3,"mutating":1},"cpUsage":{"nodes":5,"cpNodes":3,"etcdDbBytes":null}},"feasibility":{"path":"restore","reasons":["datastore=etcd and a snapshot is obtainable"],"warnings":["1 provider-coupled StorageClass (csi.hetzner.cloud)"]}}}'

  run kubehz::render_assessment "test.kubehz.dev" "${response}"
  assert_success
  assert_output --partial "kubehz assessment — test.kubehz.dev"
  assert_output --partial "collected 2026-07-29T08:58:00Z"
  assert_output --partial "v1.33.2"
  assert_output --partial "etcd · reachable"
  assert_output --partial "pause the CAPI controllers"
  assert_output --partial "cilium"
  assert_output --partial "pods 10.244.0.0/16 · services 10.96.0.0/12"
  assert_output --partial "1 classes · 2 PVs · 40Gi"
  assert_output --partial "csi.hetzner.cloud — 2 PVs · 40Gi"
  assert_output --partial "3 validating · 1 mutating"
  assert_output --partial "5 total · 3 control-plane"
  assert_output --partial "Feasibility: restore"
  assert_output --partial "datastore=etcd and a snapshot is obtainable"
  assert_output --partial "provider-coupled StorageClass"
}

@test "assess: render_assessment says so plainly when no assessment exists yet" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::render_assessment "test.kubehz.dev" '{"data":{"assessment":null,"assessedAt":null,"feasibility":null}}'
  assert_success
  assert_output --partial "No assessment recorded for test.kubehz.dev yet"
  assert_output --partial "24h"
}

# ── dispatch: `lo kubehz handover <cmd>` resolves ────────

@test "handover: kubehz::handover dispatches receive via the argsh usage table" {
  _make_bundle
  _mock_node_binaries

  run kubehz::handover receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  assert_output --partial "handover: receive complete"
}
