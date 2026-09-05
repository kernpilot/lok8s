# Testing lok8s

The test matrix for the repository, how to run each layer locally, and the
one environment hazard that bites everyone once. For the Playwright suite
that `lo init test` scaffolds into *your* project, see
[docs/guide/testing.md](docs/guide/testing.md) — this file is about testing
lok8s itself.

## The matrix

| Layer | What it proves | Runs in CI | Command |
|---|---|---|---|
| Go unit tests (core + full) | every `internal/**` package in isolation; external tools replaced by a fake `execx.Runner`. Run twice: without tags (the `lo` core build — exec render) and with `-tags inprocess` (`lo-full`); the in-process render tests skip on core | `go-tests` | `go test ./...` · `go test -tags inprocess ./...` (`make test test-full`) |
| Toolchain pin-drift gate | `internal/toolchain/pins.go` (kustomize API ↔ CLI, khelm, helm) equals `go.mod` and the generated `.bin/b.yaml` template carries the same pins; the b bootstrap verifies a checksum, never installs on mismatch, never follows plain http | `go-tests` (part of `go test`) | `go test ./internal/toolchain/` |
| Tree-drift gate | the cobra command tree matches the argsh usage array in `.lok8s/lo` (names, aliases, hidden, markers, short text) | `go-tests` (part of `go test`) | `go test ./internal/cli/ -run TestCommandTreeMatchesArgshUsage` |
| Assets-drift gate | the embedded mirror `internal/assets/lok8s/**` (canonical) is byte-identical to its `.lok8s/**` twin in both directions — addons, `drivers/*/cluster`, the inventory CRD mirror, `chat/`, `VERSION`; resync with `hack/sync-legacy-assets.sh` (`--from-legacy`, `--check`) | `go-tests` (part of `go test`) | `go test ./internal/assets/ -run TestEmbeddedMirrorMatchesLegacyTree` · `bash hack/sync-legacy-assets.sh --check` |
| CRD render fixture | `lo crds` output is byte-identical to the committed `operator/crds/*.yaml` | `go-tests` (part of `go test`); `unit-tests` (`lo crds check`) | `go test ./internal/crds/` · `bin/lo crds check` |
| Render gate | the in-process kustomize render (`internal/render`, the `lo-full` build) is byte-identical to the pinned exec pipeline (what `lo` core runs): `go test -tags inprocess ./internal/render/` byte-compares plain, Secret-generator and khelm local-chart fixtures against `.bin/kustomize` + `.kustomize/*` (skips without them); `hack/parity-build.sh` diffs `lo build` against the bash exec pipeline for both builds; the committed kubehz.dev domain must report `render unchanged` under `bin/lo` (core) and `bin/lo-full` | `go-tests` (`go test`, `parity-build` × 2); the committed-domain run is manual (see below) | `go test -tags inprocess ./internal/render/` · `bash hack/parity-build.sh` · `DEBUG=1 bin/lo build --domain <d>` and the same with `bin/lo-full` |
| Parity harnesses (10 × 2 builds) | the binary and `LO_IMPL=bash` agree byte-for-byte on stdout, stderr, rc (and written trees) for every covered invocation — for `bin/lo` (core) and `bin/lo-full`; every harness takes the binary as `$1` (**absolute path** — the harnesses `cd` into a synthetic project) | `go-tests` | `make build build-full && bash hack/parity-<name>.sh "$PWD/bin/lo" && bash hack/parity-<name>.sh "$PWD/bin/lo-full"` |
| golangci-lint (core + full) | `.golangci.yml` (standard set + misspell, unconvert, gocritic), with and without `--build-tags inprocess` | `go-tests` | `make lint lint-full` |
| bats unit suite | the frozen bash libraries, function by function (88 files under `tests/unit/`) | `unit-tests` | `./.bin/argsh test tests/unit/` |
| bats operator suite | the shell-operator hooks: the bash bodies directly, and the Go bodies through the `operator/hooks/*.sh` shims | `operator-tests` | `make build && ./.bin/argsh test tests/operator/` |
| ShellCheck + argsh-lint | every shell file under `.lok8s/` (**including `.lok8s/legacy/`**), `operator/hooks/`, `docs/.vitepress/`, `hack/`, `install/` | `shellcheck` | `bash hack/lint-shell.sh` (= `npm run lint`) |
| yamllint | `.lok8s/`, `operator/`, `.github/` | `yamllint` | — (CI action) |
| `lo-up` bundle | `docs/public/lo-up` is a byte-exact rebuild of `.lok8s/legacy/install/lo-up` at the pinned argsh revision | `loup-bundle` | `ARGSH_SRC=… .lok8s/legacy/install/build && git diff --exit-code docs/public/lo-up` |
| E2E `lo up --ci` | a real kind cluster + registries + Cilium bootstrap, then `tilt ci` builds, pushes and deploys the fixture app and waits for it to be Ready — once with the **Go** `lo` (`bin/lo` built in the job and first on PATH) and once with `LO_IMPL=bash` (the binary execs the frozen tree) | `e2e-lo-up` × 2 (matrix `lo_impl: go, bash`; needs shellcheck, unit, operator green) | see [E2E](#e2e-lo-up-ci) |
| Integration (Kind) | CRD install, schema rejection, `ClusterInventory` SSA round-trip, every kind served under `cluster.lok8s.dev` | `integration-tests` (push to `main` only) | — (workflow only) |
| bats e2e scenarios | the scenario dirs under `tests/e2e/` (`no-services`, `single-local-build`, `cache-mode`, `remote-lo`, `remote-ci`), each on its own `10.125.<slot>.0/24` | no (opt-in) | `ARGSH_ENV_E2E=1 ./.bin/argsh test tests/e2e/<scenario>/test.bats` |
| Go round-trip | ONE real provision → status → down → destroy with the **Go** orchestration against a synthetic kind cluster | **no — manual gate** | `bash hack/e2e-go-roundtrip.sh` |

The CI job set lives in `.github/workflows/ci.yml`: `shellcheck`, `yamllint`,
`unit-tests`, `go-tests`, `operator-tests`, `e2e-lo-up` (× 2),
`loup-bundle`, `integration-tests`. `.github/workflows/security.yml` adds
`govulncheck` and `gosec` (every PR, and weekly, over all three Go modules
with the root toolchain — the one the release builds with), plus the trivy
and ShellCheck-SARIF scans. Both files run on pull requests into `main`
and into `feat/**`. `hack/e2e-go-roundtrip.sh` is deliberately not wired
in.

### Which implementation each row proves

Two implementations exist ([AGENTS.md](AGENTS.md)); a green row says
nothing about the other one unless it names it.

| Row | Go `lo` (`bin/lo`, canonical) | frozen bash tree (`.lok8s/`) |
|---|---|---|
| Go unit tests, drift gates, render gate, golangci, govulncheck, gosec | yes | no |
| Parity harnesses | yes — diffed against | yes — the oracle |
| bats unit suite | no | yes |
| bats operator suite | yes (the `operator/hooks/*.sh` shims exec the binary) | yes (the bash bodies) |
| ShellCheck + argsh-lint, yamllint, `lo-up` bundle | no | yes |
| E2E `lo up --ci` | yes — the `go` matrix leg | yes — the `bash` leg (`LO_IMPL=bash` through the binary's shim) |
| Integration (Kind) | neither — `kubectl` against the CRDs only | neither |
| bats e2e scenarios (opt-in) | no — `tests/e2e/lib/helpers.bash` puts `.lok8s` first on PATH | yes |
| Go round-trip (manual) | yes | no |

## Go

```bash
make build            # bin/lo (core), stamped with .lok8s/VERSION
make build-full       # bin/lo-full (-tags inprocess)
make test test-full   # go test ./...  ·  go test -tags inprocess ./...
make vet vet-full     # go vet, both tag sets
make lint lint-full   # golangci-lint run (needs golangci-lint on PATH), both tag sets
go test ./internal/secrets/ -run TestEncrypt -v    # one package / one test
```

Packages with tests: `internal/{addons,audit,bootstrap,build,cli,config,
crds,deploy,domain,driver,driver/capi,driver/kkp,driver/kubehz,
driver/kubeone,driver/lo,env,gitops,hooks,image,inventory,kapply,kubehz,
lint,oidc,operator,provider/bridge,provision,recover,render,scaffold,secrets,tilt,toolchain}`.
Only `internal/execx` (the runner itself) and `internal/ui` have none.

Two builds, one tree: `lo` (core) renders through the exec pipeline, `lo-full`
(`-tags inprocess`) through the kustomize API in-process. A test that needs
the fake-runner seam for `kustomize` pins `LO_RENDER=exec` first
(`t.Setenv(render.ModeEnv, string(render.ModeExec))` — the build, addons,
bootstrap and registry-TLS fakes do) and therefore passes on both builds; a
test that asserts the in-process render guards itself with
`render.InProcessAvailable()` (skips on core) or lives in a
`//go:build inprocess` file (`internal/render/render_inprocess_test.go`).
`internal/render`'s own tests serve the exec generators from the test
binary: its `TestMain` calls `render.DispatchPlugin` exactly like `cmd/lo`
(a no-op on core), which is what lets the Secret/khelm fixtures run the real
plugin protocol hermetically. The registry TLS mint runs the imported Secret
generator in-process on BOTH builds (`render.SecretInProcess()`), so its
tests need no tag.

`internal/toolchain` is hermetic too: the b bootstrap is tested against an
`httptest` TLS server serving a generated tarball (checksum match, mismatch,
missing member, dry run, darwin, plain-http refusal), `b install` through the
`execx.Runner` seam, doctor through a `Probe` seam over placeholder
executables. Nothing reaches the network. The pin-drift test reads the
repo's `go.mod` — mutation-check it by editing a version there.

Two Go modules ship alongside the root module and have their own tests:

```bash
npm run chat:test     # ai/lochat  — go test -race
make -C kustomize test   # the kustomize exec plugins (also: lo kustomize test)
```

### How a port is tested

The pattern every ported package follows, so a new one reads like the rest:

1. **Read the bash first.** The `.lok8s/libs/<name>` (or driver/util) file is
   the spec. Exact strings, exit paths and ordering are the contract, not the
   intent behind them ("bash wins").
2. **Hermetic tests through `execx.Runner`.** Every external tool call goes
   through the `Runner` seam; the test installs a fake that records the
   `execx.Cmd` (argv, dir, env, stdin) and answers scripted output. The
   recorded argv **is** the assertion — the same oracle the bats suites
   assert on with their `KLOG` stubs. Nothing under `go test` may reach
   docker, kind, tilt, kubectl or the network.
3. **A parity harness section.** Add the invocation(s) to the matching
   `hack/parity-*.sh` (or a new one, modelled on `parity-test.sh`) so both
   implementations are diffed in CI. Cluster-touching verbs get stub
   binaries in the synthetic project's `.bin`; consent gates get a closed
   stdin.
4. **Mutation-check the test.** Revert the port (or break one string) and
   watch the test and the harness go red before trusting either. A green
   check whose failure would look like success proves nothing.

## The parity harnesses

```bash
make build
bash hack/parity-test.sh          # use, version, secrets, the general set
bash hack/parity-build.sh         # lo build: artifacts, split output, secret twins
bash hack/parity-configure.sh     # lint, kubeconfig, doctor
bash hack/parity-audit.sh         # lo audit: human, --json, --sarif
bash hack/parity-loop.sh          # tilt, image, env, hooks (read-only paths)
bash hack/parity-leaves.sh        # init, crds, addons, drivers, chat, ai
bash hack/parity-ops.sh           # deploy, recover, gitops (cluster-free)
bash hack/parity-kubehz.sh        # lo kubehz (config, routing, handover checks)
bash hack/parity-operator.sh      # lo operator <hook> vs the frozen bash hooks
bash hack/parity-orchestrate.sh   # up, down, clean, provision, destroy, bootstrap, status, registry
```

Each takes an optional path to the binary (default `bin/lo`) and needs the
vendored `.bin/argsh` plus the b-pinned toolchain (`b install`) because the
bash side really runs. `PARITY_KEEP=1` leaves a harness's work directory
behind for a post-mortem where supported. What each one covers, and the
divergences it allow-lists, is in
[docs/reference/go-migration.md](docs/reference/go-migration.md#parity-gates).

The Go-only eject-model surface (`lo assets`, `lo init project`, `--origin`,
`--no-eject`) has no bash twin and therefore no harness: its gate is
`go test ./internal/assets/ ./internal/cli/` (precedence, never-overwrite,
eject + marker, the six-way classification, update refusal, `--check` exit
codes, the scaffold). Every harness hands its synthetic project a full
`.lok8s` tree, so the resolver's precedence picks the local copy and the
harnesses are unaffected — keep it that way: a harness project WITHOUT a
`.lok8s` tree would eject into its work dir on the Go side only.

### The ambient-env hazard (read this once)

A developer shell in a lok8s project usually exports `PATH_BASE`,
`PATH_BIN`, `PATH_LOK8S`, `PATH_CLUSTERS`, `PATH_SECRETS` (direnv, mise, or
by hand). **Inherited, they silently redirect both implementations — and a
harness's writes — into that live project instead of the synthetic one.**
Every harness therefore starts with the same preamble:

```bash
unset PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS \
      DOMAIN_NAME KUBECONFIG …
export LC_ALL=C          # bash globs sort by LC_COLLATE; the Go port lists in byte order
```

Copy it into any new harness or ad-hoc comparison. Before running anything
that writes, check `env | grep -E '^(PATH_|DOMAIN_NAME|KUBECONFIG)'`. The
same applies to bats: `argsh test` forwards project env into its container.

## bats (the frozen bash tree)

```bash
./.bin/argsh test tests/unit/               # ~seconds, no externals
./.bin/argsh test tests/operator/           # hook logic; build bin/lo first for the shim tests
./.bin/argsh test tests/unit/build_test.bats   # one file
npm test                                    # both suites
```

`argsh test` runs bats + bats-support + bats-assert in the pinned argsh
container with `.bin/` mounted; no host bats install is needed. Outer env
reaches the container only through the `ARGSH_ENV_<X>` prefix (see
[tests/README.md](tests/README.md)). CI pins `PATH_BIN` to the workspace
`.bin` so `kustomize`-dependent render tests find the b-pinned binary.

The bats suites keep running against the frozen tree because
`LO_IMPL=bash`, the provider plugins and the `lo drivers` fallback still
execute it. A behaviour change lands in Go first; touch the bash only when
a parity harness would otherwise go red, and then in the same change.

## Shell lint

```bash
bash hack/lint-shell.sh      # = npm run lint; shellcheck (.shellcheckrc) + argsh-lint
```

File discovery lives in that script only, so the local run and CI can never
drift. The set is `.lok8s/`, `operator/hooks/`, `docs/.vitepress/`, `hack/`,
`install/` — `*.sh` plus every extensionless `#!/usr/bin/env argsh|bash`
script. **`.lok8s/legacy/` is linted on purpose**: the retired installer is
still rebuilt into the published `lo-up` bundle, and the retired hook bodies
are still the parity oracle for `lo operator`. Without a local shellcheck +
argsh-lint pair the run is forwarded to the digest-pinned argsh container.
A local `argsh lint` that finds neither tool exits 0 silently — do not trust
a green lint you did not watch install its tools.

## E2E (`lo up --ci`)

The CI job (`e2e-lo-up`) runs the `single-local-build` fixture end to end
on a GitHub runner, twice (matrix `lo_impl: go, bash`): trusts the
plain-HTTP `10.125.0.0/16` registries in the docker daemon, `b install`s
the full toolchain, builds `bin/lo` and puts it FIRST on PATH (the step
asserts `command -v lo` resolves to it), sets `LO_IMPL` to the matrix
value (only `bash` means anything to the binary: it execs `.lok8s/lo`),
then

```bash
# from tests/e2e/single-local-build, PATH_BASE = the fixture, PATH_LOK8S/PATH_BIN = the repo
export DOMAIN_NAME=127.lok8s.dev LOK8S_CLUSTER_NAME=e2e-slb LOK8S_HOST_PORTS=false TILT_PORT=10427
timeout 720 lo up --ci --timeout 300s
kubectl get pod -l app=app -o jsonpath='{.items[0].status.phase}'   # must be Running
```

`lo up --ci` exits non-zero unless the whole stack converges, so the step's
exit status is the result. Locally the same scenario runs through the bats
wrapper (`ARGSH_ENV_E2E=1 tests/e2e/run.sh single-local-build`); every
scenario skips without `E2E=1` so a plain `argsh test` never pulls in a
five-minute cluster lifecycle. Slot allocation is in
[tests/e2e/SUBNETS.md](tests/e2e/SUBNETS.md).

## The Go round-trip (manual gate)

`hack/e2e-go-roundtrip.sh` is the only place the **Go** `lo provision`
creates a real cluster with real registries on a real docker network and
tears every piece down again. The parity harnesses only ever reach stubs;
this is the exit gate for orchestration changes, run by hand on a machine
with docker + kind + the `.bin` toolchain:

```bash
make build && bash hack/e2e-go-roundtrip.sh   # [path-to-lo]
```

Its isolation rules, so it is safe next to whatever else runs on the
machine:

- everything is named `lo-e2e-<random>`: the domain, the cluster, the
  docker network, the registry containers and volumes;
- an unused `10.213.x.0/24` is picked and **verified free** against every
  docker network's IPAM before use;
- `--domain` is passed explicitly on every call — never `clusters/.active`;
- the pre-existing kind clusters are snapshotted before and asserted
  unchanged after;
- an `EXIT` trap tears the synthetic cluster down on any exit, then removes
  the project docker network the framework leaves behind by design;
- `LOK8S_NONINTERACTIVE=1` so no gate prompts and the passthrough output
  is deterministic.

It is not in CI on purpose; run it before merging anything that touches
`internal/provision`, `internal/driver/lo`, or the registry lifecycle.

## Rules that came from incidents

- **Never run a destructive `lo`, `kind`, `docker` or `tilt` verb from a
  test or a harness against ambient state.** Stub the binary in a synthetic
  `.bin`, or use the round-trip script's naming + snapshot discipline.
  Existing kind clusters, Tilt sessions and registry containers on a dev
  machine are not yours.
- **Unset `PATH_*` first** (above). Half of the "the harness rewrote my
  project" reports were this.
- **Mutation-check every regression test**: revert the fix, watch it fail.
  Verify artifacts (files, argv logs), not exit codes.
- **Prose goes through a file, not a shell argument.** Backticks in a
  double-quoted `gh --body` / `git -m` string are command-substituted; use
  `--body-file` / `-F`.
- **Move, never delete.** A retired file moves under `.lok8s/legacy/`; the
  frozen tree is only ever deleted by an explicit owner decision.
