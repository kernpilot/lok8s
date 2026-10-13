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
    version: "v1.31.4"
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
# Parity stub: ONE synthetic cluster "alpha" exists; delete is a no-op. With
# PARITY_KIND_LOG set, create appends the --config file it got to that log.
case "${1:-} ${2:-}" in
  "get clusters")   echo "alpha"; exit 0 ;;
  "delete cluster") echo "stub: kind $*"; exit 0 ;;
  "create cluster")
    echo "stub: kind $*"
    if [[ -n "${PARITY_KIND_LOG:-}" ]]; then
      prev=""
      for arg in "$@"; do
        if [[ "${prev}" == "--config" ]]; then cat "${arg}" >> "${PARITY_KIND_LOG}"; fi
        prev="${arg}"
      done
    fi
    exit 0 ;;
esac
exit 0
SH
parity::stub "${PROJ}" docker <<'SH'
#!/usr/bin/env bash
# Parity stub: a docker that owns nothing. rm/prune succeed; listings are
# empty except the cluster's two named volumes; inspects fail (absent). With
# PARITY_DOCKER_LOG set, every argv is appended to that log.
if [[ -n "${PARITY_DOCKER_LOG:-}" ]]; then echo "docker $*" >> "${PARITY_DOCKER_LOG}"; fi
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

# ── lo provision — the kind node image (spec.kubernetes.version) ─────────────
# An unset version (tls.dev has none, nullver.dev has the literal string
# "null") names no image in the kind config: kind then uses the default node
# image of its own release. A valid version (goodver.dev, with a digest) names
# its image, and so does a version behind a merge key (mergever.dev: the Go
# reader once read it as unset, and kind then made a cluster of its default
# Kubernetes version). A value that is not an image tag (badver.dev) stops the
# provision before the first docker call. The kind stub appends the config it
# gets to PARITY_KIND_LOG, and the docker stub logs every argv: both are
# diffed across the sides and pinned.
mkdir -p "${PROJ}/clusters/nullver.dev" "${PROJ}/clusters/goodver.dev" \
  "${PROJ}/clusters/badver.dev" "${PROJ}/clusters/mergever.dev"
cat > "${PROJ}/clusters/goodver.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: goodver
spec:
  runtime: kind
  kubernetes:
    version: v1.35.0@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2
  network:
    name: goodnet
    cidr: 10.99.14.0/24
  registries:
    tls: false
YAML
cat > "${PROJ}/clusters/nullver.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: nullver
spec:
  runtime: kind
  kubernetes:
    version: "null"
  network:
    name: nullnet
    cidr: 10.99.12.0/24
  registries:
    tls: false
YAML
cat > "${PROJ}/clusters/badver.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: badver
spec:
  runtime: kind
  kubernetes:
    version: kindest/node:v1.31.4
  network:
    name: badnet
    cidr: 10.99.13.0/24
  registries:
    tls: false
YAML
cat > "${PROJ}/clusters/mergever.dev/cluster.lok8s.yaml" <<'YAML'
pinned: &pinned
  version: v1.31.4
kind: Lo
metadata:
  name: mergever
spec:
  runtime: kind
  kubernetes:
    <<: *pinned
  network:
    name: mergenet
    cidr: 10.99.15.0/24
  registries:
    tls: false
YAML
export PARITY_KIND_LOG="${WORK}/kind.log" PARITY_DOCKER_LOG="${WORK}/docker.log"
node_pre() { reset_fixtures; rm -f "${PARITY_KIND_LOG}" "${PARITY_DOCKER_LOG}" "${PROJ}/clusters"/{nullver,goodver,badver,mergever,layoutlead,layouttrail,layoutempty,layouttwo}.dev/.registries.json; }
node_post() {  # <impl>: keep the kind config and the docker argv per side (normalized)
  local f
  for f in kind docker; do
    if [[ -f "${WORK}/${f}.log" ]]; then
      sed -E "s|${PROJ}|PROJ|g" "${WORK}/${f}.log" > "${WORK}/${f}.${1}.log"
      rm -f "${WORK}/${f}.log"
    else
      : > "${WORK}/${f}.${1}.log"
    fi
  done
}
node_check() {  # <domain>: lo provision, the streams and the kind config diffed
  PARITY_PRE_EACH=node_pre PARITY_POST_EACH=node_post check "${KIND_CFG}" provision --domain "${1}"
  parity::state_same "${WORK}/kind.bash.log" "${WORK}/kind.go.log" "kind config of lo provision --domain ${1}" || failures=$((failures + 1))
  parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo provision --domain ${1}" || failures=$((failures + 1))
}
# node_pin <label> <file> <want-count> <regex>: the count of matching lines.
# want-count "+" means one line or more.
node_pin() {
  local n
  n=$(grep -cE -- "${4}" "${2}" || true)
  if [[ "${n}" == "${3}" || ( "${3}" == "+" && "${n}" -gt 0 ) ]]; then echo "ok: ${1}"; else
    echo "FAIL: ${1} — ${n} lines match ${4} in ${2}, want ${3}:"; sed 's/^/  /' "${2}" | head -20
    failures=$((failures + 1))
  fi
}
for d in tls.dev nullver.dev; do
  node_check "${d}"
  for side in bash go; do
    node_pin "${side}: ${d} (unset version) reached kind create" "${WORK}/kind.${side}.log" 1 '^kind: Cluster$'
    node_pin "${side}: ${d} (unset version) names no node image"  "${WORK}/kind.${side}.log" 0 'image:'
  done
done
node_check goodver.dev
for side in bash go; do
  node_pin "${side}: goodver.dev names its node image" "${WORK}/kind.${side}.log" 1 '^    image: "kindest/node:v1\.35\.0@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2"$'
  # Positive control for the "no docker command" pin of badver.dev below:
  # the docker stub logs on a provision that runs.
  node_pin "${side}: goodver.dev ran docker commands" "${WORK}/docker.${side}.log" + '^docker '
done
node_check mergever.dev
for side in bash go; do
  node_pin "${side}: mergever.dev names the merged node image" "${WORK}/kind.${side}.log" 1 '^    image: "kindest/node:v1\.31\.4"$'
done
node_check badver.dev
for side in bash go; do
  node_pin "${side}: badver.dev is refused with the reason" "${WORK}/${side}.err" 1 "spec\.kubernetes\.version 'kindest/node:v1\.31\.4' is not a valid image tag\. "
  node_pin "${side}: badver.dev ran no docker command" "${WORK}/docker.${side}.log" 0 '.'
  node_pin "${side}: badver.dev ran no kind create"    "${WORK}/kind.${side}.log" 0 '.'
done

# ── lo provision — the document layout of the spec file ──────────────────────
# Only one YAML document reads the same in the Go readers (first document) and
# the bash readers (yq on every document). layoutlead.dev (one document after a
# `---`) provisions; a trailing `---`, two empty documents before the spec and
# a second document stop the dispatch before the driver is chosen, with one
# [error] line that names the lines to remove, and run no docker command.
lay_cluster() {  # <domain> <before, %b> <after, %b> <cidr>
  mkdir -p "${PROJ}/clusters/${1}"
  printf '%bkind: Lo\nmetadata:\n  name: %s\nspec:\n  runtime: kind\n  network:\n    name: %snet\n    cidr: %s\n  registries:\n    tls: false\n%b' \
    "${2}" "${1%%.*}" "${1%%.*}" "${4}" "${3}" > "${PROJ}/clusters/${1}/cluster.lok8s.yaml"
}
lay_cluster layoutlead.dev  '---\n'      ''                     10.99.16.0/24
lay_cluster layouttrail.dev ''           '---\n'                10.99.17.0/24
lay_cluster layoutempty.dev '---\n---\n' ''                     10.99.18.0/24
lay_cluster layouttwo.dev   ''           '---\nkind: KubeOne\n' 10.99.19.0/24
export PARITY_KIND_LOG="${WORK}/kind.log" PARITY_DOCKER_LOG="${WORK}/docker.log"
node_check layoutlead.dev
for side in bash go; do
  node_pin "${side}: layoutlead.dev (one document after ---) reached kind create" "${WORK}/kind.${side}.log" 1 '^kind: Cluster$'
done
# fd 3: the commands in the loop never read the table.
while IFS='|' read -r -u 3 d problem; do
  node_check "${d}"
  for side in bash go; do
    node_pin "${side}: ${d} is refused with the layout" "${WORK}/${side}.err" 1 "/clusters/${d}/cluster\.lok8s\.yaml ${problem}$"
    node_pin "${side}: ${d} ran no docker command" "${WORK}/docker.${side}.log" 0 '.'
  done
