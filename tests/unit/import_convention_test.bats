#!/usr/bin/env bats
# import_convention_test.bats — every argsh import in .lok8s uses the `^` prefix.
#
# Why this exists
# ---------------
# POST-REVIEW finding 8 (issue #2) reported "import prefix usage is inconsistent
# across libs (`^libs/...` vs relative)" and counted 76 prefixed against 47
# relative. Re-counting closed it: the bash tree is already uniform. All 139
# argsh imports use `^`, and the "relative" ones are TypeScript ESM imports in
# `.lok8s/libs/init.d/test/` — the Playwright scaffolding lok8s writes into a
# user's project. Those are a different language with a different module system
# and MUST stay relative.
#
# So the decision was to keep the convention and make it enforceable rather than
# rewrite 123 import lines. A mechanical rewrite carries real regression risk —
# `^` resolves against PATH_SCRIPTS while a bare path resolves against the
# importing file, and the two differ for anything sourced outside the
# `lo` entrypoint — for no functional gain. AGENTS.md states the rule; this
# test is what makes it hold.

setup() {
  load "../test_helper"
}

# Bash/argsh sources under .lok8s come from shell_sources in
# tests/test_helper.bash — the one copy of that predicate. Three test files
# each carried their own before, in two regex dialects and with differing
# exclusions, which is the wrong direction for a change about single-sourcing
# facts.

@test "the framework really does use argsh imports" {
  # ANTI-VACUITY. The gate below asserts an ABSENCE; without this it would pass
  # just as happily over a tree with no imports at all, or a broken file sweep.
  local count
  count=$(shell_sources | xargs grep -c '^import \^' 2>/dev/null \
    | awk -F: '{ n += $NF } END { print n + 0 }')
  [ "${count}" -ge 100 ] || {
    echo "only ${count} '^'-prefixed imports found — the sweep is broken, not" >&2
    echo "the convention." >&2
    return 1
  }
}

@test "no argsh import omits the ^ prefix" {
  # `^` resolves against PATH_SCRIPTS; a bare path resolves against the
  # importing file. Both work from `lo`, which is why a mixed tree goes
  # unnoticed until something is sourced from elsewhere.
  shell_sources >/dev/null   # `|| true` below would swallow an empty sweep
  local hits
  hits=$(shell_sources | xargs grep -nE '^[[:space:]]*import[[:space:]]+[^^[:space:]]' 2>/dev/null || true)
  [ -z "${hits}" ] || {
    echo "these imports are not '^'-prefixed:" >&2
    echo "${hits}" >&2
    echo "The convention is 'import ^libs/foo' / 'import ^utils/bar' —" >&2
    echo "see AGENTS.md. (issue #2, finding 8)" >&2
    return 1
  }
}

# ── the other half: an import that is MISSING, not mis-spelled ───────────────
#
# The two gates above only look at the imports a file HAS. Nine libs called
# domain::spec_driver with no `import ^utils/domain` at all and nothing noticed,
# because the two ways of noticing were both blind:
#
#   - at runtime, `lo` imports utils/domain via utils/types and every lib is
#     sourced into that one process, so an ambient definition is always there;
#   - in the suite, tests/test_helper.bash sources utils/spec.sh and
#     utils/domain.sh globally, so no test can fail on a missing import either.
#
# That global sourcing STAYS — a test that sources a lib stubs `import` first,
# so the lib's own `import ^utils/domain` is a no-op and every driver test would
# otherwise die on an undefined helper. It is a test-harness convenience, not a
# statement about the tree. This gate is what makes the statement.
#
# The namespaces this gate covers are DERIVED from `.lok8s/utils/*.sh`, not
# listed. The first version listed two of them — domain and spec — which made
# the gate a hand-kept promise about which facts are single-sourced: exactly the
# defect class the change it belongs to exists to remove. Sweeping the other
# nine namespaces turned up eleven real callers with no declaration of their
# own, in capi, kubeone, the lo driver's utils, libs/status, libs/kubehz/hosted
# and providers/hetzner.
#
# Derivation, per `utils/<base>.sh`: the namespaces it DEFINES (`^ns::fn()`) map
# to `import ^utils/<base>`. Reading the definitions rather than the filename is
# what makes `utils/types.sh` (which defines `to::`) come out right, and what
# makes `utils/verbose.sh` (which defines plain `error`/`warn`/`debug`, no
# namespace) drop out on its own.
_util_namespaces() {
  local file base ns
  for file in "${_PROJECT_ROOT}"/.lok8s/utils/*.sh; do
    base="$(basename "${file}" .sh)"
    while IFS= read -r ns; do
      [[ -n "${ns}" ]] || continue
      _is_interface_namespace "${ns}" && continue
      printf '%s|utils/%s|utils/%s.sh\n' "${ns}" "${base}" "${base}"
    done < <(grep -oE '^[a-zA-Z0-9_]+::[a-zA-Z0-9_]+\(\)' "${file}" \
      | sed 's/::.*//' | sort -u)
  done
}

