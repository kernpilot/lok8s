#!/usr/bin/env bash
# parity-orchestrate.sh — differential test between the Go lo and the argsh
# lo for the orchestration surface: `lo up`, `lo down`, `lo clean`,
# `lo provision`, `lo destroy`, `lo bootstrap`, `lo status`, `lo registry`.
#
# Modeled on parity-test.sh: for every covered invocation, runs BOTH
# implementations (the Go binary, and the same binary routed to the frozen tree
# by the project file) against a synthetic project and diffs
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
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
# KUBECONFIG/creds/CI-ish toggles would change what a child sees.
unset KUBECONFIG KUSTOMIZE_PLUGIN_HOME LOK8S_NONINTERACTIVE LOK8S_FORCE_RECREATE LOK8S_REMOTE \
  CLOUD_DRY_RUN CLOUD_DRY_RUN_PATH HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD \
  KAPPLY_TTY KAPPLY_POLL_INTERVAL SOURCE_DATE_EPOCH CI TILT_PORT \
  LOK8S_REGISTRY_IP_CACHE LOK8S_REGISTRY_JSON KIND_EXPERIMENTAL_DOCKER_NETWORK \
  KIND_CONFIG KIND_NODE_VERSION LOK8S_BOOTSTRAP_ONLY LOK8S_BOOTSTRAP_PARALLEL \
  KUBEHZ_TOKEN LOK8S_DOMAIN_EXPLICIT

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
# Like with like on the registry TLS mint: the frozen tree needs the Secret
# plugin BINARY under .kustomize (absent in the synthetic project, so its
# `lo up` fails on "the Secret plugin is not built"), while the Go binary
# serves the generator in-process and would fail later, on the missing
# store. LO_RENDER=exec pins the Go side to the same subprocess pipeline the
# bash runs (docs/reference/go-migration.md, D19); the in-process mint has
# its own gate in internal/driver/lo (TestRegistriesTLSCertMintsInProcess).
export LO_RENDER=exec

# ── synthetic project ────────────────────────────────────────────────────────
parity::new_project "${PROJ}"
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

# The project's own .bin, with every live-state tool REPLACED by a stub.
parity::own_bin "${PROJ}"
parity::stub "${PROJ}" tilt <<'SH'
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
parity::stub_kubectl_fail "${PROJ}"
parity::stub "${PROJ}" kind <<'SH'
#!/usr/bin/env bash
# Parity stub: ONE synthetic cluster "alpha" exists; delete is a no-op.
case "${1:-} ${2:-}" in
  "get clusters")   echo "alpha"; exit 0 ;;
  "delete cluster") echo "stub: kind $*"; exit 0 ;;
  "create cluster") echo "stub: kind $*"; exit 0 ;;
esac
exit 0
SH
parity::stub "${PROJ}" docker <<'SH'
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
parity::stub_refuse "${PROJ}" kubeone

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

# check_parse <argv...> — an argsh PARSE error (stray positional, unknown
# flag): message identical, but argsh exits 2 where cobra exits 1 — the
# documented divergence every ported command shares (cmd_deploy.go). Only
# the rc pair (bash 2, go 1) is tolerated; the streams must still match.
check_parse() {
  parity::run_pair "$@"
  PARITY_PARSE_RC=1 parity::finish "lo $*" - "lo $* (parse error, rc 2→1)"
}

# stateful [allow] <argv...>: check with the fixtures restored before EACH
# implementation's run (the first implementation's side effects must not
# leak into what the second one sees).
stateful() { PARITY_PRE_EACH=reset_fixtures check "$@"; }

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

# ── lo up with registry TLS ──────────────────────────────────────────────────
# Both implementations mint the registry cert through the Secret plugin into
# the docker volume <network>-registry-tls (the plugin gets a scratch store
# under the domain dir, the volume is populated and read through a throwaway
# container, every registry mounts the volume). The streams are diffed like
# every other case, AND the docker argv and the plugin's environment are
# diffed byte for byte across the two sides (the scratch dir's random suffix
# normalized). Both sides exec the plugin (LO_RENDER=exec above): a stub that
# logs the store it was handed and answers a fake pair. The docker stub logs
# every argv; `docker cp CTR:PATH -` answers an empty stream (nothing in the
# "volume"), so both sides mint on every run. PATH_SECRETS is unset on both
# sides (parity::init), CAROOT points at an empty dir so the trust nudge sees
# the same absent CA on both.
mkdir -p "${PROJ}/clusters/mint.dev"
cat > "${PROJ}/clusters/mint.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: mintc
spec:
  runtime: kind
  network:
    name: mintnet
    cidr: 10.99.10.0/24
  registries:
    tls: true
    shared:
      enabled: false