done 3<<'LAYOUT'
layouttrail.dev|holds more than one YAML document\. Remove line 11 \(a '---' line after the spec\)\.
layoutempty.dev|holds an empty YAML document before the spec\. Remove line 2 \(a '---' line\)\.
layouttwo.dev|holds more than one YAML document\. Remove lines 11 to 12 \(the '---' at line 11 and everything after it\)\.
LAYOUT
unset PARITY_KIND_LOG PARITY_DOCKER_LOG

# ── lo up — spec.network.dnsSearch ───────────────────────────────────────────
# dns.dev sets a search list, dnsalias.dev an anchored list with aliased
# entries, any letter case and a trailing dot: the kind stub logs the config
# `kind create` got on each side, the two logs must be byte-identical, and
# both must carry the list as written. dnsbad.dev has bad entries,
# dnslist.dev a value that is not a list, and dnsnl.dev two entries that end
# in a newline (a "…\n" string, a | block scalar): the read pass rejects them
# with the same lines on both sides, and kind create never runs.
# `provision --bootstrap` reads the spec through driver::export (bash) /
# Export (Go) and must stop there too.
mkdir -p "${PROJ}/clusters"/{dns.dev,dnsalias.dev,dnsmisc.dev,dnsbad.dev,dnslist.dev,dnsnl.dev}
dns_spec() {  # <domain> <name> <slot> <dnsSearch value…>
  printf 'kind: Lo\nmetadata:\n  name: %s\nspec:\n  runtime: kind\n  network:\n    name: %snet\n    cidr: 10.99.%s.0/24\n    dnsSearch: %s\n  registries:\n    tls: false\n    shared:\n      enabled: false\n' \
    "${2}" "${2}" "${3}" "${4}" > "${PROJ}/clusters/${1}/cluster.lok8s.yaml"
}
dns_spec dns.dev dnsc 21 '[corp.example.com, example.ts.net]'
dns_spec dnsbad.dev dnsbad 22 '[corp.example.com, Corp_Example, "", {name: x}, -bad.example, a..example]'
dns_spec dnslist.dev dnslist 23 'corp.example.com'
dns_spec dnsnl.dev dnsnl 25 '
      - "corp.example.com\n"
      - |
        blk.example'
cat > "${PROJ}/clusters/dnsalias.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: dnsalias
anchors:
  one: &one corp.example.com
  list: &list [*one, Example.TS.net., *one]
spec:
  runtime: kind
  network:
    name: dnsaliasnet
    cidr: 10.99.24.0/24
    dnsSearch: *list
  registries:
    tls: false
    shared:
      enabled: false
YAML
# dnsmisc.dev: a merge key in an unrelated field, alias cycles, a
# self-merging map and a 9^9-entry anchor tree outside spec.network. None
# may slow the read or print a yq merge-key WARN line.
cat > "${PROJ}/clusters/dnsmisc.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: dnsmisc
base: &base {x: 1}
other: {<<: *base, y: 2}
cyc: &cyc [x.example, *cyc]
more: *cyc
selfm: &selfm {<<: *selfm}
l0: &l0 [x.example, x.example, x.example, x.example, x.example, x.example, x.example, x.example, x.example]
l1: &l1 [*l0, *l0, *l0, *l0, *l0, *l0, *l0, *l0, *l0]
l2: &l2 [*l1, *l1, *l1, *l1, *l1, *l1, *l1, *l1, *l1]
l3: &l3 [*l2, *l2, *l2, *l2, *l2, *l2, *l2, *l2, *l2]
l4: &l4 [*l3, *l3, *l3, *l3, *l3, *l3, *l3, *l3, *l3]
l5: &l5 [*l4, *l4, *l4, *l4, *l4, *l4, *l4, *l4, *l4]
l6: &l6 [*l5, *l5, *l5, *l5, *l5, *l5, *l5, *l5, *l5]
l7: &l7 [*l6, *l6, *l6, *l6, *l6, *l6, *l6, *l6, *l6]
l8: &l8 [*l7, *l7, *l7, *l7, *l7, *l7, *l7, *l7, *l7]
big: *l8
spec:
  runtime: kind
  network:
    name: dnsmiscnet
    cidr: 10.99.26.0/24
    dnsSearch: [corp.example.com]
  registries:
    tls: false
    shared:
      enabled: false
YAML
export PARITY_KIND_LOG="${WORK}/kindcfg.log"
dns_pre() { reset_fixtures; rm -f "${PARITY_KIND_LOG}" "${PROJ}/clusters"/dns*.dev/.registries.json; }
dns_post() { if [[ -f "${PARITY_KIND_LOG}" ]]; then mv "${PARITY_KIND_LOG}" "${WORK}/kindcfg.${1}.log"; else : > "${WORK}/kindcfg.${1}.log"; fi; }
dns_check() { PARITY_PRE_EACH=dns_pre PARITY_POST_EACH=dns_post check "$@"; }
for d in 'dns.dev|  dnsSearch: \["corp\.example\.com", "example\.ts\.net"\]' \
  'dnsalias.dev|  dnsSearch: \["corp\.example\.com", "Example\.TS\.net\.", "corp\.example\.com"\]' \
  'dnsmisc.dev|  dnsSearch: \["corp\.example\.com"\]'; do
  line="${d#*|}"; d="${d%%|*}"
  dns_check "${KIND_CFG}" up --domain "${d}"
  parity::state_same "${WORK}/kindcfg.bash.log" "${WORK}/kindcfg.go.log" "kind config of lo up --domain ${d}" || failures=$((failures + 1))
  for side in bash go; do
    if grep -qx "${line}" "${WORK}/kindcfg.${side}.log"; then
      echo "ok: ${side}: kind create got the spec.network.dnsSearch list of ${d}"
    else fail "${side}: no spec.network.dnsSearch list in ${WORK}/kindcfg.${side}.log (${d})"; fi
    if grep -q 'yaml-fix-merge-anchor-to-spec' "${WORK}/${side}.err" "${WORK}/${side}.out"; then
      fail "${side}: a yq merge-key WARN line in the output of lo up --domain ${d}"
    fi
  done
done
# --bootstrap needs the cluster's kubeconfig file before it reads the spec.
mkdir -p "${PROJ}/.kubeconfig"
for d in dnsbad.dev dnslist.dev dnsnl.dev; do
  : > "${PROJ}/.kubeconfig/${d%.dev}.yaml"
  dns_check - up --domain "${d}"
  for side in bash go; do
    if grep -q '^error: spec\.network\.dnsSearch' "${WORK}/${side}.err" && [[ ! -s "${WORK}/kindcfg.${side}.log" ]]; then
      echo "ok: ${side}: ${d} rejected in the read pass, no kind create"
    else fail "${side}: ${d} not rejected in the read pass (see ${WORK}/${side}.err)"; fi
  done
  dns_check - provision --domain "${d}" --bootstrap
  for side in bash go; do
    # shellcheck disable=SC2154 # go_rc / bash_rc: set by parity::run_pair (hack/lib/parity.sh)
    if [[ "${side}" == go ]]; then side_rc="${go_rc}"; else side_rc="${bash_rc}"; fi
    if grep -q '^error: spec\.network\.dnsSearch' "${WORK}/${side}.err" && [[ "${side_rc}" != 0 ]]; then
      echo "ok: ${side}: provision --bootstrap stops at the rejected ${d} spec"
    else fail "${side}: provision --bootstrap went on past the rejected ${d} spec"; fi
  done
  rm -f "${PROJ}/.kubeconfig/${d%.dev}.yaml"
done
unset PARITY_KIND_LOG

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
# (base64 FAKECRT / FAKEKEY). It is the plugin of THIS lok8s: `--capabilities`
# lists the dry run (lo bootstrap --dry-run probes it before rendering).
if [[ "${1:-}" == "--capabilities" ]]; then echo dry-run; exit 0; fi
[[ -t 0 ]] || cat >/dev/null
echo "PATH_SECRETS=${PATH_SECRETS:-<unset>}" >> "${PARITY_PLUGIN_LOG}"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: RkFLRUNSVA==\n  tls.key: RkFLRUtFWQ==\n'
SH
chmod +x "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
parity::stub "${PROJ}" docker <<'SH'
#!/usr/bin/env bash
# Parity stub (TLS phase): the plain stub plus an argv log. The cert volume
# is always absent (so the import path runs and the volume is created), the
# read-out finds nothing (so both sides mint on every run). The registry
# image is local, unless PARITY_PULL_FAILS is set: then it is not, and the
# first PARITY_PULL_FAILS pulls fail with a transient network error.
echo "docker $*" >> "${PARITY_DOCKER_LOG}"
if [[ -n "${PARITY_PULL_FAILS:-}" ]]; then
  case "${1:-} ${2:-}" in
    "image inspect") exit 1 ;;
    "pull "*)
      echo x >> "${PARITY_PULL_COUNT}"
      if (( $(wc -l < "${PARITY_PULL_COUNT}") <= PARITY_PULL_FAILS )); then
        printf '%s\n\n' 'Error response from daemon: Get "https://registry-1.docker.io/v2/": proxyconnect tcp: dial tcp http://user:secret@proxy.invalid:3128: i/o timeout' >&2
        exit 1
      fi
      exit 0 ;;
  esac
