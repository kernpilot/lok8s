#!/usr/bin/env bats
# provider_test.bats — unit tests for .lok8s/utils/provider.sh

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"

  # Hetzner resource helpers (for #wipe-devices coverage). resources.sh only
  # defines functions at source time, so this is inert for the other tests.
  export CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/utils/json.sh"
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/utils/resources.sh"

  # `[` is a bash builtin, so a PATH stub cannot override it. To exercise the
  # generated script's `[ -b "$target" ]` block-device assert against synthetic
  # /dev/fakedisk* targets, prepend this shell-function override: it reports our
  # fakes as block devices and delegates every other `[` test to the real
  # builtin. Prepend it to the script under `bash -c` (see execution tests).
  _WIPE_BRACKET_STUB='[() { if [[ "$1" == "-b" && "$2" == /dev/fakedisk* ]]; then return 0; fi; builtin [ "$@"; }'
}

teardown() {
  teardown_tmpdir
}

# ── provider::read_name ──────────────────────────────────

@test "provider::read_name reads spec.provider.name from YAML" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
spec:
  provider:
    name: hetzner
YAML
  run provider::read_name "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_success
  assert_output "hetzner"
}

@test "provider::read_name returns 1 when provider not set" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
spec:
  cluster:
    domain: lok8s.dev
YAML
  run provider::read_name "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_failure
}

@test "provider::read_name rejects path-traversal names" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
spec:
  provider:
    name: "../../etc"
YAML
  run provider::read_name "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_failure
  assert_output --partial "invalid"
}

@test "provider::read_name accepts valid names with hyphens and underscores" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
spec:
  provider:
    name: my-cloud_2
YAML
  run provider::read_name "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_success
  assert_output "my-cloud_2"
}

# ── provider::load ───────────────────────────────────────

@test "provider::load sources the provider and validates contract" {
  # Create a mock provider that implements all four contract functions
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/providers/mock"
  cat > "${BATS_TEST_TMPDIR}/.lok8s/providers/mock/main" <<'SCRIPT'
provider::validate() { return 0; }
provider::credential_data() { echo "key=value"; }
provider::provision() { return 0; }
provider::destroy() { return 0; }
provider::output() { echo '{"api":{},"nodes":[],"network":{}}'; }
SCRIPT

  export PATH_LOK8S="${BATS_TEST_TMPDIR}/.lok8s"
  provider::load "mock"

  # All five contract functions should be declared
  declare -F provider::validate
  declare -F provider::credential_data
  declare -F provider::provision
  declare -F provider::destroy
  declare -F provider::output
  [ "${PROVIDER_NAME}" = "mock" ]
}

@test "provider::load fails for missing provider directory" {
  export PATH_LOK8S="${BATS_TEST_TMPDIR}/.lok8s"
  run provider::load "nonexistent"
  assert_failure
  assert_output --partial "not found"
}

# ── provider::check_contract ─────────────────────────────

@test "provider::check_contract fails when functions are missing" {
  # Source a provider that only implements one function
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/providers/incomplete"
  cat > "${BATS_TEST_TMPDIR}/.lok8s/providers/incomplete/main" <<'SCRIPT'
provider::validate() { return 0; }
SCRIPT

  export PATH_LOK8S="${BATS_TEST_TMPDIR}/.lok8s"
  export PROVIDER_NAME="incomplete"
  source "${BATS_TEST_TMPDIR}/.lok8s/providers/incomplete/main"

  run provider::check_contract
  assert_failure
  assert_output --partial "missing required functions"
  assert_output --partial "provider::credential_data"
  assert_output --partial "provider::provision"
  assert_output --partial "provider::destroy"
  assert_output --partial "provider::output"
}

# ── provider::write_config ───────────────────────────────

@test "provider::write_config exports PROVIDER_CONFIG_FILE with config content" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
spec:
  provider:
    name: hetzner
    config:
      region: fsn1
      sshKeyName: my-key
YAML

  provider::write_config "${BATS_TEST_TMPDIR}/spec.yaml"

  [ -n "${PROVIDER_CONFIG_FILE}" ]
  [ -f "${PROVIDER_CONFIG_FILE}" ]

  run cat "${PROVIDER_CONFIG_FILE}"
  assert_success
  assert_output --partial "region: fsn1"
  assert_output --partial "sshKeyName: my-key"
}

