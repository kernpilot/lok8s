# Security Audit (`lo audit`)

`lo audit` is a **static, read-only security-posture audit**. It reads your
`cluster.lok8s.yaml` plus the addon/kustomize inputs — exactly like `lo lint` —
and reports security findings with a severity, a per-cluster score, and a
non-zero exit when anything **fails**. It never touches a live cluster and needs
no kubeconfig, so it runs offline and in CI.

```bash
lo audit                 # audit the active domain (lo use <domain>)
lo audit my-cluster.example.com   # audit a specific cluster
lo audit --json          # machine-readable output (stable schema, see below)
lo audit --sarif         # SARIF 2.1.0 for GitHub code scanning (see below)
```

## What it checks

Each check is fail-soft: an input it cannot read yields `unknown` (never an
error), so the audit always completes. One deliberate fail-**closed** exception:
an `EncryptionConfiguration` that is *present but unparseable* counts as
not-encrypting → **fail**, not unknown — a config that provably exists but can't
be proven to encrypt must not be trusted.

| id | severity | What it looks at |
|----|----------|------------------|
| `encryption-at-rest` | high | Secret encryption at rest (etcd). For KubeOne the authoritative signal is the driver's `features.encryptionProviders.enable`; a `spec.features.encryptionProviders.enable` override wins; a rendered `EncryptionConfiguration` in `lo build` artifacts counts as proof **only when a real provider (`aescbc`/`secretbox`/`kms`) is the write provider for the FIRST resource group covering `secrets`** (apiserver precedence: the first matching group wins — a later wildcard group can't rescue a `secrets → identity` group) — an `identity`-only or empty config encrypts nothing and is treated as disabled. **Disabled on a prod-intent cluster → fail.** A local `kind` (`kind: Lo`) cluster is **always** not-applicable (pass), even if an encryption config is present. |
| `cilium-policy-enforcement` | high | Cilium `policyAuditMode` / `policyEnforcementMode` in the **effective** cilium values (base < driver < provider < inline). **Audit mode = insecure** (see below). Values that fail to parse/merge (e.g. a broken inline override) → `unknown`, never a silent pass. |
| `exposed-endpoints` | medium / low | `NodePort` and `LoadBalancer` Services, `HTTPRoute`s, and whether a default-Deny `SecurityPolicy` (IP-allowlist) carve-out fronts them. |
| `k8s-version-support` | high / medium | Is `spec.kubernetes.version` a still-supported minor? EOL on a prod-intent cluster is a fail; on a dev cluster it's a warn. |
| `privileged-workloads` | medium | `privileged`, `hostNetwork`, or `hostPath` in the cluster's **own targets** (the vetted framework addons are out of scope to avoid noise). |
| `plaintext-endpoints` | high / medium | A non-HTTPS `spec.oidc.issuer` (fail — the apiserver would trust tokens over cleartext) and `http://` endpoints referenced in targets (warn). |

"Prod-intent" is every cluster `kind` **except** `Lo` (the local kind driver) —
an empty or unknown `kind` is scored as prod-intent too, fail-closed: a cluster
that cannot be proven to be a dev cluster is treated like production.

### Cilium: audit mode vs enforce (the headline)

Cilium can run NetworkPolicy in two modes:

- **Enforce** (`policyAuditMode: false`) — a NetworkPolicy that selects a pod
  actually **drops** traffic it doesn't allow. This is what "network policy"
  means.
- **Audit** (`policyAuditMode: true`) — every policy verdict is **logged but
  nothing is dropped**. Policies that look like they lock the cluster down
  enforce **nothing**. You develop the allow-set with
  `hubble observe --verdict AUDIT`, confirm nothing critical is denied, and only
  then flip to enforce.

lok8s ships `policyAuditMode: true` as the **KubeOne default**
(`.lok8s/addons/cilium/values.kubeone.yaml`) precisely because flipping straight
to enforce without a complete allow-set has deadlocked the pilot (a default-deny
that cut etcd peer traffic → quorum loss). Audit mode is the safe way to build
the allow-set — but it is **not** a secure end state, so `lo audit` flags it as a
**high** finding. Running `lo audit` on the stock pilot config reports:

```
  FAIL [high    ] cilium-policy-enforcement — Cilium network-policy enforcement
         Cilium policyAuditMode: true — NetworkPolicies (incl. host-firewall CCNPs)
         are LOGGED, not enforced; nothing is actually blocked.
         remediation: Validate the allow-set (hubble observe --verdict AUDIT covers
         etcd 2379/2380, apiserver 6443, kubelet 10250, vxlan 8472), then set
         policyAuditMode: false in addons/cilium/values.kubeone.yaml to ENFORCE.
```

That is by design: it is a standing reminder that the pilot has host-firewall
policies defined but not yet enforcing. To clear it, complete the allow-set and
set `policyAuditMode: false` (per-cluster via a `spec.bootstrap` cilium override,
or in the driver values file once validated everywhere).

## Scoring

The report starts at 100 and subtracts a severity-weighted penalty per finding:

| | critical | high | medium | low |
|---|---|---|---|---|
| **fail** | 40 | 25 | 15 | 5 |
| **warn** | 15 | 10 | 5 | 2 |

The score is clamped to `[0, 100]` and mapped to a grade (`A` ≥ 90, `B` ≥ 80,
`C` ≥ 70, `D` ≥ 60, `F` < 60). A `pass` costs nothing, and a low-severity
`unknown` is free too — but a **high/critical check that cannot be evaluated**
(`unknown`, e.g. a deploy-only or unreadable cluster) caps the score at **70
(grade C at best)**: "couldn't check" must never read as a perfect score for
score-keyed tooling. The command exits non-zero **iff** any finding has status
`fail`, so it gates CI.