fi
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

# The registry image is not local and the first pull fails with a
# transient network error: both sides warn once with the same cause (the
# proxy credentials masked), wait 2 s, pull again and only then create the
# io container. The streams and the docker argv are diffed byte for byte.
tls_pre_pull() { tls_pre; : > "${PARITY_PULL_COUNT}"; }
export PARITY_PULL_COUNT="${WORK}/pulls"
PARITY_PULL_FAILS=1 PARITY_PRE_EACH=tls_pre_pull PARITY_POST_EACH=tls_post check "${KIND_CFG}" up --domain mint.dev
parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo up --domain mint.dev (one failed pull)" || failures=$((failures + 1))
for side in bash go; do
  if [[ "$(grep -c '^docker pull registry:2\.8\.3$' "${WORK}/docker.${side}.log")" == 2 ]]; then echo "ok: ${side}: the failed pull is retried once"; else fail "${side}: want 2 pulls in ${WORK}/docker.${side}.log"; fi
  if grep -A1 '^docker pull registry:2\.8\.3$' "${WORK}/docker.${side}.log" | tail -1 | grep -q '^docker container create --name mintnet-registry-tls-io '; then
    echo "ok: ${side}: the io container is created after the pull"
  else fail "${side}: the io container does not follow the last pull"; fi
  tls_pin "${side}: one [warn] names the cause, credentials masked" "${WORK}/${side}.err" '^\[warn\] docker pull registry:2\.8\.3 failed \(attempt 1 of 4\): Error response from daemon: Get "https://registry-1\.docker\.io/v2/": proxyconnect tcp: dial tcp http://\*\*\*@proxy\.invalid:3128: i/o timeout\. Retrying in 2s\.$'
  tls_absent "${side}: no credentials on stderr" "${WORK}/${side}.err" 'user:secret'
done
unset PARITY_PULL_COUNT

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

# ── lo bootstrap — the staged values, byte for byte ──────────────────────────
# Review round 6, item 1: the values staging keeps yq's bytes. The last value
# of the merged values and of chart.yaml is a keep-chomped block scalar
# (`|+`), whose trailing newlines are part of the value. The stub kustomize
# copies the staged chart.yaml and values.merged.yaml out of the build copy
# and fails the build; the copies are diffed across the sides and pinned.
mkdir -p "${PROJ}/clusters/kept.dev/targets/kept"
cat > "${PROJ}/clusters/kept.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: keptc
spec:
  runtime: kind
  network:
    name: keptnet
    cidr: 10.99.14.0/24
  registries:
    tls: false
  bootstrap:
    - ./targets/kept
YAML
printf 'apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: kept\nvalueFiles:\n  - values.yaml\nnote: |+\n  beta\n\n' \
  > "${PROJ}/clusters/kept.dev/targets/kept/chart.yaml"
printf 'base: 1\n' > "${PROJ}/clusters/kept.dev/targets/kept/values.yaml"
printf 'driver: 2\nkeep: |+\n  alpha\n\n\n' > "${PROJ}/clusters/kept.dev/targets/kept/values.lo.yaml"
: > "${PROJ}/.kubeconfig/keptc.yaml"
parity::stub "${PROJ}" kustomize <<'SH'
#!/usr/bin/env bash
# Parity stub kustomize: keep the staged files of the build copy, then fail
# the build (the case stops at the render).
d="${@: -1}"
cp "${d}/chart.yaml" "${d}/values.merged.yaml" "${PARITY_STAGED}/" 2>/dev/null
echo "Error: parity stub: no render" >&2
exit 1
SH
export PARITY_STAGED="${WORK}/staged"
kept_pre() { reset_fixtures; rm -rf "${PROJ}/clusters/kept.dev/.registries.json" "${PARITY_STAGED}"; mkdir -p "${PARITY_STAGED}"; }
kept_post() { rm -rf "${WORK}/staged.${1}"; mv "${PARITY_STAGED}" "${WORK}/staged.${1}"; }
PARITY_PRE_EACH=kept_pre PARITY_POST_EACH=kept_post check - bootstrap --domain kept.dev
for f in chart.yaml values.merged.yaml; do
  parity::state_same "${WORK}/staged.bash/${f}" "${WORK}/staged.go/${f}" "the staged ${f} of the bootstrap render" || failures=$((failures + 1))
done
printf 'base: 1\ndriver: 2\nkeep: |+\n  alpha\n\n\n' > "${WORK}/staged.want"
parity::state_same "${WORK}/staged.want" "${WORK}/staged.go/values.merged.yaml" "the staged values.merged.yaml keeps the trailing newlines of a |+ value" \
  || failures=$((failures + 1))
unset PARITY_STAGED

# ── lo bootstrap --dry-run / --only ──────────────────────────────────────────
# A dry run renders every entry like the real run and applies NOTHING. The
# stub kustomize answers a manifest (one ConfigMap, named after the dir it
# renders); the stub kubectl logs every argv, answers `diff` from a script
# (unchanged for the first entry, a canned two-object diff for the second,
# unreachable for the third case) and FAILS LOUDLY on `apply`, so a dry run
# that applies cannot pass on either side. The streams, the exit codes, the
# kubectl argv and the render dir's files are diffed across the two
# implementations; the Go side is also pinned to the contract (exit codes
# 0/1/2, the report lines, no `apply` in the log).
mkdir -p "${PROJ}/clusters/dry.dev/targets/extra"
cat > "${PROJ}/clusters/dry.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: dryc
spec:
  runtime: kind
  network:
    name: drynet
    cidr: 10.99.12.0/24
  registries:
    tls: false
  bootstrap:
    - cilium
    - ./targets/extra:
        name: extra-cm
        dependsOn: [cilium]
    - ccm
YAML
printf 'resources: []\n' > "${PROJ}/clusters/dry.dev/targets/extra/kustomization.yaml"
: > "${PROJ}/.kubeconfig/dryc.yaml"
parity::stub "${PROJ}" kustomize <<'SH'
#!/usr/bin/env bash
# Parity stub kustomize: one ConfigMap named after the rendered directory
# (a chart addon renders from a temp copy, so the name comes from chart.yaml
# when there is one).
dir="${@: -1}"
name="$(basename "${dir}")"
if [[ -f "${dir}/chart.yaml" ]]; then
  name="$(sed -n 's/^  name: //p' "${dir}/chart.yaml" | head -1)"
fi
# What the render saw (round 3): the registry value a real run reads back
# from .registries.json, the sentinel, the plugin home, the flags.
if [[ -n "${PARITY_RENDER_LOG:-}" ]]; then
  echo "BUILD_HOST=${LOK8S_SPEC_REGISTRY_BUILD_HOST:-<unset>} DRYRUN=${LOK8S_BOOTSTRAP_DRYRUN:-<unset>} HOME=${KUSTOMIZE_PLUGIN_HOME:-<unset>} PATH0=${PATH%%:*} HELMP=${HELM_PLUGINS:-<unset>} TMP=${TMPDIR:-<unset>} STORE=${LOK8S_BOOTSTRAP_DRYRUN_STORE:-<unset>} ARGS=$*" >> "${PARITY_RENDER_LOG}"
fi
# A slow render, to kill the dry run in the middle of it.
if [[ -n "${PARITY_SLOW_MARK:-}" ]]; then
  : > "${PARITY_SLOW_MARK}"
  sleep 2
fi
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: kube-system\n' "${name}"
# Round 4: a CA Secret whose data changes on every render (a chart that
# mints TLS material per render, as Cilium does).
if [[ -n "${PARITY_ND_COUNTER:-}" ]]; then
  echo x >> "${PARITY_ND_COUNTER}"
  printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: %s-ca\n  namespace: kube-system\ndata:\n  ca.crt: Q0Et%s\n' "${name}" "$(wc -l < "${PARITY_ND_COUNTER}" | tr -d ' ')"
fi
SH
parity::stub "${PROJ}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub kubectl (dry run): log the argv; `diff` answers per the
# manifest on stdin (unchanged / a canned two-object diff / unreachable);
# anything that writes must never be reached.
echo "kubectl $*" >> "${PARITY_KUBECTL_LOG}"
# Only a `-f -` call carries a manifest: never read stdin otherwise (a stub
# that always reads hangs on a terminal or an open pipe).
manifest=""
[[ " $* " != *" -f - "* ]] || manifest="$(cat)"
case " $* " in
  *" diff "*)
    case "${manifest}" in
      *"name: cilium-ca"*|*"name: extra-cm"*|*"name: extra"*)
        cat <<'DIFF'
