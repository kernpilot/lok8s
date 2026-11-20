#!/usr/bin/env bats
# kubehz_handover_test.bats — `lo kubehz handover` (eject target side).
#
# Guards .lok8s/libs/kubehz/handover against the cross-repo handover
# contract (§2 bundle keys, §4 lok8s scope):
#   - bundle validation names the missing key;
#   - receive (kubeadm path) runs the steps IN ORDER — snapshot restore →
#     kubeadm init (with the documented DirAvailable--var-lib-etcd ignore) →
#     verify — against a sandboxed /etc/kubernetes + /var/lib/etcd
#     (KUBEHZ_HANDOVER_K8S_DIR / _ETCD_DIR test seams);
#   - the generated kubeadm config wires the endpoint DNS (certSANs +
#     controlPlaneEndpoint) and the encryption-provider-config;
#   - the CA-hash verify FAILS when kubeadm re-minted the CA (identity lost);
#   - preseed places exactly the six PKI files over scp and nothing else.
#
# Mock idiom mirrors kubehz_register_test.bats: bash function mocks
# (export -f) over a sourced lib, with a shared CALLS log for order asserts.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/handover"

  # The argsh :args/:usage builtins are absent when a lib is sourced in bats —
  # shim them to parse exactly as argsh does (the recover_test.bats idiom),
  # including the ':!' required-flag refusal BEFORE the body runs.
  :args() {
    shift  # description
    while (( $# )); do
      case "$1" in
        --bundle|-b)   bundle="$2"; shift ;;
        --snapshot|-s) snapshot="$2"; shift ;;
        --single-node) single_node=1 ;;
        --force|-f)    force=1 ;;
        --node|-n)     node="$2"; shift ;;
        --user|-u)     user="$2"; shift ;;
        --port|-p)     port="$2"; shift ;;
        --ssh-key|-i)  ssh_key="$2"; shift ;;
        --known-hosts) known_hosts="$2"; shift ;;
        --accept-new-host-key) accept_new_host_key=1 ;;
      esac
      shift
    done
    local spec name var
    for spec in "${args[@]}"; do
      [[ "${spec}" == *':!'* ]] || continue
      name="${spec%%|*}"
      var="${name//-/_}"
      [[ -n "${!var:-}" ]] || { echo "Error: missing required flag: ${name}" >&2; exit 2; }
    done
  }
  # :usage resolves the subcommand to handover::<cmd> and stashes the tail —
  # mirrors the real dispatch (kubehz::handover → handover::receive).
  :usage() {
    shift  # description
    local cmd="$1"; shift
    usage=("handover::${cmd}" "$@")
  }
  export -f :args :usage

  export CALLS="${BATS_TEST_TMPDIR}/calls.log"
  export KUBEHZ_HANDOVER_K8S_DIR="${BATS_TEST_TMPDIR}/etc-kubernetes"
  export KUBEHZ_HANDOVER_ETCD_DIR="${BATS_TEST_TMPDIR}/var-lib-etcd"
  export KUBEHZ_HANDOVER_WAIT=0
}

teardown() {
  # A server of tests/lib/http_stub.bash that a failed test left running.
  [[ -z "${STUB_PID:-}" ]] || kill "${STUB_PID}" 2>/dev/null || :
  teardown_tmpdir
}

# A complete contract §2 bundle. The snapshot-location deliberately points at
# s3:// so tests prove --snapshot short-circuits the remote fetch.
_make_bundle() {
  BUNDLE="${BATS_TEST_TMPDIR}/bundle"
  mkdir -p "${BUNDLE}"
  printf -- '-----BEGIN CERTIFICATE-----\ntest-cluster-ca\n-----END CERTIFICATE-----\n' > "${BUNDLE}/ca.crt"
  printf -- '-----BEGIN RSA PRIVATE KEY-----\ntest-ca-key\n-----END RSA PRIVATE KEY-----\n' > "${BUNDLE}/ca.key"
  printf -- '-----BEGIN PUBLIC KEY-----\ntest-sa-pub\n-----END PUBLIC KEY-----\n' > "${BUNDLE}/sa.pub"
  printf -- '-----BEGIN RSA PRIVATE KEY-----\ntest-sa-key\n-----END RSA PRIVATE KEY-----\n' > "${BUNDLE}/sa.key"
  printf -- '-----BEGIN CERTIFICATE-----\ntest-fpca\n-----END CERTIFICATE-----\n' > "${BUNDLE}/front-proxy-ca.crt"
  printf -- '-----BEGIN RSA PRIVATE KEY-----\ntest-fpca-key\n-----END RSA PRIVATE KEY-----\n' > "${BUNDLE}/front-proxy-ca.key"
  printf 'dGVzdC1lbmNyeXB0aW9uLWtleQ==' > "${BUNDLE}/encryption-key"
  printf 's3://kubehz-backups/handover/cl-001/snap.db' > "${BUNDLE}/snapshot-location"
  printf 'cl-001.kubermatic.kkp.kubehz.in.net' > "${BUNDLE}/endpoint-dns"

  SNAPSHOT="${BATS_TEST_TMPDIR}/snap.db"
  printf 'fake-etcd-snapshot' > "${SNAPSHOT}"
}

# Happy-path mocks: every external binary logs into CALLS.
_mock_node_binaries() {
  # A real restore leaves ${ETCD_DIR}/member behind. The mock must too, or the
  # "nothing was seeded" assertions pass whether or not the restore ran.
  etcdutl() {
    echo "etcdutl $*" >> "${CALLS}"
    mkdir -p "${KUBEHZ_HANDOVER_ETCD_DIR}/member"
  }
  kubeadm() { echo "kubeadm $*" >> "${CALLS}"; }
  kubectl() { echo "kubectl $*" >> "${CALLS}"; return 0; }
  # Deterministic node identity for the restore's member rewrite — uppercase
  # on purpose so the lowercase mirroring is exercised.
  hostname() { echo "Node-X"; }
  ip() { echo "1.1.1.1 via 10.9.8.1 dev eth0 src 10.9.8.7 uid 0"; }
  export -f etcdutl kubeadm kubectl hostname ip
}

# A fake node for preseed under NODE_ROOT. ssh runs the remote command with
# sh in the C locale: each /etc/kubernetes in it points into NODE_ROOT, and
# its output maps back. scp writes the source into its destination as scp
# does: into a file that is there already, also through a hard link or a
# symlink. NODE_ROOT/etc/kubernetes is a 755 dir of the test user.
# `_node_hook TOOL ARG` prints the remote command of an ssh call or the
# destination of an scp call. A test redefines it to change one call, or to
# fail it before it runs (a status that is not 0). For scp, the hook prints
# to the stdout of scp on fd 3.
_fake_node() {
  export NODE_ROOT="${BATS_TEST_TMPDIR}/node"
  mkdir -p "${NODE_ROOT}/etc/kubernetes"
  chmod 755 "${NODE_ROOT}/etc/kubernetes"
  _node_hook() { printf '%s\n' "${2}"; }
  ssh() {
    echo "ssh $*" >> "${CALLS}"
    local cmd out rc=0
    cmd="$(_node_hook ssh "${@: -1}")" || return
    out="$(LC_ALL=C sh -c "${cmd//\/etc\/kubernetes/${NODE_ROOT}/etc/kubernetes}")" || rc=$?
    [[ -z "${out}" ]] || printf '%s\n' "${out//"${NODE_ROOT}"/}"
    return "${rc}"
  }
  scp() {
    echo "scp $*" >> "${CALLS}"
    local dst
    { dst="$(_node_hook scp "${@: -1}")"; } 3>&1 || return
    cat -- "${@: -2:1}" > "${NODE_ROOT}${dst#*:}"
  }
  export -f _node_hook ssh scp
}

# The remote command of preseed's stage call, spelled out: a check of each
# dir on the path of the pki dir, from the top (each prints its stat line
# first; root must own / and /etc), "stage", then a staging file per key. The
# Go suite spells out the same bytes (preseedStage).
_preseed_stage() {
  local stage
  IFS= read -r stage <<'EOF'
umask 077 && echo "check / $(stat -c '%u %a %F' -- /)" && test -n "$(find / -maxdepth 0 -type d ! -perm /022 -uid 0 -print)" && echo "check /etc $(stat -c '%u %a %F' -- /etc)" && test -n "$(find /etc -maxdepth 0 -type d ! -perm /022 -uid 0 -print)" && echo "check /etc/kubernetes $(stat -c '%u %a %F' -- /etc/kubernetes)" && test -n "$(find /etc/kubernetes -maxdepth 0 -type d ! -perm /022 \( -uid 0 -o -uid "$(id -u)" \) -print)" && echo "check /etc/kubernetes/pki $(stat -c '%u %a %F' -- /etc/kubernetes/pki)" && test -n "$(find /etc/kubernetes/pki -maxdepth 0 -type d ! -perm /022 \( -uid 0 -o -uid "$(id -u)" \) -print)" && echo stage && mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.ca.key.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.sa.pub.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.sa.key.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.front-proxy-ca.crt.XXXXXXXXXX && mktemp /etc/kubernetes/pki/.front-proxy-ca.key.XXXXXXXXXX
EOF
  printf '%s\n' "${stage}"
}

# The staging files left in the pki dir of the fake node (its dot files).
_staging_left() {
  find "${NODE_ROOT}/etc/kubernetes/pki" -mindepth 1 -maxdepth 1 -name '.*' \
    -printf '/etc/kubernetes/pki/%f\n' 2>/dev/null | LC_ALL=C sort
}

# Fail unless each key is a file with one name in the pki dir of the fake
# node, with the bytes of the bundle and the mode of its kind (the keys 600,
# the certs and sa.pub 644).
_assert_installed() {
  local key path mode
  for key in "$@"; do
    path="${NODE_ROOT}/etc/kubernetes/pki/${key}"
    [[ -f "${path}" && ! -L "${path}" ]] || { echo "not a file: ${path}"; return 1; }
    [ "$(stat -c '%h' "${path}")" = 1 ] || { echo "more than one name: ${path}"; return 1; }
    mode=644
    [[ "${key}" != *.key ]] || mode=600
    [ "$(stat -c '%a' "${path}")" = "${mode}" ] || { echo "mode of ${path}: $(stat -c '%a' "${path}")"; return 1; }
    cmp -s "${path}" "${BUNDLE}/${key}" || { echo "not the bytes of the bundle: ${path}"; return 1; }
  done
}

# ── Bundle validation ────────────────────────────────────

@test "handover: validate_bundle passes on a complete bundle" {
  _make_bundle
  run handover::validate_bundle "${BUNDLE}"
  assert_success
}

@test "handover: validate_bundle NAMES the missing key" {
  _make_bundle
  rm "${BUNDLE}/sa.key"
  run handover::validate_bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "missing or empty key: sa.key"
}

@test "handover: validate_bundle rejects an empty key file too" {
  _make_bundle
  : > "${BUNDLE}/encryption-key"
  run handover::validate_bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "missing or empty key: encryption-key"
}

@test "handover: resolve_bundle unpacks a .tar.gz into a private dir" {
  _make_bundle
  # The mktemp dir goes under the test dir, so the teardown removes it.
  export TMPDIR="${BATS_TEST_TMPDIR}"
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  tar -czf "${tarball}" -C "${BUNDLE}" .
  run handover::resolve_bundle "${tarball}"
  assert_success
  [[ "${output}" == "${BATS_TEST_TMPDIR}/"* ]]
  [ -s "${output}/ca.crt" ]
  [ -s "${output}/endpoint-dns" ]
  run stat -c '%a' "${output}"
  assert_output "700"
}

# `tar -C <dir> .` writes a "./" entry, and tar gives that entry's mode to
# the dir it unpacks into. The bundle dir must stay 0700 and the keys 0600,
# with a workdir and without one.
@test "resolve_bundle: a './' entry of mode 0755 or 0777 leaves the dir 0700 and the keys 0600" {
  _make_bundle
  export TMPDIR="${BATS_TEST_TMPDIR}"
  local m perms tarball wd first
  for m in 755 777; do
    case "${m}" in 755) perms="drwxr-xr-x" ;; 777) perms="drwxrwxrwx" ;; esac
    chmod "${m}" "${BUNDLE}"
    chmod 644 "${BUNDLE}/ca.key"
    tarball="${BATS_TEST_TMPDIR}/bundle-${m}.tar.gz"
    tar -czf "${tarball}" -C "${BUNDLE}" .
    # The archive carries the mode under test on its "./" entry.
    tar -tvzf "${tarball}" > "${BATS_TEST_TMPDIR}/list-${m}"
    first="$(head -n 1 "${BATS_TEST_TMPDIR}/list-${m}")"
    [[ "${first}" == "${perms} "* && "${first}" == *" ./" ]]

    wd="${BATS_TEST_TMPDIR}/wd-${m}"
    mkdir -m 700 "${wd}"
    run handover::resolve_bundle "${tarball}" "${wd}"
    assert_success
    assert_output "${wd}/bundle"
    run stat -c '%a' "${wd}/bundle"
    assert_output "700"
    run stat -c '%a' "${wd}/bundle/ca.key"
    assert_output "600"

    run handover::resolve_bundle "${tarball}"
    assert_success
    [[ "${output}" == "${BATS_TEST_TMPDIR}/"* ]]
    run stat -c '%a' "${output}"
    assert_output "700"
  done
}

@test "resolve_bundle: a dir that the unpack left open fails closed and is removed" {
  # A tar that does not honour the options (a tar run as root that restores
  # the "./" mode, for example): the stub runs the real tar and then opens
  # the dir that -C names.
  _make_bundle
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz" wd="${BATS_TEST_TMPDIR}/wd"
  tar -czf "${tarball}" -C "${BUNDLE}" .
  mkdir -m 700 "${wd}"
  tar() {
    command tar "$@" || return
    local arg prev=""
    for arg in "$@"; do
      [[ "${prev}" != "-C" ]] || command chmod 755 "${arg}"
      prev="${arg}"
    done
  }
  export -f tar
  run handover::resolve_bundle "${tarball}" "${wd}"
  assert_failure
  assert_output --partial "handover: ${wd}/bundle has mode 755, not 700"
  [ ! -e "${wd}/bundle" ]
}

