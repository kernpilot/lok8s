#!/usr/bin/env bats
# kubehz_heartbeat_test.bats — the in-cluster heartbeat agent (CronJob) script.
#
# Guards the POSIX-sh heartbeat embedded in
# .lok8s/libs/kubehz/manifests/agent/cronjob.yaml:
#   - node IDENTITY: names come from jq over .items[].metadata.name, NEVER
#     from a greedy sed that also scrapes .status.runtimeHandlers[].name
#     ("runc") or .status.volumesAttached[].name (CSI handles) on modern k8s;
#   - ONE node list call for all nodes. The kubectl call count does not
#     grow with the node count, so a large cluster finishes the tick before
#     activeDeadlineSeconds (120 s);
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
#
# The pod runs the script with /bin/sh of its image (alpine/k8s: busybox
# ash). The cases run it with `bash --posix`, the nearest shell that every
# test host has: bash in POSIX mode, not busybox ash. A construct that bash
# accepts in POSIX mode and ash does not is not caught here (shellcheck -s
# sh on the script is the other check).

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
  # worker "worker-1". The node list fixture (`get --raw /api/v1/nodes`)
  # carries the modern-k8s
  # traps that made the shipped agent DOA: .status.runtimeHandlers[].name
  # ("runc"/"crun", present on k8s ≳1.27) and .status.volumesAttached[].name (a
  # CSI handle). The script reads the names with jq from .items[].metadata.name;
  # a greedy sed over the JSON would report those ghosts as nodes.
  # STUB_ITYPE sets cp-1's instance-type label (unset → cx32, empty string →
  # unlabeled). STUB_READY_STATUS sets the Ready condition of both nodes;
  # STUB_READY_MISSING=1 leaves it out. STUB_NODES_FILE serves a generated
  # node list instead (the node-list tests). STUB_NODES_BODY answers the node list
  # with exactly that body and exit 0 (an empty or broken answer). STUB_KUBECTL_LOG records every kubectl
  # call, and STUB_KUBECTL_DELAY makes each call sleep (a slow apiserver).
  # curl captures the POSTed heartbeat body (-d) to STUB_PAYLOAD_OUT so the
  # test can assert the wire payload.
  # STUB_STATE_FILE (a JSON object) is the annotation store of
  # kubehz-agent-state: the marker read prints it, and an annotate that is
  # not denied changes it as kubectl does (key=value sets, key- removes, all
  # or nothing). It persists from one run of the script to the next, so a
  # case can run several ticks. Without it, the read prints STUB_MARKER and
  # an annotate changes nothing. The denials: STUB_GATE_DENIED refuses the
  # first annotate of a run that writes kubehz.cloud/last-assessment (the
  # send gate), STUB_WRITEBACK_DENIED refuses any later one (the write-back),
  # and STUB_ANNOTATE_DENIED refuses every annotate. STUB_EVENTS_LOG gets
  # each annotate and each heartbeat POST in their order.
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
# timeout: the pod's `timeout -k KILL SECS CMD ARGS` (the BusyBox applet and
# GNU coreutils take the same form). It logs its whole argv, so a case sees
# the bound of each call, and runs the command as a shell function, so the
# kubectl stub below answers it. A kubectl call that does not run under it
# goes to STUB_UNBOUND_LOG. A command that contains STUB_TIMEOUT_MATCH times
# out instead: TERM kills it (rc 143), and it prints nothing.
timeout() {
  printf '%s\n' "$*" >> "${STUB_TIMEOUT_LOG}"
  case "$*" in
    *"${STUB_TIMEOUT_MATCH:-<none>}"*) return 143 ;;
  esac
  [ "$1" != "-k" ] || shift 2
  shift
  local STUB_BOUND=1
  "$@"
}
# _stub_store_annotate ARGS: apply the annotate ARGS to STUB_STATE_FILE.
# Without --overwrite, a key that holds another value fails the call, as in
# kubectl. Nothing changes on a failure.
_stub_store_annotate() {
  local overwrite="" arg cur
  cur=$(cat "${STUB_STATE_FILE}")
  for arg in "$@"; do
    case "${arg}" in
      --overwrite) overwrite=1 ;;
      annotate|configmap|kubehz-agent-state|-n|kubehz-system) ;;
      *=*)
        if [ -z "${overwrite}" ] && printf '%s' "${cur}" | command jq -e --arg k "${arg%%=*}" --arg v "${arg#*=}" \
            'has($k) and .[$k] != $v' >/dev/null; then
          echo "error: --overwrite is false but found the following declared annotation(s): '${arg%%=*}' already has a value" >&2
          return 1
        fi
        cur=$(printf '%s' "${cur}" | command jq -c --arg k "${arg%%=*}" --arg v "${arg#*=}" '.[$k] = $v') ;;
      *-) cur=$(printf '%s' "${cur}" | command jq -c --arg k "${arg%-}" 'del(.[$k])') ;;
      *) echo "stub: unknown annotate argument ${arg}" >&2; return 1 ;;
    esac
  done
  printf '%s\n' "${cur}" > "${STUB_STATE_FILE}"
}
kubectl() {
  argv_log kubectl "$@"
  [ -n "${STUB_BOUND:-}" ] || printf '%s\n' "$*" >> "${STUB_UNBOUND_LOG}"
  [ -z "${STUB_KUBECTL_LOG:-}" ] || printf '%s\n' "$*" >> "${STUB_KUBECTL_LOG}"
  [ -z "${STUB_KUBECTL_DELAY:-}" ] || sleep "${STUB_KUBECTL_DELAY}"
  # In the pod, kubectl reads the in-cluster config. A client-config flag
  # makes it skip that config and dial localhost:8080 (proven in a pod
  # with alpine/k8s 1.31.4): the stub refuses the same way.
  local arg
  for arg in "$@"; do
    case "${arg}" in
      --request-timeout|--request-timeout=*|--server|--server=*|-s|--kubeconfig|--kubeconfig=*|\
      --context|--context=*|--cluster|--cluster=*|--user|--user=*|--token|--token=*|\
      --username|--username=*|--password|--password=*|--certificate-authority|--certificate-authority=*|\
      --client-certificate|--client-certificate=*|--client-key|--client-key=*|\
      --insecure-skip-tls-verify|--insecure-skip-tls-verify=*|--tls-server-name|--tls-server-name=*|\
      --as|--as=*|--as-group|--as-group=*|--as-uid|--as-uid=*|--disable-compression|--disable-compression=*)
        echo 'The connection to the server localhost:8080 was refused - did you specify the right host or port?' >&2
        return 1 ;;
    esac
  done
  case "$*" in
    # An enrolled agent identity exists: the heartbeat reads its agent-token
    # back and authenticates with it. The empty-token guard skips the POST when
    # no token is readable, so the fixture must supply one for the heartbeat to
    # fire. claim-code is left to the default (empty) → the best-effort
    # self-register stays a no-op here.
    *"get secret kubehz-agent"*"agent-token"*) printf '%s' "${STUB_AGENT_TOKEN:-khz_agt_test}" | base64 | tr -d '\n' ;;
    # The claim code: empty unless STUB_CLAIM_CODE sets one. With one, the
    # bootstrap sends its self-register.
    *"get secret kubehz-agent"*"claim-code"*) printf '%s' "${STUB_CLAIM_CODE:-}" | base64 | tr -d '\n' ;;
    # The identity Secret exists, unless STUB_SECRET_ABSENT: then the
    # bootstrap creates it (the values on stdin are dropped here).
    "get secret kubehz-agent -n kubehz-system")
      [ -z "${STUB_SECRET_ABSENT:-}" ] || return 1 ;;
    "create secret generic kubehz-agent -n kubehz-system --from-env-file=/dev/stdin")
      cat > /dev/null ;;

    # kubernetesVersion = the SERVER version. The FIX reads the apiserver
    # /version endpoint (a single "gitVersion" → unambiguous). `version -o json`
    # below is the regression guard: it carries BOTH a client (v1.31.4) and a
    # DIFFERENT server (v1.35.5), so a revert to `version -o json | … | head -1`
    # (which grabs the CLIENT) is caught by the v1.35.5 assertion.
    *"/version"*) printf '{"major":"1","minor":"35","gitVersion":"v1.35.5","platform":"linux/amd64"}\n' ;;
    "version -o json") printf '{\n  "clientVersion": { "gitVersion": "v1.31.4" },\n  "serverVersion": { "gitVersion": "v1.35.5" }\n}\n' ;;

    # The node list: ONE call for all nodes. The default fixture is a
    # realistic two-node dump WITH the greedy-sed traps (runtimeHandlers +
    # volumesAttached). cp-1 carries the control-plane label KEY with an
    # empty value; worker-1 does not, so absent MUST read as worker.
    "get --raw /api/v1/nodes")
      [ -z "${STUB_NODES_FAIL:-}" ] || return 1
      if [ -n "${STUB_NODES_BODY+x}" ]; then printf '%s' "${STUB_NODES_BODY}"; return 0; fi
      if [ -n "${STUB_NODES_FILE:-}" ]; then cat "${STUB_NODES_FILE}"; return 0; fi
      command jq -n --arg ready "${STUB_READY_STATUS-True}" --arg itype "${STUB_ITYPE-cx32}" \
        --arg noready "${STUB_READY_MISSING:-}" '
        ([{type: "MemoryPressure", status: "False"}]
          + (if $noready == "" then [{type: "Ready", status: $ready}] else [] end)) as $cond
        | {items: [
            {metadata: {name: "cp-1", labels: ({"kubernetes.io/hostname": "cp-1",
                "node-role.kubernetes.io/control-plane": ""}
                + (if $itype == "" then {} else {"node.kubernetes.io/instance-type": $itype} end))},
             status: {conditions: $cond, runtimeHandlers: [{name: "runc"}, {name: "crun"}],
               volumesAttached: [{name: "kubernetes.io/csi/csi.hetzner.cloud^0001"}]}},
            {metadata: {name: "worker-1", labels: {"kubernetes.io/hostname": "worker-1",
                "node.kubernetes.io/instance-type": "cpx41"}},
             status: {conditions: $cond, runtimeHandlers: [{name: "runc"}]}}]}' ;;
    # A call per node is the bug the node-list tests catch. Answer it like
    # real kubectl, so a regression fails on the call count, not on a
    # missing stub.
    "get node "*) printf '' ;;

    "get csr -o json") printf '{}\n' ;;
    # STUB_READYZ_FAIL: /readyz answers with an error and no check lines, so
    # the script asks /version for the apiserver status.
    *"/readyz"*)
      if [ -n "${STUB_READYZ_FAIL:-}" ]; then echo 'Error from server (ServiceUnavailable)' >&2; return 1; fi
      printf 'ok\n'; return 0 ;;

    # ── Assessment fixtures (handover contract §1) ──
    # The agent's RBAC grants nothing on kubehz-agent-config (the pod gets
    # it through envFrom): any call on it is refused, as the apiserver does.
    *"configmap kubehz-agent-config"*)
      echo 'Error from server (Forbidden): configmaps "kubehz-agent-config" is forbidden' >&2
      return 1 ;;
    # Gate marker: the annotations map on the kubehz-agent-state ConfigMap.
    # STUB_STATE_FILE (the store) or STUB_MARKER holds the JSON annotations;
    # STUB_MARKER_FORBIDDEN simulates an install without that ConfigMap or
    # its RBAC (read fails -> fail-closed).
    "get configmap kubehz-agent-state"*)
      [ -z "${STUB_MARKER_FORBIDDEN:-}" ] || return 1
      if [ -n "${STUB_STATE_FILE:-}" ]; then cat "${STUB_STATE_FILE}"; return 0; fi
      printf '%s' "${STUB_MARKER:-}" ;;
    # A dry run stores nothing: it goes to its own log (STUB_DRYRUN_LOG) and
    # changes no store. The agent sends none.
    "annotate --dry-run"*)
      printf '%s\n' "$*" >> "${STUB_DRYRUN_LOG}"
      return 0 ;;
    # A marker write. The first one of a run that writes
    # kubehz.cloud/last-assessment is the send gate (STUB_GATE_DENIED), a
    # later one the write-back (STUB_WRITEBACK_DENIED). STUB_ANNOTATE_DENIED
    # refuses every annotate, as RBAC without the patch or an admission
    # webhook does.
    "annotate "*)
      printf '%s\n' "$*" >> "${STUB_ANNOTATE_LOG}"
      printf '%s\n' "$*" >> "${STUB_EVENTS_LOG}"
      local _which=""
      case " $* " in
        *" kubehz.cloud/last-assessment="*|*" kubehz.cloud/last-assessment- "*)
          if [ -z "${_stub_gate_seen:-}" ]; then _which=gate; else _which=writeback; fi
          _stub_gate_seen=1 ;;
      esac
      if [ -n "${STUB_ANNOTATE_DENIED:-}" ] \
        || { [ "${_which}" = gate ] && [ -n "${STUB_GATE_DENIED:-}" ]; } \
        || { [ "${_which}" = writeback ] && [ -n "${STUB_WRITEBACK_DENIED:-}" ]; }; then
        echo 'Error from server (Forbidden): configmaps "kubehz-agent-state" is forbidden' >&2
        return 1
      fi
      [ -z "${STUB_STATE_FILE:-}" ] || _stub_store_annotate "$@"
      return $? ;;
    # etcd static pods: default one Ready pod (datastore=etcd, reachable).
    # STUB_ETCD_READY="" = no etcd pods visible (kine/managed-CP edges).
    *"-l component=etcd"*) printf '%s' "${STUB_ETCD_READY-True }" ;;
    # kube-apiserver pod command+args (jsonpath renders the JSON array);
    # empty = pod not visible (managed CP).
    *"-l component=kube-apiserver"*) printf '%s' "${STUB_APISERVER_PODS-}" ;;
    # The CRD exists (STUB_CAPI) or the apiserver answers NotFound.
    "get crd clusters.cluster.x-k8s.io"*)
      if [ -n "${STUB_CAPI:-}" ]; then
        printf 'customresourcedefinition.apiextensions.k8s.io/clusters.cluster.x-k8s.io\n'
        return 0
      fi
      echo 'Error from server (NotFound): customresourcedefinitions.apiextensions.k8s.io "clusters.cluster.x-k8s.io" not found' >&2
      return 1 ;;
    "get daemonsets -n kube-system"*)
      if [ -n "${STUB_DAEMONSETS+x}" ]; then printf '%s' "${STUB_DAEMONSETS}"; else printf 'cilium\nkube-proxy\n'; fi ;;
    "get configmap kubeadm-config"*)
      printf '  networking:\n    dnsDomain: cluster.local\n    podSubnet: 10.244.0.0/16\n    serviceSubnet: 10.96.0.0/12\n' ;;
    # The assessment probes run under `timeout -k 2 5` (a stalled apiserver
    # degrades ONE probe, never the whole beat; the timeout stub above).
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
    # STUB_CP_NODES="" = no node with the control-plane label: the script
    # counts the legacy master label instead.
    "get nodes -l node-role.kubernetes.io/control-plane -o name"*)
      if [ -n "${STUB_CP_NODES+x}" ]; then printf '%s' "${STUB_CP_NODES}"; else printf 'node/cp-1\n'; fi ;;
    "get nodes -l node-role.kubernetes.io/master -o name"*) printf '' ;;

    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  # Capture the POSTed body (-d); every call overwrites STUB_PAYLOAD_OUT so a
  # retry's payload is what the test asserts on. Each invocation is logged to
  # STUB_CURL_LOG with its URL (call-count asserts grep per endpoint).
  # STUB_CURL_FAIL_ASSESS=1 fails any POST whose body carries an assessment
  # (the api-zod-400 simulation) — the plain retry then succeeds. The
  # /desired endpoint emulates the poll's `-sS -D - -w '\n__code=…'` wire
  # format (header dump, blank line, one-line body, __code marker) driven by
  # STUB_DESIRED_CODE/_ETAG/_BODY; the presented If-None-Match lands in
  # STUB_DESIRED_INM_OUT.
  # curl_capture (tests/lib/curl_capture.sh) reads the call the way curl
  # does: the bearer from the config (-K), the body from stdin.
  curl_capture "$@"
  printf '%s' "${CURL_HEADERS}" | grep '^Authorization: ' >> "${STUB_AUTH_LOG}" || :
  _body="${CURL_BODY}"
  _url="${CURL_URL}"
  _inm=""
  _hdr_ifs="${IFS}"
  IFS='
