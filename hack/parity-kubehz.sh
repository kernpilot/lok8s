#!/usr/bin/env bash
# parity-kubehz.sh — differential test between the Go `lo kubehz` and the
# argsh implementation (same structure as hack/parity-test.sh).
#
# Every case runs BOTH implementations (the Go binary, and the same binary
# with LO_IMPL=bash forcing the argsh passthrough) against a synthetic
# project and diffs stdout, stderr and exit codes. ONLY cluster-free and
# api-free paths are exercised: config validation refusals, usage/flag
# errors, `status` with no registration, the hosting-axis routing of every
# subcommand, and the handover bundle checks. No case reaches a kubeconfig,
# kubectl, the platform api or the Hetzner api — KUBEHZ_TOKEN/HCLOUD_TOKEN
# are unset and every api-bearing path stops at a local refusal.
#
# Known, deliberate divergences (allowed via the per-check regex):
#   - argsh parse errors exit 2, the Go binary exits 1 (the cli-wide
#     convention): check_parse tolerates exactly that rc pair.
#   - `lo kubehz register|join` in bash print a spurious
#     "LOK8S_SPEC_FILE: unbound variable" line (validate_config reads a
#     variable only provision::resolve_spec sets, and register calls it
#     AFTER validate) — a bash defect; the Go port passes the spec path.
#     The same defect makes validate_config's PER-KIND rules dead on those
#     two verbs (kind reads as ""), so the kind-rule domains are exercised
#     through `deploy`, where the bash sets LOK8S_SPEC_FILE itself.
#   - an unparsable spec: the bash surfaces yq's own "Error: bad file …"
#     line, the Go port its "[error] cannot parse cluster spec: …" line —
#     same rc, allowed via PARSEERR.
#   - `lo kubehz node … --cluster X`: through the real `lo`, argsh's
#     inherited global flag consumes --cluster BEFORE node::reject_global_
#     cluster_flag sees argv, so the bash guard (pinned by the bats suite on
#     a direct node::join call) is dead on dispatch and the flag is silently
#     ignored. The Go port refuses it as the guard intends — not a parity
#     case.
#
# Usage: hack/parity-kubehz.sh [path-to-go-lo]   (default: bin/lo)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${1:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# The harness must run against its synthetic project ONLY. Dev shells export
# PATH_BASE and friends pointing at a real lok8s project (direnv); inherited,
# they silently redirect BOTH implementations into that live repo instead of
# ${WORK}. The kubehz tokens are unset so no path can reach a real api.
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
  DOMAIN_NAME LOK8S_CLUSTER_NAME LOK8S_SSH_KEY SOPS_AGE_KEY SOPS_AGE_KEY_FILE DEBUG \
  KUBECONFIG KUBEHZ_TOKEN HCLOUD_TOKEN HCLOUD_API_BASE \
  KUBEHZ_HANDOVER_K8S_DIR KUBEHZ_HANDOVER_ETCD_DIR KUBEHZ_HANDOVER_ETCD_IMAGE_TAG

# Pin the C locale: bash glob expansion sorts by LC_COLLATE, and the Go port
# lists in byte order (= C collation).
export LC_ALL=C

# Isolated HOME so neither implementation can find a real ~/.kube/config.
export HOME="${WORK}/home"
mkdir -p "${HOME}"

PROJ="${WORK}/proj"
CL="${PROJ}/clusters"
mkdir -p "${CL}"
ln -s "${ROOT}/.lok8s" "${PROJ}/.lok8s"
ln -s "${ROOT}/.bin" "${PROJ}/.bin"
echo "alpha.dev" > "${CL}/.active"

# ── Synthetic domains ────────────────────────────────────────────────────────
mk() { mkdir -p "${CL}/$1"; cat > "${CL}/$1/cluster.lok8s.yaml"; }

# alpha.dev — Lo, no kubehz block: hosting self / access none everywhere.
mk alpha.dev <<'EOF'
kind: Lo
metadata:
  name: alpha
EOF
# reg.dev — self-hosted, registered, https api: routing without a token.
mk reg.dev <<'EOF'
kind: KubeOne
metadata:
  name: reg