Findings are printed most-actionable first (fail → warn → unknown → pass, and by
severity within each group).

## JSON output

`--json` emits a **stable schema** intended for tooling (e.g. a dashboard
Security tab). For a single domain it is one object; for several it is a JSON
array of these objects.

```json
{
  "domain": "my-cluster.example.com",
  "score": 75,
  "grade": "C",
  "summary": { "pass": 5, "warn": 0, "fail": 1, "unknown": 0 },
  "checks": [
    {
      "id": "cilium-policy-enforcement",
      "title": "Cilium network-policy enforcement",
      "severity": "high",
      "status": "fail",
      "detail": "Cilium policyAuditMode: true — NetworkPolicies … are LOGGED, not enforced.",
      "remediation": "Validate the allow-set …, then set policyAuditMode: false to ENFORCE."
    }
  ]
}
```

Field contract:

- `severity` ∈ `critical` | `high` | `medium` | `low`
- `status` ∈ `pass` | `warn` | `fail` | `unknown`
- every `checks[]` entry always carries `id`, `title`, `severity`, `status`,
  `detail`, `remediation`.

## SARIF output (GitHub code scanning)

`--sarif` emits a [SARIF 2.1.0](https://docs.oasis-open.org/sarif/sarif/v2.1.0/)
document on stdout. Upload it to GitHub code scanning and findings show up as
alerts on the Security tab, with file annotations where the audit can point at
one.

How findings map:

- One SARIF run; the tool is `lo-audit`. Each check id becomes a rule.
- `status` sets the alert level: `fail` → `error`, `warn` → `warning`,
  `unknown` → `note`. Unknown is uploaded on purpose — "could not check" is a
  finding, not a confirmation.
- `pass` findings are **not** uploaded. A clean audit produces `results: []`,
  so zero alerts.
- A finding keyed to one spec value (an EOL `spec.kubernetes.version`, a
  plaintext `spec.oidc.issuer`) carries a repo-relative file location and line.
  Aggregate findings (a scan over many manifests) carry no location.
- The result message is the finding's detail plus its remediation. The
  finding's own `severity`, `status`, and domain ride in each result's
  `properties` bag.
- `--sarif` and `--json` are mutually exclusive. The exit code stays the same
  as every other mode: non-zero **iff** a finding fails.

CI example:

```yaml
- name: Security audit
  run: lo audit --sarif > audit.sarif
  # non-zero exit when a finding fails — still upload the report
  continue-on-error: true

- name: Upload to code scanning
  uses: github/codeql-action/upload-sarif@v3
  with:
    sarif_file: audit.sarif
```

## Kubernetes version support list

The supported-minors list is static (the audit is cluster-free) and lives in
`.lok8s/libs/audit`:

```bash
_AUDIT_K8S_SUPPORTED_MINORS="1.34 1.35 1.36"
_AUDIT_K8S_LATEST_MINOR="1.36"
```

Update it when new minors release or old ones reach End-of-Life — see the
[Kubernetes releases page](https://kubernetes.io/releases/). A version newer than
`_AUDIT_K8S_LATEST_MINOR` is reported as a low warn ("the support list may be
stale"), so a bump reminds you to refresh the list.

## Addon overview (`lo addons --detail`)

A companion read-only view inventories the addons a cluster actually deploys —
resolved from `spec.bootstrap` (through the same parser the apply path uses, so
map-form entries stay intact) intersected with the `.lok8s/addons/` tree — with
each addon's category (from its `lok8s.dev/category` label) and a one-line "how
to configure" pointer:

```bash
lo addons --detail --domain my-cluster.example.com
```

```
Addons deployed by my-cluster.example.com (kind=kubeone)

NAME          CATEGORY        TYPE   VERSION   CONFIGURE
----          --------        ----   -------   ---------
cilium        networking      khelm  1.20.1    encryption + policy mode (policyAuditMode) in cilium inline values / values.<driver>.yaml
ccm           networking      khelm  1.35.0    spec.bootstrap ccm.values.env: ROBOT_ENABLED / HCLOUD_NETWORK (hcloud CCM)
cert-manager  infrastructure  khelm  v1.21.1   issue TLS via ClusterIssuer/Certificate CRs in a networking target
networking    target          target -         per-cluster glue in clusters/my-cluster.example.com/targets/networking
```

`./targets/*` (and absolute-path) entries are listed as **targets** (per-cluster
glue), not framework addons. Every shipped addon carries a config-help entry (a
parity test fails CI if one is added without one). Plain `lo addons` still lists
what the **tree** ships; `--detail` shows what **this cluster** runs.

## What it does not do

`lo audit` is the **static** half of the security picture — it reasons about the
spec and the rendered manifests. It cannot see runtime-only facts (the actual
apiserver flags, the live `cilium-config`, real cert expiry, exposed Services on
the cluster). Those come from the connected-cluster path and are surfaced
separately; the static audit is the part that ships today, offline, for every
self-hosted user.

## See also

- [Security](/guide/security) — how at-rest encryption and the other
  control-plane settings are configured.
- [Bootstrap Addons](/guide/addons) — the addon system `lo addons --detail`
  inventories.
- [Networking & Ingress](/guide/networking) — the Gateway / HTTPRoute /
  SecurityPolicy surface the exposure check reads.
