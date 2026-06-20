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
# Force-finalizing a stuck-Terminating NAMESPACE is the one heal destructive
# enough to get a SECOND, pointed confirm in the interactive path (it completes
# the namespace's deletion + all its contents); --force-recreate still skips it.
# Every OTHER apply error is returned unchanged so existing retry logic
# (CRD-not-established, webhook-not-ready) keeps working.
#
# Display: on a terminal, the per-object "…serverside-applied" spam is
# collapsed to a single in-place line + a one-line summary (like docker
# build). Off a terminal, full output is printed (CI/Tilt logs). The raw
# output is always stashed in KAPPLY_LAST_OUTPUT for callers to inspect.
#
# A plain sourceable util — it uses error/warn/debug from utils/verbose.sh,
# which every caller (the CLI, deploy, bootstrap, the drivers) loads first.

KAPPLY_LAST_OUTPUT=""

# True when we may draw the collapsed one-line UI / prompt on the terminal.
# Where the live UI is drawn: a real terminal, or $KAPPLY_TTY (a file) so the
# rendering can be captured + asserted in tests.
kapply::_tty() {
  [[ -n "${KAPPLY_TTY:-}" ]] && return 0               # test override: force the UI
  [[ -n "${DEBUG:-}" ]] && return 1                     # verbose (lo -v): print everything, don't aggregate
  [[ -n "${LOK8S_NONINTERACTIVE:-}" || -n "${CI:-}" ]] && return 1
  [[ -w /dev/tty ]]
}
kapply::_ui() { printf '%s' "${KAPPLY_TTY:-/dev/tty}"; }

# Spinner frames (array, not a substring — avoids byte/char issues on braille).
_KAPPLY_SPIN=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)

# kubectl success verbs — lines ending in one of these are "progress" (counted
# + rolled into the window); anything else (errors) is surfaced on failure.
_KAPPLY_OK=' (serverside-applied|created|configured|unchanged|applied|deleted|annotated|labeled|patched|restarted|scaled|rolled back|condition met)$'

# Collapse repeated identical error lines (e.g. one webhook-not-ready message
# emitted once per object) into a single line with a "(×N)" count, preserving
# first-seen order. Distinct errors (different objects) are kept separate.
kapply::_aggregate() {
  awk '
    { if (!($0 in seen)) { seen[$0]=1; order[++m]=$0 } count[$0]++ }
    END { for (i=1; i<=m; i++) { l=order[i]
            if (count[l] > 1) printf "%s  \033[2m(\303\227%d)\033[0m\n", l, count[l]
            else print l } }'
}

# kapply::run <phase> <command...>
#   Run an arbitrary command whose output is kubectl-style "<resource> <verb>"
#   lines (apply + annotate + rollout restart + …) and render it as ONE named,
#   collapsing progress block — for phases that aren't a single `kapply::apply`
#   (e.g. the Lo driver's coredns/registry setup). Runs the command in THIS
#   shell (no subshell — side effects/exports persist) and returns its status.
kapply::run() {
  local phase="$1"; shift
  kapply::_tty || { "$@"; return; }
  local tmp rcf rc; tmp=$(mktemp); rcf=$(mktemp)
  # Stream live (so a blocking `kubectl wait` shows readiness as it lands),
  # tee the full output to a file for the after-the-fact error/empty checks.
  { "$@" 2>&1; echo "$?" >"${rcf}"; } | tee "${tmp}" | kapply::_progress "${phase}" >/dev/null
  rc=$(cat "${rcf}"); rm -f "${rcf}"
  if ! grep -qE "${_KAPPLY_OK}" "${tmp}"; then
    cat "${tmp}"                                    # no progress lines (warnings/notes) — show as-is
  elif (( rc != 0 )); then
    grep -vE "${_KAPPLY_OK}" "${tmp}" | kapply::_aggregate >&2 || true   # surface errors (deduped) on failure
  fi
  rm -f "${tmp}"
  return "${rc}"
}

