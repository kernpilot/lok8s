# shellcheck shell=bash
# template.sh — YAML template rendering via envsubst
# Exports variables from a cluster spec YAML and renders template files.

import ^utils/verbose

# Render a template file by exporting vars from a cluster spec YAML.
# Usage: template::render <template_file> <cluster_yaml>
# Outputs rendered YAML to stdout.
template::render() {
  local template="${1}" cluster_yaml="${2}"

  [[ -f "${template}" ]] || { error "Template not found: ${template}"; return 1; }
  [[ -f "${cluster_yaml}" ]] || { error "Cluster spec not found: ${cluster_yaml}"; return 1; }

  # Run in subshell so exports don't leak to caller's scope
  (
    export CLUSTER_NAME CLUSTER_NAMESPACE CLUSTER_DOMAIN K8S_VERSION
    CLUSTER_NAME=$(yq -r '.metadata.name' "${cluster_yaml}")
    CLUSTER_NAMESPACE=$(yq -r '.spec.cluster.namespace // "default"' "${cluster_yaml}")
    CLUSTER_DOMAIN=$(yq -r '.spec.cluster.domain' "${cluster_yaml}")
    K8S_VERSION=$(yq -r '.spec.kubernetes.version' "${cluster_yaml}")

    envsubst < "${template}"
  )
}

