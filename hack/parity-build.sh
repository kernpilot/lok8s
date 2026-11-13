#!/usr/bin/env bash
# parity-build.sh — differential test between the Go `lo build` and the argsh
# implementation (same structure as hack/parity-test.sh).
#
# Each case runs BOTH implementations (the Go binary, and the same binary with
# routed to the frozen tree by the project file) against a synthetic project and
# diffs stdout, stderr, exit code, the rendered artifacts.yaml bytes, the
# split dir file LIST, and the non-Secret split file bytes. Secret twins are
# never byte-compared (sops mints a fresh data key per encrypt) — parity is
# presence + the `sops:` marker in both.
#
# The synthetic domains deliberately avoid the secrets.lok8s.dev generator and
# khelm so the render is hermetic (no store, no network); plugin parity is
# covered by both implementations exec'ing the identical pinned kustomize.
# One exception at the end: gen.dev runs the generator built from this tree
# (literals only, a fixture store) for the keys-only setting cases.
#
# Each implementation starts from an identically-prepared domain state
# (promote-on-change and the split carry-forward depend on prior state, so
# running the second impl over the first one's outputs would skew its
# stderr/behavior).
#
# Usage: hack/parity-build.sh [path-to-go-lo]   (default: bin/lo)
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
[[ -x "${ROOT}/.bin/kustomize" && -x "${ROOT}/.bin/yq" && -x "${ROOT}/.bin/sops" ]] \
  || { echo "error: pinned toolchain missing under ${ROOT}/.bin (b install)" >&2; exit 2; }

# ── env hygiene ──────────────────────────────────────────────────────────────
# The caller's shell often carries a foreign PATH_BASE / LOK8S_* / DOMAIN_NAME
# (this repo's memory has the scars) — build parity needs a clean slate plus
# ONE whitelisted spec var to prove the envsubst pass.
while read -r v; do unset "${v}"; done < <(compgen -e | grep -E '^(LOK8S|KIND)_' || true)
unset KUSTOMIZE_PLUGIN_HOME || true
export PATH_BASE="${PROJ}" PATH_BIN="${PROJ}/.bin" PATH_LOK8S="${PROJ}/.lok8s" PATH_CLUSTERS="${PROJ}/clusters"
export LOK8S_SPEC_PARITY_VALUE="rendered-ok"

# Fixed, well-known age public key (the age README example) — sops encrypt
# needs only the public half, so no keygen dependency.
AGE_KEY="age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj"

# ── synthetic project ────────────────────────────────────────────────────────
parity::new_project "${PROJ}"
mkdir -p "${PROJ}/clusters/split.dev" "${PROJ}/clusters/single.dev"
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
         "${PROJ}/clusters/${1}"/tmp.* "${PROJ}/clusters/${1}"/.artifacts-stage.* \
         "${PROJ}/clusters/${1}"/.artifacts-tmp.*
}