'
  for _h in ${CURL_HEADERS}; do
    case "${_h}" in "If-None-Match: "*) _inm="${_h#If-None-Match: }" ;; esac
  done
  IFS="${_hdr_ifs}"
  echo "curl ${_url}" >> "${STUB_CURL_LOG}"
  case "${_url}" in
    */desired)
      [ -z "${_inm}" ] || printf '%s\n' "${_inm}" > "${STUB_DESIRED_INM_OUT}"
      _code="${STUB_DESIRED_CODE:-304}"
      printf 'HTTP/2 %s \r\nEtag: %s\r\n\r\n' "${_code}" "${STUB_DESIRED_ETAG}"
      [ "${_code}" != "200" ] || printf '%s\n' "${STUB_DESIRED_BODY}"
      printf '\n__code=%s' "${_code}"
      return 0
      ;;
  esac
  [ -z "${_body}" ] || printf '%s' "${_body}" > "${STUB_PAYLOAD_OUT}"
  # Each heartbeat POST: "assessment" or "plain" in STUB_BEATS_LOG, and the
  # POST in STUB_EVENTS_LOG. STUB_CURL_FAIL=1 fails every heartbeat POST
  # (the delivery fails: no beat lands).
  case "${_url}" in
    */heartbeat)
      case "${_body}" in *'"assessment"'*) echo assessment ;; *) echo plain ;; esac >> "${STUB_BEATS_LOG}"
      echo "POST ${_url}" >> "${STUB_EVENTS_LOG}"
      [ -z "${STUB_CURL_FAIL:-}" ] || return 22 ;;
  esac
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
  cat "${_PROJECT_ROOT}/tests/lib/curl_capture.sh" "${stubs}" "${HEARTBEAT}" > "${RUNNER}"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
  export STUB_PAYLOAD_OUT="${BATS_TEST_TMPDIR}/payload.json"
  export STUB_ANNOTATE_LOG="${BATS_TEST_TMPDIR}/annotate.log"
  # Dry-run annotate calls, apart from the marker writes. The agent sends none.
  export STUB_DRYRUN_LOG="${BATS_TEST_TMPDIR}/dryrun.log"
  export STUB_CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  # Every `timeout …` call: its whole argv (`-k 2 5 kubectl get …`).
  export STUB_TIMEOUT_LOG="${BATS_TEST_TMPDIR}/timeout.log"
  # Every kubectl call that does not run under timeout.
  export STUB_UNBOUND_LOG="${BATS_TEST_TMPDIR}/unbound.log"
  # Each annotate and each heartbeat POST, in order; and per heartbeat POST
  # whether it carried an assessment.
  export STUB_EVENTS_LOG="${BATS_TEST_TMPDIR}/events.log"
  export STUB_BEATS_LOG="${BATS_TEST_TMPDIR}/beats.log"
  # The Authorization headers that curl got (from its config).
  export STUB_AUTH_LOG="${BATS_TEST_TMPDIR}/auth.log"
  # Desired-state poll fixtures: default 304 (unchanged → silent) so the
  # heartbeat-focused tests see no extra output/annotations; the poll tests
  # flip STUB_DESIRED_CODE to 200/401 and assert the report-only behavior.
  export STUB_DESIRED_INM_OUT="${BATS_TEST_TMPDIR}/desired-inm.txt"
  export STUB_DESIRED_ETAG='"7-100"'
  export STUB_DESIRED_BODY='{"revision":7,"kubernetesVersion":null,"workerPools":[],"execution":{"scaling":true,"upgrades":false,"healing":false},"healing":{"enabled":false}}'
}

# _argv_spies: log the argv of every curl and kubectl stub call and of the
# tools that a filter or redaction step could hand a secret to.
_argv_spies() {
  export ARGV_LOG="${BATS_TEST_TMPDIR}/argv.log"
  source "${_PROJECT_ROOT}/tests/lib/argv_sentinels.bash"
  argv_spy "${ARGV_SPY_TOOLS[@]}"
}

