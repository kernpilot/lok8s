#!/usr/bin/env bash
# parity-operator.sh — differential test between the Go operator hooks
# (`lo operator <hook>`, exec'd by the operator/hooks/*.sh shims) and the
# frozen bash hooks (.lok8s/legacy/operator/hooks/).
#
# Two surfaces, both hermetic (no cluster, no docker, no network):
#   1. `--config` — byte-identical binding configuration, bash vs Go, and
#      the shim → Go path, plus the argument/exit-code contract (a trigger
#      without BINDING_CONTEXT_PATH exits 1 in both).
#   2. trigger runs against a STUBBED kubectl/clusterctl on PATH: the stubs
#      log every argv (+ the stdin of every `-f -` apply) and answer fixed
#      replies; the logs are diffed bash vs Go — the same KLOG oracle
#      tests/operator/hooks_test.bats asserts on. Cases stop short of
#      anything that reaches the lo driver (kind/docker): the bash hook has
#      no driver in the repo layout and the Go one would run the real
#      driver.
#
# Usage: hack/parity-operator.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }
for tool in jq yq; do
  command -v "${tool}" >/dev/null 2>&1 || { echo "error: ${tool} required (the bash hooks use it)" >&2; exit 2; }
done

WORK="$(mktemp -d)"
# PARITY_KEEP=1 leaves the work dir behind for a post-mortem.
if [[ -z "${PARITY_KEEP:-}" ]]; then
  trap 'rm -rf "${WORK}"' EXIT
else
  echo "work dir: ${WORK}"
fi

# The harness must run against its synthetic layout ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they would redirect the Go runtime's layout resolution (PATH_LOK8S) and
# the bash libs into that live repo.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_STATE_DIR BINDING_CONTEXT_PATH \
  KUBECONFIG DEBUG LOK8S_NONINTERACTIVE
export LC_ALL=C