@test "handover: make_private stops when chmod fails, with its own message" {
  local d="${BATS_TEST_TMPDIR}/d"
  mkdir -m 700 "${d}"
  chmod() { return 1; }
  export -f chmod
  run handover::make_private "${d}"
  assert_failure
  assert_output --partial "handover: cannot set ${d} to mode 700, so the bundle's keys would not be private in it. Check the file system of TMPDIR, then retry."
}

# A chmod stub that fails for the path that FAIL_CHMOD names (a glob), and
# runs the real chmod for every other path.
_fail_chmod_at() {
  export FAIL_CHMOD="${1}"
  chmod() {
    # shellcheck disable=SC2254 # FAIL_CHMOD is a glob on purpose
    case "${*: -1}" in ${FAIL_CHMOD}) return 1 ;; esac
    command chmod "$@"
  }
  export -f chmod
}

@test "handover: place_pki stops when a chmod fails at the pki dir or at any file, and leaves no key" {
  # Each chmod on its own: the dir, then the new file of each of the six
  # PKI files (before its rename). The step stops, and the files that it
  # placed go.
  _make_bundle
  local pki="${KUBEHZ_HANDOVER_K8S_DIR}/pki" target
  for target in pki ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key; do
    rm -rf -- "${KUBEHZ_HANDOVER_K8S_DIR}"
    if [[ "${target}" == pki ]]; then
      _fail_chmod_at "${pki}"
    else
      _fail_chmod_at "${pki}/.${target}.*"
    fi
    run handover::place_pki "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
    assert_failure
    if [[ "${target}" == pki ]]; then
      assert_output --partial "handover: cannot set ${pki} to mode 700, so the keys would not be private in it"
    else
      assert_output --partial "handover: could not place ${target} into ${pki} with mode"
    fi
    run find "${pki}" -mindepth 1
    assert_success
    assert_output ""
  done
}

# A chmod that returns 0 but leaves the pki dir open (a file system that
# keeps the bits) fails closed too: place_pki reads the mode after it.
@test "handover: place_pki reads the mode of the pki dir after its chmod" {
  _make_bundle
  export PKI="${KUBEHZ_HANDOVER_K8S_DIR}/pki"
  chmod() {
    if [[ "${*: -1}" == "${PKI}" ]]; then
      command chmod 755 "${PKI}"
      return 0
    fi
    command chmod "$@"
  }
  export -f chmod
  run handover::place_pki "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_failure
  assert_output --partial "handover: cannot set ${PKI} to mode 700, so the keys would not be private in it"
  run find "${PKI}" -mindepth 1
  assert_output ""
}

# kubeadm makes /etc/kubernetes/pki 0755, and a run with --force finds the
# files of an earlier run. The dir is 0700 before a key lands in it, and
# each file is replaced, not written into: a key of mode 0644 ends 0600, a
# second name of it outside keeps the old bytes, and a link at a key name
# is replaced, not followed.
@test "handover: place_pki makes an open pki dir 0700 and replaces each file with its own mode" {
  _make_bundle
  local pki="${KUBEHZ_HANDOVER_K8S_DIR}/pki" outside="${BATS_TEST_TMPDIR}/outside" key
  mkdir -p "${pki}" "${outside}"
  chmod 755 "${pki}"
  printf 'old-ca-key\n' > "${pki}/ca.key"
  chmod 644 "${pki}/ca.key"
  ln "${pki}/ca.key" "${outside}/second-name"
  printf 'outside-secret\n' > "${outside}/secret"
  ln -s "${outside}/secret" "${pki}/sa.key"
  run handover::place_pki "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_success
  run stat -c '%a' "${pki}"
  assert_output "700"
  for key in ca.key sa.key front-proxy-ca.key; do
    [ ! -L "${pki}/${key}" ]
    run stat -c '%F %a %h' "${pki}/${key}"
    assert_output "regular file 600 1"
    cmp -- "${BUNDLE}/${key}" "${pki}/${key}"
  done
  for key in ca.crt sa.pub front-proxy-ca.crt; do
    run stat -c '%F %a' "${pki}/${key}"
    assert_output "regular file 644"
    cmp -- "${BUNDLE}/${key}" "${pki}/${key}"
  done
  run cat "${outside}/second-name"
  assert_output "old-ca-key"
  run cat "${outside}/secret"
  assert_output "outside-secret"
  # No new file is left behind.
  run find "${pki}" -name '.*'
  assert_output ""
}

# The encryption config holds the key: it is written 0600 from the first
# byte, also over a file of an earlier run with mode 0644. A chmod that
# fails stops the step and leaves no file.
@test "handover: write_encryption_config replaces an open file with a 0600 one, and a failed chmod leaves none" {
  _make_bundle
  local out="${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  mkdir -p "${KUBEHZ_HANDOVER_K8S_DIR}"
  printf 'old\n' > "${out}"
  chmod 644 "${out}"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_success
  assert_output "${out}"
  run stat -c '%a' "${out}"
  assert_output "600"
  # The key comes from the bundle file, so no key-like literal is in the test.
  run grep -c -F -- "secret: $(< "${BUNDLE}/encryption-key")" "${out}"
  assert_output "1"

  rm -f -- "${out}"
  _fail_chmod_at "${KUBEHZ_HANDOVER_K8S_DIR}/.kubehz-encryption-config.yaml.*"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_failure
  assert_output --partial "handover: could not write ${out} with mode 600"
  run find "${KUBEHZ_HANDOVER_K8S_DIR}" -mindepth 1
  assert_output ""
}

# An archive can give a dir in it a mode without owner search (0400) or
# without owner write (0500). The first stops the find after the unpack,
# and the second keeps its files from rm -rf, so the keys would stay on
# disk. The owner gets rwx on every dir before the find and before each
# removal. tar sets the mode of a dir after the entries under it, so the
# dir entry comes first and its file right after it (--mode sets the mode
# in the archive only).
# A new file of install_file can hold key bytes, so a removal that fails
# names it.
@test "handover: install_file names a new file that it cannot remove" {
  local dir="${BATS_TEST_TMPDIR}/pki" left
  mkdir -m 700 "${dir}"
  printf 'key bytes\n' > "${BATS_TEST_TMPDIR}/key"
  mv() { return 1; }
  rm() { return 1; }
  export -f mv rm
  run handover::install_file "${dir}/ca.key" 600 < "${BATS_TEST_TMPDIR}/key"
  unset -f mv rm
  assert_failure
  left="$(find "${dir}" -mindepth 1)"
  [[ "${left}" == "${dir}"/.ca.key.?????????? ]] || { echo "left: ${left}"; return 1; }
  assert_output --partial "handover: could not remove ${left}, which can hold key bytes. Delete it yourself."
}

@test "resolve_bundle: dirs that the archive left without owner search or write are checked and removed" {
  [[ "${EUID}" -ne 0 ]] || skip "root removes a dir without owner write or search, so the test cannot see the repair"
  _make_bundle
  local src="${BATS_TEST_TMPDIR}/src" plain="${BATS_TEST_TMPDIR}/bundle.tar" tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz" wd="${BATS_TEST_TMPDIR}/wd"
  cp -R "${BUNDLE}" "${src}"
  mkdir "${src}/noexec" "${src}/nowrite"
  printf 'held\n' > "${src}/noexec/file"
  printf 'held\n' > "${src}/nowrite/file"
  tar -cf "${plain}" -C "${src}" --no-recursion . "${KUBEHZ_HANDOVER_BUNDLE_KEYS[@]/#/./}"
  tar -rf "${plain}" -C "${src}" --no-recursion --mode=u=r,go= ./noexec
  tar -rf "${plain}" -C "${src}" --no-recursion ./noexec/file
  tar -rf "${plain}" -C "${src}" --no-recursion --mode=u=rx,go= ./nowrite
  tar -rf "${plain}" -C "${src}" --no-recursion ./nowrite/file
  gzip -c "${plain}" > "${tarball}"
  run tar -tvzf "${tarball}"
  assert_line --regexp '^dr-------- .* \./noexec/?$'
  assert_line --regexp '^dr-x------ .* \./nowrite/?$'
  mkdir -m 700 "${wd}"
  run handover::resolve_bundle "${tarball}" "${wd}"
  assert_success
  assert_output "${wd}/bundle"
  run handover::remove_dir "${wd}/bundle"
  assert_success
  [ ! -e "${wd}/bundle" ]

  # An unpack that fails after it made the dirs: the dir still goes. The
  # stub logs the two modes that the real unpack left.
  tar() {
    command tar "$@" || return
    case " $* " in *" -xzf "*) ;; *) return 0 ;; esac
    local arg prev=""
    for arg in "$@"; do
      [[ "${prev}" != "-C" ]] || command stat -c '%a' "${arg}/noexec" "${arg}/nowrite" >> "${CALLS}"
      prev="${arg}"
    done
    return 2
  }
  # remove_dir gives the owner rwx before its rm: the stub logs the two
  # modes that rm meets.
  rm() {
    [[ "${*: -1}" != "${wd}/bundle" ]] || command stat -c '%a' "${wd}/bundle/noexec" "${wd}/bundle/nowrite" >> "${CALLS}"
    command rm "$@"
  }
  export -f tar rm
  run handover::resolve_bundle "${tarball}" "${wd}"
  unset -f rm
  assert_failure
  assert_output --partial "is neither a bundle directory nor a readable .tar.gz archive"
  refute_output --partial "could not remove"
  [ ! -e "${wd}/bundle" ]
  run cat "${CALLS}"
  assert_output $'400\n500\n700\n700'
}

# The RETURN trap of receive and preseed (handover::cleanup_cmd) removes a
# dir that holds a dir without owner write, and a removal that fails names
# the dir.
@test "handover: the cleanup trap removes a dir without owner write, and names a dir it cannot remove" {
  [[ "${EUID}" -ne 0 ]] || skip "root removes a dir without owner write, so the test cannot see the repair"
  local wd="${BATS_TEST_TMPDIR}/it's a wd"
  mkdir -p "${wd}/bundle/nowrite"
  printf 'held\n' > "${wd}/bundle/nowrite/file"
  chmod 0500 "${wd}/bundle/nowrite"
  # remove_dir gives the owner rwx before its rm: the stub logs the mode
  # that rm meets.
  rm() {
    [[ "${*: -1}" != "${wd}" ]] || command stat -c '%a' "${wd}/bundle/nowrite" >> "${CALLS}"
    command rm "$@"
  }
  export -f rm
  run eval "$(handover::cleanup_cmd "${wd}")"
  unset -f rm
  assert_success
  [ ! -e "${wd}" ]
  run cat "${CALLS}"
  assert_output "700"

  mkdir -p "${wd}"
  rm() { return 1; }
  export -f rm
  run handover::remove_dir "${wd}"
  assert_failure
  assert_output --partial "handover: could not remove ${wd}, which can hold the bundle's keys. Delete it yourself."
  unset -f rm
}

# The listing check parses the -tv format, so it runs in the C locale.
@test "resolve_bundle: the listing check runs with LC_ALL=C" {
  _make_bundle
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz" wd="${BATS_TEST_TMPDIR}/wd"
  tar -czf "${tarball}" -C "${BUNDLE}" .
  mkdir -m 700 "${wd}"
  export LC_ALL=POSIX
  tar() {
    case " $* " in *" -tvzf "*) echo "listing LC_ALL=${LC_ALL-}" >> "${CALLS}" ;; esac
    command tar "$@"
  }
  export -f tar
  run handover::resolve_bundle "${tarball}" "${wd}"
  assert_success
  run cat "${CALLS}"
  assert_output "listing LC_ALL=C"
}

# A tar that logs each unpack into CALLS, so a test can see that none ran.
_log_tar_unpacks() {
  tar() {
    case " $* " in *" -xzf "*) echo "tar -x $*" >> "${CALLS}" ;; esac
    command tar "$@"
  }
  export -f tar
}

# resolve_bundle refused <tarball> with the entry-type message before any
# unpack, removed the bundle dir, and <outside> holds only its "secret".
_assert_refused_before_unpack() {
  local tarball="${1}" wd="${2}" outside="${3}"
  run handover::resolve_bundle "${tarball}" "${wd}"
  assert_failure
  assert_output --partial "handover: refusing ${tarball}: an archive entry is not a regular file or a directory (a link, a device or a FIFO)"
  [ ! -s "${CALLS}" ] || { cat "${CALLS}"; return 1; }
  [ ! -e "${wd}/bundle" ]
  run ls -A "${outside}"
  assert_output "secret"
  run cat "${outside}/secret"
  assert_output "outside-secret"
}

# A link entry can name a file outside the bundle (ca.key -> a secret), and
# preseed would send that file to the node.
@test "resolve_bundle: a symlink entry is refused before the unpack, and the dir is removed" {
  _make_bundle
  local src="${BATS_TEST_TMPDIR}/src" outside="${BATS_TEST_TMPDIR}/outside" wd="${BATS_TEST_TMPDIR}/wd"
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  mkdir -m 700 "${outside}" "${wd}"
  printf 'outside-secret\n' > "${outside}/secret"
  cp -R "${BUNDLE}" "${src}"
  rm "${src}/ca.key"
  ln -s "${outside}/secret" "${src}/ca.key"
  tar -czf "${tarball}" -C "${src}" .
  _log_tar_unpacks
  _assert_refused_before_unpack "${tarball}" "${wd}" "${outside}"
}

@test "resolve_bundle: a hardlink entry is refused before the unpack, and the dir is removed" {
  _make_bundle
  local src="${BATS_TEST_TMPDIR}/src" outside="${BATS_TEST_TMPDIR}/outside" wd="${BATS_TEST_TMPDIR}/wd"
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  mkdir -m 700 "${outside}" "${wd}"
  printf 'outside-secret\n' > "${outside}/secret"
  cp -R "${BUNDLE}" "${src}"
  rm "${src}/sa.key"
  ln "${src}/ca.key" "${src}/sa.key"
  tar -czf "${tarball}" -C "${src}" .
  # The archive holds the hardlink entry under test.
  run tar -tvzf "${tarball}"
  assert_line --regexp '^h.* link to '
  _log_tar_unpacks
  _assert_refused_before_unpack "${tarball}" "${wd}" "${outside}"
}

