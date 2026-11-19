# shellcheck shell=bash disable=SC2016
# (SC2016 for the whole file: the texts of the allowlist below are source
# text, not code, so their $ stays literal.)
#
# curl_gate.bash — a source check of the curl calls of the bash tree.
#
# The gate is a heuristic against a mistake of a maintainer, not a defence
# against a hostile author. It reads the text, closes the forms below and
# names the forms that it does not see (at the end of this header).
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
#              ( http::tls_log_vars_unset && umask NNN && timeout N curl -q …
#            The helper is the first command of the subshell (the "(" is
#            not "$(", "<(" or ">("), and only "&&" joins it to curl.
#            Between them, only "umask NNN &&" and then "timeout N" (a
#            number, no option) can stand. An assignment (NAME=value curl),
#            env, command, exec, sudo or any other word there is a finding,
#            and so is a pipe, ";" or "||" between them, or the helper
#            outside the subshell. Two exceptions: a file under a
#            manifests/ directory (a script that runs in its own image,
#            without utils/http.sh), and a call that an entry of
#            CURL_GATE_ALLOW names. An entry pins the full text of its
#            command (see CURL_GATE_ALLOW). It must match exactly one call
#            of its file, that call must carry no credential, and the run
#            must read its file: else the entry is a finding.
# MODE list  print "FILE:LINE" for each curl call, with " credential" when
#            the call carries one (the check of the detection itself); rc 0
#
# In both q and tls modes, two more forms are findings, because the gate
# cannot see the call that they make:
#   - curl named through a variable: an assignment whose value is curl or
#     ends in /curl (c=curl, c=(curl …), local c=/usr/bin/curl) or a
#     default expansion ${X:-curl};
#   - an eval in a command position whose command holds the text curl
#     (eval 'curl …'), or a variable that such an assignment or expansion
#     names, in any file of the run (c=curl, then eval "${c} …").
#
# A curl call is the word curl, \curl, "curl" or a path that ends in /curl
# in a command position: at the start of a command, after |, (, ;, &, !,
# {, $(, <(, &&, ||, a YAML list item "- ", or the words if, then, do,
# else, elif, while, until and time. Before it can stand wrappers: command,
# exec, env, xargs, timeout, nice, nohup, sudo, stdbuf, setsid, builtin,
# their options and values, and assignments (VAR=x curl). A curl in a
# quoted string (an echo of a command for the user) and comment lines do
# not count. Before any other step, a line that ends in a backslash is
# joined to the next line without the backslash and the newline, as the
# shell does (so cu\<newline>rl is curl). The lines that a trailing |, &&,
# || or ( joins are one command.
#
# A call carries a credential when its command has one of: -K or --config
# (a curl config), an Authorization header, a header in a variable
# (-H "${…}"), -u, -uUSER:PW, --user, -b, --cookie, --oauth2-bearer, -U,
# --proxy-user, a body (-d, -dX, --data*, -F, --form, --json), a body on
# stdin (@-), http::curl_config or curl_config (the config text), a user and
# password in a URL (scheme://user:pw@host) or a query key token, sig,
# signature, key, secret, password or X-Amz-Signature. It also carries one
# when its own arguments (up to the next |, ;, &, or the ")" that ends its
# subshell) have one of: a short option or a group of short options with
# n, E, K, u, b, d, F, T or U (-n, -E, -sK, -Ecert.pem), --netrc,
# --netrc-file, --netrc-optional, --cert, --key, --proxy-cert,
# --proxy-key, -T, --upload-file, --variable, --form-string, --data-*, a
# header from a file (-H @file, --header @file) or a header with an
# expansion in its value (-H "X-API-Key: ${TOKEN}").
#
# The forms that the gate does not see (the residuals): a name built from
# parts (c="cu""rl", printf, a generated file that is sourced), and an eval
# whose command holds neither the text curl nor a variable that the gate
# saw named as curl (eval "${cmd}", with cmd set from other text). Such an
# eval is outside the gate. The evals of the tree today hold neither.
#
# The check reads the source text only: it needs no shellcheck and runs no
# code of the tree.

