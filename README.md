<h3 align="center">lok8s</h3>

<h6 align="center">
  <a href="#-install">Install</a>
  ·
  <a href="#-quick-start">Quick Start</a>
  ·
  <a href="#-cli-reference">CLI Reference</a>
  ·
  <a href="https://kernpilot.github.io/lok8s/">Documentation</a>
</h6>

<p align="center">
  <a href="https://github.com/kernpilot/lok8s/stargazers">
    <img src="https://img.shields.io/github/stars/kernpilot/lok8s?style=for-the-badge&logo=starship&color=C9CBFF&logoColor=D9E0EE&labelColor=302D41" alt="Stars" />
  </a>
  <a href="https://github.com/kernpilot/lok8s/releases/latest">
    <img src="https://img.shields.io/github/v/release/kernpilot/lok8s?style=for-the-badge&logo=github&color=F2CDCD&logoColor=D9E0EE&labelColor=302D41" alt="Release" />
  </a>
  <a href="https://github.com/kernpilot/lok8s/actions/workflows/ci.yml">
    <img src="https://img.shields.io/github/actions/workflow/status/kernpilot/lok8s/ci.yml?style=for-the-badge&logo=github-actions&color=89DCEB&logoColor=D9E0EE&labelColor=302D41" alt="CI" />
  </a>
  <a href="https://github.com/kernpilot/lok8s/blob/main/LICENSE">
    <img src="https://img.shields.io/github/license/kernpilot/lok8s?style=for-the-badge&logo=open-source-initiative&color=ABE9B3&logoColor=D9E0EE&labelColor=302D41" alt="License" />
  </a>
</p>

&nbsp;

<p align="left">
Kubernetes deployment framework distributed as a <a href="https://github.com/fentas/b">b</a> environment.
One CLI, one folder convention, same workflow from local dev to production. Domain-driven cluster management
with pluggable drivers, ordered cluster-infrastructure bootstrap, and per-target workloads.
</p>

&nbsp;

### 📦 Install

```bash
# Install b (environment manager)
curl -fsSL https://raw.githubusercontent.com/fentas/b/master/install.sh | bash

# Most users: local dev (kind + Tilt + kustomize plugins)
b env add github.com/kernpilot/lok8s#local

# Sync into your project
b sync
```

**Available profiles** (each ships only the binaries it needs):

| Profile | Includes | Use case |
|---------|----------|----------|
| `core` | — | Remote deploy only — framework + default `lok8s.dev` domain, no kind/Tilt |
| `kustomize` | — | Kustomize plugins (standalone) |
| `local` | core + kustomize | **Local dev** — adds kind, Tilt, mkcert |
| `capi` | local | Cluster API provisioning (adds `clusterctl`, `hcloud`) |
| `kubeone` | local | KubeOne provisioning (adds `kubeone`, `hcloud`) |

**Default domain**: every profile ships `clusters/lok8s.dev/` — a preconfigured local-bridge cluster with TLS that works out of the box. Bring your own FQDN later, or use `*.[N].lok8s.dev` for multiple projects.