# kapply::wait_ready <label> <timeout-seconds> [kubectl flags...] < manifest
#   Wait for the Deployments / DaemonSets / StatefulSets IN THE MANIFEST to be
#   ready — scoped to exactly what we just applied, NOT the whole cluster (so it
#   never blocks on app workloads or addons applied later). On a terminal it
#   shows a ticking spinner, the still-pending names, and a countdown; off a
#   terminal it stays quiet (verbose `debug`s each poll). Best-effort: a timeout
#   is a ⚠, never fatal — the caller decides whether to care.
kapply::wait_ready() {
  local label="$1" timeout="${2:-180}"; shift 2
  local -a kf=("$@")
  local manifest; manifest=$(cat)
  local -a targets=()
  mapfile -t targets < <(printf '%s' "${manifest}" \
    | yq -r 'select(.kind == "Deployment" or .kind == "DaemonSet" or .kind == "StatefulSet")
             | (.kind | downcase) + "|" + (.metadata.namespace // "default") + "|" + .metadata.name' 2>/dev/null \
    | grep -v '^---$' | sort -u)   # yq emits a --- separator between multi-doc outputs
  (( ${#targets[@]} )) || return 0

  local ui=""; kapply::_tty && ui="$(kapply::_ui)"
  local start="${SECONDS}" tick=0
  while true; do
    local snap; snap=$(kubectl "${kf[@]}" get deploy,ds,sts --all-namespaces -o json 2>/dev/null || echo '{}')
    local -a pending=()
    local t kind ns name r
    for t in "${targets[@]}"; do
      IFS='|' read -r kind ns name <<<"${t}"
      r=$(jq -r --arg k "${kind}" --arg ns "${ns}" --arg n "${name}" '
        [ .items[] | select((.kind | ascii_downcase) == $k and .metadata.namespace == $ns and .metadata.name == $n) ] | .[0] |
        if . == null then false
        elif $k == "daemonset" then ((.status.desiredNumberScheduled // 0) > 0 and (.status.numberReady // 0) >= (.status.desiredNumberScheduled // 1))
        elif $k == "statefulset" then ((.status.readyReplicas // 0) >= (.spec.replicas // 1))
        else ((.status.availableReplicas // 0) >= (.spec.replicas // 1)) end' <<<"${snap}" 2>/dev/null)
      [[ "${r}" == "true" ]] || pending+=("${name}")
    done

    if (( ${#pending[@]} == 0 )); then
      [[ -n "${ui}" ]] && printf '\r\033[K\033[32m✓\033[0m %s · ready\n' "${label}" >>"${ui}" 2>/dev/null
      return 0
    fi
    local elapsed=$(( SECONDS - start ))
    if (( elapsed >= timeout )); then
      if [[ -n "${ui}" ]]; then
        printf '\r\033[K\033[33m⚠\033[0m %s · timed out after %ds, %d not ready: \033[2m%.50s\033[0m\n' \
          "${label}" "${timeout}" "${#pending[@]}" "${pending[*]}" >>"${ui}" 2>/dev/null
      else
        warn "${label}: timed out after ${timeout}s; not ready: ${pending[*]}"
      fi
      return 0
    fi
    if [[ -n "${ui}" ]]; then
      tick=$(( tick + 1 ))
      printf '\r\033[K\033[36m%s\033[0m %s · %ds left · waiting on %d: \033[2m%.50s\033[0m' \
        "${_KAPPLY_SPIN[$(( tick % ${#_KAPPLY_SPIN[@]} ))]}" "${label}" "$(( timeout - elapsed ))" \
        "${#pending[@]}" "${pending[*]}" >>"${ui}" 2>/dev/null
    else
      debug "${label}: waiting on ${#pending[@]}: ${pending[*]}"
    fi
    sleep "${KAPPLY_POLL_INTERVAL:-1}"
  done
}

# kapply::apply [extra kubectl flags...] < manifest
#   Reads a manifest on stdin. Extra args (e.g. --kubeconfig <path>) thread
#   through every kubectl call. Returns the apply's exit status; stashes the
#   raw kubectl output in KAPPLY_LAST_OUTPUT.
# One server-side apply: collapse the output on a tty (full off-tty), stash
# the raw output in KAPPLY_LAST_OUTPUT, surface ONLY error lines on failure,
# and return kubectl's exit. Used for BOTH the initial apply and the post-heal
# re-apply, so the re-apply renders the same named progress block (never
# escapes as raw output).
kapply::_apply_pass() {
  local label="$1" manifest="$2"; shift 2
  local -a kf=("$@")
  local out rc rcf; rcf=$(mktemp)
  if kapply::_tty; then
    out=$( { printf '%s' "${manifest}" \
      | kubectl "${kf[@]}" apply --server-side --force-conflicts -f - 2>&1; echo "$?" >"${rcf}"; } \
      | kapply::_progress "${label}" )
  else
    out=$(printf '%s' "${manifest}" \
      | kubectl "${kf[@]}" apply --server-side --force-conflicts -f - 2>&1); echo "$?" >"${rcf}"
    printf '%s\n' "${out}"
  fi
  rc=$(cat "${rcf}"); rm -f "${rcf}"
  KAPPLY_LAST_OUTPUT="${out}"
  (( rc == 0 )) || { kapply::_tty && grep -vE "${_KAPPLY_OK}" <<<"${out}" | kapply::_aggregate >&2 || true; }
  return "${rc}"
}

kapply::apply() {
  # A leading `--label <phase>` names the progress block (the addon/target);
  # everything else is passed through to kubectl.
  local label="resources"
  local -a kf=()
  while (( $# )); do
    case "$1" in
      --label) label="${2:-resources}"; shift 2 ;;
      *)       kf+=("$1"); shift ;;
    esac
  done
  local manifest; manifest=$(cat)

  kapply::_apply_pass "${label}" "${manifest}" "${kf[@]}"
  local rc=$?
  (( rc == 0 )) && return 0

  local out="${KAPPLY_LAST_OUTPUT}"
  local immutable=0 terminating=0
  grep -q 'field is immutable' <<<"${out}" && immutable=1
  grep -qE 'object is being deleted|being deleted:|because it is being terminated' <<<"${out}" && terminating=1
  (( immutable || terminating )) || return "${rc}"

  kapply::_confirm_heal || return "${rc}"

  warn "healing blocked objects, then re-applying once"
  (( immutable ))   && kapply::_heal_immutable "${manifest}" "${out}" "${kf[@]}"
  (( terminating )) && kapply::_heal_terminating "${manifest}" "${out}" "${kf[@]}"

  # Re-apply through the SAME display pass — collapses like the first apply.
  kapply::_apply_pass "${label}" "${manifest}" "${kf[@]}"
}

# A REAL interactive terminal for the prompt — its stdin must be a usable tty
# (distinct from the display sink, which $KAPPLY_TTY can redirect to a file).
kapply::_interactive() {
  [[ -n "${LOK8S_NONINTERACTIVE:-}" || -n "${CI:-}" ]] && return 1
  [[ -r /dev/tty && -w /dev/tty ]]
}

# Decide whether to heal: explicit flag → yes; interactive → prompt; else no.
kapply::_confirm_heal() {
  [[ -n "${LOK8S_FORCE_RECREATE:-}" ]] && return 0
  if ! kapply::_interactive; then
    error "apply blocked by an unrecoverable state (immutable field / stuck Terminating)."
    error "  re-run with --force-recreate to recreate the affected objects (restarts their pods),"
    error "  or resolve the conflict by hand. Not retrying — that would loop."
    return 1
  fi
  local ans
  printf '\033[33m?\033[0m kapply: recreate the blocked object(s) above to recover? this deletes + recreates them (restarts their pods); a one-time fix. [y/N] ' >/dev/tty 2>/dev/null
  read -r ans </dev/tty 2>/dev/null || return 1
  [[ "${ans}" =~ ^[Yy] ]]
}

# A SECOND, pointed confirm just for force-finalizing a namespace — the most
# destructive heal (it completes the deletion of the whole namespace and every
# object still in it, via a raw /finalize API call, irreversibly). The generic
# heal prompt above undersells that, so name the namespace and warn explicitly.
# --force-recreate still skips it (the override must stay usable non-interactively,
# e.g. CI / the deploy path); no flag + no tty → refuse (don't nuke a namespace
# unattended).
kapply::_confirm_ns_finalize() {
  local name="$1"
  [[ -n "${LOK8S_FORCE_RECREATE:-}" ]] && return 0
  kapply::_interactive || return 1
  local ans
  printf '\033[31m!\033[0m kapply: namespace/%s is stuck Terminating. Force-remove its finalizers via the API? this COMPLETES its deletion — every object still in it is destroyed, irreversibly. [y/N] ' "${name}" >/dev/tty 2>/dev/null
  read -r ans </dev/tty 2>/dev/null || return 1
  [[ "${ans}" =~ ^[Yy] ]]
}

# Stream filter: pass every line through to stdout (so the caller still
# captures the full output), and on a terminal render a compact, self-erasing
# block — a spinner header (named after the phase) plus the last ≤3 lines
# scrolling up:
#
#   ⠹ cilium
#       configmap/cilium-config serverside-applied
#       secret/cilium-ca serverside-applied
#       service/cilium-envoy serverside-applied
#
# On EOF the block is erased and replaced by a single  ✓ <phase> · N applied.
kapply::_progress() {
  local label="${1:-resources}"
  local ui; ui="$(kapply::_ui)"
  local n=0 drawn=0 line frame width l
  local -a win=()
  width=$(( $(tput cols 2>/dev/null || echo 80) - 6 )); (( width > 20 )) || width=74
  while IFS= read -r line; do
    printf '%s\n' "${line}"
    case "${line}" in
      *' serverside-applied'|*' created'|*' configured'|*' unchanged'|*' applied'|*' deleted'|*' annotated'|*' labeled'|*' patched'|*' restarted'|*' scaled'|*' condition met')
        n=$(( n + 1 ))
        win+=("${line}"); (( ${#win[@]} > 3 )) && win=("${win[@]: -3}")
        frame="${_KAPPLY_SPIN[$(( n % ${#_KAPPLY_SPIN[@]} ))]}"
        {
          (( drawn )) && printf '\033[%dA' "${drawn}"        # back to block top
          printf '\r\033[K\033[36m%s\033[0m %s\n' "${frame}" "${label}"
          drawn=1
          for l in "${win[@]}"; do
            printf '\033[K      \033[2m%.*s\033[0m\n' "${width}" "${l}"
            drawn=$(( drawn + 1 ))
          done
        } >>"${ui}" 2>/dev/null ;;
    esac
  done
  if (( n )); then
    local noun=resources; (( n == 1 )) && noun=resource
    {
      (( drawn )) && printf '\033[%dA\033[0J' "${drawn}"     # erase the block
      printf '\r\033[K\033[32m✓\033[0m %s · %d %s\n' "${label}" "${n}" "${noun}"
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

# Force-complete a stuck-Terminating namespace by dropping its spec.finalizers
# via the /finalize subresource (the built-in "kubernetes" finalizer that gates
# the delete on content garbage-collection). No-op unless the namespace really
# is terminating. Then wait (bounded) for it to actually disappear, so the
# follow-up apply recreates it cleanly instead of racing its tail-end deletion.
kapply::_finalize_namespace() {
  local name="$1"; shift
  local -a kf=("$@")
  local dts
  dts=$(kubectl "${kf[@]}" get ns "${name}" \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) || return 0
  [[ -n "${dts}" ]] || return 0
  kapply::_confirm_ns_finalize "${name}" \
    || { warn "  skipped namespace/${name} — force-finalize declined (re-apply will retry)"; return 0; }
  warn "  force-finalizing stuck-terminating namespace/${name}"
  kubectl "${kf[@]}" get ns "${name}" -o json 2>/dev/null \
    | jq 'del(.spec.finalizers)' \
    | kubectl "${kf[@]}" replace --raw "/api/v1/namespaces/${name}/finalize" -f - >/dev/null 2>&1 \
    || { warn "  could not finalize namespace/${name}"; return 0; }
  local i waitn="${KAPPLY_NS_WAIT:-20}"
  for (( i = 0; i < waitn; i++ )); do
    kubectl "${kf[@]}" get ns "${name}" &>/dev/null || return 0
    sleep "${KAPPLY_POLL_INTERVAL:-1}"
  done
}

# Heal objects stuck Terminating so the delete completes and the re-apply can
# recreate them. The apiserver reports the block two different ways:
#   (a) a 403 on writes INTO a terminating namespace ("...in namespace X
#       because it is being terminated") — the namespace itself is wedged (a
#       finalizer on its contents never cleared); force-finalize it. This is
#       the common "half-torn-down install" case (KKP, CNPG, …).
#   (b) a manifest object that is itself mid-delete (deletionTimestamp set,
#       finalizer not clearing) — e.g. a re-applied CNPG Cluster. Namespaces
#       finalize via /finalize; everything else via metadata.finalizers.
# CRDs stuck mid-delete are deliberately NOT force-removed here — that would
# cascade-delete every CR of that kind cluster-wide; the CRD-settle retry in
# the caller handles that race instead.
kapply::_heal_terminating() {
  local manifest="$1" out="$2"; shift 2
  local -a kf=("$@")

  # (a) namespaces named in "because it is being terminated" 403s
  local nsname
  while read -r nsname; do
    [[ -n "${nsname}" ]] || continue
    kapply::_finalize_namespace "${nsname}" "${kf[@]}"
  done < <(grep -oE 'in namespace [a-z0-9][a-z0-9-]* because it is being terminated' <<<"${out}" \
            | sed -E 's/^in namespace //; s/ because.*$//' | sort -u)

  # (b) manifest objects carrying their own stuck deletionTimestamp
  local kind name ns
  while IFS=$'\t' read -r kind name ns; do
    [[ -n "${kind}" && -n "${name}" ]] || continue
    if [[ "${kind,,}" == "namespace" || "${kind,,}" == "ns" ]]; then
      kapply::_finalize_namespace "${name}" "${kf[@]}"
      continue
    fi
    local -a nsf=(); [[ -n "${ns}" ]] && nsf=(-n "${ns}")
    local dts
    dts=$(kubectl "${kf[@]}" get "${kind}" "${name}" "${nsf[@]}" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) || continue
    [[ -n "${dts}" ]] || continue
    warn "  clearing finalizers on stuck-terminating ${kind}/${name}"
    kubectl "${kf[@]}" patch "${kind}" "${name}" "${nsf[@]}" \
      --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 \
      || warn "  could not clear finalizers on ${kind}/${name}"
  done < <(printf '%s' "${manifest}" | yq -r '[.kind, .metadata.name, (.metadata.namespace // "")] | @tsv' 2>/dev/null)
}
