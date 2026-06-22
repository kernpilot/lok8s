# kkp-hosted — hosted control plane (future)

The hosted path: **kubehz** runs the KKP control plane in its own
infrastructure, and you provide only the workers on your Hetzner account. The
`kubehz-core` operator reconciles a `KubehzCluster` into a KKP user cluster (the
kubehz policy layer — phases, per-tier quotas, per-tenant OIDC — on top of KKP).

This example lands when the kubehz hosted plane is GA (worker/MachineDeployment
provisioning + billing). Until then, use [`../kkp`](../kkp/) for the self-managed
KKP path; see [lok8s.io](https://lok8s.io) for the roadmap.