@test "provider::write_config handles missing config gracefully" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
spec:
  provider:
    name: hetzner
YAML

  provider::write_config "${BATS_TEST_TMPDIR}/spec.yaml"

  [ -n "${PROVIDER_CONFIG_FILE}" ]
  [ -f "${PROVIDER_CONFIG_FILE}" ]
  # Empty config → the file contains just {} (yq default)
  run cat "${PROVIDER_CONFIG_FILE}"
  assert_success
  assert_output "{}"
}

# ── configRef ─────────────────────────────────────────────

@test "provider::write_config resolves configRef relative to cluster dir" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"

  # The referenced config file
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/hetzner.yaml" <<'YAML'
region: fsn1
cluster_name: test-ref
YAML

  # Cluster spec with configRef
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
spec:
  provider:
    name: hetzner
    configRef: hetzner.yaml
YAML

  provider::write_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  [ -n "${PROVIDER_CONFIG_FILE}" ]
  run cat "${PROVIDER_CONFIG_FILE}"
  assert_success
  assert_output --partial "region: fsn1"
  assert_output --partial "cluster_name: test-ref"
}

@test "provider::write_config fails for missing configRef" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
spec:
  provider:
    name: hetzner
    configRef: nonexistent.yaml
YAML

  run provider::write_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "not found"
}

# ── Integration: provision::dispatch with provider ────────

@test "provision::dispatch loads provider when spec.provider.name is set and --remote" {
  local provision_log="${BATS_TEST_TMPDIR}/provision.log"
  # Provider load path only runs under --remote; plain local provision
  # ignores spec.provider even if present (see libs/provision:142).
  export LOK8S_REMOTE=1

  # Create a mock provider that logs when validate is called
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/providers/mock"
  cat > "${BATS_TEST_TMPDIR}/.lok8s/providers/mock/main" <<SCRIPT
provider::validate() { echo "validate_called" >> "${provision_log}"; return 0; }
provider::credential_data() { return 0; }
provider::provision() { return 0; }
provider::destroy() { return 0; }
provider::output() { echo '{"api":{},"nodes":[],"network":{}}'; }
SCRIPT

  # Create a cluster spec with spec.provider.name
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test
spec:
  cluster:
    domain: test.lok8s.dev
  provider:
    name: mock
    config:
      region: test
YAML

  # Create a fake driver
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo"
  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() { echo "provisioned"; }
SCRIPT

  # Mock yq for .kind resolution
  yq() {
    case "$2" in
      '.kind'|'.kind // ""') echo "Lo" ;;
      '.spec.gitops.provider // empty') echo "" ;;
      *) command yq "$@" ;;
    esac
  }
  export -f yq

  # Mock kubehz functions (pre-existing leak in libs/provision)
  kubehz::read_config() { :; }
  kubehz::validate_config() { return 0; }
  kubehz::register_cluster() { :; }
  export -f kubehz::read_config kubehz::validate_config kubehz::register_cluster
  export LOK8S_KUBEHZ_ACCESS="none"

  # Mock bootstrap::apply (tested separately in bootstrap_test.bats)
  bootstrap::apply() { :; }
  export -f bootstrap::apply

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  # Remote-lo targets a real cloud VM, so the infra gate (provision::confirm_infra,
  # lok8s#103) demands consent — bypass via the dynamic-scope force flag; consent
  # is the gate suite's subject, provider loading is this test's.
  force=1 provision::dispatch "test.lok8s.dev"

  # Verify provider::validate was called
  [ -f "${provision_log}" ]
  run cat "${provision_log}"
  assert_output --partial "validate_called"
}

@test "provision::dispatch works without spec.provider (Lo cluster)" {
  # Create a Lo cluster spec with no spec.provider
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test
spec:
  cluster:
    domain: test.lok8s.dev
YAML

  # Create a fake driver
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo"
  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() { echo "provisioned"; }
SCRIPT

  yq() {
    case "$2" in
      '.kind'|'.kind // ""') echo "Lo" ;;
      '.spec.gitops.provider // empty') echo "" ;;
      *) command yq "$@" ;;
    esac
  }
  export -f yq

  kubehz::read_config() { :; }
  kubehz::validate_config() { return 0; }
  export -f kubehz::read_config kubehz::validate_config

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  run provision::dispatch "test.lok8s.dev"
  assert_success
  assert_output --partial "provisioned"
}

