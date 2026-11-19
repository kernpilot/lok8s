# shellcheck shell=bash
# curl_gate.bash — a source check of the curl calls that carry a credential.
#
#   curl_gate MODE FILE...   print one line for each curl call that breaks
#                            the rule of MODE, as "FILE:LINE: <reason>".
#                            rc 0 when there is no such call, 1 when there
#                            is, 2 on a usage error
#
# MODE q     -q must be the first argument of the call. curl obeys -q only
#            there, and then reads no config file of the user (.curlrc in
#            CURL_HOME or HOME, curlrc in XDG_CONFIG_HOME).
# MODE tls   the call must run right after `http::tls_log_vars_unset &&`
#            (utils/http.sh), in the subshell that runs curl.
# MODE list  print "FILE:LINE" for each call with a credential (the check
#            of the detection itself); rc 0
#
# A call carries a credential when its command (the lines that a trailing
# backslash, |, &&, || or ( joins) has one of: -K or --config (a curl
# config, the carrier of every credential in the tree), a header with
# Authorization:, -u or --user, -b or --cookie, http::curl_config or
# ${curl_config} (the config text), or a body on stdin (@-, the carrier of
# a request body with a secret). curl is a call only in a command position:
# at the start of the command, or after |, (, ;, &, !, {, $(, &&, || or
# the words then, do and else. A curl in a quoted string (an echo of a
# command for the user) and comment lines do not count.
# The check reads the source text only: it needs no shellcheck and runs
# no code of the tree.

curl_gate() {
  local mode="${1:-}"
  shift || :
  case "${mode}" in
    q | tls | list) ;;
    *) echo "curl_gate: MODE must be q, tls or list" >&2; return 2 ;;
  esac
  (( $# > 0 )) || { echo "curl_gate: no file" >&2; return 2; }
  local out
  out=$(LC_ALL=C awk -v mode="${mode}" '
    # One command: the text and, for each joined line, its start and its
    # line number.
    function reset() { buf = ""; n = 0 }
    function add(text) {
      if (n == 0) cmdfile = FILENAME
      n++
      starts[n] = length(buf) + 1
      lines[n] = FNR
      buf = buf text " "
    }
    function line_at(pos,   i, l) {
      l = lines[1]
      for (i = 1; i <= n; i++) if (starts[i] <= pos) l = lines[i]
      return l
    }
    function credential(s) {
      return s ~ /(^|[[:space:]])(-K|--config)([[:space:]=]|$)/ ||
        s ~ /Authorization:/ ||
        s ~ /(^|[[:space:]])(-u|--user|-b|--cookie)([[:space:]=]|$)/ ||
        s ~ /curl_config/ ||
        s ~ /@-/
    }
    function command_position(prefix) {
      sub(/[[:space:]]+$/, "", prefix)
      sub(/(^|[[:space:]])command$/, "", prefix)
      sub(/[[:space:]]+$/, "", prefix)
      if (prefix == "") return 1
      if (prefix ~ /[|(;&!{]$/) return 1
      if (prefix ~ /(^|[[:space:]])(then|do|else)$/) return 1
      return 0
    }
    function check(   s, off, p, prefix, rest, l) {
      if (buf == "" || !credential(buf)) { reset(); return }
      s = buf
      off = 0
      while (match(s, /(^|[^A-Za-z0-9_:.\/-])curl([[:space:]]|$)/)) {
        p = RSTART
        if (substr(s, p, 4) != "curl") p++
        prefix = substr(buf, 1, off + p - 1)
        rest = substr(s, p + 4)
        l = line_at(off + p)
        if (command_position(prefix)) {
          if (mode == "list")
            printf "%s:%d\n", cmdfile, l
          if (mode == "q" && rest !~ /^[[:space:]]+-q([[:space:]]|$)/)
            printf "%s:%d: a curl call with a credential: -q is not its first argument\n", cmdfile, l
          if (mode == "tls" && prefix !~ /http::tls_log_vars_unset[[:space:]]*&&[[:space:]]*$/)
            printf "%s:%d: a curl call with a credential does not run after http::tls_log_vars_unset &&\n", cmdfile, l
        }
        off += p + 3
        s = substr(s, p + 4)
      }
      reset()
    }
    FNR == 1 { check(); reset() }
    # A comment line neither ends nor continues a command.
    /^[[:space:]]*#/ { next }
    {
      text = $0
      add(text)
      if (text ~ /(\\|\||&&|\(|\$\()[[:space:]]*$/) next
      check()
    }
    END { check() }
  ' "$@") || { echo "curl_gate: awk failed" >&2; return 2; }
  [[ -z "${out}" ]] && return 0
  printf '%s\n' "${out}"
  [[ "${mode}" == list ]] || return 1
}

# curl_gate_files — every file of the bash tree that names curl (the
# scaffolding under init.d is TypeScript for a user project, not framework
# code), one path per line. Fails loudly on a near-empty result: a broken
# predicate must not read as "nothing to find".
curl_gate_files() {
  local -a files=()
  mapfile -t files < <(grep -rlw curl "${_PROJECT_ROOT}/.lok8s" --exclude-dir=init.d | LC_ALL=C sort)
  (( ${#files[@]} >= 15 )) || {
    echo "curl_gate_files found only ${#files[@]} file(s) under .lok8s: the discovery is broken, not the tree" >&2
    return 1
  }
  printf '%s\n' "${files[@]}"
}
