# shellcheck shell=bash
# keysonly.sh — the keys-only marker of the Secret plugin (lo build
# --keys-only). The plugin's keys-only run puts the annotation
# secrets.lok8s.dev/keys-only on each Secret that it emits: the Secret has
# its key names and empty values only. Such a Secret never goes into a
# domain's artifacts and is never applied.

# Rendered and decrypted bytes (a render, an artifact, a decrypted
# manifest) can hold Secret values. Each function that holds them follows
# three rules (Go needs none of them):
#   - Tracing off: two lines first. Under `bash -x` (or BASH_XTRACEFD) the
#     trace would print each assignment and each argument.
#       [[ $- != *[TE]* ]] || { local - _ko_traps _ko_rc; ...; }
#       [[ $- != *x* ]] || { local -; set +x; }
#     The second line turns tracing off; `local -` sets the options back on
#     return, and no child inherits the tracing. The first line comes into
#     play when functrace (set -T) or errtrace (set -E) is on: then the
#     function inherits the DEBUG, RETURN and ERR traps of its caller, and
#     such a trap (`trap 'set -x' RETURN`) could turn tracing on again in
#     the middle of the function. The line saves the three traps and
#     clears them, turns off xtrace, functrace and errtrace, and runs the
#     function again with the same arguments; when that run returns, it
#     sets the traps back and returns its exit status. The lines run
#     `local -` only when one of these options is on, because bash 4.3 does
#     not know `local -`: with one of them on, bash 4.3 stops at the start
#     of lo (keysonly::check_trace_support).
#   - Exact bytes: a capture keeps the final newlines with a sentinel
#     (`x="$(cmd && printf .)"; x="${x%.}"`), and the bytes go on with
#     `printf '%s'`. A plain $(...) strips each final newline, which changes
#     the value of a last block scalar such as `|+`.
#   - No here-string: `printf '%s' "${x}" | cmd`, or `cmd < <(printf ...)`
#     when cmd must run in this shell. A here-string larger than the pipe
#     buffer goes into a file in TMPDIR (bash 5.1 and later), which a kill
#     can leave behind.

# keysonly::check_trace_support <major> <minor> <flags>: stop when this bash
# cannot keep Secret values out of a trace. <major>.<minor> is the bash
# version and <flags> the options ($-). The tracing rules above need
# `local -` (bash 4.4) when xtrace, functrace or errtrace is on: with an
# older bash, the guard would fail at the point that holds the value, so lo
# stops at its start instead (.lok8s/lo).
keysonly::check_trace_support() {
  (( ${1} > 4 || (${1} == 4 && ${2} >= 4) )) && return 0
  [[ ${3} != *[xTE]* ]] && return 0
  error "lo: bash ${1}.${2} cannot keep Secret values out of a trace: run lo without set -x, set -T and set -E, or use bash 4.4 or later"
  return 1
}

# The causes when yq cannot run the check (exit status 3 of
# keysonly::has_marker): an installed yq before v4.47.1 (the words of the
# spec reader, spec::_kver_read), and no yq. keysonly::yq_cause prints the
# one that applies.
_KEYSONLY_OLD_YQ='the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.47.1 or later (b install installs the pinned yq).'
_KEYSONLY_NO_YQ='yq is not installed. Install yq v4.47.1 or later (b install installs the pinned yq).'

# The end of the message of a failed keys-only render or yq run (Go:
# keysOnlyWithheld): lo does not print their stderr, as it can quote an
# input value.
_KEYSONLY_WITHHELD='in keys-only mode lo does not print its stderr, which can quote a value: run the build without --keys-only to see it'

# keysonly::yq_cause: print why yq cannot run the check, after exit status
# 3 of keysonly::has_marker: no yq, or one before v4.47.1.
keysonly::yq_cause() {
  if command -v yq >/dev/null 2>&1; then
    printf '%s' "${_KEYSONLY_OLD_YQ}"
  else
    printf '%s' "${_KEYSONLY_NO_YQ}"
  fi
}

# keysonly::has_marker <file|->: look for an object with the keys-only
# marker in the YAML stream <file> (- reads stdin; Go:
# build.HasKeysOnlyMarker). The marker is a mapping, at any depth of any
# document (a List item too), whose metadata.annotations has the key
# secrets.lok8s.dev/keys-only. yq parses the stream, so a quoted, escaped
# or flow-style key is the same key; explode(.) resolves the aliases and
# merge keys first. The yq version is checked first: a yq before v4.47.1
# fails on the flag --yaml-fix-merge-anchor-to-spec, and that failure is
# not a stream that does not parse. The Go twin parses without yq. Exit
# status:
#   0  the stream holds the marker.
#   1  it does not.
#   2  it does not parse as YAML (yq failed): it cannot be checked.
#   3  yq cannot run the check: no yq, or one before v4.47.1
#      (keysonly::yq_cause prints which).
keysonly::has_marker() {
  local out
  yq --yaml-fix-merge-anchor-to-spec --version >/dev/null 2>&1 || return 3
  out=$(yq --yaml-fix-merge-anchor-to-spec '[explode(.) | .. | select(kind == "map") | .metadata | select(kind == "map") | .annotations | select(kind == "map") | has("secrets.lok8s.dev/keys-only")] | any' "${1}" 2>/dev/null) || return 2
  [[ $'\n'"${out}"$'\n' == *$'\ntrue\n'* ]] && return 0
  return 1
}