diff -u -N /tmp/LIVE-1/v1.ConfigMap.kube-system.extra /tmp/MERGED-1/v1.ConfigMap.kube-system.extra
--- /tmp/LIVE-1/v1.ConfigMap.kube-system.extra	2026-10-12 10:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.kube-system.extra	2026-10-12 10:00:00.000000000 +0000
@@ -3,4 +3,5 @@
 kind: ConfigMap
 metadata:
   name: extra
+  foo: bar
diff -u -N /tmp/LIVE-1/v1.ConfigMap.kube-system.new /tmp/MERGED-1/v1.ConfigMap.kube-system.new
--- /tmp/LIVE-1/v1.ConfigMap.kube-system.new	1970-01-01 00:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.kube-system.new	2026-10-12 10:00:00.000000000 +0000
@@ -0,0 +1,4 @@
+apiVersion: v1
+kind: ConfigMap
+metadata:
+  name: new
diff -u -N /tmp/LIVE-1/v1.ConfigMap.kube-system.other /tmp/MERGED-1/v1.ConfigMap.kube-system.other
--- /tmp/LIVE-1/v1.ConfigMap.kube-system.other	1970-01-01 00:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.kube-system.other	2026-10-12 10:00:00.000000000 +0000
@@ -0,0 +1,4 @@
+apiVersion: v1
+kind: ConfigMap
+metadata:
+  name: other
DIFF
        exit 1 ;;
      *"name: cilium"*) exit 0 ;;
      *) echo "error: the server could not be reached" >&2; exit 2 ;;
    esac ;;
esac
echo "parity stub: kubectl $1 $2 $3 must not be reached by a dry run" >&2
exit 7
SH
export PARITY_KUBECTL_LOG="${WORK}/kubectl.log"
# The argv log is cleared BEFORE each side: a dry_expect_rc run (the Go
# side alone, no post hook) would otherwise leak its calls into the next case.
dry_pre() { reset_fixtures; rm -rf "${PROJ}/clusters/dry.dev/.registries.json" "${PROJ}/render-out" "${WORK}/kubectl.log"; }
dry_post() {  # <impl>: keep the kubectl log and the render dir per side (normalized)
  if [[ -f "${WORK}/kubectl.log" ]]; then
    sed -E "s|${PROJ}|PROJ|g" "${WORK}/kubectl.log" > "${WORK}/kubectl.${1}.log"
    rm -f "${WORK}/kubectl.log"
  else
    : > "${WORK}/kubectl.${1}.log"
  fi
  rm -rf "${WORK}/render.${1}"
  if [[ -d "${PROJ}/render-out" ]]; then cp -R "${PROJ}/render-out" "${WORK}/render.${1}"; else mkdir -p "${WORK}/render.${1}"; fi
}
dry_check() { PARITY_PRE_EACH=dry_pre PARITY_POST_EACH=dry_post check "$@"; }
dry_expect_rc() { PARITY_PRE_EACH=dry_pre expect_rc "$@"; }
# The three entries: cilium unchanged, extra-cm differs (1 changed, 2 new),
# ccm unreachable (an error) → rc 2, every line reported.
dry_check - bootstrap --domain dry.dev --dry-run --render-dir render-out
parity::state_same "${WORK}/kubectl.bash.log" "${WORK}/kubectl.go.log" "kubectl argv of lo bootstrap --dry-run" || failures=$((failures + 1))
tree_check "${WORK}/render.go" "${WORK}/render.bash" "render dir of lo bootstrap --dry-run"
for side in bash go; do
  tls_absent "${side}: a dry run never applies"  "${WORK}/kubectl.${side}.log" " apply "
  tls_pin    "${side}: three server-side diffs"  "${WORK}/kubectl.${side}.log" "^kubectl --kubeconfig PROJ/\.kubeconfig/dryc\.yaml diff --server-side --force-conflicts -f -$"
  tls_pin    "${side}: cilium unchanged"         "${WORK}/${side}.out" "^bootstrap dry-run: 01-cilium: unchanged$"
  tls_pin    "${side}: extra-cm differs"         "${WORK}/${side}.out" "^bootstrap dry-run: 02-extra-cm: differs changed=1 new=2$"
  tls_pin    "${side}: ccm error"                "${WORK}/${side}.out" "^bootstrap dry-run: 03-ccm: error$"
  tls_pin    "${side}: the summary"              "${WORK}/${side}.out" "^bootstrap dry-run: entries=3 unchanged=1 differs=1 rendered=0 skipped=0 inconclusive=0 errors=1 nondeterministic=0$"
  tls_pin    "${side}: the render dir is named"  "${WORK}/${side}.err" "^bootstrap dry-run: files under render-out$"
  if [[ -s "${WORK}/render.${side}/02-extra-cm.diff" ]]; then echo "ok: ${side}: 02-extra-cm.diff holds the diff"; else fail "${side}: 02-extra-cm.diff empty or missing"; fi
  if [[ -f "${WORK}/render.${side}/01-cilium.diff" && ! -s "${WORK}/render.${side}/01-cilium.diff" ]]; then echo "ok: ${side}: 01-cilium.diff is empty"; else fail "${side}: 01-cilium.diff not an empty file"; fi
  if [[ -s "${WORK}/render.${side}/03-ccm.yaml" ]]; then echo "ok: ${side}: 03-ccm.yaml rendered before the failed diff"; else fail "${side}: 03-ccm.yaml missing"; fi
done
dry_expect_rc 2 bootstrap --domain dry.dev --dry-run --render-dir render-out
# --only narrows the run: one entry, numbered from 01; its unchanged diff → rc 0.
dry_check - bootstrap --domain dry.dev --dry-run --only cilium
dry_expect_rc 0 bootstrap --domain dry.dev --dry-run --only cilium
# The addon name of a renamed entry matches; the entry differs → rc 1.
dry_check - bootstrap --domain dry.dev --dry-run --only extra
dry_expect_rc 1 bootstrap --domain dry.dev --dry-run --only extra
# --no-diff: render only, kubectl is never reached (an empty argv log).
dry_check - bootstrap --domain dry.dev --dry-run --no-diff --only cilium,ccm --render-dir render-out
for side in bash go; do
  if [[ ! -s "${WORK}/kubectl.${side}.log" ]]; then echo "ok: ${side}: --no-diff never reached kubectl"; else fail "${side}: --no-diff reached kubectl"; fi
  tls_pin "${side}: ccm rendered" "${WORK}/${side}.out" "^bootstrap dry-run: 02-ccm: rendered$"
done
tree_check "${WORK}/render.go" "${WORK}/render.bash" "render dir of lo bootstrap --dry-run --no-diff"
# An unknown --only name lists the valid names; the flag dependencies.
dry_check - bootstrap --domain dry.dev --only nope
for side in bash go; do
  tls_pin "${side}: the unknown --only name lists the entries" "${WORK}/${side}.err" "bootstrap: --only: unknown entry 'nope' \(valid: cilium, extra-cm, ccm\)"
done
dry_check - bootstrap --domain dry.dev --render-dir render-out
dry_check - bootstrap --domain dry.dev --no-diff
dry_check - provision --domain dry.dev --dry-run
dry_check - provision --domain dry.dev --only cilium
dry_check - provision --domain dry.dev -b --no-diff
# lo provision --bootstrap --dry-run: the same dry run through the provision
# dispatch (kind Lo: no gate either way; the tail's writers are skipped).
dry_check - provision --domain dry.dev --bootstrap --dry-run --only cilium
dry_expect_rc 0 provision --domain dry.dev --bootstrap --dry-run --only cilium
dry_check - provision --domain beta.cloud --bootstrap --dry-run     # cloud kind: NOT gated, stops at the missing kubeconfig (rc 2: a dry run's failure outside the diff; never 3)
dry_expect_rc 2 provision --domain beta.cloud --bootstrap --dry-run

