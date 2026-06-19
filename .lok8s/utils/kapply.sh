# shellcheck shell=bash disable=SC2034
# kapply.sh — server-side kubectl apply with bounded, opt-in self-healing.
#
# Two cluster states a plain `kubectl apply` can NEVER reconcile on its own —
# and which a naive retry/Tilt loop spins on forever:
#
#   1. an IMMUTABLE field changed (spec.selector, a Job's spec.template,
#      a Service's clusterIP, …). The object can only change by recreation.
#      Common trigger: the workload's label/selector scheme changed between
#      versions, so the live selector no longer matches the manifest.
#   2. an object is stuck TERMINATING — deletionTimestamp is set but a
#      finalizer never clears (e.g. a CNPG Cluster whose operator is gone, or
#      a half-deleted namespace). It blocks recreation until the finalizer is
#      removed.
#
# kapply::apply runs the apply and, ONLY when it hits one of those two states:
#   - with --force-recreate (LOK8S_FORCE_RECREATE=1): remediates just the
#     affected objects (recreate / clear finalizers) and re-applies ONCE;
#   - otherwise: fails fast with a one-line remediation hint — no silent loop.
# Every OTHER apply error is returned unchanged, so existing retry logic
# (CRD-not-established, webhook-not-ready) keeps working untouched.

import ^utils/verbose

# kapply::apply [extra kubectl flags...] < manifest
#   Reads a manifest on stdin. Extra args (e.g. --kubeconfig <path>) are
#   threaded through every kubectl call. Returns the apply's exit status.
kapply::apply() {
  local -a kf=("$@")
  local manifest out rc
  manifest=$(cat)
  out=$(printf '%s' "${manifest}" | kubectl "${kf[@]}" apply --server-side --force-conflicts -f - 2>&1)
  rc=$?
  printf '%s\n' "${out}"
  (( rc == 0 )) && return 0

  local immutable=0 terminating=0
  grep -q 'field is immutable' <<<"${out}" && immutable=1
  grep -qE 'object is being deleted|being deleted:' <<<"${out}" && terminating=1
  # Not a state we know how to heal — hand the failure back for the caller's
  # own handling (CRD/webhook retries, etc.).
  (( immutable || terminating )) || return "${rc}"

  if [[ -z "${LOK8S_FORCE_RECREATE:-}" ]]; then
    error "apply blocked by an unrecoverable state (immutable field / stuck Terminating)."
    error "  re-run with --force-recreate to recreate the affected objects (restarts their pods),"
    error "  or resolve the conflict by hand. Not retrying — that would loop."
    return "${rc}"
  fi

  warn "force-recreate: healing blocked objects, then re-applying once"
  (( immutable ))   && kapply::_heal_immutable "${manifest}" "${out}" "${kf[@]}"
  (( terminating )) && kapply::_heal_terminating "${manifest}" "${kf[@]}"

  printf '%s' "${manifest}" | kubectl "${kf[@]}" apply --server-side --force-conflicts -f -
}

# Recreate the objects kubectl reported as having an immutable-field conflict.
# kubectl phrases this two ways, both handled here:
#   client-side : `The <Kind> "<name>" is invalid: <field>: ... immutable`
#   server-side : `Error from server (Invalid): <Kind>.<group> "<name>" is invalid: ...`
# (core resources have no `.<group>`). We recreate by kind+name from the same
# manifest — `replace --force` = delete + create, which bypasses immutability.
kapply::_heal_immutable() {
  local manifest="$1" out="$2"; shift 2
  local -a kf=("$@")
  local kind name
  while read -r kind name; do
    [[ -n "${kind}" && -n "${name}" ]] || continue
    warn "  recreating immutable ${kind}/${name}"
    printf '%s' "${manifest}" \
      | yq "select(.kind == \"${kind}\" and .metadata.name == \"${name}\")" \
      | kubectl "${kf[@]}" replace --force -f - >/dev/null 2>&1 \
      || warn "  could not recreate ${kind}/${name}"
  done < <(grep -oE '[A-Z][A-Za-z]+(\.[a-z0-9.-]+)? "[^"]+" is invalid' <<<"${out}" \
            | sed -E 's/(\.[a-z0-9.-]+)? "/ /; s/" is invalid$//' | sort -u)
}

# Clear finalizers on manifest objects that are stuck Terminating, so the
# delete completes and the next apply can recreate them. Namespaces finalize
# via the /finalize subresource (spec.finalizers); everything else (CRs like
# CNPG Clusters, etc.) via metadata.finalizers.
kapply::_heal_terminating() {
  local manifest="$1"; shift
  local -a kf=("$@")
  local kind name ns
  while IFS=$'\t' read -r kind name ns; do
    [[ -n "${kind}" && -n "${name}" ]] || continue
    local -a nsf=(); [[ -n "${ns}" ]] && nsf=(-n "${ns}")
    local dts
    dts=$(kubectl "${kf[@]}" get "${kind}" "${name}" "${nsf[@]}" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) || continue
    [[ -n "${dts}" ]] || continue
    warn "  clearing finalizers on stuck-terminating ${kind}/${name}"
    if [[ "${kind,,}" == "namespace" || "${kind,,}" == "ns" ]]; then
      kubectl "${kf[@]}" get ns "${name}" -o json 2>/dev/null \
        | jq 'del(.spec.finalizers)' \
        | kubectl "${kf[@]}" replace --raw "/api/v1/namespaces/${name}/finalize" -f - >/dev/null 2>&1 \
        || warn "  could not finalize namespace/${name}"
    else
      kubectl "${kf[@]}" patch "${kind}" "${name}" "${nsf[@]}" \
        --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 \
        || warn "  could not clear finalizers on ${kind}/${name}"
    fi
  done < <(printf '%s' "${manifest}" | yq -r '[.kind, .metadata.name, (.metadata.namespace // "")] | @tsv' 2>/dev/null)
}