# keysonly::refuse_stream <prefix> <what> <file|->: the apply guard on a
# rendered stream (Go: build.RefuseKeysOnlyStream): the bootstrap applies
# each entry's render, and each decrypted restore.d manifest, through it.
# keysonly::read_checked_file reads the Tilt manifest, a restore.d working
# copy and the KubeOne addons once and checks them in the same way.
# <what> names the stream in the message, after "<prefix>: refusing to
# apply ". It refuses as keysonly::read_checked_artifact does: a stream with
# the keys-only marker, and a stream that cannot be checked.
keysonly::refuse_stream() {
  local marker=0
  keysonly::has_marker "${3}" || marker=$?
  (( marker == 1 )) && return 0
  keysonly::refusal "${1}" "${2}" "${marker}"
  return 1
}

# keysonly::refusal <prefix> <what> <status>: print why lo refuses to apply
# the stream <what>, for an exit status of keysonly::has_marker other than
# 1 (the messages of keysonly::refuse_stream).
keysonly::refusal() {
  local prefix="${1}" what="${2}"
  case "${3}" in
    2) error "${prefix}: refusing to apply ${what}: it does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)" ;;
    3) error "${prefix}: refusing to apply ${what}: it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only): $(keysonly::yq_cause)" ;;
    *) error "${prefix}: refusing to apply ${what}: it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one" ;;
  esac
}

# keysonly::read_checked_artifact <domain> <artifact>: the apply guard on a
# domain artifact (Go: build.ReadCheckedArtifact): lo deploy and lo hooks
# read the artifact through it. It reads the artifact once and checks that
# copy. It prints why the artifact must not be applied and returns 1, with
# nothing on stdout, when the artifact holds an object with the keys-only
# marker (a Secret with key names and empty values only, from lo build
# --keys-only: applied, it would replace each value in the cluster with an
# empty one), or when it cannot be read or checked. `lo build` never writes
# such an artifact; the guard catches a file written in another way. Else it
# prints the copy that passed the check, byte for byte: the caller uses it,
# not the file, which can change after the check, and captures it with a
# sentinel (see the rules at the top).
keysonly::read_checked_artifact() {
  [[ $- != *[TE]* ]] || { local - _ko_traps _ko_rc; _ko_traps="$(trap -p DEBUG RETURN ERR)"; trap - DEBUG RETURN ERR; set +xTE; "${FUNCNAME[0]}" "$@"; _ko_rc=$?; trap - DEBUG RETURN ERR; eval "${_ko_traps}"; return "${_ko_rc}"; }
  [[ $- != *x* ]] || { local -; set +x; }
  local domain="${1}" artifact="${2}" content marker=0
  if ! content="$(cat -- "${artifact}" 2>/dev/null && printf .)"; then
    error "refusing to apply ${domain}: ${artifact} cannot be read, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)"
    return 1
  fi
  content="${content%.}"
  printf '%s' "${content}" | keysonly::has_marker - || marker=$?
  case "${marker}" in
    1) printf '%s' "${content}"; return 0 ;;
    2) error "refusing to apply ${domain}: ${artifact} does not parse as YAML, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)" ;;
    3) error "refusing to apply ${domain}: ${artifact} cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only): $(keysonly::yq_cause)" ;;
    *) error "refusing to apply ${domain}: ${artifact} holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one — run 'lo build' again" ;;
  esac
  return 1
}

# keysonly::read_checked_file <prefix> <what> <file|->: print <file> (-
# reads stdin), read once and checked as keysonly::refuse_stream does (Go:
# build.ReadCheckedFile, build.ReadCheckedStream); a file or a stdin that
# cannot be read is refused too. The caller applies the printed copy (byte
# for byte), not the file. Returns 1, and prints nothing on stdout, when
# the guard refuses the input.
keysonly::read_checked_file() {
  [[ $- != *[TE]* ]] || { local - _ko_traps _ko_rc; _ko_traps="$(trap -p DEBUG RETURN ERR)"; trap - DEBUG RETURN ERR; set +xTE; "${FUNCNAME[0]}" "$@"; _ko_rc=$?; trap - DEBUG RETURN ERR; eval "${_ko_traps}"; return "${_ko_rc}"; }
  [[ $- != *x* ]] || { local -; set +x; }
  local prefix="${1}" what="${2}" content
  if ! content="$(cat -- "${3}" 2>/dev/null && printf .)"; then
    error "${prefix}: refusing to apply ${what}: it cannot be read, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)"
    return 1
  fi
  content="${content%.}"
  printf '%s' "${content}" | keysonly::refuse_stream "${prefix}" "${what}" - || return 1
  printf '%s' "${content}"
}
