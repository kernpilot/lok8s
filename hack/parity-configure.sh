#!/usr/bin/env bash
# parity-configure.sh — differential test between the Go lo and the argsh lo
# for the configure/inspect commands: lint, kubeconfig, doctor.
#
# For every case, runs BOTH implementations (the Go binary, and the same
# binary routed to the frozen tree by the project file) against a synthetic
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
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
unset KUSTOMIZE_PLUGIN_HOME LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID \
  HCLOUD_TOKEN HROBOT_USER HROBOT_PASSWORD

# Isolated HOME: keeps mkcert's -CAROOT and hcloud's context store off the
# developer's real ones, so doctor's dev-TLS / provider lines are the same
# fresh-machine answer on every run (and identical for both implementations).
export HOME="${WORK}/home"
mkdir -p "${HOME}"

CL="${PROJ}/clusters"
parity::new_project "${PROJ}"

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
# Go-only: --notes adds the default-equal advisory and changes nothing
# else: same exit code, same stderr, stdout equal once the [note] lines
# are dropped, and the note itself present for a key at its default. Its
# own project, so the check cases above keep their fixtures and stay
# byte-identical to the bash lint. Both runs are the Go binary.
NOTES="${WORK}/notes"
parity::new_project "${NOTES}"
mkdir -p "${NOTES}/clusters/theta.dev"
printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: theta\nspec:\n  cluster:\n    domain: theta.dev\n  runtime: kind\n  nodes:\n    controlPlane: 1\n' \
  > "${NOTES}/clusters/theta.dev/cluster.lok8s.yaml"
notes_rc=0; plain_rc=0
parity::run go "${NOTES}" lint --domain theta.dev --notes || notes_rc=$?
cp "${WORK}/go.out" "${WORK}/notes.out"; cp "${WORK}/go.err" "${WORK}/notes.err"
parity::run go "${NOTES}" lint --domain theta.dev || plain_rc=$?
want_note='[note] clusters/theta.dev/cluster.lok8s.yaml: spec.runtime equals the default (kind); you can drop it'
if (( notes_rc == plain_rc )) && cmp -s "${WORK}/go.err" "${WORK}/notes.err" \
   && grep -qxF "${want_note}" "${WORK}/notes.out" && ! grep -q '^\[note\] ' "${WORK}/go.out" \
   && diff -q "${WORK}/go.out" <(grep -v '^\[note\] ' "${WORK}/notes.out") >/dev/null; then
  echo "ok: lo lint --notes prints the note, keeps rc (${plain_rc}), stderr and the non-note stdout"
else
  fail "lo lint --notes: rc ${notes_rc} vs ${plain_rc}, the note missing, a note without the flag, or streams differ beyond [note] lines"
fi

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
# environment by both implementations, so no environment allowance is
# needed — it would only mask a genuine port divergence. The ONE allowed
# line is PATH_SECRETS (D35): the bash entrypoint defaults the variable to
# <project>/.secrets and its doctor prints that path; the binary defaults
# nothing and prints an info line. The binary's text is pinned below, and
# the set case (both print `PATH_SECRETS=<dir>`) is diffed strictly.
check PATH_SECRETS doctor                   # active alpha.dev (kind lo)
check PATH_SECRETS doctor --domain gamma.app           # Deploy -> alpha.dev
check PATH_SECRETS doctor --domain nowhere.dev         # active domain has no spec
check PATH_SECRETS doctor --domain prov.dev            # provider / infrastructure section (hetzner, offline)
parity::select "${PROJ}" go
if (cd "${PROJ}" && "${LO_BIN}" doctor </dev/null 2>/dev/null || true) | grep -q 'PATH_SECRETS unset (per-domain stores; the flat store is retired)'; then
  echo "ok: doctor: the binary reports PATH_SECRETS unset"
else
  fail "doctor: the binary does not report PATH_SECRETS unset"
fi
PATH_SECRETS="${PROJ}/clusters" check - doctor       # set: the same line on both sides

# ── lo init --plan (Go-only contract) ────────────────────────────────────────
# Bare `lo init` off a terminal prints the help (parity-leaves pins rc 0);
# `lo init --plan` prints the mode's screen as text — in a project the
# state card (the two-column layout, the project name first, no doctor
# section), the action list and the next step; elsewhere the welcome
# and the bootstrap screen with the defaults — exits 0 and writes
# nothing, on and off a terminal. Its own
# synthetic projects (an empty directory, a project root), the tree
# snapshotted before and after. Both runs are the Go binary; the bash
# tree has no wizard.
PLAN="${WORK}/plan"
parity::new_project "${PLAN}"
mkdir -p "${PLAN}/clusters/theta.dev"
printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: theta\nspec:\n  cluster:\n    domain: theta.dev\n' \
  > "${PLAN}/clusters/theta.dev/cluster.lok8s.yaml"