# _node_list N: generate a node list of N nodes and serve it as the answer
# to `kubectl get --raw /api/v1/nodes`. The first three nodes cover each
# field case:
#   cp-0      control-plane label key (empty value), Ready True, cx53
#   master-1  legacy master label key (empty value), Ready False, cpx41
#   worker-2  no Ready condition, no instance-type label
# Every other node is a Ready cpx41 worker named worker-<i>.
# Each node has the shape of a real one, so jq gets real work: ~17 KB per
# node here, ~23 KB of node JSON on prod, most of it 50 status.images
# entries.
_node_list() {
  export STUB_NODES_FILE="${BATS_TEST_TMPDIR}/nodes-${1}.json"
  command jq -n --argjson n "$1" '
    def node($name; $labels; $conds):
      {apiVersion: "v1", kind: "Node",
       metadata: {name: $name, uid: "6f1c2a4e-0d3b-4c5a-9e7f-8a9b0c1d2e3f",
         resourceVersion: "123456789", creationTimestamp: "2026-10-01T00:00:00Z",
         labels: ({"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux",
           "kubernetes.io/hostname": $name, "topology.kubernetes.io/region": "fsn1",
           "topology.kubernetes.io/zone": "fsn1-dc14"} + $labels),
         annotations: {"csi.volume.kubernetes.io/nodeid": "{\"csi.hetzner.cloud\":\"12345678\"}",
           "node.alpha.kubernetes.io/ttl": "0",
           "volumes.kubernetes.io/controller-managed-attach-detach": "true"}},
       spec: {podCIDR: "10.244.1.0/24", podCIDRs: ["10.244.1.0/24"], providerID: "hcloud://12345678"},
       status: {
         addresses: [{type: "InternalIP", address: "10.0.0.1"}, {type: "Hostname", address: $name}],
         allocatable: {cpu: "16", memory: "32000000Ki", pods: "110"},
         capacity: {cpu: "16", memory: "32500000Ki", pods: "110"},
         conditions: [$conds[] + {lastHeartbeatTime: "2026-10-27T00:00:00Z",
           lastTransitionTime: "2026-10-01T00:00:00Z", reason: "KubeletReady",
           message: "kubelet is posting ready status"}],
         images: [range(50) as $m | {names: [
           "registry.example.com/library/image-\($m)@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
           "registry.example.com/library/image-\($m):v1.\($m).0"], sizeBytes: 123456789}],
         nodeInfo: {architecture: "amd64", containerRuntimeVersion: "containerd://1.7.24",
           kernelVersion: "6.8.0-48-generic", kubeletVersion: "v1.31.4", operatingSystem: "linux",
           osImage: "Ubuntu 24.04.1 LTS"},
         runtimeHandlers: [{name: "runc"}],
         volumesAttached: [{name: "kubernetes.io/csi/csi.hetzner.cloud^0001", devicePath: ""}]}};
    {apiVersion: "v1", kind: "List", items: [range($n) as $i |
      if $i == 0 then
        node("cp-0"; {"node-role.kubernetes.io/control-plane": "",
          "node.kubernetes.io/instance-type": "cx53"}; [{type: "Ready", status: "True"}])
      elif $i == 1 then
        node("master-1"; {"node-role.kubernetes.io/master": "",
          "node.kubernetes.io/instance-type": "cpx41"}; [{type: "Ready", status: "False"}])
      elif $i == 2 then
        node("worker-2"; {}; [{type: "MemoryPressure", status: "False"}])
      else
        node("worker-\($i)"; {"node.kubernetes.io/instance-type": "cpx41"}; [{type: "Ready", status: "True"}])
      end]}' > "${STUB_NODES_FILE}"
}

teardown() {
  # The local server of the real-curl case (tests/lib/http_stub.bash).
  [[ -z "${STUB_PID:-}" ]] || kill "${STUB_PID}" 2>/dev/null || :
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
  # The per-node field is present in the rendered heartbeat script, sourced
  # from the GA label key, not the deprecated beta alias (pins the key so a
  # silent swap to beta.kubernetes.io/instance-type fails here).
  assert_output --partial 'instanceType: ($l["node.kubernetes.io/instance-type"] // "")'
}

# ── Labeled node → instanceType is the label value ───────

@test "heartbeat: a labeled node reports its instanceType and emits valid JSON" {
  export STUB_ITYPE="cx32"
  run bash --posix "${RUNNER}"
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
  run bash --posix "${RUNNER}"
  assert_success
  # Both stub nodes present AND both mapped — `unique` alone could pass on a
  # one-node payload (review round 1 nit).
  run command jq -r '.nodes | length' "${STUB_PAYLOAD_OUT}"
  assert_output "2"
  run command jq -r '[.nodes[].status] | unique | .[]' "${STUB_PAYLOAD_OUT}"
  assert_output "Ready"
}

@test "heartbeat: a NotReady node maps the raw False to \"NotReady\"" {
  # Round-2 mutation check: breaking the False arm passed the suite — this
  # pins it. The stub reports False for every node this run.
  export STUB_READY_STATUS="False"
  run bash --posix "${RUNNER}"
  assert_success
  run command jq -r '[.nodes[].status] | unique | .[]' "${STUB_PAYLOAD_OUT}"
  assert_output "NotReady"
}

@test "heartbeat: a missing Ready condition reports \"Unknown\", never an empty string" {
  export STUB_READY_MISSING=1
  run bash --posix "${RUNNER}"
  assert_success
  # No node has a Ready condition — both must land as Unknown.
  run command jq -r '[.nodes[].status] | unique | .[]' "${STUB_PAYLOAD_OUT}"
  assert_output "Unknown"
}

@test "heartbeat: a Ready condition of Unknown reports \"Unknown\"" {
  export STUB_READY_STATUS="Unknown"
  run bash --posix "${RUNNER}"
  assert_success
  run command jq -r '[.nodes[].status] | unique | .[]' "${STUB_PAYLOAD_OUT}"
  assert_output "Unknown"
}

# ── Unlabeled node → instanceType is "" (fail-soft) ──────

@test "heartbeat: an unlabeled node reports instanceType \"\" and still emits valid JSON" {
  export STUB_ITYPE=""
  run bash --posix "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  # The field is present but empty — never omitted, never a broken heartbeat.
  run command jq -r '.nodes[0] | has("instanceType")' "${STUB_PAYLOAD_OUT}"
  assert_output "true"
  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output ""
}

# ── A node with no labels map → worker, instanceType "" (fail-soft) ──

@test "heartbeat: a node with no labels map reports worker and instanceType \"\"" {
  # metadata.labels is absent: jq must read it as an empty map, never fail
  # (a failed jq would drop every node from the beat).
  export STUB_NODES_FILE="${BATS_TEST_TMPDIR}/nodes.json"
  printf '%s\n' '{"items":[{"metadata":{"name":"bare-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}' \
    > "${STUB_NODES_FILE}"
  run bash --posix "${RUNNER}"
  assert_success

  run command jq -rc '.nodes' "${STUB_PAYLOAD_OUT}"
  assert_output '[{"name":"bare-1","status":"Ready","roles":"worker","instanceType":""}]'
}

# ── Node IDENTITY: names are the metadata names, never the greedy-sed ghosts ─
# P0 regression guard: on modern k8s the node list also carries
# .status.runtimeHandlers[].name ("runc"/"crun") and .status.volumesAttached[]
# .name (CSI handles). A greedy sed scraped those as node names, then
# `kubectl get node runc` NotFounded and `set -e` killed the beat before the
# POST — the agent was DOA on every real cluster.

@test "heartbeat: node names are EXACTLY the two metadata names (never a runtimeHandler/CSI handle)" {
  run bash --posix "${RUNNER}"
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
  run bash --posix "${RUNNER}"
  assert_success

  run command jq -r '.nodes[] | select(.name == "cp-1") | .roles' "${STUB_PAYLOAD_OUT}"
  assert_output "control-plane"
  run command jq -r '.nodes[] | select(.name == "worker-1") | .roles' "${STUB_PAYLOAD_OUT}"
  assert_output "worker"

  # BOTH roles must be present — proof the worker is not misread as control-plane.
  run command jq -rc '[.nodes[].roles] | unique' "${STUB_PAYLOAD_OUT}"
  assert_output '["control-plane","worker"]'
}

# ── ONE node list call; the call count does not grow with nodes ──
# A kubectl call takes ~1.5 s on prod. The old loop made 3 calls per node,
# so at ~18 nodes the tick ran past activeDeadlineSeconds (120 s). The job
# was killed before the POST, and the cluster stopped heart-beating.

@test "node list: the kubectl call count is the same for 3 and 200 nodes" {
  local n calls3 calls200
  for n in 3 200; do
    _node_list "${n}"
    export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl-${n}.log"
    : > "${STUB_CURL_LOG}"
    rm -f "${STUB_PAYLOAD_OUT}"
    run bash --posix "${RUNNER}"
    assert_success
    # The beat went out, with every node.
    run grep -c '/heartbeat$' "${STUB_CURL_LOG}"
    assert_output "1"
    run command jq -r '.nodes | length' "${STUB_PAYLOAD_OUT}"
    assert_output "${n}"
    # The node list is ONE call, and no call names a single node.
    run grep -c '^get --raw /api/v1/nodes$' "${STUB_KUBECTL_LOG}"
    assert_output "1"
    run grep -c '^get node ' "${STUB_KUBECTL_LOG}"
    assert_output "0"
  done
  calls3=$(wc -l < "${BATS_TEST_TMPDIR}/kubectl-3.log")
  calls200=$(wc -l < "${BATS_TEST_TMPDIR}/kubectl-200.log")
  [ "${calls3}" -gt 0 ] || fail "no kubectl call was recorded"
  [ "${calls200}" -eq "${calls3}" ] \
    || fail "kubectl calls: ${calls3} for 3 nodes, ${calls200} for 200 nodes"
}

@test "node list: the node fields map the Ready condition, the role label keys and the instance type" {
  _node_list 4
  run bash --posix "${RUNNER}"
  assert_success
  # Exact match: the values AND the field order that the platform reads.
  run command jq -c '.nodes' "${STUB_PAYLOAD_OUT}"
  assert_output '[{"name":"cp-0","status":"Ready","roles":"control-plane","instanceType":"cx53"},{"name":"master-1","status":"NotReady","roles":"control-plane","instanceType":"cpx41"},{"name":"worker-2","status":"Unknown","roles":"worker","instanceType":""},{"name":"worker-3","status":"Ready","roles":"worker","instanceType":"cpx41"}]'
}

@test "node list: a node name that needs JSON escaping keeps the payload valid JSON" {
  export STUB_NODES_FILE="${BATS_TEST_TMPDIR}/nodes.json"
  command jq -n '{items: [{metadata: {name: "we\"ird\\node\tx"},
    status: {conditions: [{type: "Ready", status: "True"}]}}]}' > "${STUB_NODES_FILE}"
  run bash --posix "${RUNNER}"
  assert_success
  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success
  run command jq -r '.nodes[0].name' "${STUB_PAYLOAD_OUT}"
  assert_output $'we"ird\\node\tx'
}

@test "node list: above 500 nodes (the api's limit) the beat sends the first 500 and logs it" {
  # At the limit: no truncation.
  _node_list 500
  run bash --posix "${RUNNER}"
  assert_success
  refute_output --partial "heartbeat summarizes the first"
  run command jq -r '.nodes | length' "${STUB_PAYLOAD_OUT}"
  assert_output "500"

  _node_list 503
  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "kubehz: heartbeat summarizes the first 500 of 503 nodes"
  run command jq -r '.nodes | length' "${STUB_PAYLOAD_OUT}"
  assert_output "500"
  run command jq -r '.nodes[499].name' "${STUB_PAYLOAD_OUT}"
  assert_output "worker-499"
  # The assessment counts every node, not only the nodes that went out.
  run command jq -r '.assessment.cpUsage.nodes' "${STUB_PAYLOAD_OUT}"
  assert_output "503"
}

@test "node list: the limits allow a burst; the requests stay low" {
  # At a 200m CPU limit the pod was throttled in 99.8 % of its CFS periods
  # on prod, and each kubectl call (a Go binary start) took ~1.5 s. At the
  # 500-node cap jq peaks at ~70 MB (measured on prod nodes).
  run command yq -r '.spec.jobTemplate.spec.template.spec.containers[0].resources | [.limits.cpu, .requests.cpu, .limits.memory, .requests.memory] | join(" ")' "${CRONJOB}"
  assert_success
  assert_output "1000m 50m 256Mi 64Mi"
}

