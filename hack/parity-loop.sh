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
set -euo pipefail

# indent — prefix every line on stdin with two spaces (a per-line anchor is
# not expressible as a parameter expansion, so no sed; keeps SC2001 quiet).
indent() { while IFS= read -r line; do printf '  %s\n' "${line}"; done; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

WORK="$(mktemp -d)"
STUB_HTTP_PID=""
cleanup() {
  [[ -n "${STUB_HTTP_PID}" ]] && kill "${STUB_HTTP_PID}" 2>/dev/null || :
  rm -rf "${WORK}"
}
trap cleanup EXIT

# The harness must run against its synthetic project ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations into that live repo instead of
# ${WORK}.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME DEBUG TILT_PORT LOK8S_SERVICE_CONFIG \
  LOK8S_REGISTRY_IP_CACHE LOK8S_REGISTRY_JSON LOK8S_PREFLIGHT \
  LOK8S_FORCE_CLEAR_TERMINATING KIND_EXPERIMENTAL_DOCKER_NETWORK \
  DOCKER_REGISTRY DOCKER_PROJECT DOCKER_TAG PARITY_TAG

# Pin the C locale: glob/sort ordering must not depend on the dev locale.
export LC_ALL=C

# mkproj <dir> — one synthetic project: three domains (Lo / KubeOne /
# deploy), the framework tree, and a guarded .bin (real yq/jq/argsh, GNU
# envsubst, STUBS for every tool that could reach live state).
mkproj() {
  local proj="${1}"
  mkdir -p "${proj}/clusters/alpha.dev" "${proj}/clusters/beta.cloud" "${proj}/clusters/gamma.app"
  printf 'kind: Lo\nmetadata:\n  name: alpha\nspec:\n  network:\n    name: paritynet\n    cidr: 10.99.7.0/24\n' \
    > "${proj}/clusters/alpha.dev/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: beta\n' > "${proj}/clusters/beta.cloud/cluster.lok8s.yaml"
  printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${proj}/clusters/gamma.app/deploy.lok8s.yaml"
  printf 'Tiltfile\n' > "${proj}/Tiltfile"
  ln -s "${ROOT}/.lok8s" "${proj}/.lok8s"

  # Real toolchain by symlink…
  mkdir -p "${proj}/.bin"
  local entry
  for entry in "${ROOT}"/.bin/*; do
    ln -s "${entry}" "${proj}/.bin/$(basename "${entry}")"
  done
  # …with the live-state tools REPLACED by stubs (rm the symlink first).
  rm -f "${proj}/.bin/tilt" "${proj}/.bin/kubectl" "${proj}/.bin/docker" "${proj}/.bin/kind"
  cat > "${proj}/.bin/tilt" <<'SH'
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
  cat > "${proj}/.bin/kubectl" <<'SH'
#!/usr/bin/env bash
# Parity stub: every kubectl fails silently (no live cluster may be reached).
exit 1
SH
  cat > "${proj}/.bin/docker" <<'SH'
#!/usr/bin/env bash
echo "parity stub: docker must not be reached" >&2
exit 1
SH
  cp "${proj}/.bin/docker" "${proj}/.bin/kind"
  sed -i 's/docker/kind/' "${proj}/.bin/kind"
  chmod +x "${proj}/.bin/tilt" "${proj}/.bin/kubectl" "${proj}/.bin/docker" "${proj}/.bin/kind"
  # GNU envsubst, pinned: the dev PATH may resolve a non-GNU envsubst whose
  # semantics differ; the Go port implements the GNU contract.
  if [[ -x /usr/bin/envsubst ]]; then
    ln -sf /usr/bin/envsubst "${proj}/.bin/envsubst"
  fi
}

mkproj "${WORK}/proj"

failures=0

# check <allow-diff-regex|-> <argv...> — run in the shared project, diff.
check() {
  local allow="${1}"; shift
  local go_rc=0 bash_rc=0
  (cd "${WORK}/proj" && "${LO_BIN}" "$@" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${WORK}/proj" && LO_IMPL=bash "${LO_BIN}" "$@" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?

  local ok=1
  if (( go_rc != bash_rc )); then
    echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  local stream
  for stream in out err; do
    local diff_out
    if [[ "${allow}" == "-" ]]; then
      diff_out="$(diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" || true)"
    else
      diff_out="$(diff <(grep -vE "${allow}" "${WORK}/bash.${stream}") \
                       <(grep -vE "${allow}" "${WORK}/go.${stream}") || true)"
    fi
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: lo $* — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if (( ok )); then
    echo "ok: lo $*"
  else
    failures=$((failures + 1))
  fi
}

# check_stdin <manifest> <argv...> — like check, with a manifest on stdin
# (the `lo tilt preflight` shape: local(..., stdin = artifacts)).
check_stdin() {
  local stdin="${1}"; shift
  local go_rc=0 bash_rc=0
  (cd "${WORK}/proj" && "${LO_BIN}" "$@" <<<"${stdin}" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${WORK}/proj" && LO_IMPL=bash "${LO_BIN}" "$@" <<<"${stdin}" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  local ok=1 stream diff_out
  if (( go_rc != bash_rc )); then
    echo "FAIL: lo $* (stdin) — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  for stream in out err; do
    diff_out="$(diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" || true)"
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: lo $* (stdin) — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if (( ok )); then echo "ok: lo $* (stdin)"; else failures=$((failures + 1)); fi
}

# ── lo env services ──────────────────────────────────────────────────────────

check - env services                       # no services.yaml → {}
cat > "${WORK}/proj/services.yaml" <<'YAML'
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
cat > "${WORK}/proj/services.local.yaml" <<'YAML'
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
  cp "${WORK}/proj/services.yaml" "${WORK}/envk-${impl}/services.yaml"
  printf '# placeholder\n' > "${WORK}/envk-${impl}/clusters/alpha.dev/artifacts.yaml"
done
envk_rc_go=0 envk_rc_bash=0
(cd "${WORK}/envk-go" && "${LO_BIN}" --domain alpha.dev env kustomization --no-build \
  >"${WORK}/ek-go.out" 2>"${WORK}/ek-go.err") || envk_rc_go=$?
(cd "${WORK}/envk-bash" && LO_IMPL=bash "${LO_BIN}" --domain alpha.dev env kustomization --no-build \
  >"${WORK}/ek-bash.out" 2>"${WORK}/ek-bash.err") || envk_rc_bash=$?
envk_ok=1
(( envk_rc_go == envk_rc_bash )) || { echo "FAIL: env kustomization rc: bash=${envk_rc_bash} go=${envk_rc_go}"; envk_ok=0; }
for stream in out err; do
  d="$(diff "${WORK}/ek-bash.${stream}" "${WORK}/ek-go.${stream}" || true)"
  [[ -z "${d}" ]] || { echo "FAIL: env kustomization std${stream} differs:"; echo "${d}" | head -10 | sed 's/^/  /'; envk_ok=0; }
done
for state in clusters/alpha.dev/artifacts/kustomization.yaml clusters/alpha.dev/artifacts/.cache-queue; do
  if diff -q "${WORK}/envk-bash/${state}" "${WORK}/envk-go/${state}" >/dev/null 2>&1; then
    echo "ok: state ${state}"
  else
    echo "FAIL: state ${state} differs between implementations"
    diff "${WORK}/envk-bash/${state}" "${WORK}/envk-go/${state}" | head -10 | sed 's/^/  /' || true
    envk_ok=0
  fi
done
if (( envk_ok )); then echo "ok: lo env kustomization --no-build (state)"; else failures=$((failures + 1)); fi

# ── lo hooks — validation + no-match paths (cluster-free) ────────────────────

check - hooks recreate                                  # missing --selector
check - hooks apply
check - hooks restart
check - hooks recreate --selector 'a=b;rm -rf /'        # injection guard
check - hooks apply --selector noequalshere             # clause shape
check - hooks recreate --selector 'lok8s.dev/role=ghost'  # no artifact → no match
mkdir -p "${WORK}/proj/clusters/lok8s.dev"
printf 'kind: Job\nmetadata:\n  name: j1\n  labels: {app: other}\n' \
  > "${WORK}/proj/clusters/lok8s.dev/artifacts.yaml"
check - hooks restart --selector app=nomatch            # artifact present, no match
rm -rf "${WORK}/proj/clusters/lok8s.dev"

# ── lo tilt — stub-backed lifecycle + preflight gates ────────────────────────

check - tilt status                                     # stub `tilt doctor`
check - tilt ci                                         # stub exit 7 → rc passthrough
check - tilt ci --timeout 90s
rm -f "${WORK}/proj/.tilt.pid"
check - tilt down                                       # no pid file → silent no-op

# tilt up: both spawn the STUB detached and must write a pid file. (pid VALUES
# differ by construction; existence + stdout are the contract.)
tiltup_ok=1
for impl in go bash; do
  rm -f "${WORK}/proj/.tilt.pid" "${WORK}/proj/.tilt.nohup"
  rc=0
  if [[ "${impl}" == go ]]; then
    (cd "${WORK}/proj" && "${LO_BIN}" tilt up >"${WORK}/up-${impl}.out" 2>"${WORK}/up-${impl}.err") || rc=$?
  else
    (cd "${WORK}/proj" && LO_IMPL=bash "${LO_BIN}" tilt up >"${WORK}/up-${impl}.out" 2>"${WORK}/up-${impl}.err") || rc=$?
  fi
  [[ ${rc} -eq 0 && -f "${WORK}/proj/.tilt.pid" ]] || { echo "FAIL: tilt up (${impl}): rc=${rc} pidfile=$([[ -f ${WORK}/proj/.tilt.pid ]] && echo yes || echo no)"; tiltup_ok=0; }
done
d="$(diff "${WORK}/up-bash.out" "${WORK}/up-go.out" || true)"
[[ -z "${d}" ]] || { echo "FAIL: tilt up stdout differs:"; indent <<<"${d}"; tiltup_ok=0; }
if (( tiltup_ok )); then echo "ok: lo tilt up (stub spawn + pid file)"; else failures=$((failures + 1)); fi
rm -f "${WORK}/proj/.tilt.pid" "${WORK}/proj/.tilt.nohup"

# tilt restart: down (no pid file → no-op) then up — the same spawn + pid
# contract as tilt up.
tiltrs_ok=1
for impl in go bash; do
  rm -f "${WORK}/proj/.tilt.pid" "${WORK}/proj/.tilt.nohup"
  rc=0
  if [[ "${impl}" == go ]]; then
    (cd "${WORK}/proj" && "${LO_BIN}" tilt restart >"${WORK}/rs-${impl}.out" 2>"${WORK}/rs-${impl}.err") || rc=$?
  else
    (cd "${WORK}/proj" && LO_IMPL=bash "${LO_BIN}" tilt restart >"${WORK}/rs-${impl}.out" 2>"${WORK}/rs-${impl}.err") || rc=$?
  fi
  [[ ${rc} -eq 0 && -f "${WORK}/proj/.tilt.pid" ]] || { echo "FAIL: tilt restart (${impl}): rc=${rc} pidfile=$([[ -f ${WORK}/proj/.tilt.pid ]] && echo yes || echo no)"; tiltrs_ok=0; }
done
d="$(diff "${WORK}/rs-bash.out" "${WORK}/rs-go.out" || true)"
[[ -z "${d}" ]] || { echo "FAIL: tilt restart stdout differs:"; indent <<<"${d}"; tiltrs_ok=0; }
if (( tiltrs_ok )); then echo "ok: lo tilt restart (down no-op + stub spawn + pid file)"; else failures=$((failures + 1)); fi
rm -f "${WORK}/proj/.tilt.pid" "${WORK}/proj/.tilt.nohup"

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
rm -f "${WORK}/proj/services.yaml" "${WORK}/proj/services.local.yaml"
check - image cache svc --domain alpha.dev              # no endpoint configured
# The registry NAME is allow-listed: bash derives it from
# KIND_EXPERIMENTAL_DOCKER_NETWORK alone (lok8s-registry-cache here), the
# binary layers spec.network.name first (paritynet-registry-cache) — an
# unaligned divergence, recorded as B6 in go-migration.md. Everything else
# on the line, and the stub-docker silence, is diffed.
check 'registry-cache' image clean --domain alpha.dev   # stub docker: rm + volume rm fail, both silent

# Unresolvable IP: a Lo spec without spec.network on a non-slot domain.
mkdir -p "${WORK}/proj/clusters/bare.dev"
printf 'kind: Lo\nmetadata:\n  name: bare\n' > "${WORK}/proj/clusters/bare.dev/cluster.lok8s.yaml"
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

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
