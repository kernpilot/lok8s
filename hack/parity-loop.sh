#!/usr/bin/env bash
# parity-loop.sh — differential test between the Go lo and the argsh lo for
# the Tilt-loop surface: `lo tilt`, `lo image`, `lo env`, `lo hooks`.
#
# Modeled on parity-test.sh: for every covered invocation, runs BOTH
# implementations (the Go binary, and the same binary with LO_IMPL=bash
# forcing the argsh passthrough) against a synthetic project and diffs
# stdout, stderr, and exit codes.
#
# READ-ONLY / ERROR PATHS ONLY. This surface's destructive verbs talk to live
# Tilt sessions, kind clusters and the docker daemon — none of which a parity
# harness may touch. The synthetic project therefore gets its own .bin with
# STUB tilt/kubectl/docker/kind binaries (both implementations resolve tools
# through that directory first), so even the "destructive" invocations below
# only ever exec the stubs. The genuinely dangerous flows (tilt down with a
# pid file → kill/pkill, image cache pulls, image clean, kapply patches) are
# covered by hermetic unit tests with fake runners instead.
#
# Usage: hack/parity-loop.sh [path-to-go-lo]   (default: bin/lo)
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
unset TILT_PORT LOK8S_SERVICE_CONFIG \
  LOK8S_REGISTRY_IP_CACHE LOK8S_REGISTRY_JSON LOK8S_PREFLIGHT \
  LOK8S_FORCE_CLEAR_TERMINATING KIND_EXPERIMENTAL_DOCKER_NETWORK \
  DOCKER_REGISTRY DOCKER_PROJECT DOCKER_TAG PARITY_TAG

# indent — prefix every line on stdin with two spaces (a per-line anchor is
# not expressible as a parameter expansion, so no sed; keeps SC2001 quiet).
indent() { while IFS= read -r line; do printf '  %s\n' "${line}"; done; }

STUB_HTTP_PID=""
parity_cleanup() { [[ -n "${STUB_HTTP_PID}" ]] && kill "${STUB_HTTP_PID}" 2>/dev/null || :; }

# mkproj <dir> — one synthetic project: three domains (Lo / KubeOne /
# deploy), the framework tree, and a guarded .bin (real yq/jq/argsh, GNU
# envsubst, STUBS for every tool that could reach live state).
mkproj() {
  local dir="${1}"
  parity::new_project "${dir}"
  parity::own_bin "${dir}"
  mkdir -p "${dir}/clusters/alpha.dev" "${dir}/clusters/beta.cloud" "${dir}/clusters/gamma.app"
  printf 'kind: Lo\nmetadata:\n  name: alpha\nspec:\n  network:\n    name: paritynet\n    cidr: 10.99.7.0/24\n' \
    > "${dir}/clusters/alpha.dev/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: beta\n' > "${dir}/clusters/beta.cloud/cluster.lok8s.yaml"
  printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${dir}/clusters/gamma.app/deploy.lok8s.yaml"
  printf 'Tiltfile\n' > "${dir}/Tiltfile"

  # The live-state tools REPLACED by stubs.
  parity::stub "${dir}" tilt <<'SH'
#!/usr/bin/env bash
# Parity stub. doctor: a kind env; get session: no apiserver; ci: fail 7
# (the rc-passthrough probe); everything else: succeed silently.
case "${1:-}" in
  doctor) echo "Env: kind"; exit 0 ;;
  get)    exit 1 ;;
  ci)     exit 7 ;;
esac
exit 0
SH
  parity::stub_kubectl_fail "${dir}"
  parity::stub_refuse "${dir}" docker
  parity::stub_refuse "${dir}" kind
  # GNU envsubst, pinned: the dev PATH may resolve a non-GNU envsubst whose
  # semantics differ; the Go port implements the GNU contract.
  if [[ -x /usr/bin/envsubst ]]; then
    ln -sf /usr/bin/envsubst "${dir}/.bin/envsubst"
  fi
}

mkproj "${PROJ}"