# ── Review round 3: the in-place dry run ─────────────────────────────────────
# A dry run renders IN PLACE, exactly as a real run does. The project now
# carries what the round-2 whole-project copy could not survive — a dangling
# symlink, a directory nobody may read, an 8 GiB sparse file in an ignored
# cache directory: the dry run never walks the project, never writes into it
# (no .registries.json either), and still exports the registry values a real
# run reads back from that file. The stub kustomize logs what the render saw
# (the boundary's temp path normalized to BOUNDARY).
ln -s /nonexistent/lok8s-parity-target "${PROJ}/dangling"
mkdir -p "${PROJ}/locked" "${PROJ}/.cache"
echo x > "${PROJ}/locked/secret"
chmod 000 "${PROJ}/locked"
truncate -s 8G "${PROJ}/.cache/core"
export PARITY_RENDER_LOG="${WORK}/render.log"
tree_snapshot() {  # the project tree without the files every run rewrites
  find "${PROJ}" -path "${PROJ}/locked" -prune -o ! -name lok8s.yaml -printf '%P %y %s %T@\n' 2>/dev/null | sort
}
r3_pre() { dry_pre; rm -f "${PARITY_RENDER_LOG}"; tree_snapshot > "${WORK}/tree.before"; }
r3_post() {  # <impl>
  dry_post "${1}"
  tree_snapshot > "${WORK}/tree.after"
  if diff -q "${WORK}/tree.before" "${WORK}/tree.after" >/dev/null; then : > "${WORK}/tree.${1}.diff"; else diff "${WORK}/tree.before" "${WORK}/tree.after" > "${WORK}/tree.${1}.diff"; fi
  if [[ -e "${PROJ}/clusters/dry.dev/.registries.json" ]]; then echo present > "${WORK}/registries.${1}"; else echo absent > "${WORK}/registries.${1}"; fi
  sed -E 's#lok8s-dryrun-[A-Za-z0-9]+#BOUNDARY#g; s#lok8s-(addon|chart)-[A-Za-z0-9]+#COPY#g' "${PARITY_RENDER_LOG}" > "${WORK}/render.${1}.log" 2>/dev/null || : > "${WORK}/render.${1}.log"
}
PARITY_PRE_EACH=r3_pre PARITY_POST_EACH=r3_post check - bootstrap --domain dry.dev --dry-run --only cilium
parity::state_same "${WORK}/render.bash.log" "${WORK}/render.go.log" "what the dry run's render saw" || failures=$((failures + 1))
for side in bash go; do
  if [[ ! -s "${WORK}/tree.${side}.diff" ]]; then echo "ok: ${side}: the dry run changed nothing in the project tree"; else fail "${side}: the dry run changed the project tree: $(head -5 "${WORK}/tree.${side}.diff")"; fi
  if [[ "$(cat "${WORK}/registries.${side}")" == absent ]]; then echo "ok: ${side}: no .registries.json written"; else fail "${side}: the dry run wrote .registries.json"; fi
  tls_pin    "${side}: the registry value a real run reads back"  "${WORK}/render.${side}.log" "^BUILD_HOST=lok8s\.local DRYRUN=1 "
  tls_pin    "${side}: the boundary's plugin home and stubs"      "${WORK}/render.${side}.log" " HOME=[^ ]*/BOUNDARY/plugins PATH0=[^ ]*/BOUNDARY/bin "
  tls_pin    "${side}: a dry run renders with --enable-alpha-plugins" "${WORK}/render.${side}.log" " ARGS=build --enable-alpha-plugins /[^ ]*BOUNDARY/tmp/COPY$"
  tls_absent "${side}: no --enable-exec in a dry run (absent)"    "${WORK}/render.${side}.log" "--enable-exec"
  tls_pin    "${side}: the boundary's helm plugins, TMPDIR and run store" "${WORK}/render.${side}.log" " HELMP=[^ ]*/BOUNDARY/helm-plugins TMP=[^ ]*/BOUNDARY/tmp STORE=[^ ]*/BOUNDARY/store "
done
dry_expect_rc 0 bootstrap --domain dry.dev --dry-run --only cilium
unset PARITY_RENDER_LOG

# An entry's env: may not set lo's own variables — checked before anything
# of it is exported: an entry error (dry run rc 2, real run rc 1), and the
# render never runs. The round-2 escapes are the names below.
mkdir -p "${PROJ}/clusters/envx.dev/targets/app"
printf 'resources: []\n' > "${PROJ}/clusters/envx.dev/targets/app/kustomization.yaml"
: > "${PROJ}/.kubeconfig/envxc.yaml"
for reserved in LOK8S_BOOTSTRAP_DRYRUN BOOTSTRAP_PROTECTED_ENV_NAMES bootstrap_render_dir HELM_PLUGINS; do
  cat > "${PROJ}/clusters/envx.dev/cluster.lok8s.yaml" <<YAML
kind: Lo
metadata:
  name: envxc
spec:
  runtime: kind
  network:
    name: envxnet
    cidr: 10.99.13.0/24
  registries:
    tls: false
  bootstrap:
    - ./targets/app:
        env:
          LOK8S_USER_OK: fine
          ${reserved}: "0"
YAML
  dry_check - bootstrap --domain envx.dev --dry-run
  dry_expect_rc 2 bootstrap --domain envx.dev --dry-run
  for side in bash go; do
    tls_pin "${side}: ${reserved} in an entry env is an entry error" "${WORK}/${side}.err" "bootstrap: app: env: may not set ${reserved} \(reserved for lo\)"
    tls_absent "${side}: the render never ran for ${reserved}" "${WORK}/kubectl.${side}.log" " diff "
  done
  dry_check - bootstrap --domain envx.dev
  dry_expect_rc 1 bootstrap --domain envx.dev
done

# The exit contract at both command boundaries: 1 only for a diff that found
# a change, 2 for every other failure of a dry run (a missing spec, an
# unknown --only name, a flag combination, a parse error), the real run's
# codes unchanged.
dry_check - bootstrap --domain missing.dev --dry-run
dry_expect_rc 2 bootstrap --domain missing.dev --dry-run
dry_expect_rc 1 bootstrap --domain missing.dev
dry_check - bootstrap --domain dry.dev --dry-run --only nope
dry_expect_rc 2 bootstrap --domain dry.dev --dry-run --only nope
dry_expect_rc 2 provision --domain dry.dev --bootstrap --dry-run --only nope
dry_expect_rc 2 provision --domain dry.dev --dry-run
dry_check - bootstrap --domain dry.dev --dry-run --bogus
dry_expect_rc 2 bootstrap --domain dry.dev --dry-run --bogus
dry_expect_rc 2 provision --domain dry.dev --bootstrap --dry-run --bogus
# Review round 4, item C(f): the flag errors of a dry run are rc 2 too —
# --only given but blank, --with-restore without --only — at both command
# boundaries; --ignore-nondeterministic needs --bootstrap and --dry-run (a
# real run's flag error stays rc 1).
dry_check - bootstrap --domain dry.dev --dry-run --only=
dry_expect_rc 2 bootstrap --domain dry.dev --dry-run --only=
dry_check - provision --domain dry.dev --bootstrap --dry-run --only=
dry_expect_rc 2 provision --domain dry.dev --bootstrap --dry-run --only=
dry_check - bootstrap --domain dry.dev --dry-run --with-restore
dry_expect_rc 2 bootstrap --domain dry.dev --dry-run --with-restore
dry_check - provision --domain dry.dev --bootstrap --dry-run --with-restore
dry_expect_rc 2 provision --domain dry.dev --bootstrap --dry-run --with-restore
dry_check - bootstrap --domain dry.dev --ignore-nondeterministic
dry_expect_rc 1 bootstrap --domain dry.dev --ignore-nondeterministic
dry_check - provision --domain dry.dev --dry-run --ignore-nondeterministic
dry_expect_rc 2 provision --domain dry.dev --dry-run --ignore-nondeterministic

# Review round 4, item A8: every entry renders twice; an object whose content
# differs between the two renders is listed as regenerated on every apply.
# The verdict stays honest (the stub's diff sees the Secret: rc 1);
# --ignore-nondeterministic judges the entry without it (rc 0) and still
# lists it.
export PARITY_ND_COUNTER="${WORK}/nd.count"
dry_check - bootstrap --domain dry.dev --dry-run --only cilium
dry_expect_rc 1 bootstrap --domain dry.dev --dry-run --only cilium
for side in bash go; do
  tls_pin "${side}: the entry differs, its regenerated object counted" "${WORK}/${side}.out" "^bootstrap dry-run: 01-cilium: differs changed=1 new=2 \(non-deterministic: 1\)$"
  tls_pin "${side}: the regenerated object is listed" "${WORK}/${side}.out" "^bootstrap dry-run: 01-cilium: non-deterministic \(regenerated on every apply\): v1 Secret kube-system/cilium-ca$"
  tls_pin "${side}: the summary counts it" "${WORK}/${side}.out" " errors=0 nondeterministic=1$"
done
dry_check - bootstrap --domain dry.dev --dry-run --only cilium --ignore-nondeterministic
dry_expect_rc 0 bootstrap --domain dry.dev --dry-run --only cilium --ignore-nondeterministic
dry_expect_rc 0 provision --domain dry.dev --bootstrap --dry-run --only cilium --ignore-nondeterministic
for side in bash go; do
  tls_pin "${side}: --ignore-nondeterministic: unchanged, still listed" "${WORK}/${side}.out" "^bootstrap dry-run: 01-cilium: unchanged \(non-deterministic: 1\)$"
