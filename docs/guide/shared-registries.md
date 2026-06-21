# Shared Registries

## Overview

By default, each lok8s project provisions its own set of Docker registry pull-through mirrors (docker.io, ghcr.io, quay.io, registry.k8s.io). When multiple projects run on the same machine, this duplicates cached layers across containers, wasting disk space and network bandwidth.

Shared registries solve this by placing pull-through mirrors on a dedicated Docker network (`lok8s-registries`) that kind nodes from any project can connect to. Each project still gets its own **build** and **cache** registries for pushing locally-built and credentialed-pre-pulled images, but the read-only public mirrors are shared.

## How it works

```
                 +-----------------------+
                 |  lok8s-registries     |  10.125.200.0/24
                 |  (dedicated network)  |
                 +-----------+-----------+
                             |
          +------------------+------------------+
          |                  |                  |
   lok8s-registry-    lok8s-registry-    lok8s-registry-
     io-docker          io-quay            io-k8s ...
   (pull-through)     (pull-through)     (pull-through)
   10.125.200.2       10.125.200.3       10.125.200.4
          |                  |                  |
          +------------------+------------------+
          |                                     |
+---------+----------+             +-----------+---------+
| project-a network  |             | project-b network   |
| (10.125.125.0/24)  |             | (10.125.50.0/24)    |
| slot 125 — default |             | slot 50 — alternate |
|                    |             |                     |
| build (.101)       |             | build (.101)        |
| cache (.102)       |             | cache (.102)        |
|                    |             |                     |
| kind nodes         |             | kind nodes          |
| (connected to both)|             | (connected to both) |
+--------------------+             +---------------------+
```