@test "node list: 200 nodes at 1 s per kubectl call finish well under the 120 s deadline" {
  # The stub sleeps 1 s per kubectl call (prod: ~1.5 s), so the run time in
  # seconds is about the call count. A call per node takes 200 s or more.
  _node_list 200
  export STUB_KUBECTL_DELAY=1
  local start=${SECONDS} took
  run bash --posix "${RUNNER}"
  took=$(( SECONDS - start ))
  assert_success
  run grep -c '/heartbeat$' "${STUB_CURL_LOG}"
  assert_output "1"
  [ "${took}" -lt 60 ] || fail "the 200-node beat took ${took} s at 1 s per kubectl call"
}

# ── kubernetesVersion is the SERVER version, not the client image tag ──
# `kubectl version -o json` carries BOTH clientVersion (= the alpine/k8s image
# tag) and serverVersion; a first-match grabbed the client (v1.31.4). The fix
# reads the apiserver /version endpoint → the true server version.

@test "heartbeat: kubernetesVersion is the SERVER version (v1.35.5), not the client (v1.31.4)" {
  run bash --posix "${RUNNER}"
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
  run bash --posix "${RUNNER}"
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

  # Delivered → the 24h marker moved (epoch annotation on the ConfigMap):
  # the send gate wrote it once, and no dry run was sent.
  run grep -cE '^annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+$' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  run grep -c 'kubehz.cloud/last-assessment' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  [ ! -s "${STUB_DRYRUN_LOG}" ]
}

# ── Assessment: pvSummary grouping + unit conversion ─────

@test "assessment: pvSummary groups by provisioner and converts Mi/Gi to Gi" {
  run bash --posix "${RUNNER}"
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

  run bash --posix "${RUNNER}"
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

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "retrying once without it"

  # Exactly two heartbeat POSTs: the assessment one (failed) + the plain retry.
  # (Scoped to the heartbeat endpoint — the desired-state poll is its own call.)
  run grep -c '/heartbeat$' "${STUB_CURL_LOG}"
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
  # The send gate wrote it before the send, and nothing put it back.
  run grep -F "kubehz.cloud/last-assessment=" "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -c 'kubehz.cloud/last-assessment' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
}

# ── Assessment gating: at most once per 24h ──────────────

@test "assessment: a fresh (<24h) marker suppresses the assessment entirely" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'

  run bash --posix "${RUNNER}"
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

  run bash --posix "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_success
  run grep -F "kubehz.cloud/last-assessment=" "${STUB_ANNOTATE_LOG}"
  assert_success
}

# ── Assessment gating: platform-requested (actions[] assess) ──

@test "assessment: assess-requested=true overrides a fresh marker and is cleared with the new time" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/assess-requested":"true"}'

  run bash --posix "${RUNNER}"
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

  run bash --posix "${RUNNER}"
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

  run bash --posix "${RUNNER}"
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

  run bash --posix "${RUNNER}"
  assert_success

  run command jq -r '.assessment.datastore' "${STUB_PAYLOAD_OUT}"
  assert_output "unknown"
}

@test "assessment: an apiserver pod WITH an https --etcd-servers reads as external etcd" {
  export STUB_ETCD_READY=""
  export STUB_APISERVER_PODS='["kube-apiserver","--etcd-servers=https://10.0.0.5:2379"]'

  run bash --posix "${RUNNER}"
  assert_success

  run command jq -r '.assessment.datastore' "${STUB_PAYLOAD_OUT}"
  assert_output "etcd"
}

# ── Assessment: fail-closed when the marker is unreadable ──
# An install without the kubehz-agent-state ConfigMap (the script of a new lo
# on an agent that an older lo deployed, until `lo kubehz deploy` runs again)
# or without the kubehz-agent-marker RBAC must NOT spam a full assessment
# every 5-minute tick: an unreadable marker sends nothing.

@test "assessment: an unreadable marker (no kubehz-agent-state, or no RBAC for it) sends NO assessment" {
  export STUB_MARKER_FORBIDDEN=1

  run bash --posix "${RUNNER}"
  assert_success
  # One line names what was skipped, the ConfigMap and the fix.
  assert_line "kubehz: assessment and desired-state poll skipped: the agent cannot read configmap/kubehz-agent-state in kubehz-system; run 'lo kubehz deploy'"

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  # The heartbeat itself still went out intact.
  run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
  assert_output "test.example.com"
  # Nothing was written: there is no ConfigMap to write to.
  [ ! -s "${STUB_ANNOTATE_LOG}" ]
  [ ! -s "${STUB_DRYRUN_LOG}" ]
}

# The re-arm on an "assess" in the response writes to the marker. With an
# unreadable marker the tick writes nothing, the re-arm included.
@test "assessment: an unreadable marker arms nothing, even when the response asks for an assessment" {
  export STUB_MARKER_FORBIDDEN=1
  export STUB_RESPONSE='{"status":"ok","actions":["assess"]}'

  run bash --posix "${RUNNER}"
  assert_success

  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
  assert_output "test.example.com"
  [ ! -s "${STUB_ANNOTATE_LOG}" ]
  [ ! -s "${STUB_DRYRUN_LOG}" ]
}

# ── Assessment: the send gate (a readable marker that cannot be written) ──
# RBAC without the patch, or an admission webhook, can deny every write to
# kubehz-agent-state while the read works. The 24h marker then never moves,
# so without the gate the full assessment went out on every 5-minute tick.
# The gate writes the marker before the send: on a denial the tick sends
# the plain heartbeat, no assessment, and consumes no assess request.

@test "assessment: a denied marker write sends no assessment; the heartbeat goes out and one line names the fix" {
  export STUB_ANNOTATE_DENIED=1
  # Readable and due: an assess request is waiting, and the response asks
  # again.
  export STUB_MARKER='{"kubehz.cloud/assess-requested":"true"}'
  export STUB_RESPONSE='{"status":"ok","actions":["assess"]}'
  local line="kubehz: assessment not sent: the agent cannot write configmap/kubehz-agent-state in kubehz-system; run 'lo kubehz deploy'"

  run bash --posix "${RUNNER}"
  assert_success
  # The fixed line, once.
  [ "$(printf '%s\n' "${output}" | grep -cxF -- "${line}")" = "1" ]

  # No assessment went out: one heartbeat POST, the plain beat, intact.
  run grep -c '/heartbeat$' "${STUB_CURL_LOG}"
  assert_output "1"
  run command jq -e 'has("assessment") | not' "${STUB_PAYLOAD_OUT}"
  assert_success
  run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
  assert_output "test.example.com"
  run command jq -rc '[.nodes[].name]' "${STUB_PAYLOAD_OUT}"
  assert_output '["cp-1","worker-1"]'

  # The gate tried once, on the state ConfigMap: the time and the request
  # removal in one call.
  run grep -cE '^annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+ kubehz\.cloud/assess-requested-$' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  [ ! -s "${STUB_DRYRUN_LOG}" ]
  # No probe ran, and nothing else tried to move the marker or clear the
  # request (no write-back: the gate wrote nothing).
  run grep -F 'get storageclasses' "${STUB_TIMEOUT_LOG}"
  assert_failure
  run grep -c 'kubehz.cloud/last-assessment' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  run grep -c 'kubehz.cloud/assess-requested-' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
}

# ── The send gate over several ticks ─────────────────────
# The gate writes the marker before the send, in one annotate call that
# also removes a pending assess request. The three outcomes of the send:
#   delivered: the beat with the assessment lands; the gate write stays;
#   rejected:  that beat fails and the plain retry lands; the gate write
#              stays (the heartbeat case of the rejected POST above);
#   failed:    both POSTs fail; the write-back puts back the old time and
#              the request, so the next tick sends again.
# A write-back that fails too costs the assessment of this window, and the
# agent does not loop. Each case runs the real script several times against
# the stub's annotation store (STUB_STATE_FILE), which persists between the
# runs.

# _tick: one run of the script with fresh per-run logs. The store stays.
_tick() {
  : > "${STUB_ANNOTATE_LOG}"
  : > "${STUB_BEATS_LOG}"
  : > "${STUB_EVENTS_LOG}"
  : > "${STUB_DRYRUN_LOG}"
  : > "${STUB_TIMEOUT_LOG}"
  rm -f -- "${STUB_PAYLOAD_OUT:?}"
  run bash --posix "${RUNNER}"
}

# _store: the annotation store, keys sorted.
_store() {
  command jq -S -c . "${STUB_STATE_FILE}"
}

# The send gate's line in STUB_ANNOTATE_LOG: the new time, and the request
# removal when a request was pending.
GATE_RE='^annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+'

@test "send gate: a delivered assessment: the gate writes the marker before the send, and nothing writes it after" {
  local now stamp
  now=$(date -u +%s)
  export STUB_STATE_FILE="${BATS_TEST_TMPDIR}/state.json"
  printf '{"kubehz.cloud/last-assessment":"%s"}\n' "$(( now - 90000 ))" > "${STUB_STATE_FILE}"

  # Tick 1: due. The gate write, then the one POST, which carries the
  # assessment. No dry run, no write after the send.
  _tick
  assert_success
  run cat "${STUB_BEATS_LOG}"
  assert_output "assessment"
  run cat "${STUB_EVENTS_LOG}"
  assert_line --index 0 --regexp "${GATE_RE}\$"
  assert_line --index 1 "POST https://api.example.com/api/clusters/test.example.com/heartbeat"
  [ "${#lines[@]}" -eq 2 ]
  [ ! -s "${STUB_DRYRUN_LOG}" ]
  stamp=$(command jq -r '."kubehz.cloud/last-assessment"' "${STUB_STATE_FILE}")
  [ "${stamp}" -ge "${now}" ]

  # Tick 2: the marker is fresh. The plain beat, and nothing is written.
  _tick
  assert_success
  run cat "${STUB_BEATS_LOG}"
  assert_output "plain"
  [ ! -s "${STUB_ANNOTATE_LOG}" ]
  run _store
  assert_output "{\"kubehz.cloud/last-assessment\":\"${stamp}\"}"
}

# (a) RBAC without the patch, or an admission webhook, refuses the gate
# write on every tick.
@test "send gate: a gate write denied on every tick sends no assessment, keeps the request and logs one line per tick" {
  local tick line="kubehz: assessment not sent: the agent cannot write configmap/kubehz-agent-state in kubehz-system; run 'lo kubehz deploy'"
  export STUB_STATE_FILE="${BATS_TEST_TMPDIR}/state.json"
  printf '{"kubehz.cloud/assess-requested":"true"}\n' > "${STUB_STATE_FILE}"
  export STUB_GATE_DENIED=1
  for tick in 1 2 3; do
    _tick
    assert_success
    [ "$(printf '%s\n' "${output}" | grep -cxF -- "${line}")" = "1" ] || fail "tick ${tick}: the fixed line is not there once"
    run cat "${STUB_BEATS_LOG}"
    assert_output "plain"
    # One annotate: the refused gate (the time and the request removal).
    run cat "${STUB_ANNOTATE_LOG}"
    assert_line --index 0 --regexp "${GATE_RE} kubehz\.cloud/assess-requested-\$"
    [ "${#lines[@]}" -eq 1 ]
    # The request stays, and no probe ran.
    run _store
    assert_output '{"kubehz.cloud/assess-requested":"true"}'
    run grep -F 'get storageclasses' "${STUB_TIMEOUT_LOG}"
    assert_failure
  done
}