done
unset PARITY_ND_COUNTER

# A dry run killed by SIGTERM in the middle of a render leaves no temp file
# behind (the bash twin removes its boundary from a chained EXIT trap, the
# Go binary from the cancelled context's defers).
export PARITY_SLOW_MARK="${WORK}/slow.started"
for side in bash go; do
  sig_tmp="${WORK}/sigterm-tmp.${side}"
  rm -rf "${sig_tmp}" "${PARITY_SLOW_MARK}"; mkdir -p "${sig_tmp}"
  parity::select "${PROJ}" "${side}"
  ( cd "${PROJ}" && TMPDIR="${sig_tmp}" exec "${LO_BIN}" bootstrap --domain dry.dev --dry-run --only cilium </dev/null >/dev/null 2>&1 ) &
  sig_pid=$!
  for (( i = 0; i < 100; i++ )); do [[ -e "${PARITY_SLOW_MARK}" ]] && break; sleep 0.1; done
  if [[ -e "${PARITY_SLOW_MARK}" ]]; then echo "ok: ${side}: the dry run reached the render"; else fail "${side}: the dry run never reached the render"; fi
  kill -TERM "${sig_pid}" 2>/dev/null || true
  wait "${sig_pid}" 2>/dev/null || true
  sleep 3   # the stub render's own sleep ends; an orphan must not write either
  if [[ -z "$(ls -A "${sig_tmp}")" ]]; then echo "ok: ${side}: SIGTERM mid-render left no temp file"; else fail "${side}: SIGTERM left $(ls -A "${sig_tmp}")"; fi
done
unset PARITY_SLOW_MARK sig_tmp sig_pid
chmod 755 "${PROJ}/locked"
rm -rf "${PROJ}/locked" "${PROJ}/dangling" "${PROJ}/.cache/core"
unset PARITY_KUBECTL_LOG
parity::stub_kubectl_fail "${PROJ}"

