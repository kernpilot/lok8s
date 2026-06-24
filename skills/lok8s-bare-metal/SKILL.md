---
name: lok8s-bare-metal
description: >-
  Use when writing a Hetzner provider descriptor (hetzner.json) or a lok8s
  cloud-init config — for cloud VMs and/or bare-metal (Robot) servers in a
  KubeOne/Capi cluster. Covers the resource arrays, the #-prefixed metadata
  fields, the vSwitch netplan via cloud-init execute:remote, and VM-vs-Robot flow.
---

# Hetzner provider descriptor & cloud-init

The Hetzner provider reads `clusters/<domain>/hetzner.json` (JSON or YAML→JSON).
The whole file is `envsubst`-expanded on load, so `${VARS}` interpolate.
Referenced from the cluster spec via `spec.provider.{name: hetzner, configRef: hetzner.json}`.

## Descriptor shape — resource-type arrays

Provisioned in a fixed order: `ssh-key` → `floating-ip` → `network` →
`load-balancer` → `server` → `volume`. Each plain field becomes an
`hcloud <resource> create --<key> <value>` flag. A **numeric** value cross-references
another array by index and is replaced with that resource's `.id`
(`"network": 0` → the created network's id). A leading `~/` expands to `$HOME`.
Idempotent: existing resources are matched by `.name` and skipped; `destroy`
deletes by label `lok8s.dev/cluster=<cluster_name>`.

```json
{
  "cluster_name": "my-cluster",
  "sshPrivateKey": "~/.ssh/id_ed25519",
  "ssh-key":  [ { "name": "primary", "public-key-from-file": "~/.ssh/id_ed25519.pub" } ],
  "network":  [ { "name": "kubernetes", "ip-range": "10.0.0.0/16",
    "#subnets": [
      { "network-zone": "eu-central", "type": "cloud",   "ip-range": "10.0.0.0/24" },
      { "network-zone": "eu-central", "type": "vswitch", "ip-range": "10.0.1.0/24", "vswitch-id": "12345" }
    ] } ],
  "server": [
    { "name": "cp-0",  "type": "cx33", "image": "ubuntu-24.04", "location": "fsn1",
      "ssh-key": [0], "network": 0,
      "#labels": "lok8s.dev/cluster=my-cluster,lok8s.dev/role=control-plane" },

    { "name": "worker-0", "#cloud.root": "true",
      "#external-ip": "203.0.113.10", "#internal-ip": "10.0.1.10",
      "#installimage": "clusters/<domain>/cloud-init/installimage/worker-0",
      "#cloud.d": "worker",
      "#labels": "lok8s.dev/cluster=my-cluster,lok8s.dev/role=worker",
      "network": 0, "ssh-key": [0] }
  ]
}
```

## `#`-prefixed metadata fields (filtered out of hcloud args; consumed by hooks)

| Field | Purpose |
|-------|---------|
| `#cloud.root: "true"` | marks a **bare-metal (Robot)** server → skip `hcloud server create`, run install hooks |
| `#external-ip` / `#internal-ip` | public / private IP (SSH + provider output) |
| `#installimage` | path to the Hetzner installimage config file |
| `#cloud.d` | cloud-init module dir(s) for this server (sets `CLOUD_PATHD`) |
| `#labels` | comma-separated labels; `role=control-plane` else worker |
| `#floating-ip` / `#ssh-private-key` | floating-IP index to assign / per-server Robot SSH key |

All `#`-fields on a server are also exported into cloud-init as `CLOUD_ENV_<FIELD>`
(uppercased, then every non-`A–Z` char — incl. digits and the leading `#` — →`_`),
e.g. `#external-ip` → `CLOUD_ENV__EXTERNAL_IP`.

## cloud-init config dir

Built-in default: `.lok8s/providers/hetzner/cloud-init/` (packages `docker.io,curl,jq`;
nameservers `1.1.1.1,8.8.8.8`; a `daemon.json` with `insecure-registries: 10.125.0.0/16`).
To use a custom dir beside the cluster spec, add a top-level `cloudInit` block to
`hetzner.json` (the provider reads `.cloudInit.*` from this descriptor):
```json
{ "cloudInit": { "path": "./cloud-init", "modules": "docker:monitoring",
                 "user": "root", "group": "root", "sshPubPath": "~/.ssh" } }
```
`path` resolves relative to `clusters/<domain>/`; `modules` is colon-separated
(`cloud.d/<module>` dirs); all fields are optional. (With inline `spec.provider.config`
instead of `configRef`, the same keys live at `spec.provider.config.cloudInit` — they're
extracted to the descriptor's top level.) Structure: `packages`, `nameservers`, `apt`,
`write_files/<path>` (+ optional `<path>.stat`, default perms `0655`), `cloud.d/<module>/`.
A `.stat` companion sets `owner`, `permissions`, `envsubst: true`, `execute: true|remote`,
`runcmd: true`.

## vSwitch netplan via `execute: remote`

The NIC name varies by hardware, so ship the netplan as a script that **detects**
the interface; its **stdout becomes the file**, run on the node during cloud-init:
```bash
#!/bin/bash
# cloud.d/worker/write_files/etc/netplan/60-vswitch.yaml   (.stat: execute: remote)
set -euo pipefail
link="$(ip -o route get 1.1.1.1 | grep -oP 'dev \K\S+' | head -1)"
cat <<EOF
network:
  version: 2
  vlans:
    ${link}.4001: { id: 4001, link: ${link}, mtu: 1400, addresses: [10.0.1.10/24],
      routes: [ { to: 10.0.0.0/16, via: 10.0.1.1 } ] }   # vSwitch gw forwards, doesn't answer ICMP
EOF
```
- `execute: true` runs **locally at generate time** (sees the provisioning host +
  `CLOUD_ENV_*`); `execute: remote` runs **on the node** (sees live NIC names).
- vSwitch routes take ~1 min to converge — test with TCP (`nc`), not `ping`.

## Cloud VM vs bare-metal (Robot)

| | Cloud VM | Bare metal (`#cloud.root: "true"`) |
|---|---|---|
| creation | `hcloud server create --user-data-from-file <cloud-config>` | pre-existing; **no** hcloud create |
| cloud-init | ships in the image, runs first boot | base image has none → `installimage -x` post-install installs it + seeds NoCloud with the **same** cloud-config |
| trigger | on create | only when in **rescue mode**; already-installed nodes are left untouched |

Design principle: *cloud-init everywhere, bare metal included* — one config, one
mechanism. Preview a node's generated config:
`source .lok8s/providers/hetzner/cloud-config; CLOUD_PATH=clusters/<domain>/cloud-init CLOUD_PATHD="node:worker" cloud-config::installimage-post-install`
