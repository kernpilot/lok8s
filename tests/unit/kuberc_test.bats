#!/usr/bin/env bats
# kuberc_test.bats — every kubectl call of the bash tree that writes runs
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

# _kubectl_sites [<dir>] — each kubectl call of the bash tree (default
# .lok8s), one per line, with four fields divided by a tab:
#   - "off" or "on": with a KUBERC=off prefix or not;
#   - "write", "read" or "?" (the scan cannot read the subcommand);
#   - the subcommand ("rollout restart" for a rollout, "create (render)" for
#     a create with --dry-run=client, which writes nothing);
#   - <file>:<line>:<the text of the command>.
# The scan reads the shell text as the shell does: it cuts the text into
# commands at | ; & and a new line, and at each ( and $( outside single
# quotes. Inside double quotes only $( starts a command, so the text of a
# message is not a command. A backslash at the end of a line joins the next
# line. A comment and the body of a here-document are not code. A command is
# a call when, after the shell words in front of it (if, then, do, ! and the
# like), its variable assignments, `command` and `timeout`, it starts with
# kubectl or ${KUBECTL}. The subcommand is the first word that is not a
# flag, the value of a global flag (-n, --kubeconfig and the like) or an
# array; a word that is a variable is "?". A call with "$@" there passes
# the arguments of its function on: it is a wrapper, not a call. A call of
# kapply::run_private with a quoted label counts as "off": the wrapper runs
# `KUBERC=off kubectl` with the arguments after the label (kapply_test.bats
# runs it). The addon and Tilt data and the manifests of the in-cluster
# agent (manifests/) are not code of lo.
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
      function emit(   c, text, env, w, n, i, x, verb, vi, kind) {
        c = cur; line = start
        cur = ""; start = 0
        text = c
        sub(/^[[:space:]]+/, "", text)
        # A function on one line: name() { … } (pop made "()" an X).
        sub(/^[[:space:]]*[A-Za-z_][A-Za-z0-9_:.-]*X[[:space:]]*[{][[:space:]]+/, "", c)
        while (sub(/^[[:space:]]*(!|[{]|if|then|do|elif|else|while|until|time)([[:space:]]|$)/, "", c)) {}
        sub(/^[[:space:]]+/, "", c)
        env = " "
        while (match(c, /^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+/)) {
          env = env substr(c, 1, RLENGTH)
          c = substr(c, RLENGTH + 1)
        }
        sub(/^command[[:space:]]+/, "", c)
        sub(/^timeout([[:space:]]+-[A-Za-z][[:space:]]+[^[:space:]]+)*[[:space:]]+[^[:space:]]+[[:space:]]+/, "", c)
        if (match(c, /^kapply::run_private[[:space:]]+"[^"]*"[[:space:]]+/)) {
          c = "kubectl " substr(c, RLENGTH + 1)
          env = env "KUBERC=off "
        }
        if (c !~ /^("[$][{]KUBECTL[}]"|[$][{]KUBECTL[}]|[$]KUBECTL|kubectl)([[:space:]]|$)/) return
        sub(/[[:space:]]+$/, "", c)
        n = split(c, w, /[[:space:]]+/)
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
        } else if (verb == "rollout" || verb == "certificate") {
          if (w[vi + 1] ~ /^(restart|undo|pause|resume|approve|deny)$/) kind = "write"
          verb = verb " " w[vi + 1]
        } else if (verb == "create" && c ~ /[[:space:]]--dry-run=client([[:space:]]|$)/) {
          verb = "create (render)"
        } else if (verb ~ /^(apply|diff|replace|patch|create|delete|annotate|label|scale|set|taint|cordon|uncordon|drain|edit|expose|run|autoscale)$/) {
          kind = "write"
        }
        print (env ~ /[[:space:]]KUBERC=off[[:space:]]/ ? "off" : "on") "\t" kind "\t" verb "\t" f ":" line ":" text
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
  # the coredns patch (split over four lines), and it reads a read as a
  # read and a render as a render.
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
  printf '%s\n' "${sites}" | grep -q $'^on\tread\tcreate (render)\tdrivers/capi/generate:'
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
on	read	create (render)	utils/a.sh:17
off	write	apply	utils/a.sh:17
on	write	patch	utils/a.sh:18
on	write	label	utils/a.sh:19
on	?	?	utils/a.sh:20
on	write	scale	utils/a.sh:22
W
)"
  [ "${sites}" = "${want}" ] || { diff <(printf '%s\n' "${want}") <(printf '%s\n' "${sites}") >&2; return 1; }
}

# The kubeone driver deletes a stale node with the kubectl of the leader
# control plane, over ssh. The scan above does not see a command that a
# remote shell runs, so this case reads the command that ssh gets: the
# remote delete runs with KUBERC=off, and the read before it does not.
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
  grep -qx 'KUBERC=off kubectl --kubeconfig /etc/kubernetes/admin.conf delete node w1 --timeout=20s' "${log}" || { cat "${log}"; return 1; }
  grep -q '^kubectl --kubeconfig /etc/kubernetes/admin.conf get node w1 ' "${log}" || { cat "${log}"; return 1; }
}

@test "kuberc: a KUBERC=off prefix reaches the kubectl child, and the shell keeps its own KUBERC" {
  export KUBERC=/user/kuberc
  local seen
  seen="$(KUBERC=off bash -c 'printf %s "${KUBERC}"')"
  [ "${seen}" = off ]
  [ "${KUBERC}" = /user/kuberc ]
}