# Render all template files in a directory, concatenated with --- separators.
# Usage: template::render_dir <dir> <cluster_yaml>
template::render_dir() {
  local dir="${1}" cluster_yaml="${2}"
  local first=1

  [[ -d "${dir}" ]] || { error "Template directory not found: ${dir}"; return 1; }

  for tmpl in "${dir}"/*.yaml; do
    [[ -f "${tmpl}" ]] || continue
    if (( first )); then
      first=0
    else
      echo "---"
    fi
    template::render "${tmpl}" "${cluster_yaml}"
  done
}

# Build an envsubst whitelist string from current LOK8S_SPEC_* and LOK8S_USER_* vars.
# Usage: template::envsubst_whitelist
# Outputs: string like "${LOK8S_SPEC_FOO} ${LOK8S_USER_BAR} ..."
template::envsubst_whitelist() {
  env | awk -F= '/^LOK8S_(SPEC|USER)_/ {printf "${%s} ", $1}'
}

# ── envsubst flavor shim ─────────────────────────────────────────────────────
# Everywhere lok8s renders manifests it restricts substitution to an explicit
# variable list, so arbitrary `$…` in user content survives untouched. GNU
# gettext envsubst takes that list as one SHELL-FORMAT positional arg
# ('${A} ${B}'). Every other flavor is UNFIT for filtering a manifest stream:
# renvsubst (the envsubst `b` curates) rejects the positional arg outright,
# and even with --variable filters its expression parser chokes on shell
# embedded in ConfigMaps — `${arr[0]}` hard-fails the whole pipe and
# `${V:?required}` is silently rewritten to `${V}` (content corruption) even
# when V is not in the filter. And no GitHub-releases alternative fits either:
# a8m/envsubst has no whitelist (and empties unset refs), drone/envsubst is a
# library. So template::envsubst uses GNU envsubst when that's what's on
# PATH, and otherwise substitutes NATIVELY — no envsubst needed at all for
# the restricted path. Only bare substitute-everything `envsubst < file`
# sites (framework-owned templates, no exotic `${…}`) touch the binary
# directly.

# Flavor is per-process stable — detect once, cache.
declare -g _TEMPLATE_ENVSUBST_FLAVOR=""

# Usage: template::envsubst_flavor  → echoes "gnu" or "other"
template::envsubst_flavor() {
  if [[ -z "${_TEMPLATE_ENVSUBST_FLAVOR}" ]]; then
    if envsubst --version 2>/dev/null | grep -q "GNU gettext"; then
      _TEMPLATE_ENVSUBST_FLAVOR="gnu"
    else
      _TEMPLATE_ENVSUBST_FLAVOR="other"
    fi
  fi
  echo "${_TEMPLATE_ENVSUBST_FLAVOR}"
}

# Substitute stdin→stdout restricted to the vars named in a GNU SHELL-FORMAT
# string. An empty/whitespace-only list replaces NOTHING (GNU semantics).
# GNU envsubst handles it when present; otherwise a native jq pass replaces
# exactly the literal `${NAME}` and boundary-delimited `$NAME` tokens of the
# listed vars (listed-but-unset → empty string, like GNU) and guarantees every
# other byte of the stream — including `${arr[0]}`/`${x:?}` shell in embedded
# scripts — passes through untouched.
# Usage: … | template::envsubst '${VAR_A} ${VAR_B} '
template::envsubst() {
  local shell_format="${1:-}"
  local -a vars=()
  local tok
  # Word-splitting the format string IS the parse: '${A} ${B}' → A B.
  # shellcheck disable=SC2086
  for tok in ${shell_format}; do
    tok="${tok#\$}"
    tok="${tok#\{}"
    tok="${tok%\}}"
    [[ -z "${tok}" ]] || vars+=("${tok}")
  done
  if (( ${#vars[@]} == 0 )); then
    cat
  elif [[ "$(template::envsubst_flavor)" == "gnu" ]]; then
    envsubst "${shell_format}"
  else
    # jq, deliberately not awk/sed/perl: jq has exactly ONE implementation
    # (b-pinned in the toolchain, required below by lo anyway) while awk
    # forks (gawk/mawk/busybox) disagree on regex details. Var names are
    # [A-Za-z0-9_] so they are regex-safe verbatim; the replacement value is
    # a jq string variable and therefore literal (no `&`/backref surprises);
    # the lookahead is the identifier boundary GNU applies to unbraced $NAME
    # ($FOO must not fire inside $FOOBAR). Unset/empty → "" like GNU (jq's
    # // only triggers on null, so a set-but-empty value stays "").
    jq -Rr --arg names "${vars[*]}" '
      ($names | split(" ") | map(select(length > 0))) as $ns
      | reduce $ns[] as $n (.;
          gsub("\\$\\{" + $n + "\\}"; ($ENV[$n] // ""))
          | gsub("\\$" + $n + "(?![A-Za-z0-9_])"; ($ENV[$n] // "")))
    '
  fi
}

# template::descriptor_json <file>
# Read a cluster descriptor (JSON or YAML), expand ${VARS}, emit JSON.
#
# The point is the YAML branch. The pattern this replaces was:
#
#     desc=$(envsubst < "${file}")
#     echo "${desc}" | jq empty || desc=$(yq -o json '.' "${file}")
#                                                        ^^^^^^^^
# — on the YAML path it re-read the RAW file and silently threw the expansion
# away, so every ${VAR} reached the provider unexpanded. Five sites in the
# hetzner provider did this (issue #65); the identical bug in the kubeone
# inventory was fixed inline earlier, which is why this is a shared helper now
# rather than a sixth copy.
#
# yq reads the EXPANDED TEXT from stdin. Bare `envsubst < file` is the
# substitute-everything form, which is the only invocation the toolchain's
# renvsubst also accepts (see template::envsubst for the whitelisted case).
template::descriptor_json() {
  local file="${1}"
  [[ -f "${file}" ]] || { error "descriptor not found: ${file}"; return 1; }

  local expanded
  expanded="$(envsubst < "${file}")" || {
    error "failed to expand variables in ${file}"
    return 1
  }

  if printf '%s' "${expanded}" | jq empty 2>/dev/null; then
    printf '%s' "${expanded}"
    return 0
  fi

  printf '%s' "${expanded}" | yq -o json '.' - || {
    error "descriptor ${file} is neither valid JSON nor valid YAML (after expansion)"
    return 1
  }
}
