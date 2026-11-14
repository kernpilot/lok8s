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

### List the keys of the Secrets (`--keys-only`)

`lo build --keys-only` prints the render of a domain to stdout. It prints no
Secret value: each Secret in the final render shows its key names with
empty values. This applies to every Secret, not only to the Secrets of the
`secrets.lok8s.dev` generator: a plain Secret manifest, a kustomize
`secretGenerator`, a Helm chart's Secret, a value that a patch adds, and a
Secret in a `List`. In each Secret, lo also empties the values of the
labels and annotations (the keys stay) and removes the comments. The build
does not read, decrypt or mint a secrets
store, so it needs no store and no age key. Use it to compare the keys that
a generator makes with the keys that a consumer expects, for example in CI.

```bash
lo build --keys-only --domain example.com > keys.yaml
```

Each Secret in the output gets the annotation
`secrets.lok8s.dev/keys-only: "true"`, so `lo build` and `lo deploy` refuse
the output (see below). `--keys-only` empties Secrets only. Every other kind
prints as it is rendered: a value in a `ConfigMap`, for example, prints. lo
empties the values with `yq` after the kustomize build and
the variable substitution, so the build needs the pinned `yq`. `yq` writes
the stream: its layout (for example, the indent of lists) can be different
from `artifacts.yaml`.

The annotation `kubectl.kubernetes.io/last-applied-configuration`, which
`kubectl apply` writes, can hold the values of a Secret. `lo build
--keys-only` refuses a Secret that has this annotation: it prints an error
that names each such Secret (`Secret <namespace>/<name>`), prints nothing to
stdout and exits with an error. Remove the annotation from the source
manifest.

`--keys-only` prints no value, but it prints the hashes of values that the
render already holds. For example, a kustomize `secretGenerator` Secret has
a hash of its values in its name (`<name>-<hash>`), and a Helm chart can
put a checksum of a Secret in an annotation of a pod template.

`--keys-only` writes no file. It does not change `artifacts.yaml` or the split
directory. You cannot use it with `--split` or `--single`.

`--keys-only` does not print the stderr of the render (kustomize and its
plugins) and of `yq`, because an error can quote an input value. When the
render fails, `lo` prints one line that says so and exits with an error. To
see the cause, run the build without `--keys-only`.

Before the render, `lo` checks the installed Secret plugin: the plugin must
have the keys-only run (`Secret --capabilities` prints the line
`keys-only`). An older plugin does not know the keys-only setting, so `lo`
stops with an error and renders nothing. Run `lo toolchain install` to
install the plugin of your `lo`. `lo-full` renders with its own plugin and
needs no check.

The plugin home (`KUSTOMIZE_PLUGIN_HOME`, else `<project>/.kustomize`) must
exist. kustomize uses the first plugin home that exists, and without the
project's home it would load plugins from directories under
`XDG_CONFIG_HOME` or `HOME` that `lo` does not check. `lo` gives kustomize
the home that it checked. The Secret plugin
(`secrets.lok8s.dev/v1/secret/Secret`) must be in the home too, also when the
domain has no `secrets.lok8s.dev` Secret: without it, a kustomize that has Go
plugin support would load a Go plugin (`Secret.so`) that `lo` cannot check.

The check is a compatibility check. It is not isolation and not
attestation: a plugin that prints `keys-only` but does not have the run, or
a plugin file that is replaced after the check, gets through. Install the
plugin of your `lo` from a source that you trust.

`lo build` never writes an object with the `secrets.lok8s.dev/keys-only`
annotation into `artifacts.yaml` or the split directory. These commands do
not apply a manifest that holds one:

- `lo deploy` and `lo hooks` (the hooks of the Tilt extension) refuse an
  `artifacts.yaml` with the annotation, and one that they cannot read. Each
  reads the artifact once and uses the copy that it checked: `lo deploy`
  applies that copy (also the subset of `-l key=value`), and `lo hooks`
  selects its objects from it. To select the CRDs and the subset of `-l`,
  `lo deploy` splits the copy at its `---` lines. It stops with an error
  on a `---` line with more text on it (`--- # note`), which kustomize
  does not write. The bash implementation reads such a line as a
  separator.