# check_stdin <manifest> <argv...> — like check, with a manifest on stdin
# (the `lo tilt preflight` shape: local(..., stdin = artifacts)).
check_stdin() {
  local stdin="${1}"; shift
  PARITY_STDIN="${stdin}" parity::run_pair "$@"
  parity::finish "lo $* (stdin)"
}

# ── lo env services ──────────────────────────────────────────────────────────

check - env services                       # no services.yaml → {}
cat > "${PROJ}/services.yaml" <<'YAML'
registry:
  prefix: lok8s.local
  endpoint: ghcr.io/myorg
  branch: parity
  tag: ${PARITY_TAG}
defaults:
  build: false
services:
  api:
    build: true
  worker:
    build: false
  pinned:
    image: ghcr.io/external/pinned:v1.2.3
YAML
check - env services
check - env services --only-services
check - env services --only-registry
# argsh routes the -s/-r shorthands to the INHERITED globals here (cluster /
# remote), not to --only-*; both are consumed no-ops for the output.
check - env services -s somecluster
check - env services -r
export PARITY_TAG=v42
check - env services --only-registry      # envsubst pass (defined var)
unset PARITY_TAG
cat > "${PROJ}/services.local.yaml" <<'YAML'
services:
  api:
    build: false
YAML
export LOK8S_SERVICE_CONFIG=local
check - env services
check - env services --only-services
unset LOK8S_SERVICE_CONFIG

# ── lo env kustomization — state parity (per-impl clones) ────────────────────
# Stateful (writes clusters/<domain>/artifacts/); each implementation gets its
# OWN clone and the produced files must be byte-identical.

for impl in go bash; do
  mkproj "${WORK}/envk-${impl}"
  cp "${PROJ}/services.yaml" "${WORK}/envk-${impl}/services.yaml"
  printf '# placeholder\n' > "${WORK}/envk-${impl}/clusters/alpha.dev/artifacts.yaml"
done
PARITY_DIR_GO="${WORK}/envk-go" PARITY_DIR_BASH="${WORK}/envk-bash" \
  parity::run_pair --domain alpha.dev env kustomization --no-build
envk_ok=1
parity::compare "env kustomization" || envk_ok=0
for state in clusters/alpha.dev/artifacts/kustomization.yaml clusters/alpha.dev/artifacts/.cache-queue; do
  parity::state_same "${WORK}/envk-bash/${state}" "${WORK}/envk-go/${state}" "${state}" || envk_ok=0
done
parity::record "${envk_ok}" "lo env kustomization --no-build (state)"

# ── lo hooks — validation + no-match paths (cluster-free) ────────────────────

check - hooks recreate                                  # missing --selector
check - hooks apply
check - hooks restart
check - hooks recreate --selector 'a=b;rm -rf /'        # injection guard
check - hooks apply --selector noequalshere             # clause shape
check - hooks recreate --selector 'lok8s.dev/role=ghost'  # no artifact → no match
mkdir -p "${PROJ}/clusters/lok8s.dev"
printf 'kind: Job\nmetadata:\n  name: j1\n  labels: {app: other}\n' \
  > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
check - hooks restart --selector app=nomatch            # artifact present, no match
rm -rf "${PROJ}/clusters/lok8s.dev"

# ── lo tilt — stub-backed lifecycle + preflight gates ────────────────────────

check - tilt status                                     # stub `tilt doctor`
check - tilt ci                                         # stub exit 7 → rc passthrough
check - tilt ci --timeout 90s
rm -f "${PROJ}/.tilt.pid"
check - tilt down                                       # no pid file → silent no-op

