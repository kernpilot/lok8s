#!/usr/bin/env bash
# parity-loop.sh — differential test between the Go lo and the argsh lo for
# the Tilt-loop surface: `lo tilt`, `lo image`, `lo env`, `lo hooks`.
#
# Modeled on parity-test.sh: for every covered invocation, runs BOTH
# implementations (the Go binary, and the same binary routed to the frozen tree
# by the project file) against a synthetic project and diffs
# stdout, stderr, and exit codes.
#
# READ-ONLY / ERROR PATHS ONLY. This surface's destructive verbs talk to live
# Tilt sessions, kind clusters and the docker daemon — none of which a parity
# harness may touch. The synthetic project therefore gets its own .bin with
# STUB tilt/kubectl/docker/kind binaries (both implementations resolve tools
# through that directory first), so even the "destructive" invocations below
# only ever exec the stubs. The genuinely dangerous flows (tilt down with a
# pid file → kill/pkill, image cache pulls, image clean, kapply patches) are
# covered by hermetic unit tests with fake runners instead.
#
# Usage: hack/parity-loop.sh [path-to-go-lo]   (default: bin/lo)
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
unset TILT_PORT LOK8S_SERVICE_CONFIG \
  LOK8S_REGISTRY_IP_CACHE LOK8S_REGISTRY_JSON LOK8S_PREFLIGHT \
  LOK8S_FORCE_CLEAR_TERMINATING KIND_EXPERIMENTAL_DOCKER_NETWORK \
  DOCKER_REGISTRY DOCKER_PROJECT DOCKER_TAG PARITY_TAG

# indent — prefix every line on stdin with two spaces (a per-line anchor is
# not expressible as a parameter expansion, so no sed; keeps SC2001 quiet).
indent() { while IFS= read -r line; do printf '  %s\n' "${line}"; done; }

STUB_HTTP_PID=""
parity_cleanup() { [[ -n "${STUB_HTTP_PID}" ]] && kill "${STUB_HTTP_PID}" 2>/dev/null || :; }

# mkproj <dir> — one synthetic project: three domains (Lo / KubeOne /
# deploy), the framework tree, and a guarded .bin (real yq/jq/argsh, GNU
# envsubst, STUBS for every tool that could reach live state).
mkproj() {
  local dir="${1}"
  parity::new_project "${dir}"
  parity::own_bin "${dir}"
  mkdir -p "${dir}/clusters/alpha.dev" "${dir}/clusters/beta.cloud" "${dir}/clusters/gamma.app"
  printf 'kind: Lo\nmetadata:\n  name: alpha\nspec:\n  network:\n    name: paritynet\n    cidr: 10.99.7.0/24\n' \
    > "${dir}/clusters/alpha.dev/cluster.lok8s.yaml"
  printf 'kind: KubeOne\nmetadata:\n  name: beta\n' > "${dir}/clusters/beta.cloud/cluster.lok8s.yaml"
  printf 'kind: Deploy\nspec:\n  clusterRef:\n    domain: beta.cloud\n' > "${dir}/clusters/gamma.app/deploy.lok8s.yaml"
  printf 'Tiltfile\n' > "${dir}/Tiltfile"

  # The live-state tools REPLACED by stubs.
  parity::stub "${dir}" tilt <<'SH'
#!/usr/bin/env bash
# Parity stub. doctor: a kind env; get session: no apiserver; ci: fail 7
# (the rc-passthrough probe); everything else: succeed silently.
case "${1:-}" in
  doctor) echo "Env: kind"; exit 0 ;;
  get)    exit 1 ;;
  ci)     exit 7 ;;
esac
exit 0
SH
  parity::stub_kubectl_fail "${dir}"
  parity::stub_refuse "${dir}" docker
  parity::stub_refuse "${dir}" kind
  # GNU envsubst, pinned: the dev PATH may resolve a non-GNU envsubst whose
  # semantics differ; the Go port implements the GNU contract.
  if [[ -x /usr/bin/envsubst ]]; then
    ln -sf /usr/bin/envsubst "${dir}/.bin/envsubst"
  fi
}

mkproj "${PROJ}"

# check_stdin <manifest> <argv...> — like check, with a manifest on stdin
# (the `lo tilt preflight` shape: local(..., stdin = artifacts)).
check_stdin() {
  local stdin="${1}"; shift
  PARITY_STDIN="${stdin}" parity::run_pair "$@"
  parity::finish "lo $* (stdin)"
}

