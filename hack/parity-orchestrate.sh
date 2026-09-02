#!/usr/bin/env bash
# parity-orchestrate.sh — differential test between the Go lo and the argsh
# lo for the orchestration surface: `lo up`, `lo down`, `lo clean`,
# `lo provision`, `lo destroy`, `lo bootstrap`, `lo status`, `lo registry`.
#
# Modeled on parity-test.sh: for every covered invocation, runs BOTH
# implementations (the Go binary, and the same binary with LO_IMPL=bash
# forcing the argsh passthrough) against a synthetic project and diffs
# stdout, stderr, and exit codes. Absolute project paths are normalized to
# PROJ.
#
# NO LIVE STATE. These verbs create/delete kind clusters, stop Tilt sessions
# and remove docker containers/volumes — none of which a parity harness may
# touch (live kind clusters `local`/`kubehz-dev`, a Tilt on 10466 and live
# registry containers exist on dev machines). The synthetic project
# therefore gets its OWN .bin with STUB tilt/kind/docker/kubectl binaries
# (both implementations resolve tools through that directory first), so
# even the destructive invocations below only ever exec the stubs. Consent
# gates are driven with closed stdin (non-interactive → refusal). The real
# round-trip lives in hack/e2e-go-roundtrip.sh (Go binary only, synthetic
# cluster, opt-in).
#
# Usage: hack/parity-orchestrate.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# The harness must run against its synthetic project ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations into that live repo instead of
# ${WORK}. KUBECONFIG/creds/CI-ish toggles would change what a child sees.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG \
  KUBECONFIG KUSTOMIZE_PLUGIN_HOME LOK8S_NONINTERACTIVE LOK8S_FORCE_RECREATE LOK8S_REMOTE \
  CLOUD_DRY_RUN CLOUD_DRY_RUN_PATH HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD \
  KAPPLY_TTY KAPPLY_POLL_INTERVAL SOURCE_DATE_EPOCH CI TILT_PORT \
  LOK8S_REGISTRY_IP_CACHE LOK8S_REGISTRY_JSON KIND_EXPERIMENTAL_DOCKER_NETWORK \
  KIND_CONFIG KIND_NODE_VERSION LOK8S_BOOTSTRAP_ONLY LOK8S_BOOTSTRAP_PARALLEL \
  KUBEHZ_TOKEN LOK8S_DOMAIN_EXPLICIT

# Pin the C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists in byte order (= C collation).
export LC_ALL=C
# Registry state (durable configs + locks) must land under ${WORK}, never
# in the operator's real state dir.
export LO_REGISTRY_STATE_DIR="${WORK}/registry-state"
export XDG_STATE_HOME="${WORK}/xdg-state" XDG_CACHE_HOME="${WORK}/xdg-cache"
# Off-tty progress output must be deterministic in BOTH implementations:
# bash's kapply::_tty tests `[[ -w /dev/tty ]]` (true by permission bits even
# with no controlling terminal, after which the progress block sinks to
# /dev/null), the Go port actually opens /dev/tty — so in a tty-less sandbox
# they disagree unless the passthrough is forced. Same pin as the Go driver
# tests and the bats. The consent gates refuse under it exactly as they do
# on a closed stdin.
export LOK8S_NONINTERACTIVE=1

PROJ="${WORK}/proj"

# ── synthetic project ────────────────────────────────────────────────────────
mkdir -p "${PROJ}/clusters" "${PROJ}/.bin"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
printf 'Tiltfile\n' > "${PROJ}/Tiltfile"

# Domains, one per routing axis:
#   alpha.dev    Lo, non-shared registries, tls off (no mkcert), .registries.json
#   shared.dev   Lo with a SHARED .registries.json (the ℹ branch of down)
#   tls.dev      Lo with tls: false spelled explicitly (the header's yq quirk)
#   beta.cloud   KubeOne (real-infrastructure gate; no provider)
#   gamma.app    deploy-only → beta.cloud
#   delta.app    deploy-only, no clusterRef
#   nokind.dev   cluster spec WITHOUT .kind (must never reach driver-destroy)
#   evil.dev     traversal-shaped .kind
mkdir -p "${PROJ}/clusters"/{alpha.dev,shared.dev,tls.dev,beta.cloud,gamma.app,delta.app,nokind.dev,evil.dev}
cat > "${PROJ}/clusters/alpha.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: alpha
spec:
  runtime: kind
  kubernetes:
    version: v1.31.12@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2
  network:
    name: paritynet
    cidr: 10.99.7.0/24
  registries:
    tls: false