# (b) The gate write lands, and the delivery fails: both POSTs.
@test "send gate: a failed delivery puts back the time and the request; the next tick sends the assessment once" {
  local now old before stamp
  now=$(date -u +%s)
  old=$(( now - 100 ))
  export STUB_STATE_FILE="${BATS_TEST_TMPDIR}/state.json"
  printf '{"kubehz.cloud/last-assessment":"%s","kubehz.cloud/assess-requested":"true"}\n' "${old}" > "${STUB_STATE_FILE}"
  before=$(_store)

  # Tick 1: the gate, two failed POSTs, the write-back in one call.
  export STUB_CURL_FAIL=1
  _tick
  assert_failure
  assert_output --partial "kubehz: heartbeat POST failed (curl rc=22); retrying next tick"
  run cat "${STUB_BEATS_LOG}"
  assert_line --index 0 "assessment"
  assert_line --index 1 "plain"
  [ "${#lines[@]}" -eq 2 ]
  run cat "${STUB_ANNOTATE_LOG}"
  assert_line --index 0 --regexp "${GATE_RE} kubehz\.cloud/assess-requested-\$"
  assert_line --index 1 "annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/last-assessment=${old} kubehz.cloud/assess-requested=true"
  [ "${#lines[@]}" -eq 2 ]
  run _store
  assert_output "${before}"

  # Tick 2: the delivery works. The assessment goes out once, the gate
  # write stays.
  unset STUB_CURL_FAIL
  _tick
  assert_success
  run cat "${STUB_BEATS_LOG}"
  assert_output "assessment"
  run cat "${STUB_ANNOTATE_LOG}"
  assert_line --index 0 --regexp "${GATE_RE} kubehz\.cloud/assess-requested-\$"
  [ "${#lines[@]}" -eq 1 ]
  stamp=$(command jq -r '."kubehz.cloud/last-assessment"' "${STUB_STATE_FILE}")
  [ "${stamp}" -ge "${now}" ]
  run _store
  assert_output "{\"kubehz.cloud/last-assessment\":\"${stamp}\"}"

  # Tick 3: the marker is fresh. No assessment.
  _tick
  assert_success
  run cat "${STUB_BEATS_LOG}"
  assert_output "plain"
  [ ! -s "${STUB_ANNOTATE_LOG}" ]
}

# (c) The gate write lands, the delivery fails, and the write-back fails
# too. (A gate write that fails sends nothing, so it has no write-back.)
@test "send gate: a write-back that fails too loses this assessment and never loops: the next ticks send none" {
  local tick stamp line="kubehz: could not put back the assessment marker on configmap/kubehz-agent-state in kubehz-system; the next assessment comes after the 24h window or on a platform request"
  export STUB_STATE_FILE="${BATS_TEST_TMPDIR}/state.json"
  printf '{"kubehz.cloud/assess-requested":"true"}\n' > "${STUB_STATE_FILE}"

  # Tick 1: the gate, two failed POSTs, the refused write-back, one line.
  export STUB_CURL_FAIL=1 STUB_WRITEBACK_DENIED=1
  _tick
  assert_failure
  [ "$(printf '%s\n' "${output}" | grep -cxF -- "${line}")" = "1" ]
  run cat "${STUB_BEATS_LOG}"
  assert_line --index 0 "assessment"
  assert_line --index 1 "plain"
  [ "${#lines[@]}" -eq 2 ]
  run cat "${STUB_ANNOTATE_LOG}"
  assert_line --index 0 --regexp "${GATE_RE} kubehz\.cloud/assess-requested-\$"
  assert_line --index 1 "annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/last-assessment- kubehz.cloud/assess-requested=true"
  [ "${#lines[@]}" -eq 2 ]
  # The gate write stays: the new time, no request.
  stamp=$(command jq -r '."kubehz.cloud/last-assessment"' "${STUB_STATE_FILE}")
  run _store
  assert_output "{\"kubehz.cloud/last-assessment\":\"${stamp}\"}"

  # Ticks 2 and 3: everything works, and the marker is fresh: the plain
  # beat, no write, no line.
  unset STUB_CURL_FAIL STUB_WRITEBACK_DENIED
  for tick in 2 3; do
    _tick
    assert_success
    refute_output --partial "${line}"
    run cat "${STUB_BEATS_LOG}"
    assert_output "plain"
    [ ! -s "${STUB_ANNOTATE_LOG}" ] || fail "tick ${tick}: an annotate ran"
    run _store
    assert_output "{\"kubehz.cloud/last-assessment\":\"${stamp}\"}"
  done
}

# (d) The request goes only with the new time: one annotate call (one
# PATCH) writes the time and removes the request, and no call removes the
# request alone.
@test "send gate: a pending request is used up only in the call that writes the new time" {
  local now stamp
  now=$(date -u +%s)
  export STUB_STATE_FILE="${BATS_TEST_TMPDIR}/state.json"
  printf '{"kubehz.cloud/last-assessment":"%s","kubehz.cloud/assess-requested":"true"}\n' "$(( now - 100 ))" > "${STUB_STATE_FILE}"
  local before
  before=$(_store)

  # Tick 1: the gate write is refused: the request stays.
  export STUB_GATE_DENIED=1
  _tick
  assert_success
  run grep -c 'kubehz.cloud/assess-requested-' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  run grep -E "${GATE_RE} kubehz\.cloud/assess-requested-\$" "${STUB_ANNOTATE_LOG}"
  assert_success
  run _store
  assert_output "${before}"

  # Tick 2: the gate write lands: one call, the time and the removal.
  unset STUB_GATE_DENIED
  _tick
  assert_success
  run cat "${STUB_BEATS_LOG}"
  assert_output "assessment"
  run cat "${STUB_ANNOTATE_LOG}"
  assert_line --index 0 --regexp "${GATE_RE} kubehz\.cloud/assess-requested-\$"
  [ "${#lines[@]}" -eq 1 ]
  stamp=$(command jq -r '."kubehz.cloud/last-assessment"' "${STUB_STATE_FILE}")
  [ "${stamp}" -ge "${now}" ]
  run _store
  assert_output "{\"kubehz.cloud/last-assessment\":\"${stamp}\"}"
}

# ── The markers live on kubehz-agent-state ───────────────
# The agent's RBAC grants get and patch on that one ConfigMap and nothing on
# kubehz-agent-config. The kubectl stub refuses every call on
# kubehz-agent-config, as the apiserver does.

# _state_only: every annotate call of the last run names the state ConfigMap,
# and no kubectl call names kubehz-agent-config. $1 is the number of annotate
# calls that the run must make.
_state_only() {
  local want=$1 all state
  all=$(grep -c '' "${STUB_ANNOTATE_LOG}") || all=0
  state=$(grep -cE '^annotate (--overwrite )?configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/' "${STUB_ANNOTATE_LOG}") || state=0
  [ "${all}" = "${want}" ] || { echo "annotate calls: ${all}, want ${want}" >&2; return 1; }
  [ "${state}" = "${want}" ] || { echo "annotate calls on kubehz-agent-state: ${state}, want ${want}" >&2; return 1; }
  if grep -F 'kubehz-agent-config' "${STUB_KUBECTL_LOG}" >&2; then
    echo "a kubectl call names kubehz-agent-config" >&2
    return 1
  fi
}

@test "state ConfigMap: each of the nine marker writers and the marker read name kubehz-agent-state only" {
  local now
  now=$(date -u +%s)
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"

  # Tick 1: no last-assessment, a served assess request, an expired nonce, a
  # stored refusal, a changed desired state and an "assess" in the response:
  # the nonce clear, the send gate (the 24h marker and the request clear in
  # one call), the re-arm, the desired-state validator and the refusal clear.
  export STUB_MARKER='{"kubehz.cloud/assess-requested":"true","kubehz.cloud/claim-nonce":"khzn_eXp1rEd_n0ncE_43charsBase64urlValue00000","kubehz.cloud/claim-nonce-placed":"'$(( now - 1000 ))'","kubehz.cloud/desired-error":"401"}'
  export STUB_DESIRED_CODE=200
  export STUB_DESIRED_ETAG='"9-120"'
  export STUB_RESPONSE='{"status":"ok","actions":["assess"]}'
  run bash --posix "${RUNNER}"
  assert_success
  run _state_only 5
  assert_success
  run grep -F 'kubehz.cloud/claim-nonce- kubehz.cloud/claim-nonce-placed-' "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -E 'kubehz\.cloud/last-assessment=[0-9]+ kubehz\.cloud/assess-requested-$' "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F "kubehz.cloud/assess-requested=true" "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F 'kubehz.cloud/desired-etag="9-120" kubehz.cloud/desired-revision=7' "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F "kubehz.cloud/desired-error-" "${STUB_ANNOTATE_LOG}"
  assert_success
  # The marker read: one call, on the state ConfigMap.
  run grep -c '^get configmap kubehz-agent-state -n kubehz-system -o jsonpath={.metadata.annotations}$' "${STUB_KUBECTL_LOG}"
  assert_output "1"

  # Tick 2: a live nonce that the platform verifies, and a stored refusal
  # with an unchanged desired state: the nonce clear and the refusal clear.
  : > "${STUB_ANNOTATE_LOG}"
  : > "${STUB_KUBECTL_LOG}"
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"khzn_vEr1f1Ed_n0ncE_43charsBase64urlValue000","kubehz.cloud/claim-nonce-placed":"'$(( now - 30 ))'","kubehz.cloud/desired-error":"401"}'
  export STUB_DESIRED_CODE=304
  export STUB_RESPONSE='{"status":"ok","claim":{"verified":true}}'
  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "claim verified"
  run _state_only 2
  assert_success
  run grep -F 'kubehz.cloud/claim-nonce- kubehz.cloud/claim-nonce-placed-' "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F "kubehz.cloud/desired-error-" "${STUB_ANNOTATE_LOG}"
  assert_success

  # Tick 3: a new refusal of the desired-state poll is stored.
  : > "${STUB_ANNOTATE_LOG}"
  : > "${STUB_KUBECTL_LOG}"
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'
  export STUB_DESIRED_CODE=403
  export STUB_RESPONSE='{"status":"ok"}'
  run bash --posix "${RUNNER}"
  assert_success
  run _state_only 1
  assert_success
  run grep -F "kubehz.cloud/desired-error=403" "${STUB_ANNOTATE_LOG}"
  assert_success

  # Tick 4: a due assessment whose delivery fails: the send gate and the
  # write-back, which removes the time again (the marker read saw none).
  : > "${STUB_ANNOTATE_LOG}"
  : > "${STUB_KUBECTL_LOG}"
  export STUB_MARKER='{}'
  export STUB_CURL_FAIL=1
  run bash --posix "${RUNNER}"
  assert_failure
  run _state_only 2
  assert_success
  run grep -cxE 'annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  run grep -cxF 'annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/last-assessment-' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
}

