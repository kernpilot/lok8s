# shellcheck shell=bash
# verbose.sh — logging helpers (debug, error, warn)
# Provides colored output when argsh :log is not sufficient. The colour is
# terminal presentation only: it prints when stderr is a terminal and
# NO_COLOR (https://no-color.org) is unset; piped stderr carries the plain
# prefix, byte for byte what the Go binary prints (internal/ui).

: "${GREEN:=\033[0;32m}"
: "${RED:=\033[0;31m}"
: "${YELLOW:=\033[0;33m}"
: "${NC:=\033[0m}"

# verbose::color — 0 when stderr may carry ANSI colour.
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