# A symlink to a dir outside, then a file entry through it: a tar that
# follows links would write the file outside the bundle dir.
@test "resolve_bundle: a symlink with a file entry through it is refused before the unpack" {
  local s1="${BATS_TEST_TMPDIR}/s1" s2="${BATS_TEST_TMPDIR}/s2" outside="${BATS_TEST_TMPDIR}/outside" wd="${BATS_TEST_TMPDIR}/wd"
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar"
  mkdir -m 700 "${outside}" "${wd}"
  printf 'outside-secret\n' > "${outside}/secret"
  mkdir -p "${s1}" "${s2}/x"
  ln -s "${outside}" "${s1}/x"
  printf 'pwned\n' > "${s2}/x/payload"
  tar -cf "${tarball}" -C "${s1}" ./x
  tar -rf "${tarball}" -C "${s2}" ./x/payload
  gzip "${tarball}"
  run tar -tvzf "${tarball}.gz"
  assert_line --partial "./x -> ${outside}"
  assert_line --partial "./x/payload"
  _log_tar_unpacks
  _assert_refused_before_unpack "${tarball}.gz" "${wd}" "${outside}"
}

@test "resolve_bundle: a link that the listing does not show is found after the unpack" {
  # A tar whose -tv listing hides a link (a listing format the check does not
  # know): the check after the unpack reads the file system itself.
  _make_bundle
  local src="${BATS_TEST_TMPDIR}/src" tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz" wd="${BATS_TEST_TMPDIR}/wd"
  cp -R "${BUNDLE}" "${src}"
  rm "${src}/ca.key"
  ln -s ca.crt "${src}/ca.key"
  tar -czf "${tarball}" -C "${src}" .
  mkdir -m 700 "${wd}"
  tar() {
    case " $* " in
      *" -tvzf "*) command tar "$@" | sed -e 's/^l/-/' -e 's/ -> .*$//' ;;
      *) command tar "$@" ;;
    esac
  }
  export -f tar
  run handover::resolve_bundle "${tarball}" "${wd}"
  assert_failure
  assert_output --partial "handover: refusing ${tarball}: an archive entry is not a regular file or a directory (a link, a device or a FIFO)"
  [ ! -e "${wd}/bundle" ]
}