# ── Upgrade: an agent that an older lo deployed ──────────
# That agent kept its markers on kubehz-agent-config. After `lo kubehz deploy`
# the agent reads only kubehz-agent-state, which starts empty. The cost is one
# extra assessment, then the 24h gate holds again. A claim nonce placed on
# the old ConfigMap is not echoed.

@test "upgrade: the first tick on a new, empty kubehz-agent-state sends one assessment, the next tick sends none" {
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  # The new ConfigMap has no annotations: the read prints nothing.
  export STUB_MARKER=''

  run bash --posix "${RUNNER}"
  assert_success
  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_success
  run command jq -e '.claim' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run grep -c '' "${STUB_ANNOTATE_LOG}"
  assert_output "1"
  run grep -E '^annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+$' "${STUB_ANNOTATE_LOG}"
  assert_success
  local stamp
  stamp=$(sed -n 's/.*kubehz\.cloud\/last-assessment=//p' "${STUB_ANNOTATE_LOG}")
  run grep -F 'kubehz-agent-config' "${STUB_KUBECTL_LOG}"
  assert_failure

  # The next tick reads the marker that the first tick wrote.
  : > "${STUB_ANNOTATE_LOG}"
  rm -f -- "${STUB_PAYLOAD_OUT:?}"
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'"${stamp}"'"}'
  run bash --posix "${RUNNER}"
  assert_success
  run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
  assert_output "test.example.com"
  run command jq -e '.assessment' "${STUB_PAYLOAD_OUT}"
  assert_failure
  [ ! -s "${STUB_ANNOTATE_LOG}" ]
}

# ── in-cluster kubectl takes no client-config flag ──
# In the pod, a kubectl client-config flag (--request-timeout, --server, …)
# makes kubectl skip the in-cluster config and dial localhost:8080. The
# kubectl stub refuses such a call the same way. The marker read carried
# --request-timeout=5s: it failed on every tick, so the 24h gate, the claim
# nonce and the desired-state poll were off, and every assessment probe read
# empty. `timeout -k 2 5` bounds the calls now.

@test "in-cluster kubectl: the marker read reaches the apiserver: the assessment probes and the desired-state poll run" {
  run bash --posix "${RUNNER}"
  assert_success
  # The marker was readable, so the desired-state poll ran.
  refute_output --partial "desired-state poll skipped"
  run grep -F "curl https://api.example.com/api/clusters/test.example.com/desired" "${STUB_CURL_LOG}"
  assert_success
  # Each probe got its answer from the apiserver, not an empty fallback.
  run command jq -r '[.assessment.datastore, .assessment.cni, .assessment.podCidr, (.assessment.storageClasses | length), .assessment.pvSummary.count, .assessment.loadBalancers, .assessment.webhooks.validating, .assessment.cpUsage.cpNodes] | map(tostring) | join(" ")' "${STUB_PAYLOAD_OUT}"
  assert_output "etcd cilium 10.244.0.0/16 2 3 2 3 1"
}

@test "in-cluster kubectl: the marker read, the send gate and every assessment probe run under timeout -k 2 5; the node list under timeout -k 2 30" {
  run bash --posix "${RUNNER}"
  assert_success
  run cat "${STUB_TIMEOUT_LOG}"
  assert_line "-k 2 5 kubectl get configmap kubehz-agent-state -n kubehz-system -o jsonpath={.metadata.annotations}"
  assert_line --regexp '^-k 2 5 kubectl annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+$'
  assert_line --partial "-k 2 5 kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath="
  assert_line --partial "-k 2 5 kubectl get pods -n kube-system -l component=etcd -o jsonpath="
  assert_line "-k 2 5 kubectl get crd clusters.cluster.x-k8s.io -o name"
  assert_line --partial "-k 2 5 kubectl get daemonsets -n kube-system -o jsonpath="
  assert_line "-k 2 5 kubectl get configmap kubeadm-config -n kube-system -o jsonpath={.data.ClusterConfiguration}"
  assert_line "-k 2 5 kubectl get storageclasses -o json"
  assert_line "-k 2 5 kubectl get pv -o json"
  assert_line "-k 2 5 kubectl get services -A -o json"
  assert_line "-k 2 5 kubectl get validatingwebhookconfigurations -o json"
  assert_line "-k 2 5 kubectl get mutatingwebhookconfigurations -o json"
  assert_line "-k 2 5 kubectl get nodes -l node-role.kubernetes.io/control-plane -o name"
  # The node list has its own bound.
  assert_line "-k 2 30 kubectl get --raw /api/v1/nodes"
  # Each call in the form that the BusyBox applet and GNU coreutils both
  # take: TERM after the seconds, KILL 2 s later. No call is unbounded.
  run grep -vcE '^(-k 2 5 kubectl |-k 2 30 kubectl get --raw /api/v1/nodes$)' "${STUB_TIMEOUT_LOG}"
  assert_output "0"
  [ ! -s "${STUB_UNBOUND_LOG}" ]
}

# Every kubectl call of the script runs under `timeout -k 2 5` (the node
# list under `timeout -k 2 30`): the identity Secret calls, the health
# reads, the marker writes and the nonce clears too. A call without the
# bound can hold the tick until the Job's deadline kills it. Four ticks
# reach every call: a first run (the Secret is created, /readyz fails, no
# control-plane label, an expired nonce, a changed desired state, a stored
# refusal and an "assess" in the response), a verified claim with an
# unchanged desired state, a new refusal, and a failed delivery (the
# write-back).
@test "in-cluster kubectl: every kubectl call of the script runs under timeout -k 2" {
  local now
  now=$(date -u +%s)
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"

  STUB_SECRET_ABSENT=1 STUB_READYZ_FAIL=1 STUB_CP_NODES='' STUB_DESIRED_CODE=200 STUB_DESIRED_ETAG='"9-120"' \
    STUB_MARKER='{"kubehz.cloud/claim-nonce":"khzn_eXp1rEd_n0ncE_43charsBase64urlValue00000","kubehz.cloud/claim-nonce-placed":"'$(( now - 1000 ))'","kubehz.cloud/desired-error":"401"}' \
    STUB_RESPONSE='{"status":"ok","actions":["assess"]}' run bash --posix "${RUNNER}"
  assert_success
  STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"khzn_vEr1f1Ed_n0ncE_43charsBase64urlValue000","kubehz.cloud/claim-nonce-placed":"'$(( now - 30 ))'","kubehz.cloud/desired-error":"401"}' \
    STUB_RESPONSE='{"status":"ok","claim":{"verified":true}}' run bash --posix "${RUNNER}"
  assert_success
  STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}' STUB_DESIRED_CODE=401 run bash --posix "${RUNNER}"
  assert_success
  STUB_MARKER='{}' STUB_CURL_FAIL=1 run bash --posix "${RUNNER}"
  assert_failure

  # No kubectl call ran outside timeout, and each one ran under the -k form.
  [ ! -s "${STUB_UNBOUND_LOG}" ]
  run grep -vcE '^(-k 2 5 kubectl |-k 2 30 kubectl get --raw /api/v1/nodes$)' "${STUB_TIMEOUT_LOG}"
  assert_output "0"
  [ "$(grep -c '' "${STUB_KUBECTL_LOG}")" = "$(grep -c ' kubectl ' "${STUB_TIMEOUT_LOG}")" ]
  # The calls that ran without a bound before, each seen under it.
  run cat "${STUB_TIMEOUT_LOG}"
  assert_line "-k 2 5 kubectl get secret kubehz-agent -n kubehz-system"
  assert_line "-k 2 5 kubectl create secret generic kubehz-agent -n kubehz-system --from-env-file=/dev/stdin"
  assert_line "-k 2 5 kubectl get secret kubehz-agent -n kubehz-system -o jsonpath={.data.agent-token}"
  assert_line "-k 2 5 kubectl get secret kubehz-agent -n kubehz-system -o jsonpath={.data.claim-code}"
  assert_line "-k 2 5 kubectl get --raw /version"
  assert_line "-k 2 5 kubectl get csr -o json"
  assert_line "-k 2 5 kubectl get --raw /readyz?verbose"
  assert_line --partial "-k 2 5 kubectl get pods -n kube-system -l component=kube-scheduler -o jsonpath="
  assert_line --partial "-k 2 5 kubectl get pods -n kube-system -l component=kube-controller-manager -o jsonpath="
  assert_line "-k 2 5 kubectl get nodes -l node-role.kubernetes.io/master -o name"
  assert_line "-k 2 5 kubectl annotate configmap kubehz-agent-state -n kubehz-system kubehz.cloud/claim-nonce- kubehz.cloud/claim-nonce-placed-"
  assert_line "-k 2 5 kubectl annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/assess-requested=true"
  assert_line "-k 2 5 kubectl annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/desired-etag=\"9-120\" kubehz.cloud/desired-revision=7"
  assert_line "-k 2 5 kubectl annotate configmap kubehz-agent-state -n kubehz-system kubehz.cloud/desired-error-"
  assert_line "-k 2 5 kubectl annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/desired-error=401"
  assert_line "-k 2 5 kubectl annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/last-assessment-"
  # The nonce clear runs on two paths: the expiry (tick 1) and the verified
  # claim (tick 2).
  [ "$(grep -cxF -- "-k 2 5 kubectl annotate configmap kubehz-agent-state -n kubehz-system kubehz.cloud/claim-nonce- kubehz.cloud/claim-nonce-placed-" "${STUB_TIMEOUT_LOG}")" = 2 ]
  # /version runs twice in tick 1: the server version and the apiserver
  # status after the failed /readyz.
  [ "$(grep -cxF -- "-k 2 5 kubectl get --raw /version" "${STUB_TIMEOUT_LOG}")" -ge 5 ]
}

# A probe for a field the contract has no "unknown" value for (a count, a
# list, capiManaged) that times out is unknown, never 0 or []: the tick sends
# no assessment, the heartbeat still goes out, and the 24h marker does not
# move: the write-back puts back what the send gate wrote, so the next tick
# probes again.
@test "assessment: a timed-out probe defers the assessment, never a 0" {
  local match
  export STUB_STATE_FILE="${BATS_TEST_TMPDIR}/state.json"
  for match in "get services -A" "get validatingwebhookconfigurations" "get mutatingwebhookconfigurations" \
      "node-role.kubernetes.io/control-plane" "get storageclasses" "get pv " "get crd clusters.cluster.x-k8s.io"; do
    export STUB_TIMEOUT_MATCH="${match}"
    rm -f "${STUB_PAYLOAD_OUT}" "${STUB_ANNOTATE_LOG}"
    printf '{}\n' > "${STUB_STATE_FILE}"
    run bash --posix "${RUNNER}"
    assert_success
    assert_output --partial "kubehz: assessment deferred: no answer for "
    run command jq -e 'has("assessment") | not' "${STUB_PAYLOAD_OUT}"
    assert_success
    run command jq -r '.clusterId' "${STUB_PAYLOAD_OUT}"
    assert_output "test.example.com"
    # The send gate, then the write-back: the marker is as before the tick.
    run cat "${STUB_ANNOTATE_LOG}"
    assert_line --index 0 --regexp '^annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz\.cloud/last-assessment=[0-9]+$'
    assert_line --index 1 'annotate --overwrite configmap kubehz-agent-state -n kubehz-system kubehz.cloud/last-assessment-'
    [ "${#lines[@]}" -eq 2 ]
    run command jq -c . "${STUB_STATE_FILE}"
    assert_output '{}'
  done
}

