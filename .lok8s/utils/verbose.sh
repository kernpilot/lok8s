# shellcheck shell=bash
# verbose.sh — logging helpers (debug, error, warn)
# Provides colored output when argsh :log is not sufficient. The colour is
# terminal presentation only: it prints when stderr is a terminal and
# NO_COLOR (https://no-color.org) is empty or unset. Piped stderr carries the plain
# prefix, byte for byte what the Go binary prints (internal/ui).

: "${GREEN:=\033[0;32m}"
: "${RED:=\033[0;31m}"
: "${YELLOW:=\033[0;33m}"
: "${NC:=\033[0m}"

# verbose::color returns 0 when stderr may carry ANSI colour.
verbose::color() {
  [[ -t 2 && -z "${NO_COLOR:-}" ]]
}

debug() {
  [[ -n "${DEBUG:-}" ]] || return 0
  if verbose::color; then
    echo -e "${GREEN}[debug]${NC} ${*}" >&2
  else
    echo -e "[debug] ${*}" >&2
  fi
}

error() {
  if verbose::color; then
    echo -e "${RED}[error]${NC} ${*}" >&2
  else
    echo -e "[error] ${*}" >&2
  fi
}

warn() {
  if verbose::color; then
    echo -e "${YELLOW}[warn]${NC} ${*}" >&2
  else
    echo -e "[warn] ${*}" >&2
  fi
}

# A jq def: unshown is true for a code point that a message must not show
# as it is. These are:
#   - each control character (C0, DEL and C1);
#   - the line and paragraph separators (U+2028, U+2029);
#   - these bidi, zero-width and invisible format characters: U+00AD,
#     U+061C, U+200B to U+200F, U+202A to U+202E, U+2060 to U+2064, U+2066
#     to U+2069, U+206A to U+206F and U+FEFF;
#   - the Hangul fillers U+115F, U+1160, U+3164 and U+FFA0;
#   - the variation selectors (U+180B to U+180F with the Mongolian vowel
#     separator, U+FE00 to U+FE0F, U+E0100 to U+E01EF) and the tag
#     characters (U+E0000 to U+E007F).
# The list is explicit: other invisible characters (for example U+034F or
# U+17B4) stay. A newline or a separator would split a line, an ESC would
# reach the terminal, a bidi character would turn the text after it around,
# and an invisible one of the list would hide in the name. Thus an emoji
# also loses its variation selector, as it loses a zero-width joiner
# (U+200D). shown_name replaces each one with '?'. The `clean` of the
# kubehz agent records (libs/kubehz/agent) drops each one. The Go twin is
# ui.Unshown (internal/ui). Each range is a clause of its own (one mutant
# each).
VERBOSE_JQ_UNSHOWN='def unshown: . < 32 or (. >= 127 and . < 160)
  or . == 173
  or . == 1564
  or (. >= 4447 and . <= 4448)
  or (. >= 6155 and . <= 6159)
  or (. >= 8203 and . <= 8207)
  or (. >= 8232 and . <= 8233)
  or (. >= 8234 and . <= 8238)
  or (. >= 8288 and . <= 8292)
  or (. >= 8294 and . <= 8297)
  or (. >= 8298 and . <= 8303)
  or . == 12644
  or (. >= 65024 and . <= 65039)
  or . == 65279
  or . == 65440
  or (. >= 917504 and . <= 917631)
  or (. >= 917760 and . <= 917999);'

# A jq def: a name from an API (a cloud part, a plan line) as a message
# shows it, as ls shows a name on a terminal: each code point of unshown,
# and each backslash, becomes '?'. A backslash sequence would become a
# control character in `echo -e`. Other characters stay, also multibyte
# ones. The Go twin is ui.Shown (internal/ui).
VERBOSE_JQ_SHOWN_NAME="${VERBOSE_JQ_UNSHOWN}"'def shown_name: tostring | explode
  | map(if unshown or . == 92 then 63 else . end) | implode;'

# verbose::shown_names: each line of stdin as shown_name shows it, one line
# each. rc of jq: a caller that shows a name stops when it fails.
verbose::shown_names() {
  jq -Rr "${VERBOSE_JQ_SHOWN_NAME}"' shown_name'
}
