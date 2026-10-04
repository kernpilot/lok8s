#!/usr/bin/env bash
# parity-kubehz.sh — differential test between the Go `lo kubehz` and the
# argsh implementation (same structure as hack/parity-test.sh).
#
# Every case runs BOTH implementations (the Go binary, and the same binary
# routed to the frozen tree by the project file) against a synthetic
# project and diffs stdout, stderr and exit codes. Every case is
# cluster-free: config validation refusals, usage/flag errors, `status` with
# no registration, the hosting-axis routing of every subcommand, the
# handover bundle checks, and the first kubectl calls of `deploy` (the
# bind-secret stage) against a stub kubectl. No case reaches a kubeconfig, a
# real kubectl, the platform api or the Hetzner api. Two sections talk to one
# local https stub (hack/lib/kubehz-api-stub.py, a self-signed certificate
# that both implementations trust through SSL_CERT_FILE and
# CURL_CA_BUNDLE): the agent tools, and the register request bodies (the
# last section). There the harness also diffs the requests that each
# implementation sent. KUBEHZ_TOKEN and HCLOUD_TOKEN are unset except for
# dummy values in those two sections.
#
# The provision path renders a SERVER string too (the api's own refusal
# message, scrubbed, clipped, and in the bash tree escaped for `echo -e`),
# and no case here reaches it: spec.kubehz.apiUrl of a provision case is
# not the stub. That rendering is pinned by a golden PAIR both suites read:
# internal/kubehz/testdata/golden/space-above-shared-message.txt (the
# hostile input) and space-above-shared.txt (the bytes both must print),
# asserted by TestProvisionSharedScrubsTheSharedCeilingMessage and by
# tests/unit/kubehz_shared_test.bats.
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
source "$(dirname "${BASH_SOURCE[0]}")/lib/parity.sh"
parity::init "${1:-}"
# The kubehz tokens and agent keys are unset so no path can reach a real
# api or token endpoint; XDG_CACHE_HOME is unset so the token cache lives
# under the isolated HOME below.
unset KUBECONFIG KUBEHZ_TOKEN HCLOUD_TOKEN HCLOUD_API_BASE \
  KUBEHZ_HANDOVER_K8S_DIR KUBEHZ_HANDOVER_ETCD_DIR KUBEHZ_HANDOVER_ETCD_IMAGE_TAG \
  KUBEHZ_AGENT_CLIENT_ID KUBEHZ_AGENT_CLIENT_SECRET KUBEHZ_AGENT_TOKEN_URL KUBEHZ_AGENT_SCOPE \
  KUBEHZ_API_URL SSL_CERT_FILE CURL_CA_BUNDLE XDG_CACHE_HOME KUBERNETES_EXEC_INFO

# Isolated HOME so neither implementation can find a real ~/.kube/config.
export HOME="${WORK}/home"
mkdir -p "${HOME}"

CL="${PROJ}/clusters"
parity::new_project "${PROJ}"
# `kubehz claim-code` reads the agent Secret from the cluster in scope. With
# the real kubectl on PATH that call reaches whatever cluster the developer's
# ambient context points at (a live one, on a dev machine) and stalls the
# harness on its timeout. Stub it: no parity case may touch a cluster.
parity::own_bin "${PROJ}"
parity::stub_kubectl_fail "${PROJ}"
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
# space-*.dev — the SHAPE checks lo still makes on the three numbers. The
# ceiling is the account's and lives on the platform, so lo refuses only what
# can never be a ceiling: zero, a negative number, and a value that is not a
# whole number at all.
mk space-nodes-0.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: 0
EOF
mk space-nodes-neg.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: -1
EOF
mk space-cap-0.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        objectCapKiB: 0
EOF
mk space-nodes-float.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: 2.5
EOF
mk space-nodes-bool.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: true
EOF
mk space-ns-map.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        namespaces:
          a: 1