spec:
  kubehz:
    access: registered
    apiUrl: https://api.kubehz.example
EOF
# shared.dev — a Space: the shared-hosting routing of every verb.
mk shared.dev <<'EOF'
kind: Kubehz
metadata:
  name: shared
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
EOF
# hosted-http.dev — hosted with a plain-http apiUrl: the https gate.
mk hosted-http.dev <<'EOF'
kind: KubeOne
metadata:
  name: hosted
spec:
  kubehz:
    hosting: hosted
    access: managed
    apiUrl: http://api.kubehz.example
EOF
# bad-access.dev / bad-agent.dev / bad-channel.dev / bad-mw.dev — one
# validate_config refusal each.
mk bad-access.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    access: weird
    apiUrl: https://api.kubehz.example
EOF
mk bad-agent.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    access: registered
    apiUrl: https://api.kubehz.example
    agent: sidecar
EOF
mk bad-channel.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    access: registered
    apiUrl: https://api.kubehz.example
    upgrades:
      channel: major
EOF
mk bad-mw.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    access: registered
    apiUrl: https://api.kubehz.example
    maintenanceWindow:
      exclusions: ["2026-12-20/2027-01-06", "christmas week"]
EOF
# lo-hosted.dev — kind Lo + hosted without spec.runner (the per-kind rule).
mk lo-hosted.dev <<'EOF'
kind: Lo
spec:
  kubehz:
    hosting: hosted
    access: managed
    apiUrl: https://api.kubehz.example
EOF
# kubehz-self.dev — kind Kubehz without hosting: shared.
mk kubehz-self.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    access: none
EOF
# shared-reg.dev — hosting shared + access registered (a Space has no agent).
mk shared-reg.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    access: registered
    apiUrl: https://api.kubehz.example
EOF
# operator-none.dev — agent operator with access none.
mk operator-none.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    agent: operator
EOF
# broken.dev — unparsable spec.
mkdir -p "${CL}/broken.dev"
printf '{{ not yaml' > "${CL}/broken.dev/cluster.lok8s.yaml"

# An incomplete handover bundle (a directory missing one key) and a plain
# file that is not an archive.
mkdir -p "${WORK}/bundle"
for k in ca.crt ca.key sa.pub sa.key front-proxy-ca.crt front-proxy-ca.key encryption-key snapshot-location; do
  printf 'x\n' > "${WORK}/bundle/${k}"
done
printf 'not an archive\n' > "${WORK}/notarchive"

failures=0

# check <allow-diff-regex|-> <argv...>
check() {
  local allow="${1}"; shift
  local go_rc=0 bash_rc=0
  (cd "${PROJ}" && "${LO_BIN}" "$@" </dev/null >"${WORK}/go.out" 2>"${WORK}/go.err") || go_rc=$?
  (cd "${PROJ}" && LO_IMPL=bash "${LO_BIN}" "$@" </dev/null >"${WORK}/bash.out" 2>"${WORK}/bash.err") || bash_rc=$?
  sed -i "s|${WORK}|WORK|g" "${WORK}/go.out" "${WORK}/go.err" "${WORK}/bash.out" "${WORK}/bash.err"

  local ok=1
  if (( go_rc != bash_rc )); then
    # Parse errors: argsh exits 2, the Go binary 1 (cli-wide convention).
    if [[ "${PARSE_CASE:-0}" == 1 && ${bash_rc} -eq 2 && ${go_rc} -eq 1 ]]; then
      :
    else
      echo "FAIL: lo $* — rc: bash=${bash_rc} go=${go_rc}"
      ok=0
    fi
  fi
  local stream diff_out
  for stream in out err; do
    if [[ "${allow}" == "-" ]]; then
      diff_out="$(diff "${WORK}/bash.${stream}" "${WORK}/go.${stream}" || true)"
    else
      diff_out="$(diff <(grep -vE "${allow}" "${WORK}/bash.${stream}") \
                       <(grep -vE "${allow}" "${WORK}/go.${stream}") || true)"
    fi
    if [[ -n "${diff_out}" ]]; then
      echo "FAIL: lo $* — std${stream} differs:"
      echo "${diff_out}" | head -20 | sed 's/^/  /'
      ok=0
    fi
  done
  if (( ok )); then
    echo "ok: lo $*"
  else
    failures=$((failures + 1))
  fi
}

