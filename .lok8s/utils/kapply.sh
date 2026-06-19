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
# Where the live UI is drawn: a real terminal, or $KAPPLY_TTY (a file) so the
# rendering can be captured + asserted in tests.
kapply::_tty() {
  [[ -n "${KAPPLY_TTY:-}" ]] && return 0
  [[ -n "${LOK8S_NONINTERACTIVE:-}" || -n "${CI:-}" ]] && return 1
  [[ -w /dev/tty ]]
}
kapply::_ui() { printf '%s' "${KAPPLY_TTY:-/dev/tty}"; }

# Spinner frames (array, not a substring — avoids byte/char issues on braille).
_KAPPLY_SPIN=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)

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

# Stream filter: pass every line through to stdout (so the caller still
# captures the full output), and on a terminal render a compact, self-erasing
# block — a spinner header plus the last ≤3 progress lines scrolling up:
#
#   ⠹ applying… (12)
#       configmap/cilium-config serverside-applied
#       secret/cilium-ca serverside-applied
#       service/cilium-envoy serverside-applied
#
# On EOF the whole block is erased and replaced by a single  ✓ applied N line.
kapply::_progress() {
  local ui; ui="$(kapply::_ui)"
  local n=0 drawn=0 line frame width l
  local -a win=()
  width=$(( $(tput cols 2>/dev/null || echo 80) - 6 )); (( width > 20 )) || width=74
  while IFS= read -r line; do
    printf '%s\n' "${line}"
    case "${line}" in
      *' serverside-applied'|*' created'|*' configured'|*' unchanged'|*' applied'|*' deleted'|*' condition met')
        n=$(( n + 1 ))
        win+=("${line}"); (( ${#win[@]} > 3 )) && win=("${win[@]: -3}")
        frame="${_KAPPLY_SPIN[$(( n % ${#_KAPPLY_SPIN[@]} ))]}"
        {
          (( drawn )) && printf '\033[%dA' "${drawn}"        # back to block top
          printf '\r\033[K\033[36m%s\033[0m applying… (%d)\n' "${frame}" "${n}"
          drawn=1
          for l in "${win[@]}"; do
            printf '\033[K      \033[2m%.*s\033[0m\n' "${width}" "${l}"
            drawn=$(( drawn + 1 ))
          done
        } >>"${ui}" 2>/dev/null ;;
    esac
  done
  if (( n )); then
    {
      (( drawn )) && printf '\033[%dA\033[0J' "${drawn}"     # erase the block
      printf '\r\033[K\033[32m✓\033[0m applied %d resource(s)\n' "${n}"
    } >>"${ui}" 2>/dev/null
  fi
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
