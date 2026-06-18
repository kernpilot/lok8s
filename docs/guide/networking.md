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
