#!/usr/bin/env bats
# build_split_test.bats — the spec-declared split-artifacts mode (#72):
#   (a) mode resolution: spec.build.artifacts / spec.gitops.provider implies /
#       explicit single pins,
#   (b) build::split emits <Kind>.<namespace>.<name>.yaml per resource
#       (cluster-scoped without the namespace segment),
#   (c) Secrets are NEVER written plaintext — sops-encrypted with the
#       spec.gitops.age recipients, and a Secret WITHOUT recipients hard-fails,
#   (d) Jobs are GitOps-shaped (TTL stripped, force annotation added),
#   (e) stale generated files are pruned; env-owned lowercase files survive.
# Real yq drives the shaping/split; sops is stubbed (the encryption contract
# is asserted via the stub's arguments + output marker).

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1
  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"

  command -v yq &>/dev/null || skip "yq not available"

  DOMAIN="fixture.lok8s.dev"
  DOMAIN_DIR="${PATH_CLUSTERS}/${DOMAIN}"
  mkdir -p "${DOMAIN_DIR}"

  # sops stub: records its argv and emits a marker file so the tests can
  # assert the encryption call shape without real age keys.
  mkdir -p "${BATS_TEST_TMPDIR}/bin"
  cat > "${BATS_TEST_TMPDIR}/bin/sops" <<'STUB'
#!/usr/bin/env bash
echo "$@" >> "${SOPS_STUB_LOG}"
out=""
prev=""
for a in "$@"; do
  [[ "${prev}" == "--output" ]] && out="${a}"
  prev="${a}"
done
printf 'sops: {stubbed: true}\n' > "${out}"
STUB
  chmod +x "${BATS_TEST_TMPDIR}/bin/sops"
  export PATH="${BATS_TEST_TMPDIR}/bin:${PATH}"
  export SOPS_STUB_LOG="${BATS_TEST_TMPDIR}/sops.log"
}

write_spec() { # write_spec <yaml-body>
  cat > "${DOMAIN_DIR}/cluster.lok8s.yaml" <<EOF
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata: { name: fixture }
spec:
${1}
EOF
}

write_artifact() { # a Deployment + cluster-scoped CRB + TTL'd Job + Secret
  cat > "${DOMAIN_DIR}/artifacts.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: app
spec: { replicas: 1 }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: web-crb
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view }
---
apiVersion: batch/v1
kind: Job
metadata:
  name: seed
  namespace: app
spec:
  ttlSecondsAfterFinished: 0
  template: { spec: { restartPolicy: Never, containers: [{ name: x, image: busybox }] } }
---
apiVersion: v1
kind: Secret
metadata:
  name: creds
  namespace: app
data: { password: aHVudGVyMg== }
EOF
}

@test "mode: default is single" {
  write_spec "  cluster: { domain: ${DOMAIN} }"
  run build::_artifacts_mode "${DOMAIN_DIR}"
  [ "$status" -eq 0 ]
  [ "$output" = "single" ]
}

@test "mode: spec.build.artifacts=split" {
  write_spec "  build: { artifacts: split }"
  run build::_artifacts_mode "${DOMAIN_DIR}"
  [ "$output" = "split" ]
}

@test "mode: gitops.provider implies split" {
  write_spec "  gitops: { provider: flux }"
  run build::_artifacts_mode "${DOMAIN_DIR}"
  [ "$output" = "split" ]
}

@test "mode: explicit single pins even with gitops.provider" {
  write_spec "$(printf '  build: { artifacts: single }\n  gitops: { provider: flux }')"
  run build::_artifacts_mode "${DOMAIN_DIR}"
  [ "$output" = "single" ]
}

@test "split: per-resource files, cluster-scoped without namespace segment" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Deployment.app.web.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/ClusterRoleBinding.web-crb.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/Job.app.seed.yaml" ]
}

@test "split: Secret emitted ONLY as sops file, recipients passed via config" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1aaa, age1bbb]')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
  [ ! -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.yaml" ]
  # encryption call shape: encrypted-regex limits to data/stringData, and a
  # dedicated sops config (not the repo's .sops.yaml) carries the recipients
  grep -qF -- '--encrypted-regex ^(data|stringData)$' "${SOPS_STUB_LOG}"
  grep -qF -- '--config' "${SOPS_STUB_LOG}"
}

@test "split: Secret without spec.gitops.age recipients hard-fails, writes nothing" {
  write_spec "  build: { artifacts: split }"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"refusing to write plaintext Secrets"* ]]
  [ ! -e "${DOMAIN_DIR}/artifacts/Secret.app.creds.yaml" ]
  [ ! -e "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
}

@test "split: Jobs are GitOps-shaped (TTL stripped, force annotation)" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  run yq -r '.spec.ttlSecondsAfterFinished // "gone"' "${DOMAIN_DIR}/artifacts/Job.app.seed.yaml"
  [ "$output" = "gone" ]
  run yq -r '.metadata.annotations."kustomize.toolkit.fluxcd.io/force"' "${DOMAIN_DIR}/artifacts/Job.app.seed.yaml"
  [ "$output" = "enabled" ]
}