YAML
cat > "${PROJ}/clusters/alpha.dev/.registries.json" <<'JSON'
{"shared": false, "project_network": "paritynet", "registries": [{"name": "build"}, {"name": "cache"}]}
JSON
cat > "${PROJ}/clusters/shared.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: sharedc
spec:
  runtime: kind
  kubernetes:
    version: "1.31"
  network:
    name: sharednet
    cidr: 10.99.8.0/24
  registries:
    shared:
      enabled: true
YAML
cat > "${PROJ}/clusters/shared.dev/.registries.json" <<'JSON'
{"shared": true, "project_network": "sharednet", "registries": [{"name": "build"}, {"name": "cache"}, {"name": "io-docker"}]}
JSON
cat > "${PROJ}/clusters/tls.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: tlsc
spec:
  runtime: kind
  network:
    name: tlsnet
    cidr: 10.99.9.0/24
  registries:
    tls: false
    shared:
      enabled: false
YAML
printf 'kind: KubeOne\nmetadata:\n  name: beta\nspec:\n  kubernetes:\n    version: "1.31.0"\n  bootstrap:\n    - name: cilium\n    - name: metallb\n' \
  > "${PROJ}/clusters/beta.cloud/cluster.lok8s.yaml"
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${PROJ}/clusters/gamma.app/deploy.lok8s.yaml"
printf 'kind: Deploy\nspec: {}\n' > "${PROJ}/clusters/delta.app/deploy.lok8s.yaml"
printf 'metadata:\n  name: prod\nspec:\n  kubernetes:\n    version: "1.31.0"\n' > "${PROJ}/clusters/nokind.dev/cluster.lok8s.yaml"
printf 'kind: ../../evil\nmetadata:\n  name: prod\n' > "${PROJ}/clusters/evil.dev/cluster.lok8s.yaml"

# Real toolchain by symlink…
for entry in "${ROOT}"/.bin/*; do
  ln -s "${entry}" "${PROJ}/.bin/$(basename "${entry}")"
done
# …with every live-state tool REPLACED by a stub (rm the symlink first).
rm -f "${PROJ}/.bin/tilt" "${PROJ}/.bin/kubectl" "${PROJ}/.bin/docker" "${PROJ}/.bin/kind" "${PROJ}/.bin/kubeone"
cat > "${PROJ}/.bin/tilt" <<'SH'
#!/usr/bin/env bash
# Parity stub. doctor: a kind env; get session: no apiserver; ci: fail 7
# (the rc-passthrough probe); everything else: succeed silently.
case "${1:-}" in
  doctor) echo "Env: kind"; exit 0 ;;
  get)    exit 1 ;;
  ci)     echo "stub tilt ci $*"; exit 7 ;;
esac
exit 0
SH
cat > "${PROJ}/.bin/kubectl" <<'SH'
#!/usr/bin/env bash
# Parity stub: every kubectl fails silently (no live cluster may be reached).
exit 1
SH
cat > "${PROJ}/.bin/kind" <<'SH'
#!/usr/bin/env bash
# Parity stub: ONE synthetic cluster "alpha" exists; delete is a no-op.
case "${1:-} ${2:-}" in
  "get clusters")   echo "alpha"; exit 0 ;;
  "delete cluster") echo "stub: kind $*"; exit 0 ;;
  "create cluster") echo "stub: kind $*"; exit 0 ;;
esac
exit 0
SH
cat > "${PROJ}/.bin/docker" <<'SH'
#!/usr/bin/env bash
# Parity stub: a docker that owns nothing. rm/prune succeed; listings are
# empty except the cluster's two named volumes; inspects fail (absent).
case "${1:-} ${2:-}" in
  "volume ls")       echo "alpha-data"; echo "alpha-cache"; exit 0 ;;
  "system prune")    echo "stub: docker $*"; exit 0 ;;
  "network inspect") exit 1 ;;
  "inspect "*)       exit 1 ;;
  "ps "*)            exit 0 ;;
esac
exit 0
SH
cat > "${PROJ}/.bin/kubeone" <<'SH'
#!/usr/bin/env bash
echo "parity stub: kubeone must not be reached" >&2
exit 1
SH
chmod +x "${PROJ}/.bin/tilt" "${PROJ}/.bin/kubectl" "${PROJ}/.bin/docker" "${PROJ}/.bin/kind" "${PROJ}/.bin/kubeone"

failures=0

# reset_fixtures — the driver REGENERATES clusters/<d>/.registries.json on
# every read of the network config (registry/up/clean/down paths), so a
# stateful check run by the first implementation would change what the
# second one sees. Restore the fixtures before each such check.
reset_fixtures() {
  cat > "${PROJ}/clusters/alpha.dev/.registries.json" <<'JSON'
{"shared": false, "project_network": "paritynet", "registries": [{"name": "build"}, {"name": "cache"}]}
JSON
  cat > "${PROJ}/clusters/shared.dev/.registries.json" <<'JSON'
{"shared": true, "project_network": "sharednet", "registries": [{"name": "build"}, {"name": "cache"}, {"name": "io-docker"}]}
JSON
  rm -f "${PROJ}/clusters/tls.dev/.registries.json"
  rm -rf "${PROJ}/clusters"/*/.containerd "${LO_REGISTRY_STATE_DIR}"
}

