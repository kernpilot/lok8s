#!/usr/bin/env bash
# parity-kubehz.sh — differential test between the Go `lo kubehz` and the
# argsh implementation (same structure as hack/parity-test.sh).
#
# Every case runs BOTH implementations (the Go binary, and the same binary
# routed to the frozen tree by the project file) against a synthetic
# project and diffs stdout, stderr and exit codes. ONLY cluster-free paths
# are exercised: config validation refusals, usage/flag errors, `status`
# with no registration, the hosting-axis routing of every subcommand, the
# handover bundle checks, the first kubectl calls of `deploy` (the
# bind-secret stage) against a stub, and the register request bodies against
# a local HTTPS stub (the last section). No case reaches a kubeconfig, a
# real kubectl, the platform api or the Hetzner api — KUBEHZ_TOKEN and
# HCLOUD_TOKEN are unset except for dummy values in the register section.
#
# What this harness CANNOT cover: how the two implementations render a SERVER
# string (the api's own refusal message — scrubbed, clipped, and in the bash
# tree escaped for `echo -e`). Only the register section talks to a stub,
# and that stub answers 2xx. That rendering is pinned instead by a golden
# PAIR both suites read: internal/kubehz/testdata/golden/space-above-shared-message.txt
# (the hostile input) and space-above-shared.txt (the bytes both must print),
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
  XDG_CACHE_HOME KUBERNETES_EXEC_INFO

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
    # directory, a link to a directory, a valid value next to a stale
    # .kubehz-bind.* file of a killed write, or no file.
    bind_reset() {
      rm -rf "${CL}/bind-reg.dev"/.kubehz-bind* "${WORK}/bind-target"
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
        stale)
          printf %s "${BIND_STORED}" > "${CL}/bind-reg.dev/.kubehz-bind"
          printf %s "${BIND_STORED}" > "${CL}/bind-reg.dev/.kubehz-bind.123456"
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
    BIND_FIXTURES=(valid newline upper oversized directory dirlink stale missing)
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