# ── lo env services ──────────────────────────────────────────────────────────

check - env services                       # no services.yaml → {}
cat > "${PROJ}/services.yaml" <<'YAML'
registry:
  prefix: lok8s.local
  endpoint: ghcr.io/myorg
  branch: parity
  tag: ${PARITY_TAG}
defaults:
  build: false
services:
  api:
    build: true
  worker:
    build: false
  pinned:
    image: ghcr.io/external/pinned:v1.2.3
YAML
check - env services
check - env services --only-services
check - env services --only-registry
# argsh routes the -s/-r shorthands to the INHERITED globals here (cluster /
# remote), not to --only-*; both are consumed no-ops for the output.
check - env services -s somecluster
check - env services -r
export PARITY_TAG=v42
check - env services --only-registry      # envsubst pass (defined var)
unset PARITY_TAG
cat > "${PROJ}/services.local.yaml" <<'YAML'
services:
  api:
    build: false
YAML
export LOK8S_SERVICE_CONFIG=local
check - env services
check - env services --only-services
unset LOK8S_SERVICE_CONFIG

# ── lo env kustomization — state parity (per-impl clones) ────────────────────
# Stateful (writes clusters/<domain>/artifacts/); each implementation gets its
# OWN clone and the produced files must be byte-identical.

for impl in go bash; do
  mkproj "${WORK}/envk-${impl}"
  cp "${PROJ}/services.yaml" "${WORK}/envk-${impl}/services.yaml"
  printf '# placeholder\n' > "${WORK}/envk-${impl}/clusters/alpha.dev/artifacts.yaml"
done
PARITY_DIR_GO="${WORK}/envk-go" PARITY_DIR_BASH="${WORK}/envk-bash" \
  parity::run_pair --domain alpha.dev env kustomization --no-build
envk_ok=1
parity::compare "env kustomization" || envk_ok=0
for state in clusters/alpha.dev/artifacts/kustomization.yaml clusters/alpha.dev/artifacts/.cache-queue; do
  parity::state_same "${WORK}/envk-bash/${state}" "${WORK}/envk-go/${state}" "${state}" || envk_ok=0
done
parity::record "${envk_ok}" "lo env kustomization --no-build (state)"

# ── lo env kustomization — services.<name>.patches (per-impl clones) ─────────
# Every case gets fresh clones with the same services.yaml (stdin) and the
# same domain artifact (ENVP_ART). A passing case writes the same overlay in
# both; a failing case prints the same [error] line and writes NO overlay and
# NO cache queue in either (the check runs before anything is written).

ENVP_ARTIFACT='apiVersion: apps/v1
kind: Deployment
metadata:
  name: web.app
  namespace: ns1
  labels:
    lok8s.dev/name: web
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webXapp
  namespace: ns1
  labels:
    lok8s.dev/name: web
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: ns1
  labels:
    lok8s.dev/name: web
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: ns1
  labels:
    lok8s.dev/name: api
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web.app
  namespace: ns2
  labels:
    lok8s.dev/name: api'

# envp_case <label> <want-rc>: services.yaml on stdin. ENVP_ALLOW: an
# allow-diff regex for lines outside this feature.
envp_case() {
  local label="${1}" want_rc="${2}" services impl ok=1 state
  services="$(cat)"
  for impl in go bash; do
    rm -rf "${WORK}/envp-${impl}"
    mkproj "${WORK}/envp-${impl}"
    printf '%s\n' "${services}" > "${WORK}/envp-${impl}/services.yaml"
    printf '%s\n' "${ENVP_ART:-${ENVP_ARTIFACT}}" > "${WORK}/envp-${impl}/clusters/alpha.dev/artifacts.yaml"
  done
  PARITY_DIR_GO="${WORK}/envp-go" PARITY_DIR_BASH="${WORK}/envp-bash" \
    parity::run_pair --domain alpha.dev env kustomization --no-build
  parity::compare "env kustomization patches (${label})" "${ENVP_ALLOW:--}" || ok=0
  # shellcheck disable=SC2154 # go_rc: set by parity::run_pair (hack/lib/parity.sh)
  if (( go_rc != want_rc )); then
    echo "FAIL: env kustomization patches (${label}) — rc ${go_rc}, want ${want_rc}"
    ok=0
  fi
  for state in kustomization.yaml .cache-queue; do
    local fgo="${WORK}/envp-go/clusters/alpha.dev/artifacts/${state}"
    local fbash="${WORK}/envp-bash/clusters/alpha.dev/artifacts/${state}"
    if (( want_rc != 0 )); then
      if [[ -e "${fgo}" || -e "${fbash}" ]]; then
        echo "FAIL: env kustomization patches (${label}) — ${state} written on a failed run"
        ok=0
      fi
    else
      parity::state_same "${fbash}" "${fgo}" "patches (${label}) ${state}" || ok=0
    fi
  done
  parity::record "${ok}" "lo env kustomization --no-build (patches: ${label})"
}

