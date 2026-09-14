# CLI Reference

The `lo` CLI is a single static Go binary. Every command below runs natively in it; the [argsh](https://github.com/arg-sh/argsh) implementation it was ported from stays in every project at `.lok8s/lo` as a frozen reference and runs the same command line when the project file routes a command to it (see [Choosing the implementation](#choosing-the-implementation), and [The Go `lo` binary](go-migration.md) for what still calls into that tree, and the catalogue of the few places the two deliberately differ: for example, argument-parse errors exit `1` in the binary where argsh exits `2`, with the same message).

`lo up` runs provision → framework bootstrap (applies `spec.bootstrap` addons via `.lok8s/libs/bootstrap`) → Tilt. `lo build` renders the domain kustomization into one `artifacts.yaml`; `lo deploy` applies that single artifact (CRDs first, then the rest). `lo lint` validates `spec.bootstrap` entries and target kustomizations. See [Concepts](../guide/concepts.md) and [Specs reference](specs.md) for the model.

## Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--verbose` | `-v` | Enable verbose/debug logging (sets `DEBUG=1`) |
| `--force` | `-f` | Force operation without prompts (also recreates immutable/terminating conflicts, like `--force-recreate`) |
| `--force-recreate` | | On apply, recreate objects blocked by an immutable field or a stuck Terminating finalizer |
| `--remote` | `-r` | Provision on a remote VM (activates `spec.provider` + `spec.remote`) |
| `--kubernetes` | | Kubernetes version to use |
| `--cluster` | `-s` | Cluster name to manage (default: `local`) |
| `--config` | | Kind config file path |
| `--domain` | | Domain name override |
| `--domain-sans` | | Domain SANs override |
| `--no-eject` | | Never write embedded framework assets into the project; serve them from a temp dir (env form: `LO_ASSETS_EJECT=never`). See [`lo assets`](#lo-assets) |

## Commands

### lo up

Start a cluster with Tilt.

```bash
lo up [--open-tilt|-o] [--remote|-r]
lo up --ci [--timeout|-t <duration>]     # headless: build + deploy + wait, real exit status
```

Needs `clusters/<domain>/cluster.lok8s.yaml` (or a `deploy.lok8s.yaml` with a `clusterRef`): the provision dispatch reads the spec first and stops with `No cluster.lok8s.yaml or deploy.lok8s.yaml found` when neither exists. There is no spec-less fallback.

Steps: provision cluster, apply `spec.bootstrap` addons in order via the framework bootstrap (`.lok8s/libs/bootstrap`), start Tilt.

With `spec.registries.tls: true` (the default), the first run of a registry set mints its TLS certificate into the docker volume `<network>-registry-tls`. Every later run reads the certificate from the volume. It mints again only when the registry set changed. See [`lo registry tls`](#lo-registry-tls).

With `--remote`: provisions a VM via `spec.provider`, then runs kind on the remote Docker host. See [Remote clusters](#remote-clusters) below.

| Flag | Description |
|------|-------------|
| `--open-tilt`, `-o` | Open the Tilt UI in a browser after startup |
| `--ci` | Headless: after provisioning, run `tilt ci` in the foreground (build + deploy + wait for Ready) instead of a backgrounded `tilt up`. No TTY, no browser. `lo up` exits with `tilt ci`'s status, so a non-zero exit means the stack did not converge |
| `--timeout`, `-t` | Readiness timeout for `--ci` (e.g. `300s`, `10m`); passed to `tilt ci --timeout` |

### lo down

Stop the cluster and Tilt.

```bash
lo down
```

Stops Tilt and deletes the kind cluster. The sharing mode decides what happens
to the registries. A **non-shared** setup (the default) is project-local with
nothing to reuse, so `lo down` tears down its registry containers (the named
volumes, and thus the build cache, stay). A **shared** setup (opt-in) stays
running: the pull-through mirrors get reused across clusters, and a warm
`build`/`cache` speeds up the next `lo up` (remove them with `lo registry down`,
or `lo registry clean --shared` to drop volumes too).

### lo clean

Clean up local volumes and optionally prune Docker.

```bash
lo clean [--all|-a]
```

Stops Tilt, deletes the kind cluster, removes cluster-prefixed Docker volumes, and cleans registries.

| Flag | Description |
|------|-------------|
| `--all`, `-a` | Also run `docker system prune -f` |

### lo provision

Provision a cluster through the full lifecycle.

```bash
lo provision [--domain <domain>] [--bootstrap|-b] [--force|-f] [--remote|-r]
```

Resolves the cluster spec, sources the driver contract, calls `driver::provision`, then runs `bootstrap::apply` to apply `spec.bootstrap` addons in order with health waits between stages.

With `--remote`: loads `spec.provider`, provisions the cloud VM, then either sets `DOCKER_HOST` to the remote Docker (docker mode) or syncs the repo and runs `lo provision` on the VM (CI mode). See [Remote clusters](#remote-clusters).

| Flag | Description |
|------|-------------|
| `--bootstrap`, `-b` | Re-apply `spec.bootstrap` only, on an existing cluster (skip the infra reconcile) |
| `--force`, `-f` | Global flag: skip the real-infrastructure confirmation prompt (cloud drivers) |

### lo bootstrap

Apply or re-apply bootstrap addons without full re-provisioning.

```bash
lo bootstrap [--domain <domain>]
```

Reads `spec.bootstrap` from the cluster spec and applies each addon in order. Useful after changing bootstrap entries or updating addon values. Addons resolve to `.lok8s/addons/<name>/` (framework) or `clusters/<domain>/targets/<path>` (cluster-specific). See [Bootstrap Addons](../guide/addons.md).

### lo build

Render the domain kustomization into one artifact.

```bash
lo build [--domain <domain>] [--cluster-override <domain>]
```

Runs `kustomize build --enable-alpha-plugins` on the domain's own kustomization (`clusters/<domain>/kustomization.yaml`) and writes ONE `clusters/<domain>/artifacts.yaml`. The domain kustomization composes the targets it wants, in order (local and shared):

```yaml
# clusters/<domain>/kustomization.yaml
resources:
  - ./targets/networking      # domain-local target
  - ../../.targets/monitoring  # shared target
```

A referenced target that does not exist is a clear `kustomize build` error. There is no per-target loop and no `artifacts/<target>/` output: target selection and ordering live in the kustomization you author.

### lo deploy

Deploy the domain artifact to a cluster.

```bash
lo deploy [--domain <domain>] [--cluster-override <domain>] [-l|--label key=value]
```

Applies the single `clusters/<domain>/artifacts.yaml`: CRDs first (server-side apply + wait for Established), then the rest (server-side apply + a scoped wait for the manifest's own workloads to become ready). Run `lo build` first.

Selective deploy is **opt-in**: pass `-l key=value` to apply only the objects carrying that label, e.g. `lo deploy -l lok8s.dev/name=zitadel`. It requires you to have labelled the targets you want to address (kustomize `labels:` or `metadata.labels`); without a match it is a graceful no-op. The key may be a bare key or a namespaced one (`lok8s.dev/name`).

| Flag | Description |
|------|-------------|
| `-l`, `--label` | Only deploy objects carrying this `key=value` label (opt-in selective deploy) |
| `--cluster-override` | Override the cluster domain used for kubeconfig resolution (deploy domains) |

### lo destroy

Destroy a cluster.

```bash
lo destroy [--domain <domain>]
```

Calls `driver::destroy` from the appropriate driver contract.

### lo recover

Rebuild a cluster **from bare metal** (disaster recovery). Orchestrates
`resolve → doctor → consent → rebuild → provision → verify`, reusing the provider's
`provider::rebuild` node reset and a fresh `lo provision` (incl. the bare-metal
`#wipe-devices` wipe). Requires a **cluster** domain whose provider implements
`provider::rebuild`. Restores the cluster, not application data. See
[Disaster Recovery](../guide/recover.md) and [Backups](../guide/backups.md).

```bash
lo recover <domain>                 # full recovery (prompts once to confirm)
lo recover <domain> --dry-run       # preview the rebuild plan; change nothing
lo recover <domain> --skip-rebuild  # re-provision + verify only
lo recover <domain> --force         # skip the confirmation prompt
```

Name the target: the positional `<domain>` is the documented form and outranks `--domain` and the active domain. A command that reimages a fleet must not inherit whatever `.active` points at mid-incident. Omitting both falls back to `--domain`, then the active domain.

| Flag | Description |
|------|-------------|
| `--dry-run` | Run doctor + the `provider::rebuild` plan under `CLOUD_DRY_RUN` (reimages nothing), then stop before provision. |
| `--skip-rebuild` | Skip the node rebuild: run `lo provision` + verify only. |
| `--force`, `-f` | Global flag: skip the destructive-consent prompt (also honored via `LOK8S_NONINTERACTIVE=1`). |

The destructive-consent prompt is **the** guard and lives in the command;
`provider::doctor` only advises, and `provider::rebuild` enforces via its own
atomic preflight. `--dry-run` is genuinely safe (it reimages nothing).

### lo init

Scaffold lok8s config from a correct template, so nothing is hand-written from imagination.

```bash
lo init                                  # on a terminal: the wizard; otherwise: this help
lo init --plan                           # what lo init sees here and the commands it would run; writes nothing
lo init project [name] [--path <dir>] [--force] [--env mise|direnv|none] [--cluster <domain> --driver lo|kubeone|capi|kkp|kubehz-hosted] [--implementation go|bash]
lo init service <name> [--path <dir>] [--force]
lo init test [--path <dir>] [--force]
```

**Bare `lo init`** first detects where you stand, then asks. On a terminal (stdin and stdout are both a TTY, `CI` is unset, no `--yes`) it runs a wizard. The wizard prints a state card and asks the questions the situation calls for. Then it prints a summary of the files it will write and the commands it will run. It writes nothing before you confirm. Off a terminal, under `CI`, or with `--yes`, it prints this help and exits 0, as it always did.

Every answer has a flag twin on one of the subcommands below: `lo init project --env`, `--cluster`, `--driver`, `--implementation`, `lo init service <name>`, `lo init test`, `lo toolchain install --groups`, `lo assets eject bash`, `lo use <domain>`. The wizard fills the same options and calls the same functions. The summary prints the equivalent command lines, so a script can reproduce a run without the conversation. The wizard never overwrites an existing file, and a cluster spec that exists is listed as "keep". When a step fails, the wizard prints the failed command and the commands not run, so you can continue by hand.

The five situations:

| Where you stand | What the wizard offers |
|---|---|
| An empty directory (a `.git` entry does not count) | The welcome: project here, `git init` (when git is installed and there is no repository), the environment file, the first cluster spec (domain and driver), the toolchain and its groups, `lo use` for the new domain |
| Inside a git repository, below its root, no project | The same, with the git root as the suggested directory and the environment file the root already has |
| A non-empty directory without a project (no git, or at the git root) | What it sees, then the same with here or a subdirectory as the target, and `git init` when there is no repository |
| A project root | The status card (the project, git, clusters and the active domain, the environment file, the toolchain, the implementation, plus the doctor's domain and toolchain sections), then a menu: add a cluster spec, add a service, add the test suite, add an environment file, install the toolchain, switch the implementation, or nothing |
| Inside a project (a subdirectory, a service directory, a submodule under the umbrella project) | The card and the menu, plus "add this service to `services.yaml`" in a service directory (a kind-less `lok8s.yaml`) |

The project root is the nearest `clusters/` directory or `kind: Project` `lok8s.yaml` above the working directory (the same walk every command uses); an exported `PATH_BASE` does not move it, the wizard acts where you stand. A service directory below the root keeps the umbrella project as the root: `lo init` in `services/api/` offers to register `./services/api`.

| Flag | Description |
|------|-------------|
| `--plan` | Print the state card and the commands the defaults would run (or, in a project, the menu with its commands), then exit 0. Writes nothing and works off a terminal. |
| `--yes`, `-y` | Never ask: print the help instead of the wizard (scripts, CI). |
| `--dry-run`, `-n` | Run the wizard up to the summary and stop; writes nothing. Off the wizard (no terminal, `CI`, `--yes`) there is no conversation to stop, so it prints the plan like `--plan`. |

The summary lists one line per step and the commands to run instead; when a step uses the network (the toolchain), the confirmation says so. Leaving the form (Ctrl-C, Esc) writes nothing and exits 1.

**`lo init project [name]`** writes the smallest project the binary needs, and nothing else: `clusters/` (one directory per domain goes here), a project-root `lok8s.yaml` (`kind: Project`, the marker `lo` resolves the root from; a service's `kind: Service` file is not one, so `lo` keeps walking up from inside a service directory), the `.gitignore` entries for the toolchain, kubeconfigs, built plugins and secret stores, and one shell environment file (`--env`). It uses no network and writes **no `.lok8s/` tree** (the framework assets a cluster references are embedded in the binary and ejected into `.lok8s/` on first use; see [`lo assets`](#lo-assets)) and no `.bin/`: the toolchain is the next step it prints, [`lo toolchain install`](#lo-toolchain). A re-run keeps every existing file (`--force` overwrites; `.gitignore` is only appended to). `name` defaults to the directory name. The target is the working directory (`--path` names another), never the ambient `PATH_BASE` of a direnv/mise shell.

`--env` selects the shell environment file: `mise` (default) writes a `mise.toml` whose `[env]` puts `.bin` on `PATH` via `mise activate` (and shows how to let mise bootstrap `b` itself); `direnv` writes an `.envrc` with `PATH_add .bin`; `none` writes neither (CI). One file per project: the old `both` is refused with an error that names the two valid values. The two files render one data structure, so they say the same thing, and neither pins any `PATH_*` variable: the binary resolves the project from `lok8s.yaml`, and an ambient `PATH_BASE` redirects runs into whatever project it points at. After `mise trust` / `direnv allow`, `lo` and the b-managed tools are on `PATH`.

`--cluster <domain>` also writes the first cluster spec, `clusters/<domain>/cluster.lok8s.yaml`: the minimal file the readers accept (`apiVersion`, `kind`, `metadata.name` from the domain's first label, `spec.cluster.domain`, `spec.bootstrap`), in the shape [Cluster specs](specs.md) documents. `--driver` selects the kind: `lo` (kind on local Docker, the default), `kubeone`, `capi`, `kkp`, or `kubehz-hosted`. The `lo` file makes the `cilium` default explicit, because kind ships no CNI. The `kubehz-hosted` file is a `KubeOne` spec with `spec.kubehz.hosting: hosted` and the platform API URL. The two `KubeOne` files carry `spec.kubernetes.version` and `spec.provider` as commented stubs with the documented example values, under a "fill in before `lo provision`" header. The domain must match the rule `lo use` applies. `lo lint` accepts every file this writes. On an existing project, `lo init project --env none --cluster <domain> --driver <driver>` adds a spec and keeps everything else.

`--implementation go|bash` sets `spec.implementation.default` in `lok8s.yaml` (see [Choosing the implementation](#choosing-the-implementation)). The file is created when it is missing; an existing file keeps its comments and its other keys. This is the flag twin of the wizard's implementation switch, and `lo assets eject bash` is the step before it when the project has no bash tree.

**`lo init service <name>`** scaffolds a bare per-service `lok8s.yaml` (shaped to pass the per-service validator), registers it in the project-root `services.yaml`, and ensures the project Tiltfile is the canonical 2-line loader.

**`lo init test`** scaffolds a domain-parameterized [Playwright](https://playwright.dev) integration suite into `tests/` (default; override with `--path`). The generated suite is project- and domain-agnostic: it runs the SAME specs against your dev cluster, staging, and production by changing only `LOK8S_TEST_DOMAIN`. See [Testing](../guide/testing.md). It refuses to overwrite a non-empty directory unless `--force` (and even then copies file-by-file, preserving local additions).

| Flag | Description |
|------|-------------|
| `--path`, `-p` | Target directory (project dir / service dir / `tests/` dir) |
| `--force`, `-f` | Overwrite existing files / non-empty target |
| `--env` | `project`: the environment file, `mise` (default), `direnv` or `none` |
| `--cluster`, `--driver` | `project`: also write `clusters/<domain>/cluster.lok8s.yaml` for that driver |
| `--implementation` | `project`: set `spec.implementation.default` (`go` or `bash`) in `lok8s.yaml` |

### lo toolchain

Install the pinned project toolchain with [`b`](https://github.com/fentas/b), and verify it. Go-only: the bash tree gets its toolchain from the b profile and has no such step.

```bash
lo toolchain install [--path <dir>] [--groups core,local[,cloud][,bash]] [--dry-run]
lo toolchain doctor
```

**`lo toolchain install`** runs four steps. The project is the nearest `kind: Project` `lok8s.yaml` (or `clusters/`) above the working directory, never the ambient `PATH_BASE` of a direnv/mise shell; `--path` names one explicitly.

1. `.bin/b.yaml` from a template whose pins are the releases this `lo` was built and byte-parity-tested against: `kustomize` v5.8.1 (the CLI built from the kustomize API `lo-full` links), `github.com/mgoltzsche/khelm` v2.8.0 installed as the `ChartRenderer` exec plugin under `.kustomize/`, and `github.com/kernpilot/lok8s`'s `kustomize-secret-*` asset at **this `lo`'s own version** installed as the `secrets.lok8s.dev` Secret plugin. The other entries are `kubectl` (group `core`), `kind`/`tilt`/`mkcert` (group `local`, on by default), `kubeone`/`hcloud` (group `cloud`, opt-in) and `argsh`/`yq`/`jq`/`envsubst`/`sops`/`ssh-to-age` (group `bash`, opt-in). The template writes entries outside the selected groups commented out. `go test` checks the pins against `go.mod` (`internal/toolchain`), so the template cannot lag the binary. The `bash` group is the runtime of the frozen bash implementation (a command routed to bash, see [Choosing the implementation](#choosing-the-implementation)) and of the provider plugins. The binary does not need those tools for its own paths. The bash tree itself ships inside the binary (see [`lo assets`](#lo-assets)). **An existing `.bin/b.yaml` is never overwritten**: a unified diff against the template is printed with the instructions (move it aside and re-run, or merge the pins by hand; `lo toolchain doctor` reports what differs).
2. The `.gitignore` entries (`.bin/*` with `b.yaml`/`b.lock` kept, `.kustomize/`, …).
3. `b` itself into `.bin/b` when absent, from b's own release tarball (`b-<os>-<arch>.tar.gz`, the same asset b's installer and `b install b` resolve), pinned to a release whose SHA-256 sums are recorded in the binary from that release's published `checksums.txt`; downloaded over https to a temp file, verified, and only then extracted. A redirect off https and an oversized archive or `b` member are refused. Never `curl | sh`. `GITHUB_TOKEN` is passed through when set (b works token-free for public sources). b publishes no darwin build: on macOS the command stops with the manual-install pointer ([binary.help](https://binary.help)); put `b` on `PATH` or at `.bin/b` and re-run.
4. `.bin/b install` in the project (`PATH_BIN=.bin`), so every binary lands in `.bin/` and the two plugins under `.kustomize/`.

`--dry-run` prints each step (the diff, the download URL and expected sum, the install command) and touches neither the tree nor the network.

**`lo toolchain doctor`** prints the toolchain section of [`lo doctor`](#lo-doctor) on its own: `.bin/b` with its version, then `kustomize`, the khelm `ChartRenderer` and the `secrets.lok8s.dev` Secret plugin, each against its pin. No marker and no flag gate it. It takes the project of the current shell (an exported `PATH_BASE` wins, else the nearest project above the working directory), like `lo doctor`; `install` resolves from the working directory only. The exit code is `1` when a tool this build execs is missing (`lo` core); `lo-full` only warns about the render tools.

`lo init toolchain` is the old name of `lo toolchain install`. It stays for one release as a hidden alias: same flags, same output, plus one hint line on stderr. Use the new name.
**A minimal project.** Two files are enough for `lo up`: the project file and one cluster spec. The Lo driver fills the rest from documented defaults (see [Cluster specs](specs.md#default-resolution)); `lo lint --notes` names the keys you can drop because they equal a default.

```
my-project/
  lok8s.yaml                          # kind: Project, metadata.name
  clusters/
    demo.dev/
      cluster.lok8s.yaml              # kind: Lo, metadata.name, spec.cluster.domain, spec.bootstrap
```

```bash
mkdir my-project && cd my-project
lo init project                       # the two directories above, .gitignore entries, mise.toml
lo toolchain install                  # .bin/b.yaml, b, the pinned toolchain (the one network step)
$EDITOR clusters/demo.dev/cluster.lok8s.yaml
lo use demo.dev && lo up
```

### lo use

Set or show the active domain.

```bash
lo use [domain]
```

Without arguments: shows the active domain and lists all available domains with their kind types. With a domain argument: validates the domain directory exists and writes it to `clusters/.active`.

### lo lint

Validate domain structure and specs.

```bash
lo lint [--domain <domain>] [--notes]
```

Checks:
- Each domain has `cluster.lok8s.yaml` or `deploy.lok8s.yaml`
- Each `spec.bootstrap` entry resolves to an existing driver addon directory or user path
- Kustomization files under `targets/` reference existing resources
- Secrets: committed encrypted (`.enc` present and current), and no per-domain secret is shadowed in the deprecated flat `.secrets/` store (identical copy = stale duplicate; differing copy = active drift)

`--notes` (Go-only) adds an advisory per Lo cluster spec key whose value equals its documented default (see [Cluster specs](specs.md#default-resolution)): `spec.nodes.controlPlane: 1`, `spec.runtime: kind`, and `spec.registries.mirrors` when the list is exactly the four default mirrors on their standard URLs. Each prints as `[note] <file>: <key> equals the default (<value>); you can drop it` on stdout. It is advice only: no finding, the exit code is unchanged, and the spec is never edited. The flag is opt-in because the bash lint prints no such line and the parity harnesses diff every lint case byte for byte.

### lo status

Check cluster health and status.

```bash
lo status [--domain <domain>]
```

Delegates to the driver contract's `driver::status` function. For Lo clusters: checks if the kind cluster exists. For Capi clusters: queries the CAPI Cluster resource phase.

### lo gitops

GitOps integration (Flux / Argo). **Deferred.** Both subcommands currently return a deferred-error stub: the integration is under redesign around the new `services.yaml` targets-map model.

```bash
lo gitops flux [--domain <domain>]    # (deferred)
lo gitops argo [--domain <domain>]    # (deferred)
```

### Cluster lifecycle (there is no `lo kind`)

The lifecycle commands manage the kind cluster: there is no `lo kind`
command. Use `lo up` / `lo down` / `lo clean` (create + teardown), `lo provision`
/ `lo destroy` (provision without starting Tilt), and `lo kubeconfig` (extract
the kubeconfig). The Docker bridge network is created automatically from
`spec.network`.

### lo tilt

Manage the Tilt environment.

```bash
lo tilt up        # Start Tilt in background
lo tilt down      # Stop Tilt
lo tilt status    # Run tilt doctor
lo tilt restart   # Stop + start
lo tilt ci [--timeout|-t <duration>]    # Headless build + deploy + wait-ready (tilt ci); exits with its status
lo tilt preflight [--age|-a <seconds>] [--crds|-c drain|skip|force] [--crd-allow <names>]
                  # Force-clear stuck-Terminating objects in the manifest read from stdin
                  # (what the Tiltfile runs before an apply; LOK8S_PREFLIGHT=0 disables it)
```

### lo registry

Manage Docker registry mirrors.

```bash
lo registry up                    # start the mirrors for the active domain
lo registry down
lo registry status [--shared|-S]
lo registry clean [--shared|-S]   # --shared also clears the shared mirror network
```

Registries derive entirely from `spec.registries` (the `mirrors[]` plus the framework-private `build` and `cache` registries): there are no per-registry flags; the only flag is `--shared`/`-S`, which includes the shared `lok8s-registries` network for `status`/`clean`. Registries run on the configured Docker bridge network (default: `lok8s` at `10.125.125.0/24` for slot 125); the driver computes IPs automatically from `spec.network.cidr` and `spec.registries.shared.network.cidr`. See [Specs reference](specs#registries-configuration).

#### lo registry tls

The registry set's TLS certificate. Both implementations have the command; the output is the same.

```bash
lo registry tls status            # the certificate and what each container mounts
lo registry tls renew             # mint a new certificate into the volume, restart the set
```

`status` prints `registry TLS on` or `off`, the volume name, `not before` and `not after`, and the `sans` line. It adds a `san set stale` line when the registries changed since the mint. Then it prints one line per container: `volume <name>`, `bind <dir> (legacy)` for a container that still mounts the directory of a release before v0.4.0, `no certificate mount`, or `absent`. It exits 0 in every one of these cases. It exits 1 when the domain is not a Lo domain or when the registry JSON cannot be resolved.

`renew` mints a fresh certificate into the volume with the current SAN set. It restarts every existing container of the set and prints `registry TLS renewed: volume <name>, <n> SANs`. It exits 1 with `spec.registries.tls is false` when TLS is off. It exits 1 when the Secret plugin or docker fails, and the error names the step. The next `lo up` recreates the containers, because the certificate signature is part of their config hash.

The default 6-registry set:

| Name | Default IP | Hostname | Purpose |
|------|------------|----------|---------|
| `build` | `10.125.125.101` | `lok8s.local` | Tilt push target for locally-built images |
| `cache` | `10.125.125.102` | `lok8s.cache` | Pre-pull target for `build:false` services with a remote registry |
| `io-docker` | `10.125.125.103` | `docker.io` | Pull-through mirror |
| `io-quay` | `10.125.125.104` | `quay.io` | Pull-through mirror |
| `io-k8s` | `10.125.125.105` | `registry.k8s.io` | Pull-through mirror |
| `io-ghcr` | `10.125.125.106` | `ghcr.io` | Pull-through mirror |

`build` and `cache` always live on the project subnet (`.101`/`.102`),
and by default the mirrors do too (`.103`+). With
`spec.registries.shared.enabled: true` (opt-in) the mirrors move to the
shared `lok8s-registries` network (`10.125.200.2`+) so multiple projects
reuse one cache. See
[Shared registries](/guide/shared-registries) for the trade-off.

### lo image

Manage the local cache registry: pre-pull private/CI images so kind can fetch them without upstream credentials.

```bash
lo image cache <service> [--force|-f]   # Pre-pull a single service's image
lo image cache --all [--force|-f]       # Drain the pre-pull queue written at build time
lo image list                            # Show what's currently in the cache registry
lo image clean                           # Drop the cache registry volume
```

The cache flow runs automatically as part of `lo build` / `lo up` when any service has `build: false` and a resolved `registry.endpoint`. See [Services Configuration → Cache mode](/guide/services#cache-mode-the-lok8scache-registry) for the full pipeline. `registry.parallel` in `services.yaml` controls parallelism (`0` unlimited, `1` sequential default, `N≥2` bounded).

### lo secrets

Manage the secret cache (`$PATH_SECRETS`) and its optional SOPS/age encryption
(`lo secrets list` / `print` inspect it). See the [Secrets guide](/guide/secrets)
for the full workflow.

```bash
lo secrets init                                # set up SOPS/age from your SSH key
lo secrets set --name <n> --namespace <ns> <key> [value]   # write a value (omitted: tty prompt / piped stdin; `-`: read the value from stdin)
lo secrets set --name <n> <key> --encrypt      # write + SOPS-encrypt this one file (-e/--enc; needs .sops.yaml)
lo secrets allow                               # approve bash: generators after a change
lo secrets encrypt                             # write committable Secret.*.enc files
lo secrets decrypt                             # restore the plaintext cache from .enc
lo secrets add-key <ssh-pubkey-path|age1…>     # add an age recipient to .sops.yaml + re-key the store (--all: every domain)
lo secrets env --name <n> [--namespace <ns>]   # emit injection-safe `export KEY=value` lines for a cached secret (for eval)
lo secrets list | print [pattern...] | path    # inspect the cache
```

**Encryption**: `init` derives an age recipient from `~/.ssh/id_ed25519` (via
`ssh-to-age`, ed25519 only) and writes `.sops.yaml`; `encrypt`/`decrypt`
round-trip the cache so secrets commit safely as `Secret.*.enc`. Needs `sops`
and `ssh-to-age` (`b install`).

### lo mcp

Serve the `lo` commands as MCP ([Model Context Protocol](https://modelcontextprotocol.io/)) tools.

```bash
lo mcp start [--allow-mutating] [--allow-destructive] [--log-level <l>]   # stdio — what editors and agents launch
lo mcp serve [--host 127.0.0.1] [--port 8080] [...]                        # streamable HTTP (loopback; no auth)
lo mcp tools [--allow-mutating] [--allow-destructive]                      # print what a server would expose
lo mcp claude|vscode|cursor enable [--env LO_MCP_ALLOW=…]                  # write the editor's MCP config
```

Every user-facing leaf subcommand becomes a tool named `lo_<path…>` (`lo_status`, `lo_build`, `lo_secrets_encrypt`, `lo_kubehz_join`, …) — the same scheme the former argsh builtin used. Dispatchers (`tilt`, `gitops`, `kubehz`, …) are traversed, not exposed; framework-internal commands (hidden from `--help`) are never tools, and neither are `mcp` and `operator` themselves.

**Exposure is structural**: a tool that is not exposed is not registered, so it cannot be called by name either. The tiers follow the usage markers:

| Marker | MCP hint | Exposed |
|-----------|----------|--------|
| `@readonly` | `readOnlyHint: true` | by default |
| _(none)_ | — | with `--allow-mutating` (a command without a marker counts as mutating — it is not known to be safe) |
| `@destructive` | `destructiveHint: true` | with `--allow-destructive` (implies `--allow-mutating`) |
| `@idempotent` | `idempotentHint: true` | informational |

Flags that carry a credential (`token`, `secret`, `password`, `key`, `nonce`, …) are never exposed; `--force` / `--force-recreate` only with `--allow-destructive`. `LO_MCP_ALLOW=mutating|destructive` is the environment form of the opt-in (flags win), which is what `lo mcp <editor> enable --env LO_MCP_ALLOW=…` writes into the editor config.

#### MCP server

Point the editor at `lo mcp start`. The `.mcp.json` at the project root does this for a checkout:

```json
{
  "mcpServers": {
    "lok8s": {
      "type": "stdio",
      "command": "bin/lo",
      "args": ["mcp", "start"],
      "env": {
        "PATH_BASE": ".",
        "LO_MCP_ALLOW": "destructive"
      }
    }
  }
}
```

The server key stays `lok8s`, so the tool names (`lo_status`, `lo_build`, `lo_tilt_up`, ...) do not change.

`bin/lo` is the checkout build. Run `make build` in a fresh clone before you start the editor. Outside a checkout, run `lo mcp <editor> enable`: it writes the absolute path of the binary, the toolchain PATH and `PATH_BASE` into the editor config. You can also set `command` to the installed `lo` (from `lo-install.sh`, or `.bin/lo` from `b install`) when that binary is on the editor's PATH. Do not use a bare `lo` in a project with the `.envrc` active. The `.envrc` puts `.lok8s` first on PATH, so `lo` resolves to the bash entry. Then `.lok8s/lo mcp start` serves the bash variant without an error. A project whose `lok8s.yaml` routes commands to bash (see [Choosing the implementation](#choosing-the-implementation)) keeps the same tool list: the server projects the Go tree without the routing, and each tool call runs `lo <cmd> …` as a subprocess, which applies the routing itself.

The server needs no other environment. `PATH_BASE: "."` pins the project root to the directory the editor starts the server in, so a `PATH_BASE` inherited from another project's shell cannot redirect it. Without that line the server takes the root from an exported `PATH_BASE` when set, else from the working directory. For every tool call it prepends the toolchain (`.bin`) and framework (`.lok8s`) directories to PATH.

`LO_MCP_ALLOW=destructive` opens the full surface (90 tools). That is the set the argsh builtin served, plus the Go-only leaves. Remove the `env` entry for the readonly default (29 tools), or set `mutating` for the middle tier (51 tools). The tiers follow the marker table above.

#### Bash variant

The frozen argsh implementation serves the same protocol from the `mcp` builtin in `argsh.so` (`argsh builtins install`). argsh loads it from `ARGSH_BUILTIN_PATH`, `PATH_BIN/argsh.so`, `BASH_LOADABLES_PATH` or `LD_LIBRARY_PATH`. The builtin has no tiers: every leaf is a tool (69 tools), and the editor's own approval prompt is the only gate. `lo chat` still drives this server.

The bash server runs when the project's `lok8s.yaml` sets `spec.implementation.default: bash` (planned, WP8). The same block routes single commands: `spec.implementation.bash.commands` lists the commands the binary runs through the bash tree, and `spec.implementation.bash.tree` names that tree (default `.lok8s`). Implementation selection lives in the committed project config, not in the environment.

Until WP8 lands, start the frozen tree directly from a checkout:

```json
"lok8s": {
  "type": "stdio",
  "command": ".lok8s/lo",
  "args": ["mcp"],
  "env": {}
}
```

The entry point derives `PATH_BASE`, `PATH_BIN` and `PATH_LOK8S` from its own location. Set `PATH_LOK8S` only for a framework tree outside the project.

The two servers differ in three places. The builtin flattens a two-level dispatcher path (`lo_handover_receive`, `lo_node_join`). The Go server keeps the full path (`lo_kubehz_handover_receive`, `lo_kubehz_node_join`). The builtin exposes `lo drivers` as one tool. The Go server spells out every driver and operation (`lo_drivers_lo_provision`, ...). The Go-only commands `lo init project` and `lo toolchain` have no builtin tool.

### lo kubeconfig

Print a domain's kubeconfig on stdout.

```bash
lo kubeconfig [--domain <domain>]    # admin kubeconfig (alias: lo kc)
lo kubeconfig --oidc                 # kubelogin exec-plugin kubeconfig (browser OIDC login)
lo kubeconfig --cluster-override <domain>   # resolve against another cluster domain
```

A deploy domain follows its `spec.clusterRef` to the real cluster. The `--oidc` form reuses the same server + CA as the admin kubeconfig but authenticates the user through `spec.oidc`'s IdP via `kubectl oidc-login`: safe to hand to teammates.

### lo audit

Static security-posture audit: read-only and cluster-free.

```bash
lo audit [domain] [--json | --sarif]
```

Scans the domain's specs, secrets hygiene, and rendered targets for posture findings. `--json` emits machine-readable output for CI. `--sarif` emits SARIF 2.1.0 for GitHub code-scanning upload (see the [audit guide](/guide/audit)).

### lo doctor

Diagnose the local environment and toolchain.

```bash
lo doctor [--toolchain]
```

Checks required binaries, versions, Docker/kind state, and common misconfigurations, with a fix hint per finding.

The **`implementation` line** (Go-only) appears only when `lok8s.yaml` routes commands to bash (see [Choosing the implementation](#choosing-the-implementation)): the routed set, the tree and its source, then one `!` line per routed command whose state Go also writes (`registry`, `image`, `secrets`, `use`, `kustomize`). An invalid block, or a routing whose tree is missing, is one `!` line (doctor runs in Go on either). The **`bash mode` line** (Go-only) says whether a command routed to bash and the provider plugins can run: the bash tree in use (the project's own tree, or the copy the binary extracts into `${XDG_CACHE_HOME:-$HOME/.cache}/lok8s/<version>/`) and whether `argsh` is at `.bin/argsh`, where the tree sources it. When `argsh` is missing the line is a warning with the fix (uncomment the `bash` group in `.bin/b.yaml`, then `.bin/b install`). The line is omitted when the project holds the tree and `argsh` is present, which keeps the output byte-identical to the bash implementation there.

The **toolchain section** (Go-only) verifies what [`lo toolchain install`](#lo-toolchain) installed against the pins: `.bin/b` (with its version), `kustomize` (`.bin` first, then `PATH`) at the pinned release, the khelm `ChartRenderer` and the `secrets.lok8s.dev` `Secret` exec plugins at the paths the render resolves under `.kustomize/` (`KUSTOMIZE_PLUGIN_HOME`), each at its pin, the Secret plugin at this `lo`'s own version (`<plugin> --version`; a plugin built before that flag existed reports "version unknown"). A mismatch is a warning; a missing tool is a failure on `lo` (core, which execs them) and a warning on `lo-full` (in-process render; the binaries only serve `LO_RENDER=exec`). The fix is always `lo toolchain install`. The section is printed when `.bin/b.yaml` carries the `lo toolchain install` marker line (the line an older `lo init toolchain` wrote also counts), or on `--toolchain`; a profile-synced or hand-written `b.yaml` is not checked unless asked, which keeps the default output byte-identical to the bash implementation. `lo toolchain doctor` prints this section alone, with no gate.

### lo trust

Install the local dev CA into the OS/browser trust stores (wraps `mkcert -install`, same CAROOT the `cert:` secrets generator uses).

```bash
lo trust
```

### lo version

Print lok8s and toolchain versions.

```bash
lo version
```

### lo addons

List driver bootstrap addons for the active cluster; name one to inspect it.

```bash
lo addons [name...] [--detail] [--origin]
```

The list is the union of the addons embedded in the binary and the project's own `.lok8s/addons/*` directories. `--origin` adds an `ORIGIN` column: `builtin` (served from the binary, not ejected), `local` (the project's copy, identical to what the binary ships), `local (modified)` (the project's copy differs; `lo assets diff addons/<name>` shows how), `local-only` (an addon the binary does not ship). Without `--origin` the table is the classic four-column one. Naming an addon that the project holds no copy of ejects it first (see [`lo assets`](#lo-assets)), so `path:` always points into the project; `--no-eject` serves it from a temp dir instead.

### lo drivers

Driver-specific commands.

```bash
lo drivers --list [--origin]  # list available drivers
lo drivers <name> <args…>     # invoke a driver's own subcommands
```

`--list` prints the union of the Go driver registry (`lo`, `capi`, `kubeone`, `kkp`, `kubehz`) and the driver directories under `.lok8s/drivers/`; `--origin` adds the origin of each driver's cluster templates (`drivers/<name>/cluster`: `builtin`, `local`, `local (modified)`, or `local-only` for a bash-only driver directory). A name that exists only as a bash driver is handed to the argsh implementation with the arguments untouched; `--help` after a nested command (`lo drivers lo status --help`) reaches that command, where argsh printed the `drivers` usage instead.

### lo assets

The framework's files ship **inside the binary**: the data files (every bootstrap addon, the driver cluster templates (`drivers/{lo,kubeone,capi}/cluster`), the ClusterInventory CRD mirror, the `lo chat` defaults and the Tilt extension the project-root `Tiltfile` loads) and the frozen bash implementation (`lo`, `libs/`, `utils/`, the drivers' code, the provider plugins; the asset `bash`). A project needs no synced `.lok8s/` tree for them. The rules:

- **Precedence.** A copy under the project's `.lok8s/<rel>` always wins over the embedded one, whatever its content.
- **Eject on first use** (the default). When a cluster references an asset (an addon in `spec.bootstrap`, a driver's templates, the CRD at publish time) and the project holds no copy, `lo` writes the embedded one into `.lok8s/<rel>/` together with a `.lo-origin` marker (the `lo` version, a timestamp, one sha256 per file) and prints one `[assets] ejected …` line. Two `lo` processes ejecting the same unit at once are safe: the second finds the unit in place and leaves it. Commit the ejected files: from then on a `lo` upgrade can never silently change what that cluster applies; the diff below shows it and you choose.
- **Never overwrite.** `lo` never touches an existing local file. The one writer of existing files is `lo assets update`.
- **Read-only commands never eject.** `lo lint`, `lo audit`, `lo addons` (the list) and `lo assets` itself serve a missing copy from a temp dir.

```bash
lo assets list [--json]                 # every asset with its origin and chart version (local vs embedded)
lo assets eject [rel...] [--all] [--check]
lo assets diff [rel...] [--json] [--check]   # with a rel: the unit and its per-file states
lo assets update <rel> [--force]
```

`<rel>` is the path below `.lok8s/`: `addons/cilium`, `drivers/lo/cluster`, `drivers/kubeone/cluster`, `drivers/capi/cluster`, `libs/inventory/manifests`, `chat`, `tilt`. The word `bash` names the frozen bash implementation: its files live directly below `.lok8s/` beside the data assets, its marker is `.lok8s/.lo-origin`, and `.lok8s/lo` is what makes it count as present.

**`eject`** without arguments writes what this project references: each cluster spec's builtin `spec.bootstrap` addons, its driver's templates, the inventory CRD, and `tilt` when the project-root `Tiltfile` loads the extension. `--all` takes every data asset. **`eject bash`** writes the bash implementation into `.lok8s/`. It also ejects every data asset the project lacks, so `.lok8s/` becomes a complete tree. From then on the provider plugins and `lo drivers <name>` run from the project, and `lok8s.yaml` can route commands to it (see [Choosing the implementation](#choosing-the-implementation)). Without it they run from the copy the binary extracts into `${XDG_CACHE_HOME:-$HOME/.cache}/lok8s/<version>/`. A local tree always wins over that cache. An existing file is kept, never overwritten. `lo assets diff bash` shows a kept file as `local modified`. `bash` is never part of `--all` or of the referenced set. `lo tilt up` and `lo tilt ci` eject `tilt` on first use themselves: Tilt reads `.lok8s/tilt/Tiltfile` from disk, so under `--no-eject` they stop with an error instead. `--check` writes nothing and exits `1` if any of that set would be ejected: the CI gate for "this repository pins what it applies".

**`diff`** is a three-way comparison per file, by content hash: ORIGIN (the `.lo-origin` hashes, what was ejected) vs LOCAL (the project's file) vs EMBEDDED (what this `lo` ships). The headline per addon is the chart version, local vs embedded. Per file:

| State | Meaning |
|---|---|
| `unchanged` | local == embedded |
| `local modified` | you edited it (origin == embedded, local differs); also the verdict for a copy with no marker (a vendored tree), where local is authoritative |
| `lo updated` | this `lo` ships a newer file and yours is untouched (origin == local) |
| `both` | conflict: all three differ |
| `local-only` | exists only in the project |
| `builtin-only` | exists only in the binary (lo added it, or it was deleted locally) |

With one or more `<rel>` arguments the table lists each unit's files with their state (there is no separate `show` command). `--check` exits `1` on any drift (anything but `unchanged`/`local-only`). `--json` is a stable shape (`{"lo": "<version>", "assets": [{rel, kind, origin, drifted, version:{local,embedded}, marker:{lo,ejectedAt}, files:[{path,state,origin,local,embedded}], path}]}`).

**`update <rel>`** applies the embedded copy over the local one only when every file is provably untouched (`unchanged` or `lo updated`) and a marker exists; otherwise it prints the classification and stops, and `--force` applies anyway. A copy that is already byte-identical to what this `lo` ships is reported as in sync, marker or not, and nothing is written. It shows the diff first, keeps local-only files, and rewrites the marker.

Opt-outs: the global `--no-eject` flag or `LO_ASSETS_EJECT=never` serve every missing asset from a per-run temp dir and write nothing into the project. `lo doctor` reports a one-line summary (`N of M local assets drifted` / `all in sync` / `none ejected`).

### lo kubehz

kubehz platform integration (alias: `lo kh`). Requires `spec.kubehz` in the cluster spec.

```bash
lo kubehz register            # register the cluster with the platform
lo kubehz deregister          # remove it
lo kubehz deploy              # deploy the in-cluster agent named by spec.kubehz.agent
lo kubehz deploy --dry-run    # print the rendered manifests, apply nothing
lo kubehz status              # registration + heartbeat status
lo kubehz claim-code          # print the one-time claim code for the dashboard
lo kubehz claim --nonce <v>   # place a dashboard-minted claim nonce for the agent to echo
                              # (`--nonce -` reads it from stdin; KUBEHZ_CLAIM_NONCE is the env fallback — keeps it out of shell history)
lo kubehz re-enroll           # re-enroll a regenerated agent token (heartbeats resume)
lo kubehz join <node>         # mint a node join ticket (hosting: shared); --print-token also prints the plaintext ticket (when a join script was written)
lo kubehz assess              # platform assessment + handover feasibility
lo kubehz handover            # control-plane handover (receive/preseed on the eject target)
lo kubehz node join           # join THIS machine to a hosted cluster (static pool)
lo kubehz node status         # the nodes you brought, and the slot count
lo kubehz node remove --name <n>  # remove one node and free its slot
```

The `node` group is the node-level surface of a **hosted** control plane: the
machines you bring yourself (`kind: static` worker pools). `node join` mints a
ticket and runs the `kubeadm join` line the platform composes, so run it as
root on the machine you are adding; `--print-only` prints the line instead.
Cluster-level `join` and `deregister` above keep their own meaning. See the
[kubehz guide](../guide/kubehz.md#nodes-you-bring-static-pools).

`deploy` renders the agent manifests, substitutes your `apiUrl` and cluster
domain, and applies them with your current kubeconfig, so point it at the right
cluster first (`lo use <domain>`). It applies the agent that
`spec.kubehz.agent` names and stops the other one, because exactly one agent
may send heartbeats. The two directions stop it differently: switching to
`operator` **keeps** the CronJob (it still bootstraps and enrolls the identity
Secret) and silences it through the `KUBEHZ_HEARTBEAT_OWNER` marker it reads
before every beat; switching to `cronjob` **deletes** the live agent's
Deployment, because the Go agent reads no marker and beats whenever it runs.
Re-run it after you change the value. It waits for the switch to be real (for
the new agent to be `Ready`, or for the deleted one's pod to be gone) and
fails instead of continuing if a step does not complete, or if it cannot see
whether the step completed. So it never starts the second producer itself: it
either finishes the switch or stops with the cluster in the single-agent state
it was already in. Extend the waits with `KUBEHZ_LIVE_AGENT_ROLLOUT_SECONDS`
(Ready, 120 s), `KUBEHZ_LIVE_AGENT_DRAIN_SECONDS` (the live agent's pod is
gone, 120 s) and `KUBEHZ_HEARTBEAT_DRAIN_SECONDS` (an in-flight CronJob pod has
finished, 130 s) when a cold image pull or a slow link needs longer. See the
[kubehz guide](../guide/kubehz.md#choosing-an-agent).

### lo kustomize

Manage the Go kustomize plugins (alias: `lo ku`).

```bash
lo kustomize build            # compile plugin binaries into the plugin home
lo kustomize test             # plugin unit + integration tests (runs `make test` in kustomize/: needs make + a Go toolchain)
lo kustomize list             # list discoverable plugins
lo kustomize clean            # remove built binaries
```

### lo chat

Chat with a local AI assistant (transparent, streaming; read-only by default).

```bash
lo chat
```

### lo ai

Manage the AI integration behind `lo chat` and the agent skills.

```bash
lo ai check                   # check the AI setup (runtime + skills)
lo ai skills                  # list skills + per-assistant delivery
lo ai link                    # link skills into an assistant skill dir
lo ai unlink                  # remove linked skills
```

## Remote Clusters

The `--remote` flag enables provisioning Lo clusters on remote VMs
instead of the local Docker host. It requires `spec.provider` and
optionally `spec.remote` in the cluster spec.

### Two modes

**Docker mode** (default): The local machine orchestrates everything
(kind, registries, bootstrap), but Docker commands target the remote VM
via `DOCKER_HOST=ssh://<ip>`. The API is accessed through an SSH tunnel.

```bash
lo up --remote --domain my.lok8s.dev
```

**CI mode** (`spec.remote.mode: ci`): The repo is rsynced to the VM and
`lo provision` runs entirely on the remote. The local machine only
triggers the process and sets up an SSH tunnel for kubectl access.

```bash
lo up --remote --domain ci.lok8s.dev   # spec.remote.mode: ci
```

### How it works

1. `--remote` causes `libs/provision` to load `spec.provider` (e.g. Hetzner)
2. The provider creates the VM (with cloud-init for Docker, SSH config, etc.)
3. The Lo driver waits for SSH, cloud-init, and Docker to be ready
4. **Docker mode**: sets `DOCKER_HOST=ssh://root@<ip>`, runs kind locally
5. **CI mode**: rsyncs the repo, runs `lo provision` on the VM via SSH,
   optionally starts Tilt, sets up nginx expose + kubeconfig tunnel
6. In CI mode, `driver::provision` returns exit code 100 to signal that
   the remote handled everything, so `libs/provision` skips local bootstrap

### Without `--remote`

Without `--remote`, `spec.provider` and `spec.remote` are ignored. The
same cluster spec works for both local and remote provisioning: the
caller drives the mode, not the file.

## Choosing the implementation

The project file `lok8s.yaml` (`kind: Project`) names the implementation. Go is the default. The block routes the whole command tree, or a list of top-level commands, to the frozen bash tree in the project:

```yaml
apiVersion: lok8s.dev/v1
kind: Project
metadata:
  name: my-project
spec:
  implementation:
    default: go              # go | bash; go when absent
    bash:
      commands: [registry]   # top-level commands routed to bash; names only
      tree: .lok8s           # project-relative; the executable is <tree>/lo
```

Rules:

- `default: bash` runs every command through `<tree>/lo` with the argv untouched. `default: go` with a `commands` list routes only the listed commands; every other command stays in Go.
- `commands` takes top-level command names from `lo --help`. An alias (`r`), an unknown name and a Go-only command (`assets`, `mcp`, `operator`, `toolchain`, and `init` for its `project` and `toolchain` subcommands) are errors. There is no `all`: `default: bash` is the whole-tree switch.
- `tree` is relative to the project and stays inside it: no absolute path, no `..`. The default is `.lok8s`. When the block routes something, the tree directory and its `lo` entrypoint must also resolve inside the project (no symlink out); a project that runs Go may link its `.lok8s` anywhere.
- A routing needs the tree in the project. The copy the binary extracts into its cache and a checkout named by `PATH_LOK8S` are never routed to. Without the tree, a routed command stops: `lo: implementation bash: the tree <project>/.lok8s/lo is missing. Run "lo assets eject bash", or set spec.implementation.default: go.` The Go-only commands, `lint`, `doctor` and `init` still run, so `lo assets eject bash` can create the tree.
- No environment variable selects the implementation. The path variables (`PATH_BASE`, `PATH_CLUSTERS`, `PATH_LOK8S`, `PATH_BIN`) select paths for the bash tree. `PATH_BIN` also selects the argsh runtime the tree sources, so a routed command ignores an inherited `PATH_BIN` and puts `<project>/.bin` on the child's PATH and in `PATH_BIN` (the project needs its toolchain there; a link is fine); the remaining trust question (a changed tree) is why a trust layer is planned.
- Every command reads the block at start. An invalid block stops the command with the error on stderr (`lo: lok8s.yaml: …`); `lint`, `doctor`, `help` and `completion` still run. `lo lint` reports the same error as a finding, completes its other checks, and warns on an unknown key under `spec.implementation` (and on a `spec.implementations` typo).
- `lo doctor` prints the effective implementation, the tree and its source, and a `!` line for each routed command whose state Go also writes (`registry`, `image`, `secrets`, `use`, `kustomize`). A customised lib behind such a command changes one writer of shared state; the parity harnesses prove the stock tree only. Such a command is the project's own fork from that point.
- The MCP server (`lo mcp`) projects the Go tree without the routing, so the tool list is stable. Each tool call runs `lo <cmd> …` as a subprocess, which applies the routing itself.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOK8S_CLUSTER_NAME` | `local` | Cluster name |
| `KIND_NODE_VERSION` | `v1.31.12@sha256:...` | Kind node image |
| `KIND_CONFIG` | `.lok8s/drivers/lo/cluster/config.yaml` | Kind config file |
| `DOMAIN_NAME` | (empty) | Domain override. Full precedence: `--domain` flag > `DOMAIN_NAME` env > `clusters/.active` > `lok8s.dev`. When the env var and `.active` disagree, `lo` prints a one-line notice naming which won |
| `DOMAIN_SANS` | `*` | Domain SANs |
| `KIND_EXPERIMENTAL_DOCKER_NETWORK` | `lok8s` | Docker network name |
| `PATH_SECRETS` | (empty) | Active domain's store: `lo build`/`lo deploy` set it to `clusters/<domain>/secrets`; `lo secrets` without `--domain` reads it. The registry TLS mint does not read it: the certificate lives in a docker volume. The bash entrypoint defaults it to `.secrets`; the binary does not |
| `LOK8S_SERVICE_CONFIG` | (empty) | Service config name for override merging |
| `DEBUG` | (empty) | Enable debug output when non-empty |
| `LO_MCP_ALLOW` | (empty) | `mutating` or `destructive`: the environment form of `lo mcp`'s `--allow-*` opt-ins |
| `LO_ASSETS_EJECT` | (empty) | `never`: the environment form of `--no-eject`; embedded framework assets are served from a temp dir, never written into the project (see [`lo assets`](#lo-assets)). The bash tree cache under `XDG_CACHE_HOME` is outside the project and stays in use |
| `XDG_CACHE_HOME` | `$HOME/.cache` | Where the binary extracts the embedded bash tree (`lok8s/<version>/lok8s/`) for the provider plugins and `lo drivers <name>` when the project holds no tree. A command routed to bash never runs from it |
| `LOK8S_NONINTERACTIVE` | (empty) | `1` disables prompts (consent gates refuse) and the collapsing progress UI |
| `ARGSH_BUILTIN_PATH` | (auto-detected) | Full path to `argsh.so`. Only the argsh `mcp` builtin needs it: the bash MCP variant and `lo chat` |
