# CAPI Clusters

lok8s supports production clusters via [Cluster API (CAPI)](https://cluster-api.sigs.k8s.io/). The Capi driver contract generates CAPI resources from YAML templates and applies them to a management cluster.

## Supported Providers

| Provider | Status | Spec Field |
|----------|--------|-----------|
| Hetzner (hcloud + hrobot) | Supported | `spec.hcloud`, `spec.hrobot` |
| AWS | Supported | `spec.aws` |

Provider detection is automatic: the presence of `spec.hcloud` or `spec.aws` in the cluster spec determines which provider is used.

## Cluster Spec

```yaml
# clusters/prod.example.com/cluster.lok8s.yaml
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: prod
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: prod.example.com
    namespace: default
  managementCluster:
    domain: mgmt.example.com      # omit for SaaS mode
  credentials:
    secretName: prod-credentials   # defaults to <name>-credentials
  hcloud:
    region: fsn1
    sshKeyName: my-ssh-key
  controlPlane:
    replicas: 3
    type: cax21
  workers:
    general:
      replicas: 3
      type: cax21
    gpu:
      replicas: 1
      type: ccx33
  bootstrap:
    - cilium
    - ccm
    - cert-manager
  gitops:
    provider: flux
    repo: https://github.com/myorg/infra.git
    branch: main
    path: clusters/prod.example.com/artifacts
```

## Provisioning

```bash
lo provision prod.example.com
```

The Capi driver contract (`drivers/capi/main`) performs these steps:

1. Read the management cluster domain from `spec.managementCluster.domain`
2. Load the management cluster kubeconfig from `.kubeconfig/<mgmt-domain>.yaml`
3. Detect the CAPI provider from spec fields
4. Create credential Secret on the management cluster via `driver::ensure_credentials`
5. Generate CAPI resources from templates via `capi::generate`
6. Apply resources to the management cluster
7. Wait for the work cluster to become `Provisioned` via `capi::wait_ready`
8. Extract the work cluster kubeconfig via `clusterctl get kubeconfig`
9. Apply `spec.bootstrap` addons on the new cluster via the framework bootstrap (`.lok8s/libs/bootstrap`). See [Addons](/guide/addons) for supported addons and the values precedence chain.
10. GitOps bootstrap is currently a deferred no-op — `lo gitops flux|argo` will be rebuilt on the upcoming `services.yaml` targets-map design

## Credentials

Credentials are provided via environment variables and stored as Kubernetes Secrets on the management cluster:

### Hetzner

```bash
export HCLOUD_TOKEN="your-hcloud-api-token"
export HROBOT_USER="your-robot-user"         # optional, for bare metal
export HROBOT_PASSWORD="your-robot-password"  # optional, for bare metal
```

### AWS

```bash
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
```

The Secret name is configured via `spec.credentials.secretName` (defaults to `<metadata.name>-credentials`).

## CAPI Templates

Templates live in `.lok8s/drivers/capi/cluster/` and are rendered using `envsubst`:

```
.lok8s/drivers/capi/cluster/
  core/
    cluster.yaml                    # Cluster + ClusterIdentity
    kubeadm-control-plane.yaml      # KubeadmControlPlane
    machine-deployment.yaml         # MachineDeployment (per worker pool)
  providers/
    hetzner/
      hetzner-cluster.yaml              # HetznerCluster
      hcloud-machine-template.yaml      # HCloudMachineTemplate
      hrobot-machine-template.yaml      # HRobotMachineTemplate
    aws/                                # AWSCluster + machine templates + managed control plane
```

Variables are extracted from the cluster spec and exported before rendering:

- `CLUSTER_NAME`, `CLUSTER_NAMESPACE`, `CLUSTER_DOMAIN`, `K8S_VERSION`
- `CP_REPLICAS`, `CREDENTIAL_SECRET_NAME`
- Infrastructure kinds: `INFRA_API_VERSION`, `INFRA_CLUSTER_KIND`, `INFRA_MACHINE_TEMPLATE_KIND`
- Provider-specific: `HCLOUD_REGION`, `HCLOUD_SSH_KEY_NAME`, `AWS_REGION`
- Worker pools: `POOL_NAME`, `POOL_REPLICAS`, `POOL_TYPE`

## Adding a Provider

To add a new CAPI provider:

1. Create templates in `.lok8s/drivers/capi/cluster/providers/<name>/`
2. Add a `case` branch in `capi::detect_provider` (in `.lok8s/drivers/capi/generate`)
3. Export provider-specific variables in the new `case` branch of `capi::generate`

No recompilation needed. The template + bash approach means adding a provider is a file drop plus a case statement.

## Cluster Status

```bash
lo status prod.example.com
```

For Capi clusters, this queries the CAPI Cluster resource on the management cluster and reports the phase (`Provisioned`, `Provisioning`, `Failed`, `NotFound`).
