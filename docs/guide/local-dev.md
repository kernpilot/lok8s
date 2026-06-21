# Local Development with Tilt

lok8s integrates with [Tilt](https://tilt.dev/) for local development with live reload, port forwarding, and automatic rebuilds.

> Looking for the full `services.yaml` and `lok8s.yaml` schema, the
> small-vs-large project workflow, or how to mix local builds with
> CI-pulled registry images? See [Services Configuration](/guide/services).

## Quick Start

```bash
lo use lok8s.dev
lo up
```

This provisions a local kind cluster with registry mirrors, then starts Tilt in the background. The Tilt UI is available at `http://localhost:10350`.

To also open the Tilt UI automatically:

```bash
lo up --open-tilt
```

## How It Works

The root `Tiltfile` loads the lok8s Tilt extension:

```python
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s()
```

The extension:

1. Runs `lo env kustomization` to generate `kustomization.yaml` and build artifacts
2. Runs `kustomize build clusters/<domain>/artifacts/` to produce the full manifest (no repo-root pollution)
3. Filters system resources (`lok8s.dev/type: system`) and applies them
4. Discovers services from `lo env services`
5. For each enabled service with `build: true`:
   - Filters Kubernetes artifacts by `lok8s.dev/name` label
   - Reads the per-service `lok8s.yaml` config
   - Sets up `docker_build` with `live_update`
   - Configures port forwarding and resource dependencies

## Service Configuration

### services.yaml

The committed base config defines which services exist and their registry settings:

```yaml
registry:
  prefix: lok8s.local
  branch: builds
  tag: latest

services:
  my-api:
    enabled: true
    build: true
    path: ./my-api
  my-frontend:
    enabled: true
    build: true
    path: ./my-frontend
```

### services.\<config\>.yaml

Personal overrides (gitignored). Set `LOK8S_SERVICE_CONFIG` to select which override to merge:

```bash
export LOK8S_SERVICE_CONFIG=dev
# merges services.yaml + services.dev.yaml
```

```yaml
# services.dev.yaml
services:
  my-frontend:
    enabled: false    # disable frontend locally
```

### Per-Service lok8s.yaml

Each service directory contains a `lok8s.yaml` that defines how Tilt builds it:

```yaml
# my-api/lok8s.yaml
build:
  dockerfile: lok8s.Dockerfile
  context: .
  live_update:
    fall_back_on:
      files:
        - package.json
    sync:
      - local_path: ./src
        remote_path: /app/src
    run:
      cmd: npm run dev
ports:
  - from: 3000
    to: 3000
links:
  - url: http://localhost:3000
    name: API
tilt:
  resource_deps:
    - postgres
  labels:
    - backend
```

## Dockerfile Convention

Each service should have two Dockerfiles:

| File | Purpose | Used by |
|------|---------|---------|
| `lok8s.Dockerfile` | Development — hot-reload enabled | Tilt (via `lok8s.yaml`) |
| `Dockerfile` | Production — optimized build | CI/CD, `lo deploy` |

The dev Dockerfile uses the runtime's native hot-reload mechanism. Tilt syncs files via `live_update`, and the process inside the container detects changes and reloads automatically — no container restarts needed.

### Hot-reload by runtime

**Bun:**
```dockerfile
CMD ["bun", "run", "--hot", "index.ts"]
```

**Node.js (nodemon):**
```dockerfile
CMD ["npx", "nodemon", "--watch", "src", "src/index.ts"]
```

**SvelteKit / Vite:**
```dockerfile
CMD ["bun", "run", "dev", "--host", "0.0.0.0", "--port", "3000"]
```

**Python (uvicorn):**
```dockerfile
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8500", "--reload"]
```

### fall_back_on

Files listed in `fall_back_on` trigger a **full Docker rebuild** instead of a live sync:

```yaml
fall_back_on:
  files:
    - package.json      # dependency changes
    - bun.lock          # lockfile changes
    - lok8s.Dockerfile  # Dockerfile changes
```

## Provisioning Flow

When `lo up` runs, the Lo driver performs these steps:

1. **Read config** from `cluster.lok8s.yaml` (applying defaults for
   `*.lok8s.dev` domains — see [Specs reference](/reference/specs#default-resolution))
2. **Validate IPs** — all registry IPs and MetalLB pool must be within their subnets
3. **Create docker network** with the configured CIDR
4. **Start registry containers** — the framework always ships `build` and `cache` on the project subnet, plus the configured pull-through mirrors on the shared network (default) or project subnet
5. **Render kind config** — nodes and containerd patches generated from spec
6. **Create kind cluster** with rendered config
7. **Create registry ConfigMap** (`lok8s-registries` in `kube-system`)
8. **Apply CoreDNS** config and patches — plus any `spec.coredns` from the
   cluster spec (see [Custom in-cluster DNS](#custom-in-cluster-dns))
9. **Apply bootstrap addons** — framework runs `.lok8s/libs/bootstrap` to apply `spec.bootstrap` addons in order (default: `[cilium]`), waits for health between each
10. **Start Tilt** for live development

> **TLS** is not minted by the driver. The gateway serves a
> [`cert:` Secret](/reference/kustomize-plugins#development-certificates-cert)
> you declare in your targets — a leaf signed by your shared dev CA at
> `CAROOT`, created on first build. Trust it once per machine with
> [`lo trust`](/guide/secrets) so browsers and `curl` accept `*.<domain>`.
> (Registry TLS, when `spec.registries.tls` is set, is minted the same way at
> provision time — see the [kind contract](/reference/kind-contract).)

## Registry Mirrors

The framework always ships two private registries (`build`, `cache`)
plus a default set of four public pull-through mirrors. All registries
listen on port `:80` and are reached via raw IP or their canonical
hostname (via containerd's `hosts.toml`).

| Registry | Default IP (slot 125) | Hostname | Purpose |
|----------|------------------------|----------|---------|
| `build` | `10.125.125.101` | `lok8s.local` | Local build images (Tilt push target) |
| `cache` | `10.125.125.102` | `lok8s.cache` | `build: false` pre-pull target |
| `io-docker` | `10.125.200.2` | `docker.io` | Docker Hub pull-through cache |
| `io-quay` | `10.125.200.3` | `quay.io` | Quay pull-through cache |
| `io-k8s` | `10.125.200.4` | `registry.k8s.io` | Kubernetes images cache |
| `io-ghcr` | `10.125.200.5` | `ghcr.io` | GitHub Container Registry cache |

`build` and `cache` always live on the project subnet (`10.125.<slot>.0/24`)
at fixed offsets. Pull-through mirrors live on the shared
`lok8s-registries` network (`10.125.200.0/24`) by default, or on the
project subnet (`.103+`) when `spec.registries.shared.enabled: false`.

See the [Specs reference — Registries Configuration](/reference/specs#registries-configuration)
for the full schema.

A `lok8s-registries` ConfigMap in `kube-system` exposes registry IPs and URLs for in-cluster tooling.

## Load Balancer

When `spec.loadBalancer.pool` is configured, MetalLB is installed to provide LoadBalancer service support inside the kind cluster. This enables services like CoreDNS external to get real IPs on the docker bridge network.

```yaml
spec:
  loadBalancer:
    pool: "10.125.125.125-10.125.125.150"   # 26 IPs from slot 125's MetalLB range
```

## Custom in-cluster DNS

Declare custom CoreDNS in the cluster spec; `lo up` renders it into a
`coredns-custom` ConfigMap that the base Corefile imports from
`/etc/coredns/custom`. **Declarative and committed** — it survives a recreate,
unlike a runtime `kubectl patch` of the `coredns` ConfigMap (which `lo up`
regenerates). A cluster with no `spec.coredns` is unaffected.

### Common case — resolve a zone to the gateway LB

In dev, `*.<domain>` is not real public DNS reachable from inside the cluster,
but operator-managed pods that can't take `hostAliases` (e.g. an API fetching an
OIDC discovery doc from `https://auth.<domain>/…`) still need to resolve it.
Declare the zone and a target; the driver writes the Corefile block for you
(`A → target`, `AAAA → NODATA` so dual-stack clients fall back to A cleanly):

```yaml
spec:
  coredns:
    hosts:
      - name: kubehz.dev      # the zone: its apex + every *.kubehz.dev
        target: gateway       # = the first loadBalancer.pool IP; or a literal IP
```

`target: gateway` resolves to the first `spec.loadBalancer.pool` IP — where the
Envoy gateway pins via `metallb.universe.tf/loadBalancerIPs` — so there is no IP
to keep in sync by hand. Resolution is self-contained (no dependency on public
DNS).

### Raw escape hatches

For anything the structured form doesn't cover, supply raw CoreDNS — inline or
from files. All inputs compose:

```yaml
spec:
  coredns:
    servers: |              # raw server block(s) — a *.server file
      metrics.internal:53 { forward . 10.0.0.53 }
    overrides: |            # directives merged into the default .:53 block
      hosts { 10.0.0.9 internal.svc ; fallthrough }
    import: ./coredns       # dir of raw *.server / *.override files
                            # (relative to the cluster dir; default ./coredns)
```

| Input | Becomes | Use for |
|---|---|---|
| `hosts[]` `{name,target}` | a generated `name:53 { … }` block | the friendly path — driver writes the template |
| `servers` | a `*.server` file (own server blocks) | raw zones |
| `overrides` | a `*.override` file (merged into `.:53`) | extra `hosts`/`rewrite`/`forward` |
| `import` | raw `*.server`/`*.override` from a path | many/large snippets |

Don't define the same zone via both `hosts` and a raw `servers`/`import` block —
CoreDNS rejects duplicate zone definitions.

## Multi-Node Clusters

For testing HA setups locally, configure multiple nodes:

```yaml
spec:
  nodes:
    controlPlane: 3
    workers: 2
```

The first control-plane node gets port mappings and the `ingress-ready=true` label. Additional nodes are plain kind nodes.

## Tilt Commands

```bash
lo tilt up          # start Tilt
lo tilt down        # stop Tilt
lo tilt status      # run tilt doctor
lo tilt restart     # stop + start
```

## Stopping

```bash
lo down             # stops Tilt + deletes kind cluster
```
