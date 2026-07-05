# Disaster Recovery (`lo recover`)

`lo recover <domain>` rebuilds a cluster **from bare metal** back to a working
Kubernetes cluster — reimaging the nodes in place (IPs, network, and
load-balancer preserved), then running a fresh `lo provision`. It is the
foundation a disaster recovery stands on: it restores the **cluster**, not the
application **data** (etcd snapshots, databases, PVs — see [Backups](./backups.md)
— restore *on top* of a recovered cluster).

It supersedes hand-rolled, per-cluster DR scripts: rather than re-implementing
teardown/rescue/reset/provision by hand, `lo recover` orchestrates the pieces the
framework already owns — the provider's [`provider::rebuild`](./bare-metal.md#diagnose-infrastructure-lo-doctor)
node reset, `lo provision` (including the bare-metal [`#wipe-devices`](./bare-metal.md#wiping-data-devices)
data-disk wipe), and the read-only [`lo doctor`](./bare-metal.md#diagnose-infrastructure-lo-doctor)
provider diagnosis.

## The flow

```
resolve → doctor → consent → rebuild → provision → verify
```

1. **resolve** — resolve the domain's cluster spec, require it is a **cluster**
   domain (not a deploy domain), load its provider, and require the provider
   implements the `provider::rebuild` hook. A provider without it can't be
   auto-recovered in place, so `lo recover` stops here with a clear message.
2. **doctor** — print the provider's **read-only** infrastructure diagnosis
   (the same one `lo doctor` renders) *before* anything destructive: credentials
   and API reachability, the rescue SSH key, per-node installed-vs-rescue state,
   inventory sanity. This is **advisory** — it never blocks (enforcement is the
   rebuild's own preflight).
3. **consent** — a destructive-consent prompt naming the cluster and node count.
   This is **the** guard. `--force` (global) or `LOK8S_NONINTERACTIVE=1` opt out
   of the prompt; declining touches nothing.
4. **rebuild** — [`provider::rebuild`](./bare-metal.md#diagnose-infrastructure-lo-doctor):
   reset the cluster's existing nodes from bare metal (cloud VMs reimaged via
   `hcloud server rebuild`; bare-metal nodes booted into the Robot rescue system).
   It runs an **atomic preflight** that validates every declared node up front and
   touches nothing if any node fails. On failure `lo recover` stops — it will not
   provision on a half-reset cluster.
5. **provision** — a fresh `lo provision` (KubeOne/CAPI install). The bare-metal
   path performs the `#wipe-devices` data-disk wipe transparently.
6. **verify** — compare Ready nodes (`kubectl`) to the inventory count
   (`provider::output`), reporting each node's readiness.

Each phase is timed; the run ends with a per-phase summary
(`DONE in XmYs — phases: resolve=… doctor=… rebuild=… provision=… verify=…`).

## Usage

```bash
lo recover <domain>                 # full recovery (prompts once to confirm)
lo recover <domain> --dry-run       # preview: print the rebuild plan, change nothing
lo recover <domain> --skip-rebuild  # re-provision + verify only (nodes already reset)
lo recover <domain> --force         # skip the confirmation prompt (non-interactive)
```

| Flag | Description |
|------|-------------|
| `--dry-run` | Run the read-only doctor + the `provider::rebuild` **plan** (reimages nothing), print that `lo provision` would follow, then stop. |
| `--skip-rebuild` | Skip the node rebuild — run `lo provision` + verify only. Use when the nodes are already in the fresh-install state. |
| `--force` | Global flag: skip the destructive-consent prompt. |

## Design: doctor advises, consent lives here, rebuild enforces

Three responsibilities, three owners:

- **`provider::doctor` advises.** It is read-only and never blocks — it exists so
  the operator sees the infrastructure state *before* deciding.
- **`lo recover` owns consent.** The destructive prompt lives in the command, not
  in the provider hooks. `provider::rebuild` deliberately does **not** prompt — a
  library primitive shouldn't; the operator-facing command does.
- **`provider::rebuild` enforces.** Its atomic preflight is the hard safety gate:
  it resolves and validates every node and refuses to reimage anything unless all
  of them check out (a name collision or a partial descriptor can never reset the
  wrong — or only some of the — machines).

## `--dry-run` is genuinely safe

`--dry-run` exports `CLOUD_DRY_RUN=1`, which flows into `provider::rebuild`'s
dry-run branches: it runs the read-only preflight, **prints** the exact per-node
reimage / rescue+reset plan (never the credentials), and reimages **nothing** —
the readiness barrier is skipped because no node changed state. It then reports
that `lo provision` would follow and stops without provisioning. Use it to review
exactly what a recovery would do before committing.

::: warning Data is a separate step
`lo recover` restores the **cluster**, not application data. After it reports the
nodes Ready, restore etcd/database/PV backups per [Backups](./backups.md). A
mid-rebuild failure can leave the cluster partially reset — it stops loudly and is
re-runnable (the nodes are still mid-reset, so recovery simply resumes).
:::
