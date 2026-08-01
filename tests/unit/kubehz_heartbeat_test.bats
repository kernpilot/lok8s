#!/usr/bin/env bats
# kubehz_heartbeat_test.bats — the in-cluster heartbeat agent (CronJob) script.
#
# Guards the POSIX-sh heartbeat embedded in
# .lok8s/libs/kubehz/manifests/agent/cronjob.yaml:
#   - node IDENTITY: names come from a targeted jsonpath, NEVER from a greedy
#     sed that also scrapes .status.runtimeHandlers[].name ("runc") or
#     .status.volumesAttached[].name (CSI handles) on modern k8s;
#   - per-node ROLES: control-plane vs worker, derived from the node-role label
#     KEY presence (an absent label must NOT read as control-plane);
#   - kubernetesVersion: the SERVER version, not the client/kubectl-image tag;
#   - the per-node "instanceType" field (node.kubernetes.io/instance-type, e.g.
#     Hetzner CCM cx32/cpx41/…) and its fail-soft empty-string fallback.
#
# The SHIPPED manifest is the fixture: the script is extracted with yq and run
# against a stubbed kubectl/curl, so a regression in the real heartbeat — a
# broken field, invalid JSON, a `set -e` abort — fails here. The stub is
# deliberately REALISTIC (two nodes, differing roles, runtimeHandlers +
# volumesAttached, distinct client/server versions) so this whole bug class —
# which slipped past a trivial single-node stub while the agent was DOA — is
# caught.

