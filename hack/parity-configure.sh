#!/usr/bin/env bash
# parity-configure.sh — differential test between the Go lo and the argsh lo
# for the configure/inspect commands: lint, kubeconfig, doctor.
#
# For every case, runs BOTH implementations (the Go binary, and the same
# binary with LO_IMPL=bash forcing the argsh passthrough) against a synthetic
# project and diffs stdout, stderr, and exit codes.
#
# doctor's output depends on the machine's toolchain (which tools exist, the
# bash version, mkcert's CA state) — but this is a DIFFERENTIAL harness: both
# implementations run in the identical environment, so every environment-
# driven line must come out identical on both sides and the diffs stay
# strict ("-" = no allowance). The only tolerance is a documented one below
# (provider section: absent entirely when the argsh toolchain is missing).
#
# Usage: hack/parity-configure.sh [path-to-go-lo]   (default: bin/lo)
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
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG \
  KUSTOMIZE_PLUGIN_HOME LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID \
  HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD

# Pin the C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists stores/dirs in byte order (= C collation). Under e.g. en_US.UTF-8 the
# two orderings differ for mixed-case names — a cosmetic listing-order
# divergence this differential harness must not trip over.
export LC_ALL=C

# Isolated HOME: keeps mkcert's -CAROOT and hcloud's context store off the
# developer's real ones, so doctor's dev-TLS / provider lines are the same
# fresh-machine answer on every run (and identical for both implementations).
export HOME="${WORK}/home"
mkdir -p "${HOME}"

PROJ="${WORK}/proj"
CL="${PROJ}/clusters"
mkdir -p "${CL}"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
ln -s "${ROOT}/.bin" "${PROJ}/.bin"

# ── Synthetic project ────────────────────────────────────────────────────────

# alpha.dev — clean Lo cluster with spec.oidc; the kubeconfig/oidc happy path.
mkdir -p "${CL}/alpha.dev/targets/good" "${CL}/alpha.dev/targets/bad" \
  "${CL}/alpha.dev/targets/nokust" "${CL}/alpha.dev/secrets"
cat > "${CL}/alpha.dev/cluster.lok8s.yaml" <<'EOF'
kind: Lo
apiVersion: lok8s.dev/v1
metadata:
  name: alpha
spec:
  oidc:
    issuer: https://id.example.dev
    clientID: kubectl
EOF
cat > "${CL}/alpha.dev/targets/good/kustomization.yaml" <<'EOF'
resources:
  - app.yaml
EOF
cat > "${CL}/alpha.dev/targets/good/app.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: app
  labels:
    lok8s.dev/name: app
EOF
# bad target: missing local resource (error), remote resource (skipped),
# unlabelled manifest (warn), multi-doc manifest whose FIRST doc carries the
# label (quirk: no warn — the bash capture is "1\n0", not "0").
cat > "${CL}/alpha.dev/targets/bad/kustomization.yaml" <<'EOF'
resources:
  - missing.yaml
  - https://example.com/remote.yaml
EOF
cat > "${CL}/alpha.dev/targets/bad/unlabelled.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: plain
EOF
cat > "${CL}/alpha.dev/targets/bad/multi.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: one
  labels:
    lok8s.dev/name: one
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: two
EOF
# secrets: unencrypted (no .enc), stale .enc, fresh .enc, legacy data: file,
# a flat-store shadow (identical) and a flat-store drift (differing).
printf 'tok-1\n' > "${CL}/alpha.dev/secrets/Secret.app.default.TOKEN"
printf 'old\n'   > "${CL}/alpha.dev/secrets/Secret.app.default.STALE.enc"
sleep 0.01  # [[ -nt ]] needs a strictly newer plaintext
printf 'new\n'   > "${CL}/alpha.dev/secrets/Secret.app.default.STALE"
printf 'ok\n'    > "${CL}/alpha.dev/secrets/Secret.app.default.FRESH"
sleep 0.01
printf 'enc\n'   > "${CL}/alpha.dev/secrets/Secret.app.default.FRESH.enc"
printf 'same\n'  > "${CL}/alpha.dev/secrets/Secret.app.default.SHADOW"
printf 'a-val\n' > "${CL}/alpha.dev/secrets/Secret.app.default.DRIFT"
cat > "${CL}/alpha.dev/secrets/manual-secret.yaml" <<'EOF'
apiVersion: v1
kind: Secret
data:
  k: dg==
EOF
mkdir -p "${PROJ}/.secrets"
printf 'same\n'  > "${PROJ}/.secrets/Secret.app.default.SHADOW"
printf 'b-val\n' > "${PROJ}/.secrets/Secret.app.default.DRIFT"

