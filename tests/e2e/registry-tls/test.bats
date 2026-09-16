#!/usr/bin/env bats
# E2E: registry-tls — slot 131. The registry set's TLS certificate, from
# the mint to the teardown.
#
# What it proves (the v0.4.0 volume model):
#   1. `lo provision` on a domain with spec.registries.tls mints one leaf
#      into the docker volume `<network>-registry-tls`, and every
#      registry container mounts that volume — no project directory holds
#      the material.
#   2. `lo registry tls status` names the volume and says, per container,
#      what it mounts.
#   3. The containers really serve that certificate: the leaf on the wire
#      at the build registry is the one in the volume.
#   4. `lo registry tls renew` mints a fresh leaf into the same volume and
#      restarts the containers, which then serve the new one.
#   5. `lo registry clean` removes the set, its data volumes AND the
#      certificate volume.
#   6. The one-time import: a project that ran a release before v0.4.0 has
#      the pair in `.secrets/tls/registries`. The next run copies those
#      exact bytes into the volume and says so once.
#   7. `lo down` keeps the certificate volume, `lo destroy` removes it,
#      and neither leaves anything else of the scenario's behind.
#
# CAROOT is redirected into the scenario's scratch, so the run neither
# reads nor writes the developer's own dev CA.

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  # init first: it puts the project's own toolchain on PATH, so the tool
  # check below reports on the binaries the run will actually use.
  e2e::init "${BATS_TEST_DIRNAME}" 131.lok8s.dev
  e2e::require_tools docker kind openssl
  e2e::require_dns 131.lok8s.dev
  e2e::require_binary
  e2e::banner
  e2e::snapshot_world
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 131.lok8s.dev
  e2e::final_teardown
  rm -rf "${BATS_TEST_DIRNAME}/.secrets"
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 131.lok8s.dev
  # Its own CA. The generator creates one when CAROOT holds none, so a
  # fresh machine (and a CI runner) needs no `mkcert -install`.
  export CAROOT="${E2E_STATE}/caroot"
  mkdir -p "${CAROOT}"
}

# _build_ip — the build registry's address on this slot (.101, the
# framework's offset).
_build_ip() { echo "10.125.131.101"; }

# _mounts — one line per registry container: "<name> <mount-source>",
# read from docker rather than from what lok8s says it did.
_mounts() {
  local c
  for c in $(docker ps -a --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Names}}' | sort); do
    printf '%s %s\n' "${c}" \
      "$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/etc/registry/certs"}}{{.Type}}:{{.Name}}{{end}}{{end}}' "${c}")"
  done
}

@test "lo provision mints the certificate into the volume and every registry mounts it" {
  e2e::provision

  run docker volume inspect "$(e2e::tls_volume)" --format '{{.Name}}'
  assert_success
  assert_output "$(e2e::tls_volume)"

  # The volume holds the pair and the SAN set it was minted for.
  run e2e::tls_volume_read tls.crt
  assert_success
  assert_line --index 0 "-----BEGIN CERTIFICATE-----"
  run e2e::tls_volume_read tls.key
  assert_success
  assert_output --partial "PRIVATE KEY"
  run e2e::tls_volume_read .sans
  assert_success
  assert_line "lok8s.local"
  assert_line "$(_build_ip)"

  # Every container mounts the volume, not a directory. A bind mount here
  # is the pre-v0.4.0 shape, which `lo registry tls status` calls legacy.
  e2e::assert_registry_containers up build cache io-docker io-quay io-k8s io-ghcr
  run _mounts
  assert_success
  local line
  while IFS= read -r line; do
    assert_equal "${line#* }" "volume:$(e2e::tls_volume)"
  done <<<"${output}"
}

@test "lo registry tls status names the volume and what each container mounts" {
  local vol
  vol="$(e2e::tls_volume)"
  run e2e::lo registry tls status --domain "${DOMAIN_NAME}"
  assert_success
  assert_line --regexp "^registry TLS +on$"
  assert_line --regexp "^volume +${vol}$"
  assert_line --regexp "^not after +[0-9]{4}-"
  assert_line --regexp "^sans +.*lok8s\.local"
  assert_line "containers"
  # The status pads the container column, so the pair is matched as a
  # pattern rather than as one spelled-out string.
  local short
  for short in build cache io-docker io-quay io-k8s io-ghcr; do
    assert_line --regexp "^ +${E2E_NETWORK}-registry-${short} +volume ${vol}$"
  done
  # "(legacy)" is the marker for a container still on the pre-v0.4.0 bind
  # mount. A fresh set has none.
  refute_output --partial "(legacy)"
}

