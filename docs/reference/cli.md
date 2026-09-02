# CLI Reference

The `lo` CLI is a single static Go binary. Every command below runs natively in it; the [argsh](https://github.com/arg-sh/argsh) implementation it was ported from stays in every project at `.lok8s/lo` as a frozen reference and runs the same command line when you set `LO_IMPL=bash` (see [The Go `lo` binary](go-migration.md) for what still calls into that tree, and the catalogue of the few places the two deliberately differ — for example, argument-parse errors exit `1` in the binary where argsh exits `2`, with the same message).

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

## Commands

### lo up

Start a cluster with Tilt.

```bash
lo up [--open-tilt|-o] [--remote|-r]
```

If `clusters/<domain>/cluster.lok8s.yaml` exists, uses the provision dispatch system. Otherwise falls back to legacy direct kind/registry calls.

Steps: provision cluster, apply `spec.bootstrap` addons in order via the framework bootstrap (`.lok8s/libs/bootstrap`), start Tilt.

With `--remote`: provisions a VM via `spec.provider`, then runs kind on the remote Docker host. See [Remote clusters](#remote-clusters) below.

| Flag | Description |
|------|-------------|
| `--open-tilt`, `-o` | Open the Tilt UI in a browser after startup |

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
lo init service <name> [--path <dir>] [--force]
lo init test [--path <dir>] [--force]
```

**`lo init service <name>`** scaffolds a bare per-service `lok8s.yaml` (shaped to pass the per-service validator), registers it in the project-root `services.yaml`, and ensures the project Tiltfile is the canonical 2-line loader.

**`lo init test`** scaffolds a domain-parameterized [Playwright](https://playwright.dev) integration suite into `tests/` (default; override with `--path`). The generated suite is project- and domain-agnostic: it runs the SAME specs against your dev cluster, staging, and production by changing only `LOK8S_TEST_DOMAIN`. See [Testing](../guide/testing.md). It refuses to overwrite a non-empty directory unless `--force` (and even then copies file-by-file, preserving local additions).

| Flag | Description |
|------|-------------|
| `--path`, `-p` | Target directory (service dir / `tests/` dir) |
| `--force`, `-f` | Overwrite existing files / non-empty target |

### lo use

Set or show the active domain.

```bash
lo use [domain]
```

Without arguments: shows the active domain and lists all available domains with their kind types. With a domain argument: validates the domain directory exists and writes it to `clusters/.active`.

### lo lint

Validate domain structure and specs.

```bash
lo lint [--domain <domain>]
```

Checks:
- Each domain has `cluster.lok8s.yaml` or `deploy.lok8s.yaml`
- Each `spec.bootstrap` entry resolves to an existing driver addon directory or user path
- Kustomization files under `targets/` reference existing resources
- Secrets: committed encrypted (`.enc` present and current), and no per-domain secret is shadowed in the deprecated flat `.secrets/` store (identical copy = stale duplicate; differing copy = active drift)

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
lo secrets set --name <n> --namespace <ns> <key> [value]   # write a value (omitted: tty prompt / piped stdin; `-`: stdin, needs argsh with arg-sh/argsh#176)
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

::: info The shipped `.mcp.json` still launches the argsh builtin
The `.mcp.json` at the project root points at `.lok8s/lo mcp` — the previous, argsh-native server, which needs `argsh.so` (`argsh builtins install`; discoverable via `ARGSH_BUILTIN_PATH`, `PATH_BIN/argsh.so`, `BASH_LOADABLES_PATH` or `LD_LIBRARY_PATH`). It keeps working. To use the binary's server instead, run `lo mcp <editor> enable` or point your client at `lo mcp start`; the switch of the shipped file is a deliberate, separate change.
:::

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
lo doctor
```

Checks required binaries, versions, Docker/kind state, and common misconfigurations, with a fix hint per finding.

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
lo addons [name]
```

### lo drivers

Driver-specific commands.

```bash
lo drivers --list             # list available drivers
lo drivers <name> <args…>     # invoke a driver's own subcommands
```

`--list` prints the union of the Go driver registry (`lo`, `capi`, `kubeone`, `kkp`, `kubehz`) and the driver directories under `.lok8s/drivers/`. A name that exists only as a bash driver is handed to the argsh implementation with the arguments untouched; `--help` after a nested command (`lo drivers lo status --help`) reaches that command, where argsh printed the `drivers` usage instead.

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
lo kubehz re-enroll           # re-enroll a regenerated agent token (heartbeats resume)
lo kubehz join                # mint a node join ticket (hosting: shared)
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
lo kustomize test             # plugin unit + integration tests
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

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOK8S_CLUSTER_NAME` | `local` | Cluster name |
| `KIND_NODE_VERSION` | `v1.31.12@sha256:...` | Kind node image |
| `KIND_CONFIG` | `.lok8s/drivers/lo/cluster/config.yaml` | Kind config file |
| `DOMAIN_NAME` | (empty) | Domain override. Full precedence: `--domain` flag > `DOMAIN_NAME` env > `clusters/.active` > `lok8s.dev`. When the env var and `.active` disagree, `lo` prints a one-line notice naming which won |
| `DOMAIN_SANS` | `*` | Domain SANs |
| `KIND_EXPERIMENTAL_DOCKER_NETWORK` | `lok8s` | Docker network name |
| `PATH_SECRETS` | `.secrets` | Active domain's store: `lo build`/`lo deploy` set it to `clusters/<domain>/secrets`; `.secrets` only with no domain context |
| `LOK8S_SERVICE_CONFIG` | (empty) | Service config name for override merging |
| `DEBUG` | (empty) | Enable debug output when non-empty |
| `LO_IMPL` | (empty) | `bash` runs the frozen argsh implementation (`.lok8s/lo`) for this invocation instead of the binary — see [The Go `lo` binary](go-migration.md#lo-impl-bash-the-escape-hatch) |
| `LO_MCP_ALLOW` | (empty) | `mutating` or `destructive`: the environment form of `lo mcp`'s `--allow-*` opt-ins |
| `LOK8S_NONINTERACTIVE` | (empty) | `1` disables prompts (consent gates refuse) and the collapsing progress UI |
| `ARGSH_BUILTIN_PATH` | (auto-detected) | Full path to `argsh.so` — needed only by the argsh `mcp` builtin the shipped `.mcp.json` launches |
