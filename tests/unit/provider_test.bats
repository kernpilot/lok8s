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
      .kind) echo "Lo" ;;
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
  provision::dispatch "test.lok8s.dev"

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
      .kind) echo "Lo" ;;
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

# ── hetzner::wipe-devices::script (#wipe-devices) ─────────

@test "wipe-devices::script true → disk enumeration + blkdiscard loop + sentinel" {
  run hetzner::wipe-devices::script 'true'
  assert_success
  assert_output --partial "lsblk -dn -o NAME,TYPE"
  assert_output --partial 'blkdiscard -f "${_dev}"'
  assert_output --partial "dd if=/dev/zero"
  assert_output --partial "wipefs -a"
  assert_output --partial "__LOK8S_WIPE_DONE__"
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
  assert_output --partial 'grep -qF "ID_MODEL=SAMSUNG MZQL2"'
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
  refute_output --partial "blkdiscard"
  [ -z "${output}" ]
}

@test "wipe-devices::script null → no-op, nothing emitted" {
  run hetzner::wipe-devices::script 'null'
  assert_success
  refute_output --partial "blkdiscard"
}

@test "wipe-devices::script false → no-op, no blkdiscard emitted" {
  run hetzner::wipe-devices::script 'false'
  assert_success
  refute_output --partial "blkdiscard"
}

@test "wipe-devices::script rejects a bare string (not true/array), no wipe" {
  run hetzner::wipe-devices::script '"just-a-string"'
  assert_failure
  refute_output --partial "blkdiscard"
}

@test "wipe-devices::script rejects an object (not true/array), no wipe" {
  run hetzner::wipe-devices::script '{"device":"/dev/sda"}'
  assert_failure
  refute_output --partial "blkdiscard"
}

@test "wipe-devices::script rejects unsafe descriptor values (no injection), no wipe" {
  # a model value carrying shell metacharacters must be refused, not templated
  run hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","model":"x\"; rm -rf / #"}]'
  assert_failure
  refute_output --partial "blkdiscard"
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
  script="$(hetzner::wipe-devices::script '[{"device":"/dev/nvme0n1","model":"GOOD-MODEL"}]')"

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

  run env PATH="${stub}:${PATH}" bash -c "${script}"
  assert_success
  assert_output --partial "__LOK8S_WIPE_DONE__"
  refute_output --partial "ABORT"
  run cat "${record}"
  assert_output --partial "/dev/nvme0n1"
}