# The first line of the Service patch starts with two spaces (|2: the
# content indentation is the key's plus two), and the patch holds an empty
# line: the emit must keep both. Without the emit's own indentation
# indicator, that first line would set the block's indentation and the
# lines after it would end the block: the render check below would fail.
envp_case "emit" 0 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app, group: apps}
        patch: |
          - op: add
            path: /metadata/annotations
            value: {patched: "yes"}
      - target: {version: v1, kind: Service, name: web, namespace: ns1}
        patch: |2
            # an indented first line
          - op: add
            path: /metadata/labels/extra
            value: "1"

          # the end
  api:
    enabled: false
    patches:
      - target: {kind: Deployment, name: not-checked}
        patch: x
  worker:
    patches: []
YAML

# The overlay must RENDER, and the patches must land on exactly the named
# objects: web.app in ns1 (not webXapp, which an unescaped dot would match,
# and not api's web.app in ns2, which a target without the label selector
# would match) and the web Service; api stays untouched.
envp_render_ok=1
if envp_render="$("${ROOT}/.bin/kustomize" build --load-restrictor=LoadRestrictionsNone \
    "${WORK}/envp-go/clusters/alpha.dev/artifacts" 2>&1)"; then
  # kustomize sorts its output: compare the sorted lines.
  envp_got="$(printf '%s\n' "${envp_render}" | "${ROOT}/.bin/yq" -r '.metadata.namespace + "/" + .metadata.name + " " + (.metadata.annotations.patched // "-") + " " + (.metadata.labels.extra // "-")' | grep -v '^---$' | LC_ALL=C sort)"
  envp_want="$(printf '%s\n' 'ns1/api - -' 'ns1/web - 1' 'ns1/web.app yes -' 'ns1/webXapp - -' 'ns2/web.app - -')"
  if [[ "${envp_got}" != "${envp_want}" ]]; then
    echo "FAIL: env kustomization patches (render) — got:"
    printf '%s\n' "${envp_got}" | sed 's/^/  /'
    envp_render_ok=0
  fi
else
  echo "FAIL: env kustomization patches (render) — kustomize build failed:"
  printf '%s\n' "${envp_render}" | head -10 | sed 's/^/  /'
  envp_render_ok=0
fi
parity::record "${envp_render_ok}" "lo env kustomization --no-build (patches: the overlay renders, exact targets)"

# A domain artifact document that is not a mapping is skipped, not an error.
ENVP_ART="${ENVP_ARTIFACT}"$'\n---\n- not\n- a mapping' envp_case "non-mapping document" 0 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: x
YAML
# kustomize matches a target namespace against the effective one (an object
# without a namespace is in "default"), and a colon (RBAC names) is a plain
# character. Both must pass the check AND be patched by kustomize.
ENVP_NONS='apiVersion: apps/v1
kind: Deployment
metadata:
  name: nons
  labels:
    lok8s.dev/name: web
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: system:web
  labels:
    lok8s.dev/name: web'
ENVP_ART="${ENVP_ARTIFACT}"$'\n---\n'"${ENVP_NONS}" envp_case "default namespace and a colon" 0 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: nons, namespace: default}
        patch: |
          - op: add
            path: /metadata/annotations
            value: {patched: "yes"}
      - target: {kind: ClusterRole, name: "system:web"}
        patch: |
          - op: add
            path: /metadata/annotations
            value: {patched: "yes"}
YAML
envp_ns_ok=1
if envp_render="$("${ROOT}/.bin/kustomize" build --load-restrictor=LoadRestrictionsNone \
    "${WORK}/envp-go/clusters/alpha.dev/artifacts" 2>&1)"; then
  envp_got="$(printf '%s\n' "${envp_render}" | "${ROOT}/.bin/yq" -r 'select(.metadata.name == "nons" or .metadata.name == "system:web") | .metadata.name + " " + (.metadata.annotations.patched // "-")' | grep -v '^---$' | LC_ALL=C sort)"
  if [[ "${envp_got}" != "$(printf '%s\n' 'nons yes' 'system:web yes')" ]]; then
    echo "FAIL: env kustomization patches (default namespace render) — got:"
    printf '%s\n' "${envp_got}" | sed 's/^/  /'
    envp_ns_ok=0
  fi
