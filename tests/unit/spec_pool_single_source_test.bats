#!/usr/bin/env bats
# spec_pool_single_source_test.bats — spec.workers is read in exactly one place.
#
# Why this exists
# ---------------
# Three drivers (kkp, kubeone, capi) build machine deployments from
# `spec.workers`, and each carried its own copy of the same three facts:
# the pool-name regex, the bracket-yq read, and the iteration form. Issue #132
# filed the regex ("Low priority — the three copies are currently identical").
# They were not identical:
#
#   - `drivers/kubeone/config` read the AWS `ami` field through a DOTTED path,
#     `.spec.workers.${pool}.ami`. That is the exact bug the bracket comment two
#     lines above it warns about: yq parses `pool-1` as `pool - 1`, so a
#     hyphenated pool read nothing and every AWS worker silently lost its ami.
#   - `drivers/kkp/main` iterated with `for pool in $(yq …)`. Word-splitting
#     happens BEFORE validation, so a pool named `a b` arrives as two names that
#     each pass the regex on their own. The other two used `while read`.
#
# Both are what "one fact in three places" produces given enough time. The
# reader is now `.lok8s/utils/spec.sh` and this file is the gate that keeps a
# fourth copy from appearing — the drift half is as important as the extraction,
# because an extraction without one just resets the clock.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq not available"
  UTIL="${_PROJECT_ROOT}/.lok8s/utils/spec.sh"
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/spec.sh"

  SPEC="${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  cat > "${SPEC}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: test
spec:
  workers:
    pool-1:
      replicas: 3
      type: cx22
      autoscaler:
        min: 0
        max: 9
    plain:
      type: cx32
YAML
}

teardown() { teardown_tmpdir; }

# Everything under .lok8s that is shell, minus the util itself. Templates and
# YAML are excluded — they hold no reader.
_shell_sources() {
  find "${_PROJECT_ROOT}/.lok8s" -type f \
    ! -path '*/utils/spec.sh' \
    ! -path '*/init.d/*' \
    ! -name '*.yaml' ! -name '*.yml' ! -name '*.json' ! -name '*.ts' \
    -print0 \
    | xargs -0 grep -l '^#!/usr/bin/env \(argsh\|bash\)\|^# shellcheck shell=bash' 2>/dev/null
}

# ── the drift gate ────────────────────────────────────────

@test "the util owns the pool-name rule and the tree agrees" {
  # ANTI-VACUITY first: if the regex is not in the util, the sweep below would
  # pass over a repo that lost its single source rather than one that has one.
  grep -q '\[a-zA-Z0-9\]\[a-zA-Z0-9-\]\*\$' "${UTIL}" || {
    echo "the pool-name regex is no longer in ${UTIL} — this gate is measuring" >&2
    echo "nothing. Put the rule back, or repoint this test at its new home." >&2
    return 1
  }

  local hits
  hits=$(_shell_sources | xargs grep -n '\[a-zA-Z0-9\]\[a-zA-Z0-9-\]\*\$' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a second copy of the pool-name regex reappeared:" >&2
    echo "${hits}" >&2
    echo "Call spec::validate_pool_name instead (issue #132)." >&2
    return 1
  }
}

@test "no driver reads a pool field on its own" {
  # The bracket form. Its only correct home is the shared reader; a copy
  # elsewhere is a place the next hyphen bug can land.
  grep -q 'spec.workers.\["' "${UTIL}" || {
    echo "the bracket read is no longer in ${UTIL} — repoint this gate." >&2
    return 1
  }

  local hits
  hits=$(_shell_sources | xargs grep -n 'spec\.workers\.\[' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a private bracket read of spec.workers reappeared:" >&2
    echo "${hits}" >&2
    echo "Call spec::pool_field instead (issue #132)." >&2
    return 1
  }
}

@test "no dotted read of a pool field survives anywhere" {
  # The bug itself, not the duplication: `.spec.workers.${pool}.x` parses the
  # hyphen as subtraction. It must not exist in any file, util included.
  local hits
  hits=$(find "${_PROJECT_ROOT}/.lok8s" -type f ! -path '*/init.d/*' -print0 \
    | xargs -0 grep -n 'spec\.workers\.\${' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a dotted read of a pool field is back — yq parses a hyphenated pool" >&2
    echo "name as arithmetic and it silently reads nothing:" >&2
    echo "${hits}" >&2
    return 1
  }
}

