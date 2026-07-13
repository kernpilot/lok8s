#!/usr/bin/env bats
# build_encrypt_test.bats — encryption DECOUPLED from split (#72 follow-up):
#   (a) spec.build.encrypt parsing: type/on defaults, `always`, unknown rejects,
#   (b) encrypt.on: change — skip re-encrypting a Secret whose committed sops
#       twin already decrypts to the same (CANONICAL) plaintext; re-encrypt when
#       it differs, or when the prior can't be decrypted (safe fallback); a
#       Secret dropped from the render is still PRUNED in a full build,
#   (c) encrypt.on: always — re-encrypt every Secret every build (no compare),
#   (d) lo build --no-secrets (LOK8S_BUILD_NO_SECRETS=1) — split ONLY non-Secret
#       resources; committed Secret.*.sops.yaml are never created, re-encrypted,
#       or PRUNED (the CI-render danger); a non-Secret dropped from the render is
#       still pruned; the Secret loop / generators are never entered.
#
# Real yq drives the shaping/split/canonical-compare; sops is stubbed. The stub
# ROUND-TRIPS content faithfully (encrypt base64-stashes the plaintext under a
# `sops:` header; decrypt base64-decodes it back) so on:change can be exercised
# deterministically WITHOUT real age keys — and it logs encrypt vs decrypt to
# SEPARATE files so a test can assert exactly which Secret got (re-)encrypted.

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1
  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"

  command -v yq &>/dev/null || skip "yq not available"

  DOMAIN="fixture.lok8s.dev"
  DOMAIN_DIR="${PATH_CLUSTERS}/${DOMAIN}"
  mkdir -p "${DOMAIN_DIR}"

  # Faithful sops stub. It records argv (combined + per-op logs) and:
  #   encrypt: reads the plaintext YAML from stdin and writes the --output file
  #            as a `sops:`-headed doc that base64-stashes that plaintext under
  #            `stub_enc:` — so it passes the lib's `^sops:` check AND can be
  #            decrypted back byte-identically.
  #   decrypt: reads the input file (last arg), base64-decodes stub_enc: to
  #            stdout. A file WITHOUT stub_enc: (a "prior we can't read") makes
  #            decrypt fail (exit 1) — driving the safe re-encrypt fallback.
  mkdir -p "${BATS_TEST_TMPDIR}/bin"
  cat > "${BATS_TEST_TMPDIR}/bin/sops" <<'STUB'
#!/usr/bin/env bash
echo "$@" >> "${SOPS_STUB_LOG}"

op=""
out=""
infile=""
prev=""
for a in "$@"; do
  case "${a}" in
    encrypt) op="encrypt" ;;
    decrypt|-d) op="decrypt" ;;
  esac
  [[ "${prev}" == "--output" ]] && out="${a}"
  # track the last non-flag, non-value token as the positional input file
  case "${a}" in
    --*) ;;                          # a flag
    *)
      # skip the value that follows --output / --config / --input-type / …
      case "${prev}" in --*) ;; *) infile="${a}" ;; esac
      ;;
  esac
  prev="${a}"
done

if [[ "${op}" == "decrypt" ]]; then
  echo "$@" >> "${SOPS_DECRYPT_LOG:-/dev/null}"
  # input file is the trailing positional
  local_in="${infile}"
  [[ -f "${local_in}" ]] || exit 1
  b64=$(sed -n 's/^[[:space:]]*stub_enc:[[:space:]]*//p' "${local_in}")
  [[ -n "${b64}" ]] || exit 1                 # not a stub-encrypted prior → fail
  printf '%s' "${b64}" | base64 --decode || exit 1
  exit 0
fi

# encrypt (default)
echo "$@" >> "${SOPS_ENCRYPT_LOG:-/dev/null}"
plaintext=$(cat)                              # from /dev/stdin
{
  printf 'sops:\n'
  printf '    stub_enc: %s\n' "$(printf '%s' "${plaintext}" | base64 | tr -d '\n')"
} > "${out}"
STUB
  chmod +x "${BATS_TEST_TMPDIR}/bin/sops"
  export PATH="${BATS_TEST_TMPDIR}/bin:${PATH}"
  export SOPS_STUB_LOG="${BATS_TEST_TMPDIR}/sops.log"
  export SOPS_ENCRYPT_LOG="${BATS_TEST_TMPDIR}/sops.encrypt.log"
  export SOPS_DECRYPT_LOG="${BATS_TEST_TMPDIR}/sops.decrypt.log"
  : > "${SOPS_STUB_LOG}"; : > "${SOPS_ENCRYPT_LOG}"; : > "${SOPS_DECRYPT_LOG}"
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

# A Deployment + a Secret. write_artifact [secret-password-b64]
write_artifact() {
  local pw="${1:-aHVudGVyMg==}"
  cat > "${DOMAIN_DIR}/artifacts.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: app
spec: { replicas: 1 }
---
apiVersion: v1
kind: Secret
metadata:
  name: creds
  namespace: app
data: { password: ${pw} }
EOF
}

