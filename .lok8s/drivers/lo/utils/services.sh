# shellcheck shell=bash
# services.sh — Cluster service setup (CoreDNS)

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