# ── provision::read_kind ──────────────────────────────────

@test "provision::read_kind reads and lowercases .kind" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
kind: KubeOne
YAML
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  run provision::read_kind "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_success
  assert_output "kubeone"
}

@test "provision::read_kind rejects a missing .kind (yq's null must not pass as a driver name)" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
metadata:
  name: nokind
YAML
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  run provision::read_kind "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_failure
  assert_output --partial "no .kind"
}

@test "provision::read_kind rejects a traversal-shaped kind (it is interpolated into a sourced path)" {
  cat > "${BATS_TEST_TMPDIR}/spec.yaml" <<'YAML'
kind: "../../evil"
YAML
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"
  run provision::read_kind "${BATS_TEST_TMPDIR}/spec.yaml"
  assert_failure
  assert_output --partial "invalid cluster kind"
}

# ── hetzner::wipe-devices::script (#wipe-devices) ─────────

@test "wipe-devices::script true → disk enumeration + blkdiscard loop + sentinel" {
  run hetzner::wipe-devices::script 'true'
  assert_success
  assert_output --partial "lsblk -dn -o NAME,TYPE"
  assert_output --partial 'blkdiscard -f "${_dev}"'
  assert_output --partial "wipefs -a"
  assert_output --partial "__LOK8S_WIPE_DONE__"
}

@test "wipe-devices::script has NO dd partial-wipe fallback (abort instead)" {
  # a device that rejects blkdiscard must ABORT, never partially dd-zero —
  # a bounded dd would leave far-offset Ceph BlueStore labels + false-green.
  run hetzner::wipe-devices::script 'true'
  assert_success
  refute_output --partial "dd if=/dev/zero"
  assert_output --partial "does not support blkdiscard"

  run hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1"}]'
  assert_success
  refute_output --partial "dd if=/dev/zero"
  assert_output --partial "does not support blkdiscard"
}

@test "wipe-devices::script true carries the Ceph-bluestore full-discard rationale" {
  run hetzner::wipe-devices::script 'true'
  assert_success
  assert_output --partial "bdev-label"
  assert_output --partial "rook/rook#17716"
}

@test "wipe-devices::script list model → ID_MODEL assert-or-abort for that device" {
  run hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","model":"SAMSUNG MZQL2"}]'
  assert_success
  # whole-line exact match (-qxF), not substring — a prefix model must not match
  assert_output --partial 'grep -qxF "ID_MODEL=SAMSUNG MZQL2"'
  refute_output --partial 'grep -qF "ID_MODEL=SAMSUNG MZQL2"'
  assert_output --partial 'ABORT: /dev/nvme0n1 identity mismatch'
  assert_output --partial 'blkdiscard -f "/dev/nvme0n1"'
}

@test "wipe-devices::script list id → asserts ID_SERIAL / ID_WWN / by-id" {
  run hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","id":"S64GNS0T123"}]'
  assert_success
  assert_output --partial 'ID_SERIAL=S64GNS0T123'
  assert_output --partial 'ID_WWN=S64GNS0T123'
  assert_output --partial '/dev/disk/by-id/S64GNS0T123'
  assert_output --partial 'ABORT: /dev/nvme0n1 identity mismatch (id)'
}

@test "wipe-devices::script list model+id → both asserts present" {
  run hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","model":"MOD-X","id":"SER-Y"}]'
  assert_success
  assert_output --partial 'ID_MODEL=MOD-X'
  assert_output --partial 'ID_SERIAL=SER-Y'
  assert_output --partial 'ID_WWN=SER-Y'
}

@test "wipe-devices::script id-only entry (no device) targets /dev/disk/by-id" {
  run hetzner::wipe-devices::script '[{"id":"nvme-Samsung_SSD_123"}]'
  assert_success
  assert_output --partial 'blkdiscard -f "/dev/disk/by-id/nvme-Samsung_SSD_123"'
}

