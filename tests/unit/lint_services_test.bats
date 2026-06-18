#!/usr/bin/env bats
# lint_services_test.bats — unit tests for the services.yaml + lok8s.yaml
# schema validation and Tilt/label drift checks added to libs/lint.
#
# These exercise lint::services / lint::lok8s_yaml / lint::drift directly.
# Like lint_test.bats, every external (yq, grep) the function calls is either
# mocked (yq) or runs against real scratch files (grep over the tmpdir).

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/lint"
}

teardown() {
  teardown_tmpdir
}

# --- lok8s.yaml: top-level schema ------------------------------------------

@test "lok8s.yaml with unknown 'kind' key is flagged with allowed-keys hint" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
kind: App
build:
  context: .
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' kind build ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "kubehz-core" "${f}"
  assert_failure
  assert_output --partial "lok8s.yaml (kubehz-core): unknown key 'kind' — allowed: build, ports, links, workloads, tilt, components"
}

@test "valid bare lok8s.yaml passes" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
build:
  context: .
  dockerfile: lok8s.Dockerfile
ports:
  - from: 3000
    to: 3000
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' build ports ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "api" "${f}"
  assert_success
  assert_output ""
}

@test "valid components lok8s.yaml passes" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
components:
  - name: api
    build:
      context: .
  - name: operator
    build:
      context: ./operator
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' components ;;
      '.components | type') echo '!!seq' ;;
      '.components | length') echo "2" ;;
      '.components[0].name // ""') echo "api" ;;
      '.components[1].name // ""') echo "operator" ;;
      '.components[0].build // ""') echo "context: ." ;;
      '.components[1].build // ""') echo "context: ./operator" ;;
      '.components[0] | keys | .[]') printf '%s\n' name build ;;
      '.components[1] | keys | .[]') printf '%s\n' name build ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "kubehz-core" "${f}"
  assert_success
  assert_output ""
}

@test "lok8s.yaml with both build and components is flagged (mutually exclusive)" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
build:
  context: .
components:
  - name: api
    build:
      context: .
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' build components ;;
      '.components | type') echo '!!seq' ;;
      '.components | length') echo "1" ;;
      '.components[0].name // ""') echo "api" ;;
      '.components[0].build // ""') echo "context: ." ;;
      '.components[0] | keys | .[]') printf '%s\n' name build ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "kubehz-core" "${f}"
  assert_failure
  assert_output --partial "'build' and 'components' are mutually exclusive"
}

@test "lok8s.yaml with neither build nor components is flagged" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
ports:
  - from: 80
    to: 80
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' ports ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "foo" "${f}"
  assert_failure
  assert_output --partial "lok8s.yaml (foo): 'build' is required (or use 'components')"
}

@test "components entry missing required name is flagged" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
components:
  - build:
      context: .
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' components ;;
      '.components | type') echo '!!seq' ;;
      '.components | length') echo "1" ;;
      '.components[0].name // ""') echo "" ;;
      '.components[0].build // ""') echo "context: ." ;;
      '.components[0] | keys | .[]') printf '%s\n' build ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "kubehz-core" "${f}"
  assert_failure
  assert_output --partial "components[0] is missing required 'name'"
}

@test "components entry missing required build is flagged" {
  local f="${BATS_TEST_TMPDIR}/lok8s.yaml"
  cat > "${f}" <<'YAML'
components:
  - name: api
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' components ;;
      '.components | type') echo '!!seq' ;;
      '.components | length') echo "1" ;;
      '.components[0].name // ""') echo "api" ;;
      '.components[0].build // ""') echo "" ;;
      '.components[0] | keys | .[]') printf '%s\n' name ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::lok8s_yaml "kubehz-core" "${f}"
  assert_failure
  assert_output --partial "components[api] is missing required 'build'"
}

# --- services.yaml: top-level schema + per-service entries ------------------

@test "services.yaml with unknown top-level key is flagged" {
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
spec:
  foo: bar
services: {}
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' spec services ;;
      '.services // {} | keys | .[]') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::services
  assert_failure
  assert_output --partial "services.yaml: unknown top-level key 'spec' — allowed: apiVersion, kind, metadata, registry, defaults, services"
}

@test "services.yaml image+registry on a service is flagged (mutually exclusive)" {
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    image: ghcr.io/org/api:latest
    registry:
      endpoint: ghcr.io/org
YAML

  yq() {
    local query="$2"
    case "${query}" in
      'keys | .[]') printf '%s\n' services ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api" | keys | .[]') printf '%s\n' image registry ;;
      '.services."api".registry | keys | .[]') printf '%s\n' endpoint ;;
      '.services."api".path // "./api"') echo "./api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::services
  assert_failure
  assert_output --partial "services.api: 'image' and 'registry' are mutually exclusive"
}

@test "services.yaml validates per-service lok8s.yaml at resolved path" {
  # services.yaml points api at ./src/api; the lok8s.yaml there has a bad key.
  mkdir -p "${BATS_TEST_TMPDIR}/src/api"
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    path: ./src/api
YAML
  cat > "${BATS_TEST_TMPDIR}/src/api/lok8s.yaml" <<'YAML'
kind: Service
build:
  context: .
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      'keys | .[]')
        case "${file}" in
          *src/api/lok8s.yaml) printf '%s\n' kind build ;;
          *services.yaml) printf '%s\n' services ;;
        esac
        ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api" | keys | .[]') printf '%s\n' path ;;
      '.services."api".path // "./api"') echo "./src/api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::services
  assert_failure
  assert_output --partial "lok8s.yaml (api): unknown key 'kind' — allowed: build, ports, links, workloads, tilt, components"
}

