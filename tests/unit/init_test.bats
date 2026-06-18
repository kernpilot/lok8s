#!/usr/bin/env bats
# init_test.bats — unit tests for .lok8s/libs/init (`lo init service`)
#
# `lo init service <name>` scaffolds config so nobody hand-writes service
# config from imagination. These tests pin the three guarantees that make the
# scaffold useful:
#   1. the emitted lok8s.yaml passes the per-service validator in
#      .lok8s/tilt/Tiltfile (build: block present + a mapping; context/
#      dockerfile strings; only allowed top-level keys),
#   2. services.yaml gains a services.<name>.path entry (created from a
#      correct template when absent; merged — preserving siblings — when not),
#   3. the project-root Tiltfile is the canonical 2-line loader (written when
#      absent; never clobbered when it hardcodes docker_build).
# Plus idempotency (no clobber without --force) and a path-traversal guard on
# the name (mirrors use::_set_active).

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # Sourcing libs/init defines init::* ; imports are stubbed and the bottom
  # guard does not fire (ARGSH_SOURCE is unset by test_helper).
  source "${_PROJECT_ROOT}/.lok8s/libs/init"

  # setup_tmpdir points PATH_BASE at the per-test sandbox; init writes
  # services.yaml + Tiltfile relative to PATH_BASE.
}

teardown() { teardown_tmpdir; }

# ── name validation ─────────────────────────────────────

@test "init::_validate_name accepts a plain lowercase name" {
  run init::_validate_name "api"
  [ "$status" -eq 0 ]
}

@test "init::_validate_name accepts dots, dashes, digits" {
  run init::_validate_name "my-svc.2"
  [ "$status" -eq 0 ]
}

@test "init::_validate_name rejects path traversal" {
  run init::_validate_name "../evil"
  [ "$status" -ne 0 ]
  [[ "$output" == *"invalid service name"* ]]
}

@test "init::_validate_name rejects a slash" {
  run init::_validate_name "foo/bar"
  [ "$status" -ne 0 ]
}

@test "init::_validate_name rejects uppercase (services.yaml keys are lowercase)" {
  run init::_validate_name "Foo"
  [ "$status" -ne 0 ]
}

# ── scaffolded lok8s.yaml passes the per-service validator ──

@test "init::_scaffold_lokyaml writes a lok8s.yaml that satisfies _validate_service" {
  init::_scaffold_lokyaml "${PATH_BASE}/foo" "foo" 0
  local f="${PATH_BASE}/foo/lok8s.yaml"
  [ -f "${f}" ]

  # Only allowed top-level keys (build,ports,links,workloads,tilt). The active
  # config must be build-only — comments (ports/workloads/tilt/components) are
  # not parsed, and `components` would be rejected as an unknown key.
  run yq -r 'keys | .[]' "${f}"
  [ "$status" -eq 0 ]
  [ "$output" = "build" ]

  # build is a required mapping with string context/dockerfile.
  [ "$(yq -r 'has("build")' "${f}")" = "true" ]
  [ "$(yq -r '.build | type' "${f}")" = "!!map" ]
  [ "$(yq -r '.build.context | type' "${f}")" = "!!str" ]
  [ "$(yq -r '.build.dockerfile | type' "${f}")" = "!!str" ]
}

@test "init::_scaffold_lokyaml does not clobber an existing file without force" {
  mkdir -p "${PATH_BASE}/foo"
  echo "build: { context: ., dockerfile: Keep }" > "${PATH_BASE}/foo/lok8s.yaml"
  run init::_scaffold_lokyaml "${PATH_BASE}/foo" "foo" 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"not overwriting"* ]] || [[ "$(yq -r '.build.dockerfile' "${PATH_BASE}/foo/lok8s.yaml")" = "Keep" ]]
  [ "$(yq -r '.build.dockerfile' "${PATH_BASE}/foo/lok8s.yaml")" = "Keep" ]
}

@test "init::_scaffold_lokyaml overwrites when force=1" {
  mkdir -p "${PATH_BASE}/foo"
  echo "build: { context: ., dockerfile: Keep }" > "${PATH_BASE}/foo/lok8s.yaml"
  init::_scaffold_lokyaml "${PATH_BASE}/foo" "foo" 1
  [ "$(yq -r '.build.dockerfile' "${PATH_BASE}/foo/lok8s.yaml")" = "Dockerfile" ]
}

# ── services.yaml creation + merge ──────────────────────

@test "init::_merge_services creates services.yaml from a template the validator accepts" {
  local s="${PATH_BASE}/services.yaml"
  init::_merge_services "${s}" "foo" "./foo"
  [ -f "${s}" ]

  # registry keys ⊆ {endpoint,branch,tag,prefix,parallel}
  [ "$(yq -r '.registry.prefix' "${s}")" = "lok8s.local" ]
  # defaults.dockerfile ∈ {service,production}; defaults.build is a bool
  [ "$(yq -r '.defaults.dockerfile' "${s}")" = "service" ]
  [ "$(yq -r '.defaults.build | type' "${s}")" = "!!bool" ]
  # the entry itself
  [ "$(yq -r '.services.foo.path' "${s}")" = "./foo" ]
}