setup() {
  load "../test_helper"
  setup_tmpdir

  AGENT_DIR="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent"
  CRONJOB="${AGENT_DIR}/cronjob.yaml"

  # Extract the embedded heartbeat (containers[0].command[2] — the `-c` script).
  HEARTBEAT="${BATS_TEST_TMPDIR}/heartbeat.sh"
  command yq -r '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' \
    "${CRONJOB}" > "${HEARTBEAT}"

  # Stubs: a realistic TWO-node cluster — a control-plane node "cp-1" and a
  # worker "worker-1". The `get nodes -o json` fixture carries the modern-k8s
  # traps that made the shipped agent DOA: .status.runtimeHandlers[].name
  # ("runc"/"crun", present on k8s ≳1.27) and .status.volumesAttached[].name (a
  # CSI handle). The FIX extracts node names via a targeted jsonpath
  # (`get nodes -o jsonpath=…`); a regression to the greedy `… -o json | sed
  # …"name"…` scrapes those ghosts → `get node runc` NotFounds → set -e aborts
  # (caught by assert_success). STUB_ITYPE sets cp-1's instance-type label
  # (unset → cx32, empty string → unlabeled). curl captures the POSTed heartbeat
  # body (-d) to STUB_PAYLOAD_OUT so the test can assert the wire payload.
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
kubectl() {
  case "$*" in
    # An enrolled agent identity exists: the heartbeat reads its agent-token
    # back and authenticates with it. The empty-token guard skips the POST when
    # no token is readable, so the fixture must supply one for the heartbeat to
    # fire. claim-code is left to the default (empty) → the best-effort
    # self-register stays a no-op here.
    *"get secret kubehz-agent"*"agent-token"*) printf 'khz_agt_test' | base64 | tr -d '\n' ;;

    # kubernetesVersion = the SERVER version. The FIX reads the apiserver
    # /version endpoint (a single "gitVersion" → unambiguous). `version -o json`
    # below is the regression guard: it carries BOTH a client (v1.31.4) and a
    # DIFFERENT server (v1.35.5), so a revert to `version -o json | … | head -1`
    # (which grabs the CLIENT) is caught by the v1.35.5 assertion.
    *"/version"*) printf '{"major":"1","minor":"35","gitVersion":"v1.35.5","platform":"linux/amd64"}\n' ;;
    "version -o json") printf '{\n  "clientVersion": { "gitVersion": "v1.31.4" },\n  "serverVersion": { "gitVersion": "v1.35.5" }\n}\n' ;;

    # Node NAMES (the FIX): a targeted jsonpath over .items[].metadata.name.
    "get nodes -o jsonpath="*) printf 'cp-1\nworker-1\n' ;;
    # Realistic multi-node dump WITH the greedy-sed traps (runtimeHandlers +
    # volumesAttached). Dormant unless the extraction regresses to `-o json`.
    "get nodes -o json") printf '%s\n' '{"items":[{"metadata":{"name":"cp-1"},"status":{"runtimeHandlers":[{"name":"runc"},{"name":"crun"}],"volumesAttached":[{"name":"kubernetes.io/csi/csi.hetzner.cloud^0001"}]}},{"metadata":{"name":"worker-1"},"status":{"runtimeHandlers":[{"name":"runc"}]}}]}' ;;

    # Per-node queries. A name that isn't a real node (what greedy-sed scrapes)
    # NotFounds like real kubectl → the old code's unguarded status=$(…) aborts.
    "get node "*)
      _n=$3
      case "${_n}" in
        cp-1|worker-1) : ;;
        *) echo "Error from server (NotFound): nodes \"${_n}\" not found" >&2; return 1 ;;
      esac
      case "$*" in
        *"jsonpath={.status.conditions"*)
          [ -n "${STUB_READY_STATUS_FAIL:-}" ] && return 1
          printf 'True' ;;
        *"instance-type}"*)
          [ -n "${STUB_ITYPE_FAIL:-}" ] && return 1
          if [ "${_n}" = "cp-1" ]; then printf '%s' "${STUB_ITYPE-cx32}"; else printf 'cpx41'; fi
          return 0 ;;
        # roles source: the labels map, rendered as JSON by the pinned kubectl
        # (1.31). cp-1 carries the control-plane label KEY (empty value);
        # worker-1 does not — so absent MUST read as worker, not control-plane.
        *"jsonpath={.metadata.labels}"*)
          if [ "${_n}" = "cp-1" ]; then
            printf '%s' '{"kubernetes.io/hostname":"cp-1","node-role.kubernetes.io/control-plane":"","node.kubernetes.io/instance-type":"cx32"}'
          else
            printf '%s' '{"kubernetes.io/hostname":"worker-1","node.kubernetes.io/instance-type":"cpx41"}'
          fi ;;
        *) printf '' ;;
      esac ;;

    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;

    # ── Assessment fixtures (handover contract §1) ──
    # Gate marker: the annotations map on the kubehz-agent-config ConfigMap.
    # STUB_MARKER holds the JSON annotations; STUB_MARKER_FORBIDDEN simulates
    # an old install without the marker RBAC (read fails -> fail-closed).
    "get configmap kubehz-agent-config"*)
      [ -z "${STUB_MARKER_FORBIDDEN:-}" ] || return 1
      printf '%s' "${STUB_MARKER:-}" ;;
    "annotate "*) printf '%s\n' "$*" >> "${STUB_ANNOTATE_LOG}"; return 0 ;;
    # etcd static pods: default one Ready pod (datastore=etcd, reachable).
    # STUB_ETCD_READY="" = no etcd pods visible (kine/managed-CP edges).
    *"-l component=etcd"*) printf '%s' "${STUB_ETCD_READY-True }" ;;
    # kube-apiserver pod command+args (jsonpath renders the JSON array);
    # empty = pod not visible (managed CP).
    *"-l component=kube-apiserver"*) printf '%s' "${STUB_APISERVER_PODS-}" ;;
    "get crd clusters.cluster.x-k8s.io"*)
      [ -n "${STUB_CAPI:-}" ] && return 0 || return 1 ;;
    "get daemonsets -n kube-system"*)
      if [ -n "${STUB_DAEMONSETS+x}" ]; then printf '%s' "${STUB_DAEMONSETS}"; else printf 'cilium\nkube-proxy\n'; fi ;;
    "get configmap kubeadm-config"*)
      printf '  networking:\n    dnsDomain: cluster.local\n    podSubnet: 10.244.0.0/16\n    serviceSubnet: 10.96.0.0/12\n' ;;
    # Prefix-glob matches: the assessment probes append --request-timeout=5s
    # (a stalled apiserver degrades ONE probe, never the whole beat).
    # STUB_SC_JSON overrides the StorageClass dump (the >50-cap fixture).
    "get storageclasses -o json"*)
      if [ -n "${STUB_SC_JSON:-}" ]; then printf '%s\n' "${STUB_SC_JSON}"; else
        printf '%s\n' '{"items":[{"metadata":{"name":"hcloud-volumes","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"csi.hetzner.cloud"},{"metadata":{"name":"ceph-block"},"provisioner":"rook-ceph.rbd.csi.ceph.com"}]}'
      fi ;;
    "get pv -o json"*)
      printf '%s\n' '{"items":[{"metadata":{"name":"pv-1","annotations":{"pv.kubernetes.io/provisioned-by":"csi.hetzner.cloud"}},"spec":{"capacity":{"storage":"10Gi"}}},{"metadata":{"name":"pv-2"},"spec":{"csi":{"driver":"csi.hetzner.cloud"},"capacity":{"storage":"30Gi"}}},{"metadata":{"name":"pv-3","annotations":{"pv.kubernetes.io/provisioned-by":"rook-ceph.rbd.csi.ceph.com"}},"spec":{"capacity":{"storage":"512Mi"}}}]}' ;;
    "get services -A -o json"*)
      printf '%s\n' '{"items":[{"spec":{"type":"LoadBalancer"}},{"spec":{"type":"ClusterIP"}},{"spec":{"type":"LoadBalancer"}}]}' ;;
    "get validatingwebhookconfigurations -o json"*) printf '%s\n' '{"items":[{},{},{}]}' ;;
    "get mutatingwebhookconfigurations -o json"*) printf '%s\n' '{"items":[{}]}' ;;
    "get nodes -l node-role.kubernetes.io/control-plane -o name"*) printf 'node/cp-1\n' ;;
    "get nodes -l node-role.kubernetes.io/master -o name"*) printf '' ;;

    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  # Capture the POSTed body (-d); every call overwrites STUB_PAYLOAD_OUT so a
  # retry's payload is what the test asserts on. Each invocation is logged to
  # STUB_CURL_LOG (call-count asserts). STUB_CURL_FAIL_ASSESS=1 fails any POST
  # whose body carries an assessment (the api-zod-400 simulation) — the plain
  # retry then succeeds.
  _body=""
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "-d" ]; then _body="$2"; shift; fi
    shift
  done
  echo "curl" >> "${STUB_CURL_LOG}"
  [ -z "${_body}" ] || printf '%s' "${_body}" > "${STUB_PAYLOAD_OUT}"
  if [ -n "${STUB_CURL_FAIL_ASSESS:-}" ]; then
    case "${_body}" in *'"assessment"'*) return 22 ;; esac
  fi
  # The heartbeat response body — STUB_RESPONSE drives the actions[] tests.
  printf '%s' "${STUB_RESPONSE:-}"
  return 0
}
STUBS_EOF

  # Prepend the stubs to the real script → a self-contained, runnable heartbeat.
  RUNNER="${BATS_TEST_TMPDIR}/run.sh"
  cat "${stubs}" "${HEARTBEAT}" > "${RUNNER}"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
  export STUB_PAYLOAD_OUT="${BATS_TEST_TMPDIR}/payload.json"
  export STUB_ANNOTATE_LOG="${BATS_TEST_TMPDIR}/annotate.log"
  export STUB_CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
}