- The bootstrap (`lo up`, `lo provision`, `lo bootstrap`) refuses an entry
  whose render holds the annotation, for example keys-only output saved as
  an addon resource. The dry run applies nothing and has no such check.
- The bootstrap checks each `restore.d` entry before it applies it: the
  working copy in the store (`secrets/restore.d/<name>.yaml`), which it
  reads once and applies from that copy, and else the decrypted
  `<name>.sops.yaml`. An entry with the annotation is skipped with an
  error. When the working copy holds the annotation, lo does not use the
  `.sops.yaml` file of that entry: remove or replace the working copy. A
  working copy that cannot be read, or that does not parse as YAML, gets
  a warning, and lo uses the `.sops.yaml` file, which it checks too.
- The KubeOne driver checks each file of the addons directory of the
  manifest (`addons.path`, for Hetzner the cilium and CCM renders in
  `<work dir>/addons/`) before it runs `kubeone apply`. A file with the
  annotation, one that does not parse, one that is not a regular file (a
  FIFO, a device), or a directory that cannot be read stops the apply.
  KubeOne reads its manifest when it starts and the addons later, at its
  addons step, and the work dir is shared. So lo reads `kubeone.yaml`
  once, and KubeOne reads a copy of those bytes
  (`<work dir>/.kubeone-run-<random>.yaml`, mode 0600), also when the
  manifest has no `addons.path`. With an addons path, lo writes the bytes
  that it checked into a new private run directory
  (`<work dir>/.kubeone-run-<random>/`, mode 0700, files 0600), and the
  copy of the manifest names it in `addons.path`, with the aliases and
  merge keys (`<<`) of the manifest resolved. `./addons`, `./addons/`
  and `addons` name the same directory. lo reads a `..` in `addons.path`
  as a step back in the text of the path, not on the disk: `a/../addons`
  is `addons`, also when `a` is a symbolic link. A `..` or an absolute
  path can name a directory outside the work dir: lo copies and checks
  it in the same way. A `kubeone.yaml` with more than one YAML document
  (a `---` line after the spec starts a second one), and an `addons.path`
  that is not a string (`false`, a number, a mapping, a list), stop the
  apply. lo removes the run directory and the copy of the manifest when
  `kubeone apply` ends, and when lo stops on a signal that it can catch
  (SIGINT, SIGTERM, SIGHUP; under `nohup`, lo ignores a SIGHUP). On a
  SIGHUP, the Go `lo` stops `kubeone`, removes the copy and its other
  temporary files (the provider config of `lo provision`), and then ends
  with the SIGHUP. A SIGKILL can leave the copies in the work dir. A
  second signal while lo removes them can also leave them. The copies
  hold the CCM render, with its Hetzner token, in plaintext. After such a
  stop, remove `.kubeone-run-*` by hand. When a write of the copy fails
  (a full disk), lo removes the part that it wrote.

  The copy of the manifest is beside `kubeone.yaml`, so the other paths
  of the manifest (the CA file, the audit and node-selector files) are
  the same, and KubeOne reads these from the work dir, not from a copy.
  So a writer who can create or rename files in the work dir can also
  replace these files: keep the work dir writable only by you. lo does
  not write over a file that is at the path of the copy already, and it
  stops the apply. The two trees differ when a file appears at that path
  after lo checked it: the Go `lo` keeps that file, and the bash
  implementation removes it when the apply ends.
- The Tilt extension runs `lo tilt preflight` on its manifest (the domain
  artifact and the service overlays on it) before it applies anything, on
  every load. A manifest with the annotation, or one that cannot be read
  from stdin, stops the load. The `preflight` setting and
  `LOK8S_PREFLIGHT=0` turn off only the cleanup of stuck objects, not this
  check.

