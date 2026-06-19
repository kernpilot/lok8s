# shellcheck shell=bash disable=SC2034
# kapply.sh — server-side kubectl apply with compact progress + bounded,
# interactive-or-opt-in self-healing.
#
# Two cluster states a plain `kubectl apply` can NEVER reconcile on its own,
# and which a naive retry/Tilt loop spins on forever:
#   1. an IMMUTABLE field changed (spec.selector, a Job's spec.template, a
#      Service's clusterIP, …). The object can only change by recreation.
#   2. an object is stuck TERMINATING — deletionTimestamp set but a finalizer
#      never clears (e.g. a CNPG Cluster whose operator is gone, or a
#      half-deleted namespace). It blocks recreation until the finalizer goes.
#
# On either state kapply::apply heals just the affected objects (recreate /
# clear finalizers) and re-applies ONCE. The decision is:
#   - LOK8S_FORCE_RECREATE (--force-recreate) → heal, no questions asked;
#   - an interactive terminal → PROMPT [y/N];
#   - no tty / CI / LOK8S_NONINTERACTIVE → fail fast with a remediation hint.
# Every OTHER apply error is returned unchanged so existing retry logic
# (CRD-not-established, webhook-not-ready) keeps working.
#
# Display: on a terminal, the per-object "…serverside-applied" spam is
# collapsed to a single in-place line + a one-line summary (like docker
# build). Off a terminal, full output is printed (CI/Tilt logs). The raw
# output is always stashed in KAPPLY_LAST_OUTPUT for callers to inspect.

import ^utils/verbose

KAPPLY_LAST_OUTPUT=""

# True when we may draw the collapsed one-line UI / prompt on the terminal.
kapply::_tty() {
  [[ -n "${LOK8S_NONINTERACTIVE:-}" || -n "${CI:-}" ]] && return 1
  [[ -w /dev/tty ]]
}

# kapply::apply [extra kubectl flags...] < manifest
#   Reads a manifest on stdin. Extra args (e.g. --kubeconfig <path>) thread
#   through every kubectl call. Returns the apply's exit status; stashes the
#   raw kubectl output in KAPPLY_LAST_OUTPUT.
kapply::apply() {
  local -a kf=("$@")
  local manifest; manifest=$(cat)
  local out rc rcf; rcf=$(mktemp)

  if kapply::_tty; then
    # Stream through the collapsing filter; the full output is captured (not
    # echoed) so only the one-line progress reaches the screen.
    out=$( { printf '%s' "${manifest}" \
      | kubectl "${kf[@]}" apply --server-side --force-conflicts -f - 2>&1; echo "$?" >"${rcf}"; } \
      | kapply::_progress )
  else
    out=$(printf '%s' "${manifest}" \
      | kubectl "${kf[@]}" apply --server-side --force-conflicts -f - 2>&1); echo "$?" >"${rcf}"
    printf '%s\n' "${out}"
  fi
  rc=$(cat "${rcf}"); rm -f "${rcf}"
  KAPPLY_LAST_OUTPUT="${out}"

  (( rc == 0 )) && return 0
  # Failure: make sure the errors are visible even in collapsed mode.
  kapply::_tty && printf '%s\n' "${out}" >&2

  local immutable=0 terminating=0
  grep -q 'field is immutable' <<<"${out}" && immutable=1
  grep -qE 'object is being deleted|being deleted:' <<<"${out}" && terminating=1
  (( immutable || terminating )) || return "${rc}"

  if ! kapply::_confirm_heal; then
    return "${rc}"
  fi

  warn "healing blocked objects, then re-applying once"
  (( immutable ))   && kapply::_heal_immutable "${manifest}" "${out}" "${kf[@]}"
  (( terminating )) && kapply::_heal_terminating "${manifest}" "${kf[@]}"

  printf '%s' "${manifest}" | kubectl "${kf[@]}" apply --server-side --force-conflicts -f -
}

# Decide whether to heal: explicit flag → yes; interactive → prompt; else no.
kapply::_confirm_heal() {
  [[ -n "${LOK8S_FORCE_RECREATE:-}" ]] && return 0
  if ! kapply::_tty; then
    error "apply blocked by an unrecoverable state (immutable field / stuck Terminating)."
    error "  re-run with --force-recreate to recreate the affected objects (restarts their pods),"
    error "  or resolve the conflict by hand. Not retrying — that would loop."
    return 1
  fi
  local ans
  printf '\033[33m?\033[0m kapply: recreate the blocked object(s) to recover? [y/N] ' >/dev/tty
  read -r ans </dev/tty || return 1
  [[ "${ans}" =~ ^[Yy] ]]
}

# Stream filter: pass every line through (captured by the caller), and on a
# terminal collapse the success lines to ONE in-place line + a final summary.
kapply::_progress() {
  local n=0 line width
  width=$(( $(tput cols 2>/dev/null || echo 80) - 4 ))
  (( width > 20 )) || width=76
  while IFS= read -r line; do
    printf '%s\n' "${line}"
    case "${line}" in
      *' serverside-applied'|*' created'|*' configured'|*' unchanged'|*' applied'|*' deleted')
        n=$(( n + 1 ))
        printf '\r\033[2K  %.*s' "${width}" "${line}" >/dev/tty ;;
    esac
  done
  (( n )) && printf '\r\033[2K  \033[32m✓\033[0m applied %d resource(s)\n' "${n}" >/dev/tty
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