@test "init::_merge_services preserves existing services" {
  local s="${PATH_BASE}/services.yaml"
  init::_merge_services "${s}" "foo" "./foo"
  init::_merge_services "${s}" "bar" "./services/bar"
  [ "$(yq -r '.services.foo.path' "${s}")" = "./foo" ]
  [ "$(yq -r '.services.bar.path' "${s}")" = "./services/bar" ]
}

# ── Tiltfile handling ───────────────────────────────────

@test "init::_ensure_tiltfile writes the canonical 2-line form when absent" {
  local t="${PATH_BASE}/Tiltfile"
  init::_ensure_tiltfile "${t}"
  [ -f "${t}" ]
  grep -q "load('./.lok8s/tilt/Tiltfile', 'lok8s')" "${t}"
  grep -q "^lok8s()$" "${t}"
}

@test "init::_ensure_tiltfile leaves an already-canonical Tiltfile untouched" {
  local t="${PATH_BASE}/Tiltfile"
  printf "load('./.lok8s/tilt/Tiltfile', 'lok8s')\nlok8s()\n# my note\n" > "${t}"
  init::_ensure_tiltfile "${t}"
  grep -q "# my note" "${t}"
}

@test "init::_ensure_tiltfile does NOT clobber a hand-rolled docker_build Tiltfile" {
  local t="${PATH_BASE}/Tiltfile"
  printf "docker_build('img', '.')\nk8s_yaml('d.yaml')\n" > "${t}"
  run init::_ensure_tiltfile "${t}"
  [ "$status" -eq 0 ]
  [[ "$output" == *"NOT overwriting"* ]]
  grep -q "docker_build('img', '.')" "${t}"
}

# ── full command ────────────────────────────────────────
#
# init::service is the thin :args wrapper; the argsh :args/:usage builtins are
# not loaded when a lib is merely sourced (see lo_commands_test.bats: "We
# re-implement main::use logic since it depends on argsh :args"). So these
# drive init::_service, the pure orchestrator behind the wrapper. The full
# `lo init service ...` flag path is exercised against the real binary in the
# end-to-end check at the bottom of this file.

@test "init::_service scaffolds lok8s.yaml + services.yaml + Tiltfile end to end" {
  run init::_service foo "${PATH_BASE}/foo" 0
  [ "$status" -eq 0 ]

  # 1. lok8s.yaml exists and passes the validator's core shape rules
  local f="${PATH_BASE}/foo/lok8s.yaml"
  [ -f "${f}" ]
  [ "$(yq -r 'keys | .[]' "${f}")" = "build" ]
  [ "$(yq -r '.build | type' "${f}")" = "!!map" ]

  # 2. services.yaml entry added
  [ "$(yq -r '.services.foo.path' "${PATH_BASE}/services.yaml")" = "${PATH_BASE}/foo" ]

  # 3. canonical 2-line Tiltfile
  grep -q "load('./.lok8s/tilt/Tiltfile', 'lok8s')" "${PATH_BASE}/Tiltfile"
  grep -q "^lok8s()$" "${PATH_BASE}/Tiltfile"
}

@test "init::_service defaults the path to ./<name> when empty" {
  cd "${PATH_BASE}"
  run init::_service foo "" 0
  [ "$status" -eq 0 ]
  [ "$(yq -r '.services.foo.path' "${PATH_BASE}/services.yaml")" = "./foo" ]
  [ -f "${PATH_BASE}/foo/lok8s.yaml" ]
}

@test "init::_service re-run without force does not clobber the lok8s.yaml" {
  init::_service foo "${PATH_BASE}/foo" 0 >/dev/null 2>&1
  echo "# user-edit-marker" >> "${PATH_BASE}/foo/lok8s.yaml"
  run init::_service foo "${PATH_BASE}/foo" 0
  [ "$status" -eq 0 ]
  grep -q "# user-edit-marker" "${PATH_BASE}/foo/lok8s.yaml"
}

@test "init::_service with force overwrites the lok8s.yaml" {
  init::_service foo "${PATH_BASE}/foo" 0 >/dev/null 2>&1
  echo "# user-edit-marker" >> "${PATH_BASE}/foo/lok8s.yaml"
  init::_service foo "${PATH_BASE}/foo" 1 >/dev/null 2>&1
  ! grep -q "# user-edit-marker" "${PATH_BASE}/foo/lok8s.yaml"
}

@test "init::_service rejects an invalid (path-traversal) name" {
  run init::_service "../evil" "" 0
  [ "$status" -ne 0 ]
  [[ "$output" == *"invalid service name"* ]]
  [ ! -f "${PATH_BASE}/services.yaml" ]
}

