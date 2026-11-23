#!/usr/bin/env bats
# lo_expose_test.bats: the `lo expose` proxy. lo::expose
# (.lok8s/drivers/lo/utils/expose.sh) starts the proxy container from
# LO_EXPOSE_IMAGE (.lok8s/drivers/lo/utils/defaults.sh), which is pinned by
# tag and digest, copies the nginx config and the TLS files into it, and
# reloads nginx. A failed docker step fails the command. Go twins: the
# TestExpose* and TestRemoteCIFailedExposeFailsTheRun tests in
# internal/driver/lo/{services,remote}_test.go.
#
# No test may reach a real tool. Setup puts failing stubs of the tools first
# on PATH (stub_tools in tests/test_helper.bash), and teardown fails the test
# when one of them ran. docker, yq and envsubst are shell functions in the
# test: a function comes before a program on PATH.

setup() {
  load "../test_helper"
  setup_tmpdir
  stub_tools docker kind kubectl helm ssh scp rsync curl yq envsubst
  import() { :; }
  export -f import
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
}

# Every test ends with the check that no tool on PATH ran.
teardown() {
  local rc=0
  assert_no_tool_ran || rc=$?
  teardown_tmpdir
  return "${rc}"
}

# The literal is the value of LO_EXPOSE_IMAGE and of ExposeImage (the Go
# twin): the tag of one version and its digest, never a floating tag. No
# other docker call names an nginx image, so a pull of another tag fails the
# test too. A word names an nginx image when it is nginx, or a registry path
# that ends in /nginx, with an optional tag or digest. A docker exec names a
# container, not an image: its nginx is the command in the container. The Go
# twin of the loop is dockerCallsNamingNginx.
@test "expose: the proxy runs the pinned nginx image, by tag and digest" {
  local log="${BATS_TEST_TMPDIR}/docker.log"
  docker() { printf '%s\n' "$*" >> "${log}"; }
  yq() { echo "test.lok8s.dev"; }
  envsubst() { cat; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s LOK8S_LB_POOL="10.125.50.125-10.125.50.150"
  run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 0 ] || { echo "${output}"; return 1; }
  local want="run -d --restart=always --name test-dns-proxy --network lok8s -p 80:80 -p 443:443 nginx:1.30.7-alpine3.24@sha256:caf4e1e032a20f9c3c5e28fc7f25ffd768ad8677b4975c54193075997b3a97e3"
  [ "$(grep -c '^run ' "${log}")" -eq 1 ] || { cat "${log}"; return 1; }
  grep -qxF "${want}" "${log}" || { cat "${log}"; return 1; }
  local line w named=()
  local -a words
  while IFS= read -r line; do
    read -ra words <<< "${line}"
    [[ "${words[0]:-}" == exec ]] && continue
    for w in "${words[@]}"; do
      if [[ "${w}" =~ ^([^[:space:]]*/)?nginx([:@][^[:space:]]*)?$ ]]; then
        named+=("${line}")
        break
      fi
    done
  done < "${log}"
  [ "${#named[@]}" -eq 1 ] && [ "${named[0]}" == "${want}" ] || {
    echo "docker calls that name an nginx image:"; printf '%s\n' "${named[@]}"; cat "${log}"; return 1
  }
}

# The two TLS copies land at the paths that nginx.conf reads: the
# ssl_certificate and ssl_certificate_key paths of the shipped template are
# the targets of the docker cp of tls.crt and of tls.key. The test reads both
# sides, from the template and from the docker argv. Go twin:
# TestExposeTLSCopiesLandAtTheConfPaths.
@test "expose: the TLS copies land at the paths that nginx.conf reads" {
  local log="${BATS_TEST_TMPDIR}/docker.log"
  docker() { printf '%s\n' "$*" >> "${log}"; }
  yq() { echo "test.lok8s.dev"; }
  envsubst() { cat; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s LOK8S_LB_POOL="10.125.50.125-10.125.50.150"
  mkdir -p "${PATH_BASE}/.secrets/tls"
  printf 'CERT' > "${PATH_BASE}/.secrets/tls/tls.crt"
  printf 'KEY' > "${PATH_BASE}/.secrets/tls/tls.key"
  run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 0 ] || { echo "${output}"; return 1; }
  local conf="${PATH_LOK8S}/drivers/lo/cluster/expose/nginx.conf"
  local directive src want got line
  for directive in ssl_certificate ssl_certificate_key; do
    src="${PATH_BASE}/.secrets/tls/tls.crt"
    [[ "${directive}" == ssl_certificate_key ]] && src="${PATH_BASE}/.secrets/tls/tls.key"
    want=() got=()
    while IFS= read -r line; do
      [[ "${line}" =~ ^[[:space:]]*${directive}[[:space:]]+([^[:space:]]+)\;$ ]] && want+=("${BASH_REMATCH[1]}")
    done < "${conf}"
    while IFS= read -r line; do
      [[ "${line}" == "cp ${src} test-dns-proxy:"* ]] && got+=("${line#"cp ${src} test-dns-proxy:"}")
    done < "${log}"
    [ "${#want[@]}" -eq 1 ] || { echo "nginx.conf has ${#want[@]} ${directive} lines, want 1"; cat "${conf}"; return 1; }
    [ "${#got[@]}" -eq 1 ] && [ "${got[0]}" == "${want[0]}" ] || {
      echo "${directive} reads ${want[0]}, but the copies of ${src} land at: ${got[*]}"; cat "${log}"; return 1
    }
  done
}

