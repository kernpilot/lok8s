#!/usr/bin/env bash
# e2e-go-roundtrip.sh — ONE controlled, real round-trip of the Go
# orchestration commands (provision → status → down → destroy) against a
# SYNTHETIC kind cluster. The Phase-3/4 exit gate of the Go migration: the
# parity harnesses only ever reach stubs; this is the single place the Go
# `lo provision` creates a real cluster with real registries on a real
# docker network and tears every piece of it down again.
#
# NOT wired into CI (the coordinator decides). Run by hand, on a machine
# with docker + kind + the toolchain in .bin.
#
# Safety contract (the machine this was written on has LIVE kind clusters
# `local` and `kubehz-dev`, a Tilt on 10466 and live registry containers):
#   • everything is named lo-e2e-<random>: the domain, the cluster, the
#     docker network, the registry containers/volumes;
#   • an unused 10.213.x.0/24 is picked and VERIFIED free before use;
#   • --domain is passed EXPLICITLY on every call — never clusters/.active;
#   • the pre-existing kind clusters are snapshotted before and asserted
#     unchanged after;
#   • an EXIT trap tears the synthetic cluster down on ANY exit, then
#     removes what the framework leaves behind by design (the project
#     docker network — `lo down`/`destroy` keep it across runs, bash and
#     Go alike).
#
# Usage: hack/e2e-go-roundtrip.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

# indent — prefix every line on stdin with two spaces (a per-line anchor is
# not expressible as a parameter expansion, so no sed; keeps SC2001 quiet).
indent() { while IFS= read -r line; do printf '  %s\n' "${line}"; done; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }
for tool in docker kind; do
  command -v "${tool}" >/dev/null || [[ -x "${ROOT}/.bin/${tool}" ]] || { echo "error: ${tool} not available" >&2; exit 2; }
done

# Never inherit a dev shell's project pointers — every path must be the
# synthetic project's.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME DEBUG KUBECONFIG KUSTOMIZE_PLUGIN_HOME \
  LOK8S_FORCE_RECREATE LOK8S_REMOTE TILT_PORT LOK8S_REGISTRY_IP_CACHE \
  LOK8S_REGISTRY_JSON KIND_EXPERIMENTAL_DOCKER_NETWORK KIND_CONFIG \
  KIND_NODE_VERSION LOK8S_DOMAIN_EXPLICIT KAPPLY_TTY
export LC_ALL=C
# Deterministic passthrough output (no collapsing progress UI) and no
# prompts anywhere.
export LOK8S_NONINTERACTIVE=1

RAND="$(head -c 3 /dev/urandom | od -An -tx1 | tr -d ' \n')"
NAME="lo-e2e-${RAND}"
DOMAIN="${NAME}.dev"
NET="${NAME}"
export PATH="${ROOT}/.bin:${PATH}"

# An unused /24 under 10.213/16 — verified against every docker network's
# IPAM config, not just assumed.
pick_cidr() {
  local used third
  used="$(docker network ls -q | xargs -r docker network inspect --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null || true)"
  for third in $(seq 10 250); do
    if ! grep -q "10\.213\.${third}\." <<<"${used}"; then
      echo "10.213.${third}.0/24"
      return 0
    fi
  done
  return 1
}
CIDR="$(pick_cidr)" || { echo "error: no free 10.213.x.0/24" >&2; exit 2; }

WORK="$(mktemp -d)"
PROJ="${WORK}/proj"
export LO_REGISTRY_STATE_DIR="${WORK}/registry-state"

# ── the pre-existing world, snapshotted ──────────────────────────────────────
BEFORE="$(kind get clusters 2>/dev/null | sort)"
echo "kind get clusters (before):"
indent <<<"${BEFORE}"
if ! grep -qx "${NAME}" <<<"${BEFORE}"; then :; else echo "error: ${NAME} already exists?!" >&2; exit 2; fi

# ── teardown on ANY exit ─────────────────────────────────────────────────────
STEP="setup"
teardown() {
  local rc=$?
  set +e
  echo
  echo "── teardown (last step: ${STEP}, rc ${rc}) ──"
  if [[ -d "${PROJ}" ]]; then
    (cd "${PROJ}" && "${LO_BIN}" down --domain "${DOMAIN}") 2>&1 | sed 's/^/  down: /'
    (cd "${PROJ}" && "${LO_BIN}" destroy --domain "${DOMAIN}") 2>&1 | sed 's/^/  destroy: /'
  fi
  # Belt and braces — nothing named after this run may survive, whatever
  # the commands under test did.
  if kind get clusters 2>/dev/null | grep -qx "${NAME}"; then
    echo "  ! cluster ${NAME} survived lo down/destroy — deleting directly"
    kind delete cluster --name "${NAME}"
  fi
  docker ps -aq --filter "name=^${NET}-registry-" | xargs -r docker rm -f >/dev/null
  docker ps -aq --filter "name=^${NAME}-proxy$" | xargs -r docker rm -f >/dev/null
  docker volume ls -q --filter "name=^${NET}-registry-" | xargs -r docker volume rm -f >/dev/null
  # The project network is LEFT by the framework on purpose (it persists
  # across lo down/up, bash and Go alike); the harness removes it.
  docker network rm "${NET}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"

  # The world after must be the world before.
  local after
  after="$(kind get clusters 2>/dev/null | sort)"
  echo "kind get clusters (after):"
  indent <<<"${after}"
  if [[ "${after}" != "${BEFORE}" ]]; then
    echo "FAIL: pre-existing kind clusters changed" >&2
    diff <(echo "${BEFORE}") <(echo "${after}") >&2 || true
    exit 1
  fi
  if docker network inspect "${NET}" >/dev/null 2>&1; then
    echo "FAIL: docker network ${NET} survived" >&2; exit 1
  fi
  if [[ -n "$(docker ps -aq --filter "name=^${NET}-registry-")" ]]; then
    echo "FAIL: registry containers survived" >&2; exit 1
  fi
  if (( rc == 0 )); then
    echo "e2e: round-trip green — world unchanged (${NAME} came and went)"
  else
    echo "e2e: FAILED at step '${STEP}' (rc ${rc}); synthetic cluster torn down" >&2
  fi
  exit "${rc}"
}
trap teardown EXIT

