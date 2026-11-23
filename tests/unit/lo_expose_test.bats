#!/usr/bin/env bats
# lo_expose_test.bats: the `lo expose` proxy. lo::expose
# (.lok8s/drivers/lo/utils/expose.sh) starts the proxy container from
# LO_EXPOSE_IMAGE (.lok8s/drivers/lo/utils/defaults.sh), which is pinned by
# tag and digest, copies the nginx config and the TLS files into it, and
# reloads nginx. Without the TLS certificate or key it starts no proxy, and
# a failed docker step fails the command. Go twins: the TestExpose* and
# TestRemoteCI* tests in internal/driver/lo/{services,remote}_test.go.
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
  # The proxy starts only with its TLS certificate and key.
  mkdir -p "${PATH_BASE}/.secrets/tls"
  printf 'CERT' > "${PATH_BASE}/.secrets/tls/tls.crt"
  printf 'KEY' > "${PATH_BASE}/.secrets/tls/tls.key"
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
# command: the error names the step and the container. After the failed
# step, lo::expose only removes the proxy container, so that no container
# with a config that is not complete stays. lo::expose does not print
# "cluster exposed", and the rendered config does not stay on disk. Go twin:
# TestExposeFailedDockerStepFailsTheCommand.
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
  local i n
  local -a lines
  for i in "${!calls[@]}"; do
    fail="${calls[i]}"
    : > "${log}"
    run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
    [ "${status}" -eq 1 ] || { echo "step ${i}: status ${status}, want 1"; echo "${output}"; return 1; }
    [[ "${output}" == *"expose: ${steps[i]} failed (exit status 3), so the proxy does not run. Fix the cause in the docker message above, then run the command again."* ]] || {
      echo "step ${i}: the error does not name the step: ${steps[i]}"; echo "${output}"; return 1
    }
    [[ "${output}" != *"cluster exposed"* ]] || { echo "step ${i}: success printed"; echo "${output}"; return 1; }
    mapfile -t lines < "${log}"
    n=${#lines[@]}
    (( n >= 2 )) && [[ "${lines[n-2]}" == *"${fail}"* && "${lines[n-1]}" == "rm -f test-dns-proxy" ]] || {
      echo "step ${i}: after the failed step, want only the removal of the proxy container"; cat "${log}"; return 1
    }
    [ ! -e "${conf}" ] || { echo "step ${i}: the rendered config stays on disk"; return 1; }
  done
}

# The stubs of a remote CI run with the expose on. docker logs DOCKER_HOST
# and its arguments, and fails when its first argument is ${fail}. scp
# writes the kubeconfig of the VM, so that the tunnel step runs.
_remote_ci_stubs() {
  calls="${BATS_TEST_TMPDIR}/calls.log"
  docker() {
    printf '%s %s\n' "${DOCKER_HOST:-}" "$*" >> "${calls}"
    [[ "${1}" == "${fail}" ]] && return 3
    return 0
  }
  ssh() { :; }
  rsync() { :; }
  scp() { printf 'scp %s\n' "$*" >> "${calls}"; printf 'kubeconfig' > "${*: -1}"; }
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
  lo::kubeconfig_tunnel() { printf 'tunnel %s\n' "$*" >> "${calls}"; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s
  export LOK8S_REMOTE_USER=ci LOK8S_REMOTE_IP=203.0.113.7 LOK8S_REMOTE_SYNC_DEST=/workspace
  export LOK8S_REMOTE_SYNC_PATH=. LOK8S_REMOTE_TILT=false LOK8S_REMOTE_EXPOSE=true
  LOK8S_REMOTE_SYNC_EXCLUDE=(.git)
}

# A failed expose does not stop the remote CI run before the kubeconfig
# fetch and the tunnel: the cluster runs. The run fails at the end, and the
# summary shows no proxy URL. The expose runs with DOCKER_HOST set to the
# VM. Go twin: TestRemoteCIFailedExposeFailsTheRun.
@test "remote CI: a failed expose still sets up the kubeconfig, then fails the run" {
  local calls fail=run kc="${PATH_BASE}/.kubeconfig/test-dns.yaml"
  _remote_ci_stubs
  run lo::remote_ci test.lok8s.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 1 ] || { echo "status ${status}, want 1"; echo "${output}"; return 1; }
  [[ "${output}" == *"expose: docker run of the proxy container test-dns-proxy failed (exit status 3)"* ]] || {
    echo "${output}"; return 1
  }
  grep -qxF "scp ci@203.0.113.7:/workspace/.kubeconfig/test-dns.yaml ${kc}" "${calls}" || { cat "${calls}"; return 1; }
  grep -qxF "tunnel ${kc} ci 203.0.113.7" "${calls}" || { cat "${calls}"; return 1; }
  local line docker_calls=0
  while IFS= read -r line; do
    [[ "${line}" == "scp "* || "${line}" == "tunnel "* ]] && continue
    docker_calls=$(( docker_calls + 1 ))
    [[ "${line}" == "ssh://ci@203.0.113.7 "* ]] || { echo "a docker call ran with another DOCKER_HOST: ${line}"; return 1; }
  done < "${calls}"
  (( docker_calls > 0 )) || { echo "expose ran no docker call"; cat "${calls}"; return 1; }
  [[ "${output}" == *":: remote CI cluster ready"* && "${output}" != *"URL:"* ]] || {
    echo "want the summary with no proxy URL"; echo "${output}"; return 1
  }
  [[ "${output}" == *"remote CI: the cluster runs, but expose failed (its error is above), so the run fails."* ]] || {
    echo "${output}"; return 1
  }
}