# CURL_GATE_ALLOW: the calls that run without the TLS helper. One entry for
# each call: the path of the file in the tree (it matches a file whose path
# ends with it), the full text of the command of the call and the reason.
# The text is the command after the join of its lines, with "@@" just
# before the curl word of that call (so two calls of one command differ).
# A run of white space counts as one space, and white space at the start
# and the end does not count. An added argument, an array expansion or a
# header changes the text: then the entry matches no call, and the call is
# a finding.
CURL_GATE_ALLOW=()

# curl_gate_allow PATH TEXT REASON: add an entry to CURL_GATE_ALLOW.
curl_gate_allow() {
  (( $# == 3 )) && [[ -n "${1}" && -n "${2}" && -n "${3}" ]] || {
    echo "curl_gate_allow: PATH TEXT REASON" >&2
    return 2
  }
  CURL_GATE_ALLOW+=("${1}"$'\t'"${2}"$'\t'"${3}")
}

curl_gate_allow libs/image '@@curl -q -s "${catalog_url}" | jq . 2>/dev/null || curl -q -s "${catalog_url}"' \
  "no credential: the catalog of the local cache registry"
curl_gate_allow libs/image 'curl -q -s "${catalog_url}" | jq . 2>/dev/null || @@curl -q -s "${catalog_url}"' \
  "no credential: the second read of that catalog"
curl_gate_allow drivers/lo/libs/registry 'if catalog=$(@@curl -q -sf --connect-timeout 1 --max-time 2 "${endpoint}/v2/_catalog" 2>/dev/null); then' \
  "no credential: the catalog of a local registry"
curl_gate_allow drivers/capi/cluster/core/kubeadm-control-plane.yaml '- @@curl -q -fsSL https://pkgs.k8s.io/core:/stable:/v$TRIMMED_KUBERNETES_VERSION/deb/Release.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg' \
  "no credential: cloud-init of a new node, which has no utils/http.sh"
curl_gate_allow drivers/capi/cluster/providers/hetzner/kubeadm-config-template.yaml '- @@curl -q -fsSL https://pkgs.k8s.io/core:/stable:/v$TRIMMED_KUBERNETES_VERSION/deb/Release.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg' \
  "no credential: cloud-init of a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fLo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${ARCH}"' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh 'KUBECTL_VERSION="$(@@curl -q -fsSL https://dl.k8s.io/release/stable.txt)"' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fsSL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/kustomize_${KUSTOMIZE_VERSION}_linux_${ARCH}.tar.gz" | tar xz -C /usr/local/bin kustomize' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fLo /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${ARCH}"' \
  "no credential: a public download on a new node, which has no utils/http.sh"
curl_gate_allow providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh '@@curl -q -fsSL "https://github.com/tilt-dev/tilt/releases/download/v${TILT_VERSION}/tilt.${TILT_VERSION}.linux.${ARCH}.tar.gz" | tar xz -C /usr/local/bin tilt' \
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
    # A run of white space as one space, without white space at the ends.
    function norm(t) { gsub(/[[:space:]]+/, " ", t); sub(/^ /, "", t); sub(/ $/, "", t); return t }
    BEGIN {
      SQ = sprintf("%c", 39)
      na = 0; ne = 0
      if (ENVIRON["CURL_GATE_ALLOW_TEXT"] != "") {
        k = split(ENVIRON["CURL_GATE_ALLOW_TEXT"], entries, "\n")
        for (i = 1; i <= k; i++) {
          if (entries[i] == "") continue
          if (split(entries[i], f, "\t") != 3) { print "curl_gate: an allowlist entry is not PATH<tab>TEXT<tab>REASON" > "/dev/stderr"; bad = 1; exit 2 }
          na++
          apath[na] = f[1]; atext[na] = norm(f[2]); acount[na] = 0; aseen[na] = 0
        }
      }
    }
    # One command: the text and, for each joined line, its start and its
    # line number. A line that ends in a backslash (cont) joins the next
    # line without a space: the backslash and the newline go away.
    function reset() { buf = ""; n = 0 }
    function add(text, cont) {
      if (n == 0) cmdfile = FILENAME
      n++
      starts[n] = length(buf) + 1
      lines[n] = FNR
      buf = buf text (cont ? "" : " ")
    }
    function line_at(pos,   i, l) {
      l = lines[1]
      for (i = 1; i <= n; i++) if (starts[i] <= pos) l = lines[i]
      return l
    }
    function path_is(file, path) {
      return file == path || substr(file, length(file) - length(path)) == "/" path
    }
    # A credential anywhere in the command.
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
    # A credential in the arguments of the call itself (a short option of
    # another command of the pipeline, head -n or grep -E, does not count).
    function credential_call(s) {
      return s ~ /(^|[[:space:]])-[A-Za-z]*[nEKubdFTU]/ ||
        s ~ /(^|[[:space:]])--(netrc|netrc-file|netrc-optional|cert|key|proxy-cert|proxy-key|upload-file|variable|form-string|data-[a-z]+)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])(-H|--header|--proxy-header)[[:space:]=]*["\047]?@/ ||
        s ~ /(^|[[:space:]])(-H|--header|--proxy-header)[[:space:]=]*("[^"]*|[^[:space:]"\047]*)\$/
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
    # The end of the call that starts at b: the first |, ; or & (not of a
    # redirection: 2>&1, &>) in code, or the ")" that closes its subshell,
    # else the end of the command.
    function call_end(b,   i, c, L, top, st, dp, pc, nc) {
      L = length(buf); top = 1; st[1] = "c"; dp[1] = 0
      for (i = b; i <= L; i++) {
        c = substr(buf, i, 1)
        if (st[top] == "s") { if (c == SQ) top--; continue }
        if (c == "\\") { i++; continue }
        if (st[top] == "d") {
          if (c == "\"") top--
          else if (c == "$" && substr(buf, i + 1, 1) == "(") { top++; st[top] = "c"; dp[top] = 0; i++ }
          continue
        }
        if (c == SQ) { top++; st[top] = "s"; continue }
        if (c == "\"") { top++; st[top] = "d"; continue }
        if (c == "$" && substr(buf, i + 1, 1) == "(") { top++; st[top] = "c"; dp[top] = 0; i++; continue }
        if (c == "(") { dp[top]++; continue }
        if (c == ")") {
          if (dp[top] > 0) { dp[top]--; continue }
          if (top > 1) { top--; continue }
          return i
        }
        if (top > 1 || dp[1] > 0) continue
        if (c == "|" || c == ";") return i
        if (c == "&") {
          pc = substr(buf, i - 1, 1); nc = substr(buf, i + 1, 1)
          if (pc != ">" && pc != "<" && nc != ">") return i
        }
      }
      return L + 1
    }
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
    # The text just before the curl word: the subshell, the helper, &&,
    # then at most "umask NNN &&" and "timeout N". The raw text, not the
    # text without the wrappers: a word that the wrapper step drops (an
    # assignment, env, command) breaks the form.
    function helper_form(p) {
      return p ~ /(^|[^$<>])\([[:space:]]*http::tls_log_vars_unset[[:space:]]*&&[[:space:]]*(umask[[:space:]]+[0-7]+[[:space:]]*&&[[:space:]]*)?(timeout[[:space:]]+[0-9]+[smhd]?[[:space:]]+)?$/
    }
    function report(l, text) { printf "%s:%d: %s\n", cmdfile, l, text }
    # curl named through a variable: each match of re in the command, in
    # code (and in double quotes too when dq is 1: an expansion there). The
    # name of the variable goes into curlname, for the eval rule.
    function indirect(re, dq, kind,   s, off, p, qa, rs, rl, m, nm) {
      s = buf; off = 0
      while (match(s, re)) {
        rs = RSTART; rl = RLENGTH
        p = off + rs
        m = substr(s, rs, rl)
        off += rs + rl - 1
        s = substr(s, rs + rl)
        qa = quote_at(buf, p)
        if (qa != "" && !(dq && qa == "d")) continue
        report(line_at(p), "curl named through a variable: the gate cannot check that call")
        if (kind == "assign" && match(m, /[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=/)) {
          nm = substr(m, RSTART, RLENGTH); sub(/[[+=].*$/, "", nm); curlname[nm] = 1
        }
        if (kind == "default" && match(m, /\{[A-Za-z_][A-Za-z0-9_]*/))
          curlname[substr(m, RSTART + 1, RLENGTH - 1)] = 1
      }
    }
    # An eval in a command position: a finding when its command holds the
    # text curl; else kept for END, which checks it against curlname.
    function evals(   s, off, e0, b, rs, rl) {
      s = buf; off = 0
      while (match(s, /eval["\047]?([[:space:]]|$)/)) {
        rs = RSTART; rl = RLENGTH
        e0 = off + rs
        off += rs + rl - 1
        s = substr(s, rs + rl)
        b = e0
        if (b > 1 && substr(buf, b - 1, 1) ~ /["\047]/) b--
        if (b > 1 && substr(buf, b - 1, 1) == "\\") b--
        if (b > 1 && substr(buf, b - 1, 1) ~ /[A-Za-z0-9_:.\/=-]/) continue
        if (quoted(buf, b) || !command_position(strip_wrappers(substr(buf, 1, b - 1)))) continue
        if (buf ~ /curl/)
          report(line_at(b), "an eval of a command that holds curl: the gate cannot check that call")
        else { ne++; evtext[ne] = buf; evfile[ne] = cmdfile; evline[ne] = line_at(b) }
      }
    }
    function check(   s, off, c0, b, adv, prefix, stripped, rest, l, marked, i, hit, cred, manifest) {
      if (buf == "") { reset(); return }
      if (mode != "list") {
        indirect("(^|[[:space:];&|({])((local|declare|typeset|readonly|export)[[:space:]]+(-[A-Za-z]+[[:space:]]+)*)?[A-Za-z_][A-Za-z0-9_]*(\\[[^]]*\\])?\\+?=\\(?[[:space:]]*[\"\047]?([^[:space:]\"\047()]*/)?curl[\"\047]?([[:space:]);]|$)", 0, "assign")
        indirect("\\$\\{[A-Za-z_][A-Za-z0-9_]*:?[-=][^}]*curl[\"\047]?\\}", 1, "default")
        evals()
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
        cred = credential(buf) || credential_call(substr(buf, b, call_end(b) - b))
        if (mode == "list")
          printf "%s:%d%s\n", cmdfile, l, cred ? " credential" : ""
        if (mode == "q" && rest !~ /^[[:space:]]*-q([[:space:]]|$)/)
          report(l, "a curl call: -q is not its first argument")
        if (mode == "tls" && !manifest && !helper_form(prefix)) {
          marked = norm(prefix "@@" substr(buf, b))
          hit = 0
          for (i = 1; i <= na; i++) {
            if (!path_is(cmdfile, apath[i])) continue
            if (marked == atext[i]) { acount[i]++; hit = 1 }
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
      cont = 0
      if (match(text, /\\+$/) && RLENGTH % 2 == 1) { text = substr(text, 1, length(text) - 1); cont = 1 }
      add(text, cont)
      if (cont || $0 ~ /(\\|\||&&|\(|\$\()[[:space:]]*$/) next
      check()
    }
    END {
      if (bad) exit 2
      check()
      for (i = 1; i <= ne; i++)
        for (nm in curlname)
          if (evtext[i] ~ ("\\$\\{?" nm "([^A-Za-z0-9_]|$)")) {
            printf "%s:%d: an eval of a variable that names curl: the gate cannot check that call\n", evfile[i], evline[i]
            break
          }
      if (mode == "tls")
        for (i = 1; i <= na; i++) {
          if (!aseen[i])
            printf "%s: the allowlist entry names a file that this run does not read (a stale entry)\n", apath[i]
          else if (acount[i] != 1)
            printf "%s: the allowlist entry <%s> matches %d curl calls, want 1\n", apath[i], atext[i], acount[i]
        }
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
