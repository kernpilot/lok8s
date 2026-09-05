# Getting Started

lok8s is a Kubernetes deployment framework distributed as a [b](https://github.com/fentas/b) environment. It gives you a single CLI (`lo`), a single folder convention (`.lok8s/`), and the same workflow from local development to production.

## Prerequisites

**[Docker](https://docs.docker.com/get-docker/) is the only tool you
install yourself.**

Every other tool the framework needs ships **pinned inside the lok8s
environment**: [kind](https://kind.sigs.k8s.io/),
[kubectl](https://kubernetes.io/docs/tasks/tools/),
[kustomize](https://kubectl.docs.kubernetes.io/installation/kustomize/),
[yq](https://github.com/mikefarah/yq), [Tilt](https://tilt.dev/),
[mkcert](https://github.com/FiloSottile/mkcert), [Helm](https://helm.sh/),
and the rest of your profile's toolchain. A single command
(`b install`; see [Installation](#installation) below) lands them in your
project's `.bin/`. Nothing touches your system, and each project pins its
own versions. Teammates get the identical toolchain from the committed
`b.yaml`/`b.lock`. See [The Toolchain](/guide/toolchain) for the file's
schema.

## Installation

lok8s is distributed as a `b` environment with five profiles. Pick the one that matches your use case:

| Profile | Includes | Use case |
|---------|----------|----------|
| `core` | (none) | Remote deploy only (framework + `lok8s.dev` default domain, no kind/Tilt) |
| `kustomize` | (none) | Kustomize plugins only (standalone build artifacts) |
| `local` | core + kustomize | **Local dev**: kind + Tilt + mkcert on top of core |
| `capi` | local | Cluster API provisioning (adds `clusterctl`, `hcloud`) |
| `kubeone` | local | KubeOne provisioning (adds `kubeone`, `hcloud`) |

### 1. The `lo` binary

`lo` is a single static binary per platform (linux/darwin × amd64/arm64),
attached to every [GitHub release](https://github.com/kernpilot/lok8s/releases)
together with a `checksums.txt`. Download, verify, then run:

```bash
curl -fsSLO https://github.com/kernpilot/lok8s/releases/latest/download/lo-install.sh
curl -fsSLO https://github.com/kernpilot/lok8s/releases/latest/download/checksums.txt
sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
less lo-install.sh                            # read it first
bash lo-install.sh                            # → ~/.local/bin/lo
```

`lo-install.sh` fetches `lo-<os>-<arch>.tar.gz` **and** `checksums.txt` from
the release, verifies the archive's SHA-256, and only then installs `lo`.
Flags: `--version <tag>` (default: latest), `--dir <path>` (default
`~/.local/bin`), `--full` (the lo-full build, below), `--dry-run` (print the
plan, touch nothing).

::: tip Two builds: `lo` (core) and `lo-full`
`lo` — the default, ~50 MB — is the **core** build: the manifest render runs
the pinned `kustomize` binary plus the two exec generators (khelm's
`ChartRenderer`, the `secrets.lok8s.dev` Secret plugin) that
[`lo init toolchain`](#2-the-project-toolchain) installs with `b`.
`lo-full` (`--full`, ~120 MB) links the same kustomize API and khelm release
into the binary and renders in-process — it needs no `kustomize`, `khelm` or
`.kustomize/`. Both install as `lo`, both render byte-identical output (the
parity gate), and `lo --version` names the build. Start with core; switch to
full when you want the render self-contained. Details:
[Core and full](/reference/go-migration#core-and-full).
:::

Without the script, the same steps by hand:

```bash
V=v0.3.0; A=lo-linux-amd64.tar.gz             # your tag and platform
curl -fsSLO "https://github.com/kernpilot/lok8s/releases/download/${V}/${A}"
curl -fsSLO "https://github.com/kernpilot/lok8s/releases/download/${V}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf "${A}" lo && install -m 0755 lo ~/.local/bin/lo
```

### 2. The project toolchain

`lo` is the entrypoint, and every command runs inside the binary. What a
project still needs next to it is a pinned **toolchain** in `.bin/` — the
tools `lo` orchestrates (kubectl, kustomize and its two exec generators,
kind, Tilt, mkcert, …) — managed by [`b`](https://github.com/fentas/b).
`lo` provisions it for you:

```bash
mkdir my-project && cd my-project
lo init project        # clusters/, lok8s.yaml, .gitignore entries, .bin/b.yaml — then the toolchain
lo doctor              # b, kustomize, khelm and the Secret plugin verified against the pins
```

`lo init project` (and `lo init toolchain` on its own, for an existing
project) writes `.bin/b.yaml` from a template pinned to the `lo` you run:
kustomize `v5.8.1`, khelm `v2.8.0`, the `secrets.lok8s.dev` Secret plugin at
`lo`'s own version, plus kubectl (`core`), kind/Tilt/mkcert (`local`,
on by default) and kubeone/hcloud (`cloud`, opt-in with
`--groups core,local,cloud`). It then installs `b` itself into `.bin/` — the
pinned release tarball, downloaded over https and verified against the
SHA-256 the release publishes before anything is extracted; never
`curl | sh` — and runs `.bin/b install`. `--dry-run` prints every step;
`--no-toolchain` (on `lo init project`) writes the file and skips the
network. An existing `.bin/b.yaml` is never overwritten: you get a diff
against the template and `lo doctor` reports which pins differ. The
framework assets a cluster references (addons, driver templates) are
embedded in the binary and ejected into `.lok8s/` on first use — see
[`lo assets`](/reference/cli#lo-assets). [The Toolchain](/guide/toolchain)
documents the file.

::: details The profile path (framework tree + frozen bash reference)
The full `b` environment — the framework tree (`.lok8s/`: addons, driver
templates, provider plugins, the Tilt extension) and the frozen argsh
implementation the binary is diffed against (the
[Go migration reference](/reference/go-migration) has the map) — syncs from
the upstream repo as a profile:

```bash
# Install b if you haven't already — download, read, run
curl -fsSL https://get.binary.help -o b-install.sh
less b-install.sh
sh b-install.sh

# Add a profile (most users want local dev), then pull it into your project
b env add github.com/kernpilot/lok8s#local
b install
```
:::

This copies the framework tree (addons, driver templates, provider plugins, the frozen argsh CLI), the kustomize plugins, and (for `local`+) the Tilt extension into your project, and installs each profile's binaries — the `lo` release binary among them (`core` declares it, so `b install` fetches the same `lo-<os>-<arch>.tar.gz` asset into `.bin/`). Each profile ships only the binaries it actually needs; [The Toolchain](/guide/toolchain#what-b-manages-today-and-what-it-will) lists what that is today and what it will shrink to.

If you join a project that already uses lok8s, the toolchain declaration
is already in the repo. Clone and run a single command:

```bash
b install   # exact pinned toolchain from the committed b.yaml / b.lock
```

### Legacy (argsh) install

Before the Go binary, one self-contained argsh script (`lo-up`) did all of
the above — it installed `b`, added the profile and ran `b install`. It is
retired, not removed: the source and its build live under
`.lok8s/legacy/install/` in the repo, and the published bundle stays online
at `https://lok8s.io/lo-up` for existing projects. If you still use it,
download and read it before running it:

```bash
curl -fsSL https://lok8s.io/lo-up -o lo-up
less lo-up
sh lo-up            # -y: no prompts;  -p <profile>;  -r <git-ref>
```

### The default `lok8s.dev` domain

Every profile includes `clusters/lok8s.dev/`, a preconfigured cluster domain that works **out of the box** on a local Docker bridge with TLS. You do not need your own domain to get started: just `lo use lok8s.dev && lo up`.

You can also bring your own FQDN (`example.com`, `infra.example.net`, etc.) as an additional domain, or run multiple projects on `*.[1-100].lok8s.dev` subdomains. See [Concepts](/guide/concepts) for the FQDN convention.

## Project Structure After Sync

Everything lok8s ships lives under `.lok8s/`, a flat framework-owned
tree synced from upstream. Your cluster definitions live under
`clusters/`, one folder per FQDN. Your project's own files live at the
repo root alongside `Tiltfile` and `services.yaml`.

```
your-project/
  .bin/                        # pinned toolchain (b) — incl. the lo binary
  .lok8s/                      # framework (synced via b — don't edit)
    VERSION                    # what `lo --version` reports
    lo                         # frozen argsh CLI (LO_IMPL=bash runs it; the binary is the entrypoint)
    libs/, utils/              # frozen bash libraries (the reference the binary was ported from)
    legacy/                    # retired code (the argsh installer, the bash operator hooks)
    addons/                    # bootstrap addons (cilium, metallb, ...)
    drivers/                   # driver templates (kind config, CAPI templates) + frozen bash drivers
    providers/                 # infra providers (hetzner, ...) — bash plugins the binary runs
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

The `.lok8s/` and `.lok8s/tilt/` directories are framework code that
`b env-sync` syncs from upstream. To override or extend behavior,
prefer `services.local.yaml` (gitignored) or wrap the CLI in your
own script. If you modify the synced files directly, the next sync
overwrites your changes.

## Your First Cluster

### 1. Set the active domain

```bash
lo use lok8s.dev
```

### 2. Start the local cluster

```bash
lo up
```

This provisions a kind cluster with registry mirrors, CoreDNS, TLS certificates, and the `spec.bootstrap` addons (Cilium by default). Then it starts Tilt for live service development. You build and deploy workload targets with `lo build` and `lo deploy`.

### 3. Check status

```bash
lo status
```

### 4. Tear it down

```bash
lo down
```

## What's Next

- [Examples](https://github.com/kernpilot/lok8s/tree/main/examples): runnable, end-to-end-tested projects per driver (`lo`, `capi`, `capi-ha`, …) you can copy as a starting point
- [Concepts](/guide/concepts): domains, targets, bootstrap addons, the driver contract
- [Addons](/guide/addons): write and reference framework-local addons
- [Local Dev with Tilt](/guide/local-dev): configure services for live reload
- [CLI Reference](/reference/cli): all `lo` subcommands
