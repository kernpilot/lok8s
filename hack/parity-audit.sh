#!/usr/bin/env bash
# parity-audit.sh — differential test between the Go `lo audit` and the argsh
# implementation.
#
# Builds a synthetic project whose domains exercise every check family (clean
# pass, spec-flag encryption fail, EOL/newer k8s versions, NodePort/privileged
# /plaintext targets, SecurityPolicy deny + unmerged route override, identity-
# only EncryptionConfiguration, deploy-only domain, invalid domain) and runs
# BOTH implementations (the Go binary, and the same binary with LO_IMPL=bash)
# for every domain × {human, --json, --sarif}, diffing stdout, stderr, and
# exit codes byte-for-byte. Absolute project paths in the output are
# normalized to PROJ (some findings embed them).
#
# Usage: hack/parity-audit.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# The harness must run against its synthetic project ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations into that live repo instead of
# ${WORK}.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG

# Pin the C locale: bash glob/sort ordering is LC_COLLATE-dependent and the Go
# port walks files in byte order (= C collation).
export LC_ALL=C

PROJ="${WORK}/proj"
mkdir -p "${PROJ}/clusters"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
ln -s "${ROOT}/.bin" "${PROJ}/.bin"

# ── clean.dev — kind=lo, everything as shipped (default bootstrap → the real
# framework cilium addon), supported k8s, https issuer, no targets.
mkdir -p "${PROJ}/clusters/clean.dev"
cat > "${PROJ}/clusters/clean.dev/cluster.lok8s.yaml" <<'EOF'
kind: Lo
metadata:
  name: clean
spec:
  kubernetes:
    version: v1.35.2
  oidc:
    issuer: https://id.clean.dev
EOF

# ── viol.cloud — one violation per check family.
mkdir -p "${PROJ}/clusters/viol.cloud/targets"
cat > "${PROJ}/clusters/viol.cloud/cluster.lok8s.yaml" <<'EOF'
kind: KubeOne
metadata:
  name: viol
spec:
  kubernetes:
    version: v1.33.7
  oidc:
    issuer: http://id.viol.cloud
  features:
    encryptionProviders:
      enable: false
EOF
cat > "${PROJ}/clusters/viol.cloud/targets/workload.yaml" <<'EOF'
apiVersion: v1
kind: Service
spec:
  type: NodePort
---
apiVersion: apps/v1
kind: DaemonSet
spec:
  template:
    spec:
      hostNetwork: true
      containers:
        - securityContext:
            privileged: true
          env:
            - name: URL
              value: http://plain.example.com/x
            - name: LOCAL
              value: http://localhost:8080
            - name: SVC
              value: http://api.ns.svc.cluster.local
      volumes:
        - name: h
          hostPath:
            path: /var/run
EOF

# ── secpol.app — gateway deny + UNMERGED route policy + NodePort (the
# combined finding), newer-than-known k8s, real EncryptionConfiguration,
# inline cilium override on the enforcing path.
mkdir -p "${PROJ}/clusters/secpol.app"
cat > "${PROJ}/clusters/secpol.app/cluster.lok8s.yaml" <<'EOF'
kind: KubeOne
metadata:
  name: secpol
spec:
  kubernetes:
    version: v1.99.1
  bootstrap:
    - cilium:
        policyAuditMode: false
        policyEnforcementMode: always
EOF
cat > "${PROJ}/clusters/secpol.app/artifacts.yaml" <<'EOF'
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - aescbc:
          keys: []
      - identity: {}
---
kind: SecurityPolicy
metadata:
  name: gw-deny
spec:
  targetRefs:
    - kind: Gateway
      name: shared
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal:
          clientCIDRs: ["10.1.0.0/16"]
---
kind: SecurityPolicy
metadata:
  name: route-open
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  authorization:
    defaultAction: Allow
---
apiVersion: v1
kind: Service
spec:
  type: NodePort
---
kind: HTTPRoute
metadata:
  name: r
EOF

# ── identity.net — identity-only EncryptionConfiguration (present-but-not-
# encrypting → fail-closed), LoadBalancer + deny + MERGED route policy (the
# allowlisted pass path), explicit bootstrap opt-out.
mkdir -p "${PROJ}/clusters/identity.net"
cat > "${PROJ}/clusters/identity.net/cluster.lok8s.yaml" <<'EOF'
kind: KubeOne
metadata:
  name: identity
spec:
  kubernetes:
    version: v1.36.0
  bootstrap: []
EOF
cat > "${PROJ}/clusters/identity.net/artifacts.yaml" <<'EOF'
kind: EncryptionConfiguration
resources:
  - resources: [secrets]
    providers:
      - identity: {}
---
apiVersion: v1
kind: Service
spec:
  type: LoadBalancer
---
kind: SecurityPolicy
spec:
  targetRefs:
    - kind: Gateway
      name: shared
  authorization:
    defaultAction: Deny
    rules:
      - action: Allow
        principal:
          clientCIDRs: ["10.2.0.0/16"]
---
kind: SecurityPolicy
spec:
  targetRef:
    kind: HTTPRoute
    name: app
  mergeType: StrategicMerge
  authorization:
    defaultAction: Allow
EOF

# ── gamma.app — deploy-only domain (no cluster spec → unknown, rc 0).
mkdir -p "${PROJ}/clusters/gamma.app"
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: identity.net\n' \
  > "${PROJ}/clusters/gamma.app/deploy.lok8s.yaml"

failures=0

# check <argv...> — run both implementations, diff rc/stdout/stderr with the
# project dir normalized to PROJ (findings embed absolute paths).
check() {
  local go_rc=0 bash_rc=0
  (cd "${PROJ}" && "${LO_BIN}" "$@" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  sed -i "s|${PROJ}|PROJ|g" "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"

  local ok=1
  if (( go_rc != bash_rc )); then
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
    echo "ok: lo $*"
  else
    failures=$((failures + 1))
  fi
}

# expect_rc <rc> <argv...> — the CONTRACT check on the Go binary alone: parity
# would also pass if both implementations drifted together, so pin the
# documented exit codes explicitly.
expect_rc() {
  local want="${1}"; shift
  local rc=0
  (cd "${PROJ}" && "${LO_BIN}" "$@" >/dev/null 2>&1) || rc=$?
  if (( rc == want )); then
    echo "ok: rc ${want}: lo $*"
  else
    echo "FAIL: lo $* — rc=${rc}, want ${want}"
    failures=$((failures + 1))
  fi
}

# Every domain × every output mode.
for d in clean.dev viol.cloud secpol.app identity.net gamma.app; do
  check audit "${d}"
  check audit "${d}" --json
  check audit "${d}" --sarif
done

# Domain resolution forms + the alias.
check au viol.cloud
check audit --domain viol.cloud
check audit --json viol.cloud
printf 'clean.dev\n' > "${PROJ}/clusters/.active"
check audit                        # bare → active domain
check audit viol.cloud extra.arg   # extra positionals ignored (argsh array)
rm -f "${PROJ}/clusters/.active"
check audit                        # no active → default lok8s.dev (missing → unknown, rc 0)

# Guards.
check audit '../evil'              # traversal → unknown finding, rc 0
check audit viol.cloud --json --sarif   # mutually exclusive → error, rc 1

# Exit-code contract, pinned (fail → 1; warn/unknown-only → 0).
expect_rc 1 audit viol.cloud
expect_rc 1 audit secpol.app --sarif
expect_rc 0 audit gamma.app
expect_rc 0 audit '../evil'
expect_rc 1 audit viol.cloud --json --sarif

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