# check_parse — an argsh parse error (rc 2 there, rc 1 here; message equal).
check_parse() { PARSE_CASE=1 check "$@"; }

UNBOUND='LOK8S_SPEC_FILE: unbound variable'
PARSEERR='bad file|cannot parse cluster spec'

# ── status: no registration, every hosting axis stops locally ───────────────
check - kubehz status
check - kh s
check - kubehz status --domain alpha.dev
check - kubehz status --domain nonexist.dev
check "${PARSEERR}" kubehz status --domain broken.dev

# ── register / deregister: config validation refusals ───────────────────────
check "${UNBOUND}" kubehz register
check "${UNBOUND}" kubehz r
check "${UNBOUND}" kubehz register --domain shared.dev
check - kubehz register --domain bad-access.dev
check - kubehz register --domain bad-agent.dev
check - kubehz register --domain bad-channel.dev
check - kubehz register --domain bad-mw.dev
check - kubehz register --domain hosted-http.dev
check - kubehz register --domain shared-reg.dev
check - kubehz register --domain operator-none.dev
check "${PARSEERR}" kubehz register --domain broken.dev
check - kubehz deregister
check - kubehz d --domain alpha.dev
check - kubehz deregister --domain nonexist.dev

# ── deploy / re-enroll / assess: routing without a cluster ──────────────────
check - kubehz deploy
check - kubehz deploy --domain shared.dev
check - kubehz deploy --domain hosted-http.dev
check - kubehz deploy --domain bad-agent.dev
check - kubehz deploy --domain operator-none.dev
check - kubehz deploy --domain lo-hosted.dev
check - kubehz deploy --domain kubehz-self.dev
check "${PARSEERR}" kubehz deploy --domain broken.dev
check - kubehz re-enroll
check - kubehz re-enroll --domain shared.dev
check - kubehz re-enroll --domain hosted-http.dev
check - kubehz assess
check - kubehz a --domain shared.dev
check - kubehz assess --domain hosted-http.dev

# ── join (space ticket) / claim: usage errors and local refusals ────────────
check_parse - kubehz join
check - kubehz join n1
check "${UNBOUND}" kubehz join n1 --domain shared.dev
check "${UNBOUND}" kubehz j n1 --domain shared-reg.dev
check_parse - kubehz claim
check - kubehz claim --nonce bad
check - kubehz claim -n khzn_short
check - kubehz claim-code                                # no cluster reachable → local refusal

# ── node: the hosting gate, the https gate, the global --cluster trap ───────
check - kubehz node join
check - kubehz n j --domain shared.dev
check - kubehz node join --domain hosted-http.dev
check - kubehz node join --domain nonexist.dev
check_parse - kubehz node remove
check - kubehz node remove --name Bad_
check - kubehz node remove --name ../../clusters
check - kubehz node status --domain hosted-http.dev
check - kubehz node status --domain alpha.dev

# ── handover: usage errors and bundle checks (no node touched) ──────────────
check_parse - kubehz handover receive
check_parse - kubehz handover preseed --bundle "${WORK}/bundle"
check - kubehz handover receive --bundle /nonexistent
check - kubehz handover receive --bundle "${WORK}/bundle" --snapshot /nonexistent
check - kubehz handover preseed --bundle "${WORK}/bundle" --node 203.0.113.7
check - kubehz handover preseed --bundle /nonexistent --node 203.0.113.7
check - kubehz handover receive --bundle "${WORK}/notarchive"
check - kubehz h r -b "${WORK}/notarchive"

# ── dispatch: unknown subcommands ───────────────────────────────────────────
check_parse - kubehz bogus
check_parse - kubehz node bogus
check_parse - kubehz handover bogus

if (( failures )); then
  echo; echo "${failures} parity failure(s)"
  exit 1
fi
echo; echo "parity: all checks passed"
