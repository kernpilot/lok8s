# Deploying to Clusters

lok8s uses a two-step, **domain-based** pipeline: **build** the domain's
kustomization into ONE artifact, then **deploy** that artifact to a cluster.

> **Breaking change (domain-based build/deploy).** This replaced the old
> per-target model. `lo build`/`lo deploy` no longer loop over
> `targets/*/` on their own and no longer write
> `clusters/<domain>/artifacts/<target>/artifacts.yaml`. Instead **you** compose
> the domain's targets in `clusters/<domain>/kustomization.yaml`, and the
> framework renders that into a single `clusters/<domain>/artifacts.yaml`. There
> is no per-target `lo build <target>`. Domains with
> `spec.build.artifacts: split` (or a `spec.gitops.provider`) additionally
> emit committable
> per-resource files under `clusters/<domain>/artifacts/` (Secrets
> sops-encrypted) for GitOps consumers (see the
> [spec reference](../reference/specs.md)); `lo deploy` always applies the
> single `artifacts.yaml`.

Select the active domain with `lo use <domain>` (or per-command with
`--domain <domain>`); the examples below assume it is set.

## Compose the domain

Author `clusters/<domain>/kustomization.yaml`, a normal kustomization whose
`resources:` list the targets you want in this domain. Targets may be **local**
(under the domain's own `targets/`) or **shared** (a repo-level tree such as
`.targets/`, referenced with a relative path):

```yaml
# clusters/example.com/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./targets/networking      # local to this domain
  - ./targets/monitoring      # local to this domain
  - ../../.targets/cert-manager  # shared across domains
```

Each entry is a directory with its own `kustomization.yaml` (a kustomize base),
so ordering and composition are entirely under your control. Cluster-infrastructure
ordering still lives in `spec.bootstrap` and runs during `lo up`/`lo provision`
before workloads. See the [Addons guide](./addons.md).

## Build

```bash
lo build            # active domain
lo build --domain example.com
```

Runs `kustomize build --enable-alpha-plugins` over
`clusters/<domain>/kustomization.yaml` (with the secrets/khelm plugins, per-domain
secret isolation, and the `LOK8S_*` envsubst pass) and writes the result to a
single file:

```
clusters/<domain>/artifacts.yaml
```

If the domain has no `kustomization.yaml`, the build fails with:

```
domain example.com has no kustomization.yaml — compose its targets there,
e.g. resources: [./targets/foo, ../../.targets/bar]
```

### Split output (GitOps) {#split-output}

A domain with `spec.build.artifacts: split` (or a `spec.gitops.provider`) also
emits committable **per-resource** files under `clusters/<domain>/artifacts/`:
`<Kind>.<namespace>.<name>.yaml`, and Secrets as sops-encrypted
`Secret.<ns>.<name>.sops.yaml` (data/stringData encrypted to the
`spec.gitops.age` recipients). This is the layout a reconciler (Flux
kustomize-controller with native sops decryption) watches; `lo deploy` still
applies the single `artifacts.yaml`. See the
[spec reference](../reference/specs.md) for the full field list.

**Encryption is decoupled from the split** via `spec.build.encrypt`:

```yaml
spec:
  build:
    artifacts: split
    encrypt:
      type: sops        # default (only supported value); anything else = hard error
      on: change        # default; or "always"
```