@test "assessment: a failed node list leaves cpUsage.nodes unknown, never 0" {
  export STUB_NODES_FAIL=1
  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "kubehz: assessment deferred: no answer for cpUsage.nodes"
  run command jq -e 'has("assessment") | not' "${STUB_PAYLOAD_OUT}"
  assert_success
}

@test "node list: an empty, cut, non-JSON or item-less node answer is a failed call" {
  # kubectl can exit 0 and still answer nothing usable, and a timeout can
  # cut the answer. jq -e then fails, so the beat sends no nodes and defers
  # the assessment, as for a failed call. The assessment never reports 0
  # nodes as a fact.
  local body
  for body in '' '{"items":[{"metadata":' '{"items":[{"metadata":{"name":"a"}},' 'not json' '{}'; do
    export STUB_NODES_BODY="${body}"
    rm -f "${STUB_PAYLOAD_OUT}"
    run bash --posix "${RUNNER}"
    assert_success
    assert_output --partial "kubehz: assessment deferred: no answer for cpUsage.nodes"
    run command jq -c '.nodes' "${STUB_PAYLOAD_OUT}"
    assert_output '[]'
    run command jq -e 'has("assessment") | not' "${STUB_PAYLOAD_OUT}"
    assert_success
  done
}

@test "assessment: a missing CAPI CRD (NotFound) reads capiManaged false; the CRD reads true" {
  run bash --posix "${RUNNER}"
  assert_success
  run command jq -r '.assessment.capiManaged' "${STUB_PAYLOAD_OUT}"
  assert_output "false"
  export STUB_CAPI=1
  run bash --posix "${RUNNER}"
  assert_success
  run command jq -r '.assessment.capiManaged' "${STUB_PAYLOAD_OUT}"
  assert_output "true"
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

# ── Mode-3 claim nonce: echo, expiry, success-cue clear ──
# `lo kubehz claim --nonce` places kubehz.cloud/claim-nonce (+ the -placed
# epoch stamp) on the marker ConfigMap; the beat echoes it as claim.nonce and
# clears it on the response's additive claim.verified cue — or after the api's
# 15-minute challenge TTL, so a dead nonce never echoes forever.

@test "claim: a live placed nonce is echoed as claim.nonce in the beat" {
  local now nonce
  now=$(date -u +%s)
  nonce="khzn_l1vE_p1AcEd_n0ncE_43charsBase64urlValue0"
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"'"${nonce}"'","kubehz.cloud/claim-nonce-placed":"'$(( now - 30 ))'"}'

  run bash --posix "${RUNNER}"
  assert_success
  # The nonce value never surfaces in stdout/stderr — it is a claim ticket.
  refute_output --partial "${nonce}"

  run command jq -r '.claim.nonce' "${STUB_PAYLOAD_OUT}"
  assert_output "${nonce}"
  # Not cleared: the stub response carried no claim.verified cue.
  run grep -F "kubehz.cloud/claim-nonce-" "${STUB_ANNOTATE_LOG}"
  assert_failure
}

@test "claim: the nonce and the agent token never reach a command line" {
  # Every process on the node can read argv (ps, /proc/<pid>/cmdline). The
  # stubs log the argv of each curl and kubectl call (curl_capture.sh).
  local now nonce
  now=$(date -u +%s)
  nonce="khzn_SENTINEL_n0ncE_43charsBase64urlValue00"
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"'"${nonce}"'","kubehz.cloud/claim-nonce-placed":"'$(( now - 30 ))'"}'
  _argv_spies

  run bash --posix "${RUNNER}"
  assert_success
  assert_argv_clean "${nonce}" khz_agt_test
  # Both still arrive: the nonce in the body, the token as the bearer of
  # the beat and of the desired-state poll.
  run command jq -r '.claim.nonce' "${STUB_PAYLOAD_OUT}"
  assert_output "${nonce}"
  [ "$(ARGV_LOG="" grep -c "^Authorization: Bearer khz_agt_test$" "${STUB_AUTH_LOG}")" = 2 ]
}

@test "heartbeat: the plain retry after a rejected assessment keeps the token off argv too" {
  export STUB_CURL_FAIL_ASSESS=1
  _argv_spies

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "retrying once without it"
  assert_argv_clean khz_agt_test
  # The beat with the assessment, the plain retry and the desired-state
  # poll: each carries the bearer from its config.
  [ "$(ARGV_LOG="" grep -c '/heartbeat$' "${STUB_CURL_LOG}")" = 2 ]
  [ "$(ARGV_LOG="" grep -c "^Authorization: Bearer khz_agt_test$" "${STUB_AUTH_LOG}")" = 3 ]
}

@test "heartbeat: a token that a curl config line cannot carry skips the beat" {
  # A quote, a backslash or a line break would break the config line, and
  # the guard allows no space either. The agent never mints such a token.
  local tok
  for tok in 'khz_agt_a"b' 'khz_agt_a\b' 'khz_agt_a b' $'khz_agt_a\nurl = https://x'; do
    : > "${STUB_CURL_LOG}"
    STUB_AGENT_TOKEN="${tok}" run bash --posix "${RUNNER}"
    assert_success
    assert_output --partial "kubehz: skipping heartbeat: agent-token holds a character that a curl config cannot carry"
    run grep -c "/heartbeat" "${STUB_CURL_LOG}"
    assert_output 0
  done
}

@test "claim: no nonce annotation means no claim key on the wire" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'

  run bash --posix "${RUNNER}"
  assert_success
  run command jq -e '.claim' "${STUB_PAYLOAD_OUT}"
  assert_failure
}

@test "claim: an expired nonce (past the 15-min TTL) is cleared and NOT echoed" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"khzn_eXp1rEd_n0ncE_43charsBase64urlValue00000","kubehz.cloud/claim-nonce-placed":"'$(( now - 1000 ))'"}'

  run bash --posix "${RUNNER}"
  assert_success

  run command jq -e '.claim' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run grep -F "kubehz.cloud/claim-nonce-" "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F "kubehz.cloud/claim-nonce-placed-" "${STUB_ANNOTATE_LOG}"
  assert_success
}

@test "claim: a stampless/malformed nonce is cleared as unsourced (never echoed)" {
  # The CLI always writes nonce + stamp in ONE annotate call — a value without
  # a valid stamp did not come from it and must not echo forever.
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"khzn_st4mplEss_n0ncE_43charsBase64urlValue00"}'

  run bash --posix "${RUNNER}"
  assert_success
  run command jq -e '.claim' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run grep -F "kubehz.cloud/claim-nonce-" "${STUB_ANNOTATE_LOG}"
  assert_success
}

@test "claim: the response's claim.verified cue clears the nonce annotations" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"khzn_vEr1f1Ed_n0ncE_43charsBase64urlValue000","kubehz.cloud/claim-nonce-placed":"'$(( now - 30 ))'"}'
  export STUB_RESPONSE='{"status":"ok","claim":{"verified":true}}'

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "claim verified"

  run command jq -r '.claim.nonce' "${STUB_PAYLOAD_OUT}"
  assert_output "khzn_vEr1f1Ed_n0ncE_43charsBase64urlValue000"
  run grep -F "kubehz.cloud/claim-nonce-" "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F "kubehz.cloud/claim-nonce-placed-" "${STUB_ANNOTATE_LOG}"
  assert_success
}

@test "claim: a cue-less response (wrong/expired nonce server-side) clears nothing" {
  # Anti-oracle: the response is byte-identical to a nonce-less beat — the
  # agent keeps echoing until the local TTL window closes.
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/claim-nonce":"khzn_uNvEr1f1Ed_n0ncE_43charsBase64urlValue0","kubehz.cloud/claim-nonce-placed":"'$(( now - 30 ))'"}'
  export STUB_RESPONSE='{"status":"ok"}'

  run bash --posix "${RUNNER}"
  assert_success
  run grep -F "kubehz.cloud/claim-nonce-" "${STUB_ANNOTATE_LOG}"
  assert_failure
}

# ── Desired-state poll: REPORT-ONLY (P0 slice) ───────────
# After the beat the agent polls GET /clusters/<id>/desired with the stored
# ETag: 304 stays silent, 200 logs revision + on-flags + version skew and
# persists the new validator, 401/403/404 log once per state change. NO
# acting — that is the P2 Go agent.

@test "desired: 304 stays silent and persists nothing" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/desired-etag":"\"7-100\""}'
  export STUB_DESIRED_CODE=304

  run bash --posix "${RUNNER}"
  assert_success
  refute_output --partial "desired state changed"
  run grep -F "kubehz.cloud/desired-etag=" "${STUB_ANNOTATE_LOG}"
  assert_failure

  # The stored validator was presented verbatim as If-None-Match.
  run cat "${STUB_DESIRED_INM_OUT}"
  assert_output '"7-100"'
}

@test "desired: 200 logs revision + on-flags + version skew and stores the new ETag" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/desired-etag":"\"6-100\""}'
  export STUB_DESIRED_CODE=200
  export STUB_DESIRED_ETAG='"8-110"'
  export STUB_DESIRED_BODY='{"revision":8,"kubernetesVersion":"v1.36.0","workerPools":[],"execution":{"scaling":true,"upgrades":true,"healing":false},"healing":{"enabled":false}}'

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "desired state changed — revision 8"
  assert_output --partial "flags on: scaling,upgrades"
  # Local (stub server) is v1.35.5 → the skew is reported.
  assert_output --partial "desired k8s v1.36.0, local v1.35.5"
  # Report-only: the slice says so on every changed report.
  assert_output --partial "report-only"

  run grep -F 'kubehz.cloud/desired-etag="8-110"' "${STUB_ANNOTATE_LOG}"
  assert_success
  run grep -F "kubehz.cloud/desired-revision=8" "${STUB_ANNOTATE_LOG}"
  assert_success
}

@test "desired: no flags on and no skew reports 'none' without a version tail" {
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'
  export STUB_DESIRED_CODE=200
  export STUB_DESIRED_ETAG='"3-000"'
  export STUB_DESIRED_BODY='{"revision":3,"kubernetesVersion":null,"workerPools":[],"execution":{"scaling":false,"upgrades":false,"healing":false},"healing":{"enabled":false}}'

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "revision 3, execution flags on: none"
  refute_output --partial "desired k8s"
}

@test "desired: a 401 logs once and stays quiet while the state is unchanged" {
  local now
  now=$(date -u +%s)
  # First refusal: no stored error state → log + persist it.
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'
  export STUB_DESIRED_CODE=401

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "desired-state poll refused (HTTP 401)"
  run grep -F "kubehz.cloud/desired-error=401" "${STUB_ANNOTATE_LOG}"
  assert_success

  # Same refusal with the state already stored → silent (once per change).
  : > "${STUB_ANNOTATE_LOG}"
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'","kubehz.cloud/desired-error":"401"}'
  run bash --posix "${RUNNER}"
  assert_success
  refute_output --partial "desired-state poll refused"
}

