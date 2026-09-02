#!/usr/bin/env bash
# parity-leaves.sh — differential test between the Go lo and the argsh lo for
# the cluster-free leaf commands: init, crds, addons, drivers, chat, ai.
#
# For every case, runs BOTH implementations (the Go binary, and the same
# binary with LO_IMPL=bash forcing the argsh passthrough) against a synthetic
# project and diffs stdout, stderr, and exit codes byte-for-byte (the work dir
# normalized to PROJ). Stateful sections (init scaffolds files, crds writes
# the generated CRDs + the .lok8s mirror, ai links skills) give each
# implementation its OWN project clone and then byte-diff the resulting
# trees — the scaffold bytes and the generated CRDs ARE the gate.
#
# Everything here is hermetic: no cluster, no Tilt, no registry, no network.
# The only external call is `lo drivers lo status <domain>` → `kind get
# clusters` (read-only) on a name that does not exist.
#
# Usage: hack/parity-leaves.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# The harness must run against its synthetic projects ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations — and this harness's WRITES
# (init scaffolds, crds generate, ai link) — into that live repo.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG \
  LO_CHAT_CONFIG

# Pin the C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists directories in byte order (= C collation).
export LC_ALL=C

failures=0

# run_pair <dir-go> <dir-bash> <argv...> — run both implementations in their
# own project dir, diff rc/stdout/stderr with the work dir normalized.
run_pair() {
  local dgo="${1}" dbash="${2}"; shift 2
  local go_rc=0 bash_rc=0
  (cd "${dgo}" && "${LO_BIN}" "$@" </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${dbash}" && LO_IMPL=bash "${LO_BIN}" "$@" </dev/null >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  sed -i "s|${dgo}|PROJ|g" "${WORK}/go.out" "${WORK}/go.err"
  sed -i "s|${dbash}|PROJ|g" "${WORK}/bash.out" "${WORK}/bash.err"

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

# tree_check <dir-go> <dir-bash> <label> — every file under the two clones
# (symlinked framework/toolchain dirs excluded) must be byte-identical, and
# the file LISTS must match. Symlinks are compared by target.
tree_check() {
  local dgo="${1}" dbash="${2}" label="${3}"
  local list_go list_bash
  list_go="$(cd "${dgo}" && find . -path ./.lok8s -prune -o -path ./.bin -prune -o \( -type f -o -type l \) -print | sort)"
  list_bash="$(cd "${dbash}" && find . -path ./.lok8s -prune -o -path ./.bin -prune -o \( -type f -o -type l \) -print | sort)"
  if [[ "${list_go}" != "${list_bash}" ]]; then
    echo "FAIL: tree ${label} — file lists differ:"
    diff <(echo "${list_bash}") <(echo "${list_go}") | head -20 | sed 's/^/  /' || true
    failures=$((failures + 1))
    return
  fi
  local f bad=0
  while IFS= read -r f; do
    [[ -n "${f}" ]] || continue
    if [[ -L "${dgo}/${f}" || -L "${dbash}/${f}" ]]; then
      local tgo tbash
      tgo="$(readlink "${dgo}/${f}" | sed "s|${dgo}|PROJ|")"
      tbash="$(readlink "${dbash}/${f}" | sed "s|${dbash}|PROJ|")"
      if [[ "${tgo}" != "${tbash}" ]]; then
        echo "FAIL: tree ${label} — symlink ${f}: bash=${tbash} go=${tgo}"
        bad=1
      fi
    elif ! cmp -s "${dbash}/${f}" "${dgo}/${f}"; then
      echo "FAIL: tree ${label} — ${f} differs between implementations:"
      diff "${dbash}/${f}" "${dgo}/${f}" | head -10 | sed 's/^/  /' || true
      bad=1
    fi
  done <<< "${list_go}"
  if (( bad )); then
    failures=$((failures + 1))
  else
    echo "ok: tree ${label} ($(echo "${list_go}" | grep -c . ) files identical)"
  fi
}

# expect_rc <rc> <dir> <argv...> — the CONTRACT check on the Go binary alone
# (argsh usage errors exit 2 in bash; the Go tree exits 1 — the documented
# use/version precedent — so those cases are pinned here, not diffed).
expect_rc() {
  local want="${1}" dir="${2}"; shift 2
  local rc=0
  (cd "${dir}" && "${LO_BIN}" "$@" </dev/null >/dev/null 2>&1) || rc=$?
  if (( rc == want )); then
    echo "ok: rc ${want}: lo $*"
  else
    echo "FAIL: lo $* — rc=${rc}, want ${want}"
    failures=$((failures + 1))
  fi
}

# new_project <dir> [copy-lok8s] — a synthetic project: framework tree +
# toolchain linked (or the framework COPIED when the section writes into it).
new_project() {
  local dir="${1}" copy="${2:-0}"
  mkdir -p "${dir}/clusters"
  if (( copy )); then
    cp -R "${ROOT}/.lok8s" "${dir}/.lok8s"
  else
    ln -s "${ROOT}/.lok8s" "${dir}/.lok8s"
  fi
  ln -s "${ROOT}/.bin" "${dir}/.bin"
}

# ── lo init ──────────────────────────────────────────────────────────────────
# Scaffolds into the project (services.yaml, Tiltfile, ./<svc>/lok8s.yaml,
# tests/): one clone per implementation, outputs diffed per call, trees
# byte-diffed at the end.
for impl in go bash; do new_project "${WORK}/init-${impl}"; done
IG="${WORK}/init-go" IB="${WORK}/init-bash"

run_pair "${IG}" "${IB}" init service                          # name required
run_pair "${IG}" "${IB}" init service Foo                      # allowlist
run_pair "${IG}" "${IB}" init service ../evil
run_pair "${IG}" "${IB}" init service foo                      # template + first entry
run_pair "${IG}" "${IB}" init service foo                      # no clobber, still registers
run_pair "${IG}" "${IB}" init service bar --path ./services/bar
run_pair "${IG}" "${IB}" init service bar -p ./services/bar --force
run_pair "${IG}" "${IB}" -v init service baz                   # debug lines
run_pair "${IG}" "${IB}" init service qux extra.positional     # extras ignored (argsh array)
run_pair "${IG}" "${IB}" init test                             # default ./tests
run_pair "${IG}" "${IB}" init test                             # non-empty → warn, still "Done"
run_pair "${IG}" "${IB}" init test --path ./e2e
run_pair "${IG}" "${IB}" init test -p ./e2e --force
for impl in go bash; do printf 'docker_build("x", ".")\n' > "${WORK}/init-${impl}/Tiltfile"; done
run_pair "${IG}" "${IB}" init service tiltcheck                # hand-rolled Tiltfile → warn, untouched
for impl in go bash; do printf 'print(1)\n' > "${WORK}/init-${impl}/Tiltfile"; done
run_pair "${IG}" "${IB}" init service tiltcheck2               # unknown form → warn
for impl in go bash; do printf 'x' > "${WORK}/init-${impl}/notadir"; done
run_pair "${IG}" "${IB}" init test --path ./notadir            # destination is a file → error
tree_check "${IG}" "${IB}" "init scaffolds"
expect_rc 0 "${IG}" init                                       # bare → help (cobra text, not diffed)
expect_rc 1 "${IG}" init bogus

# ── lo crds ──────────────────────────────────────────────────────────────────
# generate writes operator/crds/*.yaml AND the .lok8s inventory mirror, so
# each clone carries a private COPY of the framework tree. The schema source
# is the repo's real one; the generated CRDs must equal the committed ones.
for impl in go bash; do
  new_project "${WORK}/crds-${impl}" 1
  mkdir -p "${WORK}/crds-${impl}/operator/crds/schema"
  cp "${ROOT}"/operator/crds/schema/*.schema.yaml "${WORK}/crds-${impl}/operator/crds/schema/"
  rm -f "${WORK}/crds-${impl}/.lok8s/libs/inventory/manifests/clusterinventory.crd.yaml"
done
CG="${WORK}/crds-go" CB="${WORK}/crds-bash"
run_pair "${CG}" "${CB}" crds check                            # nothing generated yet → every kind STALE
run_pair "${CG}" "${CB}" crds generate
run_pair "${CG}" "${CB}" crds check
run_pair "${CG}" "${CB}" crds c
run_pair "${CG}" "${CB}" crds g
tree_check "${CG}" "${CB}" "crds generate"
for f in "${ROOT}"/operator/crds/*.yaml; do
  if cmp -s "${f}" "${CG}/operator/crds/$(basename "${f}")"; then
    echo "ok: crds $(basename "${f}") == committed"
  else
    echo "FAIL: crds $(basename "${f}") differs from the committed CRD"
    failures=$((failures + 1))
  fi
done
if cmp -s "${CG}/.lok8s/libs/inventory/manifests/clusterinventory.crd.yaml" "${ROOT}/.lok8s/libs/inventory/manifests/clusterinventory.crd.yaml"; then
  echo "ok: crds inventory mirror == committed"
else
  echo "FAIL: crds inventory mirror differs from the committed mirror"
  failures=$((failures + 1))
fi
for impl in go bash; do
  printf '  hand-edited: true\n' >> "${WORK}/crds-${impl}/operator/crds/lo.yaml"
  printf '  hand-edited: true\n' >> "${WORK}/crds-${impl}/.lok8s/libs/inventory/manifests/clusterinventory.crd.yaml"
done
run_pair "${CG}" "${CB}" crds check                            # drift → STALE (both artifacts), rc 1
run_pair "${CG}" "${CB}" crds generate                         # regenerate heals
run_pair "${CG}" "${CB}" crds check
tree_check "${CG}" "${CB}" "crds regenerate"
expect_rc 1 "${CG}" crds bogus

# ── lo addons ────────────────────────────────────────────────────────────────
# Read-only over the real framework addon tree; synthetic domains cover the
# driver default, an explicit bootstrap list (framework + target + unknown +
# map-form), an empty opt-out, a deploy-only domain and a malformed kind.
new_project "${WORK}/addons"
AD="${WORK}/addons"
mkdir -p "${AD}/clusters/alpha.dev" "${AD}/clusters/beta.cloud/targets/networking" \
  "${AD}/clusters/gamma.app" "${AD}/clusters/bad.kind" "${AD}/clusters/empty.list"
printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${AD}/clusters/alpha.dev/cluster.lok8s.yaml"
cat > "${AD}/clusters/beta.cloud/cluster.lok8s.yaml" <<'EOF'
kind: KubeOne
metadata:
  name: beta
spec:
  bootstrap:
    - cilium: { wait: true }
    - cert-manager
    - ccm:
        values:
          env:
            ROBOT_ENABLED: { value: "true" }
    - ./targets/networking: { dependsOn: [cert-manager] }
    - /abs/glue
    - nope-addon
    - { name: renamed, path: metallb }
EOF
printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${AD}/clusters/gamma.app/deploy.lok8s.yaml"
printf 'kind: "a b"\nmetadata:\n  name: bad\nspec:\n  bootstrap: [cilium]\n' > "${AD}/clusters/bad.kind/cluster.lok8s.yaml"
printf 'kind: KubeOne\nmetadata:\n  name: e\nspec:\n  bootstrap: []\n' > "${AD}/clusters/empty.list/cluster.lok8s.yaml"

run_pair "${AD}" "${AD}" addons
run_pair "${AD}" "${AD}" a
run_pair "${AD}" "${AD}" addons --domain alpha.dev
run_pair "${AD}" "${AD}" addons --domain gamma.app
run_pair "${AD}" "${AD}" addons --domain bad.kind              # malformed kind → error
run_pair "${AD}" "${AD}" addons --domain ../x                  # no spec there → lo
run_pair "${AD}" "${AD}" addons cilium
run_pair "${AD}" "${AD}" addons cilium metallb                 # separator
run_pair "${AD}" "${AD}" a mailpit                             # chart: ./chart, no version
run_pair "${AD}" "${AD}" addons oidc-rbac                      # raw addon
run_pair "${AD}" "${AD}" addons nope
run_pair "${AD}" "${AD}" addons cilium --detail                # positionals win over --detail
run_pair "${AD}" "${AD}" addons --detail                       # default domain lok8s.dev → no spec
run_pair "${AD}" "${AD}" addons --detail --domain alpha.dev    # lo default → cilium
run_pair "${AD}" "${AD}" addons --detail --domain beta.cloud
run_pair "${AD}" "${AD}" addons --detail --domain empty.list
run_pair "${AD}" "${AD}" addons --detail --domain gamma.app
run_pair "${AD}" "${AD}" addons --detail --domain bad.kind
run_pair "${AD}" "${AD}" addons --detail --domain ../x
run_pair "${AD}" "${AD}" addons --detail --domain .hidden
printf 'beta.cloud\n' > "${AD}/clusters/.active"
run_pair "${AD}" "${AD}" addons --detail                       # .active domain
DOMAIN_NAME=alpha.dev run_pair "${AD}" "${AD}" addons --detail # env outranks .active (notice line)
rm -f "${AD}/clusters/.active"

# ── lo drivers ───────────────────────────────────────────────────────────────
run_pair "${AD}" "${AD}" drivers --list                        # union: Go registry ∪ bash dirs
run_pair "${AD}" "${AD}" drivers -l
run_pair "${AD}" "${AD}" drivers
run_pair "${AD}" "${AD}" drivers ../x
run_pair "${AD}" "${AD}" drivers 'bad name'
run_pair "${AD}" "${AD}" drivers bogus
expect_rc 0 "${AD}" drivers kubehz                             # Go driver since the kubehz port: cobra help vs argsh usage (documented), rc parity only
run_pair "${AD}" "${AD}" drivers lo status alpha.dev           # read-only `kind get clusters`, absent name
run_pair "${AD}" "${AD}" drivers lo s alpha.dev
run_pair "${AD}" "${AD}" drivers kubeone status beta.cloud     # NotProvisioned (no work dir)
expect_rc 1 "${AD}" drivers lo status                          # missing domain (argsh: 2)
expect_rc 1 "${AD}" drivers lo status alpha.dev extra
expect_rc 1 "${AD}" drivers lo bogus

# ── lo chat / lo ai ──────────────────────────────────────────────────────────
# Argument/usage errors only: lochat is absent from the pinned toolchain, so
# `lo chat` and `lo ai check` fail at the preflight in both implementations.
# The section is skipped when a lochat is reachable on PATH — the bash
# implementation would exec it.
if command -v lochat >/dev/null 2>&1 || [[ -x "${ROOT}/.bin/lochat" ]]; then
  echo "skip: chat/ai — a lochat binary is reachable; the bash impl would exec it"
else
  for impl in go bash; do new_project "${WORK}/ai-${impl}"; done
  AG="${WORK}/ai-go" AB="${WORK}/ai-bash"
  run_pair "${AG}" "${AB}" chat
  run_pair "${AG}" "${AB}" chat --check
  run_pair "${AG}" "${AB}" chat -p hello --model x
  run_pair "${AG}" "${AB}" ai check                            # runtime error + skills error
  run_pair "${AG}" "${AB}" ai skills                           # no skills dir
  run_pair "${AG}" "${AB}" ai link
  run_pair "${AG}" "${AB}" ai link claude --copy
  run_pair "${AG}" "${AB}" ai link bogus
  run_pair "${AG}" "${AB}" ai unlink                           # nothing linked
  run_pair "${AG}" "${AB}" ai unlink bogus
  for impl in go bash; do
    mkdir -p "${WORK}/ai-${impl}/skills/beta" "${WORK}/ai-${impl}/skills/alpha" "${WORK}/ai-${impl}/skills/notaskill"
    printf '# beta\n' > "${WORK}/ai-${impl}/skills/beta/SKILL.md"
    printf '# alpha\n' > "${WORK}/ai-${impl}/skills/alpha/SKILL.md"
    printf 'x\n' > "${WORK}/ai-${impl}/skills/alpha/extra.txt"
    printf 'no SKILL.md here\n' > "${WORK}/ai-${impl}/skills/notaskill/README.md"
  done
  run_pair "${AG}" "${AB}" ai skills
  run_pair "${AG}" "${AB}" ai check
  run_pair "${AG}" "${AB}" ai link
  run_pair "${AG}" "${AB}" ai skills                           # linked
  tree_check "${AG}" "${AB}" "ai link (symlinks)"
  run_pair "${AG}" "${AB}" ai unlink
  run_pair "${AG}" "${AB}" ai skills
  run_pair "${AG}" "${AB}" ai link claude -c
  run_pair "${AG}" "${AB}" ai skills                           # copied
  tree_check "${AG}" "${AB}" "ai link --copy"
  for impl in go bash; do
    ln -s "${WORK}/ai-${impl}/skills/gone" "${WORK}/ai-${impl}/.claude/skills/gone"   # dangling, ours
    ln -s /elsewhere/thing "${WORK}/ai-${impl}/.claude/skills/foreign"              # not ours
    mkdir -p "${WORK}/ai-${impl}/.claude/skills/theirs"                              # not a skill
  done
  run_pair "${AG}" "${AB}" ai unlink                           # 3 removed, foreign + theirs kept
  tree_check "${AG}" "${AB}" "ai unlink"
  expect_rc 1 "${AG}" ai bogus
fi

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
