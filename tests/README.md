# lok8s tests

Three layers:

| Layer       | Path                | Speed     | What it exercises                                  |
|-------------|---------------------|-----------|----------------------------------------------------|
| unit        | `tests/unit/`       | seconds   | Library functions in isolation (stubbed externals) |
| operator    | `tests/operator/`   | seconds   | shell-operator hooks (CRD reconcile logic)         |
| e2e         | `tests/e2e/`        | minutes   | Real kind clusters via `lo`, full lifecycle        |

Fixtures are in `tests/fixtures/`.

## Running

Tests run via [argsh](https://arg.sh), which ships bats + bats-support
+ bats-assert in a container with `.bin/` on PATH and project env
vars forwarded. No host install of bats, yq, or jq is required.

```bash
# Unit tests — fast, no externals
argsh test tests/unit/

# Operator tests — fast, hook logic only
argsh test tests/operator/

# A single unit test file
argsh test tests/unit/bootstrap_test.bats

# e2e (opt-in, spins up real kind clusters)
E2E=1 argsh test tests/e2e/no-services/test.bats

# e2e — all scenarios via the wrapper
E2E=1 tests/e2e/run.sh
```

Env vars set in the outer shell reach the test **only** via the
`ARGSH_ENV_<X>` prefix; the prefix is stripped when crossing into
the argsh container:

```bash
ARGSH_ENV_E2E=1 argsh test tests/e2e/no-services/test.bats
#             └─ inside the container, ${E2E} = 1
```

Plain `E2E=1 argsh test …` is **not** forwarded when argsh is
running in docker mode. It does work when `bats` is on the host
`PATH` (argsh skips docker), so use the prefix to stay portable.

Without `E2E=1`, every e2e scenario skips at setup time.

## Layout

```
tests/
├── README.md              ← this file
├── test_helper.bash       ← shared bats setup (loaded by unit tests)
├── fixtures/              ← YAML specs, target kustomizations, services.yaml
├── unit/                  ← *_test.bats files, one per library
├── operator/              ← shell-operator hook tests
└── e2e/                   ← scenario dirs + run.sh wrapper; see e2e/README.md
```

## Adding tests

**Unit test**: add a `*_test.bats` under `tests/unit/`. Start from
`test_helper.bash` — load it, call `setup_tmpdir`, source the
library-under-test and its utility deps (see
`tests/unit/build_test.bats` for the pattern). Stub every external
(`yq`, `kubectl`, `docker`, `kustomize`, `kubehz::*`) — unit tests
never touch a real cluster or network.

**E2e scenario**: pick a free slot in `tests/e2e/SUBNETS.md`, create
`tests/e2e/<name>/{test.bats, Tiltfile, clusters/<slot>.lok8s.dev/}`,
add a gate (`e2e::require_e2e_enabled`) and scenario-specific tool
requires at the top of `setup_file`. See
[`tests/e2e/README.md`](e2e/README.md) for the full contract.

## CI

GitHub Actions runs `tests/unit/` and `tests/operator/` on every
push — see `.github/workflows/ci.yml`. E2e scenarios do not yet run
in CI (requires a Docker-capable runner with DNS setup).
