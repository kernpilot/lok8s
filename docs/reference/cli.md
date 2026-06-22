# CLI Reference

The `lo` CLI is an [argsh](https://github.com/arg-sh/argsh) script located at `.lok8s/lo`.

`lo up` runs provision → framework bootstrap (applies `spec.bootstrap` addons via `.lok8s/libs/bootstrap`) → Tilt. `lo build` and `lo deploy` iterate targets alphabetically with no framework-level ordering. `lo lint` validates `spec.bootstrap` entries and target kustomizations. See [Concepts](../guide/concepts.md) and [Specs reference](specs.md) for the model.

## Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--verbose` | `-v` | Enable verbose/debug logging (sets `DEBUG=1`) |
| `--force` | `-f` | Force operation without prompts |
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

Stops Tilt and deletes the kind cluster. Registries are handled by their sharing
mode: a **shared** setup (the default) is left running — the pull-through mirrors
are reused across clusters and a warm `build`/`cache` speeds up the next `lo up`
(remove them with `lo registry down`, or `lo registry clean --shared` to drop
volumes too); a **non-shared** setup is project-local with nothing to reuse, so
its registry containers are torn down (the named volumes — and thus the build
cache — are kept).

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
lo provision [--domain <domain>] [--remote|-r]
```

Resolves the cluster spec, sources the driver contract, calls `driver::provision`, then runs `bootstrap::apply` to apply `spec.bootstrap` addons in order with health waits between stages.

With `--remote`: loads `spec.provider`, provisions the cloud VM, then either sets `DOCKER_HOST` to the remote Docker (docker mode) or syncs the repo and runs `lo provision` on the VM (CI mode). See [Remote clusters](#remote-clusters).

### lo bootstrap

Apply or re-apply bootstrap addons without full re-provisioning.

```bash
lo bootstrap [--domain <domain>]
```

Reads `spec.bootstrap` from the cluster spec and applies each addon in order. Useful after changing bootstrap entries or updating addon values. Addons resolve to `.lok8s/addons/<name>/` (framework) or `clusters/<domain>/targets/<path>` (cluster-specific). See [Bootstrap Addons](../guide/addons.md).

### lo build

Build kustomize targets into artifacts.

```bash
lo build [--split] [domain] [target...]
```

Runs `kustomize build --enable-alpha-plugins` for each target. Output goes to `clusters/<domain>/artifacts/<target>/artifacts.yaml`.

| Flag | Description |
|------|-------------|
| `--split` | Split output into individual `<Kind>.<namespace>.<name>.yaml` files |

### lo deploy

Deploy artifacts to a cluster.

```bash
lo deploy [--filter key=value] [domain] [target...]
```

Applies per-target artifacts. Targets are discovered from `clusters/<domain>/targets/<name>/` alphabetically, or supplied explicitly. Each target: CRDs first, then resources, then wait for health.

| Flag | Description |
|------|-------------|
| `--filter` | Label filter (e.g. `type=system`), matches `lok8s.dev/<key>` labels |

### lo destroy

Destroy a cluster.

```bash
lo destroy [domain]
```

Calls `driver::destroy` from the appropriate driver contract.

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
lo lint [domain]
```

Checks:
- Each domain has `cluster.lok8s.yaml` or `deploy.lok8s.yaml`
- Each `spec.bootstrap` entry resolves to an existing driver addon directory or user path
- Kustomization files under `targets/` reference existing resources

### lo status

Check cluster health and status.

```bash
lo status [domain]
```

Delegates to the driver contract's `driver::status` function. For Lo clusters: checks if the kind cluster exists. For Capi clusters: queries the CAPI Cluster resource phase.

### lo gitops

GitOps integration (Flux / Argo). **Deferred.** Both subcommands currently return a deferred-error stub — the integration is being redesigned around the new `services.yaml` targets-map model.

```bash
lo gitops flux [domain]    # (deferred)
lo gitops argo [domain]    # (deferred)
```

### Cluster lifecycle (there is no `lo kind`)

The kind cluster is managed by the lifecycle commands — there is no `lo kind`
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

Registries are derived entirely from `spec.registries` (the `mirrors[]` plus the framework-private `build` and `cache` registries) — there are no per-registry flags; the only flag is `--shared`/`-S`, which includes the shared `lok8s-registries` network for `status`/`clean`. Registries run on the configured Docker bridge network (default: `lok8s` at `10.125.125.0/24` for slot 125); IPs are computed automatically by the driver from `spec.network.cidr` and `spec.registries.shared.network.cidr`. See [Specs reference](specs#registries-configuration).

The default 6-registry set:

| Name | Default IP | Hostname | Purpose |
|------|------------|----------|---------|
| `build` | `10.125.125.101` | `lok8s.local` | Tilt push target for locally-built images |
| `cache` | `10.125.125.102` | `lok8s.cache` | Pre-pull target for `build:false` services with a remote registry |
| `io-docker` | `10.125.200.2` | `docker.io` | Pull-through mirror (shared network) |
| `io-quay` | `10.125.200.3` | `quay.io` | Pull-through mirror (shared network) |
| `io-k8s` | `10.125.200.4` | `registry.k8s.io` | Pull-through mirror (shared network) |
| `io-ghcr` | `10.125.200.5` | `ghcr.io` | Pull-through mirror (shared network) |

`build` and `cache` always live on the project subnet (`.101`/`.102`).
With `spec.registries.shared.enabled: true` (the default) mirrors live on
the shared `lok8s-registries` network so multiple projects reuse one
cache; with `enabled: false` they move onto the project subnet
(`.103`+).

### lo image

Manage the local cache registry — pre-pull private/CI images so kind can fetch them without upstream credentials.

```bash
lo image cache <service> [--force|-f]   # Pre-pull a single service's image
lo image cache --all [--force|-f]       # Drain the queue from `lo env kustomization`
lo image list                            # Show what's currently in the cache registry
lo image clean                           # Drop the cache registry volume
```

The cache flow runs automatically as part of `lo build` / `lo up` when any service has `build: false` and a resolved `registry.endpoint`. See [Services Configuration → Cache mode](/guide/services#cache-mode-the-lok8scache-registry) for the full pipeline. Parallelism is controlled via `registry.parallel` in `services.yaml` (`0` unlimited, `1` sequential default, `N≥2` bounded).

### lo env

Manage environment and service configuration.

```bash
lo env services [--only-services|-s] [--only-registry|-r]
lo env kustomization [--no-build|-n] [--pull|-p]
```

**services**: Prints merged service config (services.yaml + overrides).

**kustomization**: Generates `kustomization.yaml` with image references from registry config inside `clusters/<domain>/artifacts/`. Writes a `.cache-queue` TSV file alongside listing every `build:false` service that needs a pre-pull. The `--pull` flag drains that queue immediately by invoking `lo image cache --all` (otherwise the queue is just written and a separate consumer drains it later — Tilt does this automatically via `lok8s()`'s `auto_cache_pull` kwarg, CI does it explicitly).

### lo secrets

Manage the secret cache (`$PATH_SECRETS`) and its optional SOPS/age encryption
(`lo secrets list` / `print` inspect it). See the [Secrets guide](/guide/secrets)
for the full workflow.

```bash
lo secrets init                                # set up SOPS/age from your SSH key
lo secrets set --name <n> --namespace <ns> <key> <value>   # write a literal value
lo secrets allow                               # approve bash: generators after a change
lo secrets encrypt                             # write committable Secret.*.enc files
lo secrets decrypt                             # restore the plaintext cache from .enc
lo secrets list | print [pattern...] | path    # inspect the cache
```

**Encryption**: `init` derives an age recipient from `~/.ssh/id_ed25519` (via
`ssh-to-age`, ed25519 only) and writes `.sops.yaml`; `encrypt`/`decrypt`
round-trip the cache so secrets commit safely as `Secret.*.enc`. Needs `sops`
and `ssh-to-age` (`b install`).

### lo k8s

Generate and render Kubernetes artifacts.

```bash
lo k8s capi [--spec path] [--out path]   # Generate CAPI resources
lo k8s infrastructure                      # Build infrastructure artifacts
lo k8s platform                            # Build platform artifacts
```

### lo mcp

Start an MCP (Model Context Protocol) tool server over stdio.

```bash
lo mcp
```

Exposes every leaf `lo` subcommand as a callable tool via the [MCP protocol](https://modelcontextprotocol.io/). AI clients (Claude Code, VS Code Copilot, Cursor) connect over stdio and can invoke `up`, `down`, `build`, `deploy`, `status`, and all other commands programmatically. Dispatchers (`tilt`, `env`, `k8s`, `gitops`) are traversed but not exposed -- only their leaf commands appear as tools.

Commands carry tool annotations that inform the client about behavior:

| Annotation | MCP hint | Effect |
|-----------|----------|--------|
| `@readonly` | `readOnlyHint: true` | Client may auto-run without confirmation |
| `@destructive` | `destructiveHint: true` | Client shows confirmation dialog |
| `@idempotent` | `idempotentHint: true` | Client knows retries are safe |

**Requires** the argsh native builtin (`argsh.so`). Install it with:

```bash
argsh builtins install
```

The `.so` must be discoverable via one of: `ARGSH_BUILTIN_PATH`, `PATH_BIN/argsh.so`, `BASH_LOADABLES_PATH`, or `LD_LIBRARY_PATH`.

Configure your AI client using the `.mcp.json` included in the project root.

## Remote Clusters

The `--remote` flag enables provisioning Lo clusters on remote VMs
instead of the local Docker host. It requires `spec.provider` and
optionally `spec.remote` in the cluster spec.

### Two modes

**Docker mode** (default): The local machine orchestrates everything —
kind, registries, bootstrap — but Docker commands target the remote VM
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
   the remote handled everything — `libs/provision` skips local bootstrap

### Without `--remote`

Without `--remote`, `spec.provider` and `spec.remote` are ignored. The
same cluster spec works for both local and remote provisioning — the
mode is driven by the caller, not the file.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOK8S_CLUSTER_NAME` | `local` | Cluster name |
| `KIND_NODE_VERSION` | `v1.31.12@sha256:...` | Kind node image |
| `KIND_CONFIG` | `<cluster_path>/config/kind-cluster.yaml` | Kind config file |
| `DOMAIN_NAME` | `lok8s.dev` | Domain name |
| `DOMAIN_SANS` | `*` | Domain SANs |
| `KIND_EXPERIMENTAL_DOCKER_NETWORK` | `lok8s` | Docker network name |
| `PATH_SECRETS` | `.secrets` | Global secrets path |
| `LOK8S_SERVICE_CONFIG` | (empty) | Service config name for override merging |
| `DEBUG` | (empty) | Enable debug output when non-empty |
| `ARGSH_BUILTIN_PATH` | (auto-detected) | Full path to `argsh.so` for MCP support |