# Without the TLS files, the expose of a remote CI run starts no proxy and
# the run succeeds, but the summary shows no proxy URL. With the files, it
# shows the URL. Go twin: TestRemoteCISummaryShowsTheProxyURLOnlyWhenTheProxyRuns.
@test "remote CI: the summary shows the proxy URL only when the proxy runs" {
  local calls fail=""
  local url="   URL:        https://*.test.lok8s.dev (via 203.0.113.7:443)"
  _remote_ci_stubs
  run lo::remote_ci test.lok8s.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 0 ] || { echo "with TLS: status ${status}"; echo "${output}"; return 1; }
  [[ "${output}" == *"${url}"* ]] || { echo "with TLS: no URL line"; echo "${output}"; return 1; }
  rm -f "${PATH_BASE}/.secrets/tls/tls.crt" "${PATH_BASE}/.secrets/tls/tls.key"
  run lo::remote_ci test.lok8s.dev "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 0 ] || { echo "without TLS: status ${status}"; echo "${output}"; return 1; }
  [[ "${output}" != *"URL:"* ]] || { echo "without TLS: a URL line"; echo "${output}"; return 1; }
}

# Without the TLS certificate or key, lo::expose does not start the proxy:
# nginx.conf serves HTTPS only. It removes the proxy of an earlier run, warns
# once and returns 0. It runs no other docker step and does not print
# "cluster exposed". Go twin: TestExposeWithoutTLSFilesDoesNotStartTheProxy.
@test "expose: without the TLS certificate or key, the proxy does not start" {
  local log="${BATS_TEST_TMPDIR}/docker.log" tls="${PATH_BASE}/.secrets/tls" missing
  docker() { printf '%s\n' "$*" >> "${log}"; }
  yq() { echo "test.lok8s.dev"; }
  envsubst() { cat; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s LOK8S_LB_POOL="10.125.50.125-10.125.50.150"
  local warning="expose: no TLS certificate and key at ${tls}/tls.crt and ${tls}/tls.key, so the proxy is not started. Add both files, or set spec.remote.expose: false."
  for missing in both key crt; do
    printf 'CERT' > "${tls}/tls.crt"
    printf 'KEY' > "${tls}/tls.key"
    case "${missing}" in
      both) rm -f "${tls}/tls.crt" "${tls}/tls.key" ;;
      *) rm -f "${tls}/tls.${missing}" ;;
    esac
    : > "${log}"
    run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
    [ "${status}" -eq 0 ] || { echo "missing ${missing}: status ${status}"; echo "${output}"; return 1; }
    [ "$(cat "${log}")" == "rm -f test-dns-proxy" ] || {
      echo "missing ${missing}: want only the removal of the proxy container"; cat "${log}"; return 1
    }
    [ "$(printf '%s\n' "${output}" | grep -cF "${warning}")" -eq 1 ] || {
      echo "missing ${missing}: want the warning once"; echo "${output}"; return 1
    }
    [[ "${output}" != *"cluster exposed"* ]] || { echo "missing ${missing}: success printed"; echo "${output}"; return 1; }
  done
}

# The docker steps of lo::expose run in this order: the removal of the proxy
# of an earlier run, the run, the copies of the config, the certificate and
# the key, and the reload. Go twin: TestExposeRunsTheStepsInOrder.
@test "expose: the docker steps run in order" {
  local log="${BATS_TEST_TMPDIR}/docker.log" conf="${BATS_TEST_TMPDIR}/nginx.rendered.conf"
  docker() { printf '%s\n' "$*" >> "${log}"; }
  mktemp() { : > "${conf}"; echo "${conf}"; }
  yq() { echo "test.lok8s.dev"; }
  envsubst() { cat; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s LOK8S_LB_POOL="10.125.50.125-10.125.50.150"
  run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 0 ] || { echo "${output}"; return 1; }
  local want
  want="rm -f test-dns-proxy
run -d --restart=always --name test-dns-proxy --network lok8s -p 80:80 -p 443:443 ${LO_EXPOSE_IMAGE}
cp ${conf} test-dns-proxy:/etc/nginx/nginx.conf
cp ${PATH_BASE}/.secrets/tls/tls.crt test-dns-proxy:/tls.crt
cp ${PATH_BASE}/.secrets/tls/tls.key test-dns-proxy:/tls.key
exec test-dns-proxy nginx -s reload"
  [ "$(cat "${log}")" == "${want}" ] || { echo "docker calls:"; cat "${log}"; echo "want:"; echo "${want}"; return 1; }
  [[ "${output}" == *":: cluster exposed at https://*.test.lok8s.dev"* ]] || { echo "${output}"; return 1; }
}