teardown() {
  teardown_tmpdir
}

# ── The shipped script is valid POSIX sh ─────────────────

@test "heartbeat: the embedded script passes sh -n" {
  run sh -n "${HEARTBEAT}"
  assert_success
}

# ── The agent kustomization renders with the instanceType field ─

@test "heartbeat: agent kustomization renders and carries the instanceType field" {
  run command kustomize build "${AGENT_DIR}"
  assert_success
  assert_output --partial "kind: CronJob"
  assert_output --partial "name: kubehz-heartbeat"
  # The per-node field is present in the rendered heartbeat script.
  assert_output --partial '\"instanceType\":\"'
  # …sourced from the GA label key, not the deprecated beta alias (pins the key
  # so a silent swap to beta.kubernetes.io/instance-type fails here).
  assert_output --partial 'node\.kubernetes\.io/instance-type'
}

# ── Labeled node → instanceType is the label value ───────

@test "heartbeat: a labeled node reports its instanceType and emits valid JSON" {
  export STUB_ITYPE="cx32"
  run bash "${RUNNER}"
  assert_success

  # The captured payload is valid JSON …
  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  # … and the node carries instanceType: cx32 alongside the existing fields.
  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output "cx32"
  run command jq -r '.nodes[0].name' "${STUB_PAYLOAD_OUT}"
  assert_output "cp-1"
}

