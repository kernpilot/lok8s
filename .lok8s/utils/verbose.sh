# shellcheck shell=bash
# verbose.sh — logging helpers (debug, error, warn)
# Provides colored output when argsh :log is not sufficient.

: "${GREEN:=\033[0;32m}"
: "${RED:=\033[0;31m}"
: "${YELLOW:=\033[0;33m}"
: "${NC:=\033[0m}"

debug() {
  [[ -z "${DEBUG:-}" ]] || echo -e "${GREEN}[debug]${NC} $*" >&2
}

error() {
  echo -e "${RED}[error]${NC} $*" >&2
}

warn() {
  echo -e "${YELLOW}[warn]${NC} $*" >&2
}