# Hook tree: the frozen bash hooks + the CAPI templates where both
# implementations look for them (${HOOK_DIR}/capi-templates for bash,
# ${PATH_LOK8S}/capi-templates for Go). No lib/ tree: the bash `declare -f`
# probes (gitops::bootstrap, deploy::apply) then fail, and the Go seams
# are wired — the one place the two are NOT comparable, so the
# post-provision gitops/deploy tail is asserted on the Go side only.
HOOKS="${WORK}/hooks"
mkdir -p "${HOOKS}"
cp "${ROOT}"/.lok8s/legacy/operator/hooks/*.sh "${HOOKS}/"
cp -R "${ROOT}/.lok8s/drivers/capi/cluster" "${HOOKS}/capi-templates"

# Stubs on PATH — kubectl/clusterctl log to ${KLOG}; the Go binary resolves
# tools via PATH (no PATH_BIN, and the state dir carries no .bin).
STUBS="${WORK}/stubs"
mkdir -p "${STUBS}"
cat > "${STUBS}/kubectl" <<'EOF'
#!/usr/bin/env bash
# One log entry per call, appended in ONE write after stdin is drained: the
# bash `kubectl create … | kubectl apply -f -` is a pipeline, so the apply
# stub runs concurrently with the create stub — logging the apply's line
# before its stdin block would interleave with the create's line and make
# the order run-dependent. Draining first means the apply entry always
# lands after the create's (its stdin closes when the create exits).
entry="kubectl $*"
case "$*" in
  *'-f -'*) entry+=$'\n--- stdin\n'"$(cat)"$'\n--- end' ;;
esac
printf '%s\n' "${entry}" >> "${KLOG}"
case "$*" in
  *'jsonpath={.metadata.finalizers}'*) echo '["lok8s.dev/lo-teardown","lok8s.dev/capi-teardown","keep"]' ;;
  'get lo -A -o json'*|'get capi -A -o json'*) echo '{"items":[]}' ;;
  *'jsonpath={.spec.gitops.provider}'*) printf '%s' "${STUB_GITOPS:-}" ;;
  *'jsonpath={.spec.cluster.domain}'*) printf '%s' "${STUB_DOMAIN:-}" ;;
  'create secret'*) cat >/dev/null 2>&1 || true; echo 'kind: Secret' ;;
  'delete cluster.cluster.x-k8s.io'*) [[ -z "${STUB_DELETE_FAIL:-}" ]] || exit 1 ;;
  'patch'*) [[ -z "${STUB_PATCH_FAIL:-}" ]] || exit 1 ;;
esac
exit 0
EOF
cat > "${STUBS}/clusterctl" <<'EOF'
#!/usr/bin/env bash
echo "clusterctl $*" >> "${KLOG}"
printf 'apiVersion: v1\nkind: Config\n'
EOF
chmod +x "${STUBS}"/*
export PATH="${STUBS}:${PATH}"

failures=0

# Lines allowed to differ on stderr: the bash `set -u` abort names the
# script line; a bash `command not found` for a lib function the Go side
# has wired (see the lib/ note above).
ALLOW_ERR='BINDING_CONTEXT_PATH|Could not open file|parse error|command not found|GitOps bootstrap failed|lo gitops is being redesigned'

# run_hook <impl> <hook> <binding-file-or-> [args...] — runs one
# implementation with its own state dir + KLOG; outputs land in
# ${WORK}/<impl>.{out,err,rc,klog}.
run_hook() {
  local impl="${1}" hook="${2}" binding="${3}"; shift 3
  local state="${WORK}/state-${impl}"
  rm -rf "${state}"; mkdir -p "${state}"
  : > "${WORK}/${impl}.klog"
  local rc=0
  if [[ "${binding}" == "-" ]]; then
    unset BINDING_CONTEXT_PATH
  else
    export BINDING_CONTEXT_PATH="${binding}"
  fi
  if [[ "${impl}" == "bash" ]]; then
    (KLOG="${WORK}/bash.klog" LOK8S_STATE_DIR="${state}" \
      bash "${HOOKS}/${hook}.sh" "$@" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || rc=$?
  else
    (KLOG="${WORK}/go.klog" LOK8S_STATE_DIR="${state}" PATH_LOK8S="${HOOKS}" \
      "${LO_BIN}" operator "${hook}" "$@" >"${WORK}/go.out" 2>"${WORK}/go.err") || rc=$?
  fi
  unset BINDING_CONTEXT_PATH
  echo "${rc}" > "${WORK}/${impl}.rc"
  sed -i "s|${state}|STATE|g; s|${HOOKS}|HOOKS|g; s|${WORK}|WORK|g" "${WORK}/${impl}.out" "${WORK}/${impl}.err" "${WORK}/${impl}.klog"
}

# check <label> <hook> <binding-file-or-> [args...] — both implementations,
# diff rc, stdout, stderr (minus ALLOW_ERR) and the stub call log.
check() {
  local label="${1}" hook="${2}" binding="${3}"; shift 3
  run_hook bash "${hook}" "${binding}" "$@"
  run_hook go "${hook}" "${binding}" "$@"
  local ok=1
  if [[ "$(cat "${WORK}/bash.rc")" != "$(cat "${WORK}/go.rc")" ]]; then
    echo "FAIL: ${hook} ${label} — rc: bash=$(cat "${WORK}/bash.rc") go=$(cat "${WORK}/go.rc")"
    ok=0
  fi
  local diff_out
  diff_out="$(diff "${WORK}/bash.out" "${WORK}/go.out" || true)"
  if [[ -n "${diff_out}" ]]; then
    echo "FAIL: ${hook} ${label} — stdout differs:"; echo "${diff_out}" | head -20 | sed 's/^/  /'; ok=0
  fi
  diff_out="$(diff <(grep -vE "${ALLOW_ERR}" "${WORK}/bash.err") <(grep -vE "${ALLOW_ERR}" "${WORK}/go.err") || true)"
  if [[ -n "${diff_out}" ]]; then
    echo "FAIL: ${hook} ${label} — stderr differs:"; echo "${diff_out}" | head -20 | sed 's/^/  /'; ok=0
  fi
  diff_out="$(diff "${WORK}/bash.klog" "${WORK}/go.klog" || true)"
  if [[ -n "${diff_out}" ]]; then
    echo "FAIL: ${hook} ${label} — kubectl/clusterctl call log differs:"; echo "${diff_out}" | head -40 | sed 's/^/  /'; ok=0
  fi
  if (( ok )); then
    echo "ok: ${hook} ${label} (rc $(cat "${WORK}/go.rc"), $(grep -c '^kubectl\|^clusterctl' "${WORK}/go.klog" || true) calls)"
  else
    failures=$((failures + 1))
  fi
}

# expect_klog <sub> — the Go call log of the LAST check must contain <sub>
# (guards a green diff that would be green because BOTH sides did nothing).
expect_klog() {
  if grep -qF -- "${1}" "${WORK}/go.klog"; then
    echo "ok:   klog has $(printf '%q' "${1}")"
  else
    echo "FAIL: klog lacks $(printf '%q' "${1}")"; failures=$((failures + 1))
  fi
}

binding() { # binding <name> <json> → path
  printf '%s\n' "${2}" > "${WORK}/${1}.json"; echo "${WORK}/${1}.json"
}

# ── --config: bash vs Go, and the shim path ───────────────────────────────
for hook in lo-reconcile capi-reconcile capi-status-sync; do
  check "--config" "${hook}" - --config
  check "--config extra-args-ignored" "${hook}" - --config extra
  # shim → Go: the shim on PATH must find THIS lo binary
  shim_out="$(cd "${WORK}" && PATH="$(dirname "${LO_BIN}"):${PATH}" LOK8S_STATE_DIR="${WORK}/state-go" \
    "${ROOT}/operator/hooks/${hook}.sh" --config)"
  if [[ "${shim_out}" == "$(cat "${WORK}/go.out")" ]]; then
    echo "ok: ${hook} shim --config == lo operator ${hook} --config"
  else
    echo "FAIL: ${hook} shim --config differs from the Go binary"; failures=$((failures + 1))
  fi
  # trigger without a binding context: exit 1 in both (message allowed to differ)
  check "no BINDING_CONTEXT_PATH" "${hook}" -
  check "unreadable BINDING_CONTEXT_PATH" "${hook}" "${WORK}/does-not-exist.json"
  check "empty batch" "${hook}" "$(binding empty '[]')"
done

# ── lo-reconcile triggers (short of the driver) ───────────────────────────
check "schedule re-list" lo-reconcile "$(binding lo-sched '[{"type":"Schedule","binding":"lo-drift"}]')"
expect_klog "kubectl get lo -A -o json"
check "synchronization re-list" lo-reconcile "$(binding lo-sync '[{"type":"Synchronization"}]')"
check "missing domain" lo-reconcile "$(binding lo-nodomain '[{"type":"Event","object":{"metadata":{"name":"bad","namespace":"ns"},"spec":{}}}]')"
expect_klog 'reason":"MissingDomain'
export STUB_PATCH_FAIL=1
check "missing domain, patch fails (warn line)" lo-reconcile "$(binding lo-nodomain2 '[{"object":{"metadata":{"name":"bad"},"spec":{}}}]')"
unset STUB_PATCH_FAIL
check "deletion without our finalizer" lo-reconcile "$(binding lo-del '[{"type":"Event","object":{"metadata":{"name":"x","namespace":"ns","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["other"]},"spec":{"cluster":{"domain":"x.lok8s.dev"}}}}]')"

# ── capi-reconcile triggers ───────────────────────────────────────────────
check "schedule re-list" capi-reconcile "$(binding capi-sched '[{"type":"Schedule","binding":"capi-drift"}]')"
expect_klog "kubectl get capi -A -o json"
check "unknown provider" capi-reconcile "$(binding capi-gcp '[{"object":{"metadata":{"name":"gcp","namespace":"default","finalizers":[]},"spec":{"cluster":{"domain":"gcp.lok8s.dev"}}}}]')"
expect_klog 'reason":"UnknownProvider'
CAPI_CR='{"metadata":{"name":"prod","namespace":"default","finalizers":["lok8s.dev/capi-teardown"]},"spec":{"cluster":{"domain":"prod.example.com","namespace":"clusters"},"kubernetes":{"version":"v1.31.10"},"controlPlane":{"replicas":3},"hcloud":{"region":"fsn1","sshKeyName":"my-key"},"workers":{"zeta":{"replicas":2,"type":"cax21"},"alpha":{"type":"cax11"}}}}'
check "hetzner render + apply (full templates)" capi-reconcile "$(binding capi-hetzner "[{\"object\":${CAPI_CR}}]")"
expect_klog "kubectl apply -f -"
expect_klog "name: prod"
check "aws render + apply" capi-reconcile "$(binding capi-aws '[{"object":{"metadata":{"name":"aws1"},"spec":{"cluster":{"domain":"a.example.com"},"aws":{"region":"eu-central-1"},"workers":{"w":{"replicas":1}}}}}]')"
expect_klog 'AWSCluster'
check "parse error in the binding context (jq rc 5)" capi-reconcile "$(binding capi-broken '[')"
check "two CRs in one batch (export leak)" capi-reconcile "$(binding capi-two "[{\"object\":${CAPI_CR}},{\"object\":{\"metadata\":{\"name\":\"second\"},\"spec\":{\"cluster\":{\"domain\":\"s.example.com\"},\"hcloud\":{\"region\":\"nbg1\"}}}}]")"
rm -rf "${HOOKS}/capi-templates"
check "hetzner, templates missing" capi-reconcile "$(binding capi-notmpl "[{\"object\":${CAPI_CR}}]")"
expect_klog 'reason":"GenerationFailed'
cp -R "${ROOT}/.lok8s/drivers/capi/cluster" "${HOOKS}/capi-templates"
CAPI_DEL='{"metadata":{"name":"prod","namespace":"default","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["lok8s.dev/capi-teardown"]},"spec":{"cluster":{"domain":"prod.example.com","namespace":"clusters"}}}'
check "deletion tears down the CAPI Cluster" capi-reconcile "$(binding capi-del "[{\"object\":${CAPI_DEL}}]")"
expect_klog "kubectl delete cluster.cluster.x-k8s.io prod -n clusters --wait=false --ignore-not-found"
expect_klog '{"metadata":{"finalizers":["lok8s.dev/lo-teardown","keep"]}}'
export STUB_DELETE_FAIL=1
check "deletion, Cluster delete fails" capi-reconcile "$(binding capi-delfail "[{\"object\":${CAPI_DEL}}]")"
expect_klog 'reason":"DestroyFailed'
unset STUB_DELETE_FAIL

# ── capi-status-sync triggers ─────────────────────────────────────────────
check "synchronization skipped" capi-status-sync "$(binding cs-sync '[{"type":"Synchronization","objects":[]}]')"
check "pending phase" capi-status-sync "$(binding cs-pending '[{"object":{"metadata":{"name":"prod","namespace":"clusters"}},"filterResult":{"phase":"Pending","controlPlaneReady":false}}]')"
expect_klog '"phase": "Provisioning"'
check "deleting phase, endpoint" capi-status-sync "$(binding cs-del '[{"object":{"metadata":{"name":"prod"}},"filterResult":{"phase":"Deleting","controlPlaneReady":true,"controlPlaneEndpoint":{"host":"10.0.0.1","port":6443}}}]')"
expect_klog '"port": 6443'
check "null status" capi-status-sync "$(binding cs-null '[{"object":{"metadata":{"name":"n"}},"filterResult":null}]')"
export STUB_PATCH_FAIL=1
check "CR missing (patch fails)" capi-status-sync "$(binding cs-nocr '[{"object":{"metadata":{"name":"gone"}},"filterResult":{"phase":"Provisioned"}}]')"
unset STUB_PATCH_FAIL
# Provisioned, no gitops/domain on the CR: kubeconfig Secret + conditions.
check "provisioned, no domain" capi-status-sync "$(binding cs-prov '[{"object":{"metadata":{"name":"prod","namespace":"clusters"}},"filterResult":{"phase":"Provisioned","controlPlaneReady":true,"controlPlaneEndpoint":{"host":"10.0.0.1","port":6443}}}]')"
expect_klog "clusterctl get kubeconfig prod -n clusters"
expect_klog "kubectl create secret generic prod-kubeconfig -n clusters --from-file=value=/dev/stdin --dry-run=client -o yaml"
expect_klog '{"status":{"kubeconfig":{"secretRef":"prod-kubeconfig"}}}'
expect_klog '"type":"InfrastructureReady"'
# Provisioned with gitops: the bash `declare -f gitops::bootstrap` fails
# without lib/ (no bootstrap, no gitops status patch); the Go seam is
# wired. The diff is confined to that one patch — compare with it removed
# and assert it on the Go side.
export STUB_GITOPS=flux STUB_DOMAIN=prod.example.com
run_hook bash capi-status-sync "$(binding cs-gitops '[{"object":{"metadata":{"name":"prod","namespace":"clusters"}},"filterResult":{"phase":"Provisioned"}}]')"
run_hook go capi-status-sync "${WORK}/cs-gitops.json"
unset STUB_GITOPS STUB_DOMAIN
if diff <(grep -v '"gitops":{"provider":"flux"' "${WORK}/go.klog") "${WORK}/bash.klog" >/dev/null \
  && grep -qF '{"status":{"gitops":{"provider":"flux","status":"Bootstrapped"}}}' "${WORK}/go.klog" \
  && grep -q 'bootstrapping GitOps (flux) for prod.example.com' "${WORK}/go.err" \
  && grep -q 'bootstrapping GitOps (flux) for prod.example.com' "${WORK}/bash.err"; then
  echo "ok: capi-status-sync provisioned + gitops (Go stamps the gitops status; bash lacks the lib)"
else
  echo "FAIL: capi-status-sync provisioned + gitops"; diff "${WORK}/bash.klog" "${WORK}/go.klog" | head -20 | sed 's/^/  /' || true
  failures=$((failures + 1))
fi

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