@test "split: prunes stale generated files, preserves env-owned lowercase files" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  echo "stale" > "${DOMAIN_DIR}/artifacts/Deployment.app.gone.yaml"
  echo "env-owned" > "${DOMAIN_DIR}/artifacts/kustomization.yaml"
  echo "env-owned" > "${DOMAIN_DIR}/artifacts/capi.yaml"
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ ! -e "${DOMAIN_DIR}/artifacts/Deployment.app.gone.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/kustomization.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/capi.yaml" ]
}

@test "split: .gitignore defense net blocks plaintext Secrets" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  grep -q '^Secret\.\*\.yaml$' "${DOMAIN_DIR}/artifacts/.gitignore"
  grep -q '^!Secret\.\*\.sops\.yaml$' "${DOMAIN_DIR}/artifacts/.gitignore"
}

@test "split: no artifacts.yaml errors clearly" {
  write_spec "  build: { artifacts: split }"
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"build first"* ]]
}

@test "mode: deploy.lok8s.yaml domains resolve too" {
  rm -f "${DOMAIN_DIR}/cluster.lok8s.yaml"
  cat > "${DOMAIN_DIR}/deploy.lok8s.yaml" <<'EOF'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
spec:
  gitops: { provider: flux, age: [age1testkey] }
EOF
  run build::_artifacts_mode "${DOMAIN_DIR}"
  [ "$output" = "split" ]
}

@test "split: non-age recipient is rejected (input validation)" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: ["age1ok; rm -rf /"]')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not an age public key"* ]]
}

@test "split: kind/ns/name collision across API groups is refused" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  cat > "${DOMAIN_DIR}/artifacts.yaml" <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: db, namespace: x }
---
apiVersion: kubermatic.k8c.io/v1
kind: Cluster
metadata: { name: db, namespace: x }
EOF
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"collision"* ]]
}

@test "split: a mid-split failure leaves the existing artifacts/ UNTOUCHED" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  echo "previous good render" > "${DOMAIN_DIR}/artifacts/Deployment.app.old.yaml"
  # make the stubbed sops fail → the swap must never happen
  cat > "${BATS_TEST_TMPDIR}/bin/sops" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
  chmod +x "${BATS_TEST_TMPDIR}/bin/sops"
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Deployment.app.old.yaml" ]
  [ ! -e "${DOMAIN_DIR}/artifacts/Deployment.app.web.yaml" ]
  # and no staging directory litter is left behind
  run bash -c "ls -d '${DOMAIN_DIR}'/.artifacts-stage.* 2>/dev/null | wc -l"
  [ "$output" = "0" ]
}

@test "split: non-RFC1123 Secret metadata is refused (selector guard)" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  cat > "${DOMAIN_DIR}/artifacts.yaml" <<'EOF'
apiVersion: v1
kind: Secret
metadata: { name: "Bad_Name", namespace: app }
data: { k: dGVzdA== }
EOF
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"non-RFC1123"* ]]
  [ ! -e "${DOMAIN_DIR}/artifacts/Secret.app.Bad_Name.sops.yaml" ]
}

@test "split: Secrets-only render works (empty non-Secret split)" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  cat > "${DOMAIN_DIR}/artifacts.yaml" <<'EOF'
apiVersion: v1
kind: Secret
metadata: { name: only, namespace: app }
data: { k: dGVzdA== }
EOF
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.only.sops.yaml" ]
  # no junk files from the empty non-Secret stream
  run bash -c "ls '${DOMAIN_DIR}/artifacts/'*.yml 2>/dev/null | wc -l"
  [ "$output" = "0" ]
  run bash -c "ls '${DOMAIN_DIR}/artifacts/' | grep -c '^\.yml$\|^yml$'"
  [ "$output" = "0" ]
}

@test "split: emitted-Secret count mismatch refuses the swap" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  echo "live" > "${DOMAIN_DIR}/artifacts/Secret.app.keepme.sops.yaml"
  # sops stub that eats the call without producing output → secrets stays 0…
  # actually produce output but make the LISTING see more: simulate by a stub
  # that succeeds while we corrupt the pairs source is impractical here, so
  # instead assert the guard via a stub that silently produces NO file AND
  # exits 0 (a masked encrypt failure = same class: emitted < rendered).
  cat > "${BATS_TEST_TMPDIR}/bin/sops" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
  chmod +x "${BATS_TEST_TMPDIR}/bin/sops"
  run build::split "${DOMAIN}"
  # either the post-condition (sops-metadata check on stage) or the count
  # guard must stop the swap — the pre-existing file survives regardless
  [ "$status" -ne 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.keepme.sops.yaml" ]
}

@test "split: REAL sops end-to-end (encrypts, no plaintext)" {
  # bypass the stub — use the toolchain sops with a syntactically valid age
  # recipient (encryption needs only the public key)
  export PATH="${PATH#${BATS_TEST_TMPDIR}/bin:}"
  command -v sops &>/dev/null || skip "sops not available"
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p]')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  grep -q '^sops:' "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"
  grep -q 'ENC\[' "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"
  ! grep -q 'aHVudGVyMg==' "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"
}
