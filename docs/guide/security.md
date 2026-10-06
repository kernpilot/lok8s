# Security

## Encryption at rest (etcd)

Kubernetes Secrets live in etcd. By default they're **base64, not
encrypted**: anyone with the etcd data (a disk image, a backup, a node
breach) can read them. lok8s turns on at-rest encryption so the apiserver
encrypts Secrets before writing them to etcd.

### Where it's configured — the driver, not a cluster resource

This is a control-plane setting: you must tell the apiserver to encrypt
*before* it writes anything, via the `--encryption-provider-config` flag and
an `EncryptionConfiguration` file **on the control-plane hosts**. You can't
`kubectl apply` your way to it, so it lives in the **KubeOne driver**, not in
a bootstrap target:

```yaml
# .lok8s/drivers/kubeone/cluster/core/kubeone.yaml
spec:
  features:
    encryptionProviders:
      enable: true
```

From that one flag, KubeOne (at `lo provision`):

- generates and stores the encryption key (provider: `aescbc`),
- writes the `EncryptionConfiguration` to each CP host and sets the apiserver
  flag,
- **re-encrypts existing Secrets on every `apply`**.

::: tip Rule of thumb
apiserver / control-plane process settings → **driver** (KubeOne).
Anything an in-cluster controller reconciles → **cluster resources**.
:::

(For KKP *user* clusters (hosted control planes), you set at-rest encryption
per-cluster through KKP, which owns those apiservers. The above is for the
seed cluster that KubeOne builds.)

### Verify it

Write a canary Secret, then read the raw key straight from etcd:

```bash
kubectl -n default create secret generic enc-test --from-literal=canary=PLAINTEXT
# on a control-plane host, exec into the etcd container:
etcdctl get /registry/secrets/default/enc-test | grep -aoE 'k8s:enc:[a-z0-9:_-]+|PLAINTEXT'
```

- `k8s:enc:aescbc:v1:...` → encrypted at rest ✅
- the canary string appearing → **not** encrypted ❌

`aescbc` is KubeOne's default; `aes-gcm` is marginally stronger if you choose
to rotate up later.

## Apiserver audit log

The apiserver can write an audit log: one line for each request, with the
user, the verb, the object and the time. Use it to record who reads Secrets
or opens a shell in a pod, for example operator access to tenant
namespaces. It is a control-plane setting, like encryption at rest, so the
KubeOne driver sets it. It is off by default. You turn it on per cluster in
the cluster spec:

```yaml
# clusters/<domain>/cluster.lok8s.yaml
spec:
  auditLog:
    policy: audit-policy.yaml   # required: the policy file, relative to this file
    maxAge: 30                  # optional: days to keep old log files
    maxBackup: 10               # optional: number of old log files to keep
    maxSize: 100                # optional: size in MB before the log file rotates
```

| Field | Required | KubeOne field | Default |
|-------|----------|---------------|---------|
| `spec.auditLog.policy` | yes | `policyFilePath` | — |
| `spec.auditLog.maxAge` | no | `logMaxAge` | `30` (KubeOne) |
| `spec.auditLog.maxBackup` | no | `logMaxBackup` | `3` (KubeOne) |
| `spec.auditLog.maxSize` | no | `logMaxSize` | `100` (KubeOne) |

At `lo provision`, the driver adds KubeOne's `features.staticAuditLog` to the
generated `clusters/<domain>/.kubeone/kubeone.yaml`. It sets `enable: true`,
the **absolute** path of the policy file, and only the limits that you set.
KubeOne applies its defaults to the other limits. The other features of the
template (for example `encryptionProviders`) stay as they are. KubeOne then
copies the policy file to each control-plane host and sets the apiserver
audit flags. The apiserver writes the log to `/var/log/kubernetes/audit.log`
on each control-plane host.

The driver checks the spec and the policy file first, before the provider
creates or changes a server. It checks them again when it writes the
manifest.

The driver writes the manifest to a temporary file (mode 0600: the manifest
can hold registry credentials). It replaces `kubeone.yaml` only when every
step succeeded, so a refusal never leaves a half-written manifest. When a
signal stops the run, the driver removes the temporary file.

The driver stops with an error that names the file or the value and the fix
when:

- `spec.auditLog` is not a mapping, or it has a field that is not in the
  table above (for example the typo `maxBackups`),
- `policy` is not set, or it is not a text value,
- a limit is not a whole number from 1 to 999999999 (to use the KubeOne
  default, leave the field out),
- the policy file does not exist, or you cannot read it,
- the first YAML document of the policy file is empty (for example two `---`
  lines before the policy): the apiserver reads only the first document,
- the first YAML document does not have `apiVersion: audit.k8s.io/v1` and
  `kind: Policy`,
- the policy has no rules (the apiserver does not start with such a
  policy),
