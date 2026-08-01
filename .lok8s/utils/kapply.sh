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
kapply::_ui() { local sink="${KAPPLY_TTY:-/dev/tty}"; [[ "${sink}" == /dev/tty && ! -t 1 ]] && sink=/dev/null; printf '%s' "${sink}"; }

# Spinner frames (array, not a substring — avoids byte/char issues on braille).
_KAPPLY_SPIN=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)

# kubectl success verbs — lines ending in one of these are "progress" (counted
# + rolled into the window); anything else (errors) is surfaced on failure.
_KAPPLY_OK=' (serverside-applied|created|configured|unchanged|applied|deleted|annotated|labeled|patched|restarted|scaled|rolled back|condition met)$'

# Conflict markers kubectl emits when an apply cannot proceed without a recreate.
# SHARED with the bootstrap scheduler: its reap-park detection greps the UNION of
# these so it parks exactly the conflicts kapply::apply heals below — add a new
# conflict class HERE, in one place, or the two would silently drift (park-but-not-
# heal, or heal-but-not-park).
_KAPPLY_IMMUTABLE_RE='field is immutable'
_KAPPLY_TERMINATING_RE='object is being deleted|being deleted:|because it is being terminated'

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

# kapply::render_captured <label> <rc>
#   Render a CAPTURED apply-output stream (read from stdin) as the OFFLINE
#   equivalent of the live collapsing block. The bootstrap DAG fans non-gate
#   entries out as concurrent background jobs, which cannot each own /dev/tty
#   for the live spinner (they would garble each other) — so they buffer their
#   output to a file and the serialized foreground reap renders it here, one
#   de-interleaved block per entry. Routine "…applied/…created/…condition met"
#   lines collapse to a single "· N resources" count (matching the live UI's
#   final line); every other line (errors, CRD/webhook-race retries, wait
#   output) is surfaced and deduped via kapply::_aggregate. <rc> picks ✓/✗.
#   Styling is tty-only: off-tty (CI, piped logs) the same collapsed block is
#   emitted PLAIN — no ANSI of our own, and embedded CSI sequences + carriage
#   returns in the captured lines are stripped (controller/kubectl errors can
#   carry their own colors/cursor moves; OSC and bare ESC are left alone —
#   nothing in an apply stream realistically emits those).
kapply::render_captured() {
  local label="${1}" rc="${2:-0}"
  local marker=✓ color=32
  (( rc == 0 )) || { marker=✗; color=31; }
  # Capture tty-ness ONCE at entry: inside a pipeline segment below, fd 1 is a
  # pipe, so a later [[ -t 1 ]] would always be false.
  local is_tty=0; [[ -t 1 ]] && is_tty=1
  local c_on='' c_off='' c_dim=''
  (( is_tty )) && { c_on=$'\033['"${color}m"; c_off=$'\033[0m'; c_dim=$'\033[2m'; }
  local line key n=0
  # Count DISTINCT resources (keyed on the first token), not raw OK lines: the
  # CRD/webhook-race retry re-applies the whole manifest up to 6×, and a raw
  # line count would inflate "· N resources" by the number of passes.
  local -A seen_resource=()
  local -a extra=()
  # `|| [[ -n ... ]]`: keep a final line that lacks a trailing newline (a killed
  # job's buffered output can end mid-write) — plain read would drop it.
  while IFS= read -r line || [[ -n "${line}" ]]; do
    if [[ "${line}" =~ ${_KAPPLY_OK} ]]; then
      # Count ONLY when the first token has kubectl's type/name resource shape.
      # This guards two verified traps: an INDENTED line ending in an OK verb
      # makes the token EMPTY, and seen_resource[""]= is a fatal bad-subscript
      # under set -e — it would kill the scheduler mid-reap, past any || true
      # (assignment errors abort non-interactive shells); and prose that merely
      # ENDS in an OK verb ("Warning: … configured") must surface as a message,
      # not vanish into the count.
      key="${line%% *}"
      if [[ -n "${key}" && "${key}" == */* ]]; then
        seen_resource["${key}"]=1
      else
        extra+=("${line}")
      fi
    elif [[ -n "${line}" ]]; then
      extra+=("${line}")
    fi
  done
  n=${#seen_resource[@]}
  if (( n > 0 )); then
    local noun=resources; (( n == 1 )) && noun=resource
    printf '%s%s%s %s %s· %d %s%s\n' "${c_on}" "${marker}" "${c_off}" "${label}" "${c_dim}" "${n}" "${noun}" "${c_off}"
  else
    printf '%s%s%s %s\n' "${c_on}" "${marker}" "${c_off}" "${label}"
  fi
  (( ${#extra[@]} )) || return 0
  printf '%s\n' "${extra[@]}" | kapply::_aggregate | {
    # off-tty: strip CSI sequences (any terminator — _aggregate's ×N styling,
    # captured colors, cursor moves) and carriage returns, so CI/piped logs
    # stay plain text.
    if (( is_tty )); then cat; else sed $'s/\x1b\\[[0-9;]*[A-Za-z]//g' | tr -d '\r'; fi
  } | while IFS= read -r line; do
    printf '      %s%s%s\n' "${c_dim}" "${line}" "${c_off}"
  done
}

# kapply::run <phase> <command...>
#   Run an arbitrary command whose output is kubectl-style "<resource> <verb>"
#   lines (apply + annotate + rollout restart + …) and render it as ONE named,
#   collapsing progress block — for phases that aren't a single `kapply::apply`
#   (e.g. the Lo driver's coredns/registry setup). Runs the command in THIS
#   shell (no subshell — side effects/exports persist) and returns its status.
kapply::run() {
  local phase="${1}"; shift
  kapply::_tty || { "${@}"; return; }
  local tmp rc_file rc; tmp=$(mktemp); rc_file=$(mktemp)
  # Stream live (so a blocking `kubectl wait` shows readiness as it lands),
  # tee the full output to a file for the after-the-fact error/empty checks.
  { "${@}" 2>&1; echo "$?" >"${rc_file}"; } | tee "${tmp}" | kapply::_progress "${phase}" >/dev/null
  rc=$(cat "${rc_file}"); rm -f "${rc_file}"
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
  local label="${1}" timeout="${2:-180}"; shift 2
  local -a kubectl_flags=("${@}")
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
    local snap; snap=$(kubectl "${kubectl_flags[@]}" get deploy,ds,sts --all-namespaces -o json 2>/dev/null || echo '{}')
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
  local label="${1}" manifest="${2}"; shift 2
  local -a kubectl_flags=("${@}")
  local out rc rc_file; rc_file=$(mktemp)
  if kapply::_tty; then
    out=$( { printf '%s' "${manifest}" \
      | kubectl "${kubectl_flags[@]}" apply --server-side --force-conflicts -f - 2>&1; echo "$?" >"${rc_file}"; } \
      | kapply::_progress "${label}" )
  else
    out=$(printf '%s' "${manifest}" \
      | kubectl "${kubectl_flags[@]}" apply --server-side --force-conflicts -f - 2>&1); echo "$?" >"${rc_file}"
    printf '%s\n' "${out}"
  fi
  rc=$(cat "${rc_file}"); rm -f "${rc_file}"
  KAPPLY_LAST_OUTPUT="${out}"
  (( rc == 0 )) || { kapply::_tty && grep -vE "${_KAPPLY_OK}" <<<"${out}" | kapply::_aggregate >&2 || true; }
  return "${rc}"
}

kapply::apply() {
  # A leading `--label <phase>` names the progress block (the addon/target);
  # everything else is passed through to kubectl.
  local label="resources"
  local -a kubectl_flags=()
  while (( $# )); do
    case "${1}" in
      --label) label="${2:-resources}"; shift 2 ;;
      *)       kubectl_flags+=("${1}"); shift ;;
    esac
  done
  local manifest; manifest=$(cat)

  kapply::_apply_pass "${label}" "${manifest}" "${kubectl_flags[@]}"
  local rc=$?
  (( rc == 0 )) && return 0

  local out="${KAPPLY_LAST_OUTPUT}"
  local immutable=0 terminating=0
  grep -qE "${_KAPPLY_IMMUTABLE_RE}" <<<"${out}" && immutable=1
  grep -qE "${_KAPPLY_TERMINATING_RE}" <<<"${out}" && terminating=1
  (( immutable || terminating )) || return "${rc}"

  kapply::_confirm_heal || return "${rc}"

  warn "healing blocked objects, then re-applying once"
  (( immutable ))   && kapply::_heal_immutable "${manifest}" "${out}" "${kubectl_flags[@]}"
  (( terminating )) && kapply::_heal_terminating "${manifest}" "${out}" "${kubectl_flags[@]}"

  # Re-apply through the SAME display pass — collapses like the first apply.
  kapply::_apply_pass "${label}" "${manifest}" "${kubectl_flags[@]}"
}

# A REAL interactive terminal for the prompt — its stdin must be a usable tty
# (distinct from the display sink, which $KAPPLY_TTY can redirect to a file).
kapply::_interactive() {
  [[ -n "${LOK8S_NONINTERACTIVE:-}" || -n "${CI:-}" ]] && return 1
  # Auto-detect headless: with no controlling terminal on our std fds, opening
  # /dev/tty fails ("No such device or address") in background jobs / CI and
  # leaves a noisy exit-1 even when the apply itself succeeded. Treat that as
  # non-interactive instead of relying on LOK8S_NONINTERACTIVE being set.
  [[ -t 0 && -t 1 ]] || return 1
  [[ -r /dev/tty && -w /dev/tty ]]
}

# One tty prompt: print the (pre-formatted) question on /dev/tty and read the
# answer from it. Returns 0 on y/Y. The single read path shared by the three
# confirms below — and the seam tests stub to script an answer, since bats has
# no /dev/tty to type into.
kapply::_ask() {
  local prompt="${1}" ans
  printf '%s' "${prompt}" >/dev/tty 2>/dev/null
  read -r ans </dev/tty 2>/dev/null || return 1
  [[ "${ans}" =~ ^[Yy] ]]
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
  kapply::_ask $'\033[33m?\033[0m kapply: recreate the blocked object(s) above to recover? this deletes + recreates them (restarts their pods); a one-time fix. [y/N] '
}

# A SECOND, pointed confirm just for recreating a SEALED Secret (one applied
# with `immutable: true` — a crown jewel by declaration). Recreating it is a
# RE-KEY: exactly the event the seal exists to make deliberate. The generic
# heal prompt undersells that, so name the Secret and spell out the blast
# radius. --force-recreate still proceeds (it IS the documented rotation path
# and must stay usable non-interactively) but logs a pointed warning per
# Secret instead of recreating silently.
kapply::_confirm_secret_recreate() {
  local name="${1}"
  if [[ -n "${LOK8S_FORCE_RECREATE:-}" ]]; then
    warn "  RE-KEYING sealed Secret/${name} (--force-recreate): pods keep the old value until restarted; state encrypted under it may be orphaned"
    return 0
  fi
  kapply::_interactive || return 1
  kapply::_ask "$(printf '\033[31m!\033[0m kapply: Secret/%s is SEALED (immutable). Recreating it RE-KEYS the credential — pods keep the old value until restarted, and state encrypted under it may be orphaned. Really re-key? [y/N] ' "${name}")"
}

# A SECOND, pointed confirm just for force-finalizing a namespace — the most
# destructive heal (it completes the deletion of the whole namespace and every
# object still in it, via a raw /finalize API call, irreversibly). The generic
# heal prompt above undersells that, so name the namespace and warn explicitly.
# --force-recreate still skips it (the override must stay usable non-interactively,
# e.g. CI / the deploy path); no flag + no tty → refuse (don't nuke a namespace
# unattended).
kapply::_confirm_ns_finalize() {
  local name="${1}"
  [[ -n "${LOK8S_FORCE_RECREATE:-}" ]] && return 0
  kapply::_interactive || return 1
  kapply::_ask "$(printf '\033[31m!\033[0m kapply: namespace/%s is stuck Terminating. Force-remove its finalizers via the API? this COMPLETES its deletion — every object still in it is destroyed, irreversibly. [y/N] ' "${name}")"
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
  local manifest="${1}" out="${2}"; shift 2
  local -a kubectl_flags=("${@}")
  local kind name
  while read -r kind name; do
    [[ -n "${kind}" && -n "${name}" ]] || continue
    # The crown-jewel confirm applies only to Secrets the manifest actually
    # SEALS (immutable: true) — a plain Secret can hit "field is immutable"
    # too (e.g. a type: change) and gets the generic heal, no re-key drama.
    if [[ "${kind}" == "Secret" ]]; then
      local sealed
      sealed=$(printf '%s' "${manifest}" \
        | yq "select(.kind == \"Secret\" and .metadata.name == \"${name}\") | .immutable // false" 2>/dev/null)
      if [[ "${sealed}" == *true* ]]; then
        kapply::_confirm_secret_recreate "${name}" \
          || { warn "  keeping sealed Secret/${name} (re-key declined)"; continue; }
      fi
    fi
    warn "  recreating immutable ${kind}/${name}"
    printf '%s' "${manifest}" \
      | yq "select(.kind == \"${kind}\" and .metadata.name == \"${name}\")" \
      | kubectl "${kubectl_flags[@]}" replace --force -f - >/dev/null 2>&1 \
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
  local name="${1}"; shift
  local -a kubectl_flags=("${@}")
  local dts
  dts=$(kubectl "${kubectl_flags[@]}" get ns "${name}" \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) || return 0
  [[ -n "${dts}" ]] || return 0
  kapply::_confirm_ns_finalize "${name}" \
    || { warn "  skipped namespace/${name} — force-finalize declined (re-apply will retry)"; return 0; }
  warn "  force-finalizing stuck-terminating namespace/${name}"
  kubectl "${kubectl_flags[@]}" get ns "${name}" -o json 2>/dev/null \
    | jq 'del(.spec.finalizers)' \
    | kubectl "${kubectl_flags[@]}" replace --raw "/api/v1/namespaces/${name}/finalize" -f - >/dev/null 2>&1 \
    || { warn "  could not finalize namespace/${name}"; return 0; }
  local i waitn="${KAPPLY_NS_WAIT:-20}"
  for (( i = 0; i < waitn; i++ )); do
    kubectl "${kubectl_flags[@]}" get ns "${name}" &>/dev/null || return 0
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
  local manifest="${1}" out="${2}"; shift 2
  local -a kubectl_flags=("${@}")

  # (a) namespaces named in "because it is being terminated" 403s
  local nsname
  while read -r nsname; do
    [[ -n "${nsname}" ]] || continue
    kapply::_finalize_namespace "${nsname}" "${kubectl_flags[@]}"
  done < <(grep -oE 'in namespace [a-z0-9][a-z0-9-]* because it is being terminated' <<<"${out}" \
            | sed -E 's/^in namespace //; s/ because.*$//' | sort -u)

  # (b) manifest objects carrying their own stuck deletionTimestamp
  local kind name ns
  while IFS=$'\t' read -r kind name ns; do
    [[ -n "${kind}" && -n "${name}" ]] || continue
    if [[ "${kind,,}" == "namespace" || "${kind,,}" == "ns" ]]; then
      kapply::_finalize_namespace "${name}" "${kubectl_flags[@]}"
      continue
    fi
    local -a nsf=(); [[ -n "${ns}" ]] && nsf=(-n "${ns}")
    local dts
    dts=$(kubectl "${kubectl_flags[@]}" get "${kind}" "${name}" "${nsf[@]}" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) || continue
    [[ -n "${dts}" ]] || continue
    warn "  clearing finalizers on stuck-terminating ${kind}/${name}"
    kubectl "${kubectl_flags[@]}" patch "${kind}" "${name}" "${nsf[@]}" \
      --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 \
      || warn "  could not clear finalizers on ${kind}/${name}"
  done < <(printf '%s' "${manifest}" | yq -r '[.kind, .metadata.name, (.metadata.namespace // "")] | @tsv' 2>/dev/null)
}

# kapply::preflight [--age <seconds>] [--crds drain|skip|force]
#                   [--crd-allow <csv>] [kubectl flags...] < manifest
#   Sweep the MANIFEST's objects for stuck-Terminating state BEFORE an apply
#   that does not go through kapply::apply — Tilt's k8s_yaml path retries a
#   terminating object until its upsert timeout and then fails the whole
#   build ("timeout waiting for delete"). Two deadlocks make "stuck"
#   permanent, both hit on cluster/Tilt restarts:
#     - a PVC held by kubernetes.io/pvc-protection while the replacement pod
#       that references it cannot start until the PVC finishes deleting;
#     - a finalizer whose owning controller is gone (a CNPG Database whose
#       Cluster was already deleted, an operator torn down mid-delete, …).
#   Objects terminating LONGER than --age (default 30s, or
#   KAPPLY_PREFLIGHT_AGE) get their finalizers cleared so the delete
#   completes and the follow-up apply recreates them; younger deletions are
#   presumed to be legitimately draining and are left to finish. Namespaces
#   finalize via the /finalize subresource.
#   NO prompts here — the CALLER is the gate (`lo tilt preflight` refuses
#   non-kind drivers unless LOK8S_FORCE_CLEAR_TERMINATING is set). Every
#   object cleared is about to be re-applied from this same manifest, which
#   is what makes the force defensible. Best-effort: per-object failures
#   warn; always returns 0 so a preflight can never brick the deploy it
#   protects.
kapply::preflight() {
  local age="${KAPPLY_PREFLIGHT_AGE:-30}"
  local crds="${KAPPLY_PREFLIGHT_CRDS:-drain}" crd_allow=""
  local -a kubectl_flags=()
  while (( $# )); do
    case "${1}" in
      # Tolerate a value-less flag (keep the default): a bare `shift 2` on
      # one remaining arg fails and would abort under set -e, and swallowing
      # a following flag as the value would misfile its own argument.
      --age)       [[ -n "${2:-}" && "${2}" != -* ]] && { age="${2}"; shift; }; shift ;;
      --crds)      [[ -n "${2:-}" && "${2}" != -* ]] && { crds="${2}"; shift; }; shift ;;
      --crd-allow) [[ -n "${2:-}" && "${2}" != -* ]] && { crd_allow="${2}"; shift; }; shift ;;
      *)           kubectl_flags+=("${1}"); shift ;;
    esac
  done
  # The age gate is a SAFETY threshold used in arithmetic below — a
  # non-numeric value ("30s") would blow up the comparison / read as 0 and
  # force-clear everything terminating at all. Fall back to the default.
  [[ "${age}" =~ ^[0-9]+$ ]] || age=30
  # Unknown CRD policy falls back to drain — the safe middle: it resolves
  # the instance-finalizer deadlock without ever touching a CRD finalizer.
  [[ "${crds}" =~ ^(drain|skip|force)$ ]] || crds=drain
  local manifest; manifest=$(cat)

  # ONE batched GET for every manifest object (a single process/connection,
  # not one kubectl per doc — a rendered domain is hundreds of docs). Missing
  # objects are skipped. Unknown kinds (a CRD not yet installed) make kubectl
  # error without stopping the sweep — DELIBERATELY silenced (2>/dev/null):
  # on a cold start every not-yet-installed CRD would spam one error per doc
  # into the Tilt log on every reload, and the apply that follows surfaces
  # any real problem anyway. Hence also the || true.
  local live
  live=$(printf '%s' "${manifest}" \
    | kubectl "${kubectl_flags[@]}" get -f - -o json --ignore-not-found 2>/dev/null) || true

  # A namespace REFERENCED by the manifest but not declared in it still
  # blocks every create inside it while terminating — fetch those too.
  local referenced
  referenced=$(printf '%s' "${manifest}" \
    | yq -r '.metadata.namespace // ""' 2>/dev/null | grep -v '^---$' | sort -u | grep . || true)
  local ns_live=""
  if [[ -n "${referenced}" ]]; then
    # shellcheck disable=SC2086 # newline-separated names, split on purpose
    ns_live=$(kubectl "${kubectl_flags[@]}" get namespace ${referenced} \
      -o json --ignore-not-found 2>/dev/null) || true
  fi

  # Same block UI as every other kapply phase (bootstrap addons, deploy):
  # counted "<type>/<name> patched" lines collapse into "✓ preflight · N
  # resources"; per-object detail (age, finalizers) + failures surface as the
  # dim indented lines. Off-tty (Tilt's captured log) the same block renders
  # plain — see kapply::render_captured.
  local now; now=$(date -u +%s)
  local apiver kind ns name dts fins epoch stuck rc=0 report=""
  local -a nsf
  while IFS=$'\t' read -r apiver kind ns name dts fins; do
    # "-" is the cluster-scoped placeholder: tab counts as IFS WHITESPACE, so
    # a genuinely empty TSV field would collapse and shift every later field.
    [[ "${ns}" == "-" ]] && ns=""
    [[ -n "${kind}" && -n "${name}" && -n "${dts}" ]] || continue
    # GNU date first, BSD/macOS `-j -f` fallback. An UNPARSEABLE timestamp
    # must SKIP the object, not count as old — defaulting to "old" would
    # silently disable the age gate on any platform where parsing fails and
    # force-clear legitimately-draining deletions.
    epoch=$(date -ud "${dts}" +%s 2>/dev/null) \
      || epoch=$(TZ=UTC date -j -f '%Y-%m-%dT%H:%M:%SZ' "${dts}" +%s 2>/dev/null) \
      || epoch=""
    if [[ -z "${epoch}" ]]; then
      report+="${kind}/${name}${ns:+ (ns ${ns})} — unparseable deletionTimestamp '${dts}', not touching it"$'\n'
      continue
    fi
    if (( now - epoch < age )); then
      report+="${kind}/${name}${ns:+ (ns ${ns})} deleting only $(( now - epoch ))s (< ${age}s) — letting it drain"$'\n'
      continue
    fi
    if [[ "${kind}" == "CustomResourceDefinition" ]]; then
      # Policy-driven (--crds drain|skip|force) — see _preflight_crd. The
      # helper emits report lines on stdout so this loop's locals stay ours
      # (no dynamic-scope mutation from a callee).
      local crd_report
      crd_report=$(kapply::_preflight_crd "${name}" "${crds}" "${crd_allow}" "${kubectl_flags[@]}") || rc=1
      [[ -n "${crd_report}" ]] && report+="${crd_report}"$'\n'
      continue
    fi
    if [[ "${kind}" == "Namespace" ]]; then
      # The caller already gated this force — assert the override for this
      # one call so _finalize_namespace's own confirm doesn't re-ask/refuse.
      LOK8S_FORCE_RECREATE=1 kapply::_finalize_namespace "${name}" "${kubectl_flags[@]}"
      # _finalize_namespace warns-and-returns-0 on failure; re-check the live
      # state so the report never claims success for a namespace still wedged.
      if kubectl "${kubectl_flags[@]}" get ns "${name}" \
           -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null | grep -q .; then
        report+="could not finalize namespace/${name} — still terminating"$'\n'
        rc=1
      else
        report+="namespace/${name} patched"$'\n'
        report+="force-finalized Namespace/${name} — stuck $(( (now - epoch) / 60 ))m${fins:+ · finalizers: ${fins}}"$'\n'
      fi
      continue
    fi
    # Fully-qualified type (Kind.version.group) — a bare kind is ambiguous
    # the moment two groups share it (v1 Secret vs secrets.lok8s.dev Secret).
    stuck="${kind}"
    [[ "${apiver}" == */* ]] && stuck="${kind}.${apiver##*/}.${apiver%%/*}"
    nsf=(); [[ -n "${ns}" ]] && nsf=(-n "${ns}")
    if kubectl "${kubectl_flags[@]}" patch "${stuck}" "${name}" "${nsf[@]}" \
         --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1; then
      report+="${stuck,,}/${name} patched"$'\n'
      report+="cleared ${kind}/${name}${ns:+ (ns ${ns})} — stuck $(( (now - epoch) / 60 ))m${fins:+ · finalizers: ${fins}}"$'\n'
    else
      report+="could not clear finalizers on ${kind}/${name}${ns:+ (ns ${ns})}"$'\n'
      rc=1
    fi
  done < <(printf '%s\n%s' "${live}" "${ns_live}" \
    | jq -r '(.items // [.])[]? | select(.metadata.deletionTimestamp)
             | [.apiVersion, .kind, (.metadata.namespace // "-"), .metadata.name,
                .metadata.deletionTimestamp, (.metadata.finalizers // [] | join(","))]
             | @tsv' 2>/dev/null)

  if [[ -n "${report}" ]]; then
    printf '%s' "${report}" | kapply::render_captured "preflight" "${rc}"
  else
    # Nothing stuck: one honest line, same glyph family as the block UI.
    local c_on='' c_off='' c_dim=''
    [[ -t 1 ]] && { c_on=$'\033[32m'; c_off=$'\033[0m'; c_dim=$'\033[2m'; }
    printf '%s✓%s preflight %s· nothing stuck%s\n' "${c_on}" "${c_off}" "${c_dim}" "${c_off}"
  fi
  return 0
}

# kapply::_preflight_crd <crd-name> <mode> <allow-csv> [kubectl flags...]
#   Handle ONE stuck-Terminating CustomResourceDefinition per the --crds
#   policy. Report lines go to stdout (the caller folds them into the block
#   UI); returns non-zero when something stayed wedged.
#
#   A terminating CRD is almost never stuck on its OWN finalizer:
#   customresourcecleanup waits for the CRD's INSTANCES to go, and the
#   instances wait on finalizers owned by an operator that is typically part
#   of the very apply this preflight unblocks (operator down → CRs wedge →
#   CRD wedges → apply that would restart the operator can't run). Hence:
#
#   skip  — report and stand back (the conservative carve-out).
#   drain — clear finalizers on the INSTANCES only: once the CRD delete was
#           accepted every CR of that kind is doomed anyway, so skipping
#           their cleanup loses nothing. The CRD's own finalizer is never
#           touched — cleanup then completes naturally, leaving no orphaned
#           etcd data and no zombie CRs when the CRD is re-applied.
#   force — drain first; if the CRD is STILL wedged after the bounded wait,
#           strip its finalizer too (restricted to <allow-csv> when set).
#           Last resort: orphaned CR data in etcd RESURRECTS as stale
#           objects the moment the CRD is re-created.
kapply::_preflight_crd() {
  local name="${1}" mode="${2}" allow="${3}"; shift 3
  local -a kubectl_flags=("${@}")

  if [[ "${mode}" == "skip" ]]; then
    echo "refusing to force stuck CustomResourceDefinition/${name} — crds policy is 'skip' (clearing it would cascade-delete every CR of that kind); resolve by hand"
    return 0
  fi

  local rc=0 drained=0 failed=0
  local ns iname
  local -a nsf
  while IFS=$'\t' read -r ns iname; do
    [[ -n "${iname}" ]] || continue
    [[ "${ns}" == "-" ]] && ns=""
    nsf=(); [[ -n "${ns}" ]] && nsf=(-n "${ns}")
    if kubectl "${kubectl_flags[@]}" patch "${name}" "${iname}" "${nsf[@]}" \
         --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1; then
      drained=$(( drained + 1 ))
    else
      echo "could not clear finalizers on ${name}/${iname}"
      failed=$(( failed + 1 )); rc=1
    fi
  done < <(kubectl "${kubectl_flags[@]}" get "${name}" -A -o json 2>/dev/null \
    | jq -r '.items[]? | [(.metadata.namespace // "-"), .metadata.name] | @tsv' 2>/dev/null)

  # With the instances gone, customresourcecleanup completes near-instantly.
  # Numeric guard mirrors the age gate: a bad override must not abort the
  # subshell mid-report with an arithmetic error.
  local i crd_wait="${KAPPLY_CRD_WAIT:-20}"
  [[ "${crd_wait}" =~ ^[0-9]+$ ]] || crd_wait=20
  for (( i = 0; i < crd_wait; i++ )); do
    kubectl "${kubectl_flags[@]}" get crd "${name}" &>/dev/null || break
    sleep "${KAPPLY_POLL_INTERVAL:-1}"
  done
  if ! kubectl "${kubectl_flags[@]}" get crd "${name}" &>/dev/null; then
    echo "customresourcedefinition/${name} patched"
    echo "drained ${drained} instance(s) of CustomResourceDefinition/${name} — cleanup completed on its own"
    return "${rc}"
  fi

  if [[ "${mode}" == "force" ]] && kapply::_crd_allowed "${name}" "${allow}"; then
    if kubectl "${kubectl_flags[@]}" patch crd "${name}" \
         --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1; then
      echo "customresourcedefinition/${name} patched"
      echo "FORCED CustomResourceDefinition/${name} finalizer off after drain (${drained} cleared, ${failed} failed) — stale etcd CRs may resurrect on re-apply"
    else
      echo "could not force finalizers on CustomResourceDefinition/${name}"
      rc=1
    fi
    return "${rc}"
  fi

  echo "CustomResourceDefinition/${name} still terminating after draining ${drained} instance(s) — not forcing its finalizer (crds policy: ${mode}$( [[ "${mode}" == "force" ]] && echo ", not in crd-allow" ))"
  return 1
}

# Empty allowlist = every manifest CRD may be forced; otherwise exact match.
# Whitespace around csv entries is stripped so a hand-typed "a, b" matches.
kapply::_crd_allowed() {
  local name="${1}" allow="${2:-}"
  allow="${allow// /}"
  [[ -z "${allow}" ]] && return 0
  [[ ",${allow}," == *",${name},"* ]]
}