# A directory the user gives is read as it is. A key in it that is a link
# (to a file outside the bundle) is refused before anything reads or sends
# it: by preseed and by receive, for a required key and an optional one.
@test "handover receive and preseed: a bundle dir whose key is a link is refused before anything reads it" {
  _make_bundle
  _mock_node_binaries
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp
  local elsewhere="${BATS_TEST_TMPDIR}/elsewhere" key
  printf 'outside-secret-content\n' > "${elsewhere}"
  for key in ca.key encryption-provider; do
    rm -f "${BUNDLE}/${key}"
    ln -s "${elsewhere}" "${BUNDLE}/${key}"
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
    assert_failure
    assert_output --partial "handover: bundle key ${key} is not a regular file (a link, a directory or a special file): ${BUNDLE}/${key}"
    run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
    assert_failure
    assert_output --partial "handover: bundle key ${key} is not a regular file (a link, a directory or a special file): ${BUNDLE}/${key}"
    refute_output --partial "outside-secret-content"
    rm -f "${BUNDLE}/${key}"
    _make_bundle
  done
  [ ! -s "${CALLS}" ] || { cat "${CALLS}"; return 1; }
  [ ! -e "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

# A key that is a second name (a hard link) of a file outside the bundle
# passes the link check, and preseed would send that file. It is refused
# too, for a required key and an optional one.
@test "handover receive and preseed: a bundle dir whose key is a hard link is refused before anything reads it" {
  _make_bundle
  _mock_node_binaries
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp
  local elsewhere="${BATS_TEST_TMPDIR}/elsewhere" key
  printf 'outside-secret-content\n' > "${elsewhere}"
  for key in ca.key encryption-provider; do
    rm -f "${BUNDLE}/${key}"
    ln "${elsewhere}" "${BUNDLE}/${key}"
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
    assert_failure
    assert_output --partial "handover: bundle key ${key} has more than one name (a hard link), so it can be a file outside the bundle: ${BUNDLE}/${key}"
    run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
    assert_failure
    assert_output --partial "handover: bundle key ${key} has more than one name (a hard link), so it can be a file outside the bundle: ${BUNDLE}/${key}"
    refute_output --partial "outside-secret-content"
    rm -f "${BUNDLE}/${key}"
    _make_bundle
  done
  [ ! -s "${CALLS}" ] || { cat "${CALLS}"; return 1; }
  [ ! -e "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

# Each dir that the bundle's keys land in on this machine is set to 0700:
# the workdir and the bundle dir of a .tar.gz. A chmod that fails at one of
# them stops receive and preseed before anything runs, and TMPDIR is empty
# after.
@test "handover receive and preseed: a chmod that fails at the workdir or at the bundle dir stops before anything runs" {
  _make_bundle
  _mock_node_binaries
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz" cmd
  tar -czf "${tarball}" -C "${BUNDLE}" .
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp" FAIL_AT
  mkdir -m 700 "${TMPDIR}"
  # The workdir is the dir in TMPDIR (mktemp -d), the bundle dir is in it.
  chmod() {
    local p="${*: -1}"
    case "${FAIL_AT}" in
      workdir) [[ "${p%/*}" != "${TMPDIR}" ]] || return 1 ;;
      bundle) [[ "${p}" != "${TMPDIR}"/*/bundle ]] || return 1 ;;
    esac
    command chmod "$@"
  }
  export -f chmod
  for FAIL_AT in workdir bundle; do
    for cmd in receive preseed; do
      if [[ "${cmd}" == receive ]]; then
        run handover::receive --bundle "${tarball}" --snapshot "${SNAPSHOT}"
      else
        run handover::preseed --bundle "${tarball}" --node 203.0.113.7
      fi
      assert_failure
      assert_output --regexp "handover: cannot set ${TMPDIR}/[^ ]+ to mode 700, so the bundle's keys would not be private in it"
      run find "${TMPDIR}" -mindepth 1
      assert_output ""
      [ ! -s "${CALLS}" ] || { cat "${CALLS}"; return 1; }
      [ ! -e "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
    done
  done
}

# The fetched snapshot is 0600 before its first byte. A chmod that fails
# there stops the receive before the download and before the restore, and
# TMPDIR is empty.
@test "handover receive: a chmod that fails at the fetched snapshot stops before the download" {
  _make_bundle
  _mock_node_binaries
  aws() { echo "aws $*" >> "${CALLS}"; }
  curl() { echo "curl $*" >> "${CALLS}"; }
  export -f aws curl
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -m 700 "${TMPDIR}"
  _fail_chmod_at "${TMPDIR}/*/kubehz-handover-snapshot.db"
  local location
  for location in s3://kubehz-backups/handover/cl-001/snap.db https://backups.example.com/snap.db; do
    printf '%s' "${location}" > "${BUNDLE}/snapshot-location"
    run handover::receive --bundle "${BUNDLE}"
    assert_failure
    run find "${TMPDIR}" -mindepth 1
    assert_output ""
    [ ! -s "${CALLS}" ] || { cat "${CALLS}"; return 1; }
    [ ! -e "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
  done
}

@test "handover receive and preseed: a TMPDIR with an apostrophe and a space still loses the workdir" {
  # The RETURN trap quotes the workdir path (printf %q). Each run fails after
  # the bundle was unpacked into the workdir.
  _make_bundle
  ssh() { echo "ssh $*" >> "${CALLS}"; return 255; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  tar -czf "${tarball}" -C "${BUNDLE}" .
  export TMPDIR="${BATS_TEST_TMPDIR}/it's a tmp"
  mkdir -m 700 "${TMPDIR}"
  run handover::preseed --bundle "${tarball}" --node 203.0.113.7
  assert_failure
  assert_output --partial "cannot reach"
  run handover::receive --bundle "${tarball}" --snapshot "${BATS_TEST_TMPDIR}/no-such-snapshot"
  assert_failure
  assert_output --partial "--snapshot file not found"
  run find "${TMPDIR}" -mindepth 1
  assert_success
  assert_output ""
}

@test "handover receive and preseed: a workdir that is not 0700 fails before anything else" {
  _make_bundle
  _mock_node_binaries
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  # mktemp gives an open dir, and chmod cannot close it (a file system that
  # ignores file modes).
  mktemp() {
    local d="${BATS_TEST_TMPDIR}/open.${RANDOM}${RANDOM}"
    mkdir -m 755 "${d}" && echo "${d}"
  }
  chmod() { :; }
  export -f ssh scp mktemp chmod

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "has mode 755, not 700"
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_failure
  assert_output --partial "has mode 755, not 700"
  # Nothing reached the node, and the RETURN trap removed both dirs.
  [ ! -s "${CALLS}" ]
  run compgen -G "${BATS_TEST_TMPDIR}/open.*"
  assert_failure
}

# ── receive: the full kubeadm path against a sandbox ─────

@test "handover receive: restores, inits and verifies IN ORDER with the seeded PKI" {
  _make_bundle
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  # Verify ran and reported the matching CA hash (output is re-used below).
  assert_output --partial "handover: verified"
  assert_output --partial "handover: receive complete"

  # The verify probe talks to the LOCAL apiserver — admin.conf's endpoint DNS
  # still points at the platform until the operator cuts over.
  # BOTH args are load-bearing: --server alone blanks TLSServerName and the
  # cert (SANs = the endpoint DNS) would be checked against 127.0.0.1.
  run grep -F -- "--server https://127.0.0.1:6443 --tls-server-name cl-001.kubermatic.kkp.kubehz.in.net" "${CALLS}"
  assert_success

  # Call order: snapshot restore BEFORE kubeadm init BEFORE the verify probe.
  # The init carries the preflight ignore for the pre-restored etcd dir AND
  # skips the addon phases (restored DNS/proxy state must not be overwritten).
  run cat "${CALLS}"
  assert_success
  local restore_line init_line verify_line
  restore_line=$(grep -n '^etcdutl snapshot restore' "${CALLS}" | head -1 | cut -d: -f1)
  init_line=$(grep -n '^kubeadm init' "${CALLS}" | head -1 | cut -d: -f1)
  verify_line=$(grep -n '^kubectl --kubeconfig' "${CALLS}" | head -1 | cut -d: -f1)
  [ -n "${restore_line}" ]
  [ -n "${init_line}" ]
  [ -n "${verify_line}" ]
  [ "${restore_line}" -lt "${init_line}" ]
  [ "${init_line}" -lt "${verify_line}" ]

  # The restore targeted the sandboxed etcd dir with the LOCAL snapshot
  # (--snapshot short-circuited the s3:// fetch).
  run grep -F "etcdutl snapshot restore ${SNAPSHOT} --data-dir ${KUBEHZ_HANDOVER_ETCD_DIR}" "${CALLS}"
  assert_success

  # kubeadm init got the config AND the documented preflight ignore, and
  # --skip-token-print: its closing join line holds the bootstrap token.
  # The whole line, and one init: an argument added after the flag, or a
  # second init, fails here.
  run grep -c '^kubeadm init' "${CALLS}"
  assert_output 1
  run grep -F -x -- "kubeadm init --config ${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml --ignore-preflight-errors=DirAvailable--var-lib-etcd --skip-phases=addon/coredns,addon/kube-proxy --skip-token-print" "${CALLS}"
  assert_success

  # PKI was seeded byte-identical (kubeadm reuses it — the identity anchor).
  run cmp "${BUNDLE}/ca.crt" "${KUBEHZ_HANDOVER_K8S_DIR}/pki/ca.crt"
  assert_success
  run cmp "${BUNDLE}/sa.key" "${KUBEHZ_HANDOVER_K8S_DIR}/pki/sa.key"
  assert_success
  run cmp "${BUNDLE}/front-proxy-ca.key" "${KUBEHZ_HANDOVER_K8S_DIR}/pki/front-proxy-ca.key"
  assert_success
}

@test "handover receive: the generated kubeadm config carries endpoint DNS + encryption wiring" {
  _make_bundle
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success

  local cfg="${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml"
  [ -s "${cfg}" ]
  run cat "${cfg}"
  assert_output --partial "apiVersion: kubeadm.k8s.io/v1beta4"
  assert_output --partial 'controlPlaneEndpoint: "cl-001.kubermatic.kkp.kubehz.in.net:6443"'
  assert_output --partial '- "cl-001.kubermatic.kkp.kubehz.in.net"'
  assert_output --partial "name: encryption-provider-config"
  assert_output --partial "value: ${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  # The etcd image pins to the SOURCE line — kubeadm's default 3.6 panics
  # applying a 3.5-origin restore (tier2 run 17).
  assert_output --partial 'imageTag: "3.5.21-0"'

  # The EncryptionConfiguration embeds the bundle key, mode 600.
  local enc="${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  run cat "${enc}"
  assert_output --partial "kind: EncryptionConfiguration"
  # Provider + key name are the SOURCE cluster's (secretbox / kubehz-key-1,
  # kubehz-core etcd_encryption.go) — the stored-value prefix must match.
  assert_output --partial "secretbox:"
  assert_output --partial "name: kubehz-key-1"
  assert_output --partial "secret: dGVzdC1lbmNyeXB0aW9uLWtleQ=="
  run stat -c '%a' "${enc}"
  assert_output "600"
}

@test "handover receive: the kubeadm config gives the init bootstrap token a TTL of 1 h" {
  # kubeadm init creates its bootstrap token with a TTL of 24 h by default.
  # No later step of the handover uses the token, so it ends after 1 h.
  _make_bundle
  _mock_node_binaries
  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  local cfg="${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml"
  run cat "${cfg}"
  assert_output --partial "---
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
bootstrapTokens:
  - ttl: 1h0m0s"
  # One TTL only: no second token, and no 0 (a token that never expires).
  run grep -c 'ttl:' "${cfg}"
  assert_output "1"
}

@test "handover receive: the kubeadm config equals the golden that the Go suite reads" {
  # internal/kubehz/testdata/golden/kubeadm-config.yaml pins the Go render
  # (TestHandoverReceiveGeneratedConfigsMatchBashGoldens). The bash render
  # must be the same bytes, with /etc/kubernetes for the test dir.
  _make_bundle
  _mock_node_binaries
  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  local got="${BATS_TEST_TMPDIR}/kubeadm-config.got" cfg
  local want="${_PROJECT_ROOT}/internal/kubehz/testdata/golden/kubeadm-config.yaml"
  # "$(cat …; printf x)" keeps the trailing newlines that "$(< …)" strips,
  # so cmp sees every byte.
  cfg="$(cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml" && printf x)"
  cfg="${cfg%x}"
  printf '%s' "${cfg//"${KUBEHZ_HANDOVER_K8S_DIR}"//etc/kubernetes}" > "${got}"
  cmp -- "${want}" "${got}" || { diff -- "${want}" "${got}"; return 1; }
}

@test "handover receive: --single-node untaints via the LOCAL apiserver, not the pre-cutover DNS" {
  _make_bundle
  _mock_node_binaries

  run handover::receive --single-node --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  # The untaint must carry the same pinning as verify — admin.conf's endpoint
  # DNS still points at the platform (same CA), so an unpinned call could
  # untaint the WRONG cluster and report success.
  run grep -F -- "taint nodes --all" "${CALLS}"
  assert_success
  assert_output --partial -- "--server https://127.0.0.1:6443"
  assert_output --partial -- "--tls-server-name cl-001.kubermatic.kkp.kubehz.in.net"
}

@test "handover receive: a bundle-carried etcd-version reaches the kubeadm config" {
  _make_bundle
  _mock_node_binaries
  echo '3.5.99-0' > "${BUNDLE}/etcd-version"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  run cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml"
  assert_output --partial 'imageTag: "3.5.99-0"'
  refute_output --partial '3.5.21-0'
}

@test "handover receive: the env override beats a bundle-carried etcd-version" {
  _make_bundle
  _mock_node_binaries
  echo '3.5.99-0' > "${BUNDLE}/etcd-version"

  KUBEHZ_HANDOVER_ETCD_IMAGE_TAG='3.5.88-0' \
    run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  run cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-handover-kubeadm.yaml"
  assert_output --partial 'imageTag: "3.5.88-0"'
}

@test "handover receive: an out-of-charset BUNDLE etcd-version is refused before any node mutation" {
  _make_bundle
  _mock_node_binaries
  printf '3.5.21-0" evil\n' > "${BUNDLE}/etcd-version"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "not a plain image tag"
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

@test "handover receive: a bad bundle provider is refused BEFORE any node mutation" {
  _make_bundle
  _mock_node_binaries
  echo kms > "${BUNDLE}/encryption-provider"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "is not one of secretbox/aescbc/aesgcm"
  # Rejecting inside write_encryption_config would strand these.
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
  [ ! -d "${KUBEHZ_HANDOVER_ETCD_DIR}/member" ]
}

@test "handover receive: a bad bundle key name is refused BEFORE any node mutation" {
  _make_bundle
  _mock_node_binaries
  printf 'k1"\n            - name: pwn\n' > "${BUNDLE}/encryption-key-name"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "is not a plain name"
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
  [ ! -d "${KUBEHZ_HANDOVER_ETCD_DIR}/member" ]
}

@test "handover receive: an override diverging from the bundle etcd-version warns" {
  _make_bundle
  _mock_node_binaries
  echo '3.5.99-0' > "${BUNDLE}/etcd-version"

  KUBEHZ_HANDOVER_ETCD_IMAGE_TAG='3.5.88-0' \
    run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  assert_output --partial "overrides the bundle's etcd-version '3.5.99-0'"
}

@test "handover receive: a bundle-carried provider and key name reach the EncryptionConfiguration" {
  _make_bundle
  _mock_node_binaries
  echo aesgcm > "${BUNDLE}/encryption-provider"
  echo src-key-2 > "${BUNDLE}/encryption-key-name"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  run cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  assert_output --partial "- aesgcm:"
  assert_output --partial "name: src-key-2"
  refute_output --partial "secretbox"
  refute_output --partial "kubehz-key-1"
}

@test "handover receive: a non-base64 encryption-key is refused BEFORE any node mutation" {
  _make_bundle
  _mock_node_binaries
  printf 'k3y"\ninjected: yaml\n' > "${BUNDLE}/encryption-key"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "is not base64"
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
  [ ! -d "${KUBEHZ_HANDOVER_ETCD_DIR}/member" ]
}

@test "handover receive: a bad endpoint-dns is refused BEFORE any node mutation" {
  _make_bundle
  _mock_node_binaries
  printf 'evil.example.com"\n  extraArgs: pwn\n' > "${BUNDLE}/endpoint-dns"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "not a plain hostname"
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
  [ ! -d "${KUBEHZ_HANDOVER_ETCD_DIR}/member" ]
}

@test "handover: bundle_value keeps interior whitespace so the guards can reject it" {
  _make_bundle
  # Welding this into `aescbc` would make a malformed bundle render happily.
  printf 'aes cbc\n' > "${BUNDLE}/encryption-provider"
  run handover::bundle_value "${BUNDLE}" encryption-provider secretbox
  assert_success
  assert_output "aes cbc"

  run handover::encryption_metadata "${BUNDLE}"
  assert_failure
  assert_output --partial "is not one of secretbox/aescbc/aesgcm"
}

@test "handover: bundle_value returns a value that looks like an echo flag" {
  _make_bundle
  # `echo -n` prints nothing and would silently fall back to the default —
  # for etcd-version that means quietly reverting to the platform tag.
  printf -- '-n\n' > "${BUNDLE}/etcd-version"
  run handover::bundle_value "${BUNDLE}" etcd-version 3.5.21-0
  assert_success
  assert_output -- "-n"
}

@test "handover receive: an out-of-charset etcd image tag is refused BEFORE any node mutation" {
  _make_bundle
  _mock_node_binaries
  KUBEHZ_HANDOVER_ETCD_IMAGE_TAG='3.5.21-0" evil' \
    run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "not a plain image tag"
  # Fail-early contract: nothing was seeded.
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

@test "handover receive: a CA re-minted by kubeadm init FAILS the verify (identity lost)" {
  _make_bundle
  # kubeadm clobbers the seeded CA — exactly what the verify must catch.
  etcdutl() { echo "etcdutl $*" >> "${CALLS}"; }
  kubeadm() {
    echo "kubeadm $*" >> "${CALLS}"
    printf 'freshly-minted-DIFFERENT-ca\n' > "${KUBEHZ_HANDOVER_K8S_DIR}/pki/ca.crt"
  }
  kubectl() { return 0; }
  # Node identity stubs — WITHOUT these the member-identity derivation
  # depends on the HOST's iproute2 + default route: green on a dev box,
  # red on CI (no `ip`) with "cannot determine advertise address".
  hostname() { echo "Node-X"; }
  ip() { echo "1.1.1.1 via 10.9.8.1 dev eth0 src 10.9.8.7 uid 0"; }
  export -f etcdutl kubeadm kubectl hostname ip

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "CA hash mismatch"
  assert_output --partial "do NOT cut over"
}

@test "handover receive: refuses a non-empty /etc/kubernetes without --force" {
  _make_bundle
  _mock_node_binaries
  mkdir -p "${KUBEHZ_HANDOVER_K8S_DIR}"
  touch "${KUBEHZ_HANDOVER_K8S_DIR}/kubelet.conf"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "holds existing content"
  assert_output --partial "${KUBEHZ_HANDOVER_K8S_DIR}/kubelet.conf"
  assert_output --partial "--force"
  # Nothing ran against the node.
  [ ! -s "${CALLS}" ]
}

@test "handover receive: kubeadm's empty package-skeleton dirs do NOT block" {
  # Stock images (and `kubeadm reset`) leave empty manifests/ + pki/ behind —
  # the guard must only refuse REAL content, or its own documented remedy
  # (kubeadm reset) can never satisfy it.
  _make_bundle
  _mock_node_binaries
  mkdir -p "${KUBEHZ_HANDOVER_K8S_DIR}/manifests" "${KUBEHZ_HANDOVER_K8S_DIR}/pki"

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
}

@test "handover receive: dispatch-consumed --force (inherited field) flips the guard" {
  # The infra-gate made `force` an inherited argsh field: `lo … receive
  # --force` is consumed by the dispatch chain and arrives here only as the
  # dynamically-scoped variable — receive must honor it, not shadow it.
  _make_bundle
  _mock_node_binaries
  mkdir -p "${KUBEHZ_HANDOVER_K8S_DIR}"
  touch "${KUBEHZ_HANDOVER_K8S_DIR}/kubelet.conf"

  local force=1
  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
}

@test "handover receive: the restore skips the hash check (KKP snapshots are streamed, no trailer)" {
  _make_bundle
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  run grep -F "etcdutl snapshot restore ${SNAPSHOT}" "${CALLS}"
  assert_success
  assert_output --partial -- "--skip-hash-check=true"
  # The restore must rewrite member identity to what kubeadm's static pod
  # starts with — a "default" member crash-loops etcd on first boot. Exact
  # values from the mocked node: lowercased hostname + route src address.
  assert_output --partial -- "--name node-x"
  assert_output --partial -- "--initial-cluster node-x=https://10.9.8.7:2380"
  assert_output --partial -- "--initial-advertise-peer-urls https://10.9.8.7:2380"
}

@test "handover receive: an incomplete bundle stops BEFORE any node mutation" {
  _make_bundle
  rm "${BUNDLE}/front-proxy-ca.crt"
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "missing or empty key: front-proxy-ca.crt"
  [ ! -s "${CALLS}" ]
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

@test "handover receive: s3 snapshot-location without an aws CLI errors with the --snapshot escape" {
  _make_bundle
  _mock_node_binaries
  # No --snapshot and no aws binary in the sandbox PATH.
  aws() { return 127; }
  unset -f aws 2>/dev/null || true
  PATH="/usr/bin:/bin" run handover::receive --bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "s3://kubehz-backups/handover/cl-001/snap.db"
  assert_output --partial "--snapshot"
}

@test "handover receive: no derivable advertise address fails BEFORE any node mutation" {
  _make_bundle
  _mock_node_binaries
  # Both route lookups come back empty → identity resolution must fail
  # loudly, and place_pki must never run (no stranded PKI).
  ip() { return 1; }
  export -f ip

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "cannot determine this node's advertise address"
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

@test "handover receive: etcdctl is the fallback when etcdutl is absent" {
  _make_bundle
  # Only etcdctl exists (older node) — plus the rest of the happy path.
  etcdctl() { echo "etcdctl $*" >> "${CALLS}"; }
  kubeadm() { echo "kubeadm $*" >> "${CALLS}"; }
  kubectl() { echo "kubectl $*" >> "${CALLS}"; return 0; }
  hostname() { echo "Node-X"; }
  ip() { echo "1.1.1.1 via 10.9.8.1 dev eth0 src 10.9.8.7 uid 0"; }
  export -f etcdctl kubeadm kubectl hostname ip

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  run grep -F "etcdctl snapshot restore ${SNAPSHOT} --data-dir ${KUBEHZ_HANDOVER_ETCD_DIR}" "${CALLS}"
  assert_success
  # The fallback path must skip the hash check too (streamed KKP snapshots
  # carry no trailer) — pinned here so only the etcdutl branch keeping the
  # flag cannot pass this test. Same for the member-identity rewrite.
  assert_output --partial -- "--skip-hash-check=true"
  assert_output --partial -- "--initial-cluster node-x=https://10.9.8.7:2380"
}

@test "handover receive: a kkp:// snapshot-location is refused as KKP-internal (use --snapshot)" {
  _make_bundle
  # kubehz-core writes kkp://<destination>/<clusterID>/<backupName> — not a
  # fetchable URL; the refusal must say so and point at --snapshot.
  printf 'kkp://s3-eu-central/cl-001/snap-2026-07-29' > "${BUNDLE}/snapshot-location"
  _mock_node_binaries

  run handover::receive --bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "KKP-internal locator"
  assert_output --partial "--snapshot"
  # Refused BEFORE any node mutation.
  [ ! -s "${CALLS}" ]
  [ ! -d "${KUBEHZ_HANDOVER_K8S_DIR}/pki" ]
}

@test "handover receive: PKI placement + encryption-config write happen BEFORE kubeadm init" {
  _make_bundle
  _mock_node_binaries
  # Make place_pki and write_encryption_config observable in the shared
  # CALLS log: each file goes in with a rename (handover::install_file), and
  # a thin pass-through wrapper over the real mv logs it.
  mv() { echo "mv $*" >> "${CALLS}"; command mv "$@"; }
  export -f mv

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success

  local init_line
  init_line=$(grep -n '^kubeadm init' "${CALLS}" | head -1 | cut -d: -f1)
  [ -n "${init_line}" ]

  # Every one of the six PKI files is in place BEFORE kubeadm init runs —
  # kubeadm must find them to REUSE (not re-mint) the identity.
  local key pki_line
  for key in ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key; do
    pki_line=$(grep -n -F -- " ${KUBEHZ_HANDOVER_K8S_DIR}/pki/${key}" "${CALLS}" | grep -E '^[0-9]+:mv ' | head -1 | cut -d: -f1)
    [ -n "${pki_line}" ] || { echo "no rename recorded for ${key}" >&2; return 1; }
    [ "${pki_line}" -lt "${init_line}" ] || { echo "${key} placed AFTER kubeadm init" >&2; return 1; }
  done

  # …and so is the EncryptionConfiguration.
  local enc_line
  enc_line=$(grep -n -F -- " ${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml" "${CALLS}" | grep -E '^[0-9]+:mv ' | head -1 | cut -d: -f1)
  [ -n "${enc_line}" ]
  [ "${enc_line}" -lt "${init_line}" ]
}

@test "handover receive: a kubeadm init failure names the seeded state and the --force re-run" {
  _make_bundle
  etcdutl() { echo "etcdutl $*" >> "${CALLS}"; }
  kubeadm() { echo "kubeadm $*" >> "${CALLS}"; return 1; }
  kubectl() { return 0; }
  # Node identity stubs — see the CA re-mint test: never depend on host `ip`.
  hostname() { echo "Node-X"; }
  ip() { echo "1.1.1.1 via 10.9.8.1 dev eth0 src 10.9.8.7 uid 0"; }
  export -f etcdutl kubeadm kubectl hostname ip

  run handover::receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_failure
  assert_output --partial "state left behind"
  assert_output --partial "${KUBEHZ_HANDOVER_K8S_DIR}/pki"
  assert_output --partial "${KUBEHZ_HANDOVER_ETCD_DIR}"
  assert_output --partial "kubeadm reset"
  assert_output --partial "--force"
}

@test "handover: a fetched snapshot lands 0600 inside the private workdir" {
  _make_bundle
  printf 'https://backups.example.com/snap.db' > "${BUNDLE}/snapshot-location"
  curl() {
    local out=""
    while [ "$#" -gt 0 ]; do
      [ "$1" = "-o" ] && out="$2"
      shift
    done
    printf 'snapshot-bytes' > "${out}"
    return 0
  }
  export -f curl

  local workdir="${BATS_TEST_TMPDIR}/work"
  mkdir -p "${workdir}"
  run handover::fetch_snapshot "${BUNDLE}" "" "${workdir}"
  assert_success
  assert_output "${workdir}/kubehz-handover-snapshot.db"
  run stat -c '%a' "${workdir}/kubehz-handover-snapshot.db"
  assert_output "600"
}

@test "handover receive: scrubs the extracted bundle + fetched snapshot on failure" {
  _make_bundle
  printf 'https://backups.example.com/snap.db' > "${BUNDLE}/snapshot-location"
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  tar -czf "${tarball}" -C "${BUNDLE}" .

  etcdutl() { echo "etcdutl $*" >> "${CALLS}"; }
  kubeadm() { echo "kubeadm $*" >> "${CALLS}"; return 1; }   # fail LATE
  kubectl() { return 0; }
  curl() {
    local out=""
    while [ "$#" -gt 0 ]; do
      [ "$1" = "-o" ] && out="$2"
      shift
    done
    printf 'snapshot-bytes' > "${out}"
    return 0
  }
  # Node identity stubs — see the CA re-mint test: never depend on host `ip`.
  hostname() { echo "Node-X"; }
  ip() { echo "1.1.1.1 via 10.9.8.1 dev eth0 src 10.9.8.7 uid 0"; }
  export -f etcdutl kubeadm kubectl curl hostname ip

  # Confine mktemp so the workdir (extracted PKI + encryption key + downloaded
  # snapshot) is observable — after the failure it must be GONE.
  export TMPDIR="${BATS_TEST_TMPDIR}/scratch"
  mkdir -p "${TMPDIR}"

  run handover::receive --bundle "${tarball}"
  assert_failure
  assert_output --partial "kubeadm init FAILED"
  run find "${TMPDIR}" -mindepth 1
  assert_output ""
  # The user's own bundle archive is never touched.
  [ -f "${tarball}" ]
}

@test "handover receive: a successful receive leaves no scratch behind and keeps user files" {
  _make_bundle
  local tarball="${BATS_TEST_TMPDIR}/bundle.tar.gz"
  tar -czf "${tarball}" -C "${BUNDLE}" .
  _mock_node_binaries

  export TMPDIR="${BATS_TEST_TMPDIR}/scratch"
  mkdir -p "${TMPDIR}"

  run handover::receive --bundle "${tarball}" --snapshot "${SNAPSHOT}"
  assert_success
  assert_output --partial "handover: receive complete"
  run find "${TMPDIR}" -mindepth 1
  assert_output ""
  # The user-provided snapshot lives OUTSIDE the workdir and stays.
  [ -f "${SNAPSHOT}" ]
}

@test "handover: write_kubeadm_config rejects a non-hostname endpoint-dns (injection guard)" {
  _make_bundle
  printf 'evil.example.com"\n  extraArgs:\n    - name: pwn' > "${BUNDLE}/endpoint-dns"
  run handover::write_kubeadm_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}" "${BATS_TEST_TMPDIR}/out.yaml"
  assert_failure
  assert_output --partial "not a plain hostname"
  [ ! -s "${BATS_TEST_TMPDIR}/out.yaml" ]
}

@test "handover: write_encryption_config rejects a non-base64 key (injection guard)" {
  _make_bundle
  printf 'k3y"\ninjected: yaml' > "${BUNDLE}/encryption-key"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_failure
  assert_output --partial "not base64"
}

# ── bundle-carried platform metadata ─────────────────────
#
# provider / key name / etcd line come FROM the bundle so the target mirrors
# whatever the source actually encrypted and ran with, instead of hardcoding
# platform constants. Absent keys keep older bundles working.

@test "handover: write_encryption_config defaults to the platform's secretbox/kubehz-key-1" {
  _make_bundle
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_success
  run cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  assert_output --partial "- secretbox:"
  assert_output --partial "name: kubehz-key-1"
}

@test "handover: write_encryption_config honours a bundle-carried provider and key name" {
  _make_bundle
  echo aescbc > "${BUNDLE}/encryption-provider"
  echo source-key-7 > "${BUNDLE}/encryption-key-name"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_success
  run cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  assert_output --partial "- aescbc:"
  assert_output --partial "name: source-key-7"
  refute_output --partial "secretbox"
  refute_output --partial "kubehz-key-1"
}

@test "handover: write_encryption_config rejects an unknown provider" {
  _make_bundle
  echo kms > "${BUNDLE}/encryption-provider"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_failure
  assert_output --partial "not one of secretbox/aescbc/aesgcm"
}

@test "handover: write_encryption_config rejects a key name carrying YAML (injection guard)" {
  _make_bundle
  printf 'k1"\n            - name: pwn' > "${BUNDLE}/encryption-key-name"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_failure
  assert_output --partial "not a plain name"
}

@test "handover: a blank bundle value falls back to the default" {
  _make_bundle
  printf '\n' > "${BUNDLE}/encryption-provider"
  run handover::write_encryption_config "${BUNDLE}" "${KUBEHZ_HANDOVER_K8S_DIR}"
  assert_success
  run cat "${KUBEHZ_HANDOVER_K8S_DIR}/kubehz-encryption-config.yaml"
  assert_output --partial "- secretbox:"
}

@test "handover: bundle_value strips the trailing newline the operator writes" {
  _make_bundle
  printf '3.5.21-0\n' > "${BUNDLE}/etcd-version"
  run handover::bundle_value "${BUNDLE}" etcd-version fallback
  assert_success
  assert_output "3.5.21-0"
}

@test "handover: bundle_value falls back when the key is absent" {
  _make_bundle
  run handover::bundle_value "${BUNDLE}" nosuchkey the-default
  assert_success
  assert_output "the-default"
}

# ── preseed: the thin kubeone variant ────────────────────

@test "handover preseed: stages each file in the pki dir, then renames it over its destination in one call" {
  # 1 mkdir, 1 stage call (umask 077, the dir checks, mktemp), 6 scp to the staging
  # names and 1 install call. Nothing of the restore goes out.
  _make_bundle
  _fake_node
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_success
  assert_output --partial "PKI pre-seeded on 203.0.113.7"
  local -a lines
  mapfile -t lines < "${CALLS}"
  [ "${#lines[@]}" -eq 9 ] || { printf '%s\n' "${lines[@]}"; return 1; }
  [[ "${lines[0]}" == "ssh "*" umask 077 && mkdir -p /etc/kubernetes/pki && chmod 0700 /etc/kubernetes/pki" ]]
  local stage
  stage="$(_preseed_stage)"
  [[ "${lines[1]}" == "ssh "*" -p 22 root@203.0.113.7 ${stage}" ]] || { echo "stage: ${lines[1]}"; return 1; }
  local key i=2
  for key in ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key; do
    [[ "${lines[i]}" == "scp "*" -P 22 ${BUNDLE}/${key} root@203.0.113.7:/etc/kubernetes/pki/.${key}."?????????? ]] ||
      { echo "scp ${i} does not go to the staging file of ${key}: ${lines[i]}"; return 1; }
    i=$(( i + 1 ))
  done
  [[ "${lines[8]}" == "ssh "*" && echo 'mv ca.crt' && mv -f -T /etc/kubernetes/pki/.ca.crt."* ]] || { echo "install: ${lines[8]}"; return 1; }
  run grep -E '(encryption-key|snapshot-location)' "${CALLS}"
  assert_failure
  _assert_installed ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key
  run _staging_left
  assert_output ""
}

@test "handover preseed: a hard link or a symlink at a destination is replaced, and its target never gets the key bytes" {
  # scp writes into a file that is there already. The rename (mv -T)
  # replaces the name and never writes through it. A symlink to a dir is
  # replaced too, so no file goes into the dir.
  _make_bundle
  _fake_node
  local pki="${NODE_ROOT}/etc/kubernetes/pki" outside="${NODE_ROOT}/outside" f
  mkdir -p "${pki}" "${outside}/dir"
  printf 'old bytes\n' > "${outside}/hard"
  printf 'old bytes\n' > "${outside}/soft"
  ln "${outside}/hard" "${pki}/ca.key"
  ln -s "${outside}/soft" "${pki}/sa.key"
  ln -s "${outside}/dir" "${pki}/front-proxy-ca.key"
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_success
  for f in hard soft; do
    run cat "${outside}/${f}"
    assert_output "old bytes"
  done
  run find "${outside}/dir" -mindepth 1
  assert_output ""
  _assert_installed ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key
  run _staging_left
  assert_output ""
}

@test "handover preseed: each failure step stops, names the step and removes the staging files" {
  # The stage call, a staging name of another shape, a valid name of another
  # key, a name twice, a line with terminal controls, text before the check
  # lines, a wrong stage marker, an extra line, the copy, the chmod, the mv,
  # an install call that fails and names no step, and an install call whose
  # ssh connection fails (exit 255) before a step or after the install ran.
  # A name of another shape goes into no later call, and no raw ESC byte of
  # the node goes into the output.
  _make_bundle
  _fake_node
  # A line that a node can print where a staging name goes: terminal
  # controls, a backslash, a quote, a C1 control, a NUL and 250 letters. The
  # Go suite uses the same line (preseedHostileLine).
  local hostile
  IFS= read -r hostile <<'EOF'
printf '\033[2J\\"\302\233\000%s\n' "$(printf 'A%.0s' $(seq 1 250))"
EOF
  export HOSTILE="${hostile}"
  # That line as the message shows it: escaped, the NUL dropped as a command
  # substitution drops it, and cut at 200 bytes.
  local quoted
  quoted='"\x1b[2J\\\"\xc2\x9b'"$(printf 'A%.0s' $(seq 1 192))"'" (the first 200 of 258 bytes)'
  local named="handover: 203.0.113.7 printed a staging name for"
  local step want scps key
  local -a placed
  local dests="/etc/kubernetes/pki/ca.crt /etc/kubernetes/pki/ca.key /etc/kubernetes/pki/sa.pub /etc/kubernetes/pki/sa.key"
  dests+=" /etc/kubernetes/pki/front-proxy-ca.crt /etc/kubernetes/pki/front-proxy-ca.key"
  local lost="handover: the install call on 203.0.113.7 ended with exit 255 (an ssh error, such as a lost connection), so the state of the install is UNKNOWN:"
  for step in stage shape otherkey twice hostile motd marker extra copy chmod mv install install255 lost; do
    rm -rf "${NODE_ROOT}/etc/kubernetes/pki"
    : > "${CALLS}"
    export STEP="${step}"
    _node_hook() {
      local tool="${1}" cmd="${2}" pki="${NODE_ROOT}/etc/kubernetes/pki"
      local bad="echo /etc/kubernetes/pki/.ca.crt.AAAAAAAAA-" stage='umask 077 && echo "check '
      local crt="mktemp /etc/kubernetes/pki/.ca.crt.XXXXXXXXXX" key="mktemp /etc/kubernetes/pki/.ca.key.XXXXXXXXXX"
      case "${STEP}:${tool}" in
        stage:ssh) cmd="${cmd/"mktemp /etc/kubernetes/pki/.sa.key.XXXXXXXXXX"/false}" ;;
        shape:ssh) cmd="${cmd/"${crt}"/${bad}}" ;;
        otherkey:ssh) cmd="${cmd/"${crt}"/echo /etc/kubernetes/pki/.ca.key.Ab12Cd34Ef}" ;;
        twice:ssh)
          cmd="${cmd/"${crt}"/echo /etc/kubernetes/pki/.ca.crt.Ab12Cd34Ef}"
          cmd="${cmd/"${key}"/echo /etc/kubernetes/pki/.ca.crt.Ab12Cd34Ef}"
          ;;
        hostile:ssh) cmd="${cmd/"${crt}"/"${HOSTILE}"}" ;;
        motd:ssh) [[ "${cmd}" != "${stage}"* ]] || cmd="echo 'Welcome to the node' && ${cmd}" ;;
        marker:ssh) cmd="${cmd/" && echo stage && "/" && echo staged && "}" ;;
        extra:ssh) [[ "${cmd}" != "${stage}"* ]] || cmd+=" && echo extra" ;;
        copy:scp) [[ "${cmd}" != *"/.sa.key."* ]] || return 1 ;;
        chmod:ssh) [[ "${cmd}" != *" && mv -f -T "* ]] || rm -f "${pki}"/.ca.key.* ;;
        mv:ssh) [[ "${cmd}" != *" && mv -f -T "* ]] || mkdir -p "${pki}/sa.key/held" ;;
        install:ssh) [[ "${cmd}" != *" && mv -f -T "* ]] || return 1 ;;
        install255:ssh) [[ "${cmd}" != *" && mv -f -T "* ]] || return 255 ;;
        # The node runs the whole install, but the connection drops after
        # the third step line came back.
        lost:ssh) [[ "${cmd}" != *" && mv -f -T "* ]] || cmd="{ ${cmd}; } | sed -n 1,3p; exit 255" ;;
      esac
      printf '%s\n' "${cmd}"
    }
    export -f _node_hook
    placed=()
    case "${step}" in
      stage) want="handover: could not make the staging files /etc/kubernetes/pki/.<key>.XXXXXXXXXX on 203.0.113.7" scps=0 ;;
      shape) want="${named} ca.crt that is not /etc/kubernetes/pki/.ca.crt. and 10 letters or digits: \"/etc/kubernetes/pki/.ca.crt.AAAAAAAAA-\"" scps=0 ;;
      otherkey) want="${named} ca.crt that is not /etc/kubernetes/pki/.ca.crt. and 10 letters or digits: \"/etc/kubernetes/pki/.ca.key.Ab12Cd34Ef\"" scps=0 ;;
      twice) want="${named} ca.key that is not /etc/kubernetes/pki/.ca.key. and 10 letters or digits: \"/etc/kubernetes/pki/.ca.crt.Ab12Cd34Ef\"" scps=0 ;;
      hostile) want="${named} ca.crt that is not /etc/kubernetes/pki/.ca.crt. and 10 letters or digits: ${quoted}" scps=0 ;;
      motd) want="handover: 203.0.113.7 printed \"Welcome to the node\" where the stage call prints the check of /" scps=0 ;;
      marker) want="handover: 203.0.113.7 printed \"staged\" where the stage call prints \"stage\"" scps=0 ;;
      extra) want="handover: 203.0.113.7 printed 7 lines for 6 staging names" scps=0 ;;
      copy) want="handover: copying sa.key to 203.0.113.7 failed" scps=4 ;;
      chmod) want="handover: could not set the mode of the staging file of ca.key on 203.0.113.7; no file was moved into place" scps=6 ;;
      mv)
        want="handover: could not move the staging file of sa.key to /etc/kubernetes/pki/sa.key on 203.0.113.7; in place before it: ca.crt ca.key sa.pub" scps=6
        placed=(ca.crt ca.key sa.pub)
        ;;
      install)
        want="handover: the install call on 203.0.113.7 failed, and its output names no step, so the state of the install is UNKNOWN."
        want+=" Check each of these files on 203.0.113.7: ${dests}"
        scps=6
        ;;
      install255)
        want="${lost} no step arrived, but steps can have run. Check each of these files on 203.0.113.7: ${dests}"
        scps=6
        ;;
      lost)
        want="${lost} the last step that arrived is \"chmod sa.pub\", and later steps can have run. Check each of these files on 203.0.113.7: ${dests}"
        scps=6
        placed=(ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key)
        ;;
    esac
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
    assert_failure
    assert_output --partial "${want}"
    refute_output --partial "PKI pre-seeded"
    refute_output --partial "could not remove the staging files"
    refute_output --partial $'\e'
    run tail -n 1 "${CALLS}"
    assert_output --regexp '^ssh .* root@203\.0\.113\.7 rm -f -- /etc/kubernetes/pki/\.'
    run grep -c '^scp ' "${CALLS}"
    assert_output "${scps}"
    run grep -c -F 'AAAAAAAAA-' "${CALLS}"
    assert_output "0"
    run _staging_left
    assert_output ""
    _assert_installed "${placed[@]}"
    for key in "${KUBEHZ_HANDOVER_PKI_KEYS[@]:${#placed[@]}}"; do
      [[ ! -e "${NODE_ROOT}/etc/kubernetes/pki/${key}" || -d "${NODE_ROOT}/etc/kubernetes/pki/${key}" ]] ||
        { echo "${step}: ${key} went into place after the failure"; return 1; }
    done
  done
}