else
  echo "FAIL: env kustomization patches (default namespace render) — kustomize build failed:"
  printf '%s\n' "${envp_render}" | head -10 | sed 's/^/  /'
  envp_ns_ok=0
fi
parity::record "${envp_ns_ok}" "lo env kustomization --no-build (patches: kustomize patches the default-namespace and colon targets)"
ENVP_ART="${ENVP_ARTIFACT}"$'\n---\n'"${ENVP_NONS}" envp_case "other namespace on an unset one" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: nons, namespace: ns1}
        patch: x
YAML
# A malformed services config has no patches; the run is not failed here
# (lint and the Tilt extension refuse it). The image-swap half differs on it
# before this feature: the bash reads `build` of a non-mapping as "" (a
# swallowed yq error) and warns, the binary reads "null" and does not.
ENVP_ALLOW="^warn: service '[^']*' has build:false" envp_case "service not a mapping" 0 <<'YAML'
services:
  web: just-a-string
YAML
ENVP_ALLOW="^warn: service '[^']*' has build:false" envp_case "services a list" 0 <<'YAML'
services:
  - web
YAML
ENVP_ALLOW="^Error: cannot get keys of" envp_case "services a string" 0 <<'YAML'
services: web
YAML
envp_case "not a list" 1 <<'YAML'
services:
  web:
    patches: {kind: Deployment}
YAML
envp_case "not a label value" 1 <<'YAML'
services:
  web_:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: x
YAML
envp_case "entry not a mapping" 1 <<'YAML'
services:
  web:
    patches:
      - just a string
YAML
envp_case "unknown entry key" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: x
        path: ./p.yaml
YAML
envp_case "patch not a string" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: {op: add}
YAML
envp_case "patch empty" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: "\n\n"
YAML
envp_case "patch control character" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: "a\rb"
YAML
# Characters a YAML 1.1 reader breaks a line at, and a NUL: rejected before
# the bash side reads the patch into a variable.
envp_case "patch line separator" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: "- op: add\u2028  path: /x"
YAML
envp_case "patch next line" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: "a\x85b"
YAML
envp_case "patch NUL" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: "a\0b"
YAML
envp_case "patch only whitespace" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app}
        patch: "  \t\n  \n"
YAML
envp_case "target value with a trailing newline" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: "web.app\n"}
        patch: x
YAML
# An integer target value is read as its text ("1"); it fails here because
# no object has version 1, in both implementations alike.
envp_case "integer target value, no match" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app, version: 1}
        patch: x
YAML
envp_case "target missing" 1 <<'YAML'
services:
  web:
    patches:
      - patch: x
YAML
envp_case "unknown target key" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app, labelSelector: a=b}
        patch: x
YAML
envp_case "name required" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: null}
        patch: x
YAML
envp_case "regex in name" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: "web.*"}
        patch: x
YAML
envp_case "null optional field" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app, namespace: ~}
        patch: x
YAML
envp_case "no match" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: webYapp}
        patch: x
YAML
envp_case "other service's object" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: api}
        patch: x
YAML
envp_case "wrong group" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app, group: batch}
        patch: x
YAML
envp_case "wrong namespace" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Deployment, name: web.app, namespace: ns2}
        patch: x
YAML
envp_case "wrong version" 1 <<'YAML'
services:
  web:
    patches:
      - target: {kind: Service, name: web, version: v2}
        patch: x
YAML

# ── lo hooks — validation + no-match paths (cluster-free) ────────────────────

check - hooks recreate                                  # missing --selector
check - hooks apply
check - hooks restart
check - hooks recreate --selector 'a=b;rm -rf /'        # injection guard
check - hooks apply --selector noequalshere             # clause shape
check - hooks recreate --selector 'lok8s.dev/role=ghost'  # no artifact → no match
mkdir -p "${PROJ}/clusters/lok8s.dev"
printf 'kind: Job\nmetadata:\n  name: j1\n  labels: {app: other}\n' \
  > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
check - hooks restart --selector app=nomatch            # artifact present, no match

