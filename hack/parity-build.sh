#!/usr/bin/env bash
# parity-build.sh — differential test between the Go `lo build` and the argsh
# implementation (same structure as hack/parity-test.sh).
#
# Each case runs BOTH implementations (the Go binary, and the same binary with
# LO_IMPL=bash forcing the argsh passthrough) against a synthetic project and
# diffs stdout, stderr, exit code, the rendered artifacts.yaml bytes, the
# split dir file LIST, and the non-Secret split file bytes. Secret twins are
# never byte-compared (sops mints a fresh data key per encrypt) — parity is
# presence + the `sops:` marker in both.
#
# The synthetic domains deliberately avoid the secrets.lok8s.dev generator and
# khelm so the render is hermetic (no store, no network); plugin parity is
# covered by both implementations exec'ing the identical pinned kustomize.
#
# Each implementation starts from an identically-prepared domain state
# (promote-on-change and the split carry-forward depend on prior state, so
# running the second impl over the first one's outputs would skew its
# stderr/behavior).
#
# Usage: hack/parity-build.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }
[[ -x "${ROOT}/.bin/kustomize" && -x "${ROOT}/.bin/yq" && -x "${ROOT}/.bin/sops" ]] \
  || { echo "error: pinned toolchain missing under ${ROOT}/.bin (b install)" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
PROJ="${WORK}/proj"

# ── env hygiene ──────────────────────────────────────────────────────────────
# The caller's shell often carries a foreign PATH_BASE / LOK8S_* / DOMAIN_NAME
# (this repo's memory has the scars) — build parity needs a clean slate plus
# ONE whitelisted spec var to prove the envsubst pass.
while read -r v; do unset "${v}"; done < <(compgen -e | grep -E '^(LOK8S|KIND)_' || true)
unset DOMAIN_NAME KUBECONFIG DEBUG PATH_SECRETS KUSTOMIZE_PLUGIN_HOME || true
export PATH_BASE="${PROJ}" PATH_BIN="${PROJ}/.bin" PATH_LOK8S="${PROJ}/.lok8s" PATH_CLUSTERS="${PROJ}/clusters"
export LOK8S_SPEC_PARITY_VALUE="rendered-ok"

# Fixed, well-known age public key (the age README example) — sops encrypt
# needs only the public half, so no keygen dependency.
AGE_KEY="age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj"

# ── synthetic project ────────────────────────────────────────────────────────
mkdir -p "${PROJ}/clusters/split.dev" "${PROJ}/clusters/single.dev"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
ln -s "${ROOT}/.bin" "${PROJ}/.bin"
echo "split.dev" > "${PROJ}/clusters/.active"

cat > "${PROJ}/clusters/split.dev/cluster.lok8s.yaml" <<EOF
kind: Lo
metadata:
  name: splitty
spec:
  build:
    artifacts: split
  gitops:
    provider: flux
    age:
      - ${AGE_KEY}
EOF
cat > "${PROJ}/clusters/split.dev/kustomization.yaml" <<'EOF'
resources:
  - manifests.yaml
EOF
cat > "${PROJ}/clusters/split.dev/manifests.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: parity-cm
  namespace: demo
data:
  value: "${LOK8S_SPEC_PARITY_VALUE}"
  keep: "$NOTLISTED and ${OTHER_VAR} stay put"
---
apiVersion: batch/v1
kind: Job
metadata:
  name: parity-job
  namespace: demo
spec:
  ttlSecondsAfterFinished: 60
  template:
    spec:
      containers:
        - name: main
          image: busybox
      restartPolicy: Never
---
apiVersion: v1
kind: Secret
metadata:
  name: parity-secret
  namespace: demo
stringData:
  password: hunter2
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: parity-role
rules: []
EOF

cat > "${PROJ}/clusters/single.dev/cluster.lok8s.yaml" <<'EOF'
kind: Lo
metadata:
  name: singleton
EOF
cat > "${PROJ}/clusters/single.dev/kustomization.yaml" <<'EOF'
resources:
  - cm.yaml
EOF
cat > "${PROJ}/clusters/single.dev/cm.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: solo
  namespace: one
data:
  hello: world
EOF

# ── state preparation ────────────────────────────────────────────────────────
domain_reset() { # <domain> — wipe generated outputs
  rm -rf "${PROJ}/clusters/${1}/artifacts.yaml" "${PROJ}/clusters/${1}/artifacts" \
         "${PROJ}/clusters/${1}"/tmp.* "${PROJ}/clusters/${1}"/.artifacts-stage.*
}

# Fixture: one bash-built split.dev output, reused so both impls can start a
# case from the SAME committed state.
domain_reset split.dev
(cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" build --domain split.dev >/dev/null 2>&1)
mkdir -p "${WORK}/fixture"
cp "${PROJ}/clusters/split.dev/artifacts.yaml" "${WORK}/fixture/artifacts.yaml"
cp -r "${PROJ}/clusters/split.dev/artifacts" "${WORK}/fixture/artifacts"
domain_reset split.dev

prep_clean() { domain_reset split.dev; domain_reset single.dev; }
prep_built() { # split.dev starts from the committed fixture state
  prep_clean
  cp "${WORK}/fixture/artifacts.yaml" "${PROJ}/clusters/split.dev/artifacts.yaml"
  cp -r "${WORK}/fixture/artifacts" "${PROJ}/clusters/split.dev/artifacts"
}
prep_built_orphan() { # plus a committed Secret twin absent from the render
  prep_built
  printf 'data: x\nsops:\n    stub: carried\n' \
    > "${PROJ}/clusters/split.dev/artifacts/Secret.demo.orphan.sops.yaml"
}
prep_empty_kustomization() { # committed state + a render that produces nothing
  prep_built
  printf 'resources: []\n' > "${PROJ}/clusters/split.dev/kustomization.yaml"
}
restore_kustomization() {
  printf 'resources:\n  - manifests.yaml\n' > "${PROJ}/clusters/split.dev/kustomization.yaml"
}

# ── runner ───────────────────────────────────────────────────────────────────
failures=0
fail() { echo "FAIL: ${*}"; failures=$((failures + 1)); }

capture_state() { # <impl> — snapshot generated outputs for cross-impl compare
  local impl="${1}" sdir="${WORK}/state.${1}"
  rm -rf "${sdir}"
  mkdir -p "${sdir}"
  [[ -f "${PROJ}/clusters/split.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/split.dev/artifacts.yaml" "${sdir}/split.artifacts.yaml"
  [[ -f "${PROJ}/clusters/single.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/single.dev/artifacts.yaml" "${sdir}/single.artifacts.yaml"
  local d
  for d in split.dev single.dev; do
    if [[ -d "${PROJ}/clusters/${d}/artifacts" ]]; then
      (cd "${PROJ}/clusters/${d}/artifacts" && ls -A) | sort > "${sdir}/${d}.listing"
      mkdir -p "${sdir}/${d}.files"
      local f
      for f in "${PROJ}/clusters/${d}/artifacts"/* "${PROJ}/clusters/${d}/artifacts"/.gitignore; do
        [[ -f "${f}" ]] || continue
        case "${f##*/}" in
          Secret.*.sops.yaml)
            # Secret twins: presence + sops marker only (fresh data key per
            # encrypt makes bytes non-deterministic by design).
            grep -q '^sops:' "${f}" && echo "sops-ok" > "${sdir}/${d}.files/${f##*/}.marker" \
              || echo "NO-SOPS-MARKER" > "${sdir}/${d}.files/${f##*/}.marker"
            ;;
          *) cp "${f}" "${sdir}/${d}.files/" ;;
        esac
      done
    fi
  done
}

check() { # <name> <prep-fn> <argv...>
  local name="${1}" prep="${2}"; shift 2
  local go_rc=0 bash_rc=0

  "${prep}"
  (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  capture_state bash

  "${prep}"
  (cd "${PROJ}" && "${LO_BIN}" "$@" >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  capture_state go

  local ok=1
  if (( go_rc != bash_rc )); then
    fail "${name} — rc: bash=${bash_rc} go=${go_rc}"; ok=0
  fi
  local stream
  for stream in out err; do
    if ! diff -q "${WORK}/bash.${stream}" "${WORK}/go.${stream}" >/dev/null; then
      fail "${name} — std${stream} differs:"
      diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if ! diff -rq "${WORK}/state.bash" "${WORK}/state.go" >/dev/null 2>&1; then
    fail "${name} — generated state differs:"
    diff -r "${WORK}/state.bash" "${WORK}/state.go" 2>&1 | head -20 | sed 's/^/  /'
    ok=0
  fi
  (( ok )) && echo "ok: ${name}"
}

# ── cases ────────────────────────────────────────────────────────────────────
# Full split-mode build (spec-declared; Secret encrypted to the age key).
check "build (bare, .active=split.dev)"      prep_clean build
check "build --domain split.dev"             prep_clean build --domain split.dev
check "b alias"                              prep_clean b --domain split.dev
# Rebuild over the committed fixture: unchanged render, no promote message;
# the Secret re-encrypts (no private key → the change-compare falls through).
check "rebuild over committed state"         prep_built build --domain split.dev
# Single-mode domain + the --split debug override (one-off warn).
check "build --domain single.dev"            prep_clean build --domain single.dev
check "build --split on single-mode domain"  prep_clean build --domain single.dev --split
# --single on a split-mode domain (stale-warn), and the flag exclusions.
check "build --single on split-mode domain"  prep_clean build --domain split.dev --single
check "--split/--single exclusion"           prep_clean build --domain split.dev --split --single
check "--no-secrets/--single exclusion"      prep_clean build --domain split.dev --no-secrets --single
# --no-secrets: committed Secret twins stay inert (prune guard) — including
# one that is NOT in the render at all.
check "--no-secrets over committed secrets"  prep_built_orphan build --domain split.dev --no-secrets
for impl in bash go; do
  [[ "$(cat "${WORK}/state.${impl}/split.dev.files/Secret.demo.orphan.sops.yaml.marker" 2>/dev/null)" == "sops-ok" ]] \
    || fail "--no-secrets prune guard (${impl}) — committed orphan Secret twin was pruned or lost its sops marker"
  [[ "$(cat "${WORK}/state.${impl}/split.dev.files/Secret.demo.parity-secret.sops.yaml.marker" 2>/dev/null)" == "sops-ok" ]] \
    || fail "--no-secrets prune guard (${impl}) — committed rendered Secret twin was pruned"
done
# Empty-render refusal (prior state must survive in both).
check "empty-render refusal"                 prep_empty_kustomization build --domain split.dev
restore_kustomization
# Unknown domain: banner + kustomization guard (the argsh ~domain validator
# does NOT fire here — pre-set local — verified against the bash impl).
check "unknown domain"                       prep_clean build --domain bogus.dev

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
