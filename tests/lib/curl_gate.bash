# shellcheck shell=bash
# curl_gate.bash — a source check of the curl calls of the bash tree.
#
#   curl_gate MODE FILE...   print one line for each curl call that breaks
#                            the rule of MODE, as "FILE:LINE: <reason>".
#                            rc 0 when there is no such call, 1 when there
#                            is, 2 on a usage error
#
# MODE q     each curl call passes -q as its first argument, with or
#            without a credential. curl obeys -q only there, and then reads
#            no config file of the user (.curlrc in CURL_HOME or HOME,
#            curlrc in XDG_CONFIG_HOME).
# MODE tls   each curl call runs as the first command after
#            http::tls_log_vars_unset (utils/http.sh), in this form only:
#              ( http::tls_log_vars_unset && curl -q …
#            The helper is the first command of the subshell (the "(" is
#            not "$(", "<(" or ">("), and only "&&" joins it to curl, with
#            "umask NNN &&" between them as the one other command. A pipe,
#            ";" or "||" between them, or the helper outside the subshell,
#            is a finding. Two exceptions: a file under a manifests/
#            directory (a script that runs in its own image, without
#            utils/http.sh), and a call that an entry of CURL_GATE_ALLOW
#            names. An entry must name exactly one call of its file, and
#            that call must carry no credential: else the entry is a
#            finding.
# MODE list  print "FILE:LINE" for each curl call, with " credential" when
#            the call carries one (the check of the detection itself); rc 0
#
# In both q and tls modes, curl named through a variable is a finding: an
# assignment whose value is curl or ends in /curl (c=curl, c=(curl …),
# local c=/usr/bin/curl) or a default expansion ${X:-curl}. The gate cannot
# see the call of such a name.
#
# A curl call is the word curl, \curl, "curl" or a path that ends in /curl
# in a command position: at the start of a command, after |, (, ;, &, !,
# {, $(, <(, &&, ||, a YAML list item "- ", or the words if, then, do,
# else, elif, while, until and time. Before it can stand wrappers: command,
# exec, env, xargs, timeout, nice, nohup, sudo, stdbuf, setsid, builtin,
# their options and values, and assignments (VAR=x curl). A curl in a
# quoted string (an echo of a command for the user) and comment lines do
# not count. The lines that a trailing backslash, |, &&, || or ( joins are
# one command.
#
# A call carries a credential when its command has one of: -K or --config
# (a curl config), an Authorization header, a header in a variable
# (-H "${…}"), -u, -uUSER:PW, --user, -b, --cookie, --oauth2-bearer, -U,
# --proxy-user, a body (-d, -dX, --data*, -F, --form, --json), a body on
# stdin (@-), http::curl_config or curl_config (the config text), a user and
# password in a URL (scheme://user:pw@host) or a query key token, sig,
# signature, key, secret, password or X-Amz-Signature.
#
# The check reads the source text only: it needs no shellcheck and runs no
# code of the tree.

# CURL_GATE_ALLOW: the calls that run without the TLS helper. One entry for
# each call: the path of the file in the tree (it matches a file whose path
# ends with it), an extended regular expression and the reason. The
# expression is matched against the command of the call, with "@@" just
# before the curl word of that call (so two calls of one command differ).
CURL_GATE_ALLOW=()