These commands pass on the bytes that they checked, with no change: the
final newlines of a last `|+` block scalar stay. The bash implementation
keeps these bytes out of its trace: with `bash -x` or `BASH_XTRACEFD`, the
functions that hold a render, an artifact or a decrypted manifest turn
tracing off until they return. It sends them to `kubectl` and `yq`
through pipes, not here-strings, which bash can write to a file in
`TMPDIR`.

Each check parses the YAML, so a quoted key, a flow-style map and a `List`
item count. The annotation counts at any depth of an object, so a custom
resource that holds a Secret template with the annotation is refused too.
So is a resource that names the annotation as a key under a nested
`metadata.annotations`, for example an admission policy whose pattern
forbids it; a rule that names the key in a CEL expression is not a match.
A render or an artifact that does not parse as YAML also stops with an
error. The existing artifacts do not change. The bash implementation
checks with `yq`, so it also stops when `yq` is not installed or is older
than v4.47.1.

The flag is the only way to start a keys-only render. lo controls the
plugin setting that it uses (see the
[env contract](../reference/kustomize-plugins.md#env-contract)). Do not
export that variable. An older `lo` does not have the checks above. A
repository can refuse the annotation in its committed artifacts, for
example with a CI check.

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
lo provision --domain example.com
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
lo provision --domain my-cluster.example.com --bootstrap
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
live-patched config). Preview the run before you start it:

```bash
lo provision --domain my-cluster.example.com --bootstrap --dry-run --render-dir /tmp/bootstrap
```

The dry run renders every entry in place, exactly as the real run does: the
same project, the same value stack, envsubst, khelm and order, and the same
`HOME`, credentials and CA. Thus sops, exec auth and kubectl behave as in a
real run. It applies nothing. It renders each entry two times, then asks the
API server for a server-side diff (`kubectl diff --server-side
--force-conflicts`, read-only) and prints one line for each entry:

```
bootstrap dry-run: restore.d: differs changed=1 new=0
bootstrap dry-run: cilium: differs changed=2 new=0 (non-deterministic: 2)
bootstrap dry-run: cilium: non-deterministic (regenerated on every apply): v1 Secret kube-system/cilium-ca
bootstrap dry-run: cilium: non-deterministic (regenerated on every apply): v1 Secret kube-system/hubble-server-certs
bootstrap dry-run: cert-manager: differs changed=2 new=1
bootstrap dry-run: rook-ceph: skipped
bootstrap dry-run: entries=4 unchanged=0 differs=3 rendered=0 skipped=1 inconclusive=0 errors=0 nondeterministic=2
```

Each line names the entry by its entry name, which `--only` accepts. The
lines come in the order of the run; only the `--render-dir` files carry the
`NN-` order prefix.

- `restore.d` is diffed as its own entry, because a real run applies it
  before all other entries. The files decrypt with the normal sops key
  discovery. A file that does not decrypt is an error: the run exits 2.
- `inconclusive (needs-crd)`: the server-side diff failed because the CRD
  or the webhook of an earlier entry is not in the cluster yet (expected on
  a new cluster). The run cannot say what this entry changes, so the run
  exits 2.
- `inconclusive (chart dependencies not vendored)`: the entry renders a
  local chart, and khelm can build the dependencies of that chart into the
  chart directory (it writes `Chart.lock` and `charts/`). The dry run does
  not render the chart. It decides as Helm does: on the chart as Helm's
  loader loads it (`.helmignore` applies, and a dependency matches a
  subchart by the name in its `Chart.yaml`), and with the status column of
  `helm dependency list`, which must be `ok` or `unpacked` for each
  dependency. These are not vendored: a missing, misnamed or hidden
  dependency, two archives of one dependency, a version outside the
  dependency's constraint, a dependency without a version, a chart that does
  not load, and a `file://` dependency. A real run renders some of these
  charts without a build (for example a misnamed archive next to the
  correct subchart directory), but the dry run cannot tell them apart and
  does not render them. Put the dependencies into the `charts/` directory of
  the chart, or pass `--allow-generators`. The run exits 2. The bash
  implementation asks `helm` for this check (`helm dependency list` and
  `helm lint`, both read-only), so it needs helm v3 (3.3 or a later 3.x) on
  `PATH`. Without it, such an entry is inconclusive too. stderr gives the
  cause and the next action for each refused chart: the vendor advice only
  for a dependency that is not vendored, other text for a `file://`
  dependency, a chart that helm cannot load, or a missing helm v3.
- `(non-deterministic: N)`, with one `non-deterministic (regenerated on
  every apply)` line for each object: the two renders of the entry gave
  different content for these objects, or only one of the renders has the
  object. Example: the Cilium chart makes a new
  CA and new TLS Secrets for Hubble on each render, thus each real apply
  changes them. The result includes these objects: the entry differs, and
  the run exits 1. `--ignore-nondeterministic` diffs the entry without them.
  The run still lists them, and `--render-dir` keeps them.
- `(non-repeatable)`: the render generated secret material that the store
  does not hold yet (a password, a key, a certificate, or a CA because
  `CAROOT` has none), or it updated a value of the store. The dry run keeps
  the value in a store of this run only, which all renders of the run share.
  An entry that reads such a value (a `secretRef`, the same CA) is
  non-repeatable too. The next real run generates different bytes, so the
  real result differs from this preview.
- `(partial: KRM function skipped)`: the entry declares a KRM function, in
  its own directory or in a kustomization it references in the project (a
  base, a component, a generator or transformer file, or an inline config in
  the kustomization file). A dry run without `--allow-generators` does not
  run it, thus the objects of the function are not in the preview.
- `(partial: a reference outside the project was not inspected)`: the
  entry references a kustomization or a file outside the project, or a
  remote one, and the dry run found no KRM function in what it read. The
  dry run does not read such a reference, thus it cannot rule out a
  function there. A reference resolves as kustomize resolves it: `..` goes
  up from the path as written, before symbolic links resolve.

**Trust model.** The dry run prevents accidental side effects of
legitimate addon code and configuration. It is not a security boundary
against an addon that is made to do harm: the real bootstrap runs the same
addon with full credentials. A malicious addon can still execute code during
a dry run.

lo and its built-in generators never apply anything and never persist new
state in a dry run: the secrets plugin reads the store and keeps new or
updated values in a store of this run only, the cert generator never writes
`CAROOT`, the Lo driver never writes `.registries.json`, and a framework
addon is never ejected. Executable addon code is refused unless you pass
`--allow-generators`:

| Addon code | In a dry run |
|---|---|
| a legacy exec or Go plugin that is not lok8s' Secret or ChartRenderer | does not resolve: the plugin home of a dry run holds only those two |
| a KRM exec function (`config.kubernetes.io/function` with `exec:`) | never runs: kustomize runs without `--enable-exec` and skips it, and the entry is `partial` |
| a KRM container function | fails: `docker` refuses |
| a remote base (a git URL in `resources:`) | fails: `git` refuses |
| `helmCharts` (the helm chart inflator of kustomize) | a chart that is not in `chartHome` fails: `helmCharts needs --allow-generators in a dry run` (`helm pull` refuses). A chart that is already in `chartHome` renders. |
| a local chart whose dependencies are not vendored | does not render: the entry is `inconclusive` |
| a helm plugin | does not resolve: `HELM_PLUGINS` is an empty directory |
| a `bash:` secrets generator | fails: the secrets plugin refuses it |

`--allow-generators` runs this code as trusted, with the side effects of a
real run.

What the dry run does not prevent:

- Helm caches may be populated: khelm downloads a remote chart into the helm
  cache of your `HOME`, as a real run does.
- Adversarial input: a generator kind (apiVersion and kind) that resolves to
  a path outside the plugin home, and a chart or value URL with a crafted
  scheme. Code that runs this way has the access of a real run.

An addon that stacks values renders from a copy (a dry run and a real run
alike). The copy keeps symlinks, and lo writes the files it changes in the
copy (`chart.yaml`, `values.inline.yaml`, `values.merged.yaml`) as new
private files. It never writes through a symlink to a file of the addon or
to a file the addon shares.

The render writes its temporary files into a private directory of the run
(`TMPDIR`), which lo removes at the end of the run, also on SIGTERM. A Secret
plugin that is older than the dry run would write the store: the dry run
refuses it (`lo toolchain install` installs the plugin of your `lo`).

An entry's `env:` goes only to the render (kustomize and envsubst), never to
lo itself. It cannot set lo's own variables (`LOK8S_BOOTSTRAP_*`,
`LOK8S_INTERNAL_*`, `BOOTSTRAP_*`, `bootstrap_*`, `PATH_SECRETS`,
`KUBECONFIG`, `KUSTOMIZE_PLUGIN_HOME`, `HELM_PLUGINS`). Such an entry is an error, on a dry
run and on a real run, and lo names the variable. lo does this check after
the skips of the real run: an entry that the run skips is `skipped`, not an
error.

