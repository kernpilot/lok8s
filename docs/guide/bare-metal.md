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
| `#wipe-devices` | Data devices to full-wipe on a **fresh** install ([details](#wiping-data-devices)) |

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

### Ceph OSDs on bare metal: force GPT

Bare-metal `installimage` defaults to an **MBR** partition table. A typical
Kubernetes disk layout has more than four partitions (`/boot`, `/`,
`/var/lib/containerd`, …), so MBR must wrap the extras in an **extended
partition** — and its ~1 KiB marker is what `ceph-volume raw list` (Rook's no-arg
OSD scan) chokes on: `ceph-bluestore-tool`'s `is_valid_io` asserts, the scan
returns `{}`, and **every OSD on the node is hidden** — the node provisions fine
but Ceph shows zero OSDs ([rook#17716](https://github.com/rook/rook/issues/17716);
fixed upstream in [ceph#69812](https://github.com/ceph/ceph/pull/69812)).

Force a GPT table in the installimage so there is no extended partition (`2` =
always, even on disks < 2 TB):

```
FORCE_GPT 2
```

::: warning The ceph-osd carve also triggers this on MBR
Its MBR path (`parted mkpart logical …`) creates a logical → extended partition —
the exact ~1 KiB trigger. On bare-metal Ceph nodes, **always `FORCE_GPT`**.
:::

For the OSD storage itself, prefer a **dedicated raw disk** — leave a drive
unpartitioned (comment out its `DRIVE`, omit its `PART` lines) and point Rook at
it via `deviceFilter` (e.g. `^nvme[12]n1$`) or `useAllDevices`; the layout example
above uses exactly this approach. The single-disk `ceph-osd` carve also works, but
only if installimage leaves a free tail — on a full OS disk it has nothing to claim.

## Wiping data devices {#wiping-data-devices}

Reinstalling the OS with `installimage` only touches the OS disk. Extra data
drives — the raw disks you hand to Rook-Ceph, a database, etc. — keep whatever
was on them from a previous life. For storage systems that write metadata
**across the whole disk** that stale data is not harmless: it makes a "fresh"
disk look half-initialised.

`#wipe-devices` declares, per server, which data devices to fully erase when
that server is being **freshly installed**. The provider honors it
transparently as part of the normal `lo provision` bare-metal path — there is
no separate command to run.

### Rescue-mode gating — a live node is never touched

The wipe is gated on **two** conditions, both of which must hold: the server is
in Hetzner **rescue mode** (RAM-booted, before `installimage` runs) **and** an
`#installimage` config is actually present, so the OS disk **will** be
reinstalled immediately afterwards. This co-gate means a wipe can never run
unless a fresh install is about to follow — a rescue node with no installimage
config is left untouched, exactly like an already-installed one. This is the
same fresh-install gate described in [Subsequent runs](#subsequent-runs): a
server that is **not** in rescue mode (already installed and running) is never
reached by this code path, so its disks are never touched. Wiping in rescue mode
is safe — nothing on the target disks is in use yet, and `installimage`
reinstalls the OS disk right afterwards.

If a declared device fails its identity check (below), the wipe **aborts the
install** with a non-zero status and touches nothing — the framework fails
loudly rather than installing over disks it could not positively identify.

::: warning A mid-wipe abort can leave a node partially wiped
The guards run per device, in order, so if wiping several devices aborts partway
(e.g. an unexpected device fails its identity check after an earlier one was
already discarded), the node may be left **partially wiped**. This is loud — the
install stops with a non-zero status — and recoverable: fix the offending
descriptor entry and re-run `lo provision` (the node is still in rescue mode, so
the wipe simply runs again from the top).
:::

### Two forms

**Wipe every physical disk** — `true`:

```json
{ "name": "worker-0", "#cloud.root": "true",
  "#external-ip": "203.0.113.10",
  "#installimage": "clusters/example.com/cloud-init/installimage/worker-0",
  "#wipe-devices": true }
```

Every physical disk (`lsblk` type `disk`) is wiped. Simple and safe on a
dedicated node whose disks you fully own — `installimage` re-partitions and
reinstalls the OS disk immediately after.

**Wipe specific devices, with sanity guards** — a list:

```json
{ "name": "worker-0", "#cloud.root": "true",
  "#external-ip": "203.0.113.10",
  "#installimage": "clusters/example.com/cloud-init/installimage/worker-0",
  "#wipe-devices": [
    { "device": "/dev/nvme1n1",
      "model": "SAMSUNG MZQL21T9HCJR-00A07",
      "id": "eui.3634..." },
    { "device": "/dev/nvme2n1",
      "model": "SAMSUNG MZQL21T9HCJR-00A07" }
  ] }
```

Each entry:

| Key | Purpose |
|-----|---------|
| `device` | Device path to wipe, e.g. `/dev/nvme1n1`. |
| `model` | *Sanity guard.* Assert udev `ID_MODEL` equals this **before** wiping. |
| `id` | *Sanity guard.* Assert a stable id — udev `ID_SERIAL` or `ID_WWN`, or a `/dev/disk/by-id/<id>` match — equals this before wiping. |

::: warning `model` must be the exact udev `ID_MODEL`, not the marketing name
The guard is a **whole-line exact** match against udev's `ID_MODEL` property,
which is device-dependent — some device classes replace spaces with
underscores. Read the exact string off the target node (in rescue mode) rather
than copying a datasheet name:

```bash
udevadm info --query=property --name=/dev/nvme1n1 | grep ID_MODEL=
```

Use the value **after** the `=` verbatim. The example above (`SAMSUNG
MZQL21T9HCJR-00A07`) is illustrative; the exact form on your hardware may differ.
:::

`model` and `id` are optional guards. When present, they must match: **any
declared mismatch aborts the install** and wipes nothing. This is the
recommended form for nodes where you must not risk erasing the wrong drive —
declare the model and/or serial you expect, and the install refuses to proceed
if the hardware doesn't line up (drives were reordered, a disk was swapped,
etc.). If an entry omits `device` but gives `id`, the device is resolved via
`/dev/disk/by-id/<id>`.

Omitting `#wipe-devices` entirely (or setting it to `false`) means **no wipe** —
the safe default.

### Why a full wipe, not just the partition table

The wipe uses `blkdiscard` across the **entire** device, not a head-only
`wipefs`/`sgdisk`. Ceph's BlueStore writes redundant block-device labels at
size-scaled offsets **across the whole disk**; a head-only wipe leaves the far
copies behind, and a freshly-prepared OSD then trips a
`_check_main_bdev_label` mismatch (akin to
[rook/rook#17716](https://github.com/rook/rook/issues/17716)). Discarding the
full device avoids that class of "the disk looks half-initialised" failure.

There is **no partial fallback**: if a device does not support discard, the
install **aborts** rather than falling back to a bounded `dd` zero-fill. A
head-or-first-few-GiB zero would leave those far-offset labels intact yet still
report success — a silent false-green. So a device is either fully discarded or
the install stops loudly.

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