@test "desired: the poll never fails the tick (beat already landed)" {
  # An unparseable 200 (garbage body) degrades to a stderr note; exit stays 0.
  local now
  now=$(date -u +%s)
  export STUB_MARKER='{"kubehz.cloud/last-assessment":"'$(( now - 100 ))'"}'
  export STUB_DESIRED_CODE=200
  export STUB_DESIRED_BODY='not json'

  run bash --posix "${RUNNER}"
  assert_success
  assert_output --partial "unparseable"
}

# ── curl reads no config file (the real curl against a local server) ──
# Every curl call of the script passes -q first: the self-register (its
# body can carry the bind secret), the beat with the assessment, the plain
# retry and the desired-state poll (both with the bearer). The script runs
# with the real curl against tests/lib/http_stub.py on 127.0.0.1: once
# without a config file of the user (the reference), then with a hostile
# one (a trace-ascii and an output line) in each place curl looks: CURL_HOME,
# XDG_CONFIG_HOME, HOME. Each run writes no trace and no output file and
# prints the same stdout and stderr as the reference, with the same status.
# The control in each place: curl without -q reads the file there. curl
# older than 7.73 does not look in XDG_CONFIG_HOME: that control is left
# out, with a note. The pod runs the script with its own HOME and no
# writable file system (cronjob.yaml), so -q there guards against a config
# file in the image. The register carries a synthetic bind secret: the stub
# logs bind=yes when that value is in the body (it never logs the value).
@test "heartbeat: curl reads no config file of the user (register, beat, plain retry, desired poll)" {
  command -v python3 >/dev/null || skip "no python3"
  [[ -n "$(type -P curl)" ]] || skip "no curl"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"
  local real="${BATS_TEST_TMPDIR}/run-real-curl.sh" ref="${BATS_TEST_TMPDIR}/ref"
  local var o e rc ref_rc=0 hex controls=0
  # The runner without the curl stub: the script calls the real curl.
  { cat "${_PROJECT_ROOT}/tests/lib/curl_capture.sh" "${BATS_TEST_TMPDIR}/stubs.sh"
    echo 'unset -f curl'
    cat "${HEARTBEAT}"; } > "${real}"
  hex=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  export STUB_AGENT_TOKEN="khz_agt_${hex}" STUB_CLAIM_CODE="khzc_${hex}" STUB_BEARER="khz_agt_${hex}"
  export BIND_SECRET="${hex}${hex}${hex}${hex}"
  export STUB_BIND_SECRET="${BIND_SECRET}"
  # The beat with the assessment gets a 400, the plain retry a 200.
  export STUB_ROUTES="${BATS_TEST_TMPDIR}/routes.json"
  cat > "${STUB_ROUTES}" <<'JSON'
{
  "POST /api/clusters/agent-register": {"code": 201, "bind_secret": true, "body": {"ok": true}},
  "POST /api/clusters/test.example.com/heartbeat": {"code": 200, "body": {"ok": true},
    "fail_if_body_has": "\"assessment\""},
  "GET /api/clusters/test.example.com/desired": {"code": 304, "headers": {"Etag": "\"7-100\""}}
}
JSON
  unset CURL_HOME XDG_CONFIG_HOME http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
  http_stub_start
  export KUBEHZ_API_URL="http://127.0.0.1:${STUB_PORT}"
  mkdir -p "${BATS_TEST_TMPDIR}/curlrc-none"
  ( export HOME="${BATS_TEST_TMPDIR}/curlrc-none" && bash --posix "${real}" ) > "${ref}.out" 2> "${ref}.err" || ref_rc=$?
  [ "${ref_rc}" -eq 0 ]
  grep -qx '{"ok":true}' "${ref}.out"
  grep -qF 'retrying once without it' "${ref}.err"
  for var in CURL_HOME XDG_CONFIG_HOME HOME; do
    echo "# config file in ${var}"
    curlrc_dir "${var}"
    o="${RC_DIR}.stdout" e="${RC_DIR}.stderr" rc=0
    ( curlrc_env "${var}" && bash --posix "${real}" ) > "${o}" 2> "${e}" || rc=$?
    [ ! -e "${RC_TRACE}" ]
    [ ! -e "${RC_OUT}" ]
    diff "${ref}.out" "${o}"
    diff "${ref}.err" "${e}"
    [ "${rc}" -eq "${ref_rc}" ]
    run grep -qF -e "${STUB_AGENT_TOKEN}" -e "${STUB_CLAIM_CODE}" -e "${BIND_SECRET}" "${o}" "${e}"
    [ "${status}" -eq 1 ]
    if [[ "${var}" == XDG_CONFIG_HOME ]] && ! curl_reads_xdg; then
      echo "# no control for XDG_CONFIG_HOME: this curl is older than 7.73 and does not look there" >&3
    else
      curlrc_control "${var}" "${KUBEHZ_API_URL}/control"
      controls=$((controls + 1))
    fi
  done
  [ "$(grep -c '^POST /api/clusters/agent-register none bind=yes$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^POST /api/clusters/test.example.com/heartbeat bearer$' "${STUB_LOG}")" -eq 8 ]
  [ "$(grep -c '^GET /api/clusters/test.example.com/desired bearer$' "${STUB_LOG}")" -eq 4 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${controls}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((16 + controls)) ]
  http_stub_stop
}

# ── the TLS log and trace controls (the real curl against a local server) ──
# The pod gets each key of the agent ConfigMap as an environment variable
# (envFrom), so the script clears the TLS log and trace controls itself
# before its first curl call (the image has no utils/http.sh). The script
# runs against the https stub with each name of _HTTP_TLS_LOG_VARS
# (utils/http.sh) set and a spy for curl (tls_spy_on: a record of names,
# never a value, then the real curl): the four calls (register, beat, plain
# retry, desired poll) get none of them, each record has the control, and
# no key log or trace file is written. The key log control first: this
# curl writes a key log for a request to the same server when SSLKEYLOGFILE
# reaches it. A curl that writes none leaves the record check alone. A
# control request that does not get to the stub fails the test. Then a
# readonly SSLKEYLOGFILE: the script stops with the line that names it,
# curl does not run and no request reaches the stub.
@test "heartbeat: curl gets no TLS log or trace variable, and a readonly one stops the script" {
  command -v python3 >/dev/null || skip "no python3"
  [[ -n "$(type -P curl)" ]] || skip "no curl"
  [[ -n "$(command -v openssl)" ]] || skip "no openssl: the https stub needs a self-signed certificate"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"
  local real="${BATS_TEST_TMPDIR}/run-spy-curl.sh" spy="${BATS_TEST_TMPDIR}/curl-env"
  local o="${BATS_TEST_TMPDIR}/tls.stdout" e="${BATS_TEST_TMPDIR}/tls.stderr" hex name f before rc=0 kc=0
  (( ${#_HTTP_TLS_LOG_VARS[@]} >= 6 ))
  hex=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  export STUB_AGENT_TOKEN="khz_agt_${hex}" STUB_CLAIM_CODE="khzc_${hex}" STUB_BEARER="khz_agt_${hex}"
  export BIND_SECRET="${hex}${hex}${hex}${hex}"
  export STUB_BIND_SECRET="${BIND_SECRET}"
  export STUB_ROUTES="${BATS_TEST_TMPDIR}/routes.json"
  cat > "${STUB_ROUTES}" <<'JSON'
{
  "POST /api/clusters/agent-register": {"code": 201, "bind_secret": true, "body": {"ok": true}},
  "POST /api/clusters/test.example.com/heartbeat": {"code": 200, "body": {"ok": true},
    "fail_if_body_has": "\"assessment\""},
  "GET /api/clusters/test.example.com/desired": {"code": 304, "headers": {"Etag": "\"7-100\""}}
}
JSON
  unset CURL_HOME XDG_CONFIG_HOME http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
  export HOME="${BATS_TEST_TMPDIR}/curlrc-none"
  mkdir -p "${HOME}"
  stub_cert
  http_stub_start "${STUB_CERT}" "${STUB_KEY}"
  export CURL_CA_BUNDLE="${STUB_CERT}" KUBEHZ_API_URL="https://127.0.0.1:${STUB_PORT}"
  keylog_control || kc=$?
  # 2: the control request did not get to the stub. That is a fault, not a skip.
  [ "${kc}" -ne 2 ]
  if (( kc != 0 )); then
    echo "# this curl writes no TLS key log (its TLS backend ignores SSLKEYLOGFILE): the record check alone" >&3
  fi
  # The runner without the curl stub: the spy, then the real curl.
  tls_spy_on "${spy}"
  { cat "${_PROJECT_ROOT}/tests/lib/curl_capture.sh" "${BATS_TEST_TMPDIR}/stubs.sh"
    echo 'unset -f curl'
    declare -f curl
    cat "${HEARTBEAT}"; } > "${real}"
  unset -f curl
  for name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    case "${name}" in
      GNUTLS_DEBUG_LEVEL) export "${name}=9" ;;
      SSLTRACE | SSLDEBUG) export "${name}=99" ;;
      *) export "${name}=${BATS_TEST_TMPDIR}/tls.${name}" ;;
    esac
  done
  export TLS_ENV_CONTROL=on
  bash --posix "${real}" > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 0 ]
  grep -qx '{"ok":true}' "${o}"
  [ "$(find "${spy}" -type f | wc -l)" -eq 4 ]
  for f in "${spy}"/env.*; do
    [ "$(cat "${f}")" = $'record\ncontrol' ]
  done
  for name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    [ ! -e "${BATS_TEST_TMPDIR}/tls.${name}" ]
  done
  [ "$(grep -c '^POST /api/clusters/agent-register none bind=yes$' "${STUB_LOG}")" -eq 1 ]
  [ "$(grep -c '^POST /api/clusters/test.example.com/heartbeat bearer$' "${STUB_LOG}")" -eq 2 ]
  [ "$(grep -c '^GET /api/clusters/test.example.com/desired bearer$' "${STUB_LOG}")" -eq 1 ]
  find "${spy}" -type f -name 'env.*' -delete
  before=$(wc -l < "${STUB_LOG}")
  { echo 'readonly SSLKEYLOGFILE'; cat "${real}"; } > "${real}.ro"
  rc=0
  bash --posix "${real}.ro" > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 1 ]
  grep -qxF 'kubehz: SSLKEYLOGFILE is readonly, so the agent cannot clear it for curl: no request is sent' "${e}"
  # That line only: the script stops at its check, before the unset that
  # the readonly name would fail with a message of the shell.
  [ "$(wc -l < "${e}")" -eq 1 ]
  [ "$(find "${spy}" -type f | wc -l)" -eq 0 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq "${before}" ]
  [ ! -e "${SSLKEYLOGFILE}" ]
  http_stub_stop
}
