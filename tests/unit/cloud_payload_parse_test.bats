#!/usr/bin/env bats
# cloud_payload_parse_test.bats — every file cloud-init writes to a VM must be
# syntactically valid BEFORE it ships.
#
# Why this exists
# ---------------
# write_files payloads are copied verbatim onto a machine and run at first boot.
# A syntax error in one is discovered at the most expensive possible moment: on
# a real server, mid-provision, with no shell to debug it. This is not
# hypothetical — the #67 brace-sweep corrupted embedded script bodies, which is
# precisely the class a parse check catches for free.
#
# cloud_config_test.bats covers the ASSEMBLY (which modules emit, de-dup, target
# path resolution). Nothing checked the payload BODIES.
#
# Discovery is by shebang, not by extension: a payload named `foo` with
# `#!/bin/bash` is just as dangerous as one named `foo.sh`, and a hand-maintained
# list is the thing that goes stale. The whole providers tree is scanned so a new
# provider's payloads are covered the day they land.

setup() {
  load "../test_helper"
  PROVIDERS="${_PROJECT_ROOT}/.lok8s/providers"
}

# Every regular file under any write_files/ directory.
_payloads() {
  find "${PROVIDERS}" -path '*/write_files/*' -type f 2>/dev/null | sort
}

@test "every embedded shell payload parses with its OWN interpreter" {
  local checked=0 bad=()
  while IFS= read -r f; do
    [[ -n "${f}" ]] || continue
    local first; first="$(head -1 "${f}")"
    [[ "${first}" =~ ^#! ]] || continue

    # Resolve the interpreter the payload DECLARES. Checking a bash script with
    # `sh -n` is a different language: it accepts and rejects different things,
    # so a bash-only construct could pass here and fail on the machine. (Naive
    # matching gets this wrong in a way that looks fine — "bash" ends in "sh".)
    local interp
    interp="$(printf '%s' "${first}" | sed -E 's|^#!\s*||; s|^/usr/bin/env\s+||; s|\s.*$||')"
    interp="${interp##*/}"
    case "${interp}" in
      bash|sh|dash|ksh|zsh) ;;
      *) continue ;;   # not a shell payload (python, etc.) — out of scope here
    esac

    command -v "${interp}" >/dev/null 2>&1 || {
      # Refuse rather than skip: an absent interpreter means this payload went
      # unchecked, and a green run must not mean that.
      bad+=("${f} (interpreter '${interp}' not available to check with)")
      continue
    }
    "${interp}" -n "${f}" 2>/dev/null || bad+=("${f} (${interp} -n failed)")
    checked=$((checked + 1))
  done < <(_payloads)

  # A glob that matches nothing must fail, not pass silently — that is how a
  # gate ends up guarding an empty set.
  [ "${checked}" -ge 3 ] || {
    echo "only ${checked} shell payload(s) found under ${PROVIDERS} — expected at least 3; the tree moved or discovery broke" >&2
    return 1
  }
  [ "${#bad[@]}" -eq 0 ] || {
    printf 'unparseable cloud-init payload: %s\n' "${bad[@]}" >&2
    return 1
  }
}

@test "every embedded JSON payload parses" {
  # daemon.json is written straight into /etc/docker: a corrupted one leaves the
  # node without a container runtime.
  local checked=0 bad=()
  while IFS= read -r f; do
    [[ "${f}" == *.json ]] || continue
    python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "${f}" 2>/dev/null \
      || bad+=("${f}")
    checked=$((checked + 1))
  done < <(_payloads)

  [ "${checked}" -ge 1 ] || {
    echo "no JSON payloads found under ${PROVIDERS} — expected at least 1" >&2
    return 1
  }
  [ "${#bad[@]}" -eq 0 ] || {
    printf 'unparseable JSON payload: %s\n' "${bad[@]}" >&2
    return 1
  }
}
