#!/usr/bin/env bats
# hooks_test.bats — unit tests for .lok8s/libs/hooks (the `lo hooks` command).
# Covers the pure logic: the label-selector → yq translation (incl. injection
# rejection — security critical, it builds a yq expression) and the artifact
# label-filter. The kubectl verbs (recreate/restart/apply) need a live cluster
# and are exercised by the Tilt hooks: integration, not here.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  # Stub argsh builtins so the lib sources without an argsh runtime.
  import() { :; }; export -f import
  :usage() { :; }; export -f :usage
  :args() { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/hooks"
}

@test "_yq_filter: multi-label selector → AND-joined select()" {
  run hooks::_yq_filter 'lok8s.dev/type=seed,lok8s.dev/name=zitadel'
  assert_success
  assert_output 'select((.metadata.labels."lok8s.dev/type" == "seed") and (.metadata.labels."lok8s.dev/name" == "zitadel"))'
}

@test "_yq_filter: single label" {
  run hooks::_yq_filter 'app=kubehz-auth'
  assert_success
  assert_output 'select((.metadata.labels."app" == "kubehz-auth"))'
}

@test "_yq_filter: rejects shell/yq injection in the value" {
  run hooks::_yq_filter 'a=b;rm -rf /'
  assert_failure
  assert_output --partial 'invalid selector clause'
}

@test "_yq_filter: rejects a clause without '='" {
  run hooks::_yq_filter 'noequalshere'
  assert_failure
  assert_output --partial 'must be key=value'
}

@test "_yq_filter: rejects an empty selector" {
  run hooks::_yq_filter ''
  assert_failure
  assert_output --partial 'required'
}

@test "_select: returns only label-matching objects from the rendered artifact" {
  mkdir -p "${PATH_CLUSTERS}/d.dev"
  cat > "${PATH_CLUSTERS}/d.dev/artifacts.yaml" <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: zitadel-provision
  labels:
    lok8s.dev/role: seed
    lok8s.dev/name: zitadel
---
apiVersion: batch/v1
kind: Job
metadata:
  name: zitadel-setup
  labels:
    lok8s.dev/name: zitadel
YAML
  run hooks::_select d.dev 'lok8s.dev/role=seed'
  assert_success
  assert_output --partial 'zitadel-provision'
  refute_output --partial 'zitadel-setup'
}

@test "_select: empty when nothing matches" {
  mkdir -p "${PATH_CLUSTERS}/d.dev"
  printf 'kind: Job\nmetadata:\n  name: x\n  labels: {lok8s.dev/name: other}\n' \
    > "${PATH_CLUSTERS}/d.dev/artifacts.yaml"
  run hooks::_select d.dev 'lok8s.dev/role=seed'
  assert_success
  refute_output --partial 'name: x'
}

@test "_yq_filter: accepts '/' in a VALUE too (regex matches the key set + message)" {
  # Regression for the round-1 fix: the value branch used to reject '/', which
  # both contradicted the error message and diverged from the Starlark _SEL_CHARS.
  run hooks::_yq_filter 'role=a/b'
  assert_success
  assert_output 'select((.metadata.labels."role" == "a/b"))'
}

@test "_yq_filter: still rejects a value with a space (arg-split / injection)" {
  run hooks::_yq_filter 'role=a b'
  assert_failure
  assert_output --partial 'invalid selector clause'
}

# ── Image preservation across recreate ───────────────────────────────────────
# `recreate` re-applies from the rendered artifact, which carries the DECLARED
# image ref. Under Tilt the running object carries Tilt's built-and-pushed ref —
# injection happens on Tilt's deploy path, not in the YAML — so re-applying the
# declared ref points the workload at an image nobody pushed. Measured on
# kubehz-api-migrate, whose DB migration had been blocked since the hook last
# fired (visual-audit r251).

@test "_overlay_images: replaces the declared ref with the live one, by container name" {
  run hooks::_overlay_images '[{"name":"migrate","image":"reg/lok8s.local_kubehz-api-migrate:tilt-abc"}]' <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: mig
  namespace: ns
spec:
  template:
    spec:
      containers:
        - name: migrate
          image: lok8s.local/kubehz-api-migrate
YAML
  assert_success
  assert_output --partial 'reg/lok8s.local_kubehz-api-migrate:tilt-abc'
  refute_output --partial 'image: lok8s.local/kubehz-api-migrate'
}

@test "_overlay_images: a container the live object does not have keeps its rendered ref" {
  run hooks::_overlay_images '[{"name":"migrate","image":"reg/a:tilt-1"}]' <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: mig
spec:
  template:
    spec:
      containers:
        - name: migrate
          image: lok8s.local/a
        - name: sidecar
          image: lok8s.local/b
YAML
  assert_success
  assert_output --partial 'reg/a:tilt-1'
  assert_output --partial 'lok8s.local/b'
}

@test "_overlay_images: initContainers are preserved too" {
  run hooks::_overlay_images '[{"name":"wait","image":"reg/wait:tilt-9"},{"name":"app","image":"reg/app:tilt-9"}]' <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: d
spec:
  template:
    spec:
      initContainers:
        - name: wait
          image: lok8s.local/wait
      containers:
        - name: app
          image: lok8s.local/app
YAML
  assert_success
  assert_output --partial 'reg/wait:tilt-9'
  assert_output --partial 'reg/app:tilt-9'
}

@test "_overlay_images: an empty live list is a no-op (first-ever apply)" {
  run hooks::_overlay_images '[]' <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: mig
spec:
  template:
    spec:
      containers:
        - name: migrate
          image: lok8s.local/kubehz-api-migrate
YAML
  assert_success
  assert_output --partial 'lok8s.local/kubehz-api-migrate'
}

@test "_live_images: a missing object yields an empty array, not a failure" {
  kubectl() { return 1; }
  run hooks::_live_images Job nope ns
  assert_success
  assert_output '[]'
}

# ── Tilt owns the recreate when Tilt owns the objects ────────────────────────
# Preserving live image refs only helps while something is running; a completed,
# cleaned-up Job has nothing to preserve and the raw ref ships anyway (measured,
# kubehz-cluster AUDIT.md r260). Tilt knows the real ref in both cases.

@test "_object_names: one metadata.name per document" {
  run hooks::_object_names "$(cat <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: mig-a
---
apiVersion: batch/v1
kind: Job
metadata:
  name: mig-b
YAML
)"
  assert_success
  assert_line --index 0 'mig-a'
  assert_line --index 1 'mig-b'
}

@test "_tilt_can_recreate: false when the tilt helpers are not loaded" {
  run hooks::_tilt_can_recreate "$(printf 'kind: Job\nmetadata:\n  name: mig\n')"
  assert_failure
}

@test "_tilt_can_recreate: true only when Tilt knows EVERY object" {
  tilt::has_resource() { [ "$1" = "known" ]; }
  run hooks::_tilt_can_recreate "$(printf 'kind: Job\nmetadata:\n  name: known\n')"
  assert_success
  run hooks::_tilt_can_recreate "$(printf 'kind: Job\nmetadata:\n  name: known\n---\nkind: Job\nmetadata:\n  name: other\n')"
  assert_failure
}

@test "_tilt_can_recreate: false when the selection is empty" {
  tilt::has_resource() { return 0; }
  run hooks::_tilt_can_recreate ""
  assert_failure
}