# Fixture: one bash-built split.dev output, reused so both impls can start a
# case from the SAME committed state.
domain_reset split.dev
parity::implementation "${PROJ}" bash
(cd "${PROJ}" && "${LO_BIN}" build --domain split.dev >/dev/null 2>&1)
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
capture_state() { # <impl> — snapshot generated outputs for cross-impl compare
  local impl="${1}" sdir="${WORK}/state.${1}"
  rm -rf "${sdir}"
  mkdir -p "${sdir}"
  [[ -f "${PROJ}/clusters/split.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/split.dev/artifacts.yaml" "${sdir}/split.artifacts.yaml"
  [[ -f "${PROJ}/clusters/single.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/single.dev/artifacts.yaml" "${sdir}/single.artifacts.yaml"
  [[ -f "${PROJ}/clusters/gen.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/gen.dev/artifacts.yaml" "${sdir}/gen.artifacts.yaml"
  [[ -f "${PROJ}/clusters/reuse.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/reuse.dev/artifacts.yaml" "${sdir}/reuse.artifacts.yaml"
  [[ -f "${PROJ}/clusters/specerr.dev/artifacts.yaml" ]] \
    && cp "${PROJ}/clusters/specerr.dev/artifacts.yaml" "${sdir}/specerr.artifacts.yaml"
  local g
  for g in guard.dev qguard.dev; do
    [[ -d "${PROJ}/clusters/${g}" ]] || continue
    (cd "${PROJ}/clusters/${g}" && find . -path ./secrets -prune -o -print | sort) > "${sdir}/${g}.tree"
  done
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

# kustomize names the temporary config file of a failed plugin in its error.
parity_normalize() {
  sed -i -E 's/kust-plugin-config-[0-9]+/kust-plugin-config-N/g' "$@"
}

# check <name> <prep-fn> <argv...>: the prep runs before EACH implementation,
# the state snapshot after it; streams, rc and the snapshots must match.
check() {
  local name="${1}" prep="${2}"; shift 2
  PARITY_PRE_EACH="${prep}" PARITY_POST_EACH=capture_state parity::run_pair "$@"
  local ok=1
  parity::compare "${name}" - || ok=0
  if ! diff -rq "${WORK}/state.bash" "${WORK}/state.go" >/dev/null 2>&1; then
    echo "FAIL: ${name} — generated state differs:"
    diff -r "${WORK}/state.bash" "${WORK}/state.go" 2>&1 | head -20 | sed 's/^/  /'
    ok=0
  fi
  parity::record "${ok}" "${name}"
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
# Cross-device scratch: TMPDIR on a filesystem the project is not on. The
# Go split once staged under $TMPDIR and moved the files into the project
# with a rename, which fails with EXDEV across devices — every split-mode
# build failed on a host or CI runner whose /tmp is its own mount (v0.3.0),
# while the bash `mv` copied and passed. Both implementations must build
# the same state with TMPDIR elsewhere. Skipped with a note when no
# writable tmpfs on another device than ${PROJ} is available.
cross_device_tmp() { # prints a writable dir on another device than PROJ
  local d
  for d in /dev/shm /run/shm; do
    [[ -d "${d}" && -w "${d}" ]] || continue
    [[ "$(stat -c %d "${d}" 2>/dev/null)" != "$(stat -c %d "${PROJ}" 2>/dev/null)" ]] || continue
    echo "${d}"
    return 0
  done
  return 1
}
if xdev="$(cross_device_tmp)"; then
  saved_tmpdir="${TMPDIR-}"; had_tmpdir="${TMPDIR+set}"
  export TMPDIR="${xdev}"
  check "build --domain split.dev (TMPDIR on another filesystem)" prep_clean build --domain split.dev
  if [[ -n "${had_tmpdir}" ]]; then export TMPDIR="${saved_tmpdir}"; else unset TMPDIR; fi
  for impl in bash go; do
    [[ -f "${WORK}/state.${impl}/split.dev.files/ConfigMap.demo.parity-cm.yaml" ]] \
      || fail "cross-device split (${impl}) — ConfigMap.demo.parity-cm.yaml missing from the split dir"
  done
else
  echo "note: no writable tmpfs on another device than ${PROJ} — the cross-device split case is skipped"
fi
# Empty-render refusal (prior state must survive in both).
check "empty-render refusal"                 prep_empty_kustomization build --domain split.dev
restore_kustomization
# Unknown domain: banner + kustomization guard (the argsh ~domain validator
# does NOT fire here — pre-set local — verified against the bash impl).
check "unknown domain"                       prep_clean build --domain bogus.dev

# A domain without its own store, in a project that keeps a store per domain,
# stops before the render: the flat .secrets/ store would read and generate
# values in the wrong place. --no-secrets never reads the store, so it renders.
# The fixture store and domain exist for these cases only.
mkdir -p "${PROJ}/clusters/store.dev/secrets" "${PROJ}/clusters/nostore.dev"
printf 'kind: Lo\nmetadata:\n  name: nostore\n' > "${PROJ}/clusters/nostore.dev/cluster.lok8s.yaml"
printf 'resources:\n  - cm.yaml\n' > "${PROJ}/clusters/nostore.dev/kustomization.yaml"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nostore\n  namespace: one\n' > "${PROJ}/clusters/nostore.dev/cm.yaml"
prep_nostore() { rm -f "${PROJ}/clusters/nostore.dev/artifacts.yaml"; }
check "domain without a store is refused"    prep_nostore build --domain nostore.dev
if ! grep -qF "[error] domain nostore.dev has no secrets store: PROJ/clusters/nostore.dev/secrets does not exist" "${WORK}/go.err" \
  || [[ -e "${PROJ}/clusters/nostore.dev/artifacts.yaml" ]]; then
  fail "domain without a store: no refusal, or an artifact was written"
fi
check "domain without a store, --no-secrets" prep_nostore build --domain nostore.dev --no-secrets
[[ -s "${PROJ}/clusters/nostore.dev/artifacts.yaml" ]] || fail "domain without a store, --no-secrets: no artifact"
rm -rf "${PROJ}/clusters/store.dev" "${PROJ}/clusters/nostore.dev"

# The Secret plugin's keys-only setting (LOK8S_INTERNAL_SECRETS_KEYS_ONLY) is
# internal to lo: only `lo build --keys-only` turns it on. Here the caller
# exports it as 1, with the store-free switch as 1. Both implementations pin
# it to 0 on the render: a plain build writes the generator's real value, a
# --no-secrets build writes no Secret, and no output carries the keys-only
# marker. The one domain that runs the real secrets.lok8s.dev generator
# (literals only: no store value is read or minted) uses a fixture store of
# its own; the plugin is built from this tree into the project's plugin
# home (the bash side runs it from there, lo-full serves the same package).
make -s -C "${ROOT}/kustomize" build BIN_ROOT="${PROJ}/.kustomize" >/dev/null
mkdir -p "${PROJ}/clusters/gen.dev/secrets"
printf 'kind: Lo\nmetadata:\n  name: gen\n' > "${PROJ}/clusters/gen.dev/cluster.lok8s.yaml"
printf 'resources:\n  - cm.yaml\ngenerators:\n  - secret.yaml\n' > "${PROJ}/clusters/gen.dev/kustomization.yaml"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: gen\n  namespace: one\n' > "${PROJ}/clusters/gen.dev/cm.yaml"
printf 'apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: one\nliterals:\n  USER: alice\n' \
  > "${PROJ}/clusters/gen.dev/secret.yaml"
prep_gen() { rm -rf "${PROJ}/clusters/gen.dev/artifacts.yaml" "${PROJ}/clusters/gen.dev/artifacts"; }
export LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 LOK8S_SECRETS_DISABLE=1
check "keys-only setting inherited: build"               prep_gen build --domain gen.dev
for impl in bash go; do
  f="${WORK}/state.${impl}/gen.artifacts.yaml"
  grep -q 'USER: YWxpY2U=' "${f}" 2>/dev/null \
    || fail "keys-only setting inherited: build (${impl}) — the generator did not write its real value"
  ! grep -q 'secrets.lok8s.dev/keys-only' "${f}" 2>/dev/null \
    || fail "keys-only setting inherited: build (${impl}) — the output carries the keys-only marker"
done
check "keys-only setting inherited: build --no-secrets"  prep_gen build --domain gen.dev --no-secrets
for impl in bash go; do
  f="${WORK}/state.${impl}/gen.artifacts.yaml"
  [[ -s "${f}" ]] || fail "keys-only setting inherited: build --no-secrets (${impl}) — no artifact"
  ! grep -qE '^kind: Secret|secrets.lok8s.dev/keys-only' "${f}" 2>/dev/null \
    || fail "keys-only setting inherited: build --no-secrets (${impl}) — the output carries a Secret"
done
# --keys-only: the build itself turns the setting on. The render goes to
# stdout (diffed across the sides), each secrets.lok8s.dev Secret with its
# key and an empty value plus the keys-only marker, never the value, and no
# artifacts.yaml is written. --split and --single contradict it.
check "build --keys-only"                                prep_gen build --domain gen.dev --keys-only
for impl in bash go; do
  if ! grep -q 'secrets.lok8s.dev/keys-only: "true"' "${WORK}/${impl}.out" || ! grep -q 'USER: ""' "${WORK}/${impl}.out"; then
    fail "build --keys-only (${impl}) — stdout lacks the keys-only Secret"
  fi
  ! grep -q 'YWxpY2U=' "${WORK}/${impl}.out" || fail "build --keys-only (${impl}) — stdout carries the value"
  [[ ! -e "${WORK}/state.${impl}/gen.artifacts.yaml" ]] || fail "build --keys-only (${impl}) — artifacts.yaml was written"
done
check "--keys-only/--split exclusion"                    prep_gen build --domain gen.dev --keys-only --split
check "--keys-only/--single exclusion"                   prep_gen build --domain gen.dev --keys-only --single
unset LOK8S_INTERNAL_SECRETS_KEYS_ONLY LOK8S_SECRETS_DISABLE

# The contract of --keys-only: it prints no Secret value, for every Secret
# in the final stream, through the real kustomize. blank.dev has the
# generator's Secret with a patch that adds a data value and stringData, a
# plain Secret manifest, a Secret in a List, and a kustomize
# secretGenerator. Both sides print the same stream (the same yq program
# empties the values), with every key name and no value.
mkdir -p "${PROJ}/clusters/blank.dev"
printf 'kind: Lo\nmetadata:\n  name: blank\n' > "${PROJ}/clusters/blank.dev/cluster.lok8s.yaml"
cat > "${PROJ}/clusters/blank.dev/kustomization.yaml" <<'EOF'
resources:
  - plain.yaml
  - list.yaml
  - commented.yaml
generators:
  - secret.yaml
secretGenerator:
  - name: builtin
    namespace: one
    literals:
      - PASS=builtin-plain
patches:
  - target: {kind: Secret, name: app}
    patch: |-
      - op: add
        path: /data/PATCHED
        value: cGF0Y2hlZA==
      - op: add
        path: /stringData
        value: {EXTRA: extra-plain}
EOF
printf 'apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: one\nliterals:\n  USER: alice\n' \
  > "${PROJ}/clusters/blank.dev/secret.yaml"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: plain\n  namespace: one\ndata:\n  TOKEN: dG9rZW4=\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: visible\n  namespace: one\ndata:\n  VISIBLE: plain-config\n' \
  > "${PROJ}/clusters/blank.dev/plain.yaml"
printf 'apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Secret\n  metadata:\n    name: listed\n    namespace: one\n  stringData:\n    KEY: listed-plain\n' \
  > "${PROJ}/clusters/blank.dev/list.yaml"
# A Secret with values in its labels, annotations and comments.
cat > "${PROJ}/clusters/blank.dev/commented.yaml" <<'EOF'
# head-canary-doc
apiVersion: v1
kind: Secret
metadata:
  name: commented
  namespace: one
  labels:
    copied: label-canary
  annotations:
    note: annotation-canary
stringData:
  # head-canary-key
  NOTE: string-canary # line-canary
  # foot-canary
EOF
check "build --keys-only empties every Secret"           : build --domain blank.dev --keys-only
for impl in bash go; do
  for v in YWxpY2U= cGF0Y2hlZA== extra-plain dG9rZW4= listed-plain YnVpbHRpbi1wbGFpbg==; do
    ! grep -qF -- "${v}" "${WORK}/${impl}.out" || fail "build --keys-only empties every Secret (${impl}) — stdout carries ${v}"
  done
  for k in 'USER: ""' 'PATCHED: ""' 'EXTRA: ""' 'TOKEN: ""' 'KEY: ""' 'PASS: ""' 'NOTE: ""' 'copied: ""' 'note: ""' 'VISIBLE: plain-config'; do
    grep -qF -- "${k}" "${WORK}/${impl}.out" || fail "build --keys-only empties every Secret (${impl}) — stdout lacks ${k}"
  done
  ! grep -q canary "${WORK}/${impl}.out" "${WORK}/${impl}.err" \
    || fail "build --keys-only empties every Secret (${impl}) — a label, annotation or comment value is left: $(grep canary "${WORK}/${impl}.out" "${WORK}/${impl}.err")"
  # app, builtin, plain, listed and commented: one marker each.
  [[ "$(grep -c 'secrets.lok8s.dev/keys-only: "true"' "${WORK}/${impl}.out")" == 5 ]] \
    || fail "build --keys-only empties every Secret (${impl}) — not one keys-only marker per Secret"
done

# The keys-only output of a domain with only a plain Secret carries the
# marker, so a normal build that takes it as a resource is refused (the
# output guard), and nothing is written.
mkdir -p "${PROJ}/clusters/plainonly.dev" "${PROJ}/clusters/reuse.dev/secrets"
printf 'kind: Lo\nmetadata:\n  name: plainonly\n' > "${PROJ}/clusters/plainonly.dev/cluster.lok8s.yaml"
printf 'resources:\n  - plain.yaml\n' > "${PROJ}/clusters/plainonly.dev/kustomization.yaml"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: only\n  namespace: one\ndata:\n  PLAIN: cGxhaW4tdmFsdWU=\n' \
  > "${PROJ}/clusters/plainonly.dev/plain.yaml"
check "build --keys-only marks a plain Secret"           : build --domain plainonly.dev --keys-only
for impl in bash go; do
  grep -q 'secrets.lok8s.dev/keys-only: "true"' "${WORK}/${impl}.out" \
    || fail "build --keys-only marks a plain Secret (${impl}) — no keys-only marker"
done
printf 'kind: Lo\nmetadata:\n  name: reuse\n' > "${PROJ}/clusters/reuse.dev/cluster.lok8s.yaml"
printf 'resources:\n  - keys.yaml\n' > "${PROJ}/clusters/reuse.dev/kustomization.yaml"
cp "${WORK}/go.out" "${PROJ}/clusters/reuse.dev/keys.yaml"
check "build refuses the keys-only output as a resource"  : build --domain reuse.dev
for impl in bash go; do
  grep -qF "it holds a keys-only Secret (secrets.lok8s.dev/keys-only)" "${WORK}/${impl}.err" \
    || fail "build refuses the keys-only output as a resource (${impl}) — no refusal"
  [[ ! -e "${WORK}/state.${impl}/reuse.artifacts.yaml" ]] || fail "build refuses the keys-only output as a resource (${impl}) — artifacts.yaml was written"
done

# A malformed value in a Secret spec fails the build, normal and keys-only,
# with an error that names the section and the key: no part of the value is
# on stdout or stderr. One malformed entry per spec (kustomize hands the
# plugin its config with sorted keys, and the first error stops it): a
# string as the passwd shorthand, and a string as a passwd length.
mkdir -p "${PROJ}/clusters/specerr.dev/secrets"
printf 'kind: Lo\nmetadata:\n  name: specerr\n' > "${PROJ}/clusters/specerr.dev/cluster.lok8s.yaml"
printf 'generators:\n  - secret.yaml\n' > "${PROJ}/clusters/specerr.dev/kustomization.yaml"
for entry in 'shorthand=Zq7-actual-password' 'length={length: Zq7-actual-length}'; do
  printf 'apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: one\npasswd:\n  PASSWORD: %s\n' "${entry#*=}" \
    > "${PROJ}/clusters/specerr.dev/secret.yaml"
  for mode in "" --keys-only; do
    name="build ${mode:+${mode} }names the key of a malformed spec value, not the value (${entry%%=*})"
    check "${name}" : build --domain specerr.dev ${mode:+"${mode}"}
    for impl in bash go; do
      ! grep -q Zq7 "${WORK}/${impl}.out" "${WORK}/${impl}.err" || fail "${name} (${impl}) — the value is printed"
      grep -qF 'passwd.PASSWORD: ' "${WORK}/${impl}.err" || fail "${name} (${impl}) — the error names no key"
      [[ ! -e "${WORK}/state.${impl}/specerr.artifacts.yaml" ]] || fail "${name} (${impl}) — artifacts.yaml was written"
    done
  done
done

# The keys-only run empties the value of each Secret annotation, but a
# Secret with the last-applied annotation (it can hold the values) is
# refused all the same, through the real kustomize: the same message on both
# sides, one line per Secret, nothing on stdout and no value on stderr.
mkdir -p "${PROJ}/clusters/applied.dev"
printf 'kind: Lo\nmetadata:\n  name: applied\n' > "${PROJ}/clusters/applied.dev/cluster.lok8s.yaml"
printf 'resources:\n  - applied.yaml\n' > "${PROJ}/clusters/applied.dev/kustomization.yaml"
cat > "${PROJ}/clusters/applied.dev/applied.yaml" <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: app
  namespace: one
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"kind":"Secret","stringData":{"KEY":"applied-plain"}}'
stringData:
  KEY: applied-plain
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: listed
    namespace: two
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: '{}'
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: visible
  namespace: one
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{}'
EOF
check "build --keys-only refuses the last-applied annotation" : build --domain applied.dev --keys-only
for impl in bash go; do
  [[ ! -s "${WORK}/${impl}.out" ]] || fail "build --keys-only refuses the last-applied annotation (${impl}) — stdout is not empty"
  ! grep -qF applied-plain "${WORK}/${impl}.err" || fail "build --keys-only refuses the last-applied annotation (${impl}) — stderr carries the value"
  [[ "$(grep -c "refusing applied.dev's render" "${WORK}/${impl}.err")" == 2 ]] \
    || fail "build --keys-only refuses the last-applied annotation (${impl}) — not one refusal per Secret: $(cat "${WORK}/${impl}.err")"
  for s in one/app two/listed; do
    grep -qF "[error] lo build --keys-only: refusing applied.dev's render: Secret ${s} has the annotation kubectl.kubernetes.io/last-applied-configuration, which can hold the Secret's values" "${WORK}/${impl}.err" \
      || fail "build --keys-only refuses the last-applied annotation (${impl}) — no refusal for ${s}"
  done
done

# A Secret plugin without the keys-only run is refused before the render.
# The stub answers --capabilities as an older plugin does (dry-run only) and
# logs each call. LO_RENDER=exec: lo-full renders with its own plugin and
# does not check the file, so both Go builds run the exec pipeline here
# (the bash tree always does; it ignores LO_RENDER).
mv "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret" "${WORK}/Secret.built"
cat > "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret" <<EOF
#!/usr/bin/env bash
echo "plugin \$*" >> "${WORK}/old-plugin.log"
echo dry-run
EOF
chmod +x "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
export LO_RENDER=exec
check "build --keys-only refuses an older plugin"        : build --domain blank.dev --keys-only
unset LO_RENDER
grep -qF "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the keys-only run" "${WORK}/go.err" \
  || fail "build --keys-only refuses an older plugin — no refusal on stderr"
[[ ! -s "${WORK}/go.out" && ! -s "${WORK}/bash.out" ]] || fail "build --keys-only refuses an older plugin — stdout is not empty"
[[ "$(cat "${WORK}/old-plugin.log")" == "$(printf 'plugin --capabilities\nplugin --capabilities')" ]] \
  || fail "build --keys-only refuses an older plugin — the plugin got more than the probe: $(cat "${WORK}/old-plugin.log")"
mv "${WORK}/Secret.built" "${PROJ}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"

# An absent plugin home is refused before the render: the real kustomize
# takes the first plugin home that exists, so it would load the old plugin
# under XDG_CONFIG_HOME, which lo did not check. That plugin writes a
# canary; it must not run. LO_RENDER=exec as above.
mv "${PROJ}/.kustomize" "${WORK}/kustomize-home"
mkdir -p "${WORK}/xdg/kustomize/plugin/secrets.lok8s.dev/v1/secret"
cat > "${WORK}/xdg/kustomize/plugin/secrets.lok8s.dev/v1/secret/Secret" <<EOF
#!/usr/bin/env bash
touch "${WORK}/xdg-plugin-ran"
printf 'dry-run\nkeys-only\n'
EOF
chmod +x "${WORK}/xdg/kustomize/plugin/secrets.lok8s.dev/v1/secret/Secret"
export LO_RENDER=exec XDG_CONFIG_HOME="${WORK}/xdg"
check "build --keys-only refuses an absent plugin home"  : build --domain blank.dev --keys-only
unset LO_RENDER XDG_CONFIG_HOME
grep -qF "[error] lo build --keys-only: the kustomize plugin home .kustomize does not exist" "${WORK}/go.err" \
  || fail "build --keys-only refuses an absent plugin home — no refusal on stderr"
[[ ! -s "${WORK}/go.out" && ! -s "${WORK}/bash.out" ]] || fail "build --keys-only refuses an absent plugin home — stdout is not empty"
[[ ! -e "${WORK}/xdg-plugin-ran" ]] || fail "build --keys-only refuses an absent plugin home — the plugin under XDG_CONFIG_HOME ran"
mv "${WORK}/kustomize-home" "${PROJ}/.kustomize"

# The output guard: a render that holds an object with the keys-only marker
# (here a plain Secret resource that carries the annotation) is never written
# into the domain's artifacts. Both sides refuse with the same message and
# write nothing.
mkdir -p "${PROJ}/clusters/guard.dev/secrets"
printf 'kind: Lo\nmetadata:\n  name: guard\n' > "${PROJ}/clusters/guard.dev/cluster.lok8s.yaml"
printf 'resources:\n  - secret.yaml\n' > "${PROJ}/clusters/guard.dev/kustomization.yaml"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: one\n  annotations:\n    secrets.lok8s.dev/keys-only: "true"\ndata:\n  USER: ""\n' \
  > "${PROJ}/clusters/guard.dev/secret.yaml"
# A pre-domain per-target dir (artifacts/old/) stays: the prune runs only
# after the checks.
prep_guard() { # <domain>
  rm -rf "${PROJ}/clusters/${1}/artifacts.yaml" "${PROJ}/clusters/${1}/artifacts"
  mkdir -p "${PROJ}/clusters/${1}/artifacts/old"
  echo "kind: Old" > "${PROJ}/clusters/${1}/artifacts/old/artifacts.yaml"
}
prep_guard_plain() { prep_guard guard.dev; }
check "output guard: keys-only marker refused"           prep_guard_plain build --domain guard.dev
grep -qF "[error] refusing to write guard.dev's render to PROJ/clusters/guard.dev/artifacts.yaml: it holds a keys-only Secret" "${WORK}/go.err" \
  || fail "output guard — no refusal on stderr"
[[ ! -e "${PROJ}/clusters/guard.dev/artifacts.yaml" ]] || fail "output guard — artifacts.yaml was written"
for impl in bash go; do
  grep -qx './artifacts/old/artifacts.yaml' "${WORK}/state.${impl}/guard.dev.tree" \
    || fail "output guard (${impl}) — the refused render pruned artifacts/old"
done
# The guard parses the stream after envsubst: here envsubst writes the
# marker key quoted (a line rule does not see it).
mkdir -p "${PROJ}/clusters/qguard.dev/secrets"
printf 'kind: Lo\nmetadata:\n  name: qguard\n' > "${PROJ}/clusters/qguard.dev/cluster.lok8s.yaml"
printf 'resources:\n  - secret.yaml\n' > "${PROJ}/clusters/qguard.dev/kustomization.yaml"
# shellcheck disable=SC2016 # the build's envsubst expands the marker, not the shell
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: one\n  annotations:\n    ${LOK8S_SPEC_PARITY_MARKER}: "true"\ndata:\n  USER: ""\n' \
  > "${PROJ}/clusters/qguard.dev/secret.yaml"
export LOK8S_SPEC_PARITY_MARKER='"secrets.lok8s.dev/keys-only"'
prep_guard_quoted() { prep_guard qguard.dev; }
check "output guard: quoted marker after envsubst"       prep_guard_quoted build --domain qguard.dev
unset LOK8S_SPEC_PARITY_MARKER
grep -qF "[error] refusing to write qguard.dev's render to PROJ/clusters/qguard.dev/artifacts.yaml: it holds a keys-only Secret" "${WORK}/go.err" \
  || fail "output guard, quoted marker — no refusal on stderr"
for impl in bash go; do
  grep -qx './artifacts/old/artifacts.yaml' "${WORK}/state.${impl}/qguard.dev.tree" \
    || fail "output guard, quoted marker (${impl}) — the refused render pruned artifacts/old"
  ! grep -qx './artifacts.yaml' "${WORK}/state.${impl}/qguard.dev.tree" \
    || fail "output guard, quoted marker (${impl}) — artifacts.yaml was written"
done
rm -rf "${PROJ}/clusters/gen.dev" "${PROJ}/clusters/blank.dev" "${PROJ}/clusters/applied.dev" "${PROJ}/clusters/plainonly.dev" "${PROJ}/clusters/reuse.dev" "${PROJ}/clusters/specerr.dev" "${PROJ}/clusters/guard.dev" "${PROJ}/clusters/qguard.dev" "${PROJ}/.kustomize"

report