@test "handover preseed: an untrusted dir on the path of the pki dir is refused before any scp, and the message names what stat showed" {
  # The stage call refuses before its first mktemp, when a dir on the path
  # is not a dir (a link or a file), has a write bit for the group or others,
  # or has an owner that it may not have: root for / and /etc, root or the
  # ssh user for /etc/kubernetes and the pki dir. No staging file is made, so
  # no cleanup call runs.
  _make_bundle
  _fake_node
  local uid owner dir facts stage n who
  uid="$(id -u)"
  owner="${uid}"
  # As root, the dir goes to uid 4242 first, because root passes -uid 0.
  [[ "${uid}" != 0 ]] || owner=4242
  stage="$(_preseed_stage)"
  local pki="${NODE_ROOT}/etc/kubernetes/pki" outside="${NODE_ROOT}/outside"
  for n in groupwrite symlink file owner etcuser rootwrite; do
    rm -rf "${pki}" "${outside}"
    chmod 755 "${NODE_ROOT}/etc" "${NODE_ROOT}/etc/kubernetes"
    [[ "${uid}" != 0 ]] || chown "${uid}" "${NODE_ROOT}/etc" "${NODE_ROOT}/etc/kubernetes"
    : > "${CALLS}"
    export CASE="${n}" OTHER_UID="$(( owner + 1 ))"
    _node_hook() {
      local cmd="${2}" pki="${NODE_ROOT}/etc/kubernetes/pki"
      local idu='"$(id -u)"' other="\"${OTHER_UID}\""
      [[ "${cmd}" == 'umask 077 && echo "check '* ]] || { printf '%s\n' "${cmd}"; return 0; }
      case "${CASE}" in
        file) rm -rf "${pki}" && : > "${pki}" && chmod 600 "${pki}" ;;
        # The ssh user is another uid than the owner of the dir.
        owner) cmd="${cmd//"${idu}"/"${other}"}" ;;
        # The check of / reads /tmp, a dir of root that others can write.
        rootwrite) cmd="${cmd/"-- /)"/"-- /tmp)"}" && cmd="${cmd/"find / -maxdepth"/"find /tmp -maxdepth"}" ;;
        # The check of /etc reads NODE_ROOT/etc, a dir of the ssh user.
        etcuser)
          cmd="${cmd/"-- /etc)"/"-- ${NODE_ROOT}/etc)"}"
          cmd="${cmd/"find /etc -maxdepth"/"find ${NODE_ROOT}/etc -maxdepth"}"
          ;;
      esac
      printf '%s\n' "${cmd}"
    }
    export -f _node_hook
    case "${n}" in
      groupwrite)
        chmod 775 "${NODE_ROOT}/etc/kubernetes"
        dir=/etc/kubernetes facts="${uid} 775 directory"
        ;;
      symlink)
        mkdir -p "${outside}"
        ln -s "${outside}" "${pki}"
        dir=/etc/kubernetes/pki facts="${uid} 777 symbolic link"
        ;;
      file) dir=/etc/kubernetes/pki facts="${uid} 600 regular empty file" ;;
      owner)
        [[ "${uid}" != 0 ]] || chown 4242 "${NODE_ROOT}/etc/kubernetes"
        dir=/etc/kubernetes facts="${owner} 755 directory"
        ;;
      rootwrite)
        [ "$(stat -c '%u %a %F' /tmp)" = "0 1777 directory" ] || skip "this case reads /tmp as a dir of root with mode 1777, and /tmp is not"
        dir=/ facts="0 1777 directory"
        ;;
      etcuser)
        # The ssh user may own /etc/kubernetes, but not /etc.
        [[ "${uid}" != 0 ]] || chown 4242 "${NODE_ROOT}/etc"
        dir=/etc facts="${owner} 755 directory"
        ;;
    esac
    who="root or the ssh user"
    [[ "${dir}" != / && "${dir}" != /etc ]] || who=root
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
    assert_failure
    assert_output --partial "handover: ${dir} on 203.0.113.7 must be a dir, not a link, that ${who} owns, with no write bit for the group or others. stat shows (uid, mode, type) \"${facts}\". No key was sent"
    # The mkdir and the stage call only: no scp and no cleanup.
    local -a lines
    mapfile -t lines < "${CALLS}"
    [ "${#lines[@]}" -eq 2 ] || { echo "${n}:"; printf '%s\n' "${lines[@]}"; return 1; }
    [[ "${lines[1]}" == "ssh "*" ${stage}" ]] || { echo "${n}: stage: ${lines[1]}"; return 1; }
    local d
    for d in "${pki}" "${outside}"; do
      [[ ! -d "${d}" || -L "${d}" ]] || [ -z "$(find "${d}" -mindepth 1)" ] || { echo "${n}: a file was made in ${d}"; return 1; }
    done
  done
}

