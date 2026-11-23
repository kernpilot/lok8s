#!/usr/bin/env bats
# kuberc_test.bats: every kubectl call of the bash tree that writes runs
# with the kubectl user defaults off: `KUBERC=off kubectl …`. A kuberc can
# set a flag default for any kubectl command: --dry-run=client makes a
# delete, an annotate or a rollout restart report success and change
# nothing, and --show-secrets on a diff (kubectl v1.36 and later) prints the
# data of a Secret. kubectl v1.33 and later read KUBERC, and the value off
# loads no kuberc file; an older kubectl ignores it. A read keeps the kuberc
# of the user. The Go twin is execx.KubeRCOff (the Env of each such call),
# with the scan TestEveryKubectlWriteRunsWithKubeRCOff in
# internal/execx/kubectl_scan_test.go and a test in each package that runs
# one. This file holds the text scan, a second net: the behavior tests run a
# kubectl on PATH (kubectl_path_stub in test_helper.bash) through the real
# functions, in kapply_test.bats, bootstrap_test.bats and
# coredns_spec_test.bats.

setup() {
  load "../test_helper"
}

# _kubectl_sites [<dir>]: each kubectl call of the bash tree (default
# .lok8s), one per line, with four fields divided by a tab:
#   - "off" or "on": the kubectl child gets KUBERC=off or not;
#   - "write", "read" or "?" (the scan cannot read the subcommand);
#   - the subcommand ("rollout restart" for a rollout, "config view" for a
#     config, "create (render)" for a create with --dry-run=client, which
#     counts as a write: lo applies the output of each render, and a kuberc
#     default such as --append-hash would rename the object);
#   - <file>:<line>:<the text of the command>.
# The scan reads the shell text as the shell does: it cuts the text into
# commands at | ; & and a new line, and at each ( and $( outside single
# quotes. Inside double quotes only $( starts a command, so the text of a
# message is not a command. A backslash at the end of a line joins the next
# line. A comment and the body of a here-document are not code. A command is
# a call when, after the shell words in front of it (if, then, do, ! and the
# like), it starts with kubectl, a path to kubectl, ${KUBECTL} or ${kctl}.
# In front of it can be variable assignments and the words command,
# timeout, nohup, env, sudo, xargs and exec, with their options. The state
# is the value of the last KUBERC assignment in front of kubectl: env -i,
# env -u KUBERC and exec -c clear it, and sudo drops each assignment before
# it (sudo resets the environment). command -v and -V look a name up and
# are not a call. The scan fails closed: a prefix word with a path
# (/usr/bin/env), an option of a prefix word that the scan does not know,
# or a prefix word with another word between it and kubectl make the call
# a "?" site in the state "on". The subcommand is the first word that is
# not a flag, the value of a global flag (-n, --kubeconfig and the like) or an array; the
# quotes of a quoted word with no $ in it do not count, and a word that is
# a variable is "?". A call with "$@" there passes the arguments of its
# function on: it is a wrapper, not a call. A call of kapply::run_private
# with a quoted label counts as "off": the wrapper runs `KUBERC=off kubectl`
# with the arguments after the label (kapply_test.bats runs it). The addon
# and Tilt data and the manifests of the in-cluster agent (manifests/) are
# not code of lo.
#
# It does not see a kubectl that runs through another variable or through a
# wrapper function (the callers of a wrapper), a command that a remote shell
# runs (ssh "kubectl …"; a case below checks the one of the kubeone driver),
# or a subcommand inside a quoted word with spaces.
_kubectl_sites() {
  local dir="${1:-${_PROJECT_ROOT}/.lok8s}" f
  while IFS= read -r f; do
    # shellcheck disable=SC2016 # an awk program: its $ names are awk fields
    awk -v f="${f#"${dir}/"}" -v sq="'" '
      function add(x) {
        if (!start && x !~ /^[[:space:]]*$/) start = FNR
        cur = cur x
      }
      function push() {
        depth++
        scur[depth] = cur; sstart[depth] = start; sstate[depth] = q
        cur = ""; start = 0; q = 0
      }
      function pop() {
        emit()
        cur = scur[depth] "X"; start = sstart[depth]; q = sstate[depth]
        if (!start) start = FNR
        depth--
      }
      # unq(v): v without the quotes around it.
      function unq(v) {
        if (v ~ /^".*"$/ || v ~ ("^" sq ".*" sq "$")) v = substr(v, 2, length(v) - 2)
        return v
      }
      # word(): cut the first word off c, and return it.
      function word(   x) {
        match(c, /^[^[:space:]]+[[:space:]]*/)
        x = substr(c, 1, RLENGTH); c = substr(c, RLENGTH + 1)
        sub(/[[:space:]]+$/, "", x)
        return x
      }
      function emit(   text, kr, pfx, unk, more, a, w, n, i, x, verb, vi, kind, sub2) {
        c = cur; line = start
        cur = ""; start = 0
        text = c
        sub(/^[[:space:]]+/, "", text)
        # A function on one line: name() { … } (pop made "()" an X).
        sub(/^[[:space:]]*[A-Za-z_][A-Za-z0-9_:.-]*X[[:space:]]*[{][[:space:]]+/, "", c)
        while (sub(/^[[:space:]]*(!|[{]|if|then|do|elif|else|while|until|time)([[:space:]]|$)/, "", c)) {}
        sub(/^[[:space:]]+/, "", c)
        # kr is the KUBERC value that the kubectl child gets ("": the one of
        # the user). pfx: a prefix word came before the command. unk: the
        # scan cannot read the prefix (an option it does not know, or a
        # prefix word with a path), so it cannot prove the state.
        kr = ""; pfx = 0; unk = 0
        for (more = 1; more; ) {
          more = 1
          if (c ~ /^[^[:space:]"$]*\/(env|sudo|xargs|exec|nohup|timeout|command)([[:space:]]|$)/) {
            unk = 1
          } else if (c ~ /^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]/) {
            a = word()
            if (a ~ /^KUBERC=/) kr = unq(substr(a, 8))
          } else if (c ~ /^command[[:space:]]/) {
            word(); pfx = 1
            while (c ~ /^-[^[:space:]]*[[:space:]]/) {
              a = word()
              # command -v and -V look a name up: they run nothing.
              if (a ~ /^-p*[vV]/) return
              else if (a != "-p") { unk = 1; break }
            }
          } else if (c ~ /^nohup[[:space:]]/) {
            word(); pfx = 1
          } else if (c ~ /^timeout[[:space:]]/) {
            pfx = 1
            if (match(c, /^timeout([[:space:]]+(-[ks][[:space:]]+[^[:space:]]+|--(kill-after|signal)=[^[:space:]]+|--foreground|--preserve-status|-v|--verbose))*[[:space:]]+[^-[:space:]][^[:space:]]*[[:space:]]+/)) c = substr(c, RLENGTH + 1)
            else unk = 1
          } else if (c ~ /^env[[:space:]]/) {
            word(); pfx = 1
            while (c ~ /^-[^[:space:]]*[[:space:]]/) {
              a = word()
              if (a == "-" || a == "-i" || a == "--ignore-environment") kr = ""
              else if (a == "-u" || a == "--unset") { if (word() == "KUBERC") kr = "" }
              else if (a ~ /^-u./ || a ~ /^--unset=/) { if (a == "-uKUBERC" || a == "--unset=KUBERC") kr = "" }
              else if (a == "-C" || a == "--chdir") word()
              else if (a !~ /^(-C.+|--chdir=.*|-0|--null|-v|--debug)$/) { unk = 1; break }
            }
          } else if (c ~ /^sudo[[:space:]]/) {
            # sudo resets the environment: only an assignment after it counts.
            word(); kr = ""; pfx = 1
            while (c ~ /^-[^[:space:]]*[[:space:]]/) {
              a = word()
              if (a ~ /^-[ugCDhprtUT]$/) word()
              else if (a !~ /^(-[ugCDhprtUT].+|-[nkEHSbisAKlvVeP]+)$/) { unk = 1; break }
            }
          } else if (c ~ /^xargs[[:space:]]/) {
            word(); pfx = 1
            while (c ~ /^-[^[:space:]]*[[:space:]]/) {
              a = word()
              if (a ~ /^-[nILPdaEs]$/) word()
              else if (a !~ /^(-[nILPdaEs].+|-[0rtpxo]+|--(null|no-run-if-empty|verbose|interactive|exit|open-tty))$/) { unk = 1; break }
            }
          } else if (c ~ /^exec[[:space:]]/) {
            # exec -c starts the command with an empty environment.
            word(); pfx = 1
            while (c ~ /^-[^[:space:]]*[[:space:]]/) {
              a = word()
              if (a !~ /^-[cl]*a?$/ || a == "-") { unk = 1; break }
              if (a ~ /c/) kr = ""
              if (a ~ /a$/) word()
            }
          } else {
            more = 0
          }
          if (unk) more = 0
        }
        if (match(c, /^kapply::run_private[[:space:]]+"[^"]*"[[:space:]]+/)) {
          c = "kubectl " substr(c, RLENGTH + 1)
          kr = "off"
        }
        # A path to kubectl (/usr/local/bin/kubectl) is kubectl.
        if (match(c, /^[^[:space:]"$]*\/kubectl/) && substr(c, RLENGTH + 1, 1) ~ /^([[:space:]]|)$/) c = "kubectl" substr(c, RLENGTH + 1)
        if (c !~ /^("[$][{](KUBECTL|kctl)[}]"|[$][{](KUBECTL|kctl)[}]|"?[$](KUBECTL|kctl)"?|kubectl)([[:space:]]|$)/) {
          # A prefix that the scan could not read to its end, with a kubectl
          # after it, is a call that the scan cannot read: "?".
          if ((pfx || unk) && c ~ /(^|[[:space:]])("[$][{](KUBECTL|kctl)[}]"|[$][{](KUBECTL|kctl)[}]|"?[$](KUBECTL|kctl)"?|[^[:space:]]*\/kubectl|kubectl)([[:space:]]|$)/)
            print "on\t?\t?\t" f ":" line ":" text
          return
        }
        if (unk) {
          print "on\t?\t?\t" f ":" line ":" text
          return
        }
        sub(/[[:space:]]+$/, "", c)
        n = split(c, w, /[[:space:]]+/)
        # A quoted word with no $ in it is the word without its quotes.
        for (i = 2; i <= n; i++) {
          if (w[i] ~ /^"[^"$`]*"$/ || w[i] ~ ("^" sq "[^" sq "]*" sq "$")) w[i] = substr(w[i], 2, length(w[i]) - 2)
        }
        verb = "?"; vi = 0
        for (i = 2; i <= n; i++) {
          x = w[i]
          if (x ~ /^"?[$][{]?[@*][}]?"?$/) return
          if (x ~ /^"?[$][{][A-Za-z_][A-Za-z0-9_]*\[[@*]\][}]"?$/) continue
          if (x ~ /^-/) {
            if (x ~ /^(--kubeconfig|--context|-n|--namespace|--server|-s|--request-timeout|--tls-server-name|--cluster|--user|--token|--as)$/) i++
            continue
          }
          verb = x; vi = i
          break
        }
        kind = "read"
        if (!vi || verb ~ /^"?[$]/) {
          verb = "?"; kind = "?"
        } else if (verb ~ /^(rollout|certificate|auth|config)$/) {
          sub2 = w[vi + 1]
          if (verb == "rollout" && sub2 ~ /^(restart|undo|pause|resume)$/) kind = "write"
          if (verb == "certificate" && sub2 ~ /^(approve|deny)$/) kind = "write"
          if (verb == "auth" && sub2 ~ /^(reconcile)$/) kind = "write"
          if (verb == "config" && sub2 ~ /^(set|set-cluster|set-context|set-credentials|unset|use-context|use|delete-cluster|delete-context|delete-user|rename-context)$/) kind = "write"
          verb = verb " " sub2
        } else if (verb == "create" && c ~ /[[:space:]]--dry-run=client([[:space:]]|$)/) {
          verb = "create (render)"; kind = "write"
        } else if (verb ~ /^(apply|diff|replace|patch|create|delete|annotate|label|scale|set|taint|cordon|uncordon|drain|edit|expose|run|autoscale|exec|cp|attach|debug)$/) {
          kind = "write"
        }
        print (kr == "off" ? "off" : "on") "\t" kind "\t" verb "\t" f ":" line ":" text
      }
      inhd {
        l = $0
        if (hdtab) sub(/^\t+/, "", l)
        if (l == hd) { inhd = 0; hd = "" }
        next
      }
      {
        s = $0; n = length(s); prev = ""; cont = 0
        for (i = 1; i <= n; i++) {
          ch = substr(s, i, 1)
          if (q == 1) {
            if (ch == sq) q = 0
            add(ch); prev = ch
            continue
          }
          if (ch == "\\") {
            if (i == n) { cont = 1; break }
            add(substr(s, i, 2)); i++; prev = "x"
            continue
          }
          if (q == 2) {
            if (ch == "\"") q = 0
            else if (ch == "$" && substr(s, i + 1, 1) == "(") { push(); i++; prev = "("; continue }
            add(ch); prev = ch
            continue
          }
          if (ch == "#" && (prev == "" || prev ~ /[[:space:];&|()]/)) break
          if (ch == sq) q = 1
          else if (ch == "\"") q = 2
          else if (ch == "$" && substr(s, i + 1, 1) == "(") { push(); i++; prev = "("; continue }
          else if (ch == "(") { push(); prev = ch; continue }
          else if (ch == ")") {
            # A ) with no ( before it ends a case pattern: not a command.
            if (depth > 0) pop(); else { cur = ""; start = 0 }
            prev = ch
            continue
          }
          else if (ch == "|" || ch == ";" || ch == "&") { emit(); prev = ch; continue }
          else if (substr(s, i, 3) == "<<<") { add("<<<"); i += 2; prev = "<"; continue }
          else if (substr(s, i, 2) == "<<") {
            d = substr(s, i + 2); hdtab = 0
            if (substr(d, 1, 1) == "-") { hdtab = 1; d = substr(d, 2) }
            sub(/^[[:space:]]+/, "", d)
            c1 = substr(d, 1, 1)
            if (c1 == sq || c1 == "\"" || c1 == "\\") d = substr(d, 2)
            if (match(d, /^[A-Za-z_][A-Za-z0-9_]*/)) hd = substr(d, 1, RLENGTH)
            add("<<"); i++; prev = "<"
            continue
          }
          add(ch); prev = ch
        }
        if (cont) { add(" "); next }
        if (q == 0) emit(); else add(" ")
        if (hd != "") inhd = 1
      }
      END {
        emit()
        while (depth > 0) { pop(); emit() }
      }' "${f}"
  done < <(grep -rlE 'kubectl|KUBECTL' --exclude-dir=addons --exclude-dir=tilt --exclude-dir=manifests "${dir}")
}

@test "kuberc: every kubectl write of the bash tree runs with KUBERC=off" {
  local sites missing
  sites="$(_kubectl_sites)"
  # Anti-vacuity: the sweep finds the writes (kapply, the bootstrap diff and
  # restore.d, the drivers, the inventory, the agent deploy, the claim patch,
  # the hooks, the heal and the handover), the replaces of the kapply heals,
  # the coredns patch (split over four lines), the renders that feed an
  # apply, and it reads a read as a read.
  [ "$(printf '%s\n' "${sites}" | grep -c $'\twrite\t')" -ge 40 ]
  [ "$(printf '%s\n' "${sites}" | grep -c $'\tread\t')" -ge 30 ]
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tapply\tutils/kapply.sh:.* apply --server-side'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\treplace\tutils/kapply.sh:.* replace --force -f -'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\treplace\tutils/kapply.sh:.* replace --raw '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tdiff\tlibs/bootstrap:.* diff --server-side'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tpatch\tdrivers/lo/utils/services.sh:.*patch deployment coredns .*--patch-file '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tannotate\tdrivers/lo/utils/services.sh:.*annotate svc coredns-external '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tdelete\tdrivers/lo/utils/network.sh:.*delete pod .*-l k8s-app=cilium '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tdelete\tdrivers/capi/main:.*delete cluster '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tapply\tlibs/kubehz/deploy:.*kapply::run_private "the live agent" apply -k '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tpatch\tlibs/kubehz/deploy:.*patch cronjob kubehz-heartbeat --type=merge -p '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tcreate\tlibs/kubehz/deploy:.*create job "${job}" --from=cronjob/kubehz-heartbeat'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tdelete\tlibs/kubehz/deploy:.*delete job "${job}" '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\ttaint\tlibs/kubehz/handover:.*taint nodes --all '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tdelete\tlibs/hooks:.*delete --ignore-not-found -f -'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\trollout restart\tlibs/hooks:'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tapply\tdrivers/capi/generate:.*kapply::run_private "the credentials Secret" apply '
  printf '%s\n' "${sites}" | grep $'^off\twrite\tpatch\tlibs/kubehz/main:' \
    | grep -qF 'patch configmap "${cm}" --type merge --patch-file /dev/stdin'
  printf '%s\n' "${sites}" | grep -q $'^on\tread\tget\tlibs/kubehz/deploy:.*get secret kubehz-agent '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tcreate (render)\tdrivers/capi/generate:'
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tcreate (render)\tdrivers/capi/main:.* create namespace '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tcreate (render)\tdrivers/lo/utils/services.sh:.* create configmap coredns-custom '
  printf '%s\n' "${sites}" | grep -q $'^off\twrite\tcreate (render)\tlibs/kubehz/deploy:.* create secret generic kubehz-agent-bind '
  # The dry run of the agent deploy (kubehz::deploy_print) renders as the
  # apply does: both of its kustomize renders run with KUBERC=off.
  [ "$(printf '%s\n' "${sites}" | grep -c $'^off\tread\tkustomize\tlibs/kubehz/deploy:')" -eq 2 ] || {
    printf '%s\n' "${sites}" | grep $'\tkustomize\tlibs/kubehz/deploy:' >&2; return 1
  }
  missing="$(printf '%s\n' "${sites}" | grep -E $'^on\t(write|[?])\t' || true)"
  if [[ -n "${missing}" ]]; then
    printf 'a kubectl write (or a call whose subcommand the scan cannot read) without KUBERC=off:\n%s\n' "${missing}" >&2
    return 1
  fi
}

# The scan on a tree of its own: each form of a call that it must find, and
# the text that is not a call.
@test "kuberc: the scan reads each write, read and render, and skips a message, a wrapper and a here-document" {
  local d="${BATS_TEST_TMPDIR}/tree" sites want
  mkdir -p "${d}/utils" "${d}/addons/x" "${d}/manifests"
  cat > "${d}/utils/a.sh" <<'SH'
kubectl apply -f a.yaml
printf '%s' "${m}" | KUBERC=off kubectl --kubeconfig "${k}" apply --server-side -f -
kubectl patch deployment x -n y \
  --type json \
  --patch-file p.json
if ! command kubectl replace --force -f - >/dev/null; then :; fi
out="$("${KUBECTL}" create -f b.yaml)"
KUBERC=off kubectl diff -k overlay
kapply::run_private "the objects" apply --kubeconfig "${k}" -f - <<< "${m}"
echo "kubectl apply -f c.yaml failed (rc=1): kubectl delete -f c.yaml"
# kubectl apply -f d.yaml
kubectl get pods -o yaml
kubectl -n "${ns}" delete pod p --ignore-not-found
KUBERC=off kubectl annotate svc x a=b --overwrite
kubectl --tls-server-name "${s}" -n x rollout restart deploy/x 2>/dev/null
kubectl rollout status deploy/x
kubectl create secret generic s --dry-run=client -o yaml | KUBERC=off kubectl apply -f -
kubectl -n x patch cronjob c --type=merge -p "${P}" || { error "could not patch (rc=1). Run: kubectl patch cronjob c"; }
kubectl "${flags[@]}" label ns x a=b
kubectl "${verb}" x
w() { kubectl "$@" --request-timeout=5s; }
timeout -k 2 "$(bound)" kubectl scale deploy/x --replicas=0
env KUBERC=on kubectl delete pod p
env KUBERC=off kubectl delete pod p
KUBERC=off env -i kubectl delete pod p
KUBERC=off env -u KUBERC kubectl delete pod p
KUBERC=off sudo kubectl delete pod p
sudo -u root env KUBERC=off kubectl delete pod p
KUBERC=off xargs -n 1 kubectl delete pod
xargs kubectl annotate pod a=b
exec kubectl delete pod p
KUBERC=off exec -c kubectl delete pod p
nohup kubectl delete pod p &
"${kctl}" delete node n
${kctl} get node n
kubectl "delete" pod p
kubectl 'get' pods
KUBERC=off KUBERC=/x kubectl delete pod p
KUBERC="off" kubectl delete pod p
kubectl exec p -- ls
kubectl cp a p:/a
kubectl attach p
kubectl debug node/n -it --image=x
kubectl auth reconcile -f r.yaml
kubectl auth can-i get pods
kubectl config set-context --current --namespace=x
kubectl config view --raw
KUBERC=off kubectl create configmap c --from-file=a --dry-run=client -o yaml
sudo --user root kubectl delete pod p
env -iu KUBERC kubectl delete pod p
xargs --max-args 1 kubectl delete pod
/usr/bin/env KUBERC=off kubectl delete pod p
KUBERC=off sudo --login kubectl delete pod p
KUBERC=off env --weird kubectl delete pod p
KUBERC=off xargs --weird kubectl delete pod
KUBERC=off exec -x kubectl delete pod p
KUBERC=off command -x kubectl delete pod p
timeout --weird 5 kubectl delete pod p
nohup retry kubectl delete pod p
timeout --foreground 5 kubectl delete pod p
KUBERC=off /usr/local/bin/kubectl delete pod p
command -v kubectl >/dev/null
KUBERC=off sudo -n env KUBERC=off kubectl delete pod p
xargs -r -n1 kubectl delete pod
KUBERC=off exec -l kubectl delete pod p
KUBERC=off env -v kubectl delete pod p
KUBERC=off env -uKUBERC kubectl delete pod p
ssh h 'kubectl delete node n'
cat <<EOF
kubectl delete -f e.yaml
EOF
x=1
SH
  echo 'kubectl apply -f addon.yaml' > "${d}/addons/x/a.sh"
  echo 'kubectl delete -f in-cluster.yaml' > "${d}/manifests/m.sh"
  # The state, the kind, the subcommand and <file>:<line> of each call.
  # shellcheck disable=SC2016 # an awk program: its $ names are awk fields
  sites="$(_kubectl_sites "${d}" | awk -F '\t' -v OFS='\t' '{ split($4, p, ":"); print $1, $2, $3, p[1] ":" p[2] }')"
  want="$(cat <<'W'
on	write	apply	utils/a.sh:1
off	write	apply	utils/a.sh:2
on	write	patch	utils/a.sh:3
on	write	replace	utils/a.sh:6
on	write	create	utils/a.sh:7
off	write	diff	utils/a.sh:8
off	write	apply	utils/a.sh:9
on	read	get	utils/a.sh:12
on	write	delete	utils/a.sh:13
off	write	annotate	utils/a.sh:14
on	write	rollout restart	utils/a.sh:15
on	read	rollout status	utils/a.sh:16
on	write	create (render)	utils/a.sh:17
off	write	apply	utils/a.sh:17
on	write	patch	utils/a.sh:18
on	write	label	utils/a.sh:19
on	?	?	utils/a.sh:20
on	write	scale	utils/a.sh:22
on	write	delete	utils/a.sh:23
off	write	delete	utils/a.sh:24
on	write	delete	utils/a.sh:25
on	write	delete	utils/a.sh:26
on	write	delete	utils/a.sh:27
off	write	delete	utils/a.sh:28
off	write	delete	utils/a.sh:29
on	write	annotate	utils/a.sh:30
on	write	delete	utils/a.sh:31
on	write	delete	utils/a.sh:32
on	write	delete	utils/a.sh:33
on	write	delete	utils/a.sh:34
on	read	get	utils/a.sh:35
on	write	delete	utils/a.sh:36
on	read	get	utils/a.sh:37
on	write	delete	utils/a.sh:38
off	write	delete	utils/a.sh:39
on	write	exec	utils/a.sh:40
on	write	cp	utils/a.sh:41
on	write	attach	utils/a.sh:42
on	write	debug	utils/a.sh:43
on	write	auth reconcile	utils/a.sh:44
on	read	auth can-i	utils/a.sh:45
on	write	config set-context	utils/a.sh:46
on	read	config view	utils/a.sh:47
off	write	create (render)	utils/a.sh:48
on	?	?	utils/a.sh:49
on	?	?	utils/a.sh:50
on	?	?	utils/a.sh:51
on	?	?	utils/a.sh:52
on	?	?	utils/a.sh:53
on	?	?	utils/a.sh:54
on	?	?	utils/a.sh:55
on	?	?	utils/a.sh:56
on	?	?	utils/a.sh:57
on	?	?	utils/a.sh:58
on	?	?	utils/a.sh:59
on	write	delete	utils/a.sh:60
off	write	delete	utils/a.sh:61
off	write	delete	utils/a.sh:63
on	write	delete	utils/a.sh:64
off	write	delete	utils/a.sh:65
off	write	delete	utils/a.sh:66
on	write	delete	utils/a.sh:67
W
)"
  [ "${sites}" = "${want}" ] || { diff <(printf '%s\n' "${want}") <(printf '%s\n' "${sites}") >&2; return 1; }
}

# The kubeone driver deletes a stale node with the kubectl of the leader
# control plane, over ssh. The scan above does not see a command that a
# remote shell runs, so this case reads the command that ssh gets: the
# remote delete runs as `env KUBERC=off kubectl …` (env sets the variable in
# each login shell of root, also in a csh, which does not read a VAR=value
# prefix), and the read before it keeps the kuberc.
@test "kuberc: the kubeone stale node delete runs the remote kubectl with KUBERC=off" {
  command -v yq &>/dev/null || skip "yq not available"
  import() { :; }
  warn() { :; }
  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
  local work="${BATS_TEST_TMPDIR}/work" log="${BATS_TEST_TMPDIR}/ssh.log"
  mkdir -p "${work}"
  : > "${log}"
  cat > "${work}/kubeone.yaml" <<'YAML'
controlPlane:
  hosts:
    - publicAddress: 203.0.113.10
staticWorkers:
  hosts:
    - hostname: w1
      publicAddress: 203.0.113.21
YAML
  # The remote command is the last argument. The node is not Ready.
  ssh() {
    printf '%s\n' "${*: -1}" >> "${log}"
    case "${*: -1}" in
      *" get node "*) echo False ;;
    esac
  }
  kubeone::_clean_reinstalled_workers "${work}"
  grep -qx 'env KUBERC=off kubectl --kubeconfig /etc/kubernetes/admin.conf delete node w1 --timeout=20s' "${log}" || { cat "${log}"; return 1; }
  grep -q '^kubectl --kubeconfig /etc/kubernetes/admin.conf get node w1 ' "${log}" || { cat "${log}"; return 1; }
}

@test "kuberc: a KUBERC=off prefix reaches the kubectl child, and the shell keeps its own KUBERC" {
  export KUBERC=/user/kuberc
  local seen
  seen="$(KUBERC=off bash -c 'printf %s "${KUBERC}"')"
  [ "${seen}" = off ]
  [ "${KUBERC}" = /user/kuberc ]
}
