# Cloud-Init

lok8s uses [cloud-init](https://cloudinit.readthedocs.io/) to bootstrap
remote VMs before Docker and kind run on them. The Hetzner provider
generates cloud-init user-data from a config directory and passes it
to `hcloud server create --user-data-from-file`.

Cloud-init is used by Lo remote clusters (`lo up --remote`) and by
production drivers (CAPI, KubeOne) that provision via the Hetzner
provider. See [CLI reference — Remote clusters](../reference/cli.md#remote-clusters)
and [Specs — Provider and remote mode](../reference/specs.md#provider-and-remote-mode).

## How it works

When the Hetzner provider creates a server, it:

1. Reads `provider.config.cloudInit` from the cluster spec
2. Sources the cloud-config generator at `.lok8s/providers/hetzner/cloud-config`
3. Renders the config directory into a `#cloud-config` YAML
4. Passes it to hcloud — the VM boots with everything pre-installed

The VM is ready when it comes up. No SSH-based post-boot installation.

### Where it's applied: cloud VM vs bare metal

The **same** generated `#cloud-config` user-data is applied two ways:

- **Cloud VMs** — passed to `hcloud server create --user-data-from-file`.
  Hetzner's image ships cloud-init, so it runs natively on first boot.
- **Bare metal (Robot)** — the installimage base image has **no cloud-init**,
  so the provider's `installimage -x` post-install installs cloud-init and
  seeds the same user-data into the [NoCloud datasource](https://cloudinit.readthedocs.io/en/latest/reference/datasources/nocloud.html).
  The node then self-bootstraps on first boot, exactly like a cloud VM.

The design principle: **cloud-init everywhere, bare metal included** — one
config, one mechanism, whether the node is a cloud VM or a dedicated server.
See [Bare Metal Servers](./bare-metal.md) for the full flow.

## Built-in default

When `cloudInit.path` is not set, the provider uses its built-in config
at `.lok8s/providers/hetzner/cloud-init/`:

```
.lok8s/providers/hetzner/cloud-init/
├── packages            # docker.io, curl, jq
├── nameservers         # 1.1.1.1, 8.8.8.8
└── write_files/
    └── etc/docker/
        ├── daemon.json       # insecure-registries for 10.125.0.0/16, log rotation
        └── daemon.json.stat  # file permissions
```

This is enough for Lo remote clusters — Docker is installed and
configured for the lok8s registry bridge at boot time.

The same directory also ships reusable **`cloud.d/` modules** (e.g. `ceph-osd`)
that a cluster can compose from a custom `cloudInit.path` without copying — see
[Framework module library](#framework-module-library) below.

## Custom cloud-init

Create a `cloud-init/` directory next to your cluster spec:

```
clusters/ci.lok8s.dev/
├── cluster.lok8s.yaml
└── cloud-init/
    ├── packages              # one package per line
    ├── nameservers           # one IP per line
    ├── sources.list.d/       # apt source definitions
    │   └── docker.list       # custom Docker repo
    ├── write_files/          # files synced to the VM
    │   └── etc/
    │       └── docker/
    │           └── daemon.json
    └── cloud.d/              # composable sub-configurations
        └── monitoring/
            ├── packages      # prometheus-node-exporter, ...
            └── write_files/
                └── etc/...
```

Reference it in the cluster spec:

```yaml
spec:
  provider:
    name: hetzner
    config:
      cloudInit:
        path: ./cloud-init            # relative to clusters/<domain>/
        modules: "monitoring"          # colon-separated cloud.d sub-configs
```

### Config directory structure

| Path | Purpose |
|------|---------|
| `packages` | One package name per line. Installed via `apt-get install`. |
| `nameservers` | One IP per line. Written to `/etc/resolv.conf`. |
| `apt` | Raw cloud-init apt configuration YAML. |
| `sources.list.d/<name>` | Apt source definitions (YAML with `source:` and `key:`). |
| `write_files/<path>` | Files synced to the VM at the exact path. |
| `write_files/<path>.stat` | Optional companion file controlling owner, permissions, execution. |
| `cloud.d/<module>/` | Sub-configuration with its own `packages`, `write_files/`, etc. |

### write_files `.stat` companion

Each file under `write_files/` can have a `.stat` companion that controls:

```yaml
owner: root:root            # override; default: CLOUD_USER:CLOUD_GROUP
permissions: 0644           # override; default: 0655
execute: true               # run the file content LOCALLY (at generate time); its stdout becomes the file
execute: remote             # run the file ON THE TARGET (in runcmd); its stdout becomes the file
envsubst: true              # substitute environment variables in the content
envsubst: $HOME             # substitute only specific variables
runcmd: true                # execute the file on first boot (must be executable)
```

### `execute: true` vs `execute: remote`

Both treat the file as a **script whose stdout becomes the file content** —
the difference is *where and when* it runs:

| Directive | Runs | Sees |
|-----------|------|------|
| `execute: true` | locally, when the cloud-config is **generated** | the provisioning host's environment + `CLOUD_ENV_*` |
| `execute: remote` | on the **target node**, during cloud-init `runcmd` (after the `CLOUD_ENV_*` exports, before `runcmd: true` scripts) | the node's actual runtime state (interface names, disks, the live `CLOUD_ENV_*`) |

Use `execute: remote` when the content depends on facts only known **on the
node** — the classic case is a netplan whose VLAN parent interface name can
change between hardware or boots:

```bash
#!/bin/bash
# 60-vswitch.yaml  (.stat: execute: remote)
# Detect the default-route interface instead of hardcoding e.g. enp41s0.
set -euo pipefail
link="$(ip -o route get 1.1.1.1 | grep -oP 'dev \K\S+' | head -1)"
cat <<EOF
network:
  version: 2
  vlans:
    ${link}.4001:
      id: 4001
      link: ${link}
      mtu: 1400
      addresses: [10.0.1.10/24]
EOF
```

**How it works under the hood:** an `execute: remote` file is written to the
node under a `<path>.lok8s-gen` name (so e.g. netplan, which only reads
`*.yaml`, ignores the staged script at boot). A generated `runcmd` entry then
runs the script and writes its stdout to the real `<path>`, applying the
`.stat` `owner`/`permissions`, before any `runcmd: true` executables run.

### Sub-configurations (modules)

The `cloud.d/` directory holds composable modules. Select them via
`cloudInit.modules` (colon-separated):

```yaml
cloudInit:
  path: ./cloud-init
  modules: "docker:monitoring:security"
```

The generator walks each module's directory first, then the root
config. First occurrence of a file wins — modules can override the
default.

### Framework module library

The built-in default dir doubles as a **module library**: framework-shipped
`cloud.d/` modules (`ceph-osd`, …) are reachable from a **custom `cloudInit.path`
without copying them**. Select a module (via `cloudInit.modules` or a server's
`#cloud.d`) and the generator resolves it **your cluster first, then the
framework** — first match wins. The root/base config comes from your dir alone.

```text
#cloud.d: node:ceph-osd            cloudInit.path: ./cloud-init
                                   ( <fw> = .lok8s/providers/hetzner/cloud-init )

  module "node"     1. clusters/<domain>/cloud-init/cloud.d/node/      ← yours    ✓
                    2. <fw>/cloud.d/node/                                (skipped)

  module "ceph-osd" 1. clusters/<domain>/cloud-init/cloud.d/ceph-osd/   (absent)
                    2. <fw>/cloud.d/ceph-osd/                           ← library  ✓

  root config       clusters/<domain>/cloud-init/                      ← YOUR dir only
    (packages / write_files / nameservers — the framework base is never mixed in)
```

So a cluster ships only what it **owns or overrides** (`node`, and its base) and
borrows the rest (`ceph-osd`) from the framework — which keeps that module
maintained in **one place**, so a fix reaches every cluster on the next
provision. No per-cluster copy to drift.

**Precedence**

| Content | Rule |
|---------|------|
| a module's `write_files` / `.stat` | **first match wins** — your file shadows the library's; library-only files in the same module still apply (per-file overlay) |
| `packages` / `nameservers` | **union** of all sources (concatenated; `uniq` only drops *adjacent* repeats — a package listed by two sources is harmless) |
| root config (top-level `packages` / `write_files` / `nameservers`) | **cluster only** — the built-in default's base is never mixed into a custom path |

**Notes**

- Want the default's Docker base on a custom path? By design it's cluster-only —
  reference it as a module, or copy just the files you need.
- Selecting the `ceph-osd` module auto-sets `growpart: off` so it can reclaim the
  disk and size root itself.
- With **no** custom `cloudInit.path`, `CLOUD_PATH` *is* the framework dir, so the
  fallback is a no-op — you get the built-in default unchanged.

## Full cloudInit config

```yaml
spec:
  provider:
    name: hetzner
    config:
      cloudInit:
        path: ./cloud-init        # config dir (default: built-in)
        modules: ""               # colon-separated cloud.d sub-configs
        user: root                # VM user (default: root)
        group: root               # VM group (default: root)
        sshPubPath: ~/.ssh        # dir with *.pub keys to inject (default: ~/.ssh)
```

All fields are optional. Omitting `cloudInit` entirely uses the
built-in default.

## Environment variables

Server parameters from the provider config are available as
`CLOUD_ENV_*` variables in `write_files` with `envsubst: true` and
in `runcmd` scripts:

```yaml
# In provider config:
config:
  cluster_name: my-cluster
  region: fsn1
```

These are accessible as `${CLOUD_ENV_CLUSTER_NAME}` and
`${CLOUD_ENV_REGION}` in write_files templates.

## Preview

To preview what cloud-init will generate without provisioning:

```bash
# Source the generator
source .lok8s/providers/hetzner/cloud-config

# Set the config dir and generate
CLOUD_PATH=clusters/ci.lok8s.dev/cloud-init cloud-config::generate
```

This outputs the full `#cloud-config` YAML to stdout.