See [env-sync docs](https://binary.help/env-sync/) for profile mechanics.

**Prerequisites**: Docker. (`b sync` installs the rest based on your profile.)

&nbsp;

### 🐾 Quick Start

```bash
# Set the active domain
lo use lok8s.dev
# Active domain: lok8s.dev

# Start local cluster + Tilt
lo up
# [debug] Provisioning lok8s.dev with kind=lo
# [debug] Creating Docker network lok8s (10.125.125.0/24)
# [debug] Starting registry mirrors...
# [debug] Creating kind cluster: local
# [debug] Deploying target: networking
# Tilt started on http://localhost:10350

# Check cluster status
lo status
# Running

# Build kustomize targets
lo build
# [debug] Building target: networking
# [debug] Built networking -> clusters/lok8s.dev/artifacts/networking/artifacts.yaml

# Deploy artifacts
lo deploy
# [debug] Deploying target: networking
# [debug] Applying CRDs for networking
# [debug] Applying resources for networking

# Tear it down
lo down
```

&nbsp;

### 🧠 Architecture

lok8s separates two concerns: **cluster creation** (operator / CLI) and **cluster content** (kustomize targets). The framework lives under `.lok8s/` (drivers, libs, Tilt extension); user cluster definitions live under `clusters/`.

Everything is keyed by FQDN:

- **Cluster domain** — has `cluster.lok8s.yaml`, owns a cluster
- **Deployment domain** — has `deploy.lok8s.yaml`, deploys to another domain's cluster

```
.lok8s/                    # framework (managed, flat tree)
  lo                       # CLI entrypoint
  libs/                    # shared bash libraries
  utils/                   # helpers (ip, types, verbose, ...)
  drivers/                 # cluster-architecture drivers
    lo/                    #   kind-based local/CI
    capi/                  #   Cluster API
    kubeone/               #   KubeOne
    kkp/                   #   Kubermatic KKP
  providers/               # physical infra providers (hetzner, ...)
  tilt/                    # Tilt extension

clusters/                  # user content
  lok8s.dev/               # local dev (cluster domain)
    cluster.lok8s.yaml
    targets/platform/
    artifacts/platform/    # (gitignored)
  example.com/             # production (cluster domain)
  api.example.com/         # deployment domain → example.com
```

**Driver contract** — pluggable cluster backends via `.lok8s/drivers/<kind>/main`:

| Kind | Purpose | Runtime |
|------|---------|---------|
| **Lo** | Local / CI clusters | Docker + kind |
| **Capi** | Production clusters | Cluster API (Hetzner, AWS) |
| **KubeOne** | Production clusters | KubeOne (Hetzner, AWS, etc.) |

**`spec.bootstrap`** — ordered list of cluster-infrastructure addons (CNI, CSI, MetalLB, cert-manager CRDs, ...) applied by the driver at provision time, before any workloads land. See [Concepts](docs/guide/concepts.md#two-deployment-planes) and [Addons](docs/guide/addons.md).

&nbsp;

### 🔧 CLI Reference

| Command | Description |
|---------|-------------|
| `lo up [--open-tilt]` | Provision cluster + start Tilt |
| `lo down` | Stop Tilt + delete cluster |
| `lo clean [--all]` | Clean volumes, optionally prune Docker |
| `lo provision [domain]` | Full lifecycle: create + build + deploy + GitOps |
| `lo build [domain] [target...]` | Build kustomize targets into artifacts (per-target) |
| `lo deploy [--filter k=v] [domain] [target...]` | Deploy built artifacts per-target |
| `lo addons [name]` | List or inspect driver addons |
| `lo destroy [domain]` | Tear down a cluster |
| `lo use [domain]` | Set/show active domain |
| `lo lint [domain]` | Validate structure and specs |
| `lo status [domain]` | Cluster health and status |
| `lo gitops flux\|argo [domain]` | Generate GitOps ordering layer |
| `lo kind network\|create\|delete\|kubeconfig` | Manage kind cluster directly |
| `lo tilt up\|down\|status\|restart` | Manage Tilt environment |
| `lo registry up\|down\|status\|clean` | Manage registry mirrors |
| `lo mcp` | Start MCP tool server (stdio) |
| `lo env services\|kustomization\|secrets` | Environment and service config |
| `lo manifest list\|addons\|kubernetes\|generate` | Cluster manifest management |
| `lo k8s capi\|infrastructure\|platform` | K8s artifact generation |

Global flags: `--verbose|-v`, `--force|-f`, `--cluster|-s`, `--kubernetes`, `--config`, `--domain-name`, `--domain-sans`

&nbsp;

### 🤖 MCP Integration

lok8s exposes all CLI commands as [MCP](https://modelcontextprotocol.io/) tools via argsh builtins. AI agents (Claude Code, Cursor, etc.) can call `lo up`, `lo deploy`, `lo status`, etc. over stdio JSON-RPC.

Tool annotations (`@readonly`, `@destructive`, `@idempotent`) inform the AI client about each command's behavior. Only leaf commands are exposed as tools -- dispatchers like `tilt` and `env` are traversed but not exposed.

**Setup:**

```bash
# Install argsh native builtins (required for MCP)
argsh builtins install

# Copy argsh.so into .bin/ so the lo runtime finds it
cp "$(argsh builtins install 2>&1 | grep -oP 'installed to \K.*')" .bin/argsh.so

# Test it
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | lo mcp
```

**Configure your AI client** — the `.mcp.json` is included in the project root.

Leaf subcommands become MCP tools (e.g. `lo_status`, `lo_build`, `lo_tilt_up`, `lo_env_services`). Dispatchers are traversed, not exposed.

&nbsp;

### 🏗️ Structure

The framework (code, drivers, Tilt extension) lives under `.lok8s/`. User
cluster definitions live under `clusters/` — separate top-level tree so
`.lok8s/` can stay flat, framework-owned, and override-free.

```
.lok8s/                          # everything the framework ships (flat tree)
  lo                             # CLI entrypoint (argsh)
  libs/                          # shared bash libraries
    provision, build, deploy, gitops, kubehz, tilt, secrets, k8s,
    manifest, lint, env, status, plugins, image, addons, ...
  utils/                         # shared helpers (verbose, types, ip, template)
  drivers/                       # cluster-architecture drivers (driver contract)
    lo/                          #   Lo driver (local/CI via kind)
      main                       #     contract entrypoint
      addons/                    #     driver-shipped addons (cni, metallb, ...)
      cluster/                   #     runtime templates (kind config, registries, ...)
    capi/                        #   Cluster API driver
    kubeone/                     #   KubeOne driver
    kkp/                         #   Kubermatic KKP driver
  providers/                     # physical infra providers used by drivers
    hetzner/                     #   Hetzner cloud-init engine
  tilt/                          # Tilt extension
    Tiltfile                     #   the lok8s() extension function

clusters/                        # user content (one dir per cluster FQDN)
  lok8s.dev/                     # default local dev cluster
    cluster.lok8s.yaml           #   cluster spec
    targets/                     #   kustomize source directories
    artifacts/                   #   built output (gitignored)
    .kubeconfig/                 #   runtime kubeconfigs (gitignored)
  example.com/                   # production cluster
    cluster.lok8s.yaml
  api.example.com/               # deployment domain → example.com
    deploy.lok8s.yaml

# Top-level (the user's project stuff)
Tiltfile                         # Bootstrap: load('./.lok8s/tilt/Tiltfile', 'lok8s')
services.yaml                    # Service definitions (apiVersion: services.lok8s.dev/v1)
.envrc                           # direnv: PATH_BASE, PATH_LOK8S, PATH_CLUSTERS, ...
.bin/b.yaml                      # Binary dependencies (managed by b)

# Kustomize plugins (Go source + binary discovery)
kustomize/                       # Go source for kustomize plugins
  cmd/secret/                    # secrets.lok8s.dev/v1/Secret entrypoint
  plugins/secret/                # Plugin-specific spec types and generators
  pkg/                           # Shared infrastructure (cache, random, ...)
.kustomize/                      # Plugin discovery layout (KUSTOMIZE_PLUGIN_HOME)
  secrets.lok8s.dev/v1/secret/Secret   # Built binary

# Operator (separate from kustomize plugins)
operator/
  crds/                          # Lo + Capi (reconciled) + Deploy (definition)
  hooks/                         # shell-operator bash hooks
  deploy/                        # Operator deployment manifests
  Dockerfile
```

&nbsp;

### 🐳 Operator

The lok8s operator runs on management clusters using [shell-operator](https://github.com/flant/shell-operator). It reconciles the `Lo` and `Capi` CRDs using the same bash libraries as the CLI. (KubeOne and Kkp are CLI-only drivers, not operator-reconciled.)

```bash
# Apply CRDs
kubectl apply -f operator/crds/

# Deploy operator
kubectl apply -k operator/deploy/

# Create a local cluster
kubectl apply -f - <<EOF
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
  runtime: kind
  bootstrap:
    - cni
    - metallb
EOF

# Check status
kubectl get lo
# NAME    PHASE         READY   DOMAIN      AGE
# local   Provisioned   true    lok8s.dev   5m
```

CRDs: `Lo` (local/CI), `Capi` (production via CAPI), `Deploy` (deployment domains).

&nbsp;

### 🤖 MCP Integration

The `lo` CLI doubles as an [MCP](https://modelcontextprotocol.io/) tool server. Every leaf subcommand — `up`, `down`, `build`, `deploy`, `status`, and more — is exposed as a callable tool over stdio. Dispatchers (`tilt`, `env`, `k8s`, `gitops`) are traversed but not exposed as tools; only their leaf commands appear (e.g. `lo_tilt_up`, `lo_env_services`).

Commands carry tool annotations that inform the AI client:

| Annotation | Meaning | Example commands |
|-----------|---------|-----------------|
| `@readonly` | Safe to auto-run, no side effects | `status`, `lint`, `env services` |
| `@destructive` | May modify/destroy resources | `up`, `down`, `destroy`, `deploy` |
| `@idempotent` | Safe to retry | `build`, `deploy`, `gitops flux` |

**Requirements**: The [argsh native builtin](https://github.com/arg-sh/argsh) must be installed:

```bash
argsh builtins install
# Downloads argsh.so for your platform
```

**Test it**:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./lo mcp
# Returns JSON with tool definitions for all lo leaf subcommands
```

**Configure your AI client** using the `.mcp.json` in the project root:

```json
{
  "mcpServers": {
    "lok8s": {
      "type": "stdio",
      "command": ".lok8s/lo",
      "args": ["mcp"],
      "env": {
        "PATH_BASE": ".",
        "PATH_BIN": ".bin"
      }
    }
  }
}
```

The `PATH_BIN` directory must contain `argsh.so` (or a symlink to it) for the native builtin to load. If argsh.so is installed elsewhere, set `ARGSH_BUILTIN_PATH` to the full path of the `.so` file.

This works with Claude Code, VS Code Copilot, Cursor, and any client that supports MCP stdio servers.

&nbsp;

### 📚 Documentation

Full documentation is available at [kernpilot.github.io/lok8s](https://kernpilot.github.io/lok8s/).

```bash
# Local docs development
npm run docs:dev

# Build docs
npm run docs:build

# Preview built docs
npm run docs:preview
```

&nbsp;

### 🎯 Roadmap

- [x] CLI with argsh (lo script + libs)
- [x] Lo driver contract (kind + registries + CNI + mkcert)
- [x] Domain-driven .lok8s/ structure
- [x] `spec.bootstrap` — ordered cluster-infra addons with health waits
- [x] Per-target kustomize build pipeline
- [x] Tilt extension for local dev
- [x] Capi driver contract (Hetzner provider)
- [x] CAPI template-based resource generation
- [x] GitOps integration (Flux CD + Argo CD)
- [x] shell-operator with CRDs (Lo, Capi, Deploy)
- [x] Secrets kustomize plugin
- [x] Management cluster bootstrap
- [x] lo lint validation
- [x] Remote/CI expose handling (nginx proxy)
- [x] AWS CAPI provider templates
- [x] MCP tool server (argsh native builtin)
- [ ] SaaS mode (kubehz managed clusters)
- [ ] Additional runtimes (k3s, talos)

&nbsp;

### 📜 License

[MIT](LICENSE)

&nbsp;

<p align="center">
  Copyright &copy; 2025 <a href="https://github.com/kernpilot">kernpilot</a>
</p>

<p align="center">
  <a href="https://github.com/kernpilot/lok8s/blob/main/LICENSE">
    <img src="https://img.shields.io/static/v1.svg?style=for-the-badge&label=License&message=MIT&color=ABE9B3&logoColor=D9E0EE&labelColor=302D41" alt="MIT License" />
  </a>
</p>