`--render-dir <dir>` keeps the rendered manifests (`NN-<entry>.yaml`) and
the diff text (`NN-<entry>.diff`, empty when unchanged). The directory holds
rendered Secrets in plaintext; the run says so on stderr. lo prepares the
directory after the checks of the run (a failed check leaves no directory).
lo creates a new directory with mode 0700 and marks it with a `.lo-dry-run`
file. lo changes an existing directory that the group or others can read to
mode 0700, and says so on stderr. A later run clears only the files it owns
(`NN-*.yaml`/`.diff`, `restore.d.yaml`/`.diff`). lo refuses a non-empty
directory without the marker (as a regular file), a path with a symlink in
it, and a symlink inside the directory.

`--no-diff` renders only and never contacts the cluster.

**Exit codes**, the same for `lo bootstrap` and `lo provision --bootstrap`:
0 when nothing would change, 1 only when a completed diff found a change
(`differs`), and 2 for all other failures: a render or diff error, an
`inconclusive` entry, a restore.d file that does not decrypt, an unreachable
hosted cluster, a missing spec or kubeconfig, an unknown `--only` name, a bad
flag (also `--only=` without a name and `--with-restore` without `--only`),
a bad `--render-dir`, and an error before the command starts (for example a
missing bash tree). A dry run is not gated and ignores `--force`: it never
recreates an object. The provision tail (kubehz registration, the inventory,
the gitops bootstrap) is skipped too.