- `encrypt.on: change` (default) **skips re-encrypting unchanged Secrets.** sops
  mints a fresh data key on every encrypt, so re-encrypting an identical Secret
  churns the whole ciphertext each build. In `change` mode split decrypts the
  committed twin (with the ambient age key `lo build` already uses to read the
  store) and, if it **canonically** matches the fresh render (key order /
  formatting don't count), keeps the existing file byte-for-byte. If the prior
  can't be decrypted (no key, corrupt, missing), it re-encrypts (fail safe: it
  can't prove "unchanged"). A Secret that vanished from the render still gets
  pruned (present-in-render decides pruning, not whether it was re-encrypted).
- `encrypt.on: always` re-encrypts every Secret every build (the original
  behavior; no compare).

> Chart-minted random secrets still need pinning (chart `existingSecret` hooks +
> the secrets plugin). `on: change` only suppresses churn when the **rendered**
> value is stable: a value that changes every render is a real change and gets
> re-encrypted. Verify with a double-build diff before pointing a reconciler at
> the output.

### CI render without secrets (`--no-secrets`)

`lo build --no-secrets` splits **only** non-Secret resources. It never renders,
re-encrypts, prunes, or even reads a committed `Secret.*.sops.yaml`, and it does
**not** invoke the secret generators, so it needs no secrets store and no age
key. This is the CI render path: regenerate the non-secret artifacts (e.g. after
an image-automation pin bump) on a runner that has neither the store nor the key,
without touching (or accidentally deleting) the committed encrypted Secrets.

```bash
lo build --no-secrets --domain example.com   # CI: non-secret artifacts only
```

**Store-free render.** `--no-secrets` exports `LOK8S_SECRETS_DISABLE=1` to the
underlying `kustomize build`, which makes the `secrets.lok8s.dev` exec generator
emit nothing and never read `$PATH_SECRETS`. This, not the split shaping alone,
is what makes the render truly store-free: without it the `kustomize build`
step would still invoke the generator and mint/read the store, even though the
split later leaves the committed encrypted Secrets inert. See
[`LOK8S_SECRETS_DISABLE`](../reference/kustomize-plugins.md#env-contract).

The swap's prune is guarded to leave `Secret.*.sops.yaml` alone (without the
guard, a `--no-secrets` build's empty secret stage would sweep every committed
Secret, and a pruning reconciler would then delete them from the cluster). A
non-Secret resource dropped from the render is still pruned as usual.
`--no-secrets` cannot be combined with `--single` (it shapes the split emit).

## Deploy

```bash
lo deploy            # active domain
lo deploy --domain example.com
```

Applies the single `clusters/<domain>/artifacts.yaml` to the cluster in two phases:

1. **CRDs first**: the deploy extracts `CustomResourceDefinition` resources and
   applies them server-side, then waits for them to become Established.
2. **Apply the rest + wait**: the deploy applies the whole artifact server-side
   (re-applying the CRDs is a no-op), then waits for the artifact's own
   Deployments/DaemonSets/StatefulSets to become ready (best-effort, 120s).

Workload-plane ordering is intentionally not a framework primitive: kubectl
handles in-manifest order; Tilt handles runtime deps via `resource_deps`;
cluster-infra ordering lives in `spec.bootstrap`.

### Selective deploy (`-l` / `--label`)

Deploy only the resources carrying a given label with `-l key=value` (optional;
omit it to apply everything):

```bash
# Deploy only the objects labelled lok8s.dev/name=zitadel
lo deploy -l lok8s.dev/name=zitadel

# The key may be any label key, not just lok8s.dev/*
lo deploy -l app=frontend
```

Selective deploy is **opt-in**: it matches on labels you put on your resources, so
it only works if those resources actually carry the label. The idiomatic way is a
kustomize `labels:` block on the target you want to address:

```yaml
# clusters/example.com/targets/zitadel/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
labels:
  - pairs:
      lok8s.dev/name: zitadel
    includeSelectors: false
resources:
  - deployment.yaml
  - configmap.yaml
```

With that, `lo deploy -l lok8s.dev/name=zitadel` applies exactly that target's
objects out of the composed artifact. lok8s does **not** require or lint these
labels. Add them only for the subsets you want to address individually.

## Deployment Domains

Deployment domains let you deploy content to another domain's cluster. They have a `deploy.lok8s.yaml` with a `clusterRef`:

```yaml
# clusters/api.example.com/deploy.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: api
spec:
  clusterRef:
    domain: example.com    # deploys to this cluster
```

A deploy domain composes its own targets in its own
`clusters/api.example.com/kustomization.yaml`, exactly like a cluster domain.
Build and deploy work the same way. They just apply to the referenced cluster's
kubeconfig:

```bash
lo build --domain api.example.com
lo deploy --domain api.example.com
# selective deploy works here too:
lo deploy --domain api.example.com -l lok8s.dev/name=api
```

## Full Lifecycle: Provision

The `lo provision` command runs the full lifecycle for a cluster domain:

```bash
lo provision example.com
```

1. Creates the cluster (via driver contract)
2. Applies `spec.bootstrap` addons via the framework bootstrap
3. Registers with kubehz (if `spec.kubehz.access` is set)

`lo deploy` (headless/CI) or Tilt (local dev) handles workload deployment separately: it is not part of `provision`.

### Re-applying bootstrap only (`-b`)

The last step of `lo provision` applies the `spec.bootstrap` addons (Cilium,
cert-manager, Rook, …) in `dependsOn` order with `wait:` health gates.
To **re-apply that bootstrap graph on an already-provisioned cluster** without the
infrastructure reconcile (no `hcloud`/`kubeone` calls, no Hetzner API check), pass
`-b` / `--bootstrap`:

```bash
lo provision my-cluster.example.com --bootstrap
```

It skips the provider reconcile and `driver::provision`, then runs the **same
bootstrap DAG** against the existing kubeconfig. Because the `dependsOn` / `wait:`
gates still fire, this is the gated way to **roll a bootstrap-addon upgrade**: bump
the addon's pinned version (the Rook operator chart, a `cephVersion`, …), then
`lo provision -b`. That's safer than `lo build <target>` + `kubectl apply`, which
applies the target directly and **bypasses** the dependency graph and its health
gates.

#### Preview first: `--dry-run`

A re-apply writes every entry with `kubectl apply --server-side
--force-conflicts`. On a production cluster that overwrites live drift in
unrelated addons (a `MachineDeployment` replica count an autoscaler owns, a
live-patched config). Preview the run before you take it:

```bash
lo provision my-cluster.example.com --bootstrap --dry-run --render-dir /tmp/bootstrap
```

The dry run renders every entry exactly as the real run would (the same
value stack, envsubst, khelm and ordering) and applies nothing. Rendering
happens inside a disposable copy of the WHOLE project, built once per run:
`HOME`, `XDG_CONFIG_HOME`/`XDG_DATA_HOME`/`XDG_CACHE_HOME`, `CAROOT` and
`HELM_CACHE_HOME`/`HELM_CONFIG_HOME`/`HELM_DATA_HOME` point into it, and
`PATH_SECRETS` always points at the copy's own secrets store — even when
the real one does not exist yet — so a generator's cache write (a fresh
password, a keypair) lands only in the copy. `KUBECONFIG` (and
`KUBERNETES_*`/any `*_TOKEN` var) is unset for the render: a plugin
executed during it has no cluster to reach. Only the diff step gets the
real cluster, through an explicit `--kubeconfig`, never through the
environment. For each entry the dry run asks the API server for a
server-side diff (`kubectl diff --server-side --force-conflicts`,
read-only) and prints one line:

```
bootstrap dry-run: 01-cilium: unchanged
bootstrap dry-run: 02-cert-manager: differs changed=2 new=1
bootstrap dry-run: 03-rook-ceph: skipped
bootstrap dry-run: restore.d: differs changed=1 new=0
bootstrap dry-run: entries=4 unchanged=1 differs=2 rendered=0 skipped=1 inconclusive=0 errors=0
```

`restore.d` is diffed as its own entry (it would be applied before
anything else on a real run, so a dry run reports what THAT would change
too) unless `--only` is given — see below. A result the race-class
server-side errors produce reads `inconclusive (needs-crd)`: an earlier
entry's CRD/webhook has not landed in this read-only, sequential probe —
expected on a from-scratch cluster, not a real diff, and not a real error
either. It is counted in its own `inconclusive=` field, separate from
`differs=`, and — like an error — keeps the run from exiting clean (see
the exit codes below). An entry whose render minted NEW secret material
(a fresh password, a keypair that did not exist yet) is marked
`(non-repeatable)`: the next real run mints different bytes, so this
entry's rendered manifest describes a result that run will not reproduce
verbatim.

`--render-dir <dir>` keeps the rendered manifests (`NN-<entry>.yaml`) and
the diff text (`NN-<entry>.diff`, empty when unchanged), written with
`umask 077`. The directory is marked as the dry run's own with a
`.lo-dry-run` file; on a later run, a directory that already carries that
marker is cleared of only the files it owns (`NN-*.yaml`/`.diff`,
`restore.d.yaml`/`.diff`) before the new set is written, so a stale entry
from an earlier, differently-numbered or differently-selected run never
survives alongside it. A non-empty directory that does NOT carry the
marker is refused outright — point `--render-dir` at an empty directory,
or one a prior dry run already owns, never at something else you keep
there. A symlink is never written through, inside the directory or as the
directory itself. `--no-diff` renders only and never contacts the
cluster. The exit code is 0 when nothing would change, 1 when a completed
diff found a change (`differs` only), and 2 for anything that kept the
assessment from finishing cleanly — a render/diff failure, an
`inconclusive` entry, an unreachable hosted cluster, a missing
kubeconfig, or a bad `--render-dir`. A dry run is not gated and ignores
`--force`: it never recreates an object. The provision tail (kubehz
registration, the inventory, the gitops bootstrap) is skipped too.

