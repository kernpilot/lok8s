#!/usr/bin/env bash
# parity-ops.sh — differential test between the Go lo and the argsh lo for
# the cluster-touching ops surface: `lo deploy`, `lo recover`, `lo gitops`.
#
# Modeled on parity-test.sh: for every covered invocation, runs BOTH
# implementations (the Go binary, and the same binary routed to the frozen tree
# by the project file) against a synthetic project and diffs
# stdout, stderr, and exit codes. Absolute project paths are normalized to
# PROJ.
#
# CLUSTER-FREE PATHS ONLY. deploy/recover mutate real clusters and cloud
# nodes — none of which a parity harness may touch. The synthetic project
# therefore gets its OWN .bin with a STUB kubectl (both implementations
# resolve tools through that directory first) and its own .lok8s/providers
# with a scripted `mock` provider, so `lo deploy` only ever reaches the stub
# and `lo recover` only ever reaches the mock — and the mock's rebuild
# refuses to run outside CLOUD_DRY_RUN. Consent paths are driven with a
# piped "no" / closed stdin; provision is never reached.
#
# Usage: hack/parity-ops.sh [path-to-go-lo]   (default: bin/lo)
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
# Creds/CI-ish toggles would change what a child sees (parity::init sets
# KUBECONFIG to a missing file).
unset KUSTOMIZE_PLUGIN_HOME LOK8S_NONINTERACTIVE LOK8S_FORCE_RECREATE LOK8S_REMOTE \
  CLOUD_DRY_RUN CLOUD_DRY_RUN_PATH HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD \
  KAPPLY_TTY KAPPLY_POLL_INTERVAL SOURCE_DATE_EPOCH CI

# ── synthetic project ────────────────────────────────────────────────────────
# .lok8s is a COPY (a routed tree and its `lo` must resolve inside the
# project; a linked entrypoint is refused) so `providers/` can carry the
# mock; .bin is a dir of links so kubectl can be the stub.
parity::new_project "${PROJ}"
parity::own_bin "${PROJ}"
parity::stub "${PROJ}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub kubectl: no live cluster may be reached.
#   apply  — consume the manifest; succeed (server-side verbs per kind) unless
#            PARITY_KUBECTL_APPLY_RC is set, then fail with that status.
#   wait   — "condition met"
#   get    — a ready snapshot for the scoped wait-ready poll
case "${*}" in
  *" apply "*|apply*)
    m="$(cat)"
    if [[ -n "${PARITY_KUBECTL_APPLY_RC:-}" ]]; then
      echo "error: parity stub refused the apply" >&2
      exit "${PARITY_KUBECTL_APPLY_RC}"
    fi
    grep -E '^kind:' <<<"${m}" | sed 's/^kind: */parity-stub\//; s/$/ serverside-applied/'
    exit 0 ;;
  *" wait "*|wait*)
    echo "condition met"; exit 0 ;;
  *" get "*|get*)
    printf '{"items":[{"kind":"Deployment","metadata":{"namespace":"default","name":"test-app"},"spec":{"replicas":1},"status":{"availableReplicas":1}}]}'
    exit 0 ;;
esac
exit 0
SH

mkdir -p "${PROJ}/.lok8s/providers/mock"
cat > "${PROJ}/.lok8s/providers/mock/main" <<'SH'
#!/usr/bin/env argsh
# mock — parity-harness provider: the contract as no-ops plus a scripted
# rebuild/doctor/output, so `lo recover` can be driven up to consent and
# through --dry-run without any cloud or cluster. rebuild REFUSES outside
# CLOUD_DRY_RUN.
provider::validate() { return 0; }
provider::credential_data() { return 0; }
provider::provision() { echo "mock provision must not run" >&2; return 1; }
provider::destroy() { echo "mock destroy must not run" >&2; return 1; }
provider::output() { printf '{"nodes":[{"name":"n1"},{"name":"n2"}]}\n'; }
provider::rebuild() {
  echo "mock rebuild: dry=${CLOUD_DRY_RUN:-} wd=${2##*/}"
  [[ -n "${CLOUD_DRY_RUN:-}" ]] || { echo "mock rebuild must not run for real" >&2; return 1; }
  return 0
}
provider::doctor() {
  printf 'ok\tmock API reachable\n'
  printf 'warn\tmock creds unset\n'
  printf 'summary\t1 ok, 1 warn\n'
  return 0
}
SH
# nodoc — a provider WITHOUT the doctor hook (the "no doctor hook" branch).
mkdir -p "${PROJ}/.lok8s/providers/nodoc"
sed '/^provider::doctor()/,/^}/d' "${PROJ}/.lok8s/providers/mock/main" > "${PROJ}/.lok8s/providers/nodoc/main"
# norebuild — the required contract only (recover must decline).
mkdir -p "${PROJ}/.lok8s/providers/norebuild"
sed '/^provider::rebuild()/,/^}/d' "${PROJ}/.lok8s/providers/nodoc/main" > "${PROJ}/.lok8s/providers/norebuild/main"

