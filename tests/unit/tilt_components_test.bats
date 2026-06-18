#!/usr/bin/env bats
# tilt_components_test.bats — unit tests for the Tilt extension's
# multi-image `components:` support in a per-service lok8s.yaml.
#
# The Tilt extension is Starlark, not bash, so these tests drive the real
# `.lok8s/tilt/Tiltfile` through `tilt alpha tiltfile-result`, which
# evaluates a Tiltfile in a sandbox and prints the resulting build/resource
# graph as JSON. We stub `lo` and `kustomize` on PATH so `lok8s()` runs in
# isolation (no cluster, no network):
#
#   lo env kustomization   → no-op (auto_cache_pull=False skips the --pull)
#   lo env services        → emits the services.yaml catalog
#   kustomize build <dir>  → emits the rendered manifests (labelled)
#
# Tilt prunes image builds that no manifest references, so each fixture
# applies a Deployment per expected image, labelled `lok8s.dev/name=<x>`.
#
# Assertions read the JSON: `.Manifests[].ImageTargets[].selector` is the
# built image ref (e.g. lok8s.local/kubehz-api), and
# `.BuildDetails.context` is the absolutized build context.

setup() {
  load "../test_helper"
  setup_tmpdir

  _TILT_BIN="${_PROJECT_ROOT}/.bin/tilt"
  _JQ_BIN="${_PROJECT_ROOT}/.bin/jq"
  _EXT_TILTFILE="${_PROJECT_ROOT}/.lok8s/tilt/Tiltfile"

  if [[ ! -x "${_TILT_BIN}" ]]; then
    if command -v tilt &>/dev/null; then _TILT_BIN="$(command -v tilt)"; else
      skip "tilt binary not available (.bin/tilt missing and not on PATH)"
    fi
  fi
  if [[ ! -x "${_JQ_BIN}" ]]; then
    if command -v jq &>/dev/null; then _JQ_BIN="$(command -v jq)"; else
      skip "jq binary not available"
    fi
  fi
  [[ -f "${_EXT_TILTFILE}" ]] || skip "extension Tiltfile not found at ${_EXT_TILTFILE}"

  # Sandbox project layout.
  _SB="${BATS_TEST_TMPDIR}/proj"
  mkdir -p "${_SB}/bin" "${_SB}/clusters/lok8s.dev/artifacts"

  # Make the Tiltfile evaluation hermetic. The extension's first line is
  # `load('ext://namespace', ...)`, which Tilt resolves against its default
  # extension repo (github.com/tilt-dev/tilt-extensions) — normally a git
  # clone over the network. The argsh CI container has neither git nor
  # network, so we:
  #   1. vendor a minimal `namespace` extension into a sandbox XDG data dir
  #      (only `namespace_inject` is needed, and only as a pass-through —
  #      no fixture here sets `namespace`), and
  #   2. provide a fake `git` that answers the two commands Tilt runs to
  #      validate the vendored repo (`remote get-url origin`, `rev-parse
  #      HEAD`), so it treats the checkout as present and up-to-date.
  # This keeps the test offline and deterministic on both CI and a dev box.
  _XDG="${BATS_TEST_TMPDIR}/xdg"
  local ext_dir="${_XDG}/tilt-dev/tilt_modules/github.com/tilt-dev/tilt-extensions/namespace"
  mkdir -p "${ext_dir}"
  cat > "${ext_dir}/Tiltfile" <<'EOF'
# Minimal vendored shim of ext://namespace for hermetic tests.
# Only namespace_inject is referenced by the lok8s extension; the fixtures
# never set a namespace, so a pass-through is sufficient.
def namespace_inject(x, ns):
  return x
EOF

  _FAKEBIN="${BATS_TEST_TMPDIR}/fakebin"
  mkdir -p "${_FAKEBIN}"
  cat > "${_FAKEBIN}/git" <<'EOF'
#!/usr/bin/env bash
# Hermetic fake git: lets Tilt treat the vendored extension repo as a
# valid, up-to-date checkout without any network or real git binary.
for a in "$@"; do
  case "$a" in
    "get-url") echo "https://github.com/tilt-dev/tilt-extensions"; exit 0 ;;
    "rev-parse") echo "0000000000000000000000000000000000000000"; exit 0 ;;
  esac