EOF
# The scalar contract is digits only, and it is the FROZEN TREE's: a sign, a
# padded value and a value past the integer range are refused on both sides,
# and a leading zero is a decimal (008 is eight, never octal).
mk space-nodes-plus.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: +5
EOF
mk space-nodes-padded.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: " 5 "
EOF
mk space-nodes-huge.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: 99999999999999999999
EOF
mk space-nodes-008.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: 008
EOF
# space-above-ceiling.dev — numbers the PLATFORM refuses and lo does not.
# Both implementations must let these through to the api, which answers with
# the account's real ceiling.
mk space-above-ceiling.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: 6
        namespaces: 4
        objectCapKiB: 513
EOF
# space-plan.dev — a retired plan; space-limits-list.dev — the machine-name
# list put under limits.nodes (the two fields confused).
mk space-plan.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      plan: shared-s
EOF
# A plan that yq's `//` reads as absent: still a plan, still refused.
mk space-plan-false.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      plan: false
EOF
mk space-limits-list.dev <<'EOF'
kind: Kubehz
spec:
  kubehz:
    hosting: shared
    apiUrl: https://api.kubehz.example
    space:
      limits:
        nodes: [worker-1, worker-2]
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
# op-reg.dev — agent operator, access registered, no bind secret on disk.
mk op-reg.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    agent: operator
    access: registered
    apiUrl: https://api.kubehz.example
EOF
# bind-cron.dev / bind-op.dev — a register stored a bind secret: the deploy
# stages it before it changes an agent, in both directions (B288).
mk bind-cron.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    access: registered
    apiUrl: https://api.kubehz.example
EOF
mk bind-op.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    agent: operator
    access: registered
    apiUrl: https://api.kubehz.example
EOF
for d in bind-cron.dev bind-op.dev; do
  printf %s 9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70 > "${CL}/${d}/.kubehz-bind"
done
# bind-bad.dev — the file holds no bind secret (a trailing newline): the
# deploy warns and stages nothing.
mk bind-bad.dev <<'EOF'
kind: KubeOne
spec:
  kubehz:
    access: registered
    apiUrl: https://api.kubehz.example
EOF
printf '%s\n' 9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70 > "${CL}/bind-bad.dev/.kubehz-bind"
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

# check_parse — an argsh parse error (rc 2 there, rc 1 here; message equal).
check_parse() { PARITY_PARSE_RC=1 check "$@"; }

UNBOUND='LOK8S_SPEC_FILE: unbound variable'
PARSEERR='bad file|cannot parse cluster spec'

# ── status: no registration, every hosting axis stops locally ───────────────
check - kubehz status
check - kh s
check - kubehz status --domain alpha.dev
check - kubehz status --domain nonexist.dev
check "${PARSEERR}" kubehz status --domain broken.dev

# ── the three numbers: the shapes lo refuses, and the one it passes on ─────
check - kubehz status --domain space-nodes-0.dev
check - kubehz status --domain space-nodes-neg.dev
check - kubehz status --domain space-cap-0.dev
check - kubehz status --domain space-nodes-float.dev
check - kubehz status --domain space-nodes-bool.dev
check - kubehz status --domain space-ns-map.dev
check - kubehz status --domain space-nodes-plus.dev
check - kubehz status --domain space-nodes-padded.dev
check - kubehz status --domain space-nodes-huge.dev
check - kubehz status --domain space-plan.dev
check - kubehz status --domain space-plan-false.dev
check - kubehz status --domain space-limits-list.dev

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
# Numbers the PLATFORM refuses and lo does not: validate_config reads them,
# accepts them, and deploy stops on the hosting rule. `status` cannot carry
# this case, because nothing local stops it and it would reach the api.
check - kubehz deploy --domain space-above-ceiling.dev
check - kubehz deploy --domain space-nodes-008.dev
check - kubehz deploy --domain hosted-http.dev
check - kubehz deploy --domain bad-agent.dev
check - kubehz deploy --domain operator-none.dev
check - kubehz deploy --domain lo-hosted.dev
check - kubehz deploy --domain kubehz-self.dev
check "${PARSEERR}" kubehz deploy --domain broken.dev