# An artifact with a keys-only Secret (lo build --keys-only output), and one
# that does not parse: the three verbs refuse it in both implementations
# (rc 1, the same message), also when the selector matches only another
# object. A kubectl that logs its argv shows that no verb reaches kubectl.
parity::stub "${PROJ}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub: log the argv, drain a manifest on stdin, fail (no live
# cluster may be reached).
echo "kubectl $*" >> "${PARITY_KUBECTL_LOG}"
[[ " $* " != *" -f - "* ]] || cat > /dev/null
exit 1
SH
export PARITY_KUBECTL_LOG="${WORK}/kubectl.log"
for hk_case in marker no-yaml; do
  if [[ "${hk_case}" == marker ]]; then
    printf 'kind: Job\nmetadata:\n  name: j1\n  labels: {lok8s.dev/role: seed}\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  annotations: {secrets.lok8s.dev/keys-only: "true"}\ndata:\n  PASSWORD: ""\n' \
      > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
    hk_want='holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one'
  else
    printf 'kind: Job\nmetadata: [a\n' > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
    hk_want='does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)'
  fi
  for hk_verb in recreate apply restart; do
    hk_label="lo hooks ${hk_verb} refuses an artifact (${hk_case})"
    rm -f "${PARITY_KUBECTL_LOG}"
    parity::run_pair hooks "${hk_verb}" --selector lok8s.dev/role=seed
    hk_ok=1
    parity::compare "${hk_label}" - || hk_ok=0
    # shellcheck disable=SC2154 # go_rc, bash_rc: set by parity::run_pair (hack/lib/parity.sh)
    if (( go_rc != 1 || bash_rc != 1 )) \
        || ! grep -qF "refusing to apply lok8s.dev: PROJ/clusters/lok8s.dev/artifacts.yaml ${hk_want}" "${WORK}/go.err"; then
      echo "FAIL: ${hk_label} — rc go=${go_rc} bash=${bash_rc}, stderr:"
      indent < "${WORK}/go.err"
      hk_ok=0
    fi
    if [[ -s "${PARITY_KUBECTL_LOG}" ]]; then
      echo "FAIL: ${hk_label} — kubectl ran:"
      indent < "${PARITY_KUBECTL_LOG}"
      hk_ok=0
    fi
    parity::record "${hk_ok}" "${hk_label}"
  done
done

# The positive control: a clean artifact, a selector that matches. Each
# verb reaches kubectl in both implementations. The stub prints, for each
# manifest on stdin, the digest of its canonical form (yq, keys sorted):
# the Go stream is yaml.v3's and the bash stream is yq's, so the bytes can
# differ, and the values cannot. A last |+ block scalar keeps its final
# newlines in both.
parity::stub "${PROJ}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub: log the argv, print the canonical digest of a manifest on
# stdin, answer a get with "not found" (no live cluster may be reached).
echo "kubectl $*" >> "${PARITY_KUBECTL_LOG}"
case " $* " in
  *" -f - "*) printf 'parity-stub -f %s\n' "$(yq -o=json -I=0 'sort_keys(..)' - | sha256sum | cut -d' ' -f1)" ;;
  *" get "*) exit 1 ;;
esac
exit 0
SH
printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: default\n  labels: {lok8s.dev/role: seed}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  labels: {lok8s.dev/role: seed}\ndata:\n  tail: |+\n    x\n\n\n' \
  > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
for hk_verb in recreate apply restart; do
  hk_label="lo hooks ${hk_verb} reaches kubectl with the selected objects"
  rm -f "${PARITY_KUBECTL_LOG}"
  parity::run_pair hooks "${hk_verb}" --selector lok8s.dev/role=seed
  hk_ok=1
  parity::compare "${hk_label}" - || hk_ok=0
  case "${hk_verb}" in
    restart) hk_want=' rollout restart deployment/web' ;;
    *)       hk_want=' apply --server-side --force-conflicts -f -' ;;
  esac
  # The log holds the calls of both runs (Go first): each verb reached kubectl.
  if (( go_rc != 0 )) || [[ "$(grep -cF -- "${hk_want}" "${PARITY_KUBECTL_LOG}" 2>/dev/null)" != 2 ]]; then
    echo "FAIL: ${hk_label} — rc go=${go_rc}, kubectl calls:"
    indent < "${PARITY_KUBECTL_LOG}"
    hk_ok=0
  fi
  # Each implementation applies the objects of the artifact: the digest is
  # in the stdout of both runs.
  if [[ "${hk_verb}" != restart ]]; then
    hk_digest="$(yq -o=json -I=0 'sort_keys(..)' "${PROJ}/clusters/lok8s.dev/artifacts.yaml" | sha256sum | cut -d' ' -f1)"
    for hk_impl in go bash; do
      if ! grep -qF -- "parity-stub -f ${hk_digest}" "${WORK}/${hk_impl}.out"; then
        echo "FAIL: ${hk_label} — the objects that ${hk_impl} applied are not the artifact's (digest ${hk_digest}):"
        indent < "${WORK}/${hk_impl}.out"
        hk_ok=0
      fi
    done
  fi
  parity::record "${hk_ok}" "${hk_label}"