# ── synthetic project ────────────────────────────────────────────────────────
mkdir -p "${PROJ}/clusters/${DOMAIN}"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
ln -s "${ROOT}/.bin" "${PROJ}/.bin"
printf 'Tiltfile\n' > "${PROJ}/Tiltfile"
cat > "${PROJ}/clusters/${DOMAIN}/cluster.lok8s.yaml" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: ${NAME}
spec:
  runtime: kind
  kubernetes:
    version: v1.31.12@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2
  network:
    name: ${NET}
    cidr: ${CIDR}
  registries:
    tls: false
    shared:
      enabled: false
  bootstrap: []
YAML
echo
echo "synthetic domain ${DOMAIN}: cluster ${NAME}, network ${NET} (${CIDR}), project ${PROJ}"

assert_clusters() {
  local want="${1}" got
  got="$(kind get clusters 2>/dev/null | sort)"
  if [[ "${got}" != "${want}" ]]; then
    echo "FAIL: kind get clusters =" >&2; indent <<<"${got}" >&2
    echo "want:" >&2; indent <<<"${want}" >&2
    return 1
  fi
}
WITH="$(printf '%s\n%s\n' "${BEFORE}" "${NAME}" | sed '/^$/d' | sort)"

# ── 1. provision ─────────────────────────────────────────────────────────────
STEP="provision"
echo; echo "── lo provision --domain ${DOMAIN} ──"
(cd "${PROJ}" && "${LO_BIN}" provision --domain "${DOMAIN}")
assert_clusters "${WITH}"
echo "ok: kind get clusters gained exactly ${NAME} (pre-existing clusters intact)"
[[ -f "${PROJ}/.kubeconfig/${NAME}.yaml" ]] || { echo "FAIL: no .kubeconfig/${NAME}.yaml" >&2; exit 1; }
regs="$(docker ps --format '{{.Names}}' --filter "name=^${NET}-registry-" | sort)"
echo "registry containers:"; indent <<<"${regs}"
[[ -n "${regs}" ]] || { echo "FAIL: no ${NET}-registry-* containers" >&2; exit 1; }
docker network inspect "${NET}" --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}}{{end}}' | sed 's/^/network: /'

# ── 2. status ────────────────────────────────────────────────────────────────
STEP="status"
echo; echo "── lo status --domain ${DOMAIN} ──"
status_out="$(cd "${PROJ}" && "${LO_BIN}" status --domain "${DOMAIN}")"
echo "${status_out}"
grep -qx "Running" <<<"${status_out}" || { echo "FAIL: status is not Running" >&2; exit 1; }
grep -q -- "--- Nodes ---" <<<"${status_out}" || { echo "FAIL: no Nodes section (kubeconfig not picked up)" >&2; exit 1; }
grep -q "${NAME}-control-plane" <<<"${status_out}" || { echo "FAIL: node listing missing" >&2; exit 1; }

# ── 3. down ──────────────────────────────────────────────────────────────────
STEP="down"
echo; echo "── lo down --domain ${DOMAIN} ──"
(cd "${PROJ}" && "${LO_BIN}" down --domain "${DOMAIN}")
assert_clusters "${BEFORE}"
echo "ok: cluster ${NAME} gone, pre-existing clusters intact"
if [[ -n "$(docker ps -aq --filter "name=^${NET}-registry-")" ]]; then
  echo "FAIL: registry containers survived lo down" >&2; exit 1
fi
echo "ok: ${NET}-registry-* containers gone"
status_out="$(cd "${PROJ}" && "${LO_BIN}" status --domain "${DOMAIN}")"
grep -qx "NotFound" <<<"${status_out}" || { echo "FAIL: status after down is not NotFound" >&2; exit 1; }
echo "ok: status → NotFound"

# ── 4. destroy (idempotent on a downed cluster; drops the volumes) ───────────
STEP="destroy"
echo; echo "── lo destroy --domain ${DOMAIN} ──"
(cd "${PROJ}" && "${LO_BIN}" destroy --domain "${DOMAIN}")
assert_clusters "${BEFORE}"
if [[ -n "$(docker volume ls -q --filter "name=^${NET}-registry-")" ]]; then
  echo "FAIL: registry volumes survived lo destroy" >&2; exit 1
fi
echo "ok: ${NET}-registry-* volumes gone"
STEP="done"