# ── Node status: canonical vocabulary, never the raw condition value ──────

@test "heartbeat: node status maps the raw Ready-condition True to \"Ready\" on the wire" {
  # Real kubectl's jsonpath on conditions[?(@.type=="Ready")].status yields the
  # literal condition value ("True") — the stub mirrors that. The platform
  # counts ready nodes by an exact "Ready" match, so the agent must translate
  # (0/12-on-a-healthy-pilot bug, 2026-08-01).
  run bash "${RUNNER}"
  assert_success
  run command jq -r '[.nodes[].status] | unique | .[]' "${STUB_PAYLOAD_OUT}"
  assert_output "Ready"
}

@test "heartbeat: a failed status probe reports \"Unknown\", never an empty string" {
  export STUB_READY_STATUS_FAIL=1
  run bash "${RUNNER}"
  assert_success
  run command jq -r '.nodes[0].status' "${STUB_PAYLOAD_OUT}"
  assert_output "Unknown"
}

# ── Unlabeled node → instanceType is "" (fail-soft) ──────

@test "heartbeat: an unlabeled node reports instanceType \"\" and still emits valid JSON" {
  export STUB_ITYPE=""
  run bash "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  # The field is present but empty — never omitted, never a broken heartbeat.
  run command jq -r '.nodes[0] | has("instanceType")' "${STUB_PAYLOAD_OUT}"
  assert_output "true"
  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output ""
}

# ── instance-type kubectl error → "" (fail-soft under set -e) ──

@test "heartbeat: an instance-type kubectl error is fail-soft (instanceType \"\", valid JSON)" {
  # kubectl returns non-zero for the instance-type lookup — the `|| itype=""`
  # guard must absorb it so `set -e` does not abort the whole heartbeat.
  export STUB_ITYPE_FAIL=1
  run bash "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output ""
}

# ── Node IDENTITY: names are the metadata names, never the greedy-sed ghosts ─
# P0 regression guard: on modern k8s `get nodes -o json` also carries
# .status.runtimeHandlers[].name ("runc"/"crun") and .status.volumesAttached[]
# .name (CSI handles). A greedy sed scraped those as node names, then
# `kubectl get node runc` NotFounded and `set -e` killed the beat before the
# POST — the agent was DOA on every real cluster.

@test "heartbeat: node names are EXACTLY the two metadata names (never a runtimeHandler/CSI handle)" {
  run bash "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  run command jq -rc '[.nodes[].name]' "${STUB_PAYLOAD_OUT}"
  assert_output '["cp-1","worker-1"]'

  # None of the greedy-sed ghosts may surface as a node. (Scoped to the nodes
  # array: the assessment block legitimately names CSI provisioners.)
  run grep -F '"name":"runc"' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run command jq -r '[.nodes[].name] | join(" ")' "${STUB_PAYLOAD_OUT}"
  refute_output --partial "runc"
  refute_output --partial "csi"
}

# ── Per-node ROLES: control-plane vs worker (not all control-plane) ──
# The role labels carry an EMPTY value, so a per-key jsonpath can't tell
# "absent" from "present-but-empty" — the old `… && echo control-plane` fired
# for EVERY node. The fix tests the label KEY's presence; absent → worker.

