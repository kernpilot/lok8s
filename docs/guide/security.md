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

One exception stays: `lo kubehz node join` runs the `kubeadm join` line
of the platform, and `kubeadm join` takes its bootstrap token as `--token`.
The token is valid for a short time only.
