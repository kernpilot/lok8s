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

  local path_clusters="${PATH_CLUSTERS:-${PATH_BASE:-}/clusters}"
  local active=""
  if [[ -f "${path_clusters}/.active" ]]; then
    active=$(<"${path_clusters}/.active")
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

# domain::driver — the driver a domain's cluster spec declares, lowercased
# (`kind: Lo` → lo, `kind: KubeOne` → kubeone …). Deploy-only domains
# (deploy.lok8s.yaml, no cluster spec) print "deploy"; a missing domain dir
# prints "" and returns 1. Mirrors provision::dispatch's routing field.
domain::driver() {
  local domain="${1}"
  local path_clusters="${PATH_CLUSTERS:-${PATH_BASE:-}/clusters}"
  local cluster_yaml="${path_clusters}/${domain}/cluster.lok8s.yaml"
  if [[ ! -f "${cluster_yaml}" ]]; then
    if [[ -f "${path_clusters}/${domain}/deploy.lok8s.yaml" ]]; then
      echo "deploy"
      return 0
    fi
    echo ""
    return 1
  fi
  # No `yq | tr` pipeline: without pipefail a yq failure (unreadable spec,
  # invalid YAML, missing yq) would be masked by tr's rc 0 and "succeed"
  # with an empty driver — confusing every downstream consumer.
  local kind
  kind=$(yq -r '.kind // ""' "${cluster_yaml}") || kind=""
  if [[ -z "${kind}" ]]; then
    echo "" && return 1
  fi
  echo "${kind,,}"
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