@test "wipe-devices::script absent (empty) → no-op, nothing emitted" {
  run hetzner::wipe-devices::script ''
  assert_success
  refute_output --partial "blkdiscard -f"
  [ -z "${output}" ]
}

@test "wipe-devices::script null → no-op, nothing emitted" {
  run hetzner::wipe-devices::script 'null'
  assert_success
  refute_output --partial "blkdiscard -f"
}

@test "wipe-devices::script false → no-op, no blkdiscard emitted" {
  run hetzner::wipe-devices::script 'false'
  assert_success
  refute_output --partial "blkdiscard -f"
}

@test "wipe-devices::script rejects a bare string (not true/array), no wipe" {
  run hetzner::wipe-devices::script '"just-a-string"'
  assert_failure
  refute_output --partial "blkdiscard -f"
}

@test "wipe-devices::script rejects an object (not true/array), no wipe" {
  run hetzner::wipe-devices::script '{"device":"/dev/sda"}'
  assert_failure
  refute_output --partial "blkdiscard -f"
}

@test "wipe-devices::script rejects unsafe descriptor values (no injection), no wipe" {
  # a model value carrying shell metacharacters must be refused, not templated
  run hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","model":"x\"; rm -rf / #"}]'
  assert_failure
  refute_output --partial "blkdiscard -f"
  assert_output --partial "unsafe"
}

@test "wipe-devices::script GUARD: model mismatch aborts before any blkdiscard" {
  local script
  script="$(hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","model":"EXPECTED-MODEL"}]')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  # udevadm reports a DIFFERENT model → the guard must fail closed
  cat > "${stub}/udevadm" <<'EOF'
#!/usr/bin/env bash
echo "ID_MODEL=SOME-OTHER-MODEL"
echo "ID_SERIAL=NOPE"
exit 0
EOF
  # recording fakes: any invocation is a test failure
  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
exit 0
EOF
  cat > "${stub}/dd" <<EOF
#!/usr/bin/env bash
echo "dd \$*" >> "${record}"
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/lsblk"
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  run env PATH="${stub}:${PATH}" bash -c "${script}"
  assert_failure
  assert_output --partial "ABORT"
  # the destructive commands must NEVER have run
  [ ! -f "${record}" ]
}

@test "wipe-devices::script GUARD: matching model proceeds to blkdiscard" {
  local script
  script="$(hetzner::wipe-devices::script '[{"device":"/dev/fakedisk0","model":"GOOD-MODEL"}]')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  cat > "${stub}/udevadm" <<'EOF'
#!/usr/bin/env bash
echo "ID_MODEL=GOOD-MODEL"
exit 0
EOF
  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  # '[' is a bash builtin PATH stubs cannot override; a shell-function override
  # makes our fake target pass the `[ -b ]` block-device assert and delegates
  # every other test to the real builtin.
  run env PATH="${stub}:${PATH}" bash -c "${_WIPE_BRACKET_STUB}
${script}"
  assert_success
  assert_output --partial "__LOK8S_WIPE_DONE__"
  refute_output --partial "ABORT"
  run cat "${record}"
  assert_output --partial "/dev/fakedisk0"
}

@test "wipe-devices::script GUARD: id mismatch (ID_SERIAL/ID_WWN) aborts before any blkdiscard" {
  local script
  script="$(hetzner::wipe-devices::script '[{"device":"/dev/fakedisk0","id":"EXPECTED-SERIAL"}]')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  # udevadm reports a DIFFERENT serial/wwn, and /dev/disk/by-id/EXPECTED-SERIAL
  # does not exist → every id branch fails → the guard must fail closed.
  cat > "${stub}/udevadm" <<'EOF'
#!/usr/bin/env bash
echo "ID_SERIAL=SOME-OTHER-SERIAL"
echo "ID_WWN=0xdeadbeef"
exit 0
EOF
  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
exit 0
EOF
  cat > "${stub}/dd" <<EOF
#!/usr/bin/env bash
echo "dd \$*" >> "${record}"
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  # even with the block-device assert satisfied, the id guard must abort first
  run env PATH="${stub}:${PATH}" bash -c "${_WIPE_BRACKET_STUB}
${script}"
  assert_failure
  assert_output --partial "ABORT: /dev/fakedisk0 identity mismatch (id)"
  refute_output --partial "__LOK8S_WIPE_DONE__"
  # the destructive commands must NEVER have run
  [ ! -f "${record}" ]
}