# beta.cloud — schema violations + every synthesizable bootstrap-entry error.
mkdir -p "${CL}/beta.cloud/targets/plain"
touch "${CL}/beta.cloud/targets/plain/kustomization.yaml"
cat > "${CL}/beta.cloud/cluster.lok8s.yaml" <<'EOF'
kind: KubeOne
spec:
  bootstrap:
    - nope
    - ./targets/absent
    - ccm:
        wait: maybe
    - x:
        name: 123
    - y:
        dependsOn: [~]
    - z:
        env:
          BAD-KEY: v
    - ./targets/plain:
        values:
          a: 1
    - w:
        valueFiles: ./one.yaml
    - {m1: 1, m2: 2}
EOF

# gamma.app — valid deploy domain (clusterRef → alpha.dev).
mkdir -p "${CL}/gamma.app"
cat > "${CL}/gamma.app/deploy.lok8s.yaml" <<'EOF'
kind: Deploy
apiVersion: lok8s.dev/v1
metadata:
  name: gamma
spec:
  clusterRef:
    domain: alpha.dev
EOF

# delta.app — deploy domain missing spec.clusterRef entirely.
mkdir -p "${CL}/delta.app"
printf 'kind: Deploy\napiVersion: lok8s.dev/v1\nmetadata:\n  name: delta\n' \
  > "${CL}/delta.app/deploy.lok8s.yaml"

# epsilon.app — deploy domain whose clusterRef points nowhere.
mkdir -p "${CL}/epsilon.app"
cat > "${CL}/epsilon.app/deploy.lok8s.yaml" <<'EOF'
kind: Deploy
apiVersion: lok8s.dev/v1
metadata:
  name: epsilon
spec:
  clusterRef:
    domain: ghost.dev
EOF

# eta.dev — spec.oidc with a plain-http issuer (boundary-rule error).
mkdir -p "${CL}/eta.dev"
cat > "${CL}/eta.dev/cluster.lok8s.yaml" <<'EOF'
kind: Lo
apiVersion: lok8s.dev/v1
metadata:
  name: eta
spec:
  bootstrap: []
  oidc:
    issuer: http://insecure.example
    clientID: kubectl
EOF

# iota.dev + kappa.app — the <metadata.name>.yaml kubeconfig fallback for a
# deploy domain (no secret.iota.dev.yaml exists).
mkdir -p "${CL}/iota.dev" "${CL}/kappa.app"
cat > "${CL}/iota.dev/cluster.lok8s.yaml" <<'EOF'
kind: Lo
apiVersion: lok8s.dev/v1
metadata:
  name: iota-cluster
spec:
  bootstrap: []
EOF
cat > "${CL}/kappa.app/deploy.lok8s.yaml" <<'EOF'
kind: Deploy
apiVersion: lok8s.dev/v1
metadata:
  name: kappa
spec:
  clusterRef:
    domain: iota.dev
EOF

# sub.alpha.dev — the one-cluster-per-apex violation.
mkdir -p "${CL}/sub.alpha.dev"
printf 'kind: Lo\napiVersion: lok8s.dev/v1\nmetadata:\n  name: sub\nspec:\n  bootstrap: []\n' \
  > "${CL}/sub.alpha.dev/cluster.lok8s.yaml"

# prov.dev — provider-backed cluster (hetzner) for doctor's provider section.
# Empty descriptor + no HCLOUD/HROBOT creds + isolated HOME keep the hetzner
# doctor hook fully offline and deterministic.
mkdir -p "${CL}/prov.dev"
cat > "${CL}/prov.dev/cluster.lok8s.yaml" <<'EOF'
kind: KubeOne
apiVersion: lok8s.dev/v1
metadata:
  name: prov
spec:
  bootstrap: []
  provider:
    name: hetzner
    configRef: provider.yaml
EOF
printf 'server: []\n' > "${CL}/prov.dev/provider.yaml"

# services.yaml + per-service lok8s.yaml violations + drift warnings.
cat > "${PROJ}/services.yaml" <<'EOF'
apiVersion: lok8s.dev/v1
kind: Services
bogusTop: 1
registry:
  endpoint: reg.example
  bogusReg: 1
defaults:
  dockerfile: bogus
services:
  svc1:
    image: pinned:1
    registry:
      endpoint: reg.example
      bogusSubReg: 1
    bogusEntry: 1
  svc2: {}
  svc3: {}
  svc4: {}
EOF
printf "docker_build('x', '.')\n" > "${PROJ}/Tiltfile"
mkdir -p "${PROJ}/svc2/deploy"
cat > "${PROJ}/svc2/lok8s.yaml" <<'EOF'
build: .
bogusKey: 1
EOF
printf '# redundant\n' > "${PROJ}/svc2/Tiltfile"
printf 'kind: ConfigMap\n' > "${PROJ}/svc2/deploy/cm.yaml"
mkdir -p "${PROJ}/svc3"
cat > "${PROJ}/svc3/lok8s.yaml" <<'EOF'
build: .
components:
  - name: a
  - build: .
    bogusComp: 1