# ── lo provision — the KubeOne manifest (spec.auditLog, spec.oidc) ───────────
# A mock provider whose provision succeeds (and says so on stderr) and whose
# output holds no inventory: the driver checks spec.auditLog, provisions,
# renders kubeone.yaml (generate_config: the core template, then the spec.oidc
# and spec.auditLog merges), then stops at the inventory step ("ssh private
# key not found"), before kubeone apply. The streams are diffed, and so is the
# rendered manifest. The Go merge writes with yaml.v3 and the bash with yq -i,
# so the CONTENT is the contract: both sides are compared as sorted JSON. A
# refused spec.auditLog must stop BEFORE the provision ("mock provision ran"
# absent) and leave no manifest and no temp file.
mkdir -p "${PROJ}/.lok8s/providers/mock"
cat > "${PROJ}/.lok8s/providers/mock/main" <<'SH'
#!/usr/bin/env argsh
# mock — parity-harness provider: provision succeeds without touching
# anything; the output holds no inventory, so the driver stops right after
# the manifest render.
provider::validate() { return 0; }
provider::credential_data() { return 0; }
provider::provision() { echo "mock provision ran" >&2; return 0; }
provider::destroy() { echo "mock destroy must not run" >&2; return 1; }
provider::output() { printf '{}\n'; }
SH
# ko_spec <domain> [spec lines…]: a KubeOne spec on the mock provider.
ko_spec() {
  local domain="${1}"; shift
  mkdir -p "${PROJ}/clusters/${domain}"
  printf 'cluster_name: %s\n' "${domain%%.*}" > "${PROJ}/clusters/${domain}/provider.yaml"
  {
    printf 'kind: KubeOne\nmetadata:\n  name: %s\nspec:\n  kubernetes:\n    version: "1.31.0"\n' "${domain%%.*}"
    printf '  provider:\n    name: mock\n    configRef: provider.yaml\n'
    if (( $# )); then printf '%s\n' "$@"; fi
  } > "${PROJ}/clusters/${domain}/cluster.lok8s.yaml"
}
ko_policy() {  # <file> [kind]: an audit Policy (or another kind)
  printf 'apiVersion: audit.k8s.io/v1\nkind: %s\nrules:\n  - level: Metadata\n    resources:\n      - group: ""\n        resources: ["secrets"]\n  - level: None\n' "${2:-Policy}" > "${1}"
}
ko_spec audit.cloud '  oidc:' '    issuer: https://id.example.com' '    clientID: kubectl' \
  '  auditLog:' '    policy: audit-policy.yaml' '    maxAge: 30' '    maxBackup: 10' '    maxSize: 100'
ko_policy "${PROJ}/clusters/audit.cloud/audit-policy.yaml"
ko_spec auditmin.cloud '  auditLog:' '    policy: ./sub/../../audit.cloud/audit-policy.yaml' '    maxBackup: 5'
ko_spec auditnone.cloud
ko_spec auditkind.cloud '  auditLog:' '    policy: audit-policy.yaml'
ko_policy "${PROJ}/clusters/auditkind.cloud/audit-policy.yaml" Event
ko_spec auditmiss.cloud '  auditLog:' '    policy: missing.yaml'
ko_spec auditkey.cloud '  auditLog:' '    policy: audit-policy.yaml' '    maxBackups: 3'
ko_spec auditlim.cloud '  auditLog:' '    policy: audit-policy.yaml' '    maxSize: "100"'
ko_spec auditmap.cloud '  auditLog: true'

# ko_check <domain> <argv…>: one case, plus the rendered manifest diffed.
ko_pre() { rm -rf "${PROJ}/clusters/${KO_DOMAIN}/.kubeone"; }
ko_post() {  # <impl>: the manifest as sorted JSON (or "absent"), then the temp-file count
  local m="${PROJ}/clusters/${KO_DOMAIN}/.kubeone/kubeone.yaml"
  if [[ -f "${m}" ]]; then
    "${ROOT}/.bin/yq" -o=json -P 'sort_keys(..)' "${m}" | sed "s|${PROJ}|PROJ|g" > "${WORK}/kubeone.${1}.json"
  else
    echo absent > "${WORK}/kubeone.${1}.json"
  fi
  printf 'temp files: %s\n' "$(find "${PROJ}/clusters/${KO_DOMAIN}" -name '.kubeone.yaml.*' 2>/dev/null | wc -l)" >> "${WORK}/kubeone.${1}.json"
}
ko_check() {
  KO_DOMAIN="${1}"; shift
  local ok=1
  PARITY_PRE_EACH=ko_pre PARITY_POST_EACH=ko_post parity::run_pair "$@"
  parity::compare "lo $*" - || ok=0
  parity::state_same "${WORK}/kubeone.bash.json" "${WORK}/kubeone.go.json" "kubeone.yaml of lo $*" || ok=0
  parity::record "${ok}" "lo $*"
}
# ko_has / ko_lacks <label> <file> <fixed string>: anti-vacuity pins. Parity
# also passes when both sides drift together (or render nothing), so the Go
# side's output is pinned to the contract as well.
ko_has() {
  if grep -qF -- "${3}" "${2}"; then echo "ok: ${1}"; else
    echo "FAIL: ${1} — '${3}' not in ${2}:"; sed 's/^/  /' "${2}" | head -40
    failures=$((failures + 1))
  fi
}
ko_lacks() {
  if grep -qF -- "${3}" "${2}"; then
    echo "FAIL: ${1} — '${3}' found in ${2}"; failures=$((failures + 1))
  else echo "ok: ${1}"; fi
}
# ko_refused <label>: the last case stopped BEFORE the provision and left no
# manifest and no temp file (the whole snapshot, compared exactly).
ko_refused() {
  ko_lacks "go: ${1} refused before the provision" "${WORK}/go.err" "mock provision ran"
  if [[ "$(cat "${WORK}/kubeone.go.json")" == $'absent\ntemp files: 0' ]]; then
    echo "ok: go: ${1} left no manifest and no temp file"
  else
    echo "FAIL: go: ${1} left a manifest or a temp file:"; sed 's/^/  /' "${WORK}/kubeone.go.json" | head -5
    failures=$((failures + 1))
  fi
}
ko_check audit.cloud provision --domain audit.cloud -f          # oidc + auditLog with every limit
for want in '"staticAuditLog": {' '"enable": true' \
  '"policyFilePath": "PROJ/clusters/audit.cloud/audit-policy.yaml"' \
  '"logMaxAge": 30' '"logMaxBackup": 10' '"logMaxSize": 100' '"openidConnect": {' '"encryptionProviders": {'; do
  ko_has "go: kubeone.yaml of audit.cloud has ${want}" "${WORK}/kubeone.go.json" "${want}"
done
ko_has "go: the provision stopped at the inventory step" "${WORK}/go.err" "ssh private key not found"
ko_has "go: audit.cloud provisioned after the check" "${WORK}/go.err" "mock provision ran"
ko_has "go: audit.cloud left no temp file" "${WORK}/kubeone.go.json" "temp files: 0"
ko_check auditmin.cloud provision --domain auditmin.cloud -f    # a relative path with .. cleaned; unset limits omitted
ko_has "go: auditmin.cloud resolves the shared policy" "${WORK}/kubeone.go.json" '"policyFilePath": "PROJ/clusters/audit.cloud/audit-policy.yaml"'
ko_lacks "go: auditmin.cloud leaves logMaxAge to KubeOne" "${WORK}/kubeone.go.json" '"logMaxAge"'
ko_check auditnone.cloud provision --domain auditnone.cloud -f  # no spec.auditLog: no staticAuditLog
ko_has "go: auditnone.cloud rendered a manifest" "${WORK}/kubeone.go.json" '"encryptionProviders": {'
ko_lacks "go: auditnone.cloud has no staticAuditLog" "${WORK}/kubeone.go.json" 'staticAuditLog'
ko_check auditkind.cloud provision --domain auditkind.cloud -f  # the policy file is not a Policy
ko_has "go: auditkind.cloud refused" "${WORK}/go.err" "is not an audit Policy"
ko_refused auditkind.cloud
ko_check auditmiss.cloud provision --domain auditmiss.cloud -f  # the policy file does not exist
ko_has "go: auditmiss.cloud refused" "${WORK}/go.err" "policy file not found: PROJ/clusters/auditmiss.cloud/missing.yaml"
ko_refused auditmiss.cloud
ko_check auditkey.cloud provision --domain auditkey.cloud -f    # an unknown field
ko_has "go: auditkey.cloud refused" "${WORK}/go.err" "spec.auditLog.maxBackups in PROJ/clusters/auditkey.cloud/cluster.lok8s.yaml is not a known field"
ko_refused auditkey.cloud
ko_check auditlim.cloud provision --domain auditlim.cloud -f    # a limit as text
ko_has "go: auditlim.cloud refused" "${WORK}/go.err" "(found !!str '100')"
ko_refused auditlim.cloud
ko_check auditmap.cloud provision --domain auditmap.cloud -f    # spec.auditLog is not a mapping
ko_has "go: auditmap.cloud refused" "${WORK}/go.err" "spec.auditLog in PROJ/clusters/auditmap.cloud/cluster.lok8s.yaml is not a mapping"
ko_refused auditmap.cloud

# The policy-file check over the shared fixtures
# (tests/fixtures/audit-policy/<verdict>--<case>.yaml; the Go and bats tests
# read the same files). yq and yaml.v3 read some YAML differently (a
# "--- # comment" header, a value on a --- line, an empty first document, a
# directive, an anchor or alias, a merge key, a duplicate key): each such
# case is a fixture here, so a future divergence fails.
declare -A ko_reason=(
  [ok]="ssh private key not found"
  [directive]="has a YAML directive (a line that starts with %)"
  [marker]="has a value on a document marker line (--- or ...)"
  [invalid]="is not valid YAML"
  [emptyfirst]="starts with an empty YAML document"
  [duplicate]="has a duplicate key"
  [alias]="has an anchor or an alias (&name, *name)"
  [merge]="has a merge key (<<)"
  [notpolicy]="is not an audit Policy"
  [norules]="has no rules"
)
ko_n=0
for fixture in "${ROOT}"/tests/fixtures/audit-policy/*--*.yaml; do
  ko_n=$((ko_n + 1))
  verdict="${fixture##*/}"; verdict="${verdict%%--*}"
  domain="pf${ko_n}.cloud"
  ko_spec "${domain}" '  auditLog:' '    policy: audit-policy.yaml'
  cp "${fixture}" "${PROJ}/clusters/${domain}/audit-policy.yaml"
  ko_check "${domain}" provision --domain "${domain}" -f          # fixture ${fixture##*/}
  ko_has "go: ${fixture##*/} → ${verdict}" "${WORK}/go.err" "${ko_reason[${verdict}]}"
  if [[ "${verdict}" == ok ]]; then
    ko_has "go: ${fixture##*/} merged" "${WORK}/kubeone.go.json" '"staticAuditLog": {'
  else
    ko_refused "${fixture##*/}"
  fi
done
(( ko_n >= 20 )) || fail "only ${ko_n} audit policy fixtures found"

# ── lo provision — the registry network's gateway priority ───────────────────
# A shared-registries cluster whose project network ("zz-gwnet") sorts after
# "lok8s-registries": with the same gateway priority, Docker gives the default
# route to the registry network. The stubs: kind reports two nodes (with
# PARITY_HA=1 also kind's HA load balancer, whose image has no `ip`: a docker
# exec in it fails with 127); docker answers `docker version` with
# PARITY_DOCKER_VERSION, `docker inspect <node>` with the kind role, a running
# state, the gateway of the node's endpoint on the project network (empty with
# PARITY_NO_GATEWAY=1) and, when asked, the gateway priority of its registry
# endpoint (PARITY_GW_PRIO, default 0), and the in-node `ip` reads (a default
# route through the registry network on eth1, the project network on eth0).
# The registry network always has two foreign members (another cluster's
# node, a registry that is not one of this project's mirrors);
# PARITY_ATTACHED=1 lists the cluster's containers too.
# `docker network inspect` reports no gateway for the project network: the
# shape of every Docker up to 28.5 for a network created with --subnet and no
# --gateway. Docker 28+: the connect carries --gw-priority -1 and the route is
# not touched. Docker 27: a plain connect, then the route replace. An attached
# node: no connect, the route replace, also when shared is off (gwprio-off.dev,
# a cluster created under the old shared default). No gateway: one warning per
# node, the route is not read. HA: the load balancer is never exec'd and never
# named in a warning; at priority -1 on Docker 29 no node is exec'd either.
# The docker argv is diffed across the sides and pinned.
mkdir -p "${PROJ}/clusters/gwprio.dev" "${PROJ}/clusters/gwprio-off.dev"
cat > "${PROJ}/clusters/gwprio.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: zz-gwprio
spec:
  runtime: kind
  kubernetes:
    version: v1.31.4
  network:
    name: zz-gwnet
    cidr: 10.99.16.0/24
  registries:
    tls: false
    shared:
      enabled: true
YAML
cat > "${PROJ}/clusters/gwprio-off.dev/cluster.lok8s.yaml" <<'YAML'
kind: Lo
metadata:
  name: zz-gwprio
spec:
  runtime: kind
  kubernetes:
    version: v1.31.4
  network:
    name: zz-gwnet
    cidr: 10.99.16.0/24
  registries:
    tls: false
YAML
parity::stub "${PROJ}" kind <<'SH'
#!/usr/bin/env bash
# Parity stub (gateway priority): no cluster exists; two nodes, and the HA
# load balancer with PARITY_HA=1.
case "${1:-} ${2:-}" in
  "get nodes")
    printf 'zz-gwprio-control-plane\nzz-gwprio-worker\n'
    if [[ "${PARITY_HA:-0}" == 1 ]]; then echo zz-gwprio-external-load-balancer; fi
    exit 0 ;;
  "create cluster") echo "stub: kind $*"; exit 0 ;;
esac
exit 0
SH
parity::stub "${PROJ}" docker <<'SH'
#!/usr/bin/env bash
# Parity stub (gateway priority): the argv log, the version, the endpoint
# gateways, the members of the registry network and the in-node `ip` reads.
echo "docker $*" >> "${PARITY_DOCKER_LOG}"
case "${1:-} ${2:-}" in
  "version --format") echo "${PARITY_DOCKER_VERSION}"; exit 0 ;;
  "network inspect")
    case "$*" in
      *".Gateway"*)    echo " "; exit 0 ;;
      *".Containers"*)
        if [[ "${3}" == lok8s-registries ]]; then
          printf 'other-control-plane lok8s-registry-zz-foreign '
          if [[ "${PARITY_ATTACHED:-0}" == 1 ]]; then
            printf 'zz-gwprio-control-plane zz-gwprio-worker '
            if [[ "${PARITY_HA:-0}" == 1 ]]; then printf 'zz-gwprio-external-load-balancer '; fi
          fi
          echo
          exit 0
        fi ;;
    esac
    exit 1 ;;
  "exec zz-gwprio-external-load-balancer")
    echo 'OCI runtime exec failed: exec: "ip": executable file not found in $PATH' >&2
    exit 127 ;;
  "exec "*)
    case "$*" in
      *"ip -4 route show default") echo "default via 10.125.200.128 dev eth1"; exit 0 ;;
      *"ip -o -4 route get 10.99.16.1") echo "10.99.16.1 dev eth0 src 10.99.16.193 uid 0"; exit 0 ;;
    esac
    exit 0 ;;
  "inspect "*)
    case "$*" in
      *'{{index .Config.Labels "io.x-k8s.kind.role"}}'*'Networks "zz-gwnet"}}{{.Gateway}}{{end}}'*)
        role=worker
        case "${2}" in
          *-external-load-balancer) role=external-load-balancer ;;
          *-control-plane)          role=control-plane ;;
        esac
        gw=10.99.16.1
        if [[ "${PARITY_NO_GATEWAY:-0}" == 1 ]]; then gw=""; fi
        out="${role}|true|${gw}"
        if [[ "$*" == *GwPriority* ]]; then out+="|${PARITY_GW_PRIO:-0}"; fi
        echo "${out}"
        exit 0 ;;
    esac
    exit 1 ;;