@test "handover preseed: a cleanup that fails names each staging file, and those are the files that are left" {
  _make_bundle
  _fake_node
  _node_hook() {
    [[ "${1}" != scp || "${2}" != *"/.sa.key."* ]] || return 1
    [[ "${2}" != "rm -f -- "* ]] || return 1
    printf '%s\n' "${2}"
  }
  export -f _node_hook
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_failure
  assert_output --partial "handover: copying sa.key to 203.0.113.7 failed"
  local prefix="handover: could not remove the staging files on 203.0.113.7; they can hold key bytes. Remove them yourself: "
  assert_output --partial "${prefix}"
  local line named
  line="$(grep -F "${prefix}" <<< "${output}")"
  named="$(tr ' ' '\n' <<< "${line#*"${prefix}"}" | LC_ALL=C sort)"
  run _staging_left
  [ "$(grep -c . <<< "${output}")" -eq 6 ]
  assert_output "${named}"
}

@test "handover preseed: --node is required" {
  _make_bundle
  run handover::preseed --bundle "${BUNDLE}"
  assert_failure
  assert_output --partial "node"
}

@test "handover preseed: an unreachable node fails before any scp" {
  _make_bundle
  # ssh ends its own messages with CR LF; the message stays readable.
  ssh() { echo "ssh $*" >> "${CALLS}"; printf 'Host key verification failed.\r\n' >&2; return 255; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp

  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_failure
  assert_output --partial "cannot reach"
  assert_line 'handover: the mkdir call on 203.0.113.7, stderr: "Host key verification failed."'
  assert_output --partial "handover: an unknown or changed host key also fails here. Compare the fingerprints of \"ssh-keyscan -p 22 203.0.113.7 | ssh-keygen -lf -\" with \"ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub\" on the node's console, and add the key to the known_hosts file if they match."
  assert_output --partial "Or retry with --accept-new-host-key to trust the first contact."
  run grep -c '^scp ' "${CALLS}"
  assert_output "0"

  # With the opt-in an unknown key cannot fail the first call; a changed one can.
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --accept-new-host-key
  assert_failure
  assert_output --partial "handover: a changed host key also fails here; check the node and the known_hosts entry before you retry"
  refute_output --partial "Or retry with --accept-new-host-key"
  run grep -c '^scp ' "${CALLS}"
  assert_output "0"
}

# Fail when an ssh or scp line lacks an option that keeps the user's ssh
# config from skipping the host key check (NoHostAuthenticationForLocalhost),
# from taking a key that is in no known_hosts file (VerifyHostKeyDNS), or from
# letting the node change that file (UpdateHostKeys).
_assert_no_config_bypass() {
  local opt
  for opt in NoHostAuthenticationForLocalhost=no VerifyHostKeyDNS=no UpdateHostKeys=no; do
    [[ "${1}" == *" -o ${opt} "* ]] || { echo "no -o ${opt}, so the ssh config can weaken the host key check: ${1}"; return 1; }
  done
}

@test "handover preseed: every ssh and scp checks the host key, never accept-new, no or /dev/null" {
  # The scp calls carry the CA private keys. An unknown or a changed key
  # fails before a key leaves this machine (StrictHostKeyChecking=yes).
  _make_bundle
  _fake_node
  ssh-keygen() { echo "ssh-keygen $*" >> "${CALLS}"; return 0; }
  export -f ssh-keygen
  local kh="${BATS_TEST_TMPDIR}/known_hosts" line n=0
  : > "${kh}"

  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts "${kh}"
  assert_success
  refute_output --partial "--accept-new-host-key: the first contact"
  # 1 mkdir, 1 stage, 6 scp and 1 install, and no record check: the loop
  # below sees every call.
  run grep -c . "${CALLS}"
  assert_output "9"
  while IFS= read -r line; do
    [[ "${line}" == "ssh -o StrictHostKeyChecking=yes "* || "${line}" == "scp -o StrictHostKeyChecking=yes "* ]] || { echo "not yes: ${line}"; return 1; }
    [[ "${line}" == *"-o UserKnownHostsFile=${kh} "* ]] || { echo "no known_hosts file: ${line}"; return 1; }
    # The given file is the only known_hosts file: not the global one too.
    [[ "${line}" == *" -o GlobalKnownHostsFile=/dev/null "* ]] || { echo "the global known_hosts file is a trust source: ${line}"; return 1; }
    [[ "${line}" != *accept-new* && "${line}" != *"StrictHostKeyChecking=no"* && "${line}" != *"UserKnownHostsFile=/dev/null"* ]] || { echo "host key not checked: ${line}"; return 1; }
    # A master connection of the user's ssh config never carries a call.
    [[ "${line}" == *" -o ControlMaster=no "* && "${line}" == *" -o ControlPath=none "* ]] || { echo "connection reuse not off: ${line}"; return 1; }
    # scp is quiet: no progress meter.
    [[ "${line}" != "scp "* || "${line}" == *" -q -P 22 "* ]] || { echo "scp without -q: ${line}"; return 1; }
    _assert_no_config_bypass "${line}"
    n=$(( n + 1 ))
  done < "${CALLS}"
  [ "${n}" -eq 9 ]
  # The first call makes the pki dir 0700 before any key goes out.
  run head -n 1 "${CALLS}"
  assert_output --regexp "^ssh .*'?umask 077 && mkdir -p /etc/kubernetes/pki && chmod 0700 /etc/kubernetes/pki'?\$"

  # Without --known-hosts, ssh uses its own known_hosts files.
  : > "${CALLS}"
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
  assert_success
  run grep -c -- '-o StrictHostKeyChecking=yes ' "${CALLS}"
  assert_output "9"
  run grep -c -e 'KnownHostsFile' -e 'accept-new' -e '^ssh-keygen ' "${CALLS}"
  assert_output "0"
}

@test "handover preseed: --accept-new-host-key trusts the first call only, then checks the record" {
  # The first call (the mkdir, no key on it) can record an unknown key. The
  # handover then asks ssh-keygen for that record, and every later call (the
  # scp calls with the keys) checks against it.
  _make_bundle
  _fake_node
  ssh-keygen() { echo "ssh-keygen $*" >> "${CALLS}"; return 0; }
  export -f ssh-keygen
  local kh="${BATS_TEST_TMPDIR}/known_hosts" line
  : > "${kh}"

  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts "${kh}" --accept-new-host-key
  assert_success
  [ "$(grep -c -F "handover: --accept-new-host-key: the first contact trusts the host key that 203.0.113.7 shows, without a check" <<<"${output}")" -eq 1 ]
  local -a lines
  mapfile -t lines < "${CALLS}"
  # 1 mkdir, 1 record check, 1 stage, 6 scp and 1 install.
  [ "${#lines[@]}" -eq 10 ]
  [[ "${lines[0]}" == "ssh -o StrictHostKeyChecking=accept-new "*"umask 077 && mkdir -p /etc/kubernetes/pki && chmod 0700 /etc/kubernetes/pki" ]]
  _assert_no_config_bypass "${lines[0]}"
  [ "${lines[1]}" = "ssh-keygen -F 203.0.113.7 -f ${kh}" ]
  for line in "${lines[@]:2}"; do
    [[ "${line}" == "ssh -o StrictHostKeyChecking=yes "* || "${line}" == "scp -o StrictHostKeyChecking=yes "* ]] || { echo "not yes: ${line}"; return 1; }
    [[ "${line}" != *accept-new* ]] || { echo "accept-new after the first call: ${line}"; return 1; }
    _assert_no_config_bypass "${line}"
  done

  # ssh's own file, a node on port 2222, a name with capitals: ssh records
  # it as [node]:port in lowercase.
  : > "${CALLS}"
  export HOME="${BATS_TEST_TMPDIR}/home"
  run handover::preseed --bundle "${BUNDLE}" --node Node.Example --port 2222 --accept-new-host-key
  assert_success
  run grep '^ssh-keygen ' "${CALLS}"
  assert_output "ssh-keygen -F [node.example]:2222 -f ${HOME}/.ssh/known_hosts"
}

@test "handover preseed: --accept-new-host-key with a key that ssh did not record fails closed before any scp" {
  # ssh could not write the file, for example: the later calls would have
  # no record to check the key against.
  _make_bundle
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  ssh-keygen() { echo "ssh-keygen $*" >> "${CALLS}"; return 1; }
  export -f ssh scp ssh-keygen
  local kh="${BATS_TEST_TMPDIR}/known_hosts"
  : > "${kh}"
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts "${kh}" --accept-new-host-key
  assert_failure
  assert_output --partial "handover: the host key of 203.0.113.7 is not in ${kh} after the first contact, so the next calls could not check it."
  run grep -c '^ssh ' "${CALLS}"
  assert_output "1"
  run grep -c '^scp ' "${CALLS}"
  assert_output "0"
}

@test "handover preseed: a --known-hosts that is missing, /dev/null, a dir, a FIFO or a device is refused before any ssh" {
  # Such a path holds no key to check against, and ssh cannot record one in it.
  _make_bundle
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp
  local fifo="${BATS_TEST_TMPDIR}/fifo" dir="${BATS_TEST_TMPDIR}/kh-dir" link="${BATS_TEST_TMPDIR}/kh-link" path
  mkfifo "${fifo}"
  mkdir "${dir}"
  ln -s /dev/null "${link}"
  for path in "${BATS_TEST_TMPDIR}/missing" /dev/null "${dir}" "${fifo}" /dev/zero "${link}"; do
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts "${path}" --accept-new-host-key
    assert_failure
    assert_output --partial "handover: --known-hosts must name an existing regular file that you can read: \"${path}\""
  done
  [ ! -s "${CALLS}" ]
}

# The start of the refusal of a --known-hosts path that ssh would expand or
# split; the quoted path follows it. The Go suite has the same text
# (preseedKnownHostsRefusal).
_known_hosts_refusal() {
  local msg
  IFS= read -r msg <<'EOF'
handover: --known-hosts path must not hold %, $, ~, ", ', \, whitespace or a control byte, because ssh expands %, $ and ~ in it and splits it at whitespace, quotes and backslashes:
EOF
  printf '%s ' "${msg}"
}

@test "handover preseed: a --known-hosts path with a byte that ssh expands or splits on is refused before any ssh" {
  # ssh expands % tokens, ${VAR} and ~ in an -o value and splits it at
  # whitespace, quotes and backslashes, so the file it reads would not be
  # the file the caller named. The file is there, so the refusal is not the
  # check of a regular file.
  _make_bundle
  ssh() { echo "ssh $*" >> "${CALLS}"; return 0; }
  scp() { echo "scp $*" >> "${CALLS}"; return 0; }
  export -f ssh scp
  local dir="${BATS_TEST_TMPDIR}/kh" refusal i
  refusal="$(_known_hosts_refusal)"
  mkdir -p "${dir}"
  local -a names=('kh%h' 'kh$x' 'kh~x' 'kh"x' "kh'x" 'kh\x' 'kh x' $'kh\tx' $'kh\ex' $'kh\x7fx')
  local -a shown=('kh%h' 'kh$x' 'kh~x' 'kh\"x' "kh'x" 'kh\\x' 'kh x' 'kh\x09x' 'kh\x1bx' 'kh\x7fx')
  for i in "${!names[@]}"; do
    : > "${dir}/${names[i]}"
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts "${dir}/${names[i]}"
    assert_failure
    assert_output --partial "${refusal}\"${dir}/${shown[i]}\""
    refute_output --partial $'\e'
  done
  [ ! -s "${CALLS}" ]
}

@test "handover preseed: a relative --known-hosts path is made absolute from the working dir before the check" {
  # The ssh calls get the absolute path, and a working dir whose name holds
  # a $ is refused.
  _make_bundle
  _fake_node
  local work="${BATS_TEST_TMPDIR}/work" bad="${BATS_TEST_TMPDIR}/a\$b" d
  for d in "${work}" "${bad}"; do
    mkdir -p "${d}"
    : > "${d}/known_hosts"
  done
  cd "${work}"
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts known_hosts
  assert_success
  run grep -c . "${CALLS}"
  assert_output "9"
  run grep -c -F -e "-o UserKnownHostsFile=${work}/known_hosts " "${CALLS}"
  assert_output "9"
  : > "${CALLS}"
  cd "${bad}"
  run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7 --known-hosts known_hosts
  assert_failure
  assert_output --partial "$(_known_hosts_refusal)\"${bad}/known_hosts\""
  [ ! -s "${CALLS}" ]
}

@test "handover preseed: ssh reads the --known-hosts path as it is, and no global known_hosts file" {
  # A real ssh prints the config that it would use (-G) and exits: it opens
  # no connection, and -F /dev/null reads no config file. It gets the
  # options of the first call of the handover.
  command -v ssh > /dev/null || skip "no ssh on PATH: the check of what ssh reads from the options needs one"
  _make_bundle
  local kh="${BATS_TEST_TMPDIR}/known_hosts"
  : > "${kh}"
  export SSH_G_HOME="${BATS_TEST_TMPDIR}/home" SSH_G_OUT="${BATS_TEST_TMPDIR}/ssh-G.out"
  mkdir -p "${SSH_G_HOME}"
  ssh() { HOME="${SSH_G_HOME}" command ssh -F /dev/null -G "$@" > "${SSH_G_OUT}" || return 2; return 1; }
  export -f ssh
  run handover::preseed --bundle "${BUNDLE}" --node node.invalid --known-hosts "${kh}"
  assert_failure
  run grep -x -F -e "userknownhostsfile ${kh}" "${SSH_G_OUT}"
  assert_success
  run grep -x -F -e "globalknownhostsfile /dev/null" "${SSH_G_OUT}"
  assert_success
}

# A remote command that prints "<ESC>[2J<what>" on stdout and
# "<ESC>[2J<what> err" on stderr: a terminal control on each stream.
_control_line() {
  printf '%s\n' "printf '\\033[2J${1}\\n' && printf '\\033[2J${1} err\\n' >&2"
}

@test "handover preseed: the output of the node reaches the terminal only quoted, and on success only the stderr of the first call" {
  # Each ssh and scp call captures both of its streams. On a failure,
  # preseed shows each line of the call that failed, and of a cleanup call
  # that fails, quoted.
  _make_bundle
  _fake_node
  export MKDIR_CMD='umask 077 && mkdir -p /etc/kubernetes/pki && chmod 0700 /etc/kubernetes/pki'
  local c
  for c in success mkdir copy; do
    rm -rf "${NODE_ROOT}/etc/kubernetes/pki"
    : > "${CALLS}"
    export CASE="${c}"
    _node_hook() {
      local tool="${1}" cmd="${2}"
      case "${CASE}:${tool}" in
        success:scp)
          printf '\033[2Jlater\n' >&3
          printf '\033[2Jlater err\n' >&2
          ;;
        success:ssh)
          if [[ "${cmd}" == "${MKDIR_CMD}" ]]; then
            cmd="$(_control_line mkdir) && ${cmd}"
          elif [[ "${cmd}" == 'umask 077 && echo "check '* ]]; then
            cmd="printf '\\033[2Jlater err\\n' >&2 && ${cmd}"
          else
            cmd="$(_control_line later) && ${cmd}"
          fi
          ;;
        mkdir:ssh) [[ "${cmd}" != "${MKDIR_CMD}" ]] || cmd="$(_control_line mkdir) && exit 255" ;;
        copy:scp)
          if [[ "${cmd}" == *"/.sa.key."* ]]; then
            printf '\033[2Jscp\n' >&3
            printf '\033[2Jscp err\n' >&2
            return 1
          fi
          ;;
        copy:ssh) [[ "${cmd}" != "rm -f -- "* ]] || cmd="$(_control_line rm) && exit 1" ;;
      esac
      printf '%s\n' "${cmd}"
    }
    export -f _node_hook _control_line
    run handover::preseed --bundle "${BUNDLE}" --node 203.0.113.7
    case "${c}" in
      success)
        assert_success
        assert_line 'handover: the mkdir call on 203.0.113.7, stderr: "\x1b[2Jmkdir err"'
        refute_output --partial "stdout:"
        refute_output --partial "later"
        ;;
      mkdir)
        assert_failure
        assert_line 'handover: the mkdir call on 203.0.113.7, stdout: "\x1b[2Jmkdir"'
        assert_line 'handover: the mkdir call on 203.0.113.7, stderr: "\x1b[2Jmkdir err"'
        ;;
      copy)
        assert_failure
        assert_line 'handover: the copy of sa.key to 203.0.113.7, stdout: "\x1b[2Jscp"'
        assert_line 'handover: the copy of sa.key to 203.0.113.7, stderr: "\x1b[2Jscp err"'
        assert_line 'handover: the cleanup call on 203.0.113.7, stdout: "\x1b[2Jrm"'
        assert_line 'handover: the cleanup call on 203.0.113.7, stderr: "\x1b[2Jrm err"'
        ;;
    esac
    refute_output --partial $'\e'
  done
}