mkdir -p "${PROJ}"/clusters/{alpha.dev,beta.cloud,gamma.app,delta.app,eps.app,nosuch.cloud,mock.cloud,nodoc.cloud,norebuild.cloud}
printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${PROJ}/clusters/alpha.dev/cluster.lok8s.yaml"
printf 'kind: KubeOne\nmetadata:\n  name: beta\nspec:\n  provider:\n    name: "bad name!"\n' > "${PROJ}/clusters/beta.cloud/cluster.lok8s.yaml"
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${PROJ}/clusters/gamma.app/deploy.lok8s.yaml"
printf 'kind: Deploy\nspec: {}\n' > "${PROJ}/clusters/delta.app/deploy.lok8s.yaml"
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: nowhere.cloud\n' > "${PROJ}/clusters/eps.app/deploy.lok8s.yaml"
printf 'kind: KubeOne\nmetadata:\n  name: nosuch\nspec:\n  provider:\n    name: nosuch\n    config:\n      cluster_name: nosuchy\n' > "${PROJ}/clusters/nosuch.cloud/cluster.lok8s.yaml"
for d in mock nodoc norebuild; do
  printf 'kind: KubeOne\nmetadata:\n  name: %s\nspec:\n  provider:\n    name: %s\n    configRef: provider.yaml\n' "${d}" "${d}" \
    > "${PROJ}/clusters/${d}.cloud/cluster.lok8s.yaml"
  printf 'cluster_name: %sy\n' "${d}" > "${PROJ}/clusters/${d}.cloud/provider.yaml"
done
# twin-a.cloud and twin-b.cloud: one domain copied from the other with the
# same cluster_name (recover refuses both).
for d in twin-a twin-b; do
  mkdir -p "${PROJ}/clusters/${d}.cloud"
  printf 'kind: KubeOne\nmetadata:\n  name: %s\nspec:\n  provider:\n    name: mock\n    configRef: provider.yaml\n' "${d}" \
    > "${PROJ}/clusters/${d}.cloud/cluster.lok8s.yaml"
  printf 'cluster_name: twiny\n' > "${PROJ}/clusters/${d}.cloud/provider.yaml"
done
# Provider configs whose cluster_name recover cannot compare: a variable (the
# rebuild expands it), a config that parses only after the expansion (a
# variable outside quotes), a config that does not parse, a backslash (a
# message cannot show it as it is), a merge key, a number, and a variable
# outside the name that adds one when the rebuild expands it. Without the
# refusal, the mock rebuild would run (and fail): refused_with checks the
# refusal text and that it did not run.
for d in tmpl unparsed broken bslash merge quote expand; do
  mkdir -p "${PROJ}/clusters/${d}.cloud"
  printf 'kind: KubeOne\nmetadata:\n  name: %s\nspec:\n  provider:\n    name: mock\n    configRef: provider.yaml\n' "${d}" \
    > "${PROJ}/clusters/${d}.cloud/cluster.lok8s.yaml"