# Each docker step of lo::expose must succeed. A failed step fails the
# command: the error names the step and the container, no later step runs,
# lo::expose does not print "cluster exposed", and the rendered config does
# not stay on disk. Go twin: TestExposeFailedDockerStepFailsTheCommand.
@test "expose: a failed docker step fails the command and names the step" {
  local log="${BATS_TEST_TMPDIR}/docker.log" fail="" conf="${BATS_TEST_TMPDIR}/nginx.rendered.conf"
  docker() {
    printf '%s\n' "$*" >> "${log}"
    [[ "$*" == *"${fail}"* ]] && return 3
    return 0
  }
  mktemp() { : > "${conf}"; echo "${conf}"; }
  yq() { echo "test.lok8s.dev"; }
  envsubst() { cat; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s LOK8S_LB_POOL="10.125.50.125-10.125.50.150"
  mkdir -p "${PATH_BASE}/.secrets/tls"
  printf 'CERT' > "${PATH_BASE}/.secrets/tls/tls.crt"
  printf 'KEY' > "${PATH_BASE}/.secrets/tls/tls.key"
  local -a calls=(
    "run "
    " test-dns-proxy:/etc/nginx/nginx.conf"
    " test-dns-proxy:/tls.crt"
    " test-dns-proxy:/tls.key"
    "exec test-dns-proxy nginx -s reload"
  )
  local -a steps=(
    "docker run of the proxy container test-dns-proxy"
    "docker cp of the nginx config to test-dns-proxy:/etc/nginx/nginx.conf"
    "docker cp of the TLS certificate to test-dns-proxy:/tls.crt"
    "docker cp of the TLS key to test-dns-proxy:/tls.key"
    "docker exec test-dns-proxy nginx -s reload"
  )
  local i
  for i in "${!calls[@]}"; do
    fail="${calls[i]}"
    : > "${log}"
    run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
    [ "${status}" -eq 1 ] || { echo "step ${i}: status ${status}, want 1"; echo "${output}"; return 1; }
    [[ "${output}" == *"expose: ${steps[i]} failed (exit status 3)."* ]] || {
      echo "step ${i}: the error does not name the step: ${steps[i]}"; echo "${output}"; return 1
    }
    [[ "${output}" != *"cluster exposed"* ]] || { echo "step ${i}: success printed"; echo "${output}"; return 1; }
    [[ "$(tail -n 1 "${log}")" == *"${fail}"* ]] || { echo "step ${i}: a step ran after the failed one"; cat "${log}"; return 1; }
    [ ! -e "${conf}" ] || { echo "step ${i}: the rendered config stays on disk"; return 1; }
  done
}

# A failed expose fails the remote CI run, as a failed sync or remote
# provision does, and the summary does not print. Go twin:
# TestRemoteCIFailedExposeFailsTheRun.
@test "remote CI: a failed expose fails the run" {
  docker() { [[ "${1}" == run ]] && return 3; return 0; }
  ssh() { :; }
  rsync() { :; }
  git() { echo "${PATH_BASE}"; }
  yq() {
    case "$*" in
      *metadata.name*) echo "test-dns" ;;
      *) echo "test.lok8s.dev" ;;
    esac
  }
  envsubst() { cat; }
  lo::read_network_config() { :; }
  lo::read_lb_config() { :; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s
  export LOK8S_REMOTE_USER=ci LOK8S_REMOTE_IP=203.0.113.7 LOK8S_REMOTE_SYNC_DEST=/workspace
  export LOK8S_REMOTE_SYNC_PATH=. LOK8S_REMOTE_TILT=false LOK8S_REMOTE_EXPOSE=true
  LOK8S_REMOTE_SYNC_EXCLUDE=(.git)
  run lo::remote_ci test.lok8s.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1"; echo "${output}"; return 1; }
  [[ "${output}" == *"expose: docker run of the proxy container test-dns-proxy failed (exit status 3)."* ]] || {
    echo "${output}"; return 1
  }
  [[ "${output}" != *"remote CI cluster ready"* ]] || { echo "${output}"; return 1; }
}