@test "handover: quote shows 200 bytes, and stream shows 20 lines" {
  # A line of exactly 200 bytes gets no suffix, a line of 201 does. The 20th
  # line of a stream says how many lines it does not show. One CR at the end
  # of a line is dropped.
  local a200 a201
  a200="$(printf 'A%.0s' $(seq 1 200))"
  a201="${a200}A"
  run handover::quote "${a200}"
  assert_output "\"${a200}\""
  run handover::quote "${a201}"
  assert_output "\"${a200}\" (the first 200 of 201 bytes)"
  local n last
  for n in 20 21 25; do
    case "${n}" in
      20) last='handover: c, stdout: "20"' ;;
      21) last='handover: c, stdout: "20" (1 more line)' ;;
      25) last='handover: c, stdout: "20" (5 more lines)' ;;
    esac
    run handover::stream c stdout "$(seq 1 "${n}" | sed 's/$/\r/')"
    [ "${#lines[@]}" -eq 20 ] || { echo "${n}: ${#lines[@]} lines"; return 1; }
    [ "${lines[0]}" = 'handover: c, stdout: "1"' ] || { echo "${n}: ${lines[0]}"; return 1; }
    [ "${lines[19]}" = "${last}" ] || { echo "${n}: ${lines[19]}"; return 1; }
  done
  run handover::streams c "" ""
  assert_output ""
}

# ── `lo kubehz assess` pretty-printer ────────────────────
# kubehz::render_assessment lives in libs/kubehz/main (it shares the status
# command's auth/config plumbing); the fetch itself is plain curl — the
# printer is the testable surface.

@test "assess: render_assessment prints aligned probes + feasibility" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  local response='{"ok":true,"data":{"assessedAt":"2026-07-29T09:00:00Z","assessment":{"collectedAt":"2026-07-29T08:58:00Z","k8sVersion":"v1.33.2","datastore":"etcd","etcdReachable":true,"capiManaged":true,"cni":"cilium","podCidr":"10.244.0.0/16","serviceCidr":"10.96.0.0/12","storageClasses":[{"name":"hcloud-volumes","provisioner":"csi.hetzner.cloud","isDefault":true}],"pvSummary":{"count":2,"totalGi":40,"byProvisioner":{"csi.hetzner.cloud":{"count":2,"totalGi":40}}},"loadBalancers":1,"webhooks":{"validating":3,"mutating":1},"cpUsage":{"nodes":5,"cpNodes":3,"etcdDbBytes":null}},"feasibility":{"path":"restore","reasons":["datastore=etcd and a snapshot is obtainable"],"warnings":["1 provider-coupled StorageClass (csi.hetzner.cloud)"]}}}'

  run kubehz::render_assessment "test.kubehz.dev" "${response}"
  assert_success
  assert_output --partial "kubehz assessment — test.kubehz.dev"
  assert_output --partial "collected 2026-07-29T08:58:00Z"
  assert_output --partial "v1.33.2"
  assert_output --partial "etcd · reachable"
  assert_output --partial "pause the CAPI controllers"
  assert_output --partial "cilium"
  assert_output --partial "pods 10.244.0.0/16 · services 10.96.0.0/12"
  assert_output --partial "1 classes · 2 PVs · 40Gi"
  assert_output --partial "csi.hetzner.cloud — 2 PVs · 40Gi"
  assert_output --partial "3 validating · 1 mutating"
  assert_output --partial "5 total · 3 control-plane"
  assert_output --partial "Feasibility: restore"
  assert_output --partial "datastore=etcd and a snapshot is obtainable"
  assert_output --partial "provider-coupled StorageClass"
}