- the policy file is not plain YAML: it has a directive (a line that starts
  with `%`, for example `%YAML 1.2`), an anchor or an alias (`&name`,
  `*name`), a merge key (`<<`) or a duplicate key. Different YAML readers
  read these in different ways, so the driver refuses them. A comment after
  `---` is fine.

The driver does not check each rule in the policy. The apiserver reads the
rules when it starts. A cluster with `spec.kubehz.hosting: hosted` does not
use a KubeOne manifest, so the driver ignores `spec.auditLog` there.

::: warning Existing clusters
KubeOne changes a feature on running control planes only with
`kubeone apply --force-upgrade` (see the KubeOne docs). `lo provision` runs a
plain `kubeone apply`, and `lo` has no flag for the forced apply yet. When you
add `spec.auditLog` to a running cluster, or change the policy file, run
`lo provision` to render the manifest. Then run
`kubeone apply --manifest kubeone.yaml --force-upgrade` in
`clusters/<domain>/.kubeone/`, with the same credentials that `lo provision`
uses.
:::

### Example policy

::: details An EXAMPLE policy. Review it before you use it.
This example records metadata (who, what, when; never the object content)
for Secrets, ConfigMaps, service account tokens, `exec`, `attach` and
`port-forward` into pods, and for all requests of admin users. It records
nothing else.

```yaml
# audit-policy.yaml: an EXAMPLE only. Change it for your cluster.
apiVersion: audit.k8s.io/v1
kind: Policy
# ResponseComplete has the same data as RequestReceived: skip the duplicate.
omitStages:
  - RequestReceived
rules:
  # Secrets, ConfigMaps and service account tokens. Metadata only, so the
  # log never holds a secret value.
  - level: Metadata
    resources:
      - group: ""
        resources: ["secrets", "configmaps", "serviceaccounts/token"]
  # A shell or a tunnel into a pod.
  - level: Metadata
    resources:
      - group: ""
        resources: ["pods/exec", "pods/attach", "pods/portforward"]
  # Every request of an admin user. system:masters is the group of the admin
  # kubeconfig. Replace oidc:admins with your OIDC admin group (with the
  # spec.oidc groupsPrefix).
  - level: Metadata
    userGroups: ["system:masters", "oidc:admins"]
  # Nothing else.
  - level: None
```

The apiserver uses the first rule that matches a request. Controllers read
Secrets and ConfigMaps all the time, so the first rule can write many lines.
To skip a noisy controller, add a `level: None` rule with its `users` before
the first rule.
:::

### Verify the audit log

On a control-plane host, follow the log. Then list Secrets from your machine:

```bash
# on a control-plane host
sudo tail -f /var/log/kubernetes/audit.log
# on your machine
kubectl -n default get secrets
```

A line with `"resource":"secrets"`, your user name and `"verb":"list"` shows
that the audit log works.

## Host firewall

The other major hardening layer (a default-deny host firewall) is the
opposite: it's **cluster resources**, Cilium `CiliumClusterwideNetworkPolicy`
objects that select the host endpoint. Always roll it out in **audit mode**
first (`policyAuditMode: true`), confirm with `hubble observe --verdict AUDIT`
that nothing critical (etcd 2379/2380, apiserver 6443, kubelet 10250, vxlan
8472) gets denied, *then* flip to enforce. Going straight to enforce
without a complete allow set will deadlock the cluster.

## Credentials and command lines

Every local user can read the command line of a process (`ps`,
`/proc/<pid>/cmdline`), and audit tools such as auditd store it. Thus `lo`
gives no credential to a child process as an argument:

- curl gets a bearer token, a user and password, or a body with a secret in
  a config on stdin (`curl -K -`), or as `-K <(…)` when stdin carries the
  body.
- jq reads a secret from stdin or from a pipe (`--rawfile`), not from
  `--arg`.
- kubectl gets the values of a Secret from a file or from an env file on
  stdin (`--from-env-file=/dev/stdin`), not from `--from-literal`.

This includes the Hetzner Cloud token, the Hetzner Robot password, the kubehz
token, the KKP token, the AWS keys and the kubehz agent token in the agent
CronJob. Both implementations follow the rule. `tests/unit/credentials_argv_test.bats`
and `internal/execx/argv_credentials_test.go` check it with sentinel values.

curl still reads `~/.curlrc` (for example a proxy setting) when `lo` starts
it without `-q`. That is so for most calls of the bash implementation, and
for the KKP calls of both implementations. The agent tools
(`lo kubehz space …`, `lo kubehz cluster …`) and `lo kubehz token` start
curl with `-q`. A `config` or a `trace` line in `~/.curlrc` can read or
record what `lo` sends, the credentials included. Thus keep `~/.curlrc`
private, and examine it on a shared machine.

One exception stays: `lo kubehz node join` runs the `kubeadm join` line
of the platform, and `kubeadm join` takes its bootstrap token as `--token`.
The token is valid for a short time only.