echo "theta.dev" > "${PLAN}/clusters/.active"
mkdir -p "${WORK}/plan-empty"
plan_snapshot() { (cd "${1}" && find . -not -path './.bin/*' -not -path './.lok8s/*' | sort); }
parity::select "${PLAN}" go                     # the project file first: the snapshot must include it
plan_before="$(plan_snapshot "${PLAN}")"
plan_rc=0
parity::run go "${PLAN}" init --plan || plan_rc=$?
if (( plan_rc == 0 )) && [[ ! -s "${WORK}/go.err" ]] \
   && grep -q '^  parity  *project · go' "${WORK}/go.out" \
   && grep -q '^  clusters  *theta.dev (kind, active)$' "${WORK}/go.out" \
   && ! grep -q '^--- ' "${WORK}/go.out" \
   && grep -q '^  actions  *Add a cluster · Add a service' "${WORK}/go.out" \
   && grep -q '^  equivalent  *lo init cluster <domain> · lo init service <name>' "${WORK}/go.out" \
   && grep -q '^  next  *lo ' "${WORK}/go.out" \
   && [[ "$(plan_snapshot "${PLAN}")" == "${plan_before}" ]]; then
  echo "ok: lo init --plan in a project root: rc 0, the card, the list, the next step, no writes"
else
  fail "lo init --plan in a project root: rc ${plan_rc}, or the card/list missing, or the tree changed"
fi
plan_rc=0
(cd "${WORK}/plan-empty" && "${LO_BIN}" init --plan </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || plan_rc=$?
if (( plan_rc == 0 )) && [[ ! -s "${WORK}/go.err" ]] \
   && grep -q '^  lo init sets up a lok8s project in this directory' "${WORK}/go.out" \
   && grep -q '^  name  *plan-empty$' "${WORK}/go.out" \
   && grep -q '^  domain  *plan-empty.dev$' "${WORK}/go.out" \
   && grep -q '^  equivalent  *lo init project plan-empty --env mise --cluster plan-empty.dev --driver lo · git init · lo toolchain install --groups core,local · lo use plan-empty.dev$' "${WORK}/go.out" \
   && [[ -z "$(find "${WORK}/plan-empty" -mindepth 1 -print -quit)" ]]; then
  echo "ok: lo init --plan in an empty directory: rc 0, the welcome, the bootstrap screen with the defaults, no writes"
else
  fail "lo init --plan in an empty directory: rc ${plan_rc}, or the commands missing, or something was written"
fi
plan_rc=0
(cd "${WORK}/plan-empty" && "${LO_BIN}" init </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || plan_rc=$?
if (( plan_rc == 0 )) && grep -q '^Usage:$' "${WORK}/go.out" && [[ -z "$(find "${WORK}/plan-empty" -mindepth 1 -print -quit)" ]]; then
  echo "ok: bare lo init off a terminal: the help, rc 0, no writes"
else
  fail "bare lo init off a terminal: rc ${plan_rc}, or no help, or something was written"
fi

# ── routed commands (spec.implementation.bash.commands) ─────────────────────
# The Go side's project file keeps `default: go` and lists one command for
# the bash tree; the binary then execs <project>/.lok8s/lo for it with the
# verbatim argv. Both sides run the same bash code, and the strict diff
# proves the exec path: argv, the prepared PATH, the tree. `doctor` is the
# widest output; `version` the smallest (routed, the Go side prints the
# `bash` row too, so no allowance is needed).
PARITY_ROUTE_GO=doctor check - doctor
PARITY_ROUTE_GO=doctor check - doctor --domain gamma.app
# --no-color is the binary's flag: parsed natively on the Go side, stripped
# from argv on the routed side and handed on as NO_COLOR=1. Piped, both
# print the plain doctor either way; the strict diff proves the routed
# tree never sees the flag (argsh would reject it).
check PATH_SECRETS doctor --no-color               # native vs the shim; the D35 line
PARITY_ROUTE_GO=doctor check - doctor --no-color   # routed on both sides
PARITY_ROUTE_GO=version check - version

report