done
exit 0
EOF
  chmod +x "${_FAKEBIN}/git"
}

teardown() {
  teardown_tmpdir
}

# Write the stub `lo` (env kustomization no-op; env services emits $1).
_write_lo_stub() {
  local services_yaml="$1"
  cat > "${_SB}/bin/lo" <<EOF
#!/usr/bin/env bash
if [ "\$1" = "env" ] && [ "\$2" = "kustomization" ]; then exit 0; fi
if [ "\$1" = "env" ] && [ "\$2" = "services" ]; then
cat <<'SERVICES_YAML'
${services_yaml}
SERVICES_YAML
exit 0
fi
exit 0
EOF
  chmod +x "${_SB}/bin/lo"
}

# Write the stub `kustomize` (build emits $1).
_write_kustomize_stub() {
  local manifests="$1"
  cat > "${_SB}/bin/kustomize" <<EOF
#!/usr/bin/env bash
cat <<'MANIFESTS_YAML'
${manifests}
MANIFESTS_YAML
EOF
  chmod +x "${_SB}/bin/kustomize"
}

# Emit a Deployment manifest labelled lok8s.dev/name=<name> referencing
# image lok8s.local/<name>. Tilt keeps the matching docker_build only if a
# manifest references its image, so this anchors each expected build.
_deployment() {
  local name="$1"
  cat <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  labels:
    lok8s.dev/name: ${name}
spec:
  selector: { matchLabels: { app: ${name} } }
  template:
    metadata: { labels: { app: ${name} } }
    spec:
      containers:
        - name: ${name}
          image: "lok8s.local/${name}"
YAML
}

# Root Tiltfile that loads the real extension. auto_cache_pull=False skips
# the `lo env kustomization --pull` cache drain (nothing to pull here).
_write_root_tiltfile() {
  cat > "${_SB}/Tiltfile" <<EOF
load('${_EXT_TILTFILE}', 'lok8s')
lok8s(auto_cache_pull = False)
EOF
}

# Run `tilt alpha tiltfile-result` in the sandbox with the stubs on PATH.
# Captures JSON on $output (when it succeeds) and the exit code on $status.
#
# PATH order: project `bin` stubs (lo, kustomize) → fake git → the real
# tilt (.bin) → inherited PATH. XDG_DATA_HOME points Tilt at the vendored
# `namespace` extension; HOME is sandboxed so Tilt doesn't touch the real
# ~/.tilt-dev. `cd` into the sandbox so relative paths (clusters/, ./svc)
# resolve there.
_run_tiltfile_result() {
  run env \
    HOME="${BATS_TEST_TMPDIR}/home" \
    XDG_DATA_HOME="${_XDG}" \
    PATH="${_SB}/bin:${_FAKEBIN}:${_PROJECT_ROOT}/.bin:${PATH}" \
    bash -c 'cd "$1" && exec "$2" alpha tiltfile-result -f "$1/Tiltfile"' _ "${_SB}" "${_TILT_BIN}"
}

# --- multi-image components: 2 builds, paths resolved against service path ---