@test "heartbeat: roles are per-node — control-plane for cp-1, worker for worker-1" {
  run bash "${RUNNER}"
  assert_success

  run command jq -r '.nodes[] | select(.name == "cp-1") | .roles' "${STUB_PAYLOAD_OUT}"
  assert_output "control-plane"
  run command jq -r '.nodes[] | select(.name == "worker-1") | .roles' "${STUB_PAYLOAD_OUT}"
  assert_output "worker"

  # BOTH roles must be present — proof the worker is not misread as control-plane.
  run command jq -rc '[.nodes[].roles] | unique' "${STUB_PAYLOAD_OUT}"
  assert_output '["control-plane","worker"]'
}

# ── kubernetesVersion is the SERVER version, not the client image tag ──
# `kubectl version -o json` carries BOTH clientVersion (= the alpine/k8s image
# tag) and serverVersion; a first-match grabbed the client (v1.31.4). The fix
# reads the apiserver /version endpoint → the true server version.

@test "heartbeat: kubernetesVersion is the SERVER version (v1.35.5), not the client (v1.31.4)" {
  run bash "${RUNNER}"
  assert_success

  run command jq -r '.kubernetes.version' "${STUB_PAYLOAD_OUT}"
  assert_output "v1.35.5"

  # The client/kubectl-image version must not leak into the payload anywhere.
  run grep -F "v1.31.4" "${STUB_PAYLOAD_OUT}"
  assert_failure
}

# ── Assessment (handover contract §1): shape ─────────────
# With NO marker annotation (first ever run), the heartbeat includes an
# `assessment` whose keys match the contract VERBATIM, populated from the
# kubectl-only probes.

@test "assessment: first run includes a contract-shaped assessment and moves the marker" {
  run bash "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_success

  # The key set is EXACTLY the contract §1 field list — a drifted probe
  # (renamed/missing/extra field) fails here.
  run command jq -r '.assessment | keys | sort | join(" ")' "${STUB_PAYLOAD_OUT}"
  assert_output "capiManaged cni collectedAt cpUsage datastore etcdReachable k8sVersion loadBalancers podCidr pvSummary serviceCidr storageClasses webhooks"

  run command jq -r '.assessment.k8sVersion' "${STUB_PAYLOAD_OUT}"
  assert_output "v1.35.5"
  run command jq -r '.assessment.datastore' "${STUB_PAYLOAD_OUT}"
  assert_output "etcd"
  run command jq -r '.assessment.etcdReachable' "${STUB_PAYLOAD_OUT}"
  assert_output "true"
  run command jq -r '.assessment.capiManaged' "${STUB_PAYLOAD_OUT}"
  assert_output "false"
  run command jq -r '.assessment.cni' "${STUB_PAYLOAD_OUT}"
  assert_output "cilium"
  run command jq -r '.assessment.podCidr' "${STUB_PAYLOAD_OUT}"
  assert_output "10.244.0.0/16"
  run command jq -r '.assessment.serviceCidr' "${STUB_PAYLOAD_OUT}"
  assert_output "10.96.0.0/12"
  run command jq -r '.assessment.loadBalancers' "${STUB_PAYLOAD_OUT}"
  assert_output "2"
  run command jq -r '.assessment.webhooks.validating' "${STUB_PAYLOAD_OUT}"
  assert_output "3"
  run command jq -r '.assessment.webhooks.mutating' "${STUB_PAYLOAD_OUT}"
  assert_output "1"
  run command jq -r '.assessment.cpUsage.nodes' "${STUB_PAYLOAD_OUT}"
  assert_output "2"
  run command jq -r '.assessment.cpUsage.cpNodes' "${STUB_PAYLOAD_OUT}"
  assert_output "1"
  run command jq -r '.assessment.cpUsage.etcdDbBytes' "${STUB_PAYLOAD_OUT}"
  assert_output "null"
  run command jq -rc '.assessment.storageClasses[0]' "${STUB_PAYLOAD_OUT}"
  assert_output '{"name":"hcloud-volumes","provisioner":"csi.hetzner.cloud","isDefault":true}'

  # Delivered → the 24h marker moved (epoch annotation on the ConfigMap).
  run grep -F "kubehz.cloud/last-assessment=" "${STUB_ANNOTATE_LOG}"
  assert_success
}