@test "wipe-devices::script GUARD: matching id (ID_SERIAL) proceeds to blkdiscard" {
  local script
  script="$(hetzner::wipe-devices::script '[{"device":"/dev/fakedisk0","id":"MATCH-SERIAL"}]')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  cat > "${stub}/udevadm" <<'EOF'
#!/usr/bin/env bash
echo "ID_SERIAL=MATCH-SERIAL"
exit 0
EOF
  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  run env PATH="${stub}:${PATH}" bash -c "${_WIPE_BRACKET_STUB}
${script}"
  assert_success
  assert_output --partial "__LOK8S_WIPE_DONE__"
  refute_output --partial "ABORT"
  run cat "${record}"
  assert_output --partial "/dev/fakedisk0"
}

@test "wipe-devices::script GUARD: true wipes every lsblk disk, skips non-disk rows" {
  local script
  script="$(hetzner::wipe-devices::script 'true')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  # two physical disks + one partition row that must be skipped
  cat > "${stub}/lsblk" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "fakedisk0 disk" "fakedisk1 disk" "fakedisk0p1 part"
EOF
  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
exit 0
EOF
  cat > "${stub}/dd" <<EOF
#!/usr/bin/env bash
echo "dd \$*" >> "${record}"
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  run env PATH="${stub}:${PATH}" bash -c "${_WIPE_BRACKET_STUB}
${script}"
  assert_success
  assert_output --partial "__LOK8S_WIPE_DONE__"
  refute_output --partial "ABORT"
  run cat "${record}"
  assert_output --partial "/dev/fakedisk0"
  assert_output --partial "/dev/fakedisk1"
  # the non-disk 'part' row must NOT have been wiped
  refute_output --partial "fakedisk0p1"
}

@test "wipe-devices::script rejects an array with a non-object element (no wipe)" {
  run hetzner::wipe-devices::script '["/dev/nvme0n1"]'
  assert_failure
  refute_output --partial "blkdiscard -f"
}

@test "wipe-devices::script GUARD: true aborts mid-loop when a later disk rejects blkdiscard" {
  local script
  script="$(hetzner::wipe-devices::script 'true')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  cat > "${stub}/lsblk" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "fakedisk0 disk" "fakedisk1 disk"
EOF
  # succeeds on fakedisk0, fails on fakedisk1 (device rejects discard)
  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
case "\$*" in *fakedisk1*) exit 1 ;; *) exit 0 ;; esac
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  run env PATH="${stub}:${PATH}" bash -c "${_WIPE_BRACKET_STUB}
${script}"
  # the per-disk exit 1 must propagate out of the lsblk|while subshell:
  assert_failure
  refute_output --partial "__LOK8S_WIPE_DONE__"
  assert_output --partial "ABORT: /dev/fakedisk1 does not support blkdiscard"
  run cat "${record}"
  assert_output --partial "/dev/fakedisk0"
}

