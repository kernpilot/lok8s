# Backups

> HA is **not** a backup. Replication (Ceph, etcd quorum) protects against
> hardware failure; it does nothing for accidental deletion, corruption,
> ransomware, or losing the whole cluster/site. You back up regardless.

lok8s clusters use a **two-tier**, off-cluster backup model, configured per
cluster under `spec.backup` and applied by the `./targets/backup` bootstrap
target. It ships **disabled** — enable it once you have credentials.

## Tiers

| Tier | Backend | Interface | Holds |
|---|---|---|---|
| **hot / live** | Hetzner **Object Storage** | S3 | control-plane etcd (seed + KKP user clusters), databases (CNPG/barman), velero |
| **cold / archive** | Hetzner **Storage Box** | rclone sync (S3→SFTP) | cheap long retention |

**Why S3 for the hot tier:** KKP CE's etcd backup/restore — which powers
free-tier *pause/hibernate* as well as disaster recovery — **requires an S3
endpoint**, and S3 is also the common API for KubeOne's restic etcd backup,
CNPG's barman, and velero. So S3 is a hard dependency, not a nice-to-have.

**Why Storage Box for cold:** it's far cheaper per-TB and has no egress fees,
but it is **not S3** (SFTP/CIFS/BorgBackup/restic only). So it can't be a hot
S3 target — reach it with `rclone` (S3→SFTP) or `restic`, never a MinIO-on-CIFS
shim (MinIO is unsupported on network filesystems). etcd/DB snapshots are
tiny, so the hot tier easily fits Object Storage's included quota; the cold
tier only earns its keep for multi-TB archives.

## What gets backed up where

| Data | Tool | Target |
|---|---|---|
| Seed (KubeOne) etcd + PKI | KubeOne `backups-restic` addon | S3 |
| KKP user-cluster etcd | KKP CE `EtcdBackupConfig` (auto, via Seed `etcdBackupRestore`) | S3 |
| PostgreSQL (CNPG) | barman (built in) — *app-consistent, preferred for DBs* | S3 |
| Namespaced state / PVs | upstream velero (KKP's built-in velero PV backup is **EE-only**) | S3 |
| Long retention | rclone archive job | Storage Box |

## Configure

```yaml
# cluster.lok8s.yaml
spec:
  backup:
    s3:                                   # hot — Hetzner Object Storage
      enabled: true
      endpoint: fsn1.your-objectstorage.com
      region: fsn1
      bucket: kubehz-backups
    storageBox:                           # cold — Hetzner Storage Box
      enabled: true
      host: uXXXXXX.your-storagebox.de
      user: uXXXXXX
      remotePath: kubehz/archive
    schedule: "0 2 * * *"
    retentionDays: 30
```

Set the credentials (never in git) with the secrets plugin:

```bash
lo secrets set --name backup-s3         --namespace backup-system accessKey <KEY>
lo secrets set --name backup-s3         --namespace backup-system secretKey <SECRET>
lo secrets set --name backup-storagebox --namespace backup-system password  <STORAGEBOX_PW>
```

Then flip `S3_ENABLED`/`ARCHIVE_ENABLED` in the target's ConfigMap and
`suspend: false` on the archive CronJob. For KKP, the install also creates
`kkp-etcd-backup-s3` in the `kubermatic` namespace (see `.kkp/seed.yaml`).

## Ceph

If you run Rook-Ceph (multi-node), its **RGW** can *provide* an in-cluster S3
(app/customer buckets, and optionally the KKP backup target) — but still ship
backups to **external** Object Storage for the off-site copy. Ceph replication
is availability, not recoverability.
