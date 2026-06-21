# shellcheck shell=bash
# services.sh — Cluster service setup (CoreDNS, TLS)

lo::coredns() {
  local domain="$1"
  local coredns_dir="${PATH_LOK8S}/drivers/lo/cluster/coredns"
  local cluster_yaml="${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
  local cluster_name kubeconfig
  cluster_name=$(yq -r '.metadata.name' "${cluster_yaml}")
  kubeconfig="${PATH_BASE}/.kubeconfig/${cluster_name}.yaml"

  kubectl apply --kubeconfig "${kubeconfig}" -f "${coredns_dir}/corefile.yaml"
  kubectl apply --kubeconfig "${kubeconfig}" -f "${coredns_dir}/expose.yaml"

  # Pin coredns-external to the LAST loadBalancer.pool IP so it does not race the
  # ingress/Envoy gateway for pool[0]. coredns-external is created HERE — before
  # the metallb bootstrap addon — so without a pin metallb later hands it the
  # first free pool IP (pool[0]); a gateway pinned to pool[0] (the convention,
  # and what spec.coredns `target: gateway` resolves to) then cannot allocate it
  # ("address already in use by coredns-external") → gateway stuck <pending> →
  # nothing serves. Setting the annotation now (pre-metallb) makes metallb honor
  # it on first allocation. Only meaningful for a range pool.
  local _pool
  _pool=$(yq -r '.spec.loadBalancer.pool // ""' "${cluster_yaml}")
  if [[ "${_pool}" == *-* ]]; then
    kubectl annotate svc coredns-external -n kube-system --kubeconfig "${kubeconfig}" \
      "metallb.universe.tf/loadBalancerIPs=${_pool##*-}" --overwrite
  fi

  # Per-cluster custom CoreDNS from spec.coredns — loaded into the
  # `coredns-custom` ConfigMap, imported by the Corefile from /etc/coredns/custom
  # (see corefile.yaml + patch.json). Declarative + committed, survives `lo up`.
  lo::coredns_custom "${domain}" "${cluster_yaml}" "${kubeconfig}"

  kubectl patch deployment coredns -n kube-system \
    --kubeconfig "${kubeconfig}" \
    --type json \
    --patch-file "${coredns_dir}/patch.json" 2>/dev/null || true

  kubectl rollout restart deployment/coredns -n kube-system --kubeconfig "${kubeconfig}"
}