# check <allow-diff-regex|-> <argv...> — run in the project, closed stdin,
# diff rc/stdout/stderr with the project path normalized.
# pre_each runs before EACH implementation's run (stateful checks restore
# the fixtures there — the first implementation's side effects must not
# leak into what the second one sees).
pre_each() { :; }

check() {
  local allow="${1}"; shift
  local go_rc=0 bash_rc=0
  pre_each
  (cd "${PROJ}" && "${LO_BIN}" "$@" </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  pre_each
  (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" </dev/null >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  sed -i "s|${PROJ}|PROJ|g" "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"

  local ok=1
  if (( go_rc != bash_rc )); then
    echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  local stream diff_out
  for stream in out err; do
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

# check_parse <argv...> — an argsh PARSE error (stray positional, unknown
# flag): message identical, but argsh exits 2 where cobra exits 1 — the
# documented divergence every ported command shares (cmd_deploy.go). Only
# the rc pair (bash 2, go 1) is tolerated; the streams must still match.
check_parse() {
  local go_rc=0 bash_rc=0
  (cd "${PROJ}" && "${LO_BIN}" "$@" </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" </dev/null >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  local ok=1
  if ! { (( go_rc == bash_rc )) || (( bash_rc == 2 && go_rc == 1 )); }; then
    echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
    ok=0
  fi
  local stream diff_out
  for stream in out err; do
    diff_out="$(diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" || true)"
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: lo $* — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if (( ok )); then
    echo "ok: lo $* (parse error, rc 2→1)"
  else
    failures=$((failures + 1))
  fi
}

# stateful [allow] <argv...> — check with the fixtures restored before each
# implementation's run.
stateful() {
  local allow="${1}"; shift
  pre_each() { reset_fixtures; }
  check "${allow}" "$@"
  pre_each() { :; }
}

# ── lo status ────────────────────────────────────────────────────────────────
check - status --domain alpha.dev                  # lo: Running (stub lists alpha), no kubeconfig → no nodes, no targets, tilt not running
check - status --domain shared.dev                 # lo: NotFound
check - status --domain beta.cloud                 # kubeone driver status, no Tilt section
check - status --domain gamma.app                  # deploy → follows clusterRef
check - status --domain delta.app                  # deploy without clusterRef (error merged into stdout)
# NOT checked: `status --domain nope.dev` — see the ../evil note below (the
# same unbound-variable crash: any domain without a spec).
# NOT checked: `status --domain ../evil` — the bash dispatch_status ignores
# resolve_spec's return and dies on an unbound LOK8S_SPEC_KIND (`set -u`);
# the Go port prints the raw invalid-domain error and carries on with the
# cluster-free sections. A bash defect, not a parity target.
check_parse status extra
mkdir -p "${PROJ}/clusters/alpha.dev/targets/zitadel" "${PROJ}/clusters/alpha.dev/targets/api"
: > "${PROJ}/clusters/alpha.dev/artifacts.yaml"
echo "2147480000" > "${PROJ}/.tilt.pid"              # a pid that is virtually certain not to exist
check - status --domain alpha.dev                  # targets listed, built, stale pid
: > "${PROJ}/.tilt.pid"
check - status --domain alpha.dev                  # empty pidfile
rm -f "${PROJ}/.tilt.pid"

# ── lo down ──────────────────────────────────────────────────────────────────
check - down --domain nokind.dev                   # spec without .kind → refusal, NEVER driver-destroy
check - down --domain evil.dev                     # traversal-shaped kind → refusal
check - down --domain gamma.app                    # deploy-only → local path, cluster "local" (not listed by the stub)
check - down --domain nope.dev                     # no spec → local path
check - down --domain alpha.dev                    # lo: tilt stop, kind delete, 2 project registries shut down
check - down --domain shared.dev                   # lo, shared registries left up (ℹ lines)
check - down --domain tls.dev                      # lo, no .registries.json
check - down --domain beta.cloud                   # kubeone: gate refuses non-interactively → silent rc 1
check - down --domain beta.cloud -f                # forced: driver destroy fails (no work dir) → orphan line
check - down --domain alpha.dev --cluster other    # spec metadata.name outranks --cluster
check - down --domain gamma.app --cluster alpha    # deploy-only: --cluster names the kind cluster
check - down extra                                 # positionals ignored (argsh)
check - down --bogus                               # unknown flags ignored too (main::down has no :args)

# ── lo clean ─────────────────────────────────────────────────────────────────
stateful - clean --domain alpha.dev                # volumes rm + registry clean (lo gate passes)
stateful - clean --domain alpha.dev --all          # docker system prune
# --domain BEFORE -a: argsh's main() stops scanning global flags at the
# first flag it does not own, so `clean -a --domain x` leaves the ambient
# DOMAIN_NAME/cluster at the default in bash (a quirk cobra does not have).
stateful - clean --domain shared.dev -a
check - clean --domain gamma.app                   # deploy domain: warn, registries skipped
check - clean --domain nope.dev                    # no spec: warn
check - clean --domain beta.cloud                  # down declines → clean stops (rc 1)
check - clean --domain nokind.dev                  # down refuses → clean stops
check_parse clean extra

# ── lo provision ─────────────────────────────────────────────────────────────
check - provision --domain gamma.app               # deploy refusal
check - provision --domain delta.app
check - provision --domain nope.dev
check - provision --domain '../evil'
check - provision --domain nokind.dev              # no .kind
check - provision --domain evil.dev                # malformed kind
check - provision --domain beta.cloud              # gate: refuses non-interactively (rc 3)
check - provision --domain beta.cloud --bootstrap  # gate: bootstrap wording
check - provision -b --domain beta.cloud -f        # forced --bootstrap: no kubeconfig yet
check - provision --domain beta.cloud -f           # forced: kubeone needs spec.provider
check - p --domain gamma.app                       # alias
check_parse provision extra
check_parse provision --bogus

# ── lo destroy ───────────────────────────────────────────────────────────────
check - destroy --domain gamma.app
check - destroy --domain nope.dev
check - destroy --domain nokind.dev
check - destroy --domain beta.cloud                # gate: destroy demands yes; closed stdin → rc 3
check - destroy --domain beta.cloud -f             # forced: driver destroy fails (no work dir)
stateful - destroy --domain alpha.dev              # lo: stub kind delete + registry cleanup
check_parse destroy extra

# ── lo bootstrap ─────────────────────────────────────────────────────────────
check - bootstrap --domain nope.dev
check - bootstrap --domain gamma.app               # no cluster spec
check - bootstrap --domain '../evil'
check - bootstrap --domain nokind.dev
check - bootstrap --domain evil.dev
check_parse bootstrap extra

# ── lo registry ──────────────────────────────────────────────────────────────
# NOT checked: bare `lo registry` — argsh usage vs cobra help (the global,
# accepted help-format divergence of every ported group command).
check_parse registry bogus
check - registry status --domain beta.cloud        # driver gate: KubeOne domain refused
check - registry status --domain gamma.app         # driver gate: deploy domain refused
check - registry status --domain nope.dev          # driver gate: no readable spec
check - registry down --domain beta.cloud
check - registry clean --domain gamma.app
check - registry up --domain nope.dev
check - r s --domain beta.cloud                    # aliases
stateful - registry status --domain alpha.dev      # stub docker: every registry "not running"
stateful - registry status -S --domain shared.dev  # shared mirrors listed
stateful - registry status --domain shared.dev     # shared mirrors hidden without -S
stateful - registry down --domain alpha.dev
stateful - registry clean --domain alpha.dev
stateful - registry clean --shared --domain shared.dev
check - registry status --bogus                    # unknown flags pass through unparsed (no :args in registry::status)

# ── lo up — the run header ───────────────────────────────────────────────────
check - up --domain gamma.app                      # header "deploy → beta.cloud", then the deploy refusal
check - up --domain delta.app                      # header "deploy → ?"
check - up --domain nope.dev                       # header with empty meta, then no-spec error
check - up --domain nokind.dev                     # header: unreadable kind → empty meta
check - up --domain beta.cloud                     # header "kubeone · 1.31.0", gate refusal (rc 3)
check - up --domain beta.cloud --ci                # same — the gate runs before tilt
check_parse up extra
check_parse up --bogus
# Lo headers: "lo · kind · <version>" + the registries summary line. The
# dispatch then runs the Lo driver against the stubbed docker; the header
# is the parity target, the driver's own lines are diffed too (both impls
# run the same stub answers).
# The kind config is fed via <(echo …) in bash and a temp file in Go — that
# one argv line is allowed to differ.
KIND_CFG='kind create cluster --name .* --config '
stateful "${KIND_CFG}" up --domain alpha.dev
stateful "${KIND_CFG}" up --domain shared.dev
stateful "${KIND_CFG}" up --domain tls.dev

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