YAML
mkdir -p "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret"
cat > "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret" <<'SH'
#!/usr/bin/env bash
# Parity stub Secret plugin: log the store it was handed, answer a fake pair
# (base64 FAKECRT / FAKEKEY).
cat >/dev/null
echo "PATH_SECRETS=${PATH_SECRETS:-<unset>}" >> "${PARITY_PLUGIN_LOG}"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: RkFLRUNSVA==\n  tls.key: RkFLRUtFWQ==\n'
SH
chmod +x "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
parity::stub "${PROJ}" docker <<'SH'
#!/usr/bin/env bash
# Parity stub (TLS phase): the plain stub plus an argv log. The cert volume
# is always absent (so the import path runs and the volume is created), the
# read-out finds nothing (so both sides mint on every run).
echo "docker $*" >> "${PARITY_DOCKER_LOG}"
case "${1:-} ${2:-}" in
  "volume ls")       echo "alpha-data"; echo "alpha-cache"; exit 0 ;;
  "volume inspect")  exit 1 ;;
  "system prune")    echo "stub: docker $*"; exit 0 ;;
  "network inspect") exit 1 ;;
  "inspect "*)       exit 1 ;;
  "ps "*)            exit 0 ;;
esac
exit 0
SH
export PARITY_DOCKER_LOG="${WORK}/docker.log" PARITY_PLUGIN_LOG="${WORK}/plugin.log"
export CAROOT="${WORK}/caroot"
tls_pre() {
  reset_fixtures
  rm -rf "${PROJ}/.secrets" "${PROJ}/clusters/mint.dev/.registries.json"
}
tls_post() {  # <impl>: keep the logs per side (normalized), record the flat store and the scratch state
  local f
  for f in docker plugin; do
    if [[ -f "${WORK}/${f}.log" ]]; then
      sed -E "s|${PROJ}|PROJ|g; s|\.registry-tls-tmp\.[^/ ]+|.registry-tls-tmp.X|g" "${WORK}/${f}.log" > "${WORK}/${f}.${1}.log"
      rm -f "${WORK}/${f}.log"
    else
      : > "${WORK}/${f}.${1}.log"
    fi
  done
  { [[ -e "${PROJ}/.secrets" ]] && echo present || echo absent; } > "${WORK}/flat.${1}"
  { ls -d "${PROJ}/clusters/mint.dev"/.registry-tls-tmp.* 2>/dev/null || true; } > "${WORK}/scratch.${1}"
}
PARITY_PRE_EACH=tls_pre PARITY_POST_EACH=tls_post check "${KIND_CFG}" up --domain mint.dev
parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo up --domain mint.dev" || failures=$((failures + 1))
parity::state_same "${WORK}/plugin.bash.log" "${WORK}/plugin.go.log" "Secret plugin env of lo up --domain mint.dev" || failures=$((failures + 1))

# Anti-vacuity: the identical argv must be the VOLUME model on both sides.
tls_pin() {  # <label> <file> <regex>
  if grep -qE -- "${3}" "${2}"; then echo "ok: ${1}"; else
    echo "FAIL: ${1} — no line matches ${3} in ${2}:"; sed 's/^/  /' "${2}" | head -20
    failures=$((failures + 1))
  fi
}
tls_absent() {  # <label> <file> <regex>
  if grep -qE -- "${3}" "${2}"; then
    echo "FAIL: ${1} — a line matches ${3} in ${2}:"; grep -E -- "${3}" "${2}" | head -5 | sed 's/^/  /'
    failures=$((failures + 1))
  else echo "ok: ${1}"; fi
}
for side in bash go; do
  tls_pin    "${side}: plugin store = a scratch under the domain dir"  "${WORK}/plugin.${side}.log" "^PATH_SECRETS=PROJ/clusters/mint\.dev/\.registry-tls-tmp\.X$"
  tls_pin    "${side}: the volume is created"                          "${WORK}/docker.${side}.log" "^docker volume create mintnet-registry-tls$"
  tls_pin    "${side}: volume populated through the io container"      "${WORK}/docker.${side}.log" "^docker container create --name mintnet-registry-tls-io --volume mintnet-registry-tls:/etc/registry/certs registry:2\.8\.3$"
  tls_pin    "${side}: tls.key copied into the volume"                 "${WORK}/docker.${side}.log" "^docker cp PROJ/clusters/mint\.dev/\.registry-tls-tmp\.X/tls\.key mintnet-registry-tls-io:/etc/registry/certs/tls\.key$"
  tls_pin    "${side}: registries mount the volume"                    "${WORK}/docker.${side}.log" "^docker run .* --volume mintnet-registry-tls:/etc/registry/certs:ro "
  tls_absent "${side}: no flat store in any docker argv"               "${WORK}/docker.${side}.log" "\.secrets"
  if [[ "$(cat "${WORK}/flat.${side}")" == absent ]]; then echo "ok: ${side}: no <project>/.secrets created"; else fail "${side}: <project>/.secrets was created"; fi
  if [[ ! -s "${WORK}/scratch.${side}" ]]; then echo "ok: ${side}: no scratch dir left under the domain dir"; else fail "${side}: scratch left: $(cat "${WORK}/scratch.${side}")"; fi
done

