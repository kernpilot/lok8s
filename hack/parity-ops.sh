#!/usr/bin/env bash
# parity-ops.sh — differential test between the Go lo and the argsh lo for
# the cluster-touching ops surface: `lo deploy`, `lo recover`, `lo gitops`.
#
# Modeled on parity-test.sh: for every covered invocation, runs BOTH
# implementations (the Go binary, and the same binary with LO_IMPL=bash
# forcing the argsh passthrough) against a synthetic project and diffs
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
  KAPPLY_TTY KAPPLY_POLL_INTERVAL SOURCE_DATE_EPOCH CI

# Pin the C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists in byte order (= C collation).
export LC_ALL=C

PROJ="${WORK}/proj"

# ── synthetic project ────────────────────────────────────────────────────────
# .lok8s is a REAL dir of symlinks so `providers/` can carry the mock; .bin
# likewise so kubectl can be the stub.
mkdir -p "${PROJ}/clusters" "${PROJ}/.lok8s/providers" "${PROJ}/.bin"
for entry in "${ROOT}"/.lok8s/* "${ROOT}"/.lok8s/.[!.]*; do
  [[ -e "${entry}" ]] || continue
  name="$(basename "${entry}")"
  [[ "${name}" == "providers" ]] && continue
  ln -s "${entry}" "${PROJ}/.lok8s/${name}"
done
for entry in "${ROOT}"/.lok8s/providers/*; do
  ln -s "${entry}" "${PROJ}/.lok8s/providers/$(basename "${entry}")"
done
for entry in "${ROOT}"/.bin/*; do
  ln -s "${entry}" "${PROJ}/.bin/$(basename "${entry}")"
done
rm -f "${PROJ}/.bin/kubectl"
cat > "${PROJ}/.bin/kubectl" <<'SH'
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
chmod +x "${PROJ}/.bin/kubectl"

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
artifact_none()  { rm -f "${PROJ}/clusters/alpha.dev/artifacts.yaml"; }

failures=0

# check <stdin|-> <argv...> — run both impls in the shared project, diff.
check() {
  local stdin="${1}"; shift
  local go_rc=0 bash_rc=0
  if [[ "${stdin}" == "-" ]]; then
    (cd "${PROJ}" && "${LO_BIN}" "$@" </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
    (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" </dev/null >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  else
    (cd "${PROJ}" && "${LO_BIN}" "$@" <<<"${stdin}" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
    (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" <<<"${stdin}" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  fi
  sed -i "s|${PROJ}|PROJ|g" "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"

  local ok=1
  if (( go_rc != bash_rc )); then
    # The one documented rc divergence: argsh exits 2 on its own parse
    # errors ("Error: too many arguments: …"), the Go binary exits 1 with the
    # identical message (cli.argshErrorf; same as every ported command).
    if (( bash_rc == 2 && go_rc == 1 )) && grep -q '^Error: ' "${WORK}/bash.err"; then
      :
    else
      echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
      ok=0
    fi
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
    echo "ok: lo $*"
  else
    failures=$((failures + 1))
  fi
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
# --dry-run: the mock rebuild runs under CLOUD_DRY_RUN; provision never.
check - recover --dry-run mock.cloud
check - recover -v --dry-run mock.cloud
check - recover --domain mock.cloud --dry-run
check "  yes  " recover mock.cloud --skip-rebuild --dry-run   # dry-run never prompts
# The work dir both implementations create.
for impl_dir in mock.cloud nope.dom; do
  [[ -d "${PROJ}/clusters/${impl_dir}/.provider" ]] && echo "ok: workdir clusters/${impl_dir}/.provider" \
    || { echo "FAIL: workdir clusters/${impl_dir}/.provider missing"; failures=$((failures + 1)); }
done

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