@test "the registries serve the certificate that is in the volume" {
  local served in_volume
  served="$(e2e::served_cert "$(_build_ip)" | e2e::cert_serial)"
  in_volume="$(e2e::tls_volume_read tls.crt | e2e::cert_serial)"
  assert [ -n "${served}" ]
  assert_equal "${served}" "${in_volume}"
}

@test "lo registry tls renew mints a new certificate and the containers serve it" {
  local before after served
  before="$(e2e::tls_volume_read tls.crt | e2e::cert_serial)"

  run e2e::lo registry tls renew --domain "${DOMAIN_NAME}"
  assert_success
  assert_output --partial "registry TLS renewed: volume $(e2e::tls_volume)"
  assert_output --partial "restarted ${E2E_NETWORK}-registry-build"

  after="$(e2e::tls_volume_read tls.crt | e2e::cert_serial)"
  assert [ -n "${after}" ]
  refute [ "${after}" = "${before}" ]

  # The restart is the point: the containers must serve the new leaf, not
  # the one they loaded at start.
  served="$(e2e::served_cert "$(_build_ip)" | e2e::cert_serial)"
  assert_equal "${served}" "${after}"
}

@test "lo registry clean removes the set, its volumes and the certificate volume" {
  # Keep this set first: the import test below needs a real pre-v0.4.0
  # store, and this is the last moment its contents exist anywhere. All
  # three files, because that is what such a store holds — the SAN record
  # next to the pair.
  e2e::tls_volume_read tls.crt > "${E2E_STATE}/legacy-tls.crt"
  e2e::tls_volume_read tls.key > "${E2E_STATE}/legacy-tls.key"
  e2e::tls_volume_read .sans   > "${E2E_STATE}/legacy-sans"
  assert [ -s "${E2E_STATE}/legacy-tls.crt" ]
  assert [ -s "${E2E_STATE}/legacy-tls.key" ]
  assert [ -s "${E2E_STATE}/legacy-sans" ]

  run e2e::lo registry clean --domain "${DOMAIN_NAME}"
  assert_success

  e2e::assert_registry_containers gone build cache io-docker io-quay io-k8s io-ghcr
  run docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}'
  assert_success
  assert_output ""
}

@test "the legacy .secrets/tls/registries pair is imported once, byte for byte" {
  # A project from before v0.4.0 keeps the material in the flat store and
  # has no volume. Seed exactly that shape, with what the earlier mint
  # produced, so the bytes compared below are real certificate bytes.
  # The SAN record rides along, as it does in a real store: without it
  # the import still happens, and the run then mints a fresh leaf
  # because it cannot tell what the imported one covers.
  local legacy="${PATH_BASE}/.secrets/tls/registries"
  mkdir -p "${legacy}"
  cp "${E2E_STATE}/legacy-tls.crt" "${legacy}/tls.crt"
  cp "${E2E_STATE}/legacy-tls.key" "${legacy}/tls.key"
  cp "${E2E_STATE}/legacy-sans"    "${legacy}/.sans"
  run docker volume inspect "$(e2e::tls_volume)"
  assert_failure

  run e2e::lo provision --domain "${DOMAIN_NAME}"
  assert_success
  # One warning, naming both ends.
  assert_output --partial "registry TLS cert imported from"
  assert_output --partial "${legacy}"
  assert_output --partial "$(e2e::tls_volume)"
  assert_equal "$(grep -c 'registry TLS cert imported from' <<<"${output}")" "1"

  # The same bytes, not a fresh leaf that happens to work — and the
  # registries serve those bytes.
  assert_equal "$(e2e::tls_volume_read tls.crt | openssl x509 -noout -fingerprint -sha256)" \
    "$(openssl x509 -noout -fingerprint -sha256 -in "${legacy}/tls.crt")"
  assert_equal "$(e2e::served_cert "$(_build_ip)" | e2e::cert_serial)" \
    "$(e2e::cert_serial < "${legacy}/tls.crt")"

  # The import is one-time: a second run finds the volume and says
  # nothing more.
  run e2e::lo provision --domain "${DOMAIN_NAME}"
  assert_success
  refute_output --partial "registry TLS cert imported from"
}

@test "lo down keeps the certificate volume" {
  run e2e::down
  assert_success
  e2e::assert_torn_down down

  run docker volume inspect "$(e2e::tls_volume)" --format '{{.Name}}'
  assert_success
  assert_output "$(e2e::tls_volume)"
}

@test "lo destroy removes the certificate volume too" {
  # Stand the cluster back up first. `lo destroy` on a cluster that is
  # already down removes nothing, and would pass however broken its own
  # cluster deletion is — the destroy assertion has to act on a live one.
  e2e::provision
  run kind get clusters
  assert_line "${LOK8S_CLUSTER_NAME}"

  run e2e::destroy
  assert_success
  e2e::assert_torn_down destroy

  run docker volume inspect "$(e2e::tls_volume)"
  assert_failure
}