# ── deploy: the bind secret goes in before an agent changes (B288) ──────────
# A kubectl that names each call on stderr, answers the three staging calls
# and the dry-run render, and fails the first call after them. The diff then
# covers the staging calls, the stdin of the server-side apply and the first
# call that changes an agent, in order. The render dir is random per run (Go
# /tmp/<n>, bash /tmp/tmp.<x>), so the stub prints it as <work>.
parity::stub "${PROJ}" kubectl <<'SH'
#!/usr/bin/env bash
# Parity stub: no live cluster may be reached.
printf 'kubectl %s\n' "$*" | sed -E 's#[^ =]*/(agent|live-agent)\b#<work>/\1#g' >&2
case "$*" in
  "apply -f "*/agent/namespace.yaml) exit 0 ;;
  *"create secret generic kubehz-agent-bind "*"--dry-run=client -o yaml") printf 'kind: Secret\n'; exit 0 ;;
  "apply --server-side --force-conflicts -f -") sed 's/^/stdin: /' >&2; exit 0 ;;
  "kustomize "*) printf 'kind: Rendered\n'; exit 0 ;;
esac
exit 1
SH
check - kubehz deploy --domain bind-cron.dev
check - kubehz deploy --domain bind-op.dev
check - kubehz deploy --domain reg.dev                   # no bind secret: warn, then today's order
check - kubehz deploy --domain op-reg.dev
check - kubehz deploy --domain bind-bad.dev              # no bind secret in the file: warn, stage nothing
check - kubehz deploy --dry-run --domain bind-cron.dev    # the Secret line comes first
check - kubehz deploy --dry-run --domain reg.dev
# A failed stage stops the deploy before the CronJob agent.
parity::stub_kubectl_fail "${PROJ}"
check - kubehz deploy --domain bind-cron.dev
check - kubehz deploy --domain bind-op.dev
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

# ── token (agent-key exec plugin): the local refusals, no endpoint reached ──
check - kubehz token
check - kubehz token --token-url http://id.example/t --scope s
check - kubehz token --token-url https://id.example/t --scope s    # no key in the env
check - kubehz token --format yaml
check_parse - kubehz token --token-url https://id.example/t --scope s extra

# The success path, without a network: a seeded cache entry that both
# implementations must find under the same name and print the same way.
# id.example never resolves, so a cache miss would fail both sides, not
# reach an endpoint.
token_cache="${HOME}/.cache/lok8s/kubehz-token"
mkdir -p "${token_cache}" && chmod 700 "${token_cache}"
token_entry="${token_cache}/$(printf '%s\n%s\n%s' https://id.example/t parity-cid s | sha256sum | cut -c1-32).json"
printf '{"access_token":"parity-jwt","expires_at":4102444800}\n' > "${token_entry}"
chmod 600 "${token_entry}"
export KUBEHZ_AGENT_CLIENT_ID=parity-cid KUBEHZ_AGENT_CLIENT_SECRET=parity-secret
check - kubehz token --token-url https://id.example/t --scope s
# rc 0 only on a cache hit: a miss cannot reach id.example and fails.
expect_rc 0 kubehz token --token-url https://id.example/t --scope s
check - kubehz token --token-url https://id.example/t --scope s --format token
KUBERNETES_EXEC_INFO='{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential"}' \
  check - kubehz token --token-url https://id.example/t --scope s
unset KUBEHZ_AGENT_CLIENT_ID KUBEHZ_AGENT_CLIENT_SECRET

# ── agent tools (space / cluster): the local refusals, no api reached ───────
check - kubehz space list                                # KUBEHZ_API_URL unset
KUBEHZ_API_URL=http://api.kubehz.example check - kubehz cluster list
export KUBEHZ_API_URL=https://127.0.0.1:1
check - kubehz space list                                # no credential
KUBEHZ_AGENT_CLIENT_ID=cid check - kubehz space list     # half an agent key
KUBEHZ_TOKEN=$'a\nb' check - kubehz cluster list          # a line break in the bearer
check_parse - kubehz space get
check_parse - kubehz space get sp-1a2b3c4d sp-2
check_parse - kubehz cluster list extra
check - kubehz space get ../clusters
check - kubehz cluster get sp-1a2b3c4d
check - kubehz space get sp-1a2b3c4d -o xml
check_parse - kubehz space lease sp-1a2b3c4d
check - kubehz space lease sp-1a2b3c4d --hours 721
check - kubehz cluster lease cl-1a2b3c4d --hours 0x1
check_parse - kubehz space create --name Acme
check_parse - kubehz space create --slug acme
check - kubehz space create --name Acme --slug acme --nodes 0
check - kubehz space create --name Acme --slug acme --namespaces 99999999999999999999
check - kubehz space create --name Acme --slug acme --lease-hours 0
check_parse - kubehz cluster kubeconfig cl-1a2b3c4d
check_parse - kubehz space bogus
check_parse - kubehz cluster bogus
# Nothing listens on port 1: both answer "did not answer" the same way.
KUBEHZ_TOKEN=khzt_parity check - kubehz space get sp-1a2b3c4d
unset KUBEHZ_API_URL

