# The Go `lo` binary

`lo` is a single static Go binary (`cmd/lo`, `internal/**`). Every `lo`
command runs natively in it. The [argsh](https://github.com/arg-sh/argsh)
implementation the binary was ported from stays in the repository under
`.lok8s/` as a **frozen reference**: still runnable, still linted, still
diffed against the binary in CI, but no longer where features land.

This page is the map: what the binary runs, where it still calls out to
bash or to an external tool, how to force the bash implementation, the
gates that keep the two in agreement, and the places where the port
deliberately does not match the bash.

## Install

The binary is a GitHub release asset per platform (linux/darwin × amd64/arm64),
with a `checksums.txt` for every asset. See [Getting Started](/guide/#installation)
for the download-verify-run steps, or use `install/lo-install.sh` from the
same release (it verifies the archive against `checksums.txt` before
extracting; `--dry-run` shows the plan).

Local builds (`make build` → `bin/lo`) stamp the embedded
`internal/assets/lok8s/VERSION` (its synced twin is `.lok8s/VERSION`);
release builds stamp the git tag (`lo --version`). The binary never reads a
VERSION file from the project tree.

## What runs where

```text
$ lo <command>
   │
   ├─ LO_IMPL=bash set? ──yes──▶ exec bash .lok8s/lo <command> …   (verbatim argv)
   │
   └─ no ──────────────────────▶ Go implementation (internal/cli/cmd_<command>.go)
```

### Command inventory

All of these are native. The tree mirrors the argsh usage list in `.lok8s/lo`
one-to-one (a `go test` gate enforces it — see [Parity gates](#parity-gates)),
plus three commands that exist only in the binary.

| Group | Commands | Go packages behind them |
|---|---|---|
| Cluster lifecycle | `up`, `down`, `clean`, `provision`, `bootstrap`, `build`, `deploy`, `destroy`, `recover` | `internal/provision`, `internal/driver/{lo,capi,kubeone,kkp,kubehz}`, `internal/bootstrap`, `internal/build`, `internal/deploy`, `internal/recover`, `internal/kapply` |
| Configure & inspect | `use`, `kubeconfig`, `init`, `lint`, `audit`, `status`, `doctor`, `trust`, `version` | `internal/domain`, `internal/oidc`, `internal/scaffold`, `internal/lint`, `internal/audit` |
| Integrations | `chat`, `ai`, `gitops`, `kubehz`, `tilt` | `internal/gitops`, `internal/kubehz`, `internal/tilt` |
| Components | `kustomize`, `registry`, `image`, `addons`, `secrets`, `drivers` | `internal/driver/lo` (registries), `internal/image`, `internal/addons`, `internal/secrets` |
| Internal (hidden from `--help`) | `hooks`, `env`, `k8s`, `crds` | `internal/hooks`, `internal/env`, `internal/crds` |
| Go-only | `mcp` (native MCP server), `operator` (the shell-operator hook bodies), `assets` (the embedded framework assets: list, eject, diff, update) | `internal/cli/cmd_mcp.go`, `internal/operator`, `internal/assets` |

`mcp`, `operator` and `assets` have no twin in the argsh usage list, so they
are allow-listed by name in `internal/cli/root.go` (`goOnlyCommands`) with a
reason each. The same holds for the Go-only additions to existing commands:
`lo init project`, `lo addons --origin`, `lo drivers --list --origin`, the
global `--no-eject` flag — see [Embedded assets](#embedded-assets-the-eject-model). `lo mcp` replaces the argsh `mcp` builtin; the shipped
`.mcp.json` still launches the builtin (`.lok8s/lo mcp`) until that switch is
made deliberately — see [`lo mcp`](/reference/cli#lo-mcp).

### What still calls bash

The binary is the entrypoint for everything, but three seams still run bash
code from the framework tree. They are deliberate, hermetic under test
(every child goes through the `execx.Runner` seam), and documented here so
nobody mistakes them for unported commands.

| Seam | Where | Why |
|---|---|---|
| **Providers** (`.lok8s/providers/<name>/main`, e.g. `hetzner`) | `internal/provider/bridge` (provision dispatch: `up`, `provision`, `destroy`, `recover`), `lo doctor`'s provider section, `lo recover`'s rebuild | Providers are bash plugins that source the argsh runtime. Each contract call runs as a `bash -c` child over the ORIGINAL libs; state lives at the cloud or on disk, never in shell variables, so per-call loading is correct. |
| **`lo drivers <name> …`** for a driver directory with no Go twin | `internal/cli/cmd_drivers.go` → `Shim` | A name that exists only as `.lok8s/drivers/<name>/main` is handed to the argsh implementation with argv verbatim; `--list` prints the union of both worlds. |
| **`LO_IMPL=bash`** | `cmd/lo/main.go` → `internal/cli/shim.go` | The whole-process escape hatch below. |

::: warning Custom bash drivers and the provision dispatch
The Go provision dispatch (`lo up` / `provision` / `destroy` / `status` /
`bootstrap`) resolves the spec's `kind:` against the **Go driver registry**
(`lo`, `capi`, `kubeone`, `kkp`, `kubehz`). A custom driver that exists only
as `.lok8s/drivers/<kind>/main` is reported as `Unknown cluster kind` by the
binary. It still works through `LO_IMPL=bash lo provision …` and through
`lo drivers <kind> …`. The [Driver Contract](/reference/kind-contract)
describes the bash contract that the frozen tree honours; a Go driver
contract is not published yet.
:::

### Core and full

The binary ships in **two builds from one tree**, selected by the
`inprocess` build tag (`internal/render/core.go` vs `inprocess.go`):

| | `lo` — **core** (the default: `make build`, `lo-<os>-<arch>.tar.gz`) | `lo-full` (`make build-full` = `-tags inprocess`, `lo-full-<os>-<arch>.tar.gz`, `lo-install.sh --full`) |
|---|---|---|
| The render | **execs** the pinned `kustomize` binary (`.bin` first, then `PATH`) with `KUSTOMIZE_PLUGIN_HOME` defaulted to `<project>/.kustomize`, where the two exec generators live as b-installed binaries: khelm's `ChartRenderer` and the `kustomize-secret` `Secret` plugin | the kustomize API (`krusty`) **in-process**, both generators served by the binary itself through the self-exec plugin home ([below](#in-process-rendering)) |
| Linked in | all first-party logic, the `secrets.lok8s.dev` generator (imported — the registry TLS mint runs it in-process on both builds; it is ours and small) | the same, plus `sigs.k8s.io/kustomize/api`, khelm v2 and helm v3 |
| Size (linux/amd64, stripped) | **49.8 MB** | **123.1 MB** |
| `LO_RENDER` | unset/`exec` = the exec pipeline (the only one); `inprocess` is an **error** naming lo-full (`LO_RENDER=inprocess: this is lo core … install lo-full`) | unset/`inprocess` = in-process; `exec` = the subprocess pipeline for an A/B |
| Needs in the project | `.bin/kustomize` + `.kustomize/{khelm…/ChartRenderer, secrets.lok8s.dev/…/Secret}` — what [`lo init toolchain`](cli.md#lo-init) installs, pinned; `lo doctor` fails when they are missing | nothing for the render (the toolchain is still needed for kubectl/kind/tilt); `lo doctor` only warns about absent render tools |
| `lo --version` | `lo version 0.3.0 (core)` | `lo version 0.3.0 (full)` |

Everything else — every command, the parity harnesses, the tests — is the
same code. `go.mod` keeps khelm and the kustomize API for both; only the
tag-gated imports decide what is linked. The pins that keep the two
byte-identical live in **`internal/toolchain/pins.go`**: `KustomizeAPI`
(`v0.21.1`, what lo-full links) ↔ `KustomizeCLI` (`v5.8.1`, what core execs
and the b.yaml template pins — the `kustomizeAPIToCLI` table encodes the
release pairing), `KhelmVersion` (`2.8.0`: the library and the
`ChartRenderer` binary) and `HelmVersion` (`3.21.2`). `go test
./internal/toolchain/` fails when `go.mod`'s kustomize/api, khelm or helm
version moves without the pins, when the API↔CLI table lacks the pinned
API, or when the generated `.bin/b.yaml` template stops carrying the pins —
bumping any one side alone is a red build.

CI runs every gate against both builds (`go build`/`vet`/`test`/golangci
with and without `-tags inprocess`; all ten parity harnesses against
`bin/lo` — the exec path, i.e. the same pinned kustomize + plugins the bash
side runs — and against `bin/lo-full`; the goreleaser snapshot asserts both
archives). Locally: `make build build-full test test-full vet vet-full lint
lint-full`.

### External tools still exec'd

The port swapped shell for Go and, since phase 7, the kustomize renderer
too — on the lo-full build (see [In-process rendering](#in-process-rendering)
below); lo core keeps kustomize as a subprocess by design. The following
remain subprocesses, resolved through the project's `.bin/` first and `PATH`
second (`internal/execx.Look`):

| Tool | Used by | Why not in-process |
|---|---|---|
| `kustomize` (+ the `khelm` and `secrets.lok8s.dev` exec plugins under `.kustomize/`) | **lo core: every render** (`lo build`, the addon render, the KubeOne addon staging) — the pinned binary and the two b-installed plugins `lo init toolchain` provisions. lo-full: only `LO_RENDER=exec` (the A/B escape hatch), `lo kustomize` (which builds the standalone plugin for the bash path), and `LO_IMPL=bash` | Core stays small and exec-only on purpose (the owner decision behind the two builds); lo-full links the same releases. Both are byte-identical — the pins are drift-tested. |
| `yq` | `lo build` split mode (the YAML stream transforms and per-Secret re-renders), `lo env services` (the deep-merge the Tiltfile consumes) | yq's emitter has opinions (`---` on every non-first document, sequence-dash indentation) that `gopkg.in/yaml.v3` does not reproduce byte-for-byte. Field *extraction* from YAML is native everywhere. |
| `sops` | `lo build` split-mode Secret twins | The `.enc` files must stay interoperable with the sops CLI. (`lo secrets` itself uses the sops **library** in binary mode; the files it writes are the same format.) |
| `kubectl`, `kind`, `docker`, `tilt`, `clusterctl`, `kubeone`, `hcloud`, `mkcert`, `envsubst` | drivers, deploy, registries, tilt, trust | These are the tools lok8s orchestrates; calling them as-is keeps "what lok8s runs is what you'd run by hand" true. |

**Renderer-swap roadmap.** Moving yq/sops in-process is deferred by the
*renderer-drift rule*: a swap is allowed only once byte-parity with the
pinned tool's output is proven for the committed domains, not just the parity
fixtures (`internal/build/split.go` carries the TODO). The CRD render
(`lo crds`) was the first precedent — a native `yaml.Node` transform whose
output is byte-identical to the former `yq eval` render, with the committed
CRDs as the parity fixture. The kustomize render is the second (below).

### In-process rendering

Phase 7 (WP3 + WP4) moved `kustomize build` — and both exec generators every
lok8s render depends on — into the binary; since the core/full split this
is the **lo-full** build (`-tags inprocess`). There, `lo build`, the
bootstrap engine's addon render (`internal/addons`), the KubeOne driver's
addon staging, the legacy `lo k8s` paths and the Lo driver's registry TLS
mint no longer exec `kustomize`, `khelm` or the `Secret` plugin, and need
neither a `.kustomize/` directory nor `KUSTOMIZE_PLUGIN_HOME`. (On lo core
the registry TLS mint is the one piece that stays in-process — the imported
generator — while every kustomize render execs the pinned binary.) The
output is byte-identical to the exec pipeline's — that was the gate, not a
goal.

**What runs where** (`internal/render`):

| Piece | In the binary | Pinned to |
|---|---|---|
| `kustomize build --enable-alpha-plugins [--enable-exec] <dir>` | `sigs.k8s.io/kustomize/api/krusty`, driven option-for-option like the kustomize CLI's build command (`Reorder` unspecified, `EnabledPluginConfig(BploUseStaticallyLinked)`, the builtin helm inflator enabled with the default `helm` command, `KUSTOMIZE_ENABLE_MANAGEDBY_LABEL` honoured, `ResMap.AsYaml()` as the bytes) | `api v0.21.1` + `kyaml v0.21.1` — the modules behind the **kustomize v5.8.1** binary the repo pins in `.bin/b.yaml` (kubehz-cluster pins `v5.8.1` explicitly) |
| `secrets.lok8s.dev/v1/Secret` | `kustomize/plugins/secret` **imported** (go.mod: `replace github.com/kernpilot/lok8s/kustomize => ./kustomize`) — the same `secret.Run` the standalone `kustomize-secret` binary's `main` calls; the nested module keeps building on its own for the release assets and the bash path | the repo's own module, one source tree |
| `khelm.mgoltzsche.github.com/v2/ChartRenderer` | `github.com/mgoltzsche/khelm/v2/pkg/{config,helm}` as a **library**, replicating khelm's kustomize-plugin `main` (config from `KUSTOMIZE_PLUGIN_CONFIG_STRING`, `helm.NewHelm()` from the helm `cli.New()` settings, `KHELM_TRUST_ANY_REPO`/`KHELM_DEBUG`/`HELM_DEBUG`, `ReadGeneratorConfig`, `Render`, then khelm's `output.Marshal` — the kyaml encoder, one `Encode` per RNode document — repeated verbatim because that package is internal to khelm) | **khelm v2.8.0** — the exact release the repo pins as the `ChartRenderer` binary (kubehz-cluster: "the pair PROVEN to reproduce the committed artifacts byte-for-byte"); its go.mod requires **helm.sh/helm/v3 v3.21.2**, pinned in the root go.mod as well, so the chart inflation is the same helm code the binary was built from |

**The self-exec plugin home.** kustomize's exec-plugin protocol is a
subprocess: `<pluginhome>/<group>/<version>/<kind>/<Kind> <cfgfile>` run in
the kustomization directory with `KUSTOMIZE_PLUGIN_CONFIG_STRING` in the
environment. The binary keeps that protocol — nothing in the kustomize API
is patched — and points it at itself: on the first render of a process
`internal/render` creates a temp plugin home holding the two plugin paths
as symlinks to `os.Executable()` (a copy where symlinks are unavailable),
sets `KUSTOMIZE_PLUGIN_HOME` to it for the duration of the run, and
`main` dispatches on `argv[0]` **before anything else** (`render.DispatchPlugin`:
`…/secret/Secret` → the imported generator, `…/chartrenderer/ChartRenderer`
→ the khelm library). The child is therefore `lo` again, started under the
plugin's name; a non-zero exit fails the build with the child's stderr in
the message exactly as before (`secret plugin: …`, `khelm: …`). The home is
removed on exit (`render.Cleanup`).

The per-render environment the exec pipeline handed to the kustomize child
(`KUBECONFIG`, `KHELM_TRUST_ANY_REPO=true`, `LOK8S_SECRETS_DISABLE`, the
toolchain on `PATH`, an addon entry's `env:` overrides) is `render.Options.Env`.
The plugin children inherit the process environment, so the overlay is
installed in it for the duration of the run and restored afterwards, under
a package mutex — concurrent renders (the bootstrap DAG) serialize on the
render only; the apply and wait phases stay parallel.

**Chart cache.** `helm.NewHelm()` reads the same `HELM_*` environment the
khelm binary read, so chart downloads and repository indexes land in the
same helm cache (`$HELM_REPOSITORY_CACHE`, else `$XDG_CACHE_HOME/helm/
repository`, else `~/.cache/helm/repository`) and a warm cache stays warm
across the switch. The `KHELM_TRUST_ANY_REPO=true` the pipeline always set
still decides whether an undeclared repository is trusted.

**`LO_RENDER=exec`** restores the subprocess pipeline everywhere
(`internal/render` execs the pinned `kustomize` from `.bin` with
`KUSTOMIZE_PLUGIN_HOME` defaulted to `<project>/.kustomize` for `lo build`,
and the registry TLS mint execs the built `Secret` plugin as before). Use
it to A/B a render: `lo build` promotes `artifacts.yaml` only when the bytes
change, so a `DEBUG=1 lo build` that reports `render unchanged` under both
settings is the proof. An unknown value is rejected (`LO_RENDER: unknown
value`).

**The gate** (all held at the switch, 2026-09-03): `hack/parity-build.sh`
green (bash exec vs Go in-process, byte-diffed); kubehz-cluster's committed
`kubehz.dev` — 244 documents, the Secret generator cache-first, 20+ khelm
charts, envsubst — `render unchanged` with identical SHA-256 under the
default and under `LO_RENDER=exec`, and still `render unchanged` with
`KUSTOMIZE_PLUGIN_HOME=/nonexistent` (where the exec pipeline fails to find
its plugins — the proof the in-process dispatch was taken);
`internal/render`'s tests byte-compare the in-process render with the
pinned binaries in the repo's `.bin`/`.kustomize` for a plain
kustomization, the Secret generator and a local-chart `ChartRenderer`;
`internal/driver/lo`'s in-process mint test verifies the leaf (SANs, CA
signature, key match) against a throwaway CAROOT.

What did **not** move: `yq` (split-mode transforms, `lo env services`) and
`sops` — WP5. `lo kustomize {build,test,clean,list}` still manages the
standalone plugin under `.kustomize/` for the frozen tree (and for a core
build without `b`), and `lo doctor` still reports `KUSTOMIZE_PLUGIN_HOME` /
the built plugin because `LO_IMPL=bash`, the provider plugins, lo core and
`LO_RENDER=exec` need them (that text is unchanged; `hack/parity-configure.sh`
diffs it — the pinned-toolchain section doctor adds is gated on the
`lo init toolchain` marker or `--toolchain`, so the diff stays strict).
`kubectl kustomize` in `lo kubehz deploy` is kubectl's embedded kustomize,
not the pinned one, in both implementations. The in-process render grew the
binary from 49 MB to 123 MB (helm + client-go + the kustomize API) — which
is why it is the `lo-full` build and `lo` core stays at ~50 MB with the
exec pipeline; the root module's `go` directive is `1.26.0` because khelm
v2.8.0 requires it.

## Embedded assets: the eject model

Phase 7 / WP1 (done). The framework's first-party **data** ships inside
the binary; a project no longer needs a synced `.lok8s/` tree for it.

**What is embedded** — `internal/assets` embeds a committed mirror,
`internal/assets/lok8s/**` (113 files, ~168 KB):

| Mirror path | Content | Materialization unit |
|---|---|---|
| `addons/**` | all 24 bootstrap addons, byte-identical, vendored `chart/` dirs included | one unit per addon (`addons/<name>`) |
| `drivers/lo/cluster/**` | kind config, CoreDNS, registry and expose templates | `drivers/lo/cluster` |
| `drivers/kubeone/cluster/**` | the KubeOne core template | `drivers/kubeone/cluster` |
| `drivers/capi/cluster/**` | the CAPI core + provider templates | `drivers/capi/cluster` |
| `libs/inventory/manifests/` | the ClusterInventory CRD mirror | `libs/inventory/manifests` |
| `chat/` | `lo chat` defaults | `chat` |
| `VERSION` | the fallback for an unstamped build | — (never ejected) |

The embedded copy is canonical. The repo's `.lok8s/**` twin stays (the
frozen bash implementation and the parity harnesses read it) and is held
byte-identical by `hack/sync-legacy-assets.sh` (mirror → `.lok8s`;
`--from-legacy` the other way; `--check` diffs) and the Go test
`TestEmbeddedMirrorMatchesLegacyTree`, which fails on any divergence in
either direction.

**Resolver and precedence** — `assets.Resolve(paths, rel)` returns the
on-disk path for a rel like `addons/cilium` or
`drivers/lo/cluster/registry`: the project's `.lok8s/<rel>` when it
exists (whatever its content), else the embedded copy. `assets.Peek` is the
same lookup without side effects. `lo` never overwrites an existing local
file; the only writer of existing files is `lo assets update`. Every
runtime read of the framework tree in the binary goes through the
resolver: the bootstrap entry parser (`internal/bootstrap`, and the
twin parsers in `internal/lint` and `internal/audit`, which peek), the
`lo` driver's CoreDNS/registry/expose templates, the KubeOne core
template, the CAPI templates, the inventory CRD, the chat defaults, and
the version (`assets.Version()`: ldflags, else the embedded `VERSION`).
What does NOT go through it, on purpose: the bash seams (`.lok8s/lo`,
`.lok8s/drivers/<name>/main`, `.lok8s/providers/*`) — those are the frozen
implementation, not assets — and `lo crds generate`'s write of the
`.lok8s` CRD mirror, which is a generator output in this repo.

**Eject on first use** (the default) — when a consumer needs an asset and
the project holds no copy, the whole unit is written into `.lok8s/<unit>/`
(atomically: a sibling temp dir renamed into place) with a `.lo-origin`
marker and one `[assets] ejected <rel> -> .lok8s/<rel>` line on stderr.
Read-only commands (`lint`, `audit`, the `addons` listing, `assets`
itself) never eject. Opt-outs: `--no-eject` / `LO_ASSETS_EJECT=never`
(the embedded copy is served from a per-run temp dir, nothing is written);
`lo assets eject --check` and `lo assets diff --check` for CI. The
`.lo-origin` marker:

```yaml
# .lo-origin — written by lo when it ejected this asset. Do not edit.
lo: 0.1.0
ejectedAt: 2026-09-03T10:00:00Z
files:
  chart.yaml: sha256:…
  values.yaml: sha256:…
```

**Three-way diff** — `lo assets diff` classifies every file of a unit
from ORIGIN (marker) vs LOCAL vs EMBEDDED: `unchanged`, `local modified`,
`lo updated`, `both` (conflict), `local-only`, `builtin-only`; a copy with
no marker (a tree vendored by `b env sync`) classifies its differences as
`local modified` because nothing proves otherwise. `lo assets update`
applies the embedded copy only when local == origin for every file (else
`--force`). The full surface, the state table and the JSON shape are in
the [CLI reference](/reference/cli#lo-assets).

**Project marker** — `config.ResolvePaths` recognizes a project by
`clusters/` or `lok8s.yaml` (what `lo init project` writes); `.lok8s/lo`
stays a fallback marker during the coexistence with the frozen tree.

**Parity** — the bash implementation reads `.lok8s/**` from disk and has
no embedded copy, so `lo assets` has no twin and no parity harness (its
gate is `go test ./internal/assets/ ./internal/cli/`). Every harness
gives its synthetic project a full `.lok8s` tree, so precedence picks the
local copy there and every existing harness stays green unchanged. Two
Go-only affordances are opt-in for exactly that reason: the origin column
of `lo addons` / `lo drivers --list` is behind `--origin`, and the doctor
summary line is omitted for a complete vendored tree with no ejected unit
and no drift (the one layout the bash implementation also runs in), so
`hack/parity-configure.sh`'s strict doctor diff holds.

## The framework tree still ships

A project synced with `b env add github.com/kernpilot/lok8s#<profile> &&
b install` still carries `.lok8s/` and the pinned toolchain in `.bin/`,
and the binary honors that tree first (precedence above). What the binary
still needs from it is the bash that has no Go twin yet: the Tilt
extension (`.lok8s/tilt/`), the provider plugins, and the frozen
entrypoint + libraries the parity gates and `LO_IMPL=bash` run. The data
files it used to need (addons, driver templates, the CRD mirror, the chat
defaults, `VERSION`) are embedded now. What changed for the user is only
the entrypoint: `lo` on your `PATH` is the binary instead of `.lok8s/lo`.

The `core` profile's `.bin/b.yaml` declares the binary
(`github.com/kernpilot/lok8s`, asset `lo-*.tar.gz`, alias `lo`), so `b
install` fetches it into `.bin/` alongside the rest of the toolchain. The
[Toolchain](/guide/toolchain#what-b-manages-today-and-what-it-will) page
states what `b` still manages today versus the intended end state.

## `LO_IMPL=bash`: the escape hatch

```sh
LO_IMPL=bash lo build          # this one call: bash implementation, Go skipped
export LO_IMPL=bash            # this shell: every command via .lok8s/lo
```

`LO_IMPL=bash` bypasses the Go implementation entirely, for every command.
The binary replaces itself with `bash .lok8s/lo <args>` after preparing the
environment the way the project's `.envrc` would (toolchain and framework
directories on `PATH`, `KUSTOMIZE_PLUGIN_HOME` defaulted). Nothing is
parsed on the way; argv reaches the argsh implementation untouched.

Use it when a command misbehaves: if the bash side is right and the Go side
is wrong, that is a parity bug — please report it with the command line and
both outputs. There is no `LO_IMPL=go`; the binary is the default.

## Parity gates

A port is not "done" when it compiles. Every command is held to the bash
implementation by differential tests under `hack/`: each covered invocation
runs **both** implementations (the binary, and the same binary with
`LO_IMPL=bash`) against a synthetic project and diffs stdout, stderr, and
exit codes byte-for-byte — and, where a command writes files, the resulting
trees.

| Gate | Surface | Notes |
|---|---|---|
| `hack/parity-test.sh` | `use`, `version`, `secrets`, and the general ported set | the original harness; `lo version` intentionally drops the `bash` row (allow-listed) |
| `hack/parity-build.sh` | `lo build` | artifacts bytes, split-dir file list, non-Secret split bytes; Secret twins compared by presence + `sops:` marker (sops mints a fresh data key per encrypt) |
| `hack/parity-configure.sh` | `lint`, `kubeconfig`, `doctor` | isolated `HOME`; doctor's environment-driven lines must match since both run in the same environment |
| `hack/parity-audit.sh` | `lo audit` | human, `--json`, `--sarif` for every check family |
| `hack/parity-loop.sh` | `tilt`, `image`, `env`, `hooks` | read-only / error paths; stub `tilt`/`kubectl`/`docker`/`kind` in the synthetic `.bin` |
| `hack/parity-leaves.sh` | `init`, `crds`, `addons`, `drivers`, `chat`, `ai` | stateful sections get one project clone per implementation and byte-diff the trees |
| `hack/parity-ops.sh` | `deploy`, `recover`, `gitops` | cluster-free paths; stub `kubectl`, a scripted `mock` provider whose rebuild refuses outside `CLOUD_DRY_RUN` |
| `hack/parity-kubehz.sh` | `lo kubehz` | config validation, usage errors, hosting-axis routing, handover bundle checks; no api tokens set |
| `hack/parity-operator.sh` | `lo operator <hook>` vs the frozen bash hooks | `--config` bytes and stubbed `kubectl`/`clusterctl` call logs |
| `hack/parity-orchestrate.sh` | `up`, `down`, `clean`, `provision`, `destroy`, `bootstrap`, `status`, `registry` | stub `tilt`/`kind`/`docker`/`kubectl`; consent gates driven with closed stdin; `LO_RENDER=exec` pinned so the registry TLS mint fails on the missing plugin binary in both (D19) |

All ten run in CI on every push (`go-tests` job), after `go build`, `go
vet`, `go test` and golangci-lint. Deliberate divergences are allow-listed
per check (a regex of lines permitted to differ, or the rc pair `bash 2 /
go 1` for parse errors) — anything else that differs fails the build.

`go test ./...` also carries the **tree-drift gate**
(`internal/cli/root_test.go`): the cobra command tree must match the usage
array in `.lok8s/lo` — names, aliases, hidden flags, `@destructive` /
`@readonly` / `@idempotent` markers and the short text — while both
implementations exist. A command that exists in one tree and not the other
fails the test unless it is allow-listed in `goOnlyCommands` with a reason.

None of the harnesses reaches a live cluster, a Tilt session or the docker
daemon. The one real round-trip is `hack/e2e-go-roundtrip.sh` (provision →
status → down → destroy on a synthetic kind cluster), run by hand, not in CI
— see [TESTING.md](https://github.com/kernpilot/lok8s/blob/main/TESTING.md).

## Deviations catalogue

"Bash wins" is the porting rule: every user-visible string, exit path and
ordering quirk of the bash implementation is reproduced, and the source
comments say so at each spot (`// bash wins`, `// quirk preserved`). The
list below is the complete set of places where the binary **deliberately
does not** match, consolidated from those comments and the parity
allow-lists. Everything not listed here is expected to be byte-identical.

### CLI shape

| # | Deviation | Where |
|---|---|---|
| D1 | **Parse errors exit 1, not 2.** argsh exits 2 on its own parse errors (`Error: too many arguments: …`, `Error: unknown flag: …`); the binary prints the identical message (including the `Run "lo -h"` hint) and exits 1 — the cli-wide convention. The parity harnesses tolerate exactly the rc pair (2, 1) and nothing else. | `internal/cli/cmd_secrets.go` (`argshErrorf`), `internal/cli/dispatch.go` (`argshFlagErrors`) |
| D2 | **Group help is cobra's, not argsh's.** A bare `lo gitops`, `lo registry`, `lo tilt`, … or `-h` prints cobra's help layout instead of the argsh usage block. Content is the same set of commands; the formatting differs. Not diffed by the harnesses. | every command group (`cmd_gitops.go` states it) |
| D3 | **Global flags are position-insensitive.** argsh's `main()` stops scanning global flags at the first flag it does not own, so `lo clean -a --domain x` left `--domain` unread in bash. cobra reads persistent flags anywhere on the line. The harnesses always place `--domain` first so both agree. | `hack/parity-orchestrate.sh` |
| D4 | **`--help` reaches nested commands.** argsh intercepted `-h` at the `drivers` level (`lo drivers lo status --help` printed the drivers usage). cobra resolves the nested command first — a documented improvement. | `internal/cli/cmd_drivers.go` |
| D5 | **`lo version` drops the `bash` row.** The binary has no interpreter to report. `lo doctor` still reports the bash the prepared `PATH` resolves, because the provider plugins run under it. | `internal/cli/cmd_version.go`, `cmd_doctor.go` |
| D6 | **`lo kubehz node … --cluster X` is refused.** Through the real `lo`, argsh's inherited global `--cluster` consumed the flag before the node guard saw argv, so the bash guard was dead and the flag silently ignored. The binary enforces the guard as intended. | `hack/parity-kubehz.sh` |
| D7 | **Spec parse errors carry the binary's own message.** An unparsable `cluster.lok8s.yaml` surfaces yq's `Error: bad file …` line in bash and `[error] cannot parse cluster spec: …` in Go; same rc. | `internal/kubehz`, `hack/parity-kubehz.sh` |
| D8 | **`lo status` on a domain without a spec.** The bash `dispatch_status` ignores `resolve_spec`'s return and dies on an unbound variable (`set -u`); the binary prints the invalid-domain error and continues with the cluster-free sections. A bash defect, not a parity target. | `hack/parity-orchestrate.sh` |
| D9 | **`lo kubehz register` / `join` do not print `LOK8S_SPEC_FILE: unbound variable`.** See defect B3 below; the binary passes the spec path. | `hack/parity-kubehz.sh` |
| D23 | **A `-s` placed before the subcommand binds to the leaf's own `-s` on `secrets set`, `secrets env` and `kubehz handover receive`.** Those leaves give `-s` to `--namespace` / `--snapshot` (the bash spec); cobra hands the leaf every flag on the line regardless of position, so `lo -s myns secrets set …` reads `myns` as the namespace, where argsh's main consumed it as `--cluster` first. After the subcommand both agree. The harnesses place `-s` after the verb. | `internal/cli/cmd_secrets.go`, `cmd_kubehz.go` |

### Fail-loud instead of fail-silent

| # | Deviation | Where |
|---|---|---|
| D10 | **KubeOne provider detection errors out.** The bash call inside `extract_vars` ran under a disabled errexit, so a spec with no detectable provider silently rendered `cloudProvider: "": {}`. The binary prints `No provider found in cluster spec` and stops. | `internal/driver/kubeone/vars.go` |
| D11 | **KKP unsupported provider / non-numeric replicas abort before the wire.** The bash printed the error and then POSTed a mangled payload the server rejected. Same message, no request. | `internal/driver/kkp/kkp.go` |
| D12 | **Tool-not-found checks in `lo secrets`.** `sops` and `ssh-to-age` are libraries in the binary, so their "not installed" branches do not exist. | `internal/secrets/ops.go` |

### Rendering and display

| # | Deviation | Where |
|---|---|---|
| D13 | **Progress UI on a terminal is the final state, not a live spinner.** The bash tty path streams a spinner and a 3-line scrolling window to `/dev/tty`; the binary renders the identical *final* summary / surfaced errors after the phase. Off a terminal (CI, Tilt logs, `LOK8S_NONINTERACTIVE`, `DEBUG`) the output is byte-identical. | `internal/kapply` |
| D14 | **YAML edits re-serialize with yaml.v3 formatting.** The CAPI placement-group and KubeOne value edits keep the merged *content* as the contract; edited documents come out with 2-space indentation (comments preserved), untouched documents pass through byte-identical. yq's 4-space sequence indent is not reproduced. | `internal/driver/capi/pg.go`, kubeone `yamledit` |
| D15 | **`lo image cache` / `list` do not rewrite `.registries.json`.** The bash sourced the whole Lo driver, which regenerated the file as a side effect; the binary computes the same values from the spec without the rewrite. | `internal/image/image.go` |
| D16 | **SOPS output is not byte-compared.** Encryption is nondeterministic; cross-tool decrypt is the contract. | `internal/secrets/sops.go`, `hack/parity-build.sh` |
| D17 | **The kind config is a temp file, not a process substitution.** One `kind create cluster … --config <path>` argv line differs. | `hack/parity-orchestrate.sh` |
| D18 | **Operator hooks: `set -u` abort text.** An unset `BINDING_CONTEXT_PATH` exits 1 in both; the bash message names the script line, the binary prints `error: BINDING_CONTEXT_PATH: unbound variable`. | `internal/operator/operator.go`, `hack/parity-operator.sh` |
| D19 | **The render needs no `kustomize`/`khelm`/`.kustomize/` and no `KUSTOMIZE_PLUGIN_HOME`.** `lo build`, the addon render and the registry TLS mint run in-process ([In-process rendering](#in-process-rendering)); the bash tree execs the pinned binary and the built plugins. Output bytes are identical; the *failure* modes differ where a plugin binary is missing: bash's `lo up` stops on `the Secret plugin is not built at …`, the binary mints in-process and proceeds. `LO_RENDER=exec` reproduces the bash behaviour (the orchestrate harness pins it). | `internal/render`, `internal/driver/lo/registries.go`, `hack/parity-orchestrate.sh` |

### Credentials on disk and on the terminal

Go-only hardening. The bash tree keeps its behaviour; the harnesses never
reach these paths (they stop at the local refusals, tokens unset).

| # | Deviation | Where |
|---|---|---|
| D20 | **CAPI and KKP kubeconfigs are written 0600.** The bash drivers' `> "${kc}"` redirects left cluster-admin kubeconfigs at the umask default (0644), readable by every local user. The binary writes them owner-only and tightens a file that already exists (`os.Chmod`), as the kind and hosted paths already did. | `internal/driver/capi/capi.go` (`writeKubeconfigFile`), `internal/driver/kkp/api.go` (`getKubeconfig`) |
| D21 | **`lo kubehz join` (hosting: shared): the join script is private and the ticket is not echoed.** The api-shipped script lands in a fresh `os.MkdirTemp` directory — `<TMPDIR>/kubehz-join-<random>/kubehz-join-<node>.sh`, 0700 over 0600 — so a shared `/tmp` offers no name to pre-plant. The terminal repeats the plaintext ticket only on `--print-token`, or when no script came (the terminal is the only channel then). A write failure after the mint prints the live-ticket note. Server strings go through `scrub` and the ticket must match the bootstrap-token shape (`[a-z0-9]{6}.[a-z0-9]{16}`) or the mint is refused. The bash lib prints the ticket and writes no script. | `internal/kubehz/shared.go` (`spaceMintJoin`, `writeJoinScript`), `internal/cli/cmd_kubehz.go` |
| D22 | **`lo kubehz claim` reads the nonce from stdin (`--nonce -`) or `KUBEHZ_CLAIM_NONCE`.** The flag form stays for parity and the refusal text is unchanged; the two additions keep the claim ticket out of shell history and `/proc/*/cmdline`. | `internal/kubehz/cluster.go` (`ClaimNonce`), `internal/cli/cmd_kubehz.go` |

### Reproduced on purpose (so nobody "fixes" them in one implementation only)

These are behaviours the binary mirrors exactly because the bash has them.
They read like bugs; they are kept so the two implementations stay
diffable. Change them in **both** trees, in one change, with the parity
harness updated.

| # | Behaviour | Where |
|---|---|---|
| Q1 | `yq -r '.path // "alt"'` semantics: the default fires on a missing key, `null` **and a boolean `false`** (jq's `//`). So `.spec.registries.tls // true` reads `true` for `tls: false`; the run header, `lo env`, the kubehz and driver spec readers all inherit it. A bare `yq -r '.path'` prints the literal word `null` for a missing key. | `internal/cli/specyq.go`, `internal/env`, `internal/kubehz/{jsonx,yamlspec}.go`, `internal/driver/*/yamlspec.go`, `internal/tilt`, `internal/addons` |
| Q2 | A mirror with no `name:` reads as `"null"`, which passes the name regex; the failure comes later on the URL check. | `internal/driver/lo/configregistry.go` |
| Q3 | `lo::coredns` and `lo::expose` ran under a caller's `|| return 1`, which disables errexit for the whole body: intermediate `kubectl`/`docker` failures do not abort, the status is the last command's. | `internal/driver/lo/services.go`, `expose.go` |
| Q4 | `lo secrets` single-match lookups: zero matches also takes the "more than one" branch and prints an empty `Multiple matches found:` list. | `internal/secrets/readops.go` |
| Q5 | `lo secrets init` against an existing `.sops.yaml` with an empty derivation: `grep -qF ""` matches any non-empty file. | `internal/secrets/ops.go` (`grepQF`) |
| Q6 | `lo lint` label check: yq emits one count per document and aborts at the first non-map `.metadata.labels`; a multi-document manifest whose first document carries the label never warns, an unparsable file always does. `labels` and `secrets` findings are advisory and never change the verdict. | `internal/lint/lint.go` |
| Q7 | `lo audit`: a malformed cilium bootstrap entry is skipped silently, so the cilium check reports "not in spec.bootstrap" (pass). SARIF and JSON reproduce jq's formatting (2-space indent, insertion-order keys, inline empty containers, no HTML escaping, `group_by(.id)` sorting the rules). | `internal/audit/bootstrap.go`, `sarif.go` |
| Q8 | `lo deploy -l k=v`: the subset apply ran as `deploy::_apply … || rc=$?`, suspending errexit — a failing apply is logged and the phase continues; the status is the scoped wait's (always 0). | `internal/deploy/deploy.go` |
| Q9 | Operator `removeFinalizer`: `kubectl … | jq … || echo '[]'` under pipefail yields `""` when the object has no finalizers field, so the merge patch is malformed and kubectl's rejection becomes the warn line. The CAPI hook substitutes templates with an unrestricted `envsubst`. | `internal/operator/kube.go`, `capi.go` |
| Q10 | `lo kubehz register`: `.data.validation.writable // "unknown"` — only the *string* `"false"` reaches the read-only branch (a JSON `false` is swallowed by `//`). | `internal/kubehz/register.go` |
| Q11 | `lo down` / `lo clean` ignore positionals and unknown flags (`main::down` has no `:args`). `lo init service` and `lo audit` collect extra positionals and ignore all but the first (argsh array parameter). | `internal/cli/cmd_down.go`, `cmd_init.go`, `cmd_audit.go` |
| Q12 | `lo image list` against a dead registry endpoint prints the header only and exits 0 (the `curl | jq` pipeline). | `hack/parity-loop.sh` |
| Q13 | `lo bootstrap` with nothing to apply prints its debug line twice. `LOK8S_BOOTSTRAP_ONLY` defaults to `0` when unset (defer to the driver), never `1`. | `internal/bootstrap/entries.go`, `engine.go` |
| Q14 | `--no-secrets` has no `=false` off-form: the flag ON wins over `LOK8S_BUILD_NO_SECRETS`, otherwise the env is honoured. | `internal/build/spec.go` |
| Q15 | The registry ConfigMap block is emitted with jq's blank-line separators, byte-for-byte. | `internal/driver/lo/registries.go` |

## Flagged bash defects

Defects found while porting, **not fixed** in either implementation because
fixing them is a user-visible behaviour change that deserves its own change
(and, for B1/B2, coordination with in-cluster consumers). The binary
reproduces each one and the source carries a `KNOWN DEFECT, PRESERVED ON
PURPOSE` comment. Fix them in both trees together and update the parity
harness in the same change.

| # | Defect | Where |
|---|---|---|
| B1 | **Remote-expose nginx TLS path mismatch.** The shipped `nginx.conf` references `/tls.cert` (with an E) while the copy lands the file at `/tls.crt`, so the proxy's HTTPS server block cannot find its certificate and the reload fails on TLS. Shipped since the template landed. | `internal/driver/lo/expose.go`, `.lok8s/drivers/lo/utils/expose.sh` |
| B2 | **Registry ConfigMap port is hardcoded `5000`.** The registries listen on 80/443 (TLS-mode dependent). In-cluster consumers may compensate; correcting it is a coordinated change. | `internal/driver/lo/registries.go` |
| B3 | **`lo kubehz register` / `join` read `LOK8S_SPEC_FILE` before it is set.** `validate_config` reads a variable only `provision::resolve_spec` sets, and `register` calls it after `validate`, so bash prints a spurious `unbound variable` line and the per-kind validation rules are dead on those two verbs (kind reads as `""`). The binary passes the spec path (D9); the kind rules are exercised through `deploy`, where the bash sets the variable itself. | `hack/parity-kubehz.sh`, `.lok8s/libs/kubehz/` |
| B4 | **`lo kubehz node … --cluster` guard is dead on dispatch** in bash (D6). | `hack/parity-kubehz.sh` |
| B5 | **`lo status` on a domain without a spec crashes on an unbound variable** in bash (D8): `dispatch_status` ignores `resolve_spec`'s return and dies on `LOK8S_SPEC_KIND` under `set -u`. A traversal-shaped domain (`../evil`) takes the same path. | `hack/parity-orchestrate.sh` |

## Release artifacts

Every tag publishes, via goreleaser (`.goreleaser.yaml`,
`.github/workflows/release.yml`):

| Asset | What |
|---|---|
| `lo-<os>-<arch>.tar.gz` | the `lo` binary (+ LICENSE) |
| `kustomize-secret-<os>-<arch>` | the `secrets.lok8s.dev/v1/Secret` kustomize exec plugin |
| `lochat-<os>-<arch>` | the `lo chat` engine |
| `lok8s-<tag>.tar.gz` | the framework tree (`.lok8s/`, operator CRDs + deploy, `.kustomize/`) |
| `lo-install.sh` | the installer, so it can be verified like everything else |
| `checksums.txt` | SHA-256 of all of the above |

The `kustomize-secret-*` names are a contract other projects' `b.yaml` files
address; they do not change. The same generator source now also ships
*inside* `lo` (the root module imports `./kustomize`), so the standalone
asset serves the frozen bash tree and the render CI of projects that still
run the exec pipeline. `make release-check` validates the goreleaser
config; `make snapshot` runs the whole pipeline locally without publishing.

## Legacy: what moved under `.lok8s/legacy/`

Retired code is moved, never deleted (`.lok8s/` is the frozen reference; a
file retired from *outside* it moves *under* it):

| Path | What |
|---|---|
| `.lok8s/legacy/install/` | the argsh `lo-up` bootstrap installer (source, build script, `argsh.pin`); the published bundle stays at `docs/public/lo-up` and the `loup-bundle` CI job still rebuilds and diffs it |
| `.lok8s/legacy/operator/hooks/` | the original bash shell-operator hook bodies; `operator/hooks/*.sh` are now two-line shims that `exec lo operator <hook>` |

The frozen tree is bugfix-only. Anything that changes behaviour lands in
Go first, and the bash side is changed in the same commit only if a parity
harness would otherwise go red.
