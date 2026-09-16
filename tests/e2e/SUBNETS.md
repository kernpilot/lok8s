# e2e subnet allocation

Each e2e scenario gets its own `/24` slot under the parent
`10.125.0.0/16` so they don't collide with the default dev cluster
or with each other.

## Slot allocation

| Slot | Domain          | Scenario          | Purpose                                    |
|------|-----------------|-------------------|--------------------------------------------|
| 125  | `lok8s.dev`     | (default dev)     | Reserved — primary local dev cluster       |
| 126  | `126.lok8s.dev` | `no-services`     | Provision-only smoke; no workloads         |
| 127  | `127.lok8s.dev` | `single-local-build` | Tilt build → push → deploy roundtrip    |
| 128  | `128.lok8s.dev` | `cache-mode`      | `build:false` cache pre-pull path          |
| 129  | `129.lok8s.dev` | `remote-lo`       | Lo on remote Hetzner VM — docker mode (E2E_REMOTE=1) |
| 130  | `130.lok8s.dev` | `remote-ci`       | Lo on remote Hetzner VM — CI mode (E2E_REMOTE=1) |
| 131  | `131.lok8s.dev` | `registry-tls`    | `registries.tls` — the certificate volume, renew, import |
| 132  | `132.lok8s.dev` | `shared-registries` | First of two domains sharing one mirror set |
| 133  | `133.lok8s.dev` | `shared-registries` | Second domain — reuses that set          |
| 134  | `134.lok8s.dev` | `lifecycle`       | up / up again / down / up / destroy, and the prompts |
| 200  | (shared)        | (shared mirrors)  | Pull-through registry network — the framework default |
| 201  | (shared)        | `shared-registries` | Its own mirror network (`e2e-shr-net`)   |

A new scenario takes the next free slot and adds its row here before it
adds a directory.

## Per-slot layout

Within slot `<n>` the `/24` subnet `10.125.<n>.0/24` is split:

```
  10.125.<n>.1                   docker bridge gateway
  10.125.<n>.101                 build registry  (lok8s.local)
  10.125.<n>.102                 cache registry  (lok8s.cache)
  10.125.<n>.103-106             io-* mirrors (when shared: false)
  10.125.<n>.107-124             reserved
  10.125.<n>.125-150             MetalLB pool (26 IPs)
  10.125.<n>.192-254             kind nodes (docker IPAM — reserved --ip-range .192/26)
```

By default the io-* mirrors live on the project subnet at `.103+`.
With `spec.registries.shared.enabled: true` (opt-in) they move to a
shared registry network, reused across clusters.

## Names, and why a shared scenario names its own

Every object a scenario creates carries the `e2e-` prefix: the kind
cluster, the project docker network (`spec.network.name`), and by
derivation the registry containers (`<network>-registry-<name>`) and the
certificate volume (`<network>-registry-tls`). `e2e::guard_name` in
[lib/helpers.bash](./lib/helpers.bash) refuses a destructive verb
against any other name, and every docker filter is anchored.

Shared mirrors are the exception the rule has to work around. Their
container prefix is the framework's — `lok8s-registry-<mirror name>` —
and bringing the shared network up REMOVES every container with that
prefix attached to it. On a developer machine the default shared network
(`lok8s-registries`, slot 200) carries another project's live mirrors.
So `shared-registries` names its own network (`e2e-shr-net`, slot 201)
and its own mirrors (`e2e-docker`, `e2e-quay`, which become
`lok8s-registry-e2e-*`). A scenario that opts into shared registries
must do the same.

## DNS

Each slot relies on `*.<n>.lok8s.dev` resolving to its slot subnet
via the porkbun zone (`lok8s.dev`). Without the wildcard DNS, mkcert
certificates fail and any in-cluster service references break in
confusing ways late in the run. The `e2e::require_dns` helper in
`lib/helpers.bash` checks this up front and skips the scenario if the
DNS isn't provisioned — except under `E2E_STRICT=1` (CI), where a
missing prerequisite fails instead, because a skipped CI leg is a green
check over an empty run.
