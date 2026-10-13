# shellcheck shell=bash
# domain.sh — the ONE place domain resolution + driver identity live.
#
# `lo` main resolves the active domain exactly once (domain::resolve) and
# every subcommand/lib inherits it — via the argsh `'domain|:^~domain'` arg
# (dynamic scoping hands them main's `domain` local as the default) or the
# re-exported DOMAIN_NAME. Nothing below main may re-derive the domain from
# `.active` on its own; that duplication is how `lo registry up` once ran
# against a KubeOne spec while DOMAIN_NAME pointed at a kind cluster.

# domain::resolve — resolve the active domain with the canonical precedence:
#
#   explicit value (--domain flag) > DOMAIN_NAME env > clusters/.active
#     > lok8s.dev (the framework's default slot)
#
# The env var outranks `.active` deliberately: exporting DOMAIN_NAME is a
# more explicit act than the state `lo use` persisted earlier — same layering
# as KUBECONFIG vs a kubectl context. When BOTH are set and disagree, a
# one-line notice goes to stderr so neither interactive nor scripted callers
# are surprised by which one won.
#
# The terminal lok8s.dev default lives HERE, at the end of the chain — a
# script-level `: "${DOMAIN_NAME:=lok8s.dev}"` (the old home) is
# indistinguishable from user-set env and would permanently outrank
# `.active`. Never fails: an unreadable/invalid `.active` warns and is
# ignored; validation is the consumer's job (e.g. to::domain).
domain::resolve() {
  local explicit="${1:-}"
  if [[ -n "${explicit}" ]]; then
    echo "${explicit}"
    return 0
  fi

  # `.` fallback keeps a bare (PATH_BASE-less) sourcing repo-relative — an
  # empty expansion would consult the system root's /clusters.
  local path_clusters="${PATH_CLUSTERS:-${PATH_BASE:-.}/clusters}"
  local active=""
  if [[ -f "${path_clusters}/.active" ]]; then
    # Guarded read: an unreadable .active must warn-and-fall-back, not abort
    # the caller under set -e ("never fails" is this function's contract).
    active=$(cat "${path_clusters}/.active" 2>/dev/null) || {
      echo "warning: clusters/.active exists but is unreadable, ignoring" >&2
      active=""
    }
    if [[ -n "${active}" && ! "${active}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]]; then
      echo "warning: invalid domain in clusters/.active, ignoring" >&2
      active=""
    fi
  fi

  local env_domain="${DOMAIN_NAME:-}"
  if [[ -n "${env_domain}" ]]; then
    if [[ -n "${active}" && "${active}" != "${env_domain}" ]]; then
      echo "notice: using DOMAIN_NAME=${env_domain} (env); the active domain is '${active}' — pass --domain or unset DOMAIN_NAME to switch" >&2
    fi
    echo "${env_domain}"
    return 0
  fi

  echo "${active:-lok8s.dev}"
}

# domain::spec_driver <cluster_spec> [fallback]
#
# THE reader for "which driver does this cluster spec declare". Thirteen sites
# used to run their own `yq -r '.kind' | tr`, and the private copies disagreed
# on all three of the things that matter (issue #89):
#
#   - the missing-key coercion. Without `// ""` yq prints the literal string
#     "null", which is non-empty and passes any `[[ -n … ]]` test. That is how
#     `lo down` routed a kind-less spec to driver-destroy.
#   - the shape guard. The value is interpolated into `drivers/<kind>/main`,
#     which is then sourced. Two sites checked `^[a-z][a-z0-9]*$`; the rest
#     took whatever the file said.
#   - the failure mode. `yq | tr` has no pipefail here, so tr's rc 0 masks an
#     unreadable spec and the caller reads an empty driver instead of an error.
#
# Echoes the lowercased kind and returns 0. Otherwise:
#   rc 1 — nothing to read: no file, unreadable file, key absent or empty.
#          With a fallback given, the fallback is echoed and the rc is 0.
#   rc 2 — `.kind` is present but is not a bare driver name. NEVER defaulted:
#          the value reaches a path we source, and quietly replacing a crafted
#          one with "lo" would hide the crafting.
domain::spec_driver() {
  local spec="${1}" fallback="${2-}"
  # No `-f` pre-check: a missing file and an unreadable one are the same answer
  # (rc 1, or the fallback), and yq already reports both. No `yq | tr` pipeline
  # either — without pipefail a yq failure would be masked by tr's rc 0 and
  # "succeed" with an empty driver. Bash's `,,` lowercases in-process.
  local kind
  kind=$(yq -r '.kind // ""' "${spec}" 2>/dev/null) || kind=""
  kind="${kind,,}"
  if [[ -z "${kind}" ]]; then
    [[ -n "${fallback}" ]] || return 1
    echo "${fallback}"
    return 0
  fi
  [[ "${kind}" =~ ^[a-z][a-z0-9]*$ ]] || return 2
  echo "${kind}"
}

# domain::spec_layout <spec_file> — the document layout of a spec file (Go
# twin: domain.SpecLayout in internal/domain/layout.go).
#
# The two implementations read a spec file in different ways when it has more
# than one YAML document: the Go readers decode the first document (yaml.v3
# counts an empty one before it), the bash readers run yq on every document
# (yq drops empty documents before the first one, keeps the ones after it, and
# prints one line per document). The same file then reads as two different
# specs. Only one layout reads the same in both: one document, with an
# optional `---` before it and an optional `...` after it. `lo provision`,
# `lo lint` and the spec.kubernetes.version reader refuse every other layout.
#
# Returns 0 for that layout: no content before a `---` line, no second `---`
# line, no content after a `...` line (blank lines, comments and `%`
# directives are not content). Otherwise prints "<line> <marker>" (the 1-based
# line and the marker, `---` or `...`, that breaks the layout) and returns 1.
# A file that cannot be read returns 0: the readers report that themselves.
domain::spec_layout() {
  local spec="${1}" text rest n=0 content=0 started=0 end_line=0
  [[ -r "${spec}" ]] || return 0
  while IFS= read -r text || [[ -n "${text}" ]]; do
    n=$(( n + 1 ))
    text="${text%$'\r'}"
    if [[ "${text}" =~ ^(---|\.\.\.)([[:blank:]](.*))?$ ]]; then
      if [[ "${BASH_REMATCH[1]}" == "..." ]]; then
        (( end_line )) || end_line=${n}
        continue
      fi
      if (( content || started || end_line )); then
        printf '%s ---\n' "${n}"
        return 1
      fi
      started=1
      rest="${BASH_REMATCH[3]}"
      rest="${rest#"${rest%%[![:blank:]]*}"}"
      [[ -z "${rest}" || "${rest}" == "#"* ]] || content=1
      continue
    fi
    rest="${text#"${text%%[![:blank:]]*}"}"
    [[ -z "${rest}" || "${rest}" == "#"* || "${text}" == "%"* ]] && continue
    if (( end_line )); then
      printf '%s ...\n' "${end_line}"
      return 1
    fi
    content=1
  done < "${spec}"
  return 0
}

# domain::driver — the driver a domain's cluster spec declares, lowercased
# (`kind: Lo` → lo, `kind: KubeOne` → kubeone …). Deploy-only domains
# (deploy.lok8s.yaml, no cluster spec) print "deploy"; a missing domain dir
# prints "" and returns 1. Mirrors provision::dispatch's routing field.
domain::driver() {
  local domain="${1}"
  local path_clusters="${PATH_CLUSTERS:-${PATH_BASE:-.}/clusters}"
  local cluster_yaml="${path_clusters}/${domain}/cluster.lok8s.yaml"
  if [[ ! -f "${cluster_yaml}" ]]; then
    if [[ -f "${path_clusters}/${domain}/deploy.lok8s.yaml" ]]; then
      echo "deploy"
      return 0
    fi
    echo ""
    return 1
  fi
  local kind
  kind=$(domain::spec_driver "${cluster_yaml}") || { echo ""; return 1; }
  echo "${kind}"
}

# domain::require_driver — gate a driver-specific operation on the resolved
# domain actually using that driver. Fails with an ACTIONABLE message naming
# the mismatch instead of letting the operation die three layers down on a
# spec field the other driver never has (the "spec.network.name is required"
# class of confusion).
#
#   domain::require_driver lo "${domain}" "registry management"
domain::require_driver() {
  local want="${1}" domain="${2}" what="${3:-this operation}"
  local got
  got=$(domain::driver "${domain}") || {
    # Covers both a missing domain dir and a dir whose spec is unreadable —
    # "not found" alone misled when the directory existed but had no spec.
    echo "error: domain '${domain}' has no readable cluster/deploy spec under clusters/ — cannot run ${what}" >&2
    return 1
  }
  if [[ "${got}" != "${want}" ]]; then
    echo "error: domain '${domain}' uses the '${got}' driver — ${what} is a '${want}'-driver (local cluster) feature." >&2
    echo "       Pass --domain <a-${want}-domain> or switch with 'lo use <domain>'." >&2
    return 1
  fi
}