done

# lo deploy reads artifacts.yaml once, checks that copy and applies that copy
# (the full apply and -l key=value). The kubectl stub logs its argv and
# prints a digest of each manifest that it gets: for a clean artifact both
# implementations apply the same manifests; a marked artifact and one that
# does not parse are refused (rc 1, the same message) before any kubectl
# call.
parity::stub "${PROJ}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub: log the argv, print the digest of the bytes of an applied
# manifest, answer a get with no objects (no live cluster may be reached).
echo "kubectl $*" >> "${PARITY_KUBECTL_LOG}"
case " $* " in
  *" apply "*) printf 'parity-stub applied %s\n' "$(sha256sum | cut -d' ' -f1)" ;;
  *" get "*) printf '{"items":[]}' ;;
esac
exit 0
SH
export LOK8S_NONINTERACTIVE=1
for dp_case in clean plus marker no-yaml; do
  case "${dp_case}" in
    clean)
      printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.test.lok8s.dev\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: system\n  labels: {lok8s.dev/type: system}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: platform\n  labels: {lok8s.dev/type: platform}\n' \
        > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
      dp_want='' ;;
    plus)
      # The last literal scalar (|+) of the CRD subset, of the label subset
      # and of the artifact ends in several newlines: kubectl gets them.
      printf 'apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.test.lok8s.dev\n  annotations:\n    note: |+\n      crd\n\n\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: platform\n  labels: {lok8s.dev/type: platform}\ndata:\n  tail: |+\n    x\n\n\n' \
        > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
      dp_want='' ;;
    marker)
      printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: platform\n  labels: {lok8s.dev/type: platform}\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  labels: {lok8s.dev/type: platform}\n  annotations: {secrets.lok8s.dev/keys-only: "true"}\ndata:\n  PASSWORD: ""\n' \
        > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
      dp_want='holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one' ;;
    no-yaml)
      printf 'kind: ConfigMap\nmetadata: [a\n' > "${PROJ}/clusters/lok8s.dev/artifacts.yaml"
      dp_want='does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)' ;;
  esac
  for dp_route in full filtered; do
    dp_label="lo deploy (${dp_route}) refuses an artifact (${dp_case})"
    [[ -n "${dp_want}" ]] || dp_label="lo deploy (${dp_route}) applies the checked copy (${dp_case})"
    # The full apply has two phases (the CRDs, then all), the subset one.
    dp_args=(deploy --domain lok8s.dev) dp_n=2
    [[ "${dp_route}" == full ]] || { dp_args+=(-l lok8s.dev/type=platform); dp_n=1; }
    rm -f "${PARITY_KUBECTL_LOG}"
    parity::run_pair "${dp_args[@]}"
    dp_ok=1
    parity::compare "${dp_label}" - || dp_ok=0
    if [[ -z "${dp_want}" ]]; then
      # Each implementation applies dp_n manifests, and the full apply gives
      # kubectl the bytes of the artifact: the digest is in the stdout of
      # both runs.
      dp_digest="$(sha256sum < "${PROJ}/clusters/lok8s.dev/artifacts.yaml" | cut -d' ' -f1)"
      for dp_impl in go bash; do
        dp_rc="${go_rc}"
        [[ "${dp_impl}" == go ]] || dp_rc="${bash_rc}"
        dp_applied=$(grep -c '^parity-stub applied ' "${WORK}/${dp_impl}.out" || true)
        if (( dp_rc != 0 || dp_applied != dp_n )); then
          echo "FAIL: ${dp_label} — ${dp_impl}: rc=${dp_rc}, ${dp_applied} manifest(s) applied, stdout:"
          indent < "${WORK}/${dp_impl}.out"
          dp_ok=0
        fi
        if [[ "${dp_route}" == full ]] && ! grep -qF "parity-stub applied ${dp_digest}" "${WORK}/${dp_impl}.out"; then
          echo "FAIL: ${dp_label} — ${dp_impl}: kubectl did not get the bytes of the artifact (digest ${dp_digest}):"
          indent < "${WORK}/${dp_impl}.out"
          dp_ok=0
        fi
      done
    else
      if (( go_rc != 1 || bash_rc != 1 )) \
          || ! grep -qF "refusing to apply lok8s.dev: PROJ/clusters/lok8s.dev/artifacts.yaml ${dp_want}" "${WORK}/go.err"; then
        echo "FAIL: ${dp_label} — rc go=${go_rc} bash=${bash_rc}, stderr:"
        indent < "${WORK}/go.err"
        dp_ok=0
      fi
      if [[ -s "${PARITY_KUBECTL_LOG}" ]]; then
        echo "FAIL: ${dp_label} — kubectl ran:"
        indent < "${PARITY_KUBECTL_LOG}"
        dp_ok=0
      fi
    fi
    parity::record "${dp_ok}" "${dp_label}"
  done