done
# shellcheck disable=SC2016  # the config holds ${...} as text: the guard must see the variable
printf 'cluster_name: ${NAME}-a\n' > "${PROJ}/clusters/tmpl.cloud/provider.yaml"
# shellcheck disable=SC2016  # the config holds ${...} as text: the guard must see the variable
printf '{"cluster_name": "twiny", "token": ${LOK8S_PARITY_TOKEN}}\n' > "${PROJ}/clusters/unparsed.cloud/provider.yaml"
printf '{"cluster_name": "twiny", "token": [}\n' > "${PROJ}/clusters/broken.cloud/provider.yaml"
printf 'cluster_name: "a\\\\b"\n' > "${PROJ}/clusters/bslash.cloud/provider.yaml"
# merge.cloud gets its provider config (a merge key at the top) after the
# last row that scans the other domains: the scan of every other domain stops
# on it from then on (the rows at the end).
printf 'cluster_name: 0123\n' > "${PROJ}/clusters/quote.cloud/provider.yaml"
# shellcheck disable=SC2016  # the config holds ${...} as text: the guard must see the variable
printf '{"x": 1, "note": "${LOK8S_PARITY_NOTE}"}\n' > "${PROJ}/clusters/expand.cloud/provider.yaml"
echo alpha.dev > "${PROJ}/clusters/.active"

# The deploy artifact: one CRD, one labelled Namespace (with an UNQUOTED
# boolean label — yq compares scalar text), one labelled Deployment.
artifact_full() {
  cat > "${PROJ}/clusters/alpha.dev/artifacts.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.test.lok8s.dev
---
apiVersion: v1
kind: Namespace
metadata:
  name: networking
  labels:
    lok8s.dev/type: system
    flag: true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
  labels:
    lok8s.dev/type: platform
YAML
}
artifact_empty() { printf '# just a comment\n---\n' > "${PROJ}/clusters/alpha.dev/artifacts.yaml"; }
# The full artifact plus a Secret with the keys-only marker (a quoted key),
# and an artifact that does not parse as YAML: lo deploy refuses both.
artifact_keys_only() {
  artifact_full
  printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: default\n  labels:\n    lok8s.dev/type: platform\n  annotations:\n    "secrets.lok8s.dev/keys-only": "true"\ndata:\n  TOKEN: ""\n' \
    >> "${PROJ}/clusters/alpha.dev/artifacts.yaml"
}
artifact_unparsed() { printf 'apiVersion: v1\nkind: ConfigMap\ndata: [x\n' > "${PROJ}/clusters/alpha.dev/artifacts.yaml"; }
artifact_none()  { rm -f "${PROJ}/clusters/alpha.dev/artifacts.yaml"; }

# check <stdin|-> <argv...> — run both impls in the shared project, diff.
# The recover phase timer prints wall-clock seconds; a run that straddles a
# second boundary differs by one — normalize before diffing. argsh exits 2 on
# its own parse errors ("Error: too many arguments: …") where the Go binary
# exits 1 with the identical message (cli.argshErrorf): tolerated when the
# bash stderr carries that line (PARITY_PARSE_RC=auto).
parity_normalize() {
  sed -i -E 's/took [0-9]+m [0-9]+s/took Nm Ns/; s/DONE in [0-9]+m [0-9]+s/DONE in Nm Ns/; s/=[0-9]+m[0-9]+s/=NmNs/g' "$@"
}
check() {
  local stdin="${1}"; shift
  [[ "${stdin}" != "-" ]] || stdin=""
  PARITY_STDIN="${stdin}" parity::run_pair "$@"
  PARITY_PARSE_RC=auto parity::finish "lo $*"
}