@test "components: two-image service produces two docker_build calls" {
  # The kubehz-core shape: api (Dockerfile.api) + operator
  # (Dockerfile.operator, only: [operator/]) as components in ONE lok8s.yaml.
  mkdir -p "${_SB}/kubehz-core/operator"
  printf 'FROM scratch\n' > "${_SB}/kubehz-core/Dockerfile.api"
  printf 'FROM scratch\n' > "${_SB}/kubehz-core/Dockerfile.operator"
  cat > "${_SB}/kubehz-core/lok8s.yaml" <<'YAML'
components:
  - name: kubehz-api
    build:
      context: .
      dockerfile: Dockerfile.api
    ports:
      - { from: 3000, to: 3000 }
    workloads: [kubehz-api]
  - name: kubehz-operator
    build:
      context: .
      dockerfile: Dockerfile.operator
      only: [operator/]
    workloads: [kubehz-operator]
YAML

  _write_lo_stub 'services:
  kubehz-core:
    path: ./kubehz-core'
  _write_kustomize_stub "$(_deployment kubehz-api)
---
$(_deployment kubehz-operator)"
  _write_root_tiltfile

  _run_tiltfile_result
  assert_success

  # Exactly two builds, named per component (lok8s.local/<name>).
  local images
  images="$(printf '%s' "${output}" \
    | "${_JQ_BIN}" -r '[.Manifests[]?.ImageTargets[]?.selector] | sort | unique | .[]')"
  assert_equal "${images}" "lok8s.local/kubehz-api
lok8s.local/kubehz-operator"

  local count
  count="$(printf '%s' "${output}" \
    | "${_JQ_BIN}" -r '[.Manifests[]?.ImageTargets[]?.selector] | unique | length')"
  assert_equal "${count}" "2"

  # Both contexts absolutized against the SERVICE path (kubehz-core), not
  # the repo root — proving _update_paths runs per component.
  local ctxs
  ctxs="$(printf '%s' "${output}" \
    | "${_JQ_BIN}" -r '[.Manifests[]?.ImageTargets[]?.BuildDetails.context] | unique | .[]')"
  assert_equal "${ctxs}" "${_SB}/kubehz-core"

  # The operator's `only: [operator/]` passthrough reached docker_build
  # (Tilt records it as a contextIgnore "!operator/" pattern).
  printf '%s' "${output}" \
    | "${_JQ_BIN}" -e '[.Manifests[] | select(.Name=="kubehz-operator")
        | .ImageTargets[].BuildDetails.contextIgnores[]?.patterns[]?]
        | any(. == "!operator/")' >/dev/null
}

# --- mutual exclusion: components + top-level build is rejected ---

@test "components: combined with a top-level build is rejected" {
  mkdir -p "${_SB}/svc"
  printf 'FROM scratch\n' > "${_SB}/svc/Dockerfile.api"
  cat > "${_SB}/svc/lok8s.yaml" <<'YAML'
build:
  context: .
  dockerfile: Dockerfile.api
components:
  - name: svc-api
    build:
      context: .
      dockerfile: Dockerfile.api
YAML

  _write_lo_stub 'services:
  svc:
    path: ./svc'
  _write_kustomize_stub "$(_deployment svc-api)"
  _write_root_tiltfile

  _run_tiltfile_result
  # tilt alpha tiltfile-result exits 5 on a Tiltfile evaluation error
  # (fail()), and prints the message to stderr (folded into $output by run).
  assert_failure
  assert_output --partial "'components' and a top-level 'build' are mutually exclusive"
}

# --- backward compatibility: a single-build lok8s.yaml still works ---

@test "components: single-build lok8s.yaml is unchanged (one build, live_update intact)" {
  mkdir -p "${_SB}/app/src"
  printf 'FROM scratch\n' > "${_SB}/app/lok8s.Dockerfile"
  printf '{}' > "${_SB}/app/package.json"
  cat > "${_SB}/app/lok8s.yaml" <<'YAML'
build:
  context: .
  dockerfile: lok8s.Dockerfile
  live_update:
    sync:
      - local_path: ./src
        remote_path: /app/src
    fall_back_on:
      files:
        - package.json
ports:
  - { from: 3000, to: 3000 }
YAML

  _write_lo_stub 'services:
  app:
    path: ./app'
  _write_kustomize_stub "$(_deployment app)"
  _write_root_tiltfile

  _run_tiltfile_result
  assert_success

  # Exactly one build, the canonical lok8s.local/app.
  local images
  images="$(printf '%s' "${output}" \
    | "${_JQ_BIN}" -r '[.Manifests[]?.ImageTargets[]?.selector] | unique | .[]')"
  assert_equal "${images}" "lok8s.local/app"

  # Context absolutized to the service dir.
  printf '%s' "${output}" \
    | "${_JQ_BIN}" -e '[.Manifests[]?.ImageTargets[]?.BuildDetails.context]
        | any(. == "'"${_SB}/app"'")' >/dev/null

  # live_update survived: one sync (app/src -> /app/src) and the
  # fall_back_on file recorded as a stopPath.
  printf '%s' "${output}" \
    | "${_JQ_BIN}" -e '[.Manifests[]?.ImageTargets[]?.LiveUpdateSpec.syncs[]?
        | select(.localPath=="app/src" and .containerPath=="/app/src")] | length == 1' >/dev/null
  printf '%s' "${output}" \
    | "${_JQ_BIN}" -e '[.Manifests[]?.ImageTargets[]?.LiveUpdateSpec.stopPaths[]?]
        | any(. == "app/package.json")' >/dev/null
}
