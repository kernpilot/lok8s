#!/usr/bin/env bats
# kuberc_test.bats — every kubectl call of the bash tree that reads a
# manifest or a patch from a file or stdin runs with the kubectl user
# defaults off: `KUBERC=off kubectl …`. A kuberc can set a flag default for
# any kubectl command (for example --show-secrets on a diff, kubectl v1.36
# and later). kubectl v1.33 and later read KUBERC, and the value off loads
# no kuberc file; an older kubectl ignores it. The Go twin is
# execx.KubeRCOff (the Env of each such call), with a test in each package
# that runs one. This file holds the text scan, a second net: the behavior
# tests run a kubectl on PATH (kubectl_path_stub in test_helper.bash) through
# the real functions, in kapply_test.bats, bootstrap_test.bats and
# coredns_spec_test.bats.

setup() {
  load "../test_helper"
}

# _kubectl_file_sites [<dir>] — each kubectl call of the bash tree (default
# .lok8s) that reads a manifest or a patch from a file or stdin, one per
# line: "off" or "on" (with a KUBERC=off prefix or not), a tab, then
# <file>:<line>:<text>. A line that ends in a backslash is joined to the next
# one first, so a call split over lines is one site. The text is cut into
# commands at | ; & and (. A command is a site when, after the shell words
# in front of it (if, then, do, ! and the like) and its variable
# assignments, it starts with kubectl, command kubectl or ${KUBECTL}, holds
# the verb apply, diff, replace, patch or create, and holds -f, --filename,
# --patch-file, -k or --kustomize. So the text of a message that names a
# kubectl command is not a site. Comment lines and the addon and Tilt data
# are not code of lo. A call of the wrapper kapply::run_private with a
# quoted label is a site too, and it counts as "off": the wrapper runs
# `KUBERC=off kubectl` with the arguments after the label (kapply_test.bats
# runs it).
#
# It does not see a kubectl that runs through another variable or another
# wrapper function, a verb or a file flag that comes from a variable or an
# array, or a call that a | ; & or ( inside a quoted string cuts.
_kubectl_file_sites() {
  local dir="${1:-${_PROJECT_ROOT}/.lok8s}" f
  while IFS= read -r f; do
    # shellcheck disable=SC2016 # an awk program: its $ names are awk fields
    awk -v f="${f#"${dir}/"}" '
      { if (cont) line = line " " $0; else { line = $0; start = FNR } }
      /\\$/ { sub(/\\$/, "", line); cont = 1; next }
      {
        cont = 0
        if (line ~ /^[[:space:]]*#/) next
        n = split(line, cmd, /[|;&(]/)
        for (i = 1; i <= n; i++) {
          c = cmd[i]
          while (sub(/^[[:space:]]*(!|[{]|if|then|do|elif|else|while|until|time)([[:space:]]|$)/, "", c)) {}
          sub(/^[[:space:]]+/, "", c)
          env = " "
          while (match(c, /^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+/)) {
            env = env substr(c, 1, RLENGTH)
            c = substr(c, RLENGTH + 1)
          }
          sub(/^command[[:space:]]+/, "", c)
          if (match(c, /^kapply::run_private[[:space:]]+"[^"]*"[[:space:]]+/)) {
            c = "kubectl " substr(c, RLENGTH + 1)
            env = env "KUBERC=off "
          }
          if (c !~ /^("[$][{]KUBECTL[}]"|[$][{]KUBECTL[}]|[$]KUBECTL|kubectl)[[:space:]]/) continue
          if (c !~ /[[:space:]](apply|diff|replace|patch|create)[[:space:]]/) continue
          if (c !~ /[[:space:]](-f|--filename|--patch-file|-k|--kustomize)([[:space:]=]|$)/) continue
          print (env ~ /[[:space:]]KUBERC=off[[:space:]]/ ? "off" : "on") "\t" f ":" start ":" line
        }
      }' "${f}"
  done < <(grep -rlE 'kubectl|KUBECTL' --exclude-dir=addons --exclude-dir=tilt "${dir}")
}

# A call of the bash tree that is not yet covered: the claim-nonce patch of
# the kubehz lib (the other apply sites are a follow-up). It must match
# exactly one site that runs without KUBERC=off, so the list shrinks when the
# site is fixed.
_KUBERC_NOT_YET='kubectl -n "${ns}" patch configmap "${cm}" --type merge --patch-file /dev/stdin'

@test "kuberc: every kubectl call of the bash tree that reads a file or stdin runs with KUBERC=off" {
  local sites missing
  sites="$(_kubectl_file_sites)"
  # Anti-vacuity: the sweep finds the call sites (kapply, the bootstrap diff
  # and restore.d, the drivers, the inventory, the agent deploy), the
  # replaces of the kapply heals and the coredns patch, which is split over
  # four lines.
  [ "$(printf '%s\n' "${sites}" | grep -c .)" -ge 25 ]
  printf '%s\n' "${sites}" | grep -q $'^off\tutils/kapply.sh:.* apply --server-side'
  printf '%s\n' "${sites}" | grep -q $'^off\tutils/kapply.sh:.* replace --force -f -'
  printf '%s\n' "${sites}" | grep -q $'^off\tutils/kapply.sh:.* replace --raw '
  printf '%s\n' "${sites}" | grep -q $'^off\tlibs/bootstrap:.* diff --server-side'
  printf '%s\n' "${sites}" | grep -q $'^off\tdrivers/lo/utils/services.sh:.*patch deployment coredns .*--patch-file '
  printf '%s\n' "${sites}" | grep -q $'^off\tlibs/kubehz/deploy:.*kapply::run_private "the live agent" apply -k '
  printf '%s\n' "${sites}" | grep -q $'^off\tdrivers/capi/generate:.*kapply::run_private "the credentials Secret" apply '
  [ "$(printf '%s\n' "${sites}" | grep $'^on\tlibs/kubehz/main:' | grep -cF "${_KUBERC_NOT_YET}")" -eq 1 ]
  missing="$(printf '%s\n' "${sites}" | grep $'^on\t' | grep -vF "${_KUBERC_NOT_YET}" || true)"
  if [[ -n "${missing}" ]]; then
    printf 'a kubectl call that reads a file or stdin without KUBERC=off:\n%s\n' "${missing}" >&2
    return 1
  fi
}

# The scan on a tree of its own: each form of a call that it must find, and
# the text that is not a call.
@test "kuberc: the scan finds a call split over lines, command kubectl and \${KUBECTL}, and skips a message" {
  local d="${BATS_TEST_TMPDIR}/tree" sites
  mkdir -p "${d}/utils" "${d}/addons/x"
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
echo "kubectl apply -f c.yaml failed (rc=1)"
# kubectl apply -f d.yaml
kubectl get pods -o yaml
SH
  echo 'kubectl apply -f addon.yaml' > "${d}/addons/x/a.sh"
  sites="$(_kubectl_file_sites "${d}")"
  [ "$(printf '%s\n' "${sites}" | grep -c .)" -eq 7 ] || { printf '%s\n' "${sites}"; return 1; }
  printf '%s\n' "${sites}" | grep -qx $'on\tutils/a.sh:1:kubectl apply -f a.yaml'
  printf '%s\n' "${sites}" | grep -q $'^off\tutils/a.sh:2:'
  printf '%s\n' "${sites}" | grep -qx $'on\tutils/a.sh:3:kubectl patch deployment x -n y    --type json    --patch-file p.json'
  printf '%s\n' "${sites}" | grep -q $'^on\tutils/a.sh:6:if ! command kubectl replace'
  printf '%s\n' "${sites}" | grep -q $'^on\tutils/a.sh:7:out='
  printf '%s\n' "${sites}" | grep -q $'^off\tutils/a.sh:8:'
  printf '%s\n' "${sites}" | grep -qx $'off\tutils/a.sh:9:kapply::run_private "the objects" apply --kubeconfig "${k}" -f - <<< "${m}"'
}

@test "kuberc: a KUBERC=off prefix reaches the kubectl child, and the shell keeps its own KUBERC" {
  export KUBERC=/user/kuberc
  local seen
  seen="$(KUBERC=off bash -c 'printf %s "${KUBERC}"')"
  [ "${seen}" = off ]
  [ "${KUBERC}" = /user/kuberc ]
}
