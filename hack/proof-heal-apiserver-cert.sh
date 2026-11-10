#!/usr/bin/env bash
# proof-heal-apiserver-cert.sh — a real proof, on a throwaway kind cluster,
# that the node-IP heal renews the apiserver certificate of a control-plane
# node (Go: internal/driver/lo/heal.go, bash: lo::heal_node_ips in
# drivers/lo/utils/network.sh).
#
# NOT wired into CI. Run it by hand on a machine with docker and the
# toolchain in .bin (b install). It makes one cluster (one control-plane
# node, one worker) with shared registries on, and removes it again.
#
# Usage: hack/proof-heal-apiserver-cert.sh <lo> <lo-without-the-fix>
#   <lo>                  the binary under test (make build: bin/lo)
#   <lo-without-the-fix>  a binary whose heal does not renew the certificate,
#                         for example one built from a commit before the fix.
#                         Phase 1 must show the bug with it, or the proof stops.
#
# Environment:
#   PROOF_PREFIX       name prefix of everything the proof makes
#                      (default lo-heal-proof)
#   PROOF_K8S_VERSION  kindest/node tag (default: the e2e-go-roundtrip pin)
#   PROOF_WINDOW       seconds of logs that the x509 count reads (default 60)
#   PROOF_EVIDENCE     directory for the logs of each step
#                      (default: a new directory under ${TMPDIR:-/tmp})
#
# `lo provision` is the reconcile that `lo up` runs before it starts Tilt, so
# the proof uses it and no Tilt starts. Phases:
#   0. lo provision with <lo>, then kind's default CNI. The cluster is healthy.
#   1. The bug, with <lo-without-the-fix>: force the drift on both nodes, then
#      lo provision. The --node-ip is repaired, but the certificate does not
#      name the cluster-network address, and TLS to that address fails.
#   2. <lo> repairs what phase 1 left: the flag agrees, the certificate does
#      not, so <lo> renews the certificate.
#   3. The full repair with <lo>: force the drift, then lo provision.
#   4. docker restart of the control-plane node. When kind's entrypoint moves
#      the node to the registry address (the drift itself), the heal repairs
#      it; else the certificate stays as it was.
#   5. A renewal that fails: force the drift, move ca.key aside, lo provision.
#      <lo> must warn with the kubeadm reason, keep the old pair and still
#      finish lo provision. A manual kubeadm call with the old pair in place
#      changes nothing. Then ca.key comes back, and the next lo provision
#      renews the certificate.
#   6. The bash twin (lo::heal_node_ips, sourced from .lok8s) on the state
#      that phase 1 makes: it renews the certificate.
#   7. The bash twin with a failing renewal, then again with ca.key back.
#   8. The bash twin, the full repair.
# After each renewal, the controller-manager and scheduler containers that
# ran before it must be gone (the heal stops them, kubelet starts them again).
#
# Safety contract (the same as hack/e2e-go-roundtrip.sh):
#   • every name is <prefix>-<random>: the domain, the cluster, the cluster
#     network, the registry network and the mirror;
#   • the two /24 networks are verified free against every docker network;
#   • --domain is passed on every call, never clusters/.active;
#   • every kind cluster from before must still be there after, except
#     another run of this proof (<prefix>-*), which removes its own cluster;
#     this run's cluster must be gone;
#   • an EXIT trap removes the cluster, every container whose name has the
#     run's name in it with its anonymous volumes, every volume with the run's
#     name, and both networks. It checks that none of them is left, the
#     anonymous volumes included.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
abs_bin() {
  local d
  [[ "${1}" == /* ]] && { echo "${1}"; return; }
  d="$(cd "$(dirname "${1}")" 2>/dev/null && pwd)" || { echo "${1}"; return; }
  echo "${d}/$(basename "${1}")"
}
[[ $# -eq 2 ]] || { echo "usage: ${0} <lo> <lo-without-the-fix>" >&2; exit 2; }
LO_NEW="$(abs_bin "${1}")"
LO_OLD="$(abs_bin "${2}")"
for b in "${LO_NEW}" "${LO_OLD}"; do
  [[ -x "${b}" ]] || { echo "error: ${b} is not an executable" >&2; exit 2; }
done
export PATH="${ROOT}/.bin:${PATH}"
for tool in docker kind kubectl; do
  command -v "${tool}" >/dev/null || { echo "error: ${tool} not available (b install)" >&2; exit 2; }
done

unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME DEBUG KUBECONFIG KUSTOMIZE_PLUGIN_HOME \
  LOK8S_FORCE_RECREATE LOK8S_REMOTE TILT_PORT LOK8S_REGISTRY_IP_CACHE \
  LOK8S_REGISTRY_JSON KIND_EXPERIMENTAL_DOCKER_NETWORK KIND_CONFIG \
  KIND_NODE_VERSION LOK8S_DOMAIN_EXPLICIT KAPPLY_TTY
export LC_ALL=C LOK8S_NONINTERACTIVE=1

PREFIX="${PROOF_PREFIX:-lo-heal-proof}"
K8S_VERSION="${PROOF_K8S_VERSION:-v1.31.12@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2}"
WINDOW="${PROOF_WINDOW:-60}"
RAND="$(head -c 3 /dev/urandom | od -An -tx1 | tr -d ' \n')"
NAME="${PREFIX}-${RAND}"
DOMAIN="${NAME}.dev"
NET="${NAME}"
REGNET="${NAME}-reg"
CP="${NAME}-control-plane"
WK="${NAME}-worker"
POD_SUBNET="10.244.0.0/16"
WORK="$(mktemp -d)"
PROJ="${WORK}/proj"
KCFG="${PROJ}/.kubeconfig/${NAME}.yaml"
EVID="${PROOF_EVIDENCE:-$(mktemp -d "${TMPDIR:-/tmp}/${NAME}-evidence.XXXX")}"
mkdir -p "${EVID}"
export LO_REGISTRY_STATE_DIR="${WORK}/registry-state"

log() { printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
indent() { while IFS= read -r line; do printf '    %s\n' "${line}"; done; }
fail() { echo "FAIL [${STEP}]: $*" >&2; exit 1; }

# A free /24 under 10.<second>.0.0/16, verified against the IPAM config of
# every docker network. The start is random, so two runs on one host rarely
# try the same one.
pick_cidr() {
  local second="${1}" used third i
  used="$(docker network ls -q | xargs -r docker network inspect --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null || true)"
  for i in $(seq 0 239); do
    third=$(( (16#${RAND:0:2} + i) % 240 + 10 ))
    if [[ " ${used} " != *" 10.${second}.${third}."* ]]; then
      echo "10.${second}.${third}.0/24"
      return 0
    fi
  done
  return 1
}
CIDR="$(pick_cidr 213)" || { echo "error: no free 10.213.x.0/24" >&2; exit 2; }
REGCIDR="$(pick_cidr 214)" || { echo "error: no free 10.214.x.0/24" >&2; exit 2; }

BEFORE="$(kind get clusters 2>/dev/null | sort)"
log "kind get clusters (before):"
indent <<<"${BEFORE}"

STEP="setup"
teardown() {
  local rc=$? after left gone c v anon
  set +e
  echo
  log "teardown (last step: ${STEP}, rc ${rc})"
  # The anonymous volumes of this run's containers (for example /var of a kind
  # node), read before any container goes. They have no name to filter by, so
  # the name check below does not see them. A name of 64 hex digits is the
  # name docker gives an anonymous volume; a named volume of another project
  # never matches.
  anon="$(docker ps -aq --filter "name=${NAME}" \
    | xargs -r docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}' 2>/dev/null \
    | grep -E '^[0-9a-f]{64}$' | sort -u)"
  if [[ -d "${PROJ}" ]]; then
    (cd "${PROJ}" && "${LO_NEW}" down --domain "${DOMAIN}") >"${EVID}/teardown-down.log" 2>&1
    (cd "${PROJ}" && "${LO_NEW}" destroy --domain "${DOMAIN}") >"${EVID}/teardown-destroy.log" 2>&1
  fi
  if kind get clusters 2>/dev/null | grep -qx "${NAME}"; then
    log "cluster ${NAME} survived lo down and lo destroy: deleting it with kind"
    kind delete cluster --name "${NAME}" >/dev/null 2>&1
  fi
  # Every container and volume of this run has the run's name in its name
  # (the shared mirror is lok8s-registry-<name>).
  docker ps -aq --filter "name=${NAME}" | xargs -r docker rm -f -v >/dev/null 2>&1
  docker volume ls -q --filter "name=${NAME}" | xargs -r docker volume rm -f >/dev/null 2>&1
  # A container that lo or kind removed without -v leaves its anonymous
  # volumes behind.
  while IFS= read -r v; do
    [[ -z "${v}" ]] || docker volume rm -f "${v}" >/dev/null 2>&1
  done <<<"${anon}"
  docker network rm "${REGNET}" >/dev/null 2>&1
  docker network rm "${NET}" >/dev/null 2>&1
  rm -rf "${WORK}"

  # Every cluster from before must still be there, and this run's cluster
  # must be gone. Another run of this proof (<prefix>-*) removes its own
  # cluster meanwhile, so the check skips those names.
  after="$(kind get clusters 2>/dev/null | sort)"
  log "kind get clusters (after):"
  indent <<<"${after}"
  gone=""
  while IFS= read -r c; do
    [[ -n "${c}" && "${c}" != "${PREFIX}-"* ]] || continue
    grep -qx -- "${c}" <<<"${after}" || gone+=" ${c}"
  done <<<"${BEFORE}"
  if [[ -n "${gone}" ]]; then
    echo "FAIL: kind clusters from before the run are gone:${gone}" >&2
    rc=1
  fi
  if grep -qx "${NAME}" <<<"${after}"; then
    echo "FAIL: the cluster ${NAME} survived the teardown" >&2
    rc=1
  fi
  left="$(docker ps -aq --filter "name=${NAME}"; docker volume ls -q --filter "name=${NAME}"; docker network ls -q --filter "name=${NAME}")"
  while IFS= read -r v; do
    if [[ -n "${v}" ]] && docker volume inspect "${v}" >/dev/null 2>&1; then left+=" ${v}"; fi
  done <<<"${anon}"
  log "anonymous volumes of the run's containers: $(wc -w <<<"${anon}"); objects left after the teardown: $(wc -w <<<"${left}")"
  if [[ -n "${left}" ]]; then
    left="${left//$'\n'/ }"
    echo "FAIL: containers, volumes or networks of ${NAME} survived the teardown: ${left# }" >&2
    rc=1
  fi
  if (( rc == 0 )); then
    log "proof: PASS — ${NAME} came and went (evidence: ${EVID})"
  else
    log "proof: FAILED at step '${STEP}' (rc ${rc}; evidence: ${EVID})" >&2
  fi
  exit "${rc}"
}
trap teardown EXIT

# ── helpers that read the cluster ───────────────────────────────────────────
addr_on() { docker inspect "${1}" --format "{{with index .NetworkSettings.Networks \"${2}\"}}{{.IPAddress}}{{end}}"; }
node_ip_flag() { docker exec "${1}" sed -n 's/.*--node-ip=\([0-9.]*\).*/\1/p' /var/lib/kubelet/kubeadm-flags.env; }
kc() { docker exec "${CP}" kubectl --kubeconfig /etc/kubernetes/admin.conf "$@"; }
# The IP SANs of the certificate file, as "IPAddress:a,IPAddress:b,".
san_file() {
  docker exec "${CP}" openssl x509 -in /etc/kubernetes/pki/apiserver.crt -noout -ext subjectAltName |
    tail -n +2 | tr -d '[:space:]' | tr ',' '\n' | grep '^IPAddress:' | tr '\n' ','
}
cert_sum() { docker exec "${CP}" sha256sum /etc/kubernetes/pki/apiserver.crt | cut -d' ' -f1; }
# The verify code that a client of https://<addr>:6443 gets: it checks the
# served certificate against the cluster CA and <addr> (0 = ok, 64 = IP
# address mismatch).
served_code() {
  docker exec "${CP}" sh -c "openssl s_client -connect '${1}:6443' -CAfile /etc/kubernetes/pki/ca.crt -verify_ip '${1}' </dev/null 2>/dev/null" |
    sed -n 's/^ *Verify return code: \([0-9]*\).*/\1/p' | tail -1
}
node_mark() { docker exec "${CP}" date -u +%Y-%m-%dT%H:%M:%S; }
# The lines with x509 or "bad certificate" that were logged after <mark>:
# controller-manager, scheduler and apiserver (their pod logs on the
# control-plane node) and kubelet on both nodes.
x509_after() {
  local mark="${1}" n
  docker exec -i "${CP}" bash -s -- "${mark}" <<'COUNT'
since=$1
pod() {
  # A CRI log line starts with its RFC 3339 time stamp.
  cat /var/log/pods/kube-system_"$1"-*/*/*.log 2>/dev/null |
    awk -v s="${since}" 'substr($1, 1, 19) > s' | grep -c -e x509 -e 'bad certificate' || true
}
printf 'controller-manager=%s scheduler=%s apiserver=%s ' \
  "$(pod kube-controller-manager)" "$(pod kube-scheduler)" "$(pod kube-apiserver)"
COUNT
  for n in "${CP}" "${WK}"; do
    printf 'kubelet[%s]=%s ' "${n##*-}" "$(docker exec "${n}" bash -c \
      "journalctl -u kubelet --since @\$(( \$(date -u -d '${mark}' +%s) + 1 )) --no-pager -q | grep -c x509 || true")"
  done
  echo
}
x509_total() {
  local t=0 kv
  local -a kvs
  read -ra kvs <<<"${1}"
  for kv in "${kvs[@]}"; do t=$(( t + ${kv##*=} )); done
  echo "${t}"
}

wait_api() {
  local i
  for i in $(seq 1 90); do
    if kc get --raw /readyz >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  fail "the apiserver did not answer /readyz in 180 s"
}
wait_served_ok() {
  local i
  for i in $(seq 1 60); do
    [[ "$(served_code "${1}")" == "0" ]] && return 0
    sleep 2
  done
  return 1
}

# ── helpers that change the cluster ─────────────────────────────────────────
# What kind's entrypoint does when docker DNS answers with the registry
# network address at boot: every kubeadm and kubelet file moves to that
# address, and a control-plane node gets a certificate for it.
drift() {
  local node="${1}" reg old
  reg="$(addr_on "${node}" "${REGNET}")"
  old="$(node_ip_flag "${node}")"
  [[ -n "${reg}" && -n "${old}" ]] || fail "drift ${node}: no address (registry network '${reg}', --node-ip '${old}')"
  [[ "${reg}" != "${old}" ]] || fail "drift ${node}: --node-ip is already ${reg}"
  docker exec -i "${node}" bash -s -- "${old}" "${reg}" <<'DRIFT'
set -e
old_re=${1//./\\.}
for f in /etc/kubernetes/manifests/etcd.yaml /etc/kubernetes/manifests/kube-apiserver.yaml \
         /etc/kubernetes/manifests/kube-controller-manager.yaml /etc/kubernetes/manifests/kube-scheduler.yaml \
         /etc/kubernetes/controller-manager.conf /etc/kubernetes/scheduler.conf /etc/kubernetes/kubelet.conf \
         /kind/kubeadm.conf /var/lib/kubelet/kubeadm-flags.env; do
  if [ -f "$f" ]; then sed -i "s|\b${old_re}\b|$2|g" "$f"; fi
done
if [ -f /etc/kubernetes/pki/apiserver.key ]; then
  rm -f /etc/kubernetes/pki/apiserver.crt /etc/kubernetes/pki/apiserver.key
  kubeadm init phase certs apiserver --config /kind/kubeadm.conf >/dev/null
fi
printf '%s' "$2" > /kind/old-ipv4
systemctl restart kubelet
DRIFT
  log "drift: ${node} --node-ip ${old} -> ${reg}"
}
# drift_both sets WANT and REG: the cluster-network and registry-network
# addresses of the control-plane node.
WANT=""
REG=""
drift_both() {
  WANT="$(addr_on "${CP}" "${NET}")"
  REG="$(addr_on "${CP}" "${REGNET}")"
  drift "${CP}"
  drift "${WK}"
  wait_api
  sleep 20
  [[ "$(san_file)" == *"IPAddress:${REG},"* ]] ||
    fail "after the drift the certificate does not name the registry address ${REG}: $(san_file)"
}
# The reason in the "could not repair" warning must be kubeadm's, not empty.
failed_with_reason() {
  local line
  line="$(grep -F "could not repair ${CP}: " <<<"${OUT}" || true)"
  [[ -n "${line}" ]] || fail "no warning 'could not repair ${CP}'"
  [[ "${line}" == *". The next lo up tries again." ]] || fail "the warning does not end with the retry note: ${line}"
  [[ "${line}" != *"no error output"* ]] || fail "the warning has no kubeadm reason: ${line}"
  log "failed renewal warning: ${line}"
}

# The containers of the control-plane node, the pki files, the logs of the
# apiserver, controller-manager and scheduler, and the kubelet lines about the
# static pods: for a step that failed.
diag() {
  local id c
  set +e
  {
    log "diagnostics of ${CP}"
    docker exec "${CP}" crictl ps -a
    docker exec "${CP}" ls -l /etc/kubernetes/pki /kind/heal-pki
    for c in kube-apiserver kube-controller-manager kube-scheduler; do
      for id in $(docker exec "${CP}" crictl ps -a -q --name "^${c}\$" | head -3); do
        echo "--- ${c} ${id}"
        docker exec "${CP}" crictl inspect --output go-template --template '{{.status.startedAt}} {{.status.finishedAt}} exit={{.status.exitCode}} {{.status.reason}}' "${id}"
        echo
        docker exec "${CP}" crictl logs --tail 30 "${id}" 2>&1
      done
    done
    echo "--- kubelet (last 5 min)"
    docker exec "${CP}" journalctl -u kubelet --since -5min --no-pager -q | grep -iE 'apiserver|kill|probe|static|Started|Stopp' | tail -60
  } >"${EVID}/${STEP}-diag.log" 2>&1
  indent <"${EVID}/${STEP}-diag.log"
  set -e
}

# No phase makes a renewal that keeps the old certificate, so the warning of
# the certificate check after a repair script must never appear.
renewal_complete() {
  [[ "${OUT}" != *"did not complete"* ]] || fail "the certificate check after a repair failed: ${OUT}"
}
# lo provision with <bin>; the output goes to the evidence directory and to
# the log. A non-zero exit fails the proof.
OUT=""
provision() {
  local bin="${1}" tag="${2}" rc=0
  log "lo provision --domain ${DOMAIN} (${tag})"
  (cd "${PROJ}" && "${bin}" provision --domain "${DOMAIN}") >"${EVID}/${STEP}-${tag}.log" 2>&1 || rc=$?
  OUT="$(cat "${EVID}/${STEP}-${tag}.log")"
  indent <<<"${OUT}"
  (( rc == 0 )) || { diag; fail "lo provision exited ${rc}"; }
  renewal_complete
}
# The heal of the bash twin, sourced from the tree and run against the real
# cluster: lo::heal_node_ips with the real docker, kind and kubectl.
bash_heal() {
  local tag="${1}" rc=0
  log "bash: lo::heal_node_ips ${NAME} (${tag})"
  KIND_EXPERIMENTAL_DOCKER_NETWORK="${NET}" bash -c '
    warn() { printf "[warn] %s\n" "$*" >&2; }
    import() { :; }
    source "$1/.lok8s/utils/ip.sh"
    source "$1/.lok8s/drivers/lo/utils/network.sh"
    lo::heal_node_ips "$2" "$3"' _ "${ROOT}" "${NAME}" "${KCFG}" >"${EVID}/${STEP}-${tag}.log" 2>&1 || rc=$?
  OUT="$(cat "${EVID}/${STEP}-${tag}.log")"
  indent <<<"${OUT}"
  (( rc == 0 )) || { diag; fail "lo::heal_node_ips returned ${rc}"; }
  renewal_complete
}
# The certificate check of the bash twin on the real node (D43: openssl in
# the node, not crypto/x509).
bash_reason() {
  bash -c '
    import() { :; }
    source "$1/.lok8s/utils/ip.sh"
    source "$1/.lok8s/drivers/lo/utils/network.sh"
    lo::heal_cert_reason "$2" "$3"' _ "${ROOT}" "${CP}" "${1}"
}
# The running controller-manager and scheduler containers of the
# control-plane node, selected by exact name (not with the pattern of the
# heal).
cs_ids() {
  local c
  for c in kube-controller-manager kube-scheduler; do
    docker exec "${CP}" crictl ps -q --name "^${c}\$" | tr '\n' ' '
  done
}
# After a renewal, none of the containers in <ids> (cs_ids from before the
# heal) may still run.
CS_BEFORE=""
cs_mark() {
  CS_BEFORE="$(cs_ids)"
  [[ -n "${CS_BEFORE// /}" ]] || fail "no running controller-manager or scheduler on ${CP} before the heal"
}
cs_restarted() {
  local now id
  now="$(cs_ids)"
  for id in ${CS_BEFORE}; do
    [[ " ${now} " != *" ${id} "* ]] || fail "the controller-manager or scheduler container ${id} still runs after the renewal"
  done
  log "controller-manager and scheduler: ${CS_BEFORE}-> ${now}"
}
has() { [[ "${OUT}" == *"${1}"* ]] || fail "the output has no line with: ${1}"; }
hasnt() { [[ "${OUT}" != *"${1}"* ]] || fail "the output has a line with: ${1}"; }
move_ca_key() { docker exec "${CP}" mv /etc/kubernetes/pki/ca.key /kind/proof-ca.key; log "ca.key moved to /kind/proof-ca.key"; }
restore_ca_key() { docker exec "${CP}" mv /kind/proof-ca.key /etc/kubernetes/pki/ca.key; log "ca.key back in /etc/kubernetes/pki"; }

# The good state: flag, /kind/old-ipv4 and InternalIP on the cluster network,
# no /kind/heal-pki, a certificate that names the cluster-network address and
# not the registry address, TLS to that address verifies, all nodes Ready,
# and no x509 line in the last PROOF_WINDOW seconds.
check_good() {
  local n want flag old san reg ips mark counts i
  docker exec "${CP}" crictl ps -a --name kube-apiserver | indent
  for n in "${CP}" "${WK}"; do
    want="$(addr_on "${n}" "${NET}")"
    flag="$(node_ip_flag "${n}")"
    old="$(docker exec "${n}" cat /kind/old-ipv4)"
    [[ "${flag}" == "${want}" ]] || fail "${n}: --node-ip=${flag}, want ${want}"
    [[ "${old}" == "${want}" ]] || fail "${n}: /kind/old-ipv4=${old}, want ${want}"
    if docker exec "${n}" test -e /kind/heal-pki; then fail "${n}: /kind/heal-pki is still there"; fi
  done
  want="$(addr_on "${CP}" "${NET}")"
  reg="$(addr_on "${CP}" "${REGNET}")"
  wait_served_ok "${want}" || fail "TLS to ${want}:6443 does not verify (code $(served_code "${want}"))"
  san="$(san_file)"
  [[ "${san}" == *"IPAddress:${want},"* ]] || fail "the certificate does not name ${want}: ${san}"
  [[ "${san}" != *"IPAddress:${reg},"* ]] || fail "the certificate still names the registry address ${reg}: ${san}"
  [[ -z "$(bash_reason "${want}")" ]] || fail "lo::heal_cert_reason finds a fault in a good certificate: $(bash_reason "${want}")"
  kc wait --for=condition=Ready node --all --timeout=180s >/dev/null || fail "not all nodes are Ready"
  for i in $(seq 1 30); do
    ips="$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.addresses[?(@.type=="InternalIP")].address} {end}')"
    [[ "${ips}" == *"${CP}=${want} "* && "${ips}" == *"${WK}=$(addr_on "${WK}" "${NET}") "* ]] && break
    (( i < 30 )) || fail "the InternalIP of the nodes is not the cluster-network address: ${ips}"
    sleep 2
  done
  sleep 20
  mark="$(node_mark)"
  sleep "${WINDOW}"
  counts="$(x509_after "${mark}")"
  log "good: certificate ${san} served verify=0, nodes ${ips}"
  log "x509 lines after ${mark}Z: ${counts}"
  (( $(x509_total "${counts}") == 0 )) || fail "x509 lines after the heal: ${counts}"
}

# ── the throwaway project ───────────────────────────────────────────────────
mkdir -p "${PROJ}/clusters/${DOMAIN}"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
ln -s "${ROOT}/.bin" "${PROJ}/.bin"
printf 'apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: %s\nspec:\n  implementation:\n    default: go\n' "${NAME}" > "${PROJ}/lok8s.yaml"
printf 'Tiltfile\n' > "${PROJ}/Tiltfile"
cat > "${PROJ}/clusters/${DOMAIN}/cluster.lok8s.yaml" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: ${NAME}
spec:
  runtime: kind
  kubernetes:
    version: ${K8S_VERSION}
  nodes:
    controlPlane: 1
    workers: 1
    hostPorts: false
  network:
    name: ${NET}
    cidr: ${CIDR}
  registries:
    tls: false
    shared:
      enabled: true
      network:
        name: ${REGNET}
        cidr: ${REGCIDR}
    mirrors:
      - name: ${NAME}
        url: https://registry-1.docker.io
  bootstrap: []
YAML
log "domain ${DOMAIN}: cluster ${NAME}, network ${NET} (${CIDR}), registry network ${REGNET} (${REGCIDR})"
log "kindest/node ${K8S_VERSION}; evidence in ${EVID}"

# ── 0. a healthy cluster ────────────────────────────────────────────────────
STEP="0-create"
provision "${LO_NEW}" new
[[ -f "${KCFG}" ]] || fail "no kubeconfig at ${KCFG}"
[[ -n "$(addr_on "${CP}" "${REGNET}")" && -n "$(addr_on "${WK}" "${REGNET}")" ]] ||
  fail "the nodes are not on the registry network ${REGNET}"
docker exec "${CP}" sh -c "sed 's|{{ .PodSubnet }}|${POD_SUBNET}|' /kind/manifests/default-cni.yaml |
  kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f -" | indent
kc version | indent
docker exec "${CP}" openssl version | indent
docker exec "${CP}" grep -h 'server:' /etc/kubernetes/controller-manager.conf /etc/kubernetes/scheduler.conf /etc/kubernetes/kubelet.conf | indent
check_good

# ── 1. the bug, with the binary without the fix ─────────────────────────────
STEP="1-bug"
drift_both
CS_BEFORE="$(cs_ids)"
provision "${LO_OLD}" old
log "bug: controller-manager and scheduler: ${CS_BEFORE}-> $(cs_ids)"
has "lo: ${CP} registered --node-ip=${REG} (wrong network) — repointing to ${WANT} on ${NET}"
[[ "$(node_ip_flag "${CP}")" == "${WANT}" ]] || fail "the old heal did not repair the --node-ip"
wait_api
sleep 30
SAN="$(san_file)"
CODE="$(served_code "${WANT}")"
MARK="$(node_mark)"
sleep "${WINDOW}"
COUNTS="$(x509_after "${MARK}")"
REASON="$(bash_reason "${WANT}")"
log "bug: --node-ip=${WANT}, certificate ${SAN} served verify=${CODE}"
log "bug: x509 lines after ${MARK}Z: ${COUNTS}"
log "bug: lo::heal_cert_reason: ${REASON}"
[[ "${SAN}" != *"IPAddress:${WANT},"* && "${SAN}" == *"IPAddress:${REG},"* ]] ||
  fail "the bug did not show: the certificate names ${WANT} after the old heal (${SAN})"
[[ "${CODE}" == "64" ]] || fail "the bug did not show: TLS to ${WANT}:6443 gives verify code '${CODE}', not 64 (IP address mismatch)"
(( $(x509_total "${COUNTS}") > 0 )) || fail "the bug did not show: no x509 line after the old heal"
[[ "${REASON}" == *"does not name that address"* ]] || fail "lo::heal_cert_reason does not see the stale certificate: '${REASON}'"

# ── 2. the fix repairs what the old heal left ───────────────────────────────
STEP="2-renew"
cs_mark
provision "${LO_NEW}" new
has "lo: ${CP} has --node-ip=${WANT}, but its apiserver certificate does not name that address — renewing the certificate"
hasnt "could not repair"
hasnt "lo: ${WK} has --node-ip"
cs_restarted
check_good

# ── 3. the full repair ──────────────────────────────────────────────────────
STEP="3-full"
drift_both
cs_mark
provision "${LO_NEW}" new
has "lo: ${CP} registered --node-ip=${REG} (wrong network) — repointing to ${WANT} on ${NET}"
has "lo: ${WK} registered --node-ip=$(addr_on "${WK}" "${REGNET}") (wrong network) — repointing to $(addr_on "${WK}" "${NET}") on ${NET}"
hasnt "could not repair"
hasnt "renewing the certificate"
cs_restarted
check_good

# ── 4. a restart of the control-plane node ──────────────────────────────────
STEP="4-restart"
SUM="$(cert_sum)"
log "docker restart ${CP}"
docker restart "${CP}" >/dev/null
wait_api
WANT="$(addr_on "${CP}" "${NET}")"
if [[ "$(node_ip_flag "${CP}")" == "${WANT}" ]]; then
  [[ "$(cert_sum)" == "${SUM}" ]] || fail "the restart changed the certificate, but the address did not change"
  wait_served_ok "${WANT}" || fail "after the restart TLS to ${WANT}:6443 does not verify (code $(served_code "${WANT}"))"
  log "restart: same address ${WANT}, same certificate, TLS verifies"
  provision "${LO_NEW}" new
  hasnt "repointing"
  hasnt "renewing the certificate"
  hasnt "could not repair"
else
  log "restart: kind's entrypoint moved the --node-ip to $(node_ip_flag "${CP}") (the drift itself); the heal must repair it"
  cs_mark
  provision "${LO_NEW}" new
  hasnt "could not repair"
  cs_restarted
fi
check_good

# ── 5. a renewal that fails, then succeeds ──────────────────────────────────
STEP="5-fail"
drift_both
SUM="$(cert_sum)"
move_ca_key
provision "${LO_NEW}" new
has "lo: ${CP} registered --node-ip=${REG} (wrong network) — repointing to ${WANT} on ${NET}"
failed_with_reason
hasnt "could not repair ${WK}"
[[ "$(cert_sum)" == "${SUM}" ]] || fail "the failed renewal did not keep the old certificate"
docker exec "${CP}" test -f /etc/kubernetes/pki/apiserver.key || fail "the failed renewal lost apiserver.key"
if docker exec "${CP}" test -e /kind/heal-pki; then fail "the failed renewal left /kind/heal-pki"; fi
[[ "$(docker exec "${CP}" cat /kind/old-ipv4)" == "${REG}" ]] || fail "the failed renewal changed /kind/old-ipv4"
[[ "$(node_ip_flag "${CP}")" == "${WANT}" ]] || fail "the failed renewal did not repair the --node-ip file"
log "failed renewal: old certificate kept (${SUM}), /kind/old-ipv4 still ${REG}, no /kind/heal-pki"
# The guide says: a manual kubeadm call with the old pair in place changes
# nothing and reports no error.
MANUAL_RC=0
MANUAL="$(docker exec "${CP}" kubeadm init phase certs apiserver --config /kind/kubeadm.conf 2>&1)" || MANUAL_RC=$?
indent <<<"${MANUAL}"
log "manual kubeadm with the old pair in place: rc=${MANUAL_RC}"
[[ "$(cert_sum)" == "${SUM}" ]] || fail "the manual kubeadm call changed the certificate"
(( MANUAL_RC == 0 )) || fail "the manual kubeadm call failed (the guide says it reports no error)"
restore_ca_key
STEP="5-rerun"
cs_mark
provision "${LO_NEW}" new
has "lo: ${CP} has --node-ip=${WANT}, but its apiserver certificate does not name that address — renewing the certificate"
hasnt "could not repair"
cs_restarted
check_good

# ── 6. the bash twin on the state of the old heal ───────────────────────────
STEP="6-bash-renew"
drift_both
provision "${LO_OLD}" old
REASON="$(bash_reason "${WANT}")"
[[ "${REASON}" == *"does not name that address"* ]] || fail "lo::heal_cert_reason does not see the stale certificate: '${REASON}'"
cs_mark
bash_heal bash
has "lo: ${CP} has --node-ip=${WANT}, but its apiserver certificate does not name that address — renewing the certificate"
hasnt "could not repair"
cs_restarted
check_good

# ── 7. the bash twin with a failing renewal, then with ca.key back ──────────
STEP="7-bash-fail"
drift_both
SUM="$(cert_sum)"
move_ca_key
bash_heal bash
has "lo: ${CP} registered --node-ip=${REG} (wrong network) — repointing to ${WANT} on ${NET}"
failed_with_reason
hasnt "could not repair ${WK}"
[[ "$(cert_sum)" == "${SUM}" ]] || fail "the failed renewal did not keep the old certificate"
if docker exec "${CP}" test -e /kind/heal-pki; then fail "the failed renewal left /kind/heal-pki"; fi
[[ "$(docker exec "${CP}" cat /kind/old-ipv4)" == "${REG}" ]] || fail "the failed renewal changed /kind/old-ipv4"
restore_ca_key
STEP="7-bash-rerun"
cs_mark
bash_heal bash
has "lo: ${CP} has --node-ip=${WANT}, but its apiserver certificate does not name that address — renewing the certificate"
hasnt "could not repair"
cs_restarted
check_good

# ── 8. the bash twin, the full repair ───────────────────────────────────────
STEP="8-bash-full"
drift_both
cs_mark
bash_heal bash
has "lo: ${CP} registered --node-ip=${REG} (wrong network) — repointing to ${WANT} on ${NET}"
hasnt "could not repair"
hasnt "renewing the certificate"
cs_restarted
check_good

STEP="done"
