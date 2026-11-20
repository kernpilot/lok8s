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
#            number, no option) can stand. An assignment (NAME=value,
#            NAME="a b", NAME='a', NAME=$(…) or NAME= before curl), env,
#            command, exec, sudo or any other word there is a finding,
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
# In both q and tls modes, more forms are findings, because the gate
# cannot see the call that they make:
#   - curl named through a variable: an assignment whose value is curl or
#     ends in /curl (c=curl, c=(curl …), local c=/usr/bin/curl), a lookup
#     of curl (c=$(command -v curl), $(which curl), $(type -P curl)) or a
#     default expansion ${X:-curl};
#   - an eval in a command position whose command holds the text curl
#     (eval 'curl …'), or a variable that such an assignment or expansion
#     names, in any file of the run (c=curl, then eval "${c} …");
#   - the backstop: a curl word in code that the gate does not place as a
#     call and that no rule above reports: "a curl word the gate cannot
#     place". A curl word is the text curl after the start of the command,
#     white space, an operator, =, `, / or a backslash, and before the end,
#     white space, an operator, = or `. So these forms are findings, not
#     calls: a case arm (a) curl …), a redirection before the command
#     (2>&1 curl …), a lookup or an argument (command -v curl, apt-get
#     install curl, echo if curl). A curl word in quotes is a finding too
#     when the gate and the shell can read its quotes in two ways: on a
#     line that starts in a quote, a ${ }, a $( ) or a `…` of a line
#     before (a string over more lines), and after a single quote in a
#     ${ } in double quotes ("${a:-'}": the gate reads that quote as text,
#     as POSIX does, but bash in its default mode reads it as a quote). A
#     line that holds no call opts out only with the trailing comment
#     "# curl-gate: not a call" (exactly this text, after white space, as
#     a comment in code).
#     The opt-out does not apply to a call that the gate places.
#   - a curl word in the body of a here-document whose delimiter has no
#     quote and no backslash (<<EOF): the shell runs the $( ) and `…` of
#     such a body, and the gate does not read them ("a curl word the gate
#     cannot place"; a backslash at the end of a line of the body joins
#     the next line). A quoted delimiter (<<'EOF', <<"EOF", <<\EOF) makes
#     the body text: a curl word there is no finding, unless the file ends
#     before the delimiter line. In an unquoted body, a curl name in
#     quotes ("curl", 'curl', or a quoted path that ends in /curl) is a
#     finding too when a $( or a ` of the body comes before it: the shell
#     runs that command, and the quotes do not change its name. The gate
#     does not track where the $( ) or `…` ends, so a quoted name after a
#     $( ) that already ended is a finding as well.
# In the q mode, a definition of curl is a finding too: an alias
# (alias curl=…) or a function (curl(), function curl). The gate checks
# the source text, not the command that a call then runs.
#
# A curl call is the word curl, \curl, "curl" or a path that ends in /curl,
# then white space, a redirection (curl</dev/null, curl>out) or the end of
# the command, in a command position: at the start of a command, after |,
# (, ;, &, {, `, $(, <(, &&, ||, a YAML list item "- ", or the words if,
# then, do, else, elif, while, until, time and ! when that word is in a
# command position itself (if curl, x; then curl; not echo if curl).
# Before it can stand
# wrappers: command, exec, env, xargs, timeout, nice, nohup, sudo,
# stdbuf, setsid, builtin, bounded (the tick budget helper of the agent
# CronJob, which runs its arguments under timeout), their options and
# values (also quoted: sudo -u "${u}"), and assignment words (NAME= or
# NAME+=, then any value: quoted, $( ) or none). The words
# are shell words: white space in quotes, in a ${ } or in a $( ) does not
# end one, and an operator of the shell (;, &&, ||, |, &, ( or )) in code
# ends one, so the x=y of "echo x=y; curl" is no assignment word before
# curl. A ${ } nests (a ${ in it opens one more, quotes in it are quotes,
# only its own } ends it), and its text is no code. In double quotes, a
# single quote is text, also in a ${ } in double quotes. A $( ) or a `…`
# is a command of its own, also in double quotes and in a ${ }: the gate
# finds a call there as at the top level. A curl in a quoted string (an
# echo of a command for the user), in the text of a ${ } and in a comment
# (a # that starts a word, to the end of its line) does not count, and
# comment lines neither end nor continue a command. Before any other step,
# a line that ends in a backslash is joined to the next line without the
# backslash and the newline, as the shell does (so cu\<newline>rl is
# curl). The lines that a trailing |, &&, || or ( joins are one command,
# and so are the lines of a quote that is still open at the end of a line
# (there a line that starts with # is text, not a comment). After a line
# that ends in a $( ) or a `…` that is still open, the next line is a new
# command in that $( ) or `…` (the newline ends a command there), so the
# ) and the " of a "$( )" over more lines close what they open. The body
# of a here-document (<< or <<- and a delimiter word, in code, not <<<
# and not a shift in (( ))) is no command: it starts at the next line and
# ends at the line that is the delimiter, compared exactly, as bash does.
# For <<- the leading tabs of each line go first. In a YAML file (.yaml,
# .yml) the leading spaces of each line go too (the indentation of a
# block scalar, which the shell does not see). In a body whose delimiter
# has no quote and no backslash, a line that ends in an odd number of
# backslashes is joined to the next line before the test, as bash does:
# x\<newline>EOF is body text, E\<newline>OF ends the body. A quoted body
# joins no lines.
# The first argument of a call is the first word after the curl word that
# is not a redirection: the operand of a redirection is one shell word,
# with its quotes and escapes (curl >"a -q " -u … has -u first).
#
# A call carries a credential when its own arguments (from the curl word up
# to the next |, ;, & of && or ||, or the ")" that ends its subshell: not an
# option of another command of the pipeline, as grep -F, tr -d or head -n)
# have one of: -K or --config (a curl config), a header in a variable
# (-H "${…}"), -u, -uUSER:PW, --user, -b, --cookie, --oauth2-bearer, -U,
# --proxy-user, a body (-d, -dX, --data*, -F, --form, --json), a body on
# stdin (@-), a short option or a group of short options with n, E, K, u,
# b, d, F, T or U (-n, -E, -sK, -Ecert.pem), --netrc, --netrc-file,
# --netrc-optional, --cert, --key, --proxy-cert, --proxy-key, -T,
# --upload-file, --variable, --form-string, --data-*, a header from a file
# (-H @file, --header @file) or a header with an expansion in its value
# (-H "X-API-Key: ${TOKEN}"). It also carries one when the command up to
# the end of the call (so also a command that feeds the stdin of curl)
# has one of these texts: an Authorization header, http::curl_config or
# curl_config (the config text), a user and password in a URL
# (scheme://user:pw@host) or a query key token, sig, signature, key,
# secret, password or X-Amz-Signature.
#
# Out of the scope of the gate (the residuals): a name built from parts
# (c="cu""rl", printf), code that the tree generates and sources, an eval
# whose command holds neither the text curl nor a variable that the gate
# saw named as curl (eval "${cmd}", with cmd set from other text; the evals
# of the tree today hold neither), an alias or a function of curl that is
# defined outside the tree, and the text of a ${ } in a command position
# (${a:-curl -u x} https://h: only ${X:-curl} is the finding above). The
# lexer does not model all of bash: an ANSI-C string with an escaped
# quote ($'it\'s') and the ) of a case arm in a $( ) can give it a wrong
# quote state for the rest of that line (the lines after it then start in
# that state, and their curl words in quotes are findings). A quoted body
# of a here-document that the tree writes to a script, and a call in a
# body that another program runs (bash <<'EOF'), are code that the tree
# generates.
#
# The check reads the source text only: it needs no shellcheck and runs no
# code of the tree. curl_gate_files gives it every file of the tree, not
# only the files that hold the word curl (a split name holds no such
# word).

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
      na = 0; ne = 0; hq_n = 0; hq_i = 1
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
    function reset() { buf = ""; n = 0; evalfrom = 0; LXSET = 0; hfrom = 1; inq = 0; split("", placed); split("", handled) }
    function add(text, cont) {
      if (n == 0) cmdfile = FILENAME
      n++
      starts[n] = length(buf) + 1
      lines[n] = FNR
      ltext[n] = text
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
    # A text that carries a credential: in the call, or in a command before
    # it that feeds its stdin (http::curl_config | curl -K -). The caller
    # gives the command up to the end of the call, not the commands after
    # it.
    function credential_text(s) {
      return s ~ /[Aa]uthorization:/ ||
        s ~ /curl_config/ ||
        s ~ /:\/\/[^\/[:space:]"\047@]*@/ ||
        s ~ /[?&](token|access_token|sig|signature|key|secret|password|X-Amz-Signature)=/
    }
    # A credential option in the arguments of the call (the caller gives
    # the call only: an option of another command of the pipeline, grep -F
    # or tr -d, does not count).
    function credential(s) {
      return s ~ /(^|[[:space:]])(-K|--config)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])(-H|--header)[[:space:]=]+["\047]?\$/ ||
        s ~ /(^|[[:space:]])(-u|--user|-b|--cookie|--oauth2-bearer|-U|--proxy-user)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])-u[^[:space:]-]/ ||
        s ~ /(^|[[:space:]])(-d|--data|--data-raw|--data-binary|--data-urlencode|-F|--form|--json)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])-d[^[:space:]-]/ ||
        s ~ /@-/
    }
    # More credential options of the call (the caller gives the call only,
    # so head -n or grep -E of another command does not count).
    function credential_call(s) {
      return s ~ /(^|[[:space:]])-[A-Za-z]*[nEKubdFTU]/ ||
        s ~ /(^|[[:space:]])--(netrc|netrc-file|netrc-optional|cert|key|proxy-cert|proxy-key|upload-file|variable|form-string|data-[a-z]+)([[:space:]=]|$)/ ||
        s ~ /(^|[[:space:]])(-H|--header|--proxy-header)[[:space:]=]*["\047]?@/ ||
        s ~ /(^|[[:space:]])(-H|--header|--proxy-header)[[:space:]=]*("[^"]*|[^[:space:]"\047]*)\$/
    }
    # The lexer: one pass over s (buf, or a prefix of it: the positions are
    # those of buf). For each position i it records the state before the
    # character at i:
    #   LS[i]  "c" code, "s" single quotes, "d" double quotes, "b" the text
    #          of a ${ }, "#" a comment;
    #   LC[i]  the command context: 1 for the command, a new one for each
    #          $( ) and each `…`, also in double quotes and in a ${ };
    #   LD[i]  the depth of ( ) in that context;
    #   LW[i]  the start of the current word of the nearest code context;
    #   LE[i]  1 when a backslash escapes the character.
    # A ${ } nests: a ${ in it opens one more, quotes in it are quotes, and
    # its } ends it. A # that starts a word in code starts a comment, which
    # ends at the end of its line. CL[k] is the character that ends context
    # k: ")" for $( ), "`" for `…`, "" for the command.
    # In double quotes a single quote is text, also in a ${ } in double
    # quotes (dq[] marks such a ${ }). LQ is the position of the first single
    # quote in such a ${ }, else 0: bash in its default mode reads that quote
    # as a quote, so the backstop reports each curl word in quotes after it.
    # The state at the start is the code of the command, or IS: the state
    # at the end of the command before, when that command ended in a $( )
    # or a `…` that is still open (carry). ES is the state at the end of s.
    function lex(s,   i, c, nx, L, top, st, cx, dp, ws, dq, nctx, ls, k, pc, nc) {
      L = length(s)
      if (LXSET && s == LXS) return
      LXSET = 1; LXS = s; LQ = 0
      split("", LS); split("", LC); split("", LD); split("", LW); split("", LE); split("", CL)
      for (k = 2; k <= n; k++) ls[starts[k]] = 1
      top = 1; st[1] = "c"; cx[1] = 1; dp[1] = 0; ws[1] = 1; dq[1] = 0; nctx = 1; CL[1] = ""
      if (IS_top > 1) {
        top = IS_top; nctx = IS_nctx
        for (k = 1; k <= top; k++) { st[k] = IS_st[k]; cx[k] = IS_cx[k]; dp[k] = IS_dp[k]; dq[k] = IS_dq[k]; ws[k] = 1 }
        for (k = 1; k <= nctx; k++) CL[k] = IS_CL[k]
      }
      for (i = 1; i <= L + 1; i++) {
        if (st[top] == "#" && (i in ls)) top--
        lex_rec(i, st, cx, dp, ws, top)
        if (i > L) {
          # A comment ends at the end of the line.
          if (st[top] == "#") top--
          ES_top = top; ES_nctx = nctx
          for (k = 1; k <= top; k++) { ES_st[k] = st[k]; ES_cx[k] = cx[k]; ES_dp[k] = dp[k]; ES_dq[k] = dq[k] }
          break
        }
        c = substr(s, i, 1); nx = substr(s, i + 1, 1)
        if (st[top] == "#") continue
        if (st[top] == "s") { if (c == SQ) top--; continue }
        if (c == "\\") { if (i < L) { i++; lex_rec(i, st, cx, dp, ws, top); LE[i] = 1 }; continue }
        if (c == "$" && (nx == "(" || nx == "{")) {
          i++; lex_rec(i, st, cx, dp, ws, top)
          top++
          if (nx == "(") { st[top] = "c"; cx[top] = ++nctx; dp[top] = 0; ws[top] = i + 1; dq[top] = 0; CL[nctx] = ")" }
          else { st[top] = "b"; cx[top] = cx[top - 1]; dp[top] = dp[top - 1]; dq[top] = st[top - 1] == "d" || (st[top - 1] == "b" && dq[top - 1]) }
          continue
        }
        if (c == "`") {
          if (st[top] == "c" && CL[cx[top]] == "`") { top--; continue }
          top++; st[top] = "c"; cx[top] = ++nctx; dp[top] = 0; ws[top] = i + 1; dq[top] = 0; CL[nctx] = "`"
          continue
        }
        if (st[top] == "d") { if (c == "\"") top--; continue }
        if (c == SQ && st[top] == "b" && dq[top]) { if (!LQ) LQ = i; continue }
        if (c == SQ || c == "\"") { top++; st[top] = (c == SQ ? "s" : "d"); cx[top] = cx[top - 1]; dp[top] = dp[top - 1]; dq[top] = 0; continue }
        if (st[top] == "b") { if (c == "}") top--; continue }
        # Code.
        pc = i > 1 ? substr(s, i - 1, 1) : ""
        if (c == "#" && (i == 1 || pc ~ /[[:space:];&|()<>]/)) { top++; st[top] = "#"; cx[top] = cx[top - 1]; dp[top] = dp[top - 1]; dq[top] = 0; continue }
        if (c == "(") { dp[top]++; ws[top] = i + 1; continue }
        if (c == ")") {
          if (dp[top] > 0) { dp[top]--; ws[top] = i + 1 }
          else if (CL[cx[top]] == ")") top--
          continue
        }
        if (c == ";" || c == "|") { ws[top] = i + 1; continue }
        if (c == "&") {
          if (pc != ">" && pc != "<" && nx != ">") ws[top] = i + 1
          continue
        }
        if (c ~ /[[:space:]]/) ws[top] = i + 1
      }
    }
    function lex_rec(i, st, cx, dp, ws, top,   k) {
      LS[i] = st[top]; LC[i] = cx[top]; LD[i] = dp[top]
      k = top
      while (k > 1 && st[k] != "c") k--
      LW[i] = ws[k]
    }
    # The quote at position p of s: "s", "d", "b" (the text of a ${ }) or
    # "#" (a comment), and "" in code (also in a $( ) or `…` inside quotes).
    function quote_at(s, p) { lex(s); return LS[p] == "c" ? "" : LS[p] }
    function quoted(s, p) { return quote_at(s, p) != "" }
    # The end of the call that starts at b: the first |, ; or & (not of a
    # redirection: 2>&1, &>) in its context and at its ( ) depth, the ")"
    # or "`" that closes that context or its subshell, or a comment; else
    # the end of the command.
    function call_end(b,   i, c, L, cb, db, pc, nc) {
      lex(buf); L = length(buf); cb = LC[b]; db = LD[b]
      for (i = b; i <= L; i++) {
        if (LS[i] != "c" || LC[i] != cb || LE[i]) continue
        c = substr(buf, i, 1)
        if (c == "#" && LS[i + 1] == "#") return i
        if (c == ")" && LD[i] == db) return i
        if (c == "`" && CL[cb] == "`") return i
        if (LD[i] != db) continue
        if (c == "|" || c == ";") return i
        if (c == "&") {
          pc = substr(buf, i - 1, 1); nc = substr(buf, i + 1, 1)
          if (pc != ">" && pc != "<" && nc != ">") return i
        }
      }
      return L + 1
    }
    # The end of the shell word that starts at position a of buf in the
    # context cb: the first white space or operator in the code of that
    # context. Quotes, escapes, a ${ }, a $( ) and a `…` stay in the word.
    function word_end(a, cb,   i, L, c) {
      lex(buf); L = length(buf)
      for (i = a; i <= L; i++) {
        if (LS[i] != "c" || LC[i] != cb || LE[i]) continue
        c = substr(buf, i, 1)
        if (c == "(" && i > 1 && substr(buf, i - 1, 1) == "$" && !LE[i - 1]) continue
        if (c == "`" && CL[cb] != "`") continue
        if (c ~ /[[:space:];&|()<>`]/) return i
      }
      return L + 1
    }
    # The start of the last shell word of s (from the lexer). Words end at
    # white space in code, not in quotes, in a ${ } or in a $( ) that closes
    # before the end (NAME="a b", NAME=${a:-x;y} and NAME=$(f x) are one
    # word each). An operator of the shell in code ends a word too: ;, |, &
    # (not of a redirection: 2>&1, &>), and a ( or ) that is not the $( ) of
    # a word. So "echo x=y;" ends in an empty word after the ;, not in the
    # assignment word "x=y;". In a $( ) or `…` that is still open at the
    # end (also in a ${ }), the word starts after the $( or the `, or after
    # the last white space or operator in it (the prefix "x=$(echo" ends in
    # the word echo).
    function word_start(s) { lex(s); return LW[length(s) + 1] }
    function last_word(s) { return s ~ /[[:space:]]$/ ? "" : substr(s, word_start(s)) }
    function drop_last(s) { sub(/[[:space:]]+$/, "", s); s = substr(s, 1, word_start(s) - 1); sub(/[[:space:]]+$/, "", s); return s }
    # An operator of the shell in the code of a word (not in its quotes or
    # in a ${ } expansion): the wrapper search stops there.
    function has_op(w) { gsub(/"[^"]*"|\047[^\047]*\047|\$\{[^}]*\}/, "", w); return w ~ /[|;&(){}<>`]/ }
    function is_wrapper(w) { return w ~ /^(command|exec|env|xargs|timeout|nice|nohup|sudo|stdbuf|setsid|builtin|bounded)$/ }
    # An assignment word: NAME= or NAME+= and then anything (a quoted or
    # unquoted value, a $( ), or nothing). The word ends before an operator
    # of the shell (word_start), so the x=y of "echo x=y; curl" is no
    # assignment before curl.
    function is_assign(w) { return w ~ /^[A-Za-z_][A-Za-z0-9_]*\+?=/ }
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
          if (w == "" || has_op(w)) break
          if (is_wrapper(w)) { found = 1; break }
          if (w ~ /^-[A-Za-z]*[vV]/) lookup = 1
          t = drop_last(t); k++
        }
        if (found && k > 0 && !(lookup && w == "command")) { p = t; continue }
        return p
      }
    }
    # A command position: the start of a command, after an operator, or
    # after a keyword (if, then, do, else, elif, while, until, time or !)
    # that is in a command position itself (so the if of "echo if" is no
    # keyword).
    function command_position(p,   q) {
      sub(/[[:space:]]+$/, "", p)
      if (p == "") return 1
      if (match(p, /(if|then|do|else|elif|while|until|time|!)$/)) {
        q = substr(p, 1, RSTART - 1)
        if (q == "" || q ~ /[[:space:];&|({`]$/) return command_position(q)
      }
      if (p ~ /[|(;&{`]$/) return 1
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
        mark_words(p, m)
        if (kind == "assign" && match(m, /[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=/)) {
          nm = substr(m, RSTART, RLENGTH); sub(/[[+=].*$/, "", nm); curlname[nm] = 1
        }
        if (kind == "default" && match(m, /\{[A-Za-z_][A-Za-z0-9_]*/))
          curlname[substr(m, RSTART + 1, RLENGTH - 1)] = 1
      }
    }
    # The curl words of the text m at position p of buf: a rule reported
    # them, so the backstop does not.
    function mark_words(p, m,   k, q) {
      q = 0
      while ((k = index(m, "curl")) > 0) { handled[p + q + k - 1] = 1; q += k + 3; m = substr(m, k + 4) }
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
        if (buf ~ /curl/) {
          report(line_at(b), "an eval of a command that holds curl: the gate cannot check that call")
          if (!evalfrom || b < evalfrom) evalfrom = b
        }
        else { ne++; evtext[ne] = buf; evfile[ne] = cmdfile; evline[ne] = line_at(b) }
      }
    }
    # A definition of curl: an alias (alias curl=…, alias 'curl=…') or a
    # function (curl(), curl (), function curl). The gate checks the source
    # text, so it cannot check what a curl call then runs: a finding in the
    # q mode. Its curl word is no call and no backstop word in any mode.
    function shadows(   s, off, k, j, p, b, nc, pre, w, def) {
      s = buf; off = 0
      while ((k = index(s, "curl")) > 0) {
        p = off + k; off += k + 3; s = substr(s, k + 4)
        if (p > 1 && substr(buf, p - 1, 1) ~ /[A-Za-z0-9_:.\/=@-]/) continue
        nc = substr(buf, p + 4)
        def = 0
        if (nc ~ /^[[:space:]]*\([[:space:]]*\)/)
          def = !quoted(buf, p) && command_position(strip_wrappers(substr(buf, 1, p - 1)))
        if (!def && nc ~ /^([[:space:]({]|$)/ && match(substr(buf, 1, p - 1), /(^|[[:space:];&|(){])function[[:space:]]+$/)) {
          j = substr(buf, RSTART, 1) == "f" ? RSTART : RSTART + 1
          def = !quoted(buf, p) && command_position(strip_wrappers(substr(buf, 1, j - 1)))
        }
        if (!def && substr(nc, 1, 1) == "=") {
          b = p
          if (b > 1 && substr(buf, b - 1, 1) ~ /["\047]/) b--
          if (!quoted(buf, b) && (b == 1 || substr(buf, b - 1, 1) ~ /[[:space:]]/)) {
            pre = substr(buf, 1, b - 1); w = ""
            for (j = 0; j < 16; j++) {
              sub(/[[:space:]]+$/, "", pre)
              if (command_position(pre)) break
              w = last_word(pre)
              if (w == "") break
              pre = drop_last(pre)
            }
            def = w == "alias" && command_position(pre)
          }
        }
        if (!def) continue
        handled[p] = 1
        if (mode == "q") report(line_at(p), "an alias or a function named curl: the gate cannot check what a curl call runs")
      }
    }
    # The opt-out: the line l of the command ends in white space and the
    # comment "# curl-gate: not a call" (exactly this text), and its #
    # starts a comment in code.
    function optout(l,   k, t, pos) {
      for (k = 1; k <= n; k++) {
        if (lines[k] != l) continue
        t = ltext[k]
        if (t !~ /[[:space:]]# curl-gate: not a call$/) return 0
        pos = starts[k] + length(t) - 23
        return quote_at(buf, pos) == "" && quote_at(buf, pos + 1) == "#"
      }
      return 0
    }
    # A curl word at position p of s: the text curl after the start of s,
    # white space, an operator, =, `, / or a backslash, and before the end,
    # white space, an operator, = or `.
    function curl_word(s, p,   nc) {
      if (p > 1 && substr(s, p - 1, 1) !~ /[[:space:];&|()<>=`\/\\]/) return 0
      nc = substr(s, p + 4, 1)
      return nc == "" || nc ~ /[[:space:];&|()<>=`]/
    }
    # The backstop: each curl word in code that no rule placed or reported
    # is a finding, unless its line opts out. In quotes it is a finding too
    # when its line starts in a quote (a string over more lines) or when a
    # single quote in a ${ } in double quotes comes before it (LQ): there
    # the gate and the shell can disagree on what is quoted.
    function backstop(   s, off, k, p, lq, qs) {
      lex(buf); lq = LQ
      split("", qs)
      if (IS_top > 1) qs[lines[1]] = 1
      for (k = 2; k <= n; k++) if (LS[starts[k]] ~ /^[sdb]$/ || LC[starts[k]] > 1) qs[lines[k]] = 1
      s = buf; off = 0
      while ((k = index(s, "curl")) > 0) {
        p = off + k; off += k + 3; s = substr(s, k + 4)
        if (!curl_word(buf, p)) continue
        if ((p in placed) || (p in handled) || (evalfrom && p > evalfrom)) continue
        if (quoted(buf, p) && !(line_at(p) in qs) && !(lq && p > lq)) continue
        if (optout(line_at(p))) continue
        report(line_at(p), "a curl word the gate cannot place")
      }
    }
    # The here-documents that the command opens: each << or <<- in code
    # (not <<<, not a shift in (( ))) and its delimiter word, from position
    # hfrom of buf on. A quote or a backslash in the delimiter makes a
    # quoted body (text to the shell); else the shell runs the $( ) and `…`
    # of the body. The bodies start at the next line, in this order.
    function heredocs(   i, L, j, w, we, t, q, tabs) {
      lex(buf); L = length(buf)
      for (i = hfrom; i < L; i++) {
        if (substr(buf, i, 2) != "<<" || LS[i] != "c" || LE[i]) continue
        if (substr(buf, i + 2, 1) == "<") { i += 2; continue }
        j = i + 2
        tabs = substr(buf, j, 1) == "-"
        if (tabs) j++
        while (substr(buf, j, 1) ~ /[ \t]/) j++
        if (j > L || substr(buf, j, 1) ~ /[;&|()<>=]/) continue
        t = substr(buf, 1, i - 1)
        if (gsub(/\(\(/, "&", t) > gsub(/\)\)/, "&", t)) continue
        we = word_end(j, LC[i])
        w = substr(buf, j, we - j)
        q = w ~ /["\047\\]/
        gsub(/["\047\\]/, "", w)
        hq_n++; hq_delim[hq_n] = w; hq_quoted[hq_n] = q; hq_tabs[hq_n] = tabs; hfile = FILENAME
        i = we - 1
      }
      hfrom = L + 1
    }
    # A line of a here-document body (FNR, the text $0). The body ends at
    # the line that is the delimiter, compared exactly, as bash does: for
    # <<- the leading tabs go first, and in a YAML file the leading spaces
    # (the indentation of the block) go too. A curl word in an unquoted
    # body is a finding: the gate does not read the $( ) and `…` there. In
    # an unquoted body, a line that ends in an odd number of backslashes
    # joins the next line BEFORE the delimiter test, as bash does: a joined
    # line can be the delimiter, and a line after a joined one is no
    # delimiter. A quoted body is text and joins no lines: its curl words
    # are findings only when the body has no end in the file (hd_close).
    function body_line(   t) {
      t = $0
      if (hfile ~ /\.ya?ml$/) sub(/^ +/, "", t)
      if (hq_tabs[hq_i]) sub(/^\t+/, "", t)
      if (hq_quoted[hq_i]) {
        if (t == hq_delim[hq_i]) body_end()
        else body_words($0, FNR, 1)
        return
      }
      if (hbuf == "") hbl = FNR
      if (match(t, /\\+$/) && RLENGTH % 2 == 1) { hbuf = hbuf substr(t, 1, length(t) - 1); return }
      t = hbuf t; hbuf = ""
      if (t == hq_delim[hq_i]) body_end()
      else body_words(t, hbl, 0)
    }
    # The end of a body: the next here-document of the command, if any.
    function body_end() {
      hbuf = ""; hb_n = 0; hsub = 0
      if (++hq_i > hq_n) { hq_n = 0; hq_i = 1 }
    }
    # The curl words of the text t of a body (its first line l; keep: a
    # quoted body, whose lines hd_close reports). In an unquoted body, a
    # curl name in quotes (quoted_name) is a finding too when a $( or a `
    # of the body comes before it (hsub: on a line before): the shell runs
    # that command, and its quotes do not change the name. The gate does
    # not track where the $( ) or `…` ends.
    function body_words(t, l, keep,   s, off, k, p, hit) {
      if (mode == "list") return
      s = t; off = 0
      while ((k = index(s, "curl")) > 0) {
        p = off + k; off += k + 3; s = substr(s, k + 4)
        hit = curl_word(t, p)
        if (!hit && !keep) hit = quoted_name(t, p) && (hsub || substr(t, 1, p - 1) ~ /\$\(|`/)
        if (!hit) continue
        if (keep) hb_line[++hb_n] = l
        else printf "%s:%d: %s\n", hfile, l, "a curl word the gate cannot place"
      }
      if (!keep && t ~ /\$\(|`/) hsub = 1
    }
    # A curl name in quotes at position p of s: the text curl with a double
    # or a single quote just before it or just after it, and on each side
    # a quote or a bound of a curl word (curl_word).
    function quoted_name(s, p,   b, a) {
      b = p > 1 ? substr(s, p - 1, 1) : ""
      a = substr(s, p + 4, 1)
      if (b !~ /["\047]/ && a !~ /["\047]/) return 0
      return (b == "" || b ~ /[[:space:];&|()<>=`\/\\"\047]/) && (a == "" || a ~ /[[:space:];&|()<>=`"\047]/)
    }
    # The end of a file in a here-document body: the curl words of a quoted
    # body that did not end are findings.
    function hd_close(   i) {
      if (hq_n > 0) {
        if (hbuf != "") body_words(hbuf, hbl, 0)
        for (i = 1; i <= hb_n; i++) printf "%s:%d: %s\n", hfile, hb_line[i], "a curl word the gate cannot place"
      }
      hq_n = 0; hq_i = 1; hb_n = 0; hbuf = ""; hsub = 0
    }
    function check(   s, off, c0, b, adv, prefix, stripped, rest, l, marked, i, hit, cred, manifest, seg, e, a, cb) {
      if (buf == "") { reset(); return }
      if (mode != "list") {
        indirect("(^|[[:space:];&|({])((local|declare|typeset|readonly|export)[[:space:]]+(-[A-Za-z]+[[:space:]]+)*)?[A-Za-z_][A-Za-z0-9_]*(\\[[^]]*\\])?\\+?=(\\([[:space:]]*)?[\"\047]?([^[:space:]\"\047()]*/)?curl[\"\047]?([[:space:]);]|$)", 0, "assign")
        indirect("\\$\\{[A-Za-z_][A-Za-z0-9_]*:?[-=][^}]*curl[\"\047]?\\}", 1, "default")
        indirect("(^|[[:space:];&|({])((local|declare|typeset|readonly|export)[[:space:]]+(-[A-Za-z]+[[:space:]]+)*)?[A-Za-z_][A-Za-z0-9_]*\\+?=[\"\047]?(\\$\\(|`)[[:space:]]*(command[[:space:]]+-[A-Za-z]*[vV]|which|type[[:space:]]+-[A-Za-z]*[pP])[[:space:]]+[\"\047]?curl[\"\047]?[[:space:]]*(\\)|`)", 0, "assign")
        evals()
      }
      shadows()
      manifest = cmdfile ~ /(^|\/)manifests\//
      s = buf; off = 0
      # A call starts at the word curl and white space, a redirection
      # (curl</dev/null, curl>out) or the end of the command.
      while (match(s, /curl["\047]?([[:space:]<>]|$)/)) {
        c0 = off + RSTART
        adv = RSTART + RLENGTH - 1
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
        if (c0 in handled) continue
        prefix = substr(buf, 1, b - 1)
        stripped = strip_wrappers(prefix)
        if (quoted(buf, b) || !command_position(stripped)) continue
        placed[c0] = 1
        l = line_at(b)
        e = call_end(b)
        seg = substr(buf, b, e - b)
        cred = credential(seg) || credential_call(seg) || credential_text(substr(buf, 1, e - 1))
        if (mode == "list")
          printf "%s:%d%s\n", cmdfile, l, cred ? " credential" : ""
        # The first argument: a redirection (curl </dev/null -q, curl 2>&1
        # -q) is no argument. Its operand is one shell word, with its
        # quotes and escapes (curl >"a -q " -u … gives -u first).
        lex(buf); cb = LC[c0]
        a = c0 + 4
        if (substr(buf, a, 1) ~ /["\047]/) a++
        while (match(substr(buf, a), /^[[:space:]]*([0-9]+|&|\{[A-Za-z_][A-Za-z0-9_]*\})?(<<<|<<-|<<|>>|>&|<&|<>|>\||<|>)[[:space:]]*/))
          a = word_end(a + RLENGTH, cb)
        rest = substr(buf, a)
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
      if (mode != "list") backstop()
      reset()
    }
    FNR == 1 {
      check(); reset(); hd_close(); IS_top = 0
      for (i = 1; i <= na; i++) if (path_is(FILENAME, apath[i])) aseen[i] = 1
    }
    hq_n > 0 { body_line(); next }
    # A comment line neither ends nor continues a command. In a string over
    # more lines it is text of the string.
    !inq && /^[[:space:]]*#/ { next }
    # The command goes on at the next line after a backslash, a trailing |,
    # &&, || or (, and in a quote that is still open (inq): there the
    # newline is text. In a $( ) or a `…` that is still open, the newline
    # ends the command, and the next command starts in that state (IS).
    {
      text = $0
      cont = 0
      if (match(text, /\\+$/) && RLENGTH % 2 == 1) { text = substr(text, 1, length(text) - 1); cont = 1 }
      add(text, cont)
      lex(buf)
      NS_top = ES_top; NS_nctx = ES_nctx
      for (k = 1; k <= ES_top; k++) { NS_st[k] = ES_st[k]; NS_cx[k] = ES_cx[k]; NS_dp[k] = ES_dp[k]; NS_dq[k] = ES_dq[k] }
      for (k = 1; k <= ES_nctx; k++) NS_CL[k] = CL[k]
      e = NS_st[NS_top]; inq = e == "s" || e == "d" || e == "b"
      if (!cont) heredocs()
      if (cont || inq || $0 ~ /(\\|\||&&|\(|\$\()[[:space:]]*$/) next
      check()
      IS_top = NS_top; IS_nctx = NS_nctx
      for (k = 1; k <= NS_top; k++) { IS_st[k] = NS_st[k]; IS_cx[k] = NS_cx[k]; IS_dp[k] = NS_dp[k]; IS_dq[k] = NS_dq[k] }
      for (k = 1; k <= NS_nctx; k++) IS_CL[k] = NS_CL[k]
    }
    END {
      if (bad) exit 2
      check(); hd_close()
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

# curl_gate_files [ROOT [MIN]] — every regular file under ROOT (the bash
# tree, .lok8s, by default), one path per line. No text search selects
# them: a file whose only call is cu\<newline>rl holds no word curl, so the
# scanner reads each file and decides. Left out: the scaffolding under
# init.d (TypeScript for a user project, not framework code) and the
# package list of the hetzner cloud-init (a list of package names, not a
# script). Fails loudly when it finds fewer than MIN files (150 by default;
# the tree has 229 today): a broken discovery must not read as "nothing to
# find".
curl_gate_files() {
  local root="${1:-${_PROJECT_ROOT}/.lok8s}" min="${2:-150}"
  local -a files=()
  mapfile -t files < <(find "${root}" -name init.d -prune -o -type f -print |
    grep -vxF -- "${root}/providers/hetzner/cloud-init/packages" | LC_ALL=C sort)
  (( ${#files[@]} >= min )) || {
    echo "curl_gate_files found only ${#files[@]} file(s) under ${root}: the discovery is broken, not the tree" >&2
    return 1
  }
  printf '%s\n' "${files[@]}"
}