# ── Assessment: pvSummary grouping + unit conversion ─────

@test "assessment: pvSummary groups by provisioner and converts Mi/Gi to Gi" {
  run bash "${RUNNER}"
  assert_success

  run command jq -r '.assessment.pvSummary.count' "${STUB_PAYLOAD_OUT}"
  assert_output "3"
  run command jq -r '.assessment.pvSummary.totalGi' "${STUB_PAYLOAD_OUT}"
  assert_output "40.5"
  run command jq -r '.assessment.pvSummary.byProvisioner["csi.hetzner.cloud"].count' "${STUB_PAYLOAD_OUT}"
  assert_output "2"
  run command jq -r '.assessment.pvSummary.byProvisioner["csi.hetzner.cloud"].totalGi' "${STUB_PAYLOAD_OUT}"
  assert_output "40"
  run command jq -r '.assessment.pvSummary.byProvisioner["rook-ceph.rbd.csi.ceph.com"].totalGi' "${STUB_PAYLOAD_OUT}"
  assert_output "0.5"
}

# ── Assessment: array caps (the api zod schema caps at 50) ──
# An uncapped storageClasses list 400s the WHOLE heartbeat server-side; the
# agent must cap at 50 (logged, never a silent drop) so the beat always fits.

@test "assessment: >50 StorageClasses are capped to 50 and the beat still succeeds" {
  export STUB_SC_JSON
  STUB_SC_JSON=$(command jq -nc '{items: [range(60) | {metadata: {name: "sc-\(.)"}, provisioner: "csi.example.com"}]}')

  run bash "${RUNNER}"
  assert_success
  assert_output --partial "first 50 of 60 StorageClasses"

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success
  run command jq -r '.assessment.storageClasses | length' "${STUB_PAYLOAD_OUT}"
  assert_output "50"
  run command jq -r '.assessment.storageClasses[0].name' "${STUB_PAYLOAD_OUT}"
  assert_output "sc-0"
}

# ── Heartbeat POST failure with an assessment attached: retry WITHOUT it ──
# The regression this pins: an unguarded RESPONSE=$(curl -f …) under set -e
# aborted BEFORE the marker write, so a rejected (e.g. over-cap → zod 400)
# assessment resent deterministically every 5 minutes and the heartbeat was
# PERMANENTLY lost. The fix retries once without the assessment so the plain
# beat always lands, then moves the marker to break the resend loop.

@test "heartbeat: a rejected assessment POST retries ONCE as a plain heartbeat (beat never lost)" {
  export STUB_CURL_FAIL_ASSESS=1

  run bash "${RUNNER}"
  assert_success
  assert_output --partial "retrying once without it"

  # Exactly two heartbeat POSTs: the assessment one (failed) + the plain retry.
  run grep -c '^curl$' "${STUB_CURL_LOG}"
  assert_output "2"

  # The payload that LANDED (last capture) is the plain beat — full heartbeat
  # fields, no assessment.
  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success
  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
  assert_output "test.example.com"
  run command jq -rc '[.nodes[].name]' "${STUB_PAYLOAD_OUT}"
  assert_output '["cp-1","worker-1"]'

  # The marker moved anyway — a known-rejected assessment must NOT resend
  # every 5 minutes; the next attempt waits for 24h or a platform request.
  run grep -F "kubehz.cloud/last-assessment=" "${STUB_ANNOTATE_LOG}"
  assert_success
}

# ── Assessment gating: at most once per 24h ──────────────