esac
exit 0
SH
export PARITY_DOCKER_LOG="${WORK}/docker.log"
gw_pre() {
  reset_fixtures
  rm -f "${PARITY_DOCKER_LOG}" "${PROJ}/clusters/gwprio.dev/.registries.json" "${PROJ}/clusters/gwprio-off.dev/.registries.json"
}
gw_post() {  # <impl>: keep the docker argv per side (normalized)
  if [[ -f "${PARITY_DOCKER_LOG}" ]]; then
    sed -E "s|${PROJ}|PROJ|g" "${PARITY_DOCKER_LOG}" > "${WORK}/docker.${1}.log"
    rm -f "${PARITY_DOCKER_LOG}"
  else
    : > "${WORK}/docker.${1}.log"
  fi
}
gw_check() {  # <label> [domain]: lo provision --domain <domain>, streams and docker argv diffed
  local domain="${2:-gwprio.dev}"
  PARITY_PRE_EACH=gw_pre PARITY_POST_EACH=gw_post check "${KIND_CFG}" provision --domain "${domain}"
  parity::state_same "${WORK}/docker.bash.log" "${WORK}/docker.go.log" "docker argv of lo provision --domain ${domain} (${1})" || failures=$((failures + 1))
}
gw_pin() {  # <label> <file> <want-count> <regex>
  local n
  n=$(grep -cE -- "${4}" "${2}" || true)
  if [[ "${n}" == "${3}" ]]; then echo "ok: ${1}"; else
    echo "FAIL: ${1} — ${n} lines match ${4} in ${2}, want ${3}:"; sed 's/^/  /' "${2}" | head -40
    failures=$((failures + 1))
  fi
}
export PARITY_DOCKER_VERSION="29.6.2 29.6.2"
gw_check "Docker 29"
for side in bash go; do
  gw_pin "${side}: Docker 29: one version read"                 "${WORK}/docker.${side}.log" 1 '^docker version '
  gw_pin "${side}: Docker 29: both nodes connect at priority -1" "${WORK}/docker.${side}.log" 2 '^docker network connect --gw-priority -1 lok8s-registries zz-gwprio-(control-plane|worker)$'
  gw_pin "${side}: Docker 29: no plain connect"                  "${WORK}/docker.${side}.log" 0 '^docker network connect lok8s-registries '
  gw_pin "${side}: Docker 29: the route is not touched"          "${WORK}/docker.${side}.log" 0 '^docker exec .* ip '
done
export PARITY_DOCKER_VERSION="27.5.1 27.5.1"
gw_check "Docker 27"
for side in bash go; do
  gw_pin "${side}: Docker 27: no --gw-priority"                  "${WORK}/docker.${side}.log" 0 'gw-priority'
  gw_pin "${side}: Docker 27: both nodes connect plainly"        "${WORK}/docker.${side}.log" 2 '^docker network connect lok8s-registries zz-gwprio-(control-plane|worker)$'
  gw_pin "${side}: Docker 27: both routes move to the project network" "${WORK}/docker.${side}.log" 2 '^docker exec zz-gwprio-(control-plane|worker) ip route replace default via 10\.99\.16\.1 dev eth0$'
  gw_pin "${side}: Docker 27: the move is reported"              "${WORK}/${side}.err" 2 'zz-gwprio-(control-plane|worker): the default route did not go through zz-gwnet — moved it there \(via 10\.99\.16\.1 dev eth0\)'
  gw_pin "${side}: Docker 27: the gateway comes from each node endpoint" "${WORK}/docker.${side}.log" 2 '^docker inspect zz-gwprio-(control-plane|worker) --format .*\{\{with index \.NetworkSettings\.Networks "zz-gwnet"\}\}\{\{\.Gateway\}\}\{\{end\}\}'
  gw_pin "${side}: Docker 27: GwPriority is not asked"           "${WORK}/docker.${side}.log" 0 'GwPriority'
  gw_pin "${side}: Docker 27: the network gateway is not read"   "${WORK}/docker.${side}.log" 0 '^docker network inspect zz-gwnet .*\.Gateway'
done
export PARITY_NO_GATEWAY=1
gw_check "Docker 27, no gateway on the endpoint"
for side in bash go; do
  gw_pin "${side}: no gateway: the route is not read"            "${WORK}/docker.${side}.log" 0 '^docker exec .* ip '
  gw_pin "${side}: no gateway: one warning per node"             "${WORK}/${side}.err" 2 'zz-gwprio-(control-plane|worker): found no IPv4 gateway for network zz-gwnet \(docker inspect: ""\)\. lo did not examine the default route\.'
done
unset PARITY_NO_GATEWAY
export PARITY_DOCKER_VERSION="29.6.2 29.6.2" PARITY_ATTACHED=1
gw_check "attached without a priority"
for side in bash go; do
  gw_pin "${side}: attached: no connect"                         "${WORK}/docker.${side}.log" 0 '^docker network connect '
  gw_pin "${side}: attached: both routes move to the project network" "${WORK}/docker.${side}.log" 2 '^docker exec zz-gwprio-(control-plane|worker) ip route replace default via 10\.99\.16\.1 dev eth0$'
done
gw_check "shared off, attached under the old shared default" gwprio-off.dev
for side in bash go; do
  gw_pin "${side}: shared off, attached: no connect"             "${WORK}/docker.${side}.log" 0 '^docker network connect '
  gw_pin "${side}: shared off, attached: both routes move to the project network" "${WORK}/docker.${side}.log" 2 '^docker exec zz-gwprio-(control-plane|worker) ip route replace default via 10\.99\.16\.1 dev eth0$'
done
export PARITY_ATTACHED=0
gw_check "shared off, not attached" gwprio-off.dev
for side in bash go; do
  gw_pin "${side}: shared off, not attached: no connect"         "${WORK}/docker.${side}.log" 0 '^docker network connect '
  gw_pin "${side}: shared off, not attached: no node read"       "${WORK}/docker.${side}.log" 0 '^docker (exec .* ip |inspect zz-gwprio-[a-z-]+ --format .*\.Gateway)'
done
export PARITY_HA=1 PARITY_ATTACHED=1 PARITY_DOCKER_VERSION="29.6.2 29.6.2" PARITY_GW_PRIO=-1
gw_check "HA, Docker 29, attached at -1"
for side in bash go; do
  gw_pin "${side}: HA, -1: no docker exec"                       "${WORK}/docker.${side}.log" 0 '^docker exec '
  gw_pin "${side}: HA, -1: one inspect per container"            "${WORK}/docker.${side}.log" 3 '^docker inspect zz-gwprio-(control-plane|worker|external-load-balancer) --format .*GwPriority'
  gw_pin "${side}: HA, -1: no foreign member touched"            "${WORK}/docker.${side}.log" 0 ' (other-control-plane|lok8s-registry-zz-foreign)( |$)'
  gw_pin "${side}: HA, -1: no warning"                           "${WORK}/${side}.err" 0 'lo: zz-gwprio-'
done
export PARITY_DOCKER_VERSION="27.5.1 27.5.1"
unset PARITY_GW_PRIO
gw_check "HA, Docker 27, attached at 0"
for side in bash go; do
  gw_pin "${side}: HA, 27: the load balancer is not exec'd"      "${WORK}/docker.${side}.log" 0 '^docker exec zz-gwprio-external-load-balancer '
  gw_pin "${side}: HA, 27: both node routes move"                "${WORK}/docker.${side}.log" 2 '^docker exec zz-gwprio-(control-plane|worker) ip route replace default via 10\.99\.16\.1 dev eth0$'
  gw_pin "${side}: HA, 27: no warning names the load balancer"   "${WORK}/${side}.err" 0 'external-load-balancer'
done
unset PARITY_DOCKER_LOG PARITY_DOCKER_VERSION PARITY_ATTACHED PARITY_HA

report