# Build the coredns-custom ConfigMap from spec.coredns in cluster.lok8s.yaml.
# All three inputs compose (and are optional); nothing configured -> no ConfigMap
# (the Corefile's `import custom/*` is then a no-op):
#   spec.coredns.hosts[]   {name,target} -> a generated server block resolving the
#                          zone `name` (its apex + every *.name) to `target`:
#                          A -> target, AAAA -> NODATA (no dual-stack SERVFAIL),
#                          other types forwarded. target "gateway" resolves to the
#                          first spec.loadBalancer.pool IP (where the gateway pins
#                          by convention) — so the IP isn't duplicated by hand.
#   spec.coredns.servers   raw CoreDNS server block(s), inline (a *.server file)
#   spec.coredns.overrides raw directives merged into the default .:53 block
#   spec.coredns.import    path (relative to the cluster dir; default ./coredns)
#                          to a dir of raw *.server / *.override files
# Do NOT define the same zone via both hosts and a raw server/import (CoreDNS
# rejects duplicate zone blocks).
lo::coredns_custom() {
  local domain="$1" cluster_yaml="$2" kubeconfig="$3"
  local tmp; tmp=$(mktemp -d)
  # NOTE: no `trap ... RETURN` for cleanup — a RETURN trap is NOT function-local
  # without `set -o functrace`, so it leaks and re-fires when the CALLER returns,
  # where ${tmp} is out of scope → "unbound variable" under set -u. Clean up
  # explicitly at the end instead.

  # gateway shorthand = first IP of the LB pool ("a-b" -> "a")
  local pool gateway_ip
  pool=$(yq -r '.spec.loadBalancer.pool // ""' "${cluster_yaml}")
  gateway_ip="${pool%%-*}"

  # (1) structured hosts -> generated server blocks
  local count i name target
  count=$(yq -r '(.spec.coredns.hosts // []) | length' "${cluster_yaml}" 2>/dev/null || echo 0)
  for ((i = 0; i < count; i++)); do
    name=$(yq -r ".spec.coredns.hosts[${i}].name" "${cluster_yaml}")
    target=$(yq -r ".spec.coredns.hosts[${i}].target" "${cluster_yaml}")
    if [[ "${target}" == "gateway" ]]; then target="${gateway_ip}"; fi
    cat >"${tmp}/host-${i}.server" <<EOF
${name}:53 {
    errors
    template IN A {
        match ".*"
        answer "{{ .Name }} 30 IN A ${target}"
    }
    template IN AAAA {
        match ".*"
        rcode NOERROR
    }
    forward . /etc/resolv.conf
    cache 30
}
EOF
  done

  # (2) raw inline servers / overrides
  local servers overrides
  servers=$(yq -r '.spec.coredns.servers // ""' "${cluster_yaml}")
  if [[ -n "${servers}" ]]; then printf '%s\n' "${servers}" >"${tmp}/inline.server"; fi
  overrides=$(yq -r '.spec.coredns.overrides // ""' "${cluster_yaml}")
  if [[ -n "${overrides}" ]]; then printf '%s\n' "${overrides}" >"${tmp}/inline.override"; fi

  # (3) raw files from the import path (default ./coredns, relative to cluster dir)
  local import
  import=$(yq -r '.spec.coredns.import // "./coredns"' "${cluster_yaml}")
  if [[ "${import}" != /* ]]; then import="${PATH_CLUSTERS}/${domain}/${import#./}"; fi
  if [[ -d "${import}" ]]; then
    if compgen -G "${import}/*.server" >/dev/null 2>&1; then cp "${import}"/*.server "${tmp}/"; fi
    if compgen -G "${import}/*.override" >/dev/null 2>&1; then cp "${import}"/*.override "${tmp}/"; fi
  fi

  if compgen -G "${tmp}/*" >/dev/null 2>&1; then
    kubectl create configmap coredns-custom -n kube-system --kubeconfig "${kubeconfig}" \
      --from-file="${tmp}" --dry-run=client -o yaml \
      | kubectl apply --kubeconfig "${kubeconfig}" -f -
  fi

  rm -rf "${tmp}"
}

# lo::certgen — path to the certgen CLI (built by `lo kustomize build` into
# .kustomize/bin/), or non-zero if not built. It mints dev TLS via crypto/x509 —
# the mkcert-binary replacement; trust install stays `lo trust` / `mkcert -install`.
lo::certgen() {
  local bin="${PATH_BASE}/.kustomize/bin/certgen"
  [[ -x "${bin}" ]] || return 1
  echo "${bin}"
}

lo::mkcert() {
  local domain="$1" cluster_yaml="$2"
  local cluster_domain
  cluster_domain=$(yq -r '.spec.cluster.domain' "${cluster_yaml}")

  local cg
  cg=$(lo::certgen) || {
    echo "warning: certgen not built (run: lo kustomize build) — skipping dev TLS for ${cluster_domain}" >&2
    return 0
  }
  local tls_dir="${PATH_BASE}/.secrets/tls"
  mkdir -p "${tls_dir}"
  if [[ ! -f "${tls_dir}/tls.crt" ]] || [[ ! -f "${tls_dir}/tls.key" ]]; then
    "${cg}" \
      -cert-file "${tls_dir}/tls.crt" \
      -key-file "${tls_dir}/tls.key" \
      "${cluster_domain}" "*.${cluster_domain}"
  fi
}

# lo::registry_ca_path — absolute path to the local dev CA (CAROOT/rootCA.pem),
# or empty if certgen is unavailable. Used to wire containerd trust.
lo::registry_ca_path() {
  local cg
  cg=$(lo::certgen) || { echo ""; return 0; }
  local caroot
  caroot=$("${cg}" -CAROOT 2>/dev/null) || { echo ""; return 0; }
  [[ -n "${caroot}" ]] || { echo ""; return 0; }
  echo "${caroot}/rootCA.pem"
}

# lo::mkcert_registries — generate the mkcert-signed cert the registries
# serve in TLS mode. The cert's SAN list is built dynamically from the
# generated .registries.json so it always covers every registry's IP and
# (for framework registries) its canonical hostname. One cert, reused by
# every registry container. Stored at .secrets/tls/registries/.
#
# A separate cert from the application wildcard (.secrets/tls/tls.crt)
# because the SAN list is registry-derived and has a different lifecycle.
#
# Idempotent: regenerated only when missing or when the SAN set the cert
# was built for changed (e.g. IPs shifted, a mirror was added/removed).
# certgen mints the cert (creating the CA at CAROOT if needed); the host Docker
# client + curl trust it only after `lo trust` (mkcert -install) has run once.
lo::mkcert_registries() {
  registry::is_tls || return 0

  local cg
  cg=$(lo::certgen) || {
    echo "error: spec.registries.tls is true but certgen is not built." >&2
    echo "       Run 'lo kustomize build', then 'lo trust' once so docker push trusts the CA." >&2
    return 1
  }

  local tls_dir="${PATH_BASE}/.secrets/tls/registries"
  local crt="${tls_dir}/tls.crt"
  local key="${tls_dir}/tls.key"
  local sans_file="${tls_dir}/.sans"
  mkdir -p "${tls_dir}"

  # Build the SAN list (hostnames first, then IPs) from the registry JSON.
  # Framework registries contribute their canonical hostname; mirrors
  # contribute the upstream domain they impersonate inside the cluster.
  # Every registry contributes its IP so raw-IP refs verify.
  local -a sans=()
  # shellcheck disable=SC2329  # invoked indirectly via `registry::each` below
  _lo_mkcert_san() {
    # registry::each invokes this callback with 6 positional fields; this one only
    # needs ip/reg_domain/host, but the full signature documents the contract.
    # shellcheck disable=SC2034
    local name="$1" ip="$2" url="$3" reg_domain="$4" host="$5" type="$6"
    [[ -n "${host}" ]] && sans+=("${host}")
    [[ -n "${reg_domain}" ]] && sans+=("${reg_domain}")
    [[ -n "${ip}" ]] && sans+=("${ip}")
  }
  registry::each _lo_mkcert_san

  if (( ${#sans[@]} == 0 )); then
    echo "error: no registry SANs resolved — cannot generate registry TLS cert" >&2
    return 1
  fi

  # Deduplicate while preserving order.
  local -A seen=()
  local -a uniq_sans=()
  local s
  for s in "${sans[@]}"; do
    [[ -n "${seen[${s}]:-}" ]] && continue
    seen[${s}]=1
    uniq_sans+=("${s}")
  done

  # Regenerate only if missing or the SAN set changed.
  local sans_repr
  sans_repr=$(printf '%s\n' "${uniq_sans[@]}")
  if [[ -f "${crt}" && -f "${key}" && -f "${sans_file}" ]] \
    && [[ "$(cat "${sans_file}")" == "${sans_repr}" ]]; then
    debug "registry TLS cert up to date (${#uniq_sans[@]} SANs)"
    return 0
  fi

  "${cg}" -cert-file "${crt}" -key-file "${key}" "${uniq_sans[@]}"
  printf '%s\n' "${uniq_sans[@]}" > "${sans_file}"
  debug "generated registry TLS cert with SANs: ${uniq_sans[*]}"
}
