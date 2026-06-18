# Providers

Physical infrastructure providers. Each provider implements the
**provider contract** for creating cloud resources (VMs, networks,
load balancers, firewalls, etc.) that drivers then install Kubernetes on.

| Provider | Path | Cloud |
|----------|------|-------|
| **hetzner** | `hetzner/main` | [Hetzner Cloud](https://www.hetzner.com/cloud) via `hcloud` CLI |

## Provider contract

Every provider implements five functions:

| Function | Purpose |
|----------|---------|
| `provider::validate` | Check credentials + config |
| `provider::credential_data` | Emit k=v pairs for Kubernetes Secrets |
| `provider::provision` | Create cloud resources |
| `provider::destroy` | Tear down cloud resources |
| `provider::output` | Standard inventory JSON (the bridge to drivers) |

Optional: `provider::status` (Running / Partial / NotFound).

## Standard output

The key interface between providers and drivers. Every provider produces
the same JSON shape after provisioning:

```json
{
  "api": { "endpoint": "1.2.3.4", "port": 6443 },
  "nodes": [
    {
      "name": "prod-cp-0",
      "role": "control-plane",
      "group": "cp",
      "public_ip": "1.2.3.4",
      "private_ip": "10.0.0.2",
      "ssh_user": "root",
      "ssh_port": 22
    }
  ],
  "network": { "id": "12345", "name": "prod", "cidr": "10.0.0.0/8" }
}
```

Drivers transform this output into their own format (KubeOne → tfjson,
CAPI → Machine templates, KKP → REST payload). The provider doesn't
know or care which driver consumes it.

## Spec integration

```yaml
spec:
  provider:
    name: hetzner              # selects providers/hetzner/main
    config:                    # inline, opaque — provider-specific
      region: fsn1
      cluster_name: prod
      controlPlane:
        type: cpx31
        replicas: 3
    # OR: load config from a file
    configRef: hetzner.yaml    # relative to clusters/<domain>/
    credentials:
      envVars: [HCLOUD_TOKEN]
      secretRef: prod-creds
```

`config` and `configRef` are the same format — just inline vs file.
The provider reads from `PROVIDER_CONFIG_FILE` either way.

## Adding a new provider

1. Create `providers/<name>/main`
2. Implement the five contract functions
3. `provider::output` must produce the standard JSON shape
4. Users add `spec.provider.name: <name>` to their cluster spec — done

No driver changes needed. The standard output contract is the bridge.

## Documentation

- [Concepts — Two Deployment Planes](https://kernpilot.github.io/lok8s/guide/concepts#two-deployment-planes)
- [Specs Reference — Provider](https://kernpilot.github.io/lok8s/reference/specs#default-resolution)
- [Addons Guide](https://kernpilot.github.io/lok8s/guide/addons)
- [Driver README](../drivers/README.md)