@test "wipe-devices::script GUARD: non-block target aborts before blkdiscard" {
  local script
  # a bare device entry (no model/id) → the only gate is the [ -b ] assert
  script="$(hetzner::wipe-devices::script '[{"device":"/dev/lok8s-nonexistent-testdev"}]')"

  local stub="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${stub}"
  local record="${BATS_TEST_TMPDIR}/destructive.calls"

  cat > "${stub}/blkdiscard" <<EOF
#!/usr/bin/env bash
echo "blkdiscard \$*" >> "${record}"
exit 0
EOF
  cat > "${stub}/dd" <<EOF
#!/usr/bin/env bash
echo "dd \$*" >> "${record}"
exit 0
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/udevadm"
  printf '#!/usr/bin/env bash\nexit 0\n' > "${stub}/wipefs"
  chmod +x "${stub}"/*

  # NO bracket override here: the real builtin `[ -b ]` must fail on a target
  # that is not a block special file.
  run env PATH="${stub}:${PATH}" bash -c "${script}"
  assert_failure
  assert_output --partial "ABORT: /dev/lok8s-nonexistent-testdev not a block device"
  refute_output --partial "__LOK8S_WIPE_DONE__"
  [ ! -f "${record}" ]
}

# ── boundary: path-traversal / non-/dev targets rejected at generation ──

@test "wipe-devices::script rejects a device outside /dev (no wipe emitted)" {
  run hetzner::wipe-devices::script '[{"device":"etc/passwd"}]'
  assert_failure
  refute_output --partial "blkdiscard -f"
  assert_output --partial "absolute /dev path"
}

@test "wipe-devices::script rejects a device with '..' traversal (no wipe emitted)" {
  run hetzner::wipe-devices::script '[{"device":"/dev/../etc/passwd"}]'
  assert_failure
  refute_output --partial "blkdiscard -f"
  assert_output --partial "without '..'"
}

@test "wipe-devices::script rejects an id containing a slash (no wipe emitted)" {
  run hetzner::wipe-devices::script '[{"id":"../../dev/sda"}]'
  assert_failure
  refute_output --partial "blkdiscard -f"
  assert_output --partial "bare by-id name"
}

@test "wipe-devices::script rejects an id containing '..' (no wipe emitted)" {
  run hetzner::wipe-devices::script '[{"id":"nvme..evil"}]'
  assert_failure
  refute_output --partial "blkdiscard -f"
  assert_output --partial "bare by-id name"
}

# ── hetzner::wipe-devices (the ssh runner) ────────────────
#
# Hermetic: stub `hetzner::json` to hand the runner a chosen #wipe-devices value
# and `ssh` to echo canned remote output — no real ssh/network. The runner's
# success contract is: the DONE sentinel present AND no line-leading `ABORT:`.

@test "wipe-devices runner: DONE sentinel + no leading ABORT → success" {
  hetzner::json() { printf '%s\n' 'true'; }
  local _sshlog="${BATS_TEST_TMPDIR}/ssh.called"
  ssh() { cat >/dev/null 2>&1; printf '%s\n' "$@" > "${_sshlog}"; printf '%s\n' "lok8s: wipe /dev/sda" "__LOK8S_WIPE_DONE__"; }

  run hetzner::wipe-devices 0 "root@203.0.113.10" -o BatchMode=yes
  assert_success
  # ssh WAS invoked (the wipe was actually attempted)
  [ -f "${_sshlog}" ]
}

@test "wipe-devices runner: a line-leading ABORT: → failure (even with sentinel)" {
  hetzner::json() { printf '%s\n' 'true'; }
  # sentinel present too, so the ONLY reason to fail is the ABORT: detection
  ssh() { cat >/dev/null 2>&1; printf '%s\n' "ABORT: /dev/sda identity mismatch (model)" "__LOK8S_WIPE_DONE__"; }

  run hetzner::wipe-devices 0 "root@203.0.113.10" -o BatchMode=yes
  assert_failure
  assert_output --partial "device wipe FAILED"
}

@test "wipe-devices runner: mid-line ABORT substring (not line-leading) → success" {
  hetzner::json() { printf '%s\n' 'true'; }
  # 'ABORT' appears only mid-line — the ^ABORT: anchor must NOT treat it as a
  # failure. Sentinel present → the wipe is reported clean.
  ssh() { cat >/dev/null 2>&1; printf '%s\n' "note: no ABORT: happened here" "__LOK8S_WIPE_DONE__"; }

  run hetzner::wipe-devices 0 "root@203.0.113.10" -o BatchMode=yes
  assert_success
  refute_output --partial "device wipe FAILED"
}

@test "wipe-devices runner: descriptor neither true nor array → rejected, no ssh" {
  hetzner::json() { printf '%s\n' '"just-a-string"'; }
  local _sshlog="${BATS_TEST_TMPDIR}/ssh.called"
  # if ssh is reached the test fails: this marker must never be written
  ssh() { : > "${_sshlog}"; printf '%s\n' "__LOK8S_WIPE_DONE__"; }

  run hetzner::wipe-devices 0 "root@203.0.113.10" -o BatchMode=yes
  assert_failure
  assert_output --partial "must be 'true' or an array"
  # no wipe was ever attempted over ssh
  [ ! -f "${_sshlog}" ]
}