@test "assessment: a fresh (<24h) marker suppresses the assessment entirely" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'

  run bash "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  # …and the marker is NOT rewritten on a suppressed tick.
  [ ! -s "${STUB_ANNOTATE_LOG}" ]
}

@test "assessment: a stale (>24h) marker sends a fresh assessment" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 90000 ))'"}'

  run bash "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_success
  run grep -F "kubehz.cloud/last-assessment=" "${STUB_ANNOTATE_LOG}"
  assert_success
}

# ── Assessment gating: platform-requested (actions[] assess) ──

@test "assessment: assess-requested=true overrides a fresh marker and is cleared after the send" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/assess-requested":"true"}'

  run bash "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_success
  # The served request is removed (annotate with the trailing-dash form).
  run grep -F "kubehz.cloud/assess-requested-" "${STUB_ANNOTATE_LOG}"
  assert_success
}

@test "assessment: an 'assess' in the response actions[] arms the marker for the NEXT tick" {
  local now
  now=$(date -u +%s)
  # Fresh marker → nothing sent THIS tick; the response demands one.
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'
  export STUB_RESPONSE='{"status":"ok","actions":["assess"]}'

  run bash "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run grep -F "kubehz.cloud/assess-requested=true" "${STUB_ANNOTATE_LOG}"
  assert_success
}

# ── Assessment: datastore edges (kine / unknown) ─────────

@test "assessment: no etcd pods + unix-socket --etcd-servers reads as kine" {
  export STUB_ETCD_READY=""
  export STUB_APISERVER_PODS='["kube-apiserver","--etcd-servers=unix:///var/run/kine.sock","--authorization-mode=Node,RBAC"]'

  run bash "${RUNNER}"
  assert_success

  run command jq -r '.assessment.datastore' "${STUB_PAYLOAD_OUT}"
  assert_output "kine"
  # /readyz has no "[+]etcd ok" line in the stub → not reachable.
  run command jq -r '.assessment.etcdReachable' "${STUB_PAYLOAD_OUT}"
  assert_output "false"
}

@test "assessment: neither etcd nor apiserver pods visible reads as unknown" {
  export STUB_ETCD_READY=""
  export STUB_APISERVER_PODS=""

  run bash "${RUNNER}"
  assert_success

  run command jq -r '.assessment.datastore' "${STUB_PAYLOAD_OUT}"
  assert_output "unknown"
}

@test "assessment: an apiserver pod WITH an https --etcd-servers reads as external etcd" {
  export STUB_ETCD_READY=""
  export STUB_APISERVER_PODS='["kube-apiserver","--etcd-servers=https://10.0.0.5:2379"]'

  run bash "${RUNNER}"
  assert_success

  run command jq -r '.assessment.datastore' "${STUB_PAYLOAD_OUT}"
  assert_output "etcd"
}

# ── Assessment: fail-closed when the marker is unreadable ──
# An old install without the kubehz-agent-marker RBAC must NOT spam a full
# assessment every 5-minute tick — an unreadable marker sends nothing.

@test "assessment: an unreadable marker (missing RBAC) sends NO assessment" {
  export STUB_MARKER_FORBIDDEN=1

  run bash "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  # The heartbeat itself still went out intact.
  run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
  assert_output "test.example.com"
}

# ── Assessment: RBAC ships with the kustomization ────────

@test "assessment: the agent kustomization renders the probe RBAC + marker Role" {
  run command kustomize build "${AGENT_DIR}"
  assert_success
  # Marker Role (get/patch on the ONE ConfigMap) + its binding.
  assert_output --partial "name: kubehz-agent-marker"
  # Cluster-scoped probe reads.
  assert_output --partial "storageclasses"
  assert_output --partial "persistentvolumes"
  assert_output --partial "validatingwebhookconfigurations"
  # The CAPI CRD probe is resourceNames-scoped to the one CRD.
  assert_output --partial "clusters.cluster.x-k8s.io"
  # kube-system: daemonsets (CNI guess) + the kubeadm-config ConfigMap.
  assert_output --partial "daemonsets"
  assert_output --partial "kubeadm-config"
}