# ── lo deploy ────────────────────────────────────────────────────────────────
# Pin kapply's display to the OFF-tty passthrough on both sides: bash decides
# "tty" by `-w /dev/tty` (a permission test that passes without a controlling
# terminal, then sinks the UI to /dev/null because stdout is a pipe) while the
# Go port actually opens /dev/tty — the two disagree exactly in a harness like
# this one. CI/Tilt logs are the off-tty contract anyway.
export LOK8S_NONINTERACTIVE=1
# Usage / label guards (no artifact needed, nothing applied).
artifact_none
check - deploy extra
check - deploy -l foo
check - deploy -l =v
check - deploy -l foo=
check - deploy -l 'a;b=c'                      # selector validation (before the artifact check)
# Deploy-domain routing (kubeconfig pass A) — before the label is looked at.
check - deploy --domain delta.app              # no clusterRef
check - deploy --domain eps.app                # dangling clusterRef
check - deploy --domain gamma.app              # resolves → no artifact
check - deploy --domain gamma.app -l foo       # kubeconfig resolution outranks the label guard
# No artifact (build not run) — both routes.
check - deploy --domain alpha.dev
check - d --domain alpha.dev --label lok8s.dev/type=system
# An artifact with no objects: graceful no-op (debug under -v).
artifact_empty
check - deploy --domain alpha.dev
check - deploy -v --domain alpha.dev
check - deploy --domain alpha.dev -l lok8s.dev/type=system
# A real artifact against the stub kubectl.
artifact_full
check - deploy --domain alpha.dev                          # CRD phase + wait + full apply + wait-ready
check - deploy -v --domain alpha.dev
check - deploy --domain alpha.dev -l lok8s.dev/type=nothing   # no match → warn, rc 0
check - deploy --domain alpha.dev -l lok8s.dev/type=platform  # subset (no CRD phase)
check - deploy -v --domain alpha.dev --label=flag=true        # unquoted scalar label matches
check - deploy --domain alpha.dev -l app=x extra
# Filtered: a failing kubectl apply is logged and the sequence continues (rc 0).
export PARITY_KUBECTL_APPLY_RC=1
check - deploy --domain alpha.dev -l lok8s.dev/type=platform
unset PARITY_KUBECTL_APPLY_RC
# An artifact with the keys-only marker, and one that does not parse: both
# routes refuse before kubectl runs (the stub kubectl prints its verbs).
for artifact in artifact_keys_only artifact_unparsed; do
  "${artifact}"
  check - deploy --domain alpha.dev
  grep -qF "[error] refusing to apply alpha.dev: PROJ/clusters/alpha.dev/artifacts.yaml" "${WORK}/go.err" \
    || fail "deploy (${artifact}): no refusal on stderr"
  ! grep -qE 'serverside-applied|condition met' "${WORK}/go.out" "${WORK}/go.err" || fail "deploy (${artifact}): kubectl ran"
  check - deploy --domain alpha.dev -l lok8s.dev/type=platform
  grep -qF "[error] refusing to apply alpha.dev: PROJ/clusters/alpha.dev/artifacts.yaml" "${WORK}/go.err" \
    || fail "deploy -l (${artifact}): no refusal on stderr"
  ! grep -qE 'serverside-applied|condition met' "${WORK}/go.out" "${WORK}/go.err" || fail "deploy -l (${artifact}): kubectl ran"
done
unset LOK8S_NONINTERACTIVE   # recover below treats it as CONSENT

# ── lo gitops ────────────────────────────────────────────────────────────────
# (the bare `lo gitops` / `-h` usage text is cobra's vs argsh's — the same
# documented divergence every ported command group carries — not diffed.)
check - gitops flux
check - gitops argo
check - g f
check - g a
check - gitops flux --domain alpha.dev
check - gitops --domain alpha.dev flux
check - gitops flux extra