# A namespace that is a PLUGIN INTERFACE rather than a single-sourced util.
# `provider::` is declared partly in `utils/provider.sh` (detect/load/…) and
# implemented per backend in `providers/<name>/main`, which `provider::detect`
# sources at run time. Callers guard with `declare -F provider::provision`
# precisely because no static import can promise the symbol. Requiring one
# would be wrong, not merely noisy.
#
# The exemption is asserted below, so it cannot rot into a way of quietly
# dropping a namespace from the gate.
_is_interface_namespace() {
  [[ "${1}" == provider ]]
}

@test "the declared interface namespaces really are implemented outside their util" {
  # Guards the exemption above. If provider:: ever became an ordinary util —
  # defined only in utils/provider.sh — this exemption would be silently
  # excusing every caller of it.
  local ns=provider hits
  hits=$(shell_sources | xargs grep -lE "^${ns}::[a-zA-Z0-9_]+\(\)" 2>/dev/null \
    | grep -v "utils/${ns}.sh" || true)
  [ -n "${hits}" ] || {
    echo "'${ns}' is exempted from the missing-import gate as a plugin" >&2
    echo "interface, but nothing outside utils/${ns}.sh implements it any" >&2
    echo "more. Drop the exemption in _is_interface_namespace." >&2
    return 1
  }
}

@test "every lib that calls a shared util's helpers imports it" {
  local -a missing=()
  local -a seen=()
  local ns import_path definer file rel
  while IFS='|' read -r ns import_path definer; do
    [[ -n "${ns}" ]] || continue
    while IFS= read -r file; do
      rel="${file#"${_PROJECT_ROOT}/"}"
      # A file that DEFINES the namespace does not import it — that covers the
      # util itself and any file extending the same namespace.
      grep -qE "^${ns}::[a-zA-Z0-9_]+\(\)" "${file}" && continue
      # Full-line comments are prose, not calls (several libs name
      # domain::spec_driver in a comment explaining why they do NOT call it).
      grep -vE '^[[:space:]]*#' "${file}" \
        | grep -qE "(^|[^[:alnum:]_:])${ns}::[a-z_]" || continue
      seen+=("${rel}:${ns}")
      grep -qE "^[[:space:]]*import[[:space:]]+\^${import_path}[[:space:]]*$" "${file}" \
        && continue
      # A direct `source "${PATH_LOK8S}/utils/<base>.sh"` satisfies the rule
      # too, and is what drivers/lo/main and drivers/lo/libs/registry do (they
      # carry a `# shellcheck source=` line with it, which `import` would lose).
      # The defect is depending on SOMEONE ELSE having loaded the util; naming
      # the file yourself is not that.
      grep -qE "^[[:space:]]*source[[:space:]].*${definer//./\\.}" "${file}" \
        && continue
      missing+=("${rel} calls ${ns}:: without 'import ^${import_path}'")
    done < <(shell_sources)
  done < <(_util_namespaces)

  # ANTI-VACUITY: this gate asserts an ABSENCE, so it has to prove it actually
  # examined callers, over more than one namespace. 100 pairs today across 11
  # namespaces; a derivation that collapsed to one util would trip this.
  local -a namespaces=()
  mapfile -t namespaces < <(_util_namespaces | cut -d'|' -f1 | sort -u)
  [ "${#namespaces[@]}" -ge 8 ] || {
    echo "only ${#namespaces[@]} util namespace(s) derived from .lok8s/utils —" >&2
    echo "the derivation in _util_namespaces is broken, not the tree." >&2
    return 1
  }
  [ "${#seen[@]}" -ge 40 ] || {
    echo "the sweep found only ${#seen[@]} caller(s) across ${#namespaces[@]}" >&2
    echo "namespaces — it is broken, not the tree." >&2
    return 1
  }

  [ "${#missing[@]}" -eq 0 ] || {
    printf '%s\n' "${missing[@]}" >&2
    echo "Relying on 'lo' having imported it first works until the lib is" >&2
    echo "sourced from anywhere else — see AGENTS.md." >&2
    return 1
  }
}

@test "AGENTS.md states the convention this test enforces" {
  # A gate with no written rule is a trap: the next contributor learns the rule
  # from a red CI run instead of the guide.
  grep -q 'import \^' "${_PROJECT_ROOT}/AGENTS.md" || {
    echo "AGENTS.md no longer documents the '^' import prefix." >&2
    return 1
  }
}
