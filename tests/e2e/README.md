# lok8s e2e tests

End-to-end scenarios that spin up real kind clusters via `lo` and
exercise the full provision → bootstrap → tilt pipeline. Each
scenario lives in its own directory and runs in its own slot under
`10.125.0.0/16` (see [SUBNETS.md](./SUBNETS.md)).

## Running

```bash
# All scenarios (via run.sh wrapper):
E2E=1 tests/e2e/run.sh

# Single scenario:
E2E=1 tests/e2e/run.sh no-services

# Via argsh directly:
E2E=1 argsh test tests/e2e/no-services/test.bats

# Unit tests only (no clusters):
argsh test tests/unit/
```

Without `E2E=1`, every e2e scenario skips at setup time. The opt-in
gate prevents `argsh test` from accidentally pulling in 5-minute
cluster lifecycles when you only wanted unit tests.

## Prerequisites

Tests run via [argsh](https://arg.sh) which provides bats + bats-support
+ bats-assert. No vendored bats submodules — just `argsh test`.

Additional prereqs (scenarios skip automatically if missing):

- `docker` (daemon must be running)
- `kind`
- `kustomize`
- `yq`
- `tilt`
- `dig` (for DNS preflight)
- Wildcard DNS for the scenario's slot resolves
  (`*.<slot>.lok8s.dev` → `10.125.<slot>.x`)

## Scenario layout

```
tests/e2e/
├── run.sh                  # discover + run scenarios
├── README.md
├── SUBNETS.md              # slot allocation
├── lib/
│   └── helpers.bash        # shared bats helpers
├── no-services/            # slot 126 — provision smoke
│   ├── test.bats
│   ├── Tiltfile
│   └── clusters/126.lok8s.dev/
│       └── cluster.lok8s.yaml
├── single-local-build/     # slot 127 — full build/deploy
│   ├── test.bats
│   ├── Tiltfile
│   ├── app/                # local-build service source
│   └── clusters/127.lok8s.dev/
│       ├── cluster.lok8s.yaml
│       └── targets/app/
└── cache-mode/             # slot 128 — build:false cache pre-pull
    ├── test.bats
    ├── Tiltfile
    ├── services.yaml
    └── clusters/128.lok8s.dev/
        ├── cluster.lok8s.yaml
        └── targets/upstream/
```

## How a scenario works

Each scenario directory IS a `PATH_BASE` for `lo`. Helpers point
`PATH_BASE` at the scenario dir, leave `PATH_LOK8S`/`PATH_BIN`
pointing at the project root, and set `PATH_CLUSTERS` to the
scenario's local `clusters/`. From `lo`'s perspective the scenario
looks like a complete project with its own clusters tree, services
config, secrets dir, and kubeconfigs — but the framework code is
sourced from the parent repo so framework changes are picked up
immediately by re-running tests.