@test "no driver iterates spec.workers on its own" {
  grep -q 'spec\.workers // {} | keys' "${UTIL}" || {
    echo "the keys iteration is no longer in ${UTIL} — repoint this gate." >&2
    return 1
  }

  local hits
  hits=$(_shell_sources | xargs grep -n 'spec\.workers[^"]*keys' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a private iteration of spec.workers reappeared:" >&2
    echo "${hits}" >&2
    echo "Call spec::pool_names / spec::pool_count instead (issue #132)." >&2
    return 1
  }
}

@test "every adopter feeds spec::pool_names to while-read, never to for-in" {
  # `for pool in $(spec::pool_names …)` word-splits before the name is
  # validated, which is what kkp did. The helper's docstring says so; this
  # makes it enforceable.
  local hits
  hits=$(_shell_sources | xargs grep -n 'for .* in \$(spec::pool_names' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "a pool loop word-splits the name list before validating it:" >&2
    echo "${hits}" >&2
    return 1
  }
}

# ── the reader's behaviour ────────────────────────────────

@test "spec::validate_pool_name accepts the names Kubernetes accepts" {
  run spec::validate_pool_name "pool-1"; [ "${status}" -eq 0 ]
  run spec::validate_pool_name "a"; [ "${status}" -eq 0 ]
  run spec::validate_pool_name "base-fsn1-cx53"; [ "${status}" -eq 0 ]
}

@test "spec::validate_pool_name rejects a leading hyphen, spaces and quoting" {
  run spec::validate_pool_name "-lead"; [ "${status}" -ne 0 ]
  run spec::validate_pool_name "a b"; [ "${status}" -ne 0 ]
  run spec::validate_pool_name 'a"]|.x'; [ "${status}" -ne 0 ]
  run spec::validate_pool_name ""; [ "${status}" -ne 0 ]
}

@test "spec::pool_field reads a HYPHENATED pool — the arithmetic trap" {
  # This is the regression the dotted form failed. `.spec.workers.pool-1.type`
  # reads as `.spec.workers.pool - 1.type` and yields nothing.
  run spec::pool_field "${SPEC}" "pool-1" type
  [ "${status}" -eq 0 ]
  [ "${output}" = "cx22" ]
}

@test "spec::pool_field reads a nested field" {
  run spec::pool_field "${SPEC}" "pool-1" autoscaler.max
  [ "${status}" -eq 0 ]
  [ "${output}" = "9" ]
}

@test "spec::pool_field returns 0 for a real zero, not the default" {
  # yq's `//` is a FALSY test, not a null test — the reason the default is
  # applied in bash. A pool that scales to zero must not read back as its
  # default.
  run spec::pool_field "${SPEC}" "pool-1" autoscaler.min 7
  [ "${status}" -eq 0 ]
  [ "${output}" = "0" ]
}

@test "spec::pool_field falls back to the default for a missing field" {
  run spec::pool_field "${SPEC}" "plain" replicas 1
  [ "${status}" -eq 0 ]
  [ "${output}" = "1" ]
}

@test "spec::pool_field returns empty (not 'null') with no default" {
  run spec::pool_field "${SPEC}" "plain" replicas
  [ "${status}" -eq 0 ]
  [ "${output}" = "" ]
}

@test "spec::pool_field refuses an invalid pool name before it reaches yq" {
  run spec::pool_field "${SPEC}" 'x"] | .metadata' type
  [ "${status}" -ne 0 ]
}

@test "spec::pool_field refuses a field path that is not a field path" {
  run spec::pool_field "${SPEC}" "plain" 'type | .. | keys'
  [ "${status}" -ne 0 ]
}

@test "spec::pool_names emits one name per line" {
  run spec::pool_names "${SPEC}"
  [ "${status}" -eq 0 ]
  [ "${#lines[@]}" -eq 2 ]
  [[ " ${lines[*]} " == *" pool-1 "* ]]
  [[ " ${lines[*]} " == *" plain "* ]]
}

@test "spec::pool_names and pool_count are quiet on a spec with no workers" {
  local bare="${BATS_TEST_TMPDIR}/bare.yaml"
  printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: t\n' > "${bare}"
  run spec::pool_count "${bare}"
  [ "${output}" = "0" ]
  run spec::pool_names "${bare}"
  [ "${output}" = "" ]
}

@test "spec::pool_count counts the pools" {
  run spec::pool_count "${SPEC}"
  [ "${output}" = "2" ]
}