# spawn_check <label> <verb>: tilt up / tilt restart: both spawn the STUB
# detached and must write a pid file. (pid VALUES differ by construction;
# existence + stdout are the contract.)
spawn_check() {
  local label="${1}" verb="${2}" impl rc ok=1 d
  for impl in go bash; do
    rm -f "${PROJ}/.tilt.pid" "${PROJ}/.tilt.nohup"
    rc=0
    parity::run "${impl}" "${PROJ}" tilt "${verb}" || rc=$?
    [[ ${rc} -eq 0 && -f "${PROJ}/.tilt.pid" ]] || { echo "FAIL: tilt ${verb} (${impl}): rc=${rc} pidfile=$([[ -f ${PROJ}/.tilt.pid ]] && echo yes || echo no)"; ok=0; }
  done
  d="$(diff "${WORK}/bash.out" "${WORK}/go.out" || true)"
  [[ -z "${d}" ]] || { echo "FAIL: tilt ${verb} stdout differs:"; indent <<<"${d}"; ok=0; }
  parity::record "${ok}" "${label}"
  rm -f "${PROJ}/.tilt.pid" "${PROJ}/.tilt.nohup"
}
spawn_check "lo tilt up (stub spawn + pid file)" up
# tilt restart: down (no pid file → no-op) then up — the same spawn + pid
# contract as tilt up.
spawn_check "lo tilt restart (down no-op + stub spawn + pid file)" restart

# preflight gates (manifest on stdin; kubectl is the failing stub, so the Lo
# path reports "nothing stuck").
check_stdin 'kind: ConfigMap' tilt preflight --domain beta.cloud   # non-kind refusal
export LOK8S_PREFLIGHT=0
check_stdin 'kind: ConfigMap' tilt preflight --domain alpha.dev    # kill switch
unset LOK8S_PREFLIGHT
check_stdin 'kind: ConfigMap' tilt preflight --domain alpha.dev    # sweep, nothing stuck

# ── lo image — gates, error paths, catalog listing ───────────────────────────

check - image cache svc --domain beta.cloud             # non-lo driver gate
check - image list --domain beta.cloud
check - image cache --all --domain alpha.dev            # empty queue → no-op
check - image cache pinned --domain alpha.dev           # explicit image: pin
check - image cache --domain alpha.dev                  # no service, no --all
rm -f "${PROJ}/services.yaml" "${PROJ}/services.local.yaml"
check - image cache svc --domain alpha.dev              # no endpoint configured
# --domain BEFORE the verb (D3): bash main resolves the spec — and with it
# KIND_EXPERIMENTAL_DOCKER_NETWORK, hence the registry name — from the
# domain it parsed before dispatch, so a trailing --domain is invisible to
# that lookup in bash only. Placed first, both name paritynet-registry-cache.
check - --domain alpha.dev image clean                  # stub docker: rm + volume rm fail, both silent

# Unresolvable IP: a Lo spec without spec.network on a non-slot domain.
mkdir -p "${PROJ}/clusters/bare.dev"
printf 'kind: Lo\nmetadata:\n  name: bare\n' > "${PROJ}/clusters/bare.dev/cluster.lok8s.yaml"
check - image cache --all --domain bare.dev
check - image list --domain bare.dev

# Catalog listing against a local HTTP stub (registry /v2/_catalog). The env
# override doubles as the issue-#89 gate-escape probe (KubeOne domain).
if command -v python3 >/dev/null 2>&1; then
  mkdir -p "${WORK}/registry/v2"
  printf '{"repositories":["parity/one","parity/two"]}' > "${WORK}/registry/v2/_catalog"
  (cd "${WORK}/registry" && exec python3 -u -m http.server 0 --bind 127.0.0.1 >"${WORK}/http.log" 2>&1) &
  STUB_HTTP_PID=$!
  for _ in $(seq 1 50); do
    STUB_PORT="$(sed -nE 's/.*port ([0-9]+).*/\1/p' "${WORK}/http.log" | head -1)"
    [[ -n "${STUB_PORT}" ]] && break
    sleep 0.1
  done
  if [[ -n "${STUB_PORT:-}" ]]; then
    export LOK8S_REGISTRY_IP_CACHE="127.0.0.1:${STUB_PORT}"
    check - image list --domain beta.cloud              # override skips the gate
    check - image list --domain alpha.dev
    unset LOK8S_REGISTRY_IP_CACHE
  else
    echo "skip: image list catalog stub (python http.server did not report a port)"
  fi
else
  echo "skip: image list catalog stub (python3 not available)"
fi
# Dead endpoint: header only, rc 0 (the curl|jq quirk both must preserve).
export LOK8S_REGISTRY_IP_CACHE="127.0.0.1:1"
check - image list --domain alpha.dev
unset LOK8S_REGISTRY_IP_CACHE

report