# ── agent tools against an https stub of the api ────────────────────────────
# Both implementations call the stub with the agent key: each run mints its
# own token (the cache is cleared before every run), so the request logs
# carry the grant too. check_api diffs the logs on top of the outputs, and
# refuses an empty log (two empty logs prove nothing).
agent_stub() {
  command -v python3 >/dev/null 2>&1 && command -v openssl >/dev/null 2>&1 || return 1
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=127.0.0.1 \
    -addext subjectAltName=IP:127.0.0.1 -keyout "${WORK}/stub.key" -out "${WORK}/stub.pem" >/dev/null 2>&1 || return 1
  python3 "${ROOT}/hack/lib/kubehz-api-stub.py" "${WORK}/stub.pem" "${WORK}/stub.key" \
    "${WORK}/stub.port" "${WORK}/stub.log" >"${WORK}/stub.out" 2>&1 &
  STUB_API_PID=$!
  local _
  for _ in $(seq 1 100); do
    [[ -s "${WORK}/stub.port" ]] && return 0
    sleep 0.1
  done
  return 1
}
parity_cleanup() { [[ -z "${STUB_API_PID:-}" ]] || kill "${STUB_API_PID}" 2>/dev/null || :; }

# The kubeconfig fixtures (KC_SETUP names one; agent_pre runs it before each
# implementation): kc.yaml as a file that exists, a link to a file, a link
# to a directory, a link to nothing.
kc_exists() { echo contexts > "${PROJ}/kc.yaml"; }
kc_link_file() { mkdir -p "${PROJ}/kube"; echo contexts > "${PROJ}/kube/real"; ln -s kube/real "${PROJ}/kc.yaml"; }
kc_link_dir() { mkdir -p "${PROJ}/kc-dir"; ln -s kc-dir "${PROJ}/kc.yaml"; }
kc_link_nothing() { ln -s gone "${PROJ}/kc.yaml"; }

agent_pre() {
  rm -rf "${HOME}/.cache/lok8s/kubehz-token" "${PROJ}/kc.yaml" "${PROJ}/kube" "${PROJ}/kc-dir"
  : > "${WORK}/stub.log"
  [[ -z "${KC_SETUP:-}" ]] || "${KC_SETUP}"
}
# agent_post: the requests, and what kc.yaml is afterwards: a link or a
# file, its content and mode (followed), and the content of kube/real.
agent_post() {
  cp "${WORK}/stub.log" "${WORK}/req.${1}"
  if [[ -e "${PROJ}/kc.yaml" || -L "${PROJ}/kc.yaml" ]]; then
    {
      if [[ -L "${PROJ}/kc.yaml" ]]; then echo "link -> $(readlink "${PROJ}/kc.yaml")"; else echo file; fi
      if [[ -f "${PROJ}/kc.yaml" ]]; then cat "${PROJ}/kc.yaml"; stat -L -c '%a' "${PROJ}/kc.yaml"; fi
      if [[ -f "${PROJ}/kube/real" ]]; then echo "kube/real:"; cat "${PROJ}/kube/real"; ls -A "${PROJ}/kube"; fi
    } > "${WORK}/kc.${1}"
  else
    rm -f "${WORK}/kc.${1}"
  fi
}

# check_api <allow|-> <argv...>: check, then the request logs must match and
# hold at least the grant.
check_api() {
  PARITY_PRE_EACH=agent_pre PARITY_POST_EACH=agent_post check "$@"
  shift
  if [[ ! -s "${WORK}/req.go" ]]; then
    fail "requests lo $* — the Go run sent nothing; the diff proves nothing"
  elif diff -q "${WORK}/req.bash" "${WORK}/req.go" >/dev/null; then
    echo "ok: requests lo $*"
  else
    fail "requests lo $* — the two implementations sent different requests:"
    diff "${WORK}/req.bash" "${WORK}/req.go" | head -10 | sed 's/^/  /' || true
  fi
}

