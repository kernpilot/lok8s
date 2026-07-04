# Networking & Ingress (Hetzner)

## Two load-balancer paths

A lok8s Hetzner cluster gets load balancers from **two different layers** —
know which one you're configuring:

| LB | Provisioned by | Declared in | Created |
|---|---|---|---|
| **API LB** (`apiserver:6443`) | the lok8s **hetzner provider** (`hcloud load-balancer create`) | `hetzner.json` (provider config) | at `lo provision` — static infra |
| **Ingress / Service LBs** (Envoy `:80/:443`, …) | the **hcloud CCM** (in-cluster controller) | Kubernetes Service annotations | at runtime — k8s-native |

So ingress is **configured via cluster resources**, not the driver/provider:
you write Gateway-API / Envoy-Gateway objects, the CCM reconciles the
`Service` into a Hetzner LB. The provider only owns the control-plane API LB.

## Ingress: Envoy Gateway + the hcloud CCM

Envoy Gateway exposes its proxy as a `Service` of type `LoadBalancer`; the
hcloud CCM turns that into a Hetzner LB based on annotations. Set those
annotations through an **`EnvoyProxy`** (referenced by the `GatewayClass`),
not by hand — Envoy Gateway owns the Service:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: { name: envoy }
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: kubehz-proxy
    namespace: envoy-gateway-system
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata: { name: kubehz-proxy, namespace: envoy-gateway-system }
spec:
  provider:
    type: Kubernetes
    kubernetes:
      envoyService:
        annotations:
          load-balancer.hetzner.cloud/location: fsn1            # REQUIRED or the LB never gets created
          load-balancer.hetzner.cloud/use-private-ip: "true"
          load-balancer.hetzner.cloud/uses-proxyprotocol: "true"
```

::: warning `location` is mandatory
Without `load-balancer.hetzner.cloud/location` (or `network-zone`) the CCM
can't place the LB and the `Service` stays `<pending>` forever.
:::

The CCM targets cluster nodes automatically — including a **bare-metal Robot
worker**, which it adds by its private vSwitch IP (e.g. `10.0.1.10:NodePort`).

## Preserving the client IP — PROXY protocol

A Hetzner LB is L4, so backends see the LB's IP, not the client's. Enable
PROXY protocol on **both** sides (mismatch breaks every connection):

1. **LB** — `load-balancer.hetzner.cloud/uses-proxyprotocol: "true"` (above).
2. **Envoy** — a `ClientTrafficPolicy` so it parses the header and sets
   `X-Forwarded-For`:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: ClientTrafficPolicy
metadata: { name: kubehz-proxy-protocol, namespace: default }
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: kubehz
  proxyProtocol: {}        # v1.7+; supersedes the deprecated enableProxyProtocol
```

Verify the handshake by checking the LB **target health** is `healthy` — the
Hetzner health check sends the PROXY header too, so an unhealthy target means
the two sides disagree.

All of these live in the **networking bootstrap target** (`./targets/networking`,
e.g. `gateway.yaml`) and are applied by `lo bootstrap` / `lo provision`.

## Storage: the provider CSI is opt-in (`spec.network.csi`)

KubeOne ships an **embedded `csi-hetzner` addon** that it force-deploys on every
Hetzner cluster. On a cluster that provides its own storage — Ceph (Rook), or any
other CSI you bootstrap — that bundled hcloud CSI is unused (and, on a bare-metal
worker with no cloud metadata service, crash-loops). So
lok8s makes it **opt-in**, exactly the way `spec.network.cni` gates the CNI:

| `spec.network.csi` | Storage | Behaviour |
|---|---|---|
| _absent_ / `external` | Ceph / your own CSI | **Default.** No bundled CSI — KubeOne's embedded `csi-hetzner` is shadowed and never deployed. |
| `hetzner` | KubeOne's hcloud CSI | Opt in — KubeOne deploys its embedded hcloud CSI (PVCs use `hcloud-volumes`). |

```yaml
spec:
  network:
    cni: external        # lok8s provides the CNI (Cilium) — same pattern
    csi: hetzner         # opt in to KubeOne's bundled hcloud CSI (omit for Ceph-first)
```

This parallels `cni: external`, where lok8s owns the CNI instead of KubeOne's
embedded one. With `csi: external` (the default) lok8s owns storage the same way.

::: warning BREAKING CHANGE — existing KubeOne clusters
Older lok8s always let KubeOne auto-deploy its hcloud CSI. The default is now
**`external` (Ceph-first)**, so a re-provision **removes the bundled CSI**.
- **Ceph / own-storage clusters** — unaffected; the crash-looping CSI is simply
  gone. Nothing to do.
- **Clusters that relied on the hcloud CSI** (PVCs on `hcloud-volumes`) — set
  `spec.network.csi: hetzner` to keep it, **before** the next `lo provision`;
  otherwise those PVCs become unmountable once the driver is removed.
:::

**How the default works (no bundled CSI):** during `lo provision`, the KubeOne
driver writes an **empty** `csi-hetzner/` directory into the addons dir. KubeOne's
`EnsureAddonByName` matches a local addon by **name only**, so the empty directory
shadows the embedded `csi-hetzner` and KubeOne skips an empty addon dir as a no-op
— the embedded CSI is never applied. (The override must be an empty _directory_:
KubeOne pipes addon manifests straight to `kubectl`, so a `kustomization.yaml`
there would error.) It is the same name-match override lok8s already uses to swap
in its robot-aware `ccm-hetzner`.