@test "assess: render_assessment says so plainly when no assessment exists yet" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::render_assessment "test.kubehz.dev" '{"data":{"assessment":null,"assessedAt":null,"feasibility":null}}'
  assert_success
  assert_output --partial "No assessment recorded for test.kubehz.dev yet"
  assert_output --partial "24h"
}

# ── dispatch: `lo kubehz handover <cmd>` resolves ────────

@test "handover: kubehz::handover dispatches receive via the argsh usage table" {
  _make_bundle
  _mock_node_binaries

  run kubehz::handover receive --bundle "${BUNDLE}" --snapshot "${SNAPSHOT}"
  assert_success
  assert_output --partial "handover: receive complete"
}

@test "resolve_bundle: rejects a path-traversal archive before extraction" {
  local evil="${BATS_TEST_TMPDIR}/evil.tar.gz" marker="${BATS_TEST_TMPDIR}/escaped"
  mkdir -p "${BATS_TEST_TMPDIR}/payload"
  echo pwned > "${BATS_TEST_TMPDIR}/payload/escaped"
  # ../-prefixed entry: extraction into <dir> would land NEXT TO it.
  tar -czf "${evil}" -C "${BATS_TEST_TMPDIR}/payload" --transform 's|^|../|' escaped 2>/dev/null \
    || tar -czf "${evil}" -C "${BATS_TEST_TMPDIR}/payload" -s '|^|../|' escaped
  run handover::resolve_bundle "${evil}"
  assert_failure
  assert_output --partial "escapes the bundle dir"
  [ ! -e "${marker}" ]
}

@test "resolve_bundle: traversal-guard truth table — every ..-component position rejected, literal dots pass" {
  # Drive the guard's case statement directly: the tar stub answers -tzf
  # with the single entry under test (bare `..` can't even be crafted with
  # a real tar --transform, hence the stub), -xzf is a no-op.
  local probe="${BATS_TEST_TMPDIR}/probe.tar.gz"
  : > "${probe}"
  # Confine resolve_bundle's mktemp workdirs — the pass-through iterations
  # would otherwise litter the real /tmp (never cleaned on the later
  # validation failure).
  export TMPDIR="${BATS_TEST_TMPDIR}"
  tar() {
    if [[ "$*" == *-tzf* ]]; then printf '%s\n' "${TAR_ENTRY}"; else return 0; fi
  }
  export -f tar

  # Genuine traversals: a `..` COMPONENT in any position, or an absolute path.
  local entry
  for entry in '/abs/path' '..' '../x' 'a/../b' 'a/..' './../x' './..' 'x/../'; do
    TAR_ENTRY="${entry}" run handover::resolve_bundle "${probe}"
    assert_failure
    assert_output --partial "escapes the bundle dir"
  done

  # Literal dot-names are NOT traversals — the old `*../*` pattern rejected
  # these (false positives); the component-anchored set must let them PAST
  # the guard, and with the guard passed resolve_bundle completes (echoes
  # the extraction dir) — assert_success pins that, not just the absence of
  # the refusal message.
  for entry in 'foo../bar' 'a/..foo' 'x..' 'ok/path'; do
    TAR_ENTRY="${entry}" run handover::resolve_bundle "${probe}"
    assert_success
    refute_output --partial "escapes the bundle dir"
  done
}

# The real curl downloads the snapshot from a local https server
# (tests/lib/http_stub.bash), with a config file of the user in each place
# curl looks for one: CURL_HOME, XDG_CONFIG_HOME, HOME. With -q first curl
# reads none of them: the snapshot lands in the private file and nowhere
# else, and no trace of the URL or the snapshot is written. The control in
# each place, as in the api test: curl without -q reads the file there
# (none for XDG_CONFIG_HOME on curl older than 7.73, with a note).
@test "handover: the snapshot download reads no config file of the user" {
  command -v python3 >/dev/null || skip "no python3"
  [[ -n "$(command -v openssl)" ]] || skip "no openssl: the https stub needs a self-signed certificate"
  [[ -n "$(type -P curl)" ]] || skip "no curl"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"
  _make_bundle
  local var workdir got o e rc controls=0
  stub_cert
  export STUB_SNAPSHOT="${BATS_TEST_TMPDIR}/snap.src"
  printf 'snap-%s' "$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')" > "${STUB_SNAPSHOT}"
  http_stub_start "${STUB_CERT}" "${STUB_KEY}"
  export CURL_CA_BUNDLE="${STUB_CERT}"
  printf 'https://127.0.0.1:%s/snap.db' "${STUB_PORT}" > "${BUNDLE}/snapshot-location"
  for var in CURL_HOME XDG_CONFIG_HOME HOME; do
    echo "# config file in ${var}"
    curlrc_dir "${var}"
    workdir="${RC_DIR}.work"
    mkdir -m 700 "${workdir}"
    o="${RC_DIR}.stdout" e="${RC_DIR}.stderr" rc=0
    # The checks of the config file come first, as in the api test.
    ( curlrc_env "${var}" && handover::fetch_snapshot "${BUNDLE}" "" "${workdir}" ) > "${o}" 2> "${e}" || rc=$?
    [ ! -e "${RC_TRACE}" ]
    [ ! -e "${RC_OUT}" ]
    [ "${rc}" -eq 0 ]
    [ ! -s "${e}" ]
    got=$(<"${o}")
    [ "${got}" = "${workdir}/kubehz-handover-snapshot.db" ]
    cmp -- "${STUB_SNAPSHOT}" "${got}"
    [ "$(stat -c %a "${got}")" = 600 ]
    if [[ "${var}" == XDG_CONFIG_HOME ]] && ! curl_reads_xdg; then
      echo "# no control for XDG_CONFIG_HOME: this curl is older than 7.73 and does not look there" >&3
    else
      curlrc_control "${var}" "https://127.0.0.1:${STUB_PORT}/control"
      controls=$((controls + 1))
    fi
  done
  [ "$(grep -c '^GET /snap.db none$' "${STUB_LOG}")" -eq 3 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq "${controls}" ]
  [ "$(wc -l < "${STUB_LOG}")" -eq $((3 + controls)) ]
  http_stub_stop
}

# The real curl downloads the snapshot over https with SSLKEYLOGFILE set in
# the environment: it must not append the TLS session keys to that file
# (the keys decrypt a captured download: the URL with its signature, and
# the snapshot). The control first, as in the api test: this curl writes a
# key log for a request to the same server when the variable reaches it,
# else the test skips; a control request that does not get to the stub
# fails the test. A readonly SSLKEYLOGFILE stops the download before curl
# sends a request, with the line that names it.
@test "handover: the snapshot download writes no TLS key log" {
  command -v python3 >/dev/null || skip "no python3"
  [[ -n "$(command -v openssl)" ]] || skip "no openssl: the https stub needs a self-signed certificate"
  [[ -n "$(type -P curl)" ]] || skip "no curl"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"
  _make_bundle
  local workdir="${BATS_TEST_TMPDIR}/kl.work" o="${BATS_TEST_TMPDIR}/kl.stdout" e="${BATS_TEST_TMPDIR}/kl.stderr" got rc=0
  stub_cert
  export STUB_SNAPSHOT="${BATS_TEST_TMPDIR}/snap.src"
  printf 'snap-%s' "$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')" > "${STUB_SNAPSHOT}"
  http_stub_start "${STUB_CERT}" "${STUB_KEY}"
  export CURL_CA_BUNDLE="${STUB_CERT}"
  local kc=0
  keylog_control || kc=$?
  # 2: the control request did not get to the stub. That is a fault, not a skip.
  [ "${kc}" -ne 2 ]
  [ "${kc}" -eq 0 ] || skip "this curl writes no TLS key log (its TLS backend ignores SSLKEYLOGFILE), so the test cannot fail"
  printf 'https://127.0.0.1:%s/snap.db' "${STUB_PORT}" > "${BUNDLE}/snapshot-location"
  mkdir -m 700 "${workdir}" "${workdir}.ro"
  export SSLKEYLOGFILE="${BATS_TEST_TMPDIR}/tls.keys"
  ( handover::fetch_snapshot "${BUNDLE}" "" "${workdir}" ) > "${o}" 2> "${e}" || rc=$?
  [ ! -e "${SSLKEYLOGFILE}" ]
  [ "${rc}" -eq 0 ]
  [ ! -s "${e}" ]
  got=$(<"${o}")
  [ "${got}" = "${workdir}/kubehz-handover-snapshot.db" ]
  cmp -- "${STUB_SNAPSHOT}" "${got}"
  rc=0
  ( readonly SSLKEYLOGFILE && handover::fetch_snapshot "${BUNDLE}" "" "${workdir}.ro" ) > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -ne 0 ]
  grep -qF 'SSLKEYLOGFILE is readonly, so lo cannot clear it for curl: the request does not run' "${e}"
  [ ! -e "${SSLKEYLOGFILE}" ]
  [ "$(grep -c '^GET /snap.db none$' "${STUB_LOG}")" -eq 1 ]
  [ "$(grep -c '^GET /control none$' "${STUB_LOG}")" -eq 1 ]
  [ "$(wc -l < "${STUB_LOG}")" -eq 2 ]
  http_stub_stop
}

# http::tls_log_vars_unset at the snapshot download, with a curl stand-in
# that records which names of the TLS list (_HTTP_TLS_LOG_VARS of
# utils/http.sh) it gets (tls_spy_record: names, never a value): curl gets
# none of them, the download works, and the caller keeps each one. The
# record must hold the control, else an empty record would pass. Then each
# name is readonly in turn: one line names it, the stand-in does not run,
# the snapshot file stays empty, and the download fails with its error.
# This test needs no GnuTLS or NSS curl, so it never skips.
@test "handover: curl gets no TLS log or trace variable, and a readonly one stops the download" {
  _make_bundle
  printf 'https://backups.example.com/snap.db' > "${BUNDLE}/snapshot-location"
  source "${_PROJECT_ROOT}/tests/lib/http_stub.bash"
  local name rc workdir="${BATS_TEST_TMPDIR}/tl.work" o="${BATS_TEST_TMPDIR}/tl.stdout" e="${BATS_TEST_TMPDIR}/tl.stderr"
  local -a names=("${_HTTP_TLS_LOG_VARS[@]}")
  (( ${#names[@]} >= 6 ))
  mkdir -m 700 "${workdir}" "${workdir}.ro"
  export CURL_ENV_LOG="${BATS_TEST_TMPDIR}/curl.env" TLS_SPY_NAMES="${names[*]}"
  curl() {
    local out=""
    tls_spy_record "${CURL_ENV_LOG}"
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "-o" ]; then out="$2"; fi
      shift
    done
    printf 'snapshot-bytes' > "${out}"
  }
  export -f curl tls_spy_record
  for name in "${names[@]}"; do
    export "${name}=${BATS_TEST_TMPDIR}/tls.${name}"
  done
  export TLS_ENV_CONTROL=on
  handover::fetch_snapshot "${BUNDLE}" "" "${workdir}" > "${o}" 2> "${e}"
  [ "$(cat "${o}")" = "${workdir}/kubehz-handover-snapshot.db" ]
  [ "$(cat "${workdir}/kubehz-handover-snapshot.db")" = snapshot-bytes ]
  [ ! -s "${e}" ]
  # The record: the marker and the control, no name of the list.
  [ "$(cat "${CURL_ENV_LOG}")" = $'record\ncontrol' ]
  for name in "${names[@]}"; do
    # The caller keeps each one, with its value, as an export.
    [[ "$(declare -p "${name}")" == "declare -x ${name}="* ]]
    [ "${!name}" = "${BATS_TEST_TMPDIR}/tls.${name}" ]
  done
  for name in "${names[@]}"; do
    rm -f -- "${CURL_ENV_LOG}"
    rc=0
    ( readonly "${name}" || exit 9
      handover::fetch_snapshot "${BUNDLE}" "" "${workdir}.ro" ) > "${o}" 2> "${e}" || rc=$?
    [ "${rc}" -eq 1 ]
    grep -qF "${name} is readonly, so lo cannot clear it for curl: the request does not run" "${e}"
    [ "$(grep -c 'is readonly' "${e}")" -eq 1 ]
    grep -qF 'handover: downloading the snapshot from https://backups.example.com/snap.db failed' "${e}"
    [ ! -e "${CURL_ENV_LOG}" ]
    # The download makes the file (0600, empty) before curl runs: no bytes got in.
    [ ! -s "${workdir}.ro/kubehz-handover-snapshot.db" ]
  done
}

# A presigned https URL carries its signature in the query. When the
# download fails, the error names the URL without the query (as the Go
# twin's stripQuery): the path is there, no part of the query is. The
# signature and the credential are built at runtime.
@test "handover: a failed download names the URL without its query" {
  _make_bundle
  local sig cred workdir="${BATS_TEST_TMPDIR}/q.work" o="${BATS_TEST_TMPDIR}/q.stdout" e="${BATS_TEST_TMPDIR}/q.stderr" rc=0
  sig="$(printf '%s' 5ign4ture)deadbeef"
  cred="$(printf '%s' EXAMPLEKEY)%2F20261118"
  printf 'https://backups.example.com/handover/snap.db?X-Amz-Credential=%s&X-Amz-Signature=%s' "${cred}" "${sig}" \
    > "${BUNDLE}/snapshot-location"
  mkdir -m 700 "${workdir}"
  curl() { echo "curl: (22) The requested URL returned error: 403" >&2; return 22; }
  export -f curl
  handover::fetch_snapshot "${BUNDLE}" "" "${workdir}" > "${o}" 2> "${e}" || rc=$?
  [ "${rc}" -eq 1 ]
  grep -qF 'handover: downloading the snapshot from https://backups.example.com/handover/snap.db failed' "${e}"
  [ "$(grep -c -F -e '?' -e 'X-Amz' -e "${sig}" -e "${cred}" "${e}" "${o}" | awk -F: '{s += $NF} END {print s}')" -eq 0 ]
}