# Encrypt the current fresh render of Secret app/creds through the stub and drop
# it into the committed artifacts/ dir as its sops twin — the "prior" a later
# on:change build compares against. Optionally scramble the key ORDER so the
# canonical compare (not a byte compare) is what's under test.
seed_prior_secret() { # seed_prior_secret [reorder]
  mkdir -p "${DOMAIN_DIR}/artifacts"
  local sel
  sel=$(yq eval 'select(.kind == "Secret")' "${DOMAIN_DIR}/artifacts.yaml")
  if [[ "${1:-}" == "reorder" ]]; then
    # same content, different key order + flow style → must still be "unchanged"
    sel=$(printf '%s' "${sel}" | yq -P 'sort_keys(..)' | yq eval 'pick(["data","metadata","kind","apiVersion"])')
  fi
  printf '%s' "${sel}" \
    | sops --config /dev/null encrypt --output "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" /dev/stdin
  # reset the logs so the test asserts only the build-under-test's calls
  : > "${SOPS_STUB_LOG}"; : > "${SOPS_ENCRYPT_LOG}"; : > "${SOPS_DECRYPT_LOG}"
}

# ── spec.build.encrypt parsing ───────────────────────────

@test "encrypt: defaults are sops/change when no encrypt block" {
  write_spec "  build: { artifacts: split }"
  run build::_encrypt_mode "${DOMAIN_DIR}"
  [ "$status" -eq 0 ]
  [ "$output" = "sops change" ]
}

@test "encrypt: on:always is parsed" {
  write_spec "$(printf '  build:\n    artifacts: split\n    encrypt: { on: always }')"
  run build::_encrypt_mode "${DOMAIN_DIR}"
  [ "$status" -eq 0 ]
  [ "$output" = "sops always" ]
}

@test "encrypt: explicit type sops + on change" {
  write_spec "$(printf '  build:\n    encrypt: { type: sops, on: change }')"
  run build::_encrypt_mode "${DOMAIN_DIR}"
  [ "$output" = "sops change" ]
}

@test "encrypt: unknown type is rejected" {
  write_spec "$(printf '  build:\n    encrypt: { type: vault }')"
  run build::_encrypt_mode "${DOMAIN_DIR}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not supported"* ]]
}

@test "encrypt: invalid on is rejected" {
  write_spec "$(printf '  build:\n    encrypt: { on: sometimes }')"
  run build::_encrypt_mode "${DOMAIN_DIR}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"invalid"* ]]
}

@test "encrypt: unknown type fails the whole split build" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { type: vault }')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not supported"* ]]
}

# ── encrypt.on: change ───────────────────────────────────

@test "on:change: unchanged Secret is KEPT, not re-encrypted" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  write_artifact
  seed_prior_secret

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
  # It was decrypt-compared (decrypt called) but NOT re-encrypted.
  grep -q . "${SOPS_DECRYPT_LOG}"
  ! grep -q 'Secret.app.creds.sops.yaml' "${SOPS_ENCRYPT_LOG}"
}

@test "on:change: canonical compare — reordered/reformatted prior still unchanged" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  write_artifact
  seed_prior_secret reorder            # same values, different key order/style

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  ! grep -q 'Secret.app.creds.sops.yaml' "${SOPS_ENCRYPT_LOG}"
}

@test "on:change: kept file is BYTE-identical to the committed prior" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  write_artifact
  seed_prior_secret
  local before
  before=$(sha256sum "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" | cut -d' ' -f1)

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  local after
  after=$(sha256sum "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" | cut -d' ' -f1)
  [ "${before}" = "${after}" ]
}

@test "on:change: a CHANGED Secret IS re-encrypted" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  write_artifact                       # prior = hunter2
  seed_prior_secret
  # now the render changes the value
  write_artifact "Y2hhbmdlZA=="        # base64("changed")

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  grep -q 'Secret.app.creds.sops.yaml' "${SOPS_ENCRYPT_LOG}"
}

@test "on:change: a prior that can't be decrypted falls back to re-encrypt" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  # A sops-looking prior with NO stub_enc: → the stub's decrypt fails → fallback.
  printf 'sops:\n    unreadable: true\n' > "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  grep -q 'Secret.app.creds.sops.yaml' "${SOPS_ENCRYPT_LOG}"
  # and the result is a real (stub) ciphertext, not the unreadable prior
  grep -q 'stub_enc:' "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"
}

@test "on:change: no prior at all → encrypt (first build)" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  write_artifact
  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  grep -q 'Secret.app.creds.sops.yaml' "${SOPS_ENCRYPT_LOG}"
}

@test "on:change: a Secret dropped from the render is PRUNED in a full build" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: change }')"
  # render now has NO Secret, only the Deployment
  cat > "${DOMAIN_DIR}/artifacts.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: web, namespace: app }
spec: { replicas: 1 }
EOF
  mkdir -p "${DOMAIN_DIR}/artifacts"
  # a stale committed Secret twin that must NOT survive (it's gone from render)
  printf 'sops:\n    stub_enc: Zm9v\n' > "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ ! -e "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/Deployment.app.web.yaml" ]
}