# ── lo recover ───────────────────────────────────────────────────────────────
check - recover a b
check - recover mock.cloud extra1 extra2
check - recover nope.dom                       # resolve: no spec
check - recover ../evil                        # resolve: invalid domain (raw error family)
check - recover gamma.app                      # deploy domain refused
check - recover alpha.dev                      # no spec.provider
check - recover beta.cloud                     # invalid provider name: read_name's diagnostic surfaces
check - recover nosuch.cloud                   # provider not found (bash provider::load's error)
check - recover norebuild.cloud                # provider without provider::rebuild
# Consent refusal: "no", a closed stdin, whitespace — nothing destructive.
check no recover mock.cloud
check - recover mock.cloud
check "   " recover mock.cloud
check no recover mock.cloud --skip-rebuild     # re-provision wording
check no recover nodoc.cloud                   # "no doctor hook" branch, then the prompt
# A cluster_name that another domain also has: refused at resolve, before the
# doctor, the dry run and the consent. A "yes", --force, --dry-run and
# LOK8S_NONINTERACTIVE do not skip it. Both implementations could pass the
# diff and still run the rebuild, so check the refusal and that no rebuild ran.
refused_shared() {
  grep -qF "[error] recover: cluster_name 'twiny' of '${1}' is also the cluster_name of: ${2}. " "${WORK}/go.err" \
    || fail "recover ${1}: no refusal of the shared cluster_name on stderr"
  ! grep -qF "mock rebuild" "${WORK}/go.out" "${WORK}/go.err" || fail "recover ${1}: the rebuild ran"
}
check yes recover twin-a.cloud
refused_shared twin-a.cloud twin-b.cloud
check - recover --force twin-a.cloud
refused_shared twin-a.cloud twin-b.cloud
check - recover --dry-run twin-b.cloud
refused_shared twin-b.cloud twin-a.cloud
check - recover --force --skip-rebuild twin-b.cloud
refused_shared twin-b.cloud twin-a.cloud
export LOK8S_NONINTERACTIVE=1
check - recover twin-a.cloud
refused_shared twin-a.cloud twin-b.cloud
unset LOK8S_NONINTERACTIVE
# A cluster_name that recover cannot compare: refused before it compares.
# refused_with <domain> <refusal text>
refused_with() {
  grep -qF "${2}" "${WORK}/go.err" || fail "recover ${1}: no refusal on stderr: ${2}"
  ! grep -qF "is also the cluster_name of" "${WORK}/go.err" || fail "recover ${1}: the guard compared the name"
  ! grep -qF "mock rebuild" "${WORK}/go.out" "${WORK}/go.err" || fail "recover ${1}: the rebuild ran"
}
check - recover --force tmpl.cloud
refused_with tmpl.cloud "[error] recover: the cluster_name '\${NAME}-a' of 'tmpl.cloud' has a '\$'. The rebuild expands the variables in the provider config"
# The variable is unset: the expansion gives `"token": }`, which parses.
unset LOK8S_PARITY_TOKEN
check - recover --force unparsed.cloud
refused_with unparsed.cloud "[error] recover: the provider config of 'unparsed.cloud' parses only after its variables are expanded."
check - recover --force broken.cloud
refused_with broken.cloud "[error] recover: the provider config of 'broken.cloud' is not one YAML or JSON document whose top is a mapping"
check - recover --force bslash.cloud
refused_with bslash.cloud "[error] recover: the provider config of 'bslash.cloud' names a cluster_name with a character that a message cannot show as it is"
check - recover --force quote.cloud
refused_with quote.cloud "[error] recover: the cluster_name of the provider config of 'quote.cloud' is not a text"
# The variable adds the cluster_name of the twins when the rebuild expands it:
# both implementations expand with the envsubst on PATH, in the shell of the
# provider.
export LOK8S_PARITY_NOTE='a", "cluster_name": "twiny'
check - recover --force expand.cloud
refused_with expand.cloud "[error] recover: the provider config of 'expand.cloud' gives another cluster_name when the rebuild expands its variables"
unset LOK8S_PARITY_NOTE
# --dry-run: the mock rebuild runs under CLOUD_DRY_RUN; provision never.
check - recover --dry-run mock.cloud
check - recover -v --dry-run mock.cloud
check - recover --domain mock.cloud --dry-run
check "  yes  " recover mock.cloud --skip-rebuild --dry-run   # dry-run never prompts
# A merge key: recover refuses it in its own provider config, and the scan of
# the other domains stops on it, so a twin is refused with the domain of the
# merge key before the names are compared.
printf 'base: &b {cluster_name: mergy}\n<<: *b\n' > "${PROJ}/clusters/merge.cloud/provider.yaml"
check - recover --force merge.cloud
refused_with merge.cloud "[error] recover: the provider config of 'merge.cloud' has a merge key (<<) at the top."
check - recover --force twin-a.cloud
refused_with twin-a.cloud "[error] recover: a domain has a merge key (<<) on the path to the cluster_name of its provider config: merge.cloud. The provider can take the cluster_name from the merged mapping, so recover cannot compare it with the cluster_name 'twiny' of 'twin-a.cloud'. Nothing was changed."
# The work dir both implementations create.
for impl_dir in mock.cloud nope.dom; do
  if [[ -d "${PROJ}/clusters/${impl_dir}/.provider" ]]; then
    echo "ok: workdir clusters/${impl_dir}/.provider"
  else
    fail "workdir clusters/${impl_dir}/.provider missing"
  fi
done

report