done
unset LOK8S_NONINTERACTIVE
unset PARITY_KUBECTL_LOG
parity::stub_kubectl_fail "${PROJ}"
rm -rf "${PROJ}/clusters/lok8s.dev"

# ── lo tilt — stub-backed lifecycle + preflight gates ────────────────────────

check - tilt status                                     # stub `tilt doctor`
check - tilt ci                                         # stub exit 7 → rc passthrough
check - tilt ci --timeout 90s
rm -f "${PROJ}/.tilt.pid"
check - tilt down                                       # no pid file → silent no-op

# spawn_check <label> <verb>: tilt up / tilt restart: both spawn the STUB
# detached and must write a pid file. (pid VALUES differ by construction;
# existence + stdout are the contract.)
spawn_check() {
  local label="${1}" verb="${2}" impl rc ok=1 d
  for impl in go bash; do
    rm -f "${PROJ}/.tilt.pid" "${PROJ}/.tilt.nohup"
    rc=0
    parity::run "${impl}" "${PROJ}" tilt "${verb}" || rc=$?
    [[ ${rc} -eq 0 && -f "${PROJ}/.tilt.pid" ]] || { echo "FAIL: tilt ${verb} (${impl}): rc=${rc} pidfile=$([[ -f ${PROJ}/.tilt.pid ]] && echo yes || echo no)"; ok=0; }
  done
  d="$(diff "${WORK}/bash.out" "${WORK}/go.out" || true)"
  [[ -z "${d}" ]] || { echo "FAIL: tilt ${verb} stdout differs:"; indent <<<"${d}"; ok=0; }
  parity::record "${ok}" "${label}"
  rm -f "${PROJ}/.tilt.pid" "${PROJ}/.tilt.nohup"
}
spawn_check "lo tilt up (stub spawn + pid file)" up
# tilt restart: down (no pid file → no-op) then up — the same spawn + pid
# contract as tilt up.
spawn_check "lo tilt restart (down no-op + stub spawn + pid file)" restart

# preflight gates (manifest on stdin; kubectl is the failing stub, so the Lo
# path reports "nothing stuck").
check_stdin 'kind: ConfigMap' tilt preflight --domain beta.cloud   # non-kind refusal
export LOK8S_PREFLIGHT=0
check_stdin 'kind: ConfigMap' tilt preflight --domain alpha.dev    # kill switch
unset LOK8S_PREFLIGHT
check_stdin 'kind: ConfigMap' tilt preflight --domain alpha.dev    # sweep, nothing stuck

# A service overlay that adds the keys-only marker to a Secret of the domain
# artifact. The Tiltfile renders the overlay (kustomize build of the
# artifacts dir) and pipes the render to lo tilt preflight on every load;
# both implementations refuse it (rc 1, the same message) on the sweep path,
# with the kill switch that the Tiltfile sets for preflight = False, and on
# a non-kind domain. The refusal fails the load, so no k8s_yaml runs.
ENVP_ART='apiVersion: v1
kind: Secret
metadata:
  name: app
  namespace: ns1
  labels:
    lok8s.dev/name: web
data:
  PASSWORD: cGFzcw==' envp_case "a keys-only marker" 0 <<'YAML'
services:
  web:
    patches:
      - target: {version: v1, kind: Secret, name: app}
        patch: |
          - op: add
            path: /metadata/annotations
            value: {secrets.lok8s.dev/keys-only: "true"}