# The import: a legacy .secrets/tls/registries pair (one file a symlink, so
# `docker cp -L` must follow it) is copied into a fresh volume with one
# [warn] (in the diffed stderr); an incomplete pair is warned about and a
# fresh cert minted instead. The argv is diffed byte for byte, as above.
tls_pre_legacy() {
  tls_pre
  mkdir -p "${PROJ}/.secrets/tls/registries" "${PROJ}/.secrets/real"
  printf 'LEGACYCRT' > "${PROJ}/.secrets/real/tls.crt"
  ln -s ../../real/tls.crt "${PROJ}/.secrets/tls/registries/tls.crt"
  printf 'LEGACYKEY' > "${PROJ}/.secrets/tls/registries/tls.key"
}
tls_pre_half() {
  tls_pre
  mkdir -p "${PROJ}/.secrets/tls/registries"
  printf 'LEGACYCRT' > "${PROJ}/.secrets/tls/registries/tls.crt"
}
PARITY_PRE_EACH=tls_pre_legacy PARITY_POST_EACH=tls_post check "${KIND_CFG}" up --domain mint.dev
parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo up --domain mint.dev (legacy import)" || failures=$((failures + 1))
for side in bash go; do
  tls_pin "${side}: the legacy pair is copied with -L"  "${WORK}/docker.${side}.log" "^docker cp -L PROJ/\.secrets/tls/registries/tls\.key mintnet-registry-tls-io:/etc/registry/certs/tls\.key$"
done
rm -rf "${PROJ}/.secrets"
PARITY_PRE_EACH=tls_pre_half PARITY_POST_EACH=tls_post check "${KIND_CFG}" up --domain mint.dev
parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo up --domain mint.dev (incomplete legacy pair)" || failures=$((failures + 1))
for side in bash go; do
  tls_absent "${side}: nothing copied from an incomplete legacy pair" "${WORK}/docker.${side}.log" "^docker cp -L "
done
rm -rf "${PROJ}/.secrets"

# lo registry tls status / renew: the volume is absent (the stub), the
# containers are absent. Every case runs under the TLS hooks so each side's
# docker argv lands in its own log.
tls_check() { PARITY_PRE_EACH=tls_pre PARITY_POST_EACH=tls_post check "$@"; }
tls_check - registry tls status --domain mint.dev
parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo registry tls status" || failures=$((failures + 1))
tls_check - registry tls status --domain alpha.dev        # tls: false
tls_check - registry tls status --domain beta.cloud       # driver gate
tls_check - registry t s --domain mint.dev                # aliases
tls_check - registry tls renew --domain mint.dev
parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo registry tls renew" || failures=$((failures + 1))
tls_check - registry tls renew --domain alpha.dev         # tls: false, refuses
check_parse registry tls bogus
unset CAROOT PARITY_DOCKER_LOG PARITY_PLUGIN_LOG

# ── lo bootstrap — the render's plugin home ──────────────────────────────────
# A shell that exports only PATH (KUSTOMIZE_PLUGIN_HOME is unset above): the
# kustomize child of the addon render must still get <project>/.kustomize on
# BOTH sides (bash: the shim's default; Go: config.KustomizePluginHome).
# v0.4.0 handed the bootstrap render no home on the Go side and every addon
# failed on `unable to find plugin root`. The stub kustomize logs the home it
# was given and fails the build, so the case stops at the render (no kubectl,
# no retry loop); the log is diffed across the sides and pinned to the
# project value.
mkdir -p "${PROJ}/clusters/boot.dev" "${PROJ}/.kubeconfig"
cat > "${PROJ}/clusters/boot.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: bootc
spec:
  runtime: kind
  network:
    name: bootnet
    cidr: 10.99.11.0/24
  registries:
    tls: false
  bootstrap:
    - cilium
YAML
: > "${PROJ}/.kubeconfig/bootc.yaml"
parity::stub "${PROJ}" kustomize <<'SH'
#!/usr/bin/env bash
# Parity stub kustomize: log the plugin home the render handed over, then
# fail the build (the case stops at the render).
echo "KUSTOMIZE_PLUGIN_HOME=${KUSTOMIZE_PLUGIN_HOME:-<unset>}" >> "${PARITY_KUSTOMIZE_LOG}"
echo "Error: parity stub: no render" >&2
exit 1
SH
export PARITY_KUSTOMIZE_LOG="${WORK}/kustomize.log"
boot_pre() { reset_fixtures; rm -f "${PROJ}/clusters/boot.dev/.registries.json"; }
boot_post() {  # <impl>: keep the log per side (normalized)
  if [[ -f "${WORK}/kustomize.log" ]]; then
    sed -E "s|${PROJ}|PROJ|g" "${WORK}/kustomize.log" > "${WORK}/kustomize.${1}.log"
    rm -f "${WORK}/kustomize.log"
  else
    : > "${WORK}/kustomize.${1}.log"
  fi
}
PARITY_PRE_EACH=boot_pre PARITY_POST_EACH=boot_post check - bootstrap --domain boot.dev
parity::state_same "${WORK}/kustomize.bash.log" "${WORK}/kustomize.go.log" "plugin home of the bootstrap render" || failures=$((failures + 1))
for side in bash go; do
  tls_pin "${side}: the render's kustomize child has the project plugin home" "${WORK}/kustomize.${side}.log" "^KUSTOMIZE_PLUGIN_HOME=PROJ/\.kustomize$"
done
unset PARITY_KUSTOMIZE_LOG

report