EOF
mkdir -p "${PROJ}/svc4/deploy"
cat > "${PROJ}/svc4/lok8s.yaml" <<'EOF'
components:
  - name: web
    build: .
  - name: api
    build: .
EOF
cat > "${PROJ}/svc4/deploy/dep.yaml" <<'EOF'
metadata:
  labels:
    lok8s.dev/name: web
EOF

# kubeconfig fixtures: the provisioned-cluster files under .kubeconfig/.
mkdir -p "${PROJ}/.kubeconfig"
cat > "${PROJ}/.kubeconfig/alpha.yaml" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: kind-alpha
    cluster:
      server: https://127.0.0.1:6443
      certificate-authority-data: QUJD
contexts: []
users: []
EOF
# secret.alpha.dev.yaml must WIN over alpha.yaml for the deploy domain, and
# exercises the certificate-authority FILE fallback in the --oidc emit.
cat > "${PROJ}/.kubeconfig/secret.alpha.dev.yaml" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: alpha-secret
    cluster:
      server: https://10.0.0.1:6443
      certificate-authority: /etc/ca.pem
contexts: []
users: []
EOF
cat > "${PROJ}/.kubeconfig/iota-cluster.yaml" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: iota
    cluster:
      server: https://10.0.0.9:6443
contexts: []
users: []
EOF

echo "alpha.dev" > "${CL}/.active"

failures=0

# check <allow-diff-regex|-> <argv...>
check() {
  local allow="${1}"; shift
  local go_rc=0 bash_rc=0
  (cd "${PROJ}" && "${LO_BIN}" "$@" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?

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

# ── lo lint ──────────────────────────────────────────────────────────────────
check - lint                            # active domain (alpha.dev): warnings + bad-target error
check - l                               # alias
check - lint --domain alpha.dev
check - lint --domain beta.cloud        # schema + every bootstrap-entry error
check - lint --domain gamma.app         # clean deploy domain
check - lint --domain delta.app         # missing spec.clusterRef
check - lint --domain epsilon.app       # clusterRef.domain not found
check - lint --domain sub.alpha.dev     # apex violation reported repo-globally anyway
check - lint --domain iota.dev          # fully clean domain
check - lint --domain nowhere.dev       # no spec at all
check - --domain iota.dev lint          # global-flag spelling

# ── lo kubeconfig ────────────────────────────────────────────────────────────
check - kubeconfig                          # active alpha.dev → cat alpha.yaml
check - kc                                  # alias
check - kubeconfig --domain alpha.dev
check - kubeconfig --domain gamma.app       # deploy → clusterRef → secret.alpha.dev.yaml wins
check - kubeconfig --domain kappa.app       # deploy → <metadata.name>.yaml fallback (iota-cluster.yaml)
check - kubeconfig --domain beta.cloud      # no metadata.name → could not resolve
check - kubeconfig --domain epsilon.app     # broken clusterRef → resolver error
check - kubeconfig --domain iota.dev        # cat iota-cluster.yaml
check - kubeconfig --domain gamma.app --cluster-override beta.cloud  # override, then admin-path fallback
check - kubeconfig --domain alpha.dev --oidc      # exec-plugin emit, inline CA data
check - kubeconfig --domain alpha.dev -o          # short flag
check - kubeconfig --domain gamma.app --oidc      # deploy domain: spec via clusterRef, CA FILE branch
check - kubeconfig --domain iota.dev --oidc       # no spec.oidc → no usable spec.oidc
check - kubeconfig --domain eta.dev --oidc        # http issuer → boundary-rule error (needs a kubeconfig first)
check - -v kubeconfig --domain gamma.app          # debug line parity

# eta.dev has no kubeconfig file, so the http-issuer path needs one:
cat > "${PROJ}/.kubeconfig/eta.yaml" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: eta
    cluster:
      server: https://10.0.0.7:6443
EOF
check - kubeconfig --domain eta.dev --oidc        # http issuer → load_spec boundary error

# ── lo doctor ────────────────────────────────────────────────────────────────
# Strict diffs on purpose: every environment-driven doctor line (tool
# presence, bash version, mkcert CA state) is produced from the SAME
# environment by both implementations, so no allow-filter is needed — an
# environment allowance would only mask a genuine port divergence.
check - doctor                              # active alpha.dev (kind lo)
check - doctor --domain gamma.app           # Deploy -> alpha.dev
check - doctor --domain nowhere.dev         # active domain has no spec
check - doctor --domain prov.dev            # provider / infrastructure section (hetzner, offline)

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