By default, a dry run refuses to execute anything that is not one of
lok8s' own two built-in exec plugins — the secrets generator and khelm's
chart renderer. A `bash:` secrets generator specifically (`error:
executable generator not run in dry-run`), and any OTHER exec plugin an
addon's kustomization tree references (`error: exec plugin <kind> not run
in dry-run`), both stop the whole render rather than run with no
kubeconfig and no live cluster to check. Pass `--allow-generators` to run
them anyway, when you specifically need their output diffed — the code
still only gets the isolated project copy, never your real kubeconfig.
Separately, an entry's `env:` override can never set one of the sandbox's
own variables (`PATH_SECRETS`, `HOME`, `CAROOT`, `KUBECONFIG`, …) — trying
to is reported as that entry's own error, naming the variable, rather
than silently doing nothing.

#### One entry: `--only`

```bash
lo provision my-cluster.example.com --bootstrap --only cert-manager
lo provision my-cluster.example.com --bootstrap --only cert-manager,envoy-gateway --dry-run
lo provision my-cluster.example.com --bootstrap --only cert-manager --with-restore
```

`--only` applies (or dry-runs) the named `spec.bootstrap` entries, matched
by the entry name (the `name:` override when set) or the addon name.
`dependsOn` is not expanded: a dependency you do not name is not applied,
and only the edges among the named entries still order the run — if an
omitted entry is a prerequisite of one you named, that is printed as a
warning before anything applies, not silently assumed. An unknown or
blank name is an error that lists the valid names and changes nothing;
the selection is validated before any writer runs, so a typo can never
apply part of the graph (or `restore.d`) before failing. The same flags
exist on `lo bootstrap`.

`--only` leaves `restore.d` out by default — it names addons, never
`restore.d` — and on a REAL run prints a loud warning naming every file
under it that was skipped: restoring a wildcard TLS Secret matters for
more than the one addon you named, since an addon that would otherwise
reuse it (cert-manager, say) ACME-reissues instead when it is missing,
and repeated reissuance can hit the Let's Encrypt rate limit. Pass
`--with-restore` to apply (or, under `--dry-run`, diff) `restore.d` first,
exactly as if `--only` were absent.

`--only` never applies `restore.d` — it prints that `restore.d` was
skipped and leaves the live Secrets it would otherwise restore alone.
Without `--only`, a plain `--dry-run` still reports what `restore.d`
would change (see above); a real, unrestricted run applies it first, as
always.

It requires an already-provisioned cluster (an existing
`.kubeconfig/<cluster-name>.yaml`); otherwise it errors and changes nothing.

## Provisioning from CI

You can run `lo provision` from GitHub Actions to spin up a committed cluster on
every push (or on demand). A reusable workflow ships with lok8s:

```yaml
# .github/workflows/spinup.yml in your repo
jobs:
  spinup:
    uses: kernpilot/lok8s/.github/workflows/spinup.yml@main
    with:
      domain: my-cluster.example.com
    secrets:
      HCLOUD_TOKEN: ${{ secrets.HCLOUD_TOKEN }}
```

It needs **only** `HCLOUD_TOKEN` (your Hetzner Cloud API token, used to create the
infrastructure). The `cluster.lok8s.yaml` for the domain must already be committed
under `clusters/<domain>/`.

### Claiming a registered cluster

When a cluster opts into kubehz dashboard visibility (`spec.kubehz.access` is set
to `registered` or `managed`), `lo provision` registers it as **pending** and
prints its SSH-key **MD5 fingerprint**. You then claim the cluster in the
dashboard with that fingerprint plus your own Hetzner Cloud token (used once as
ownership proof, never stored). CI needs no platform token.

See the [kubehz Platform guide](./kubehz.md#claiming) for the full flow:
registration, claiming, the heartbeat agent, and troubleshooting.

## Destroy

```bash
lo destroy example.com
```

Tears down the cluster via the driver contract's `driver::destroy` function.