YAML
if ko_render="$("${ROOT}/.bin/kustomize" build --load-restrictor=LoadRestrictionsNone \
    "${WORK}/envp-go/clusters/alpha.dev/artifacts" 2>&1)" \
    && grep -q 'secrets.lok8s.dev/keys-only' <<<"${ko_render}"; then
  for ko_case in sweep kill-switch non-kind; do
    ko_domain=alpha.dev
    [[ "${ko_case}" != kill-switch ]] || export LOK8S_PREFLIGHT=0
    [[ "${ko_case}" != non-kind ]] || ko_domain=beta.cloud
    ko_label="lo tilt preflight refuses a keys-only service overlay (${ko_case})"
    PARITY_STDIN="${ko_render}" parity::run_pair tilt preflight --domain "${ko_domain}"
    ko_ok=1
    parity::compare "${ko_label}" - || ko_ok=0
    # shellcheck disable=SC2154 # go_rc, bash_rc: set by parity::run_pair (hack/lib/parity.sh)
    if (( go_rc != 1 || bash_rc != 1 )) \
        || ! grep -qF 'preflight: refusing to apply the Tilt manifest: it holds a keys-only Secret (secrets.lok8s.dev/keys-only)' "${WORK}/go.err"; then
      echo "FAIL: ${ko_label} — rc go=${go_rc} bash=${bash_rc}, stderr:"
      indent < "${WORK}/go.err"
      ko_ok=0
    fi
    parity::record "${ko_ok}" "${ko_label}"
    unset LOK8S_PREFLIGHT
  done
else
  fail "lo tilt preflight refuses a keys-only service overlay — the overlay does not render with the marker: ${ko_render}"
fi

# ── lo image — gates, error paths, catalog listing ───────────────────────────

check - image cache svc --domain beta.cloud             # non-lo driver gate
check - image list --domain beta.cloud
check - image cache --all --domain alpha.dev            # empty queue → no-op
check - image cache pinned --domain alpha.dev           # explicit image: pin
check - image cache --domain alpha.dev                  # no service, no --all
rm -f "${PROJ}/services.yaml" "${PROJ}/services.local.yaml"
check - image cache svc --domain alpha.dev              # no endpoint configured
# --domain BEFORE the verb (D3): bash main resolves the spec — and with it
# KIND_EXPERIMENTAL_DOCKER_NETWORK, hence the registry name — from the
# domain it parsed before dispatch, so a trailing --domain is invisible to
# that lookup in bash only. Placed first, both name paritynet-registry-cache.
check - --domain alpha.dev image clean                  # stub docker: rm + volume rm fail, both silent
check - image clean --domain beta.cloud                 # non-lo driver gate: both refuse before docker

# Unresolvable IP: a Lo spec without spec.network on a non-slot domain.
mkdir -p "${PROJ}/clusters/bare.dev"
printf 'kind: Lo\nmetadata:\n  name: bare\n' > "${PROJ}/clusters/bare.dev/cluster.lok8s.yaml"
check - image cache --all --domain bare.dev
check - image list --domain bare.dev

# Catalog listing against a local HTTP stub (registry /v2/_catalog). The env
# override doubles as the issue-#89 gate-escape probe (KubeOne domain).
if command -v python3 >/dev/null 2>&1; then
  mkdir -p "${WORK}/registry/v2"
  printf '{"repositories":["parity/one","parity/two"]}' > "${WORK}/registry/v2/_catalog"
  (cd "${WORK}/registry" && exec python3 -u -m http.server 0 --bind 127.0.0.1 >"${WORK}/http.log" 2>&1) &
  STUB_HTTP_PID=$!
  for _ in $(seq 1 50); do
    STUB_PORT="$(sed -nE 's/.*port ([0-9]+).*/\1/p' "${WORK}/http.log" | head -1)"
    [[ -n "${STUB_PORT}" ]] && break
    sleep 0.1
  done
  if [[ -n "${STUB_PORT:-}" ]]; then
    export LOK8S_REGISTRY_IP_CACHE="127.0.0.1:${STUB_PORT}"
    check - image list --domain beta.cloud              # override skips the gate
    check - image list --domain alpha.dev
    unset LOK8S_REGISTRY_IP_CACHE
  else
    echo "skip: image list catalog stub (python http.server did not report a port)"
  fi
else
  echo "skip: image list catalog stub (python3 not available)"
fi
# Dead endpoint: header only, rc 0 (the curl|jq quirk both must preserve).
export LOK8S_REGISTRY_IP_CACHE="127.0.0.1:1"
check - image list --domain alpha.dev
unset LOK8S_REGISTRY_IP_CACHE

report