# curl_gate_allow PATH ERE REASON: add an entry to CURL_GATE_ALLOW.
curl_gate_allow() {
  (( $# == 3 )) && [[ -n "${1}" && -n "${2}" && -n "${3}" ]] || {
    echo "curl_gate_allow: PATH ERE REASON" >&2
    return 2
  }
  CURL_GATE_ALLOW+=("${1}"$'\t'"${2}"$'\t'"${3}")
}

curl_gate_allow libs/image '^[[:space:]]*@@curl -q -s "\$\{catalog_url\}" \| jq' \
  "no credential: the catalog of the local cache registry"
curl_gate_allow libs/image '\|\| @@curl -q -s "\$\{catalog_url\}"' \
  "no credential: the second read of that catalog"
curl_gate_allow drivers/lo/libs/registry '@@curl -q -sf --connect-timeout 1 --max-time 2 "\$\{endpoint\}/v2/_catalog"' \
  "no credential: the catalog of a local registry"
curl_gate_allow drivers/capi/cluster/core/kubeadm-control-plane.yaml '@@curl -q -fsSL https://pkgs\.k8s\.io/' \
  "no credential: cloud-init of a new node, which has no utils/http.sh"
curl_gate_allow drivers/capi/cluster/providers/hetzner/kubeadm-config-template.yaml '@@curl -q -fsSL https://pkgs\.k8s\.io/' \
  "no credential: cloud-init of a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fLo /usr/local/bin/kind ' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fsSL https://dl\.k8s\.io/release/stable\.txt' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fLo /usr/local/bin/kubectl ' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fsSL "https://github\.com/kubernetes-sigs/kustomize/' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fLo /usr/local/bin/yq ' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fsSL "https://github\.com/tilt-dev/tilt/' \
  "no credential: a public download on a new node, which has no utils/http.sh"

curl_gate() {
  local mode="${1:-}"
  shift || :
  case "${mode}" in
    q | tls | list) ;;
    *) echo "curl_gate: MODE must be q, tls or list" >&2; return 2 ;;
  esac
  (( $# > 0 )) || { echo "curl_gate: no file" >&2; return 2; }
  local out allow
  allow=$(printf '%s\n' "${CURL_GATE_ALLOW[@]}")
  out=$(CURL_GATE_ALLOW_TEXT="${allow}" LC_ALL=C awk -v mode="${mode}" '
    BEGIN {
      SQ = sprintf("%c", 39)
      na = 0
      if (ENVIRON["CURL_GATE_ALLOW_TEXT"] != "") {
        k = split(ENVIRON["CURL_GATE_ALLOW_TEXT"], entries, "\n")
        for (i = 1; i <= k; i++) {
          if (entries[i] == "") continue
          if (split(entries[i], f, "\t") != 3) { print "curl_gate: an allowlist entry is not PATH<tab>ERE<tab>REASON" > "/dev/stderr"; bad = 1; exit 2 }
          na++
          apath[na] = f[1]; aere[na] = f[2]; acount[na] = 0; aseen[na] = 0
        }
      }
    }
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
    function path_is(file, path) {
      return file == path || substr(file, length(file) - length(path)) == "/" path
    }
    function credential(s) {
      return s ~ /(^|[[:space:]])(-K|--config)([[:space:]=]|$)/ ||
        s ~ /[Aa]uthorization:/ ||
        s ~ /(^|[[:space:]])(-H|--header)[[:space:]=]+["\047]?\$/ ||
        s ~ /(^|[[:space:]])(-u|--user|-b|--cookie|--oauth2-bearer|-U|--proxy-user)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])-u[^[:space:]-]/ ||
        s ~ /(^|[[:space:]])(-d|--data|--data-raw|--data-binary|--data-urlencode|-F|--form|--json)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])-d[^[:space:]-]/ ||
        s ~ /@-/ ||
        s ~ /curl_config/ ||
        s ~ /:\/\/[^\/[:space:]"\047@]*@/ ||
        s ~ /[?&](token|access_token|sig|signature|key|secret|password|X-Amz-Signature)=/
    }
    # The quote at position p of s: "s" in single quotes, "d" in double
    # quotes (and not in a $( ) inside them), "" in code.
    function quote_at(s, p,   i, c, top, st, dp) {
      top = 1; st[1] = "c"; dp[1] = 0
      for (i = 1; i < p; i++) {
        c = substr(s, i, 1)
        if (st[top] == "s") { if (c == SQ) top--; continue }
        if (c == "\\") { i++; continue }
        if (st[top] == "d") {
          if (c == "\"") top--
          else if (c == "$" && substr(s, i + 1, 1) == "(") { top++; st[top] = "c"; dp[top] = 0; i++ }
          continue
        }
        if (c == SQ) { top++; st[top] = "s"; continue }
        if (c == "\"") { top++; st[top] = "d"; continue }
        if (c == "(") { dp[top]++; continue }
        if (c == ")") { if (dp[top] > 0) dp[top]--; else if (top > 1) top--; continue }
      }
      return st[top] == "c" ? "" : st[top]
    }
    function quoted(s, p) { return quote_at(s, p) != "" }
    function last_word(s) { return match(s, /[^[:space:]]+$/) ? substr(s, RSTART) : "" }
    function drop_last(s) { sub(/[^[:space:]]+[[:space:]]*$/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    function is_wrapper(w) { return w ~ /^(command|exec|env|xargs|timeout|nice|nohup|sudo|stdbuf|setsid|builtin)$/ }
    function is_assign(w) { return w ~ /^[A-Za-z_][A-Za-z0-9_]*=[^|;&()<>`"\047]*$/ }
    # The prefix without the wrappers, options and assignments before curl.
    # "command -v curl" (or -V) looks curl up and runs nothing: the prefix
    # then stays as it is, which is no command position.
    function strip_wrappers(p,   w, t, k, found, lookup) {
      while (1) {
        sub(/[[:space:]]+$/, "", p)
        w = last_word(p)
        if (w == "") return p
        if (is_wrapper(w) || is_assign(w)) { p = drop_last(p); continue }
        t = p; k = 0; found = 0; lookup = 0
        while (k < 8) {
          w = last_word(t)
          if (w == "" || w ~ /[|;&(){}<>`]/) break
          if (is_wrapper(w)) { found = 1; break }
          if (w ~ /^-[A-Za-z]*[vV]/) lookup = 1
          t = drop_last(t); k++
        }
        if (found && k > 0 && !(lookup && w == "command")) { p = t; continue }
        return p
      }
    }
    function command_position(p) {
      sub(/[[:space:]]+$/, "", p)
      if (p == "") return 1
      if (p ~ /[|(;&!{]$/) return 1
      if (p ~ /(^|[[:space:]])(if|then|do|else|elif|while|until|time)$/) return 1
      if (p ~ /^[[:space:]]*-$/) return 1
      return 0
    }
    function helper_form(p) {
      return p ~ /(^|[^$<>])\([[:space:]]*http::tls_log_vars_unset[[:space:]]*&&[[:space:]]*(umask[[:space:]]+[0-7]+[[:space:]]*&&[[:space:]]*)*$/
    }
    function report(l, text) { printf "%s:%d: %s\n", cmdfile, l, text }
    # curl named through a variable: each match of re in the command, in
    # code (and in double quotes too when dq is 1: an expansion there).
    function indirect(re, dq,   s, off, p, qa) {
      s = buf; off = 0
      while (match(s, re)) {
        p = off + RSTART
        qa = quote_at(buf, p)
        if (qa == "" || (dq && qa == "d"))
          report(line_at(p), "curl named through a variable: the gate cannot check that call")
        off += RSTART + RLENGTH - 1
        s = substr(s, RSTART + RLENGTH)
      }
    }
    function check(   s, off, c0, b, adv, prefix, stripped, rest, l, marked, i, hit, cred, manifest) {
      if (buf == "") { reset(); return }
      if (mode != "list") {
        indirect("(^|[[:space:];&|({])((local|declare|typeset|readonly|export)[[:space:]]+(-[A-Za-z]+[[:space:]]+)*)?[A-Za-z_][A-Za-z0-9_]*(\\[[^]]*\\])?\\+?=\\(?[[:space:]]*[\"\047]?([^[:space:]\"\047()]*/)?curl[\"\047]?([[:space:]);]|$)", 0)
        indirect("\\$\\{[A-Za-z_][A-Za-z0-9_]*:?[-=][^}]*curl[\"\047]?\\}", 1)
      }
      manifest = cmdfile ~ /(^|\/)manifests\//
      s = buf; off = 0
      while (match(s, /curl["\047]?([[:space:]]|$)/)) {
        c0 = off + RSTART
        adv = RSTART + RLENGTH - 1
        rest = substr(s, RSTART + RLENGTH)
        # The start of the word: back over a path that ends in /, then an
        # opening quote, then a backslash.
        b = c0
        if (b > 1 && substr(buf, b - 1, 1) == "/") {
          b--
          while (b > 1 && substr(buf, b - 1, 1) !~ /[[:space:]"\047|&;()<>]/) b--
        }
        if (b > 1 && substr(buf, b - 1, 1) ~ /["\047]/) b--
        if (b > 1 && substr(buf, b - 1, 1) == "\\") b--
        off += adv
        s = substr(s, adv + 1)
        # The byte before the word must not be part of a word (mycurl, a.curl,
        # the value of c=curl: that assignment is the finding above).
        if (b > 1 && substr(buf, b - 1, 1) ~ /[A-Za-z0-9_:.\/=-]/) continue
        prefix = substr(buf, 1, b - 1)
        stripped = strip_wrappers(prefix)
        if (quoted(buf, b) || !command_position(stripped)) continue
        l = line_at(b)
        cred = credential(buf)
        if (mode == "list")
          printf "%s:%d%s\n", cmdfile, l, cred ? " credential" : ""
        if (mode == "q" && rest !~ /^[[:space:]]*-q([[:space:]]|$)/)
          report(l, "a curl call: -q is not its first argument")
        if (mode == "tls" && !manifest && !helper_form(stripped)) {
          marked = prefix "@@" substr(buf, b)
          hit = 0
          for (i = 1; i <= na; i++) {
            if (!path_is(cmdfile, apath[i])) continue
            if (marked ~ aere[i]) { acount[i]++; hit = 1 }
          }
          if (!hit)
            report(l, "a curl call does not run as ( http::tls_log_vars_unset && curl … (the helper first in its subshell)")
          else if (cred)
            report(l, "a curl call in the allowlist carries a credential")
        }
      }
      reset()
    }
    FNR == 1 {
      check(); reset()
      for (i = 1; i <= na; i++) if (path_is(FILENAME, apath[i])) aseen[i] = 1
    }
    # A comment line neither ends nor continues a command.
    /^[[:space:]]*#/ { next }
    {
      text = $0
      add(text)
      if (text ~ /(\\|\||&&|\(|\$\()[[:space:]]*$/) next
      check()
    }
    END {
      if (bad) exit 2
      check()
      if (mode == "tls")
        for (i = 1; i <= na; i++)
          if (aseen[i] && acount[i] != 1)
            printf "%s: the allowlist entry /%s/ matches %d curl calls, want 1\n", apath[i], aere[i], acount[i]
    }
  ' "$@") || { echo "curl_gate: awk failed" >&2; return 2; }
  [[ -z "${out}" ]] && return 0
  printf '%s\n' "${out}"
  [[ "${mode}" == list ]] || return 1
}

# curl_gate_files — every file of the bash tree that names curl, one path
# per line. Left out: the scaffolding under init.d (TypeScript for a user
# project, not framework code) and the package list of the hetzner
# cloud-init (a list of package names, not a script). Fails loudly on a
# near-empty result: a broken predicate must not read as "nothing to find".
curl_gate_files() {
  local -a files=()
  mapfile -t files < <(grep -rlw curl "${_PROJECT_ROOT}/.lok8s" --exclude-dir=init.d |
    grep -vx "${_PROJECT_ROOT}/.lok8s/providers/hetzner/cloud-init/packages" | LC_ALL=C sort)
  (( ${#files[@]} >= 15 )) || {
    echo "curl_gate_files found only ${#files[@]} file(s) under .lok8s: the discovery is broken, not the tree" >&2
    return 1
  }
  printf '%s\n' "${files[@]}"
}