# check_api_none <allow|-> <argv...>: check, then neither implementation may
# have sent a request (a local refusal costs no grant and no download).
check_api_none() {
  PARITY_PRE_EACH=agent_pre PARITY_POST_EACH=agent_post check "$@"
  shift
  if [[ -s "${WORK}/req.go" || -s "${WORK}/req.bash" ]]; then
    fail "requests lo $* — a local refusal sent requests"
  else
    echo "ok: no requests lo $*"
  fi
}

# kc_state <label>: kc.yaml (and a link target) is the same in both, and
# exists (a missing file in both would prove nothing).
kc_state() {
  if [[ ! -e "${WORK}/kc.go" ]]; then
    fail "kubeconfig file (${1}) — the Go run left no kc.yaml"
  elif ! parity::state_same "${WORK}/kc.bash" "${WORK}/kc.go" "kubeconfig file (${1})"; then
    failures=$((failures + 1))
  fi
}

if agent_stub; then
  STUB_PORT="$(cat "${WORK}/stub.port")"
  export SSL_CERT_FILE="${WORK}/stub.pem" CURL_CA_BUNDLE="${WORK}/stub.pem"
  export KUBEHZ_API_URL="https://127.0.0.1:${STUB_PORT}/"
  export KUBEHZ_AGENT_CLIENT_ID=parity-cid KUBEHZ_AGENT_CLIENT_SECRET=parity-secret
  export KUBEHZ_AGENT_TOKEN_URL="https://127.0.0.1:${STUB_PORT}/oauth/v2/token" KUBEHZ_AGENT_SCOPE=parity-scope

  for format in text json yaml; do
    check_api - kubehz space list -o "${format}"
    check_api - kubehz space get sp-1a2b3c4d -o "${format}"
    check_api - kubehz cluster list -o "${format}"     # a short page: the warning
    check_api - kubehz cluster get cl-1a2b3c4d -o "${format}"
    check_api - kubehz space delete sp-1a2b3c4d -o "${format}"
    check_api - kubehz space lease sp-1a2b3c4d --hours 024 -o "${format}"
    check_api - kubehz cluster lease cl-1a2b3c4d --hours 3 -o "${format}"
    check_api - kubehz space create --name 'Acme Prod' --slug acme -o "${format}"
  done
  check_api - kubehz space create --name Acme --slug acme --nodes 2 --namespaces 008 \
    --object-cap-kib 256 --region fsn1 --lease-hours 0720
  for slug in taken big full capped; do
    check_api - kubehz space create --name Acme --slug "${slug}"
  done
  for id in sp-gone0001 sp-broken01 sp-boom0001 sp-hostile1 sp-noread01; do
    check_api - kubehz space get "${id}"
  done
  check_api - kubehz space delete sp-gone0001
  check_api - kubehz space lease sp-capped01 --hours 2
  check_api - kubehz cluster lease cl-self0001 --hours 3
  check_api - kubehz cluster lease cl-viewer01 --hours 3
  check_api - kubehz cluster kubeconfig cl-pending1 --file kc.yaml
  check_api - kubehz space kubeconfig sp-dedicat1 --file kc.yaml

  # The kubeconfig: the same bytes and mode in both, the path on stdout.
  for format in text json; do
    check_api - kubehz space kubeconfig sp-1a2b3c4d --file kc.yaml -o "${format}"
    kc_state "-o ${format}"
  done
  # The path rules: a directory or a link to one is refused, a path that
  # exists needs --force, --force writes through a link.
  check_api_none - kubehz cluster kubeconfig cl-1a2b3c4d --file clusters
  KC_SETUP=kc_link_dir check_api_none - kubehz cluster kubeconfig cl-1a2b3c4d --file kc.yaml --force
  KC_SETUP=kc_exists check_api_none - kubehz space kubeconfig sp-1a2b3c4d --file kc.yaml
  kc_state "a file that exists, no --force"
  KC_SETUP=kc_link_nothing check_api_none - kubehz space kubeconfig sp-1a2b3c4d --file kc.yaml
  KC_SETUP=kc_link_nothing check_api_none - kubehz space kubeconfig sp-1a2b3c4d --file kc.yaml --force
  KC_SETUP=kc_exists check_api - kubehz space kubeconfig sp-1a2b3c4d --file kc.yaml --force
  kc_state "--force replaces a file"
  KC_SETUP=kc_link_file check_api - kubehz space kubeconfig sp-1a2b3c4d --file kc.yaml -f
  kc_state "-f writes through a link"

  # The grant follows no redirect: the stub's redirect route would hand
  # the client secret to the token endpoint, and no request may reach it.
  KUBEHZ_AGENT_TOKEN_URL="https://127.0.0.1:${STUB_PORT}/oauth/v2/redirect" check_api - kubehz space list
  if grep -q '"/oauth/v2/token"' "${WORK}/req.go" "${WORK}/req.bash"; then
    fail "a token grant followed the redirect"
  else
    echo "ok: no grant followed the redirect"
  fi

  # KUBEHZ_TOKEN without a key: no grant; the minted token is the stub's
  # bearer, so the same value passes, another one is refused.
  unset KUBEHZ_AGENT_CLIENT_ID KUBEHZ_AGENT_CLIENT_SECRET
  KUBEHZ_TOKEN=parity-jwt check_api - kubehz space list -o json
  KUBEHZ_TOKEN=parity-jwt check_api - kubehz space get sp-noread01   # the api's help, not lo's hint
  KUBEHZ_TOKEN=wrong check_api - kubehz space get sp-1a2b3c4d
  export KUBEHZ_AGENT_CLIENT_ID=parity-cid KUBEHZ_AGENT_CLIENT_SECRET=parity-secret
  # The contract on the Go side alone: 0 on success, 1 on a refusal.
  expect_rc 0 kubehz space list
  expect_rc 1 kubehz space get sp-gone0001

  unset SSL_CERT_FILE CURL_CA_BUNDLE KUBEHZ_API_URL KUBEHZ_AGENT_CLIENT_ID KUBEHZ_AGENT_CLIENT_SECRET \
    KUBEHZ_AGENT_TOKEN_URL KUBEHZ_AGENT_SCOPE