@test "lint::services returns 0 when no services.yaml present" {
  run lint::services
  assert_success
  assert_output ""
}

# --- drift checks -----------------------------------------------------------

@test "drift (a): root Tiltfile with docker_build alongside populated services.yaml warns" {
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    path: ./api
YAML
  cat > "${BATS_TEST_TMPDIR}/Tiltfile" <<'TILT'
docker_build('lok8s.local/api', 'api')
k8s_yaml('api/deploy.yaml')
TILT

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api".path // "./api"') echo "./api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  assert_output --partial "Tiltfile: contains docker_build()/k8s_yaml() while services.yaml declares 1 service(s)"
  assert_output --partial "load('./.lok8s/tilt/Tiltfile','lok8s'); lok8s()"
}

@test "drift (a): thin 2-line Tiltfile does NOT warn" {
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    path: ./api
YAML
  cat > "${BATS_TEST_TMPDIR}/Tiltfile" <<'TILT'
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s()
TILT

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api".path // "./api"') echo "./api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  refute_output --partial "prefer the 2-line form"
}

@test "drift (b): per-service path with its own Tiltfile warns" {
  mkdir -p "${BATS_TEST_TMPDIR}/api"
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    path: ./api
YAML
  cat > "${BATS_TEST_TMPDIR}/api/Tiltfile" <<'TILT'
# redundant per-submodule Tiltfile
TILT

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api".path // "./api"') echo "./api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  assert_output --partial "./api/Tiltfile is redundant"
}

@test "drift (c): deploy manifests missing lok8s.dev/name label warn" {
  mkdir -p "${BATS_TEST_TMPDIR}/api/deploy"
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    path: ./api
YAML
  cat > "${BATS_TEST_TMPDIR}/api/deploy/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 1
YAML

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api".path // "./api"') echo "./api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  assert_output --partial "no 'lok8s.dev/name: api' label found in ./api/deploy"
}

@test "drift (c): deploy manifests WITH lok8s.dev/name label do not warn" {
  mkdir -p "${BATS_TEST_TMPDIR}/api/deploy"
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  api:
    path: ./api
YAML
  cat > "${BATS_TEST_TMPDIR}/api/deploy/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  labels:
    lok8s.dev/name: api
spec:
  replicas: 1
YAML

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "api" ;;
      '.services."api".path // "./api"') echo "./api" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  refute_output --partial "will silently drop"
}

@test "drift (c): components service checks per-component labels (no false positive)" {
  mkdir -p "${BATS_TEST_TMPDIR}/core/deploy/api" "${BATS_TEST_TMPDIR}/core/deploy/operator"
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  kubehz-core:
    path: ./core
YAML
  cat > "${BATS_TEST_TMPDIR}/core/lok8s.yaml" <<'YAML'
components:
  - name: kubehz-api
    build: { context: ., dockerfile: Dockerfile.api }
  - name: kubehz-operator
    build: { context: ., dockerfile: Dockerfile.operator }
YAML
  printf 'metadata:\n  labels:\n    lok8s.dev/name: kubehz-api\n' > "${BATS_TEST_TMPDIR}/core/deploy/api/d.yaml"
  printf 'metadata:\n  labels:\n    lok8s.dev/name: kubehz-operator\n' > "${BATS_TEST_TMPDIR}/core/deploy/operator/d.yaml"

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "kubehz-core" ;;
      '.services."kubehz-core".path // "./kubehz-core"') echo "./core" ;;
      '.components // [] | length') echo "2" ;;
      '.components[].name') printf 'kubehz-api\nkubehz-operator\n' ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  refute_output --partial "silently drop"
}

@test "drift (c): components service warns for the component whose label is missing" {
  mkdir -p "${BATS_TEST_TMPDIR}/core/deploy/api"
  cat > "${BATS_TEST_TMPDIR}/services.yaml" <<'YAML'
services:
  kubehz-core:
    path: ./core
YAML
  cat > "${BATS_TEST_TMPDIR}/core/lok8s.yaml" <<'YAML'
components:
  - name: kubehz-api
    build: { context: ., dockerfile: Dockerfile.api }
  - name: kubehz-operator
    build: { context: ., dockerfile: Dockerfile.operator }
YAML
  printf 'metadata:\n  labels:\n    lok8s.dev/name: kubehz-api\n' > "${BATS_TEST_TMPDIR}/core/deploy/api/d.yaml"

  yq() {
    local query="$2"
    case "${query}" in
      '.services // {} | length') echo "1" ;;
      '.services // {} | keys | .[]') echo "kubehz-core" ;;
      '.services."kubehz-core".path // "./kubehz-core"') echo "./core" ;;
      '.components // [] | length') echo "2" ;;
      '.components[].name') printf 'kubehz-api\nkubehz-operator\n' ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::drift
  assert_success
  assert_output --partial "no 'lok8s.dev/name: kubehz-operator' label found"
  refute_output --partial "lok8s.dev/name: kubehz-api' label found"
}