@test "init::_service requires a name" {
  run init::_service "" "" 0
  [ "$status" -ne 0 ]
  [[ "$output" == *"required"* ]]
}

# End-to-end against the real `lo` binary: exercises the :args flag parsing
# (--path/--force) and the nested-subcommand dispatch (main::init -> init::service)
# that the sourced-lib tests above cannot reach. Skipped if argsh/yq aren't on
# PATH (defensive — they are present in the argsh test image).
@test "lo init service foo --path ./foo works through the real CLI" {
  local lo="${_PROJECT_ROOT}/.lok8s/lo"
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  command -v yq >/dev/null || skip "yq not available"

  local proj="${PATH_BASE}/proj"
  mkdir -p "${proj}"
  # Run from the project root so the relative --path ./foo resolves there,
  # exactly as a user invoking `lo` from their project would.
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    "${lo}" init service foo --path ./foo
  [ "$status" -eq 0 ]

  [ "$(yq -r 'keys | .[]' "${proj}/foo/lok8s.yaml")" = "build" ]
  [ "$(yq -r '.services.foo.path' "${proj}/services.yaml")" = "./foo" ]
  grep -q "load('./.lok8s/tilt/Tiltfile', 'lok8s')" "${proj}/Tiltfile"
}

# ── lo init test (Playwright scaffold) ──────────────────
#
# init::_test copies the generic Playwright suite template from
# ${PATH_LOK8S}/libs/init.d/test into the project's tests/ dir. setup_tmpdir
# points PATH_LOK8S at the per-test sandbox (no template there), so these tests
# point PATH_LOK8S back at the REAL project .lok8s for the template lookup while
# keeping PATH_BASE in the sandbox for the destination.

@test "init::_test_template_dir resolves under PATH_LOK8S" {
  PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" run init::_test_template_dir
  [ "$status" -eq 0 ]
  [ "$output" = "${_PROJECT_ROOT}/.lok8s/libs/init.d/test" ]
  [ -d "$output" ]
}

@test "init::_scaffold_tests copies the generic suite into an empty dir" {
  PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  local dest="${PATH_BASE}/tests"
  run init::_scaffold_tests "${dest}" 0
  [ "$status" -eq 0 ]
  # Key generic files land.
  [ -f "${dest}/playwright.config.ts" ]
  [ -f "${dest}/utils/config.ts" ]
  [ -f "${dest}/utils/mailpit.ts" ]
  [ -f "${dest}/utils/resolver.ts" ]
  [ -f "${dest}/utils/tls.ts" ]
  [ -f "${dest}/fixtures/test.ts" ]
  [ -f "${dest}/pages/BasePage.ts" ]
  [ -f "${dest}/config/dev.ts" ]
  [ -f "${dest}/setup/auth.setup.ts" ]
  [ -f "${dest}/package.json" ]
  [ -f "${dest}/.gitignore" ]
}

@test "init::_scaffold_tests stays GENERIC — no kubehz hostnames/specifics" {
  PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  local dest="${PATH_BASE}/tests"
  init::_scaffold_tests "${dest}" 0
  # The template must be project-agnostic: no kubehz domains or KUBEHZ_ env prefix.
  run grep -rIl -e 'kubehz' -e 'KUBEHZ_' "${dest}"
  [ -z "$output" ]
  # It SHOULD use the neutral LOK8S_TEST_ env prefix.
  grep -q 'LOK8S_TEST_DOMAIN' "${dest}/utils/config.ts"
}

@test "init::_scaffold_tests does not clobber a non-empty dir without force" {
  PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  local dest="${PATH_BASE}/tests"
  mkdir -p "${dest}"
  echo "keep me" > "${dest}/MINE.txt"
  run init::_scaffold_tests "${dest}" 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"not overwriting"* ]]
  [ -f "${dest}/MINE.txt" ]
  [ ! -f "${dest}/playwright.config.ts" ]
}

@test "init::_scaffold_tests overwrites with force (preserving local additions)" {
  PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  local dest="${PATH_BASE}/tests"
  mkdir -p "${dest}"
  echo "keep me" > "${dest}/MINE.txt"
  run init::_scaffold_tests "${dest}" 1
  [ "$status" -eq 0 ]
  [ -f "${dest}/playwright.config.ts" ]   # template now written
  [ -f "${dest}/MINE.txt" ]               # local file survives (copy, not wipe)
}

@test "init::_test defaults the destination to ./tests under PATH_BASE" {
  PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  run init::_test "" 0
  [ "$status" -eq 0 ]
  [ -f "${PATH_BASE}/tests/playwright.config.ts" ]
}

@test "lo init test works through the real CLI" {
  local lo="${_PROJECT_ROOT}/.lok8s/lo"
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"

  local proj="${PATH_BASE}/proj"
  mkdir -p "${proj}"
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    "${lo}" init test
  [ "$status" -eq 0 ]
  [ -f "${proj}/tests/playwright.config.ts" ]
  [ -f "${proj}/tests/utils/config.ts" ]
}