elif [[ -n "${CI:-}" ]]; then
  fail "agent tools: the https stub did not start (python3 and openssl are on every CI runner)"
else
  echo "skip: agent tools https stub (python3 or openssl missing, or the stub did not start)"
fi

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

# ── register: lo sends its stored bind secret (B289) ────────────────────────
# Every register mode sends the value of .kubehz-bind as bindSecret, so a
# re-run keeps the same cluster record. These cases need an api, so they run
# against a local HTTPS stub, hack/lib/kubehz-api-stub.py (a self-signed
# certificate that SSL_CERT_FILE hands to Go and CURL_CA_BUNDLE to curl).
# The stub records each request without the Authorization value. After each
# run, the record and the stored .kubehz-bind join that run's stdout, so the
# diff covers the request bodies and the stored secret. Linux only: Go reads
# SSL_CERT_FILE there.
BIND_STORED=9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70
STUB_HTTPS_PID=""
parity_cleanup() { [[ -z "${STUB_HTTPS_PID}" ]] || kill "${STUB_HTTPS_PID}" 2>/dev/null || :; }
if [[ "$(uname -s)" != Linux ]] || ! command -v python3 >/dev/null 2>&1 || ! command -v openssl >/dev/null 2>&1; then
  echo "skip: register bind-secret cases (need Linux, python3 and openssl)"
else
  mkdir -p "${WORK}/tls"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
    -subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 \
    -keyout "${WORK}/tls/key.pem" -out "${WORK}/tls/cert.pem" 2>/dev/null
  python3 -u "${PARITY_LIB_DIR}/kubehz-api-stub.py" "${WORK}/tls/requests.log" "${WORK}/tls/cert.pem" "${WORK}/tls/key.pem" \
    >"${WORK}/tls/stub.out" 2>&1 &
  STUB_HTTPS_PID=$!
  STUB_PORT=""
  for _ in $(seq 1 50); do
    STUB_PORT="$(sed -nE 's/^port ([0-9]+)$/\1/p' "${WORK}/tls/stub.out")"
    [[ -n "${STUB_PORT}" ]] && break
    sleep 0.1
  done
  if [[ -z "${STUB_PORT}" ]]; then
    echo "FAIL: register bind-secret cases — the HTTPS stub did not start: $(cat "${WORK}/tls/stub.out")"
    failures=$((failures + 1))
  else
    mk bind-reg.dev <<EOF
