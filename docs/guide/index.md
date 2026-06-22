# Getting Started

lok8s is a Kubernetes deployment framework distributed as a [b](https://github.com/fentas/b) environment. It provides a single CLI (`lo`), a single folder convention (`.lok8s/`), and the same workflow from local development to production.

## Prerequisites

**[Docker](https://docs.docker.com/get-docker/) — that's it.** It is the
only tool you install yourself.

Everything else the framework needs — [kind](https://kind.sigs.k8s.io/),
[kubectl](https://kubernetes.io/docs/tasks/tools/),
[kustomize](https://kubectl.docs.kubernetes.io/installation/kustomize/),
[yq](https://github.com/mikefarah/yq), [Tilt](https://tilt.dev/),
[mkcert](https://github.com/FiloSottile/mkcert), [Helm](https://helm.sh/),
and the rest of your profile's toolchain — ships **pinned inside the lok8s
environment** and lands in your project's `.bin/` with a single command
(`b install`; see [Installation](#installation) below). Nothing touches
your system, versions are locked per project, and teammates get the
identical toolchain from the committed `b.yaml`/`b.lock`.

## Installation

lok8s is distributed as a `b` environment with five profiles. Pick the one that matches your use case:

| Profile | Includes | Use case |
|---------|----------|----------|
| `core` | — | Remote deploy only (framework + `lok8s.dev` default domain, no kind/Tilt) |
| `kustomize` | — | Kustomize plugins only (standalone build artifacts) |
| `local` | core + kustomize | **Local dev** — kind + Tilt + mkcert on top of core |
| `capi` | local | Cluster API provisioning (adds `clusterctl`, `hcloud`) |
| `kubeone` | local | KubeOne provisioning (adds `kubeone`, `hcloud`) |

The quickest path — one command bootstraps a lok8s project in the current directory (installs [`b`](https://github.com/fentas/b), pulls the framework plus your profile's pinned toolchain into `.bin/`, and drops a `lo-up` you can re-run to update):

```bash
curl -fsSL https://get.lok8s.io | sh
```

It prompts when a terminal is attached and runs unattended otherwise. Pass flags after `--`:

```bash
curl -fsSL https://get.lok8s.io | sh -s -- -y               # no prompts (CI)
curl -fsSL https://get.lok8s.io | sh -s -- -p kubeone -y    # a specific profile
```

Under the hood that is just `b` — do it by hand if you prefer:

```bash
# Install b if you haven't already
curl -fsSL https://get.binary.help | sh

# Add a profile (most users want local dev), then pull it into your project
b env add github.com/kernpilot/lok8s#local
b install
```

Either way this copies the CLI, libraries, driver contracts, kustomize plugins, templates, and (for `local`+) the Tilt extension into your project. Each profile ships only the binaries it actually needs.

Joining a project that already uses lok8s? Then the toolchain is already
declared — clone and run a single command:

```bash
b install   # exact pinned toolchain from the committed b.yaml / b.lock
```

### The default `lok8s.dev` domain

Every profile includes `clusters/lok8s.dev/` — a preconfigured cluster domain that works **out of the box** on a local Docker bridge with TLS. You don't need to bring your own domain to get started; just `lo use lok8s.dev && lo up`.

You can also bring your own FQDN (`example.com`, `infra.example.net`, etc.) as an additional domain, or run multiple projects on `*.[1-100].lok8s.dev` subdomains. See [Concepts](/guide/concepts) for the FQDN convention.

## Project Structure After Sync

Everything lok8s ships lives under `.lok8s/` — a flat, framework-owned
tree synced from upstream. Your cluster definitions live under
`clusters/`, one folder per FQDN. Your project's own files live at the
repo root alongside `Tiltfile` and `services.yaml`.

```
your-project/
  .lok8s/                      # framework (synced via b — don't edit)
    lo                         # CLI entrypoint (argsh script)
    libs/                      # shared bash libraries
    utils/                     # shared helpers
    addons/                    # bootstrap addons (cilium, metallb, ...)
    drivers/                   # cluster drivers (lo, capi, kubeone, kkp)
    providers/                 # infra providers (hetzner, ...)
    tilt/                      # Tilt extension
      Tiltfile                 # the lok8s() extension function
  clusters/                    # your cluster definitions
    lok8s.dev/                 # local dev domain (template)
      cluster.lok8s.yaml       # cluster spec
      targets/                 # kustomize targets
      artifacts/               # built output (gitignored)
  .kustomize/                  # kustomize plugin discovery (built binaries)
  Tiltfile                     # bootstrap: load('./.lok8s/tilt/Tiltfile', 'lok8s')
  services.yaml                # service definitions (your stuff)
  .envrc                       # direnv: PATH_BASE, PATH_LOK8S, PATH_CLUSTERS, ...
```

The `.lok8s/` and `.lok8s/tilt/` directories are framework code
synced from upstream by `b env-sync`. To override or extend behavior,
prefer `services.local.yaml` (gitignored) or wrapping the CLI in your
own script — modifying the synced files directly will be overwritten on
the next sync.

## Your First Cluster

### 1. Set the active domain

```bash
lo use lok8s.dev
```

### 2. Start the local cluster

```bash
lo up
```

This provisions a kind cluster — registry mirrors, CoreDNS, TLS certificates, and the `spec.bootstrap` addons (Cilium by default) — then starts Tilt for live service development. Workload targets are built and deployed with `lo build` and `lo deploy`.

### 3. Check status

```bash
lo status
```

### 4. Tear it down

```bash
lo down
```

## What's Next

- [Concepts](/guide/concepts) — domains, targets, bootstrap addons, the driver contract
- [Addons](/guide/addons) — write and reference framework-local addons
- [Local Dev with Tilt](/guide/local-dev) — configure services for live reload
- [CLI Reference](/reference/cli) — all `lo` subcommands
