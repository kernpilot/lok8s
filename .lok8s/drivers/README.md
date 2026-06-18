# Drivers

Cluster-architecture drivers. Each driver implements the **driver contract**
(`driver::provision`, `driver::destroy`, `driver::status`, `driver::kubeconfig`)
for a specific way of creating Kubernetes clusters. Bootstrap addons
(CNI, LB, etc.) are **not** part of the driver contract — they live at
the framework level in `.lok8s/libs/bootstrap` and run identically
across every driver. See the [Driver Contract reference](https://kernpilot.github.io/lok8s/reference/kind-contract).

| Driver | Path | Purpose |
|--------|------|---------|
| **lo** | `lo/main` | Local / CI clusters via Docker + [kind](https://kind.sigs.k8s.io) |
| **capi** | `capi/main` | Production clusters via [Cluster API](https://cluster-api.sigs.k8s.io) |
| **kubeone** | `kubeone/main` | Production clusters via [KubeOne](https://docs.kubermatic.com/kubeone) |
| **kkp** | `kkp/main` | Managed clusters via [Kubermatic KKP](https://www.kubermatic.com) REST API |

## How drivers work

The framework dispatches to the right driver based on `kind:` in the
cluster spec (Lo, Capi, KubeOne, Kkp). The dispatch layer (`libs/provision`)
sources `drivers/<kind>/main`, validates the contract, and calls
`driver::provision`.

## Providers

Drivers that provision cloud infrastructure delegate to a **provider**
(`.lok8s/providers/<name>/main`). The provider is selected by
`spec.provider.name` in the cluster spec and sourced automatically
before the driver runs.

The relationship is many-to-many: CAPI can use Hetzner or AWS; KubeOne
can use the same providers. Lo (kind) has no provider — it's local only.

See: [Provider README](../providers/README.md)

## Bootstrap addons

Cluster-infrastructure addons (CNI, LB, CRDs) live at the framework
level in `.lok8s/addons/`, not inside individual drivers. They are
applied by `.lok8s/libs/bootstrap` after `driver::provision` succeeds
— the same code path for every driver.

```
.lok8s/addons/
├── cilium/       # Cilium CNI via khelm (layered values per driver/provider)
└── metallb/      # MetalLB via khelm
```

Listed in `spec.bootstrap: [cilium, metallb]`. See the
[Addons guide](https://kernpilot.github.io/lok8s/guide/addons) for
authoring and the `base < driver < provider < inline` values
precedence chain.

## Documentation

- [Concepts — Cluster Kinds](https://kernpilot.github.io/lok8s/guide/concepts#cluster-kinds)
- [Specs Reference](https://kernpilot.github.io/lok8s/reference/specs)
- [Addons Guide](https://kernpilot.github.io/lok8s/guide/addons)
- [Driver Contract](https://kernpilot.github.io/lok8s/reference/kind-contract)