kind: Lo
metadata:
  name: bind-reg
spec:
  cluster:
    domain: bind-reg.dev
  kubehz:
    access: registered
    apiUrl: https://127.0.0.1:${STUB_PORT}
EOF
    export SSL_CERT_FILE="${WORK}/tls/cert.pem" CURL_CA_BUNDLE="${WORK}/tls/cert.pem"
    # BIND_FIXTURE: what the slot .kubehz-bind holds before each run: a valid
    # value, one with a trailing newline, one in upper case, a valid value in
    # a file nobody may read, a valid value with 200 kB after it, a
    # directory, a link to a directory, or no file.
    bind_reset() {
      rm -rf "${CL}/bind-reg.dev/.kubehz-bind" "${WORK}/bind-target"
      : > "${WORK}/tls/requests.log"
      case "${BIND_FIXTURE}" in
        valid) printf %s "${BIND_STORED}" > "${CL}/bind-reg.dev/.kubehz-bind" ;;
        newline) printf '%s\n' "${BIND_STORED}" > "${CL}/bind-reg.dev/.kubehz-bind" ;;
        upper) printf %s "${BIND_STORED^^}" > "${CL}/bind-reg.dev/.kubehz-bind" ;;
        unreadable)
          printf %s "${BIND_STORED}" > "${CL}/bind-reg.dev/.kubehz-bind"
          chmod 000 "${CL}/bind-reg.dev/.kubehz-bind"
          ;;
        oversized)
          { printf %s "${BIND_STORED}"; head -c 200000 /dev/zero | tr '\0' a; } > "${CL}/bind-reg.dev/.kubehz-bind"
          ;;
        directory) mkdir "${CL}/bind-reg.dev/.kubehz-bind" ;;
        dirlink)
          mkdir "${WORK}/bind-target"
          ln -s "${WORK}/bind-target" "${CL}/bind-reg.dev/.kubehz-bind"
          ;;
      esac
    }
    bind_record() {
      {
        echo "--- requests"
        cat "${WORK}/tls/requests.log"
        echo "--- .kubehz-bind"
        cat "${CL}/bind-reg.dev/.kubehz-bind" 2>/dev/null || echo "(none)"
        echo
        echo "--- clusters/bind-reg.dev"
        ls -A "${CL}/bind-reg.dev"
        echo "--- link target"
        ls -A "${WORK}/bind-target" 2>/dev/null || echo "(none)"
      } >> "${WORK}/${1}.out"
    }
    bind_check() { PARITY_PRE_EACH=bind_reset PARITY_POST_EACH=bind_record check "$@"; }
    BIND_FIXTURES=(valid newline upper oversized directory dirlink missing)
    # root reads a 0000 file, so the case proves nothing there.
    [[ "$(id -u)" == 0 ]] || BIND_FIXTURES+=(unreadable)
    for BIND_FIXTURE in "${BIND_FIXTURES[@]}"; do
      bind_check "${UNBOUND}" kubehz register --domain bind-reg.dev            # fingerprint announce
      export KUBEHZ_TOKEN=khzt_parity
      bind_check "${UNBOUND}" kubehz register --domain bind-reg.dev            # bearer (direct claim)
      unset KUBEHZ_TOKEN
      export HCLOUD_TOKEN=hc_parity HCLOUD_API_BASE="https://127.0.0.1:${STUB_PORT}"
      bind_check "${UNBOUND}" kubehz register --domain bind-reg.dev            # claim key
      unset HCLOUD_TOKEN HCLOUD_API_BASE
    done
    unset SSL_CERT_FILE CURL_CA_BUNDLE BIND_FIXTURE BIND_FIXTURES
  fi
fi

report
