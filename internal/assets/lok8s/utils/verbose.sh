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

# A jq def: a name from an API (a cloud part, a plan line) as a message
# shows it. Each control character (C0, DEL and C1), each backslash and
# each bidi or zero-width format character (U+200B to U+200F, U+202A to
# U+202E, U+2066 to U+2069) becomes '?', as ls shows a name on a terminal.
# A newline would split a line, an ESC would reach the terminal, a
# backslash sequence would become a control character in `echo -e`, and a
# bidi character would turn the text after it around. Other characters
# stay, also multibyte ones. The Go twin is ui.Shown (internal/ui).
VERBOSE_JQ_SHOWN_NAME='def shown_name: tostring | explode
  | map(if . < 32 or (. >= 127 and . < 160) or . == 92
      or (. >= 8203 and . <= 8207) or (. >= 8234 and . <= 8238) or (. >= 8294 and . <= 8297)
    then 63 else . end) | implode;'

# verbose::shown_names: each line of stdin as shown_name shows it, one line
# each. rc of jq: a caller that shows a name stops when it fails.
verbose::shown_names() {
  jq -Rr "${VERBOSE_JQ_SHOWN_NAME}"' shown_name'
}
