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

## TLS registries (no `insecure-registries`)

By default the registries serve plain **HTTP** on port `:80` and are addressed by raw IP. For the host Docker daemon to push to them, the registry IP range must be listed in `/etc/docker/daemon.json` under `insecure-registries` — a per-machine manual step that is easy to get wrong (a single CIDR typo silently breaks every cluster's push).

Set `spec.registries.tls: true` to serve the registries over **HTTPS** with a [mkcert](https://github.com/FiloSottile/mkcert)-signed certificate instead:

```yaml
spec:
  registries:
    tls: true
```

What this does:

- **One cert for all registries.** `lo provision` generates a single certificate into `.secrets/tls/registries/` whose Subject Alternative Names cover every registry's IP plus the framework hostnames `lok8s.local` and `lok8s.cache`. It is regenerated automatically if the IP/hostname set changes (e.g. you add a mirror or change the subnet).
- **Registries listen on `:443`.** TLS mode moves the listen port from `:80` to `:443` so that a bare-IP `docker push <ip>/...` — which the Docker client resolves to the HTTPS default port — reaches the registry with no explicit port in the ref.
- **Host `docker push` validates over HTTPS.** Because mkcert installs its root CA into the system trust store (`mkcert -install`), the Docker client (and `curl`) trust the registry cert. **No `insecure-registries` entry is required.**
- **Containerd in the kind nodes trusts the same cert.** Each `hosts.toml` is written with `server = "https://<ip>"` and `ca = "/etc/containerd/certs.d/.ca/rootCA.pem"` — a copy of mkcert's root CA placed in the bind-mounted `certs.d` tree. No `skip_verify`.

### Prerequisite: `mkcert -install`

Registry TLS relies on the mkcert root CA being in the host trust store. Run this **once** per machine before provisioning a TLS cluster:

```bash
b install mkcert      # if not already managed by b
mkcert -install       # adds the local CA to the system + browser trust stores
```

`~/.local/share/mkcert/rootCA.pem` is the trust anchor for **both** the host Docker client and containerd inside the kind nodes. If `tls: true` but mkcert is missing or `mkcert -install` was never run, `lo provision` fails fast with a clear message rather than producing a registry the host can't push to.

> This is a host-level step that cannot be automated by lok8s — installing a CA into the system trust store requires the user's own privileges and consent. It is the only manual prerequisite for TLS registries.

### Verifying

```bash
lo registry status                 # catalog URLs show https:// in TLS mode
curl https://<build-ip>/v2/        # 200, no -k needed (mkcert CA trusted)
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