- **Shared mirrors** run on the `lok8s-registries` network. Container names use the prefix `lok8s-registry-` (for example, `lok8s-registry-io-docker`). IPs are assigned sequentially starting at `.2`.
- **Build and cache registries** run on each project's own Docker network, on the project's `/24` slot at fixed offsets `.101` and `.102`. These are framework-private — they hold credentialed or locally-built content that should never leak across kind clusters, and they ship implicitly (don't list them in `mirrors`).
- Kind nodes are connected to **both** networks via `docker network connect`, so they can reach shared mirrors and the project-specific build/cache registries.

## Configuration

The `spec.registries` section of `cluster.lok8s.yaml` controls shared registry behavior:

```yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: my-cluster
spec:
  network:
    name: lok8s
    cidr: "10.125.125.0/24"           # slot 125 (default cluster)
  registries:
    shared:
      enabled: true                   # default: true
      network:
        name: lok8s-registries        # default
        cidr: "10.125.200.0/24"       # default
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
      - name: io-quay
        url: https://quay.io
      - name: io-k8s
        url: https://registry.k8s.io
      - name: io-ghcr
        url: https://ghcr.io
```

### Field reference

| Field | Default | Description |
|-------|---------|-------------|
| `shared.enabled` | `true` | Pull-through mirrors live on the shared network |
| `shared.network.name` | `lok8s-registries` | Docker network for shared mirrors |
| `shared.network.cidr` | `10.125.200.0/24` | Subnet for the shared network |
| `mirrors[].name` | required | Mirror identifier (must not be `build` or `cache`) |
| `mirrors[].url` | required | Upstream registry URL |

You never specify registry IPs — the framework computes them:

- `build` → `<project subnet>.101`, `cache` → `<project subnet>.102` (always on the project subnet, even in shared mode)
- Mirrors in shared mode → `.2`, `.3`, `.4`, ... on the shared network
- Mirrors in non-shared mode → `.103`, `.104`, ... on the project subnet

## Opting out

Set `shared.enabled: false` to put all registries on the project subnet:

```yaml
spec:
  network:
    cidr: "10.125.50.0/24"            # slot 50
  registries:
    shared:
      enabled: false
    mirrors:
      - name: io-docker               # → 10.125.50.103
        url: https://registry-1.docker.io
      # ...
```

With `shared.enabled: false`, all registry containers use the project network; `shared.network` is ignored. `build` (`.101`) and `cache` (`.102`) are unaffected — they live on the project subnet in both modes.

## TLS registries (default)

Registries serve **HTTPS** on port `:443` **by default** (`spec.registries.tls: true`). The cert is minted by the [Secret plugin](/reference/kustomize-plugins#development-certificates-cert) — the same `cert:` generator used for application TLS — signed by your shared dev CA at `CAROOT`, with **no `mkcert` binary** to mint (the CA is created on demand). This avoids the fragile `insecure-registries` daemon edit that plain HTTP otherwise needs (a single CIDR typo there silently breaks every cluster's push).

Opt out for plain **HTTP** on `:80` (addressed by raw IP, with the registry IP range listed in `/etc/docker/daemon.json` under `insecure-registries`):

```yaml
spec:
  registries:
    tls: false   # default is true
```

What this does (in the default TLS mode):

- **One cert for all registries.** At provision time `lo provision` drives the [Secret plugin](/reference/kustomize-plugins#development-certificates-cert) to mint a single certificate into `.secrets/tls/registries/` whose Subject Alternative Names cover every registry's IP plus the framework hostnames `lok8s.local` and `lok8s.cache`. It is re-minted automatically if the IP/hostname set changes (e.g. you add a mirror or change the subnet).
- **Registries listen on `:443`.** TLS mode moves the listen port from `:80` to `:443` so that a bare-IP `docker push <ip>/...` — which the Docker client resolves to the HTTPS default port — reaches the registry with no explicit port in the ref.
- **Containerd in the kind nodes trusts the cert directly.** Each `hosts.toml` is written with `server = "https://<ip>"` and `ca = "/etc/containerd/certs.d/.ca/rootCA.pem"` — a copy of the dev root CA (`CAROOT`) placed in the bind-mounted `certs.d` tree. This works **without** `mkcert -install`: containerd verifies against the explicit CA file, so in-cluster pulls trust the registries out of the box. No `skip_verify`.
- **Host `docker push` needs the CA trusted** (in-cluster pulls don't). Run [`lo trust`](/guide/secrets) once — or pick another option in [Host push trust options](#host-push-trust-options) below. Then `docker push` (and `curl`) trust the registries with **no `insecure-registries` entry**.

### Host push trust options {#host-push-trust-options}

In-cluster pulls work out of the box — containerd trusts the cert via the explicit `certs.d` CA file. Only the **host** `docker push` (Tilt's build loop) must trust the registry cert, because the host Docker daemon validates against its own trust store. `lo provision` mints the cert regardless; you make the host trust it once, by **one** of these:

1. **Trust the dev CA — recommended.**
   ```bash
   b install mkcert     # one-time, if not already managed by b
   lo trust             # wraps `mkcert -install`: installs the dev CA system + browser-wide
   ```
   The same CA also makes browsers and `curl` trust your application `*.<domain>` TLS, so this is the one step that covers everything. Needs `sudo` once — see [Trusting the dev CA](/guide/secrets#trusting-the-dev-ca-lo-trust).

2. **Skip verification with `insecure-registries`.** Add the registry IP range to `/etc/docker/daemon.json` under `insecure-registries` and restart Docker. No CA install, but pushes are **unverified** (the fragility TLS is meant to avoid) — least preferred.

3. **Per-registry CA, or a rootless runtime.** Trust just this registry (no system-wide change) by dropping `$CAROOT/rootCA.pem` at the daemon's per-registry path: rootful Docker reads `/etc/docker/certs.d/<registry>/ca.crt` (still `sudo`); **rootless Docker / Podman** keep that tree under your home — e.g. Podman reads `~/.config/containers/certs.d/<registry>/ca.crt` — so you can add it **without `sudo`**.

> Installing a CA (option 1 or 3) needs your own privileges/consent, so lok8s can't fully automate it. If the CA isn't trusted, the cluster still comes up but host `docker push` fails verification until you pick one of the above; `lo up` nudges you. (If `tls: true` but the Secret plugin isn't built, `lo provision` fails fast.)

### Verifying

```bash
lo registry status                 # catalog URLs show https:// in TLS mode
curl https://<build-ip>/v2/        # 200, no -k needed (dev CA trusted via lo trust)
```

## Managing shared registries

### `lo registry status`

Shows both shared and per-project registries, including their container names, IPs, and networks:

```bash
lo registry status
# Shared mirrors (lok8s-registries network):
#   lok8s-registry-io-docker   10.125.200.2   Running
#   lok8s-registry-io-quay     10.125.200.3   Running
#   lok8s-registry-io-k8s      10.125.200.4   Running
#   lok8s-registry-io-ghcr     10.125.200.5   Running
# Per-project registries (lok8s network, slot 125):
#   lok8s-registry-build       10.125.125.101 Running
#   lok8s-registry-cache       10.125.125.102 Running
```

### `lo registry clean`

By default, only removes per-project registries (build, cache). Shared mirrors are left running so other projects can continue using them.

```bash
lo registry clean             # Removes build + cache registries only
lo registry clean --shared    # Also removes shared mirrors + network
```

The `--shared` flag is intentionally explicit to prevent accidentally breaking other projects that depend on the shared mirrors.

## Multi-project workflow

Shared registries are fully idempotent. The order of project lifecycle operations does not matter:

1. **First project** provisions and creates the `lok8s-registries` network and shared mirror containers.
2. **Second project** provisions; sees the network and containers already exist; reuses them. Kind nodes are connected to the existing network.
3. **Destroying project A** removes only the project-A kind cluster and its build + cache registries. Shared mirrors remain untouched.
4. **Project B** continues operating normally with the shared mirrors.

If all projects are destroyed, the shared mirrors keep running as idle containers. They consume minimal resources and will be reused by the next `lo provision`.

## Troubleshooting

### Subnet mismatch error

```
error: registry network 'lok8s-registries' exists with subnet 10.125.200.0/24, expected 10.0.0.0/24
```

This happens when the `lok8s-registries` network already exists with a different subnet than what your `cluster.lok8s.yaml` specifies. Another project (or a previous config) created the network with a different subnet.

**Fix:** Either align your `spec.registries.shared.network.cidr` to match the existing network, or remove the network and let it be recreated:

```bash
lo registry clean --shared
lo provision lok8s.dev
```

### Kind nodes cannot reach shared mirrors

Verify the nodes are connected to both networks:

```bash
docker inspect <node-name> --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}'
```

The output should include both the project network (for example, `lok8s`) and `lok8s-registries`. If the registry network is missing, re-run provision or manually connect:

```bash
docker network connect lok8s-registries <node-name>
```

### Build/cache registry not accessible from cluster

Build and cache registries always live at `.101`/`.102` of the project subnet. Verify the project subnet is what you expect:

```bash
yq '.spec.network.cidr' clusters/lok8s.dev/cluster.lok8s.yaml
# → 10.125.125.0/24   (build → .101, cache → .102)
```

If the cluster was provisioned with a different `spec.network.cidr` than the running registry containers, clean and re-provision: `lo registry clean && lo provision <domain>`.
