# Bare Metal Servers (Hetzner Robot)

lok8s supports provisioning bare metal (dedicated) servers alongside
cloud VMs via the Hetzner provider. Bare metal servers use the
`#cloud.root` flag in the hetzner.json descriptor.

## How it works

Cloud VMs are created by `hcloud server create`. Bare metal servers
**already exist** — they're ordered via Hetzner Robot and provisioned
via `installimage` in rescue mode.

The provider handles both types in the same JSON descriptor:

```json
{
  "server": [
    // Cloud VMs — created automatically
    { "name": "cp-0", "type": "cx33", "image": "ubuntu-24.04",
      "location": "fsn1", "ssh-key": [0], "network": 0 },

    // Bare metal — pre-existing, provisioned via installimage
    { "name": "worker-0", "#cloud.root": "true",
      "#external-ip": "203.0.113.10", "#internal-ip": "10.0.1.10",
      "#installimage": "clusters/example.com/cloud-init/installimage/worker-0",
      "#cloud.d": "ci",
      "#labels": "lok8s.dev/cluster=my-cluster,lok8s.dev/role=worker",
      "network": 0, "ssh-key": [0] }
  ]
}
```

### `#`-prefixed fields

Fields starting with `#` are **metadata** — they're not passed to
`hcloud` CLI as flags. Instead, they're consumed by the provider hooks:

| Field | Purpose |
|-------|---------|
| `#cloud.root` | Marks a bare metal server (skip `hcloud server create`) |
| `#external-ip` | Server's public IP (for SSH + provider output) |
| `#internal-ip` | Server's private/vSwitch IP |
| `#installimage` | Path to Hetzner installimage config file |
| `#cloud.d` | Cloud-init module directory to apply |
| `#labels` | Comma-separated labels (included in provider output) |
| `#floating-ip` | Index into the `floating-ip` array to assign |

## Provisioning flow

A bare metal node **self-bootstraps via cloud-init**, exactly like a cloud
VM — the only extra step is installing cloud-init, because the Hetzner
installimage base image doesn't ship it.

### First time (rescue mode)

1. Order the dedicated server via [Hetzner Robot](https://robot.hetzner.com)
2. Activate rescue mode (Robot console, or the Robot API — see [Limitations](#limitations))
3. Run `lo provision` — the provider detects rescue mode and:
   - SCPs the installimage config to the server
   - Generates an installimage **post-install** script and SCPs it
   - Runs `installimage -a -c /tmp/installimage.conf -x /tmp/lok8s-post-install`
   - Waits for the reboot
   - Waits for `cloud-init status --wait` (the node configures itself)
4. Server is ready for Kubernetes (KubeOne joins it as a worker)

### Why cloud-init on bare metal {#self-bootstrap}

The Hetzner installimage `*-base` images have **no cloud-init**. So the
generated `-x` post-install script — which runs inside the freshly installed
system's chroot, *before* any firewall exists (apt egress is unrestricted) —
does two things:

1. `apt-get install cloud-init`
2. Seeds the [NoCloud datasource](https://cloudinit.readthedocs.io/en/latest/reference/datasources/nocloud.html)
   (`/var/lib/cloud/seed/nocloud/user-data`) with the **same**
   `cloud-config::generate` output a cloud VM gets, pins the datasource, and
   disables cloud-init network rendering (installimage owns the base network).

On first boot cloud-init runs `write_files` + `runcmd` natively — vSwitch
netplan, sysctls, kernel modules, packages. One config, one mechanism, cloud
and bare metal alike. (If cloud-init is somehow absent or errors, the
provider falls back to applying the same config directly over SSH.)

::: tip Generated post-install — preview it
```bash
source .lok8s/providers/hetzner/cloud-config
CLOUD_PATH=clusters/<domain>/cloud-init CLOUD_PATHD="node:worker" \
  cloud-config::installimage-post-install
```
:::

### Subsequent runs

The bare metal bootstrap is gated **solely** on rescue mode + a fresh
installimage run. A server that is **not** in rescue mode (already installed)
already self-bootstrapped on its own first boot, so the provider leaves it
untouched — it does not re-apply config on every run.

### Rescue mode detection

The provider checks for `/root/.oldroot/nfs/install/installimage` on
the server via SSH. This binary only exists in Hetzner's rescue system.

## installimage config

The installimage config defines disk layout, RAID, hostname, and OS:

```
# Example for Kubernetes + Rook-Ceph
DRIVE1 /dev/nvme0n1       # OS drive (partitioned)
# DRIVE2 /dev/nvme1n1     # Raw for Ceph OSD
# DRIVE3 /dev/nvme2n1     # Raw for Ceph OSD

SWRAID 0                   # No RAID (Ceph handles replication)
HOSTNAME worker-0.example.com

PART /boot ext4 1G
PART / ext4 50G
PART /var/lib/containerd xfs 200G
PART /var/lib/kubelet xfs 100G
PART /var/log ext4 50G

IMAGE /root/.oldroot/nfs/install/../images/Ubuntu-2404-noble-amd64-base.tar.gz
```

See [Hetzner installimage docs](https://docs.hetzner.com/robot/dedicated-server/operating-systems/installimage/).

## vSwitch networking

Bare metal servers connect to Hetzner Cloud networks via vSwitch:

```json
{
  "network": [
    { "name": "kubernetes", "ip-range": "10.0.0.0/16",
      "#subnets": [
        { "network-zone": "eu-central", "type": "cloud", "ip-range": "10.0.0.0/24" },
        { "network-zone": "eu-central", "type": "vswitch", "ip-range": "10.0.1.0/24",
          "vswitch-id": "12345" }
      ]
    }
  ]
}
```

The vSwitch subnet allows bare metal servers to communicate with
cloud VMs on the same private network.

On the node itself, the vSwitch VLAN is brought up by a netplan dropped in
via the cloud-init `cloud.d` module. Because the physical NIC name varies by
hardware, ship the netplan as an [`execute: remote`](./cloud-init.md#execute-true-vs-execute-remote)
script that **detects** the interface rather than hardcoding it:

```bash
#!/bin/bash
# cloud.d/worker/write_files/etc/netplan/60-vswitch.yaml  (.stat: execute: remote)
set -euo pipefail
link="$(ip -o route get 1.1.1.1 | grep -oP 'dev \K\S+' | head -1)"
cat <<EOF
network:
  version: 2
  vlans:
    ${link}.4001:        # VLAN id = Hetzner vSwitch VLAN
      id: 4001
      link: ${link}
      mtu: 1400          # required by the Hetzner vSwitch
      addresses: [10.0.1.10/24]
      routes:
        - to: 10.0.0.0/16
          via: 10.0.1.1  # vSwitch gateway (forwards but does not answer ICMP)
EOF
```

::: warning vSwitch route propagation
After `netplan apply`, the vSwitch route can take **~1 minute** to converge —
node→cloud-subnet traffic fails immediately after, then works. Don't conclude
"broken" without waiting, and test reachability with TCP (`nc`) not `ping`
(the gateway forwards but doesn't answer ICMP).
:::

## Logging

All provider operations are logged to `<work_dir>/hetzner-provision.log`.
Set `CLOUD_QUIET=1` to suppress console output (log-only mode).

## Limitations

- Rescue mode activation is **manual** (via Hetzner Robot console).
  Automating this via the Robot API (`HROBOT_USER` + `HROBOT_PASSWORD`)
  is planned but not implemented.
- The provider does not manage the dedicated server lifecycle (ordering,
  cancellation) — only provisioning via installimage.