#### One entry: `--only`

```bash
lo provision --domain my-cluster.example.com --bootstrap --only cert-manager
lo provision --domain my-cluster.example.com --bootstrap --only cert-manager,envoy-gateway --dry-run
lo provision --domain my-cluster.example.com --bootstrap --only cert-manager --with-restore
```

`--only` applies (or dry-runs) the named `spec.bootstrap` entries. A value
selects every entry that it matches by the entry name (the `name:` override
when set, else the last path element), by the key as written in
`spec.bootstrap` (`./targets/flux` for `- ./targets/flux: {dependsOn:
[fluxcd]}`), or by the directory name of the addon or the target. Thus
`--only rook-ceph` selects the `rook-ceph` addon and also
`./targets/rook-ceph`, also when its `name:` is `rook-ceph-cluster`. The key
selects one of them: `--only ./targets/rook-ceph`.
`dependsOn` is not expanded: a dependency you do not name is not applied,
and only the edges among the named entries order the run. When an omitted
entry is a prerequisite of a named one, lo prints a warning before it
applies anything. An unknown or blank name is an error that lists the valid
names and changes nothing: lo validates the selection before anything
writes, so a typo never applies part of the graph (or `restore.d`).

`--only` leaves `restore.d` out, because it names addons. A real run then
prints a warning that names each skipped file: an addon that would reuse a
restored wildcard TLS Secret (cert-manager, for example) issues a new one
when it is missing, and repeated issuance can hit the Let's Encrypt rate
limit. A dry run prints that `restore.d` was skipped. With
`--with-restore`, a real run applies `restore.d` first and a dry run diffs
it, as if `--only` were absent. The same flags exist on `lo bootstrap`.

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