# ── encrypt.on: always ───────────────────────────────────

@test "on:always: an UNCHANGED Secret is STILL re-encrypted (no compare)" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]\n  build:\n    encrypt: { on: always }')"
  write_artifact
  seed_prior_secret

  run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  # always mode never decrypt-compares; it re-encrypts regardless
  [ ! -s "${SOPS_DECRYPT_LOG}" ]
  grep -q 'Secret.app.creds.sops.yaml' "${SOPS_ENCRYPT_LOG}"
}

# ── lo build --no-secrets ────────────────────────────────

@test "--no-secrets: non-Secret resources emitted, Secret loop skipped" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  LOK8S_BUILD_NO_SECRETS=1 run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Deployment.app.web.yaml" ]
  # no Secret touched at all — neither encrypt nor decrypt ran
  [ ! -s "${SOPS_ENCRYPT_LOG}" ]
  [ ! -s "${SOPS_DECRYPT_LOG}" ]
}

@test "--no-secrets: committed Secret.*.sops.yaml is left BYTE-identical" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  seed_prior_secret
  local before
  before=$(sha256sum "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" | cut -d' ' -f1)

  LOK8S_BUILD_NO_SECRETS=1 run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
  local after
  after=$(sha256sum "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" | cut -d' ' -f1)
  [ "${before}" = "${after}" ]
  # not re-encrypted, not deleted
  [ ! -s "${SOPS_ENCRYPT_LOG}" ]
}

@test "--no-secrets: works WITHOUT recipients (no store/key required)" {
  # no spec.gitops.age at all — a --no-secrets CI build must not demand it
  write_spec "  build: { artifacts: split }"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  printf 'sops:\n    stub_enc: Zm9v\n' > "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"

  LOK8S_BUILD_NO_SECRETS=1 run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  # committed Secret survives; the missing-recipients hard-fail did NOT fire
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/Deployment.app.web.yaml" ]
}

@test "--no-secrets: a non-Secret dropped from the render is STILL pruned" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  echo "stale" > "${DOMAIN_DIR}/artifacts/Deployment.app.gone.yaml"
  printf 'sops:\n    stub_enc: Zm9v\n' > "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"

  LOK8S_BUILD_NO_SECRETS=1 run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  # the dropped non-Secret is pruned…
  [ ! -e "${DOMAIN_DIR}/artifacts/Deployment.app.gone.yaml" ]
  # …but the committed Secret is NOT (the guard)
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/Deployment.app.web.yaml" ]
}

@test "--no-secrets: env-owned lowercase files still survive" {
  write_spec "$(printf '  gitops:\n    provider: flux\n    age: [age1testkey]')"
  write_artifact
  mkdir -p "${DOMAIN_DIR}/artifacts"
  echo "env-owned" > "${DOMAIN_DIR}/artifacts/kustomization.yaml"
  printf 'sops:\n    stub_enc: Zm9v\n' > "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml"

  LOK8S_BUILD_NO_SECRETS=1 run build::split "${DOMAIN}"
  [ "$status" -eq 0 ]
  [ -f "${DOMAIN_DIR}/artifacts/kustomization.yaml" ]
  [ -f "${DOMAIN_DIR}/artifacts/Secret.app.creds.sops.yaml" ]
}

# ── build::_no_secrets — the flag/env precedence (the round-1 bug's home) ─────
# main::build is argsh-:args-gated and not unit-tested directly, so the effective
# --no-secrets resolution is factored into build::_no_secrets and pinned here.
# The round-1 fix was BROKEN precisely because NO test drove this path: the
# vendored argsh reports an absent `:+` flag as a literal "0", so a `${flag:-…}`
# fallback never fired and a pre-set env was clobbered (Case B).

@test "_no_secrets: flag ON wins → 1 (env 0 or 1)" {
  LOK8S_BUILD_NO_SECRETS=0 run build::_no_secrets 1; [ "$output" = 1 ]
  LOK8S_BUILD_NO_SECRETS=1 run build::_no_secrets 1; [ "$output" = 1 ]
}

@test "_no_secrets: flag OFF inherits a pre-set env=1 (Case B — the clobber bug)" {
  LOK8S_BUILD_NO_SECRETS=1 run build::_no_secrets 0
  [ "$output" = 1 ]
}

@test "_no_secrets: flag OFF + no env → 0" {
  unset LOK8S_BUILD_NO_SECRETS
  run build::_no_secrets 0
  [ "$output" = 0 ]
}

@test "_no_secrets: robust whether argsh yields \"\" or \"0\" for an absent flag" {
  # env set → inherit regardless of the flag's empty/0 form
  LOK8S_BUILD_NO_SECRETS=1 run build::_no_secrets "";  [ "$output" = 1 ]
  LOK8S_BUILD_NO_SECRETS=1 run build::_no_secrets 0;   [ "$output" = 1 ]
  # no env → 0 regardless of the flag's empty/0 form
  unset LOK8S_BUILD_NO_SECRETS
  run build::_no_secrets "";  [ "$output" = 0 ]
  run build::_no_secrets 0;   [ "$output" = 0 ]
}
