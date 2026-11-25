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
   ├─ lok8s.yaml routes <command> to bash? ──yes──▶ exec bash <project>/<tree>/lo <command> …   (verbatim argv)
   │
   └─ no ─────────────────────────────────────────▶ Go implementation (internal/cli/cmd_<command>.go)
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
| Go-only | `mcp` (native MCP server), `operator` (the shell-operator hook bodies), `assets` (the embedded framework assets: list, eject, diff, update), `toolchain` (the pinned consumer toolchain via b: install, doctor) | `internal/cli/cmd_mcp.go`, `internal/operator`, `internal/assets`, `internal/toolchain` |

`mcp`, `operator`, `assets` and `toolchain` have no twin in the argsh usage
list, so they are allow-listed by name in `internal/cli/root.go`
(`goOnlyCommands`) with a reason each. The same holds for the Go-only
additions to existing commands: `lo init project` (with `--cluster` and
`--driver`), the `lo init` wizard on a terminal and `lo init --plan`,
`lo lint --notes`, `lo addons --origin`, `lo drivers --list --origin`, the
global `--no-eject` flag — see [Embedded assets](#embedded-assets-the-eject-model). `lo mcp` replaces the argsh `mcp` builtin.
The shipped `.mcp.json` launches `bin/lo mcp start`. The builtin stays
reachable as the bash variant (`.lok8s/lo mcp` from a checkout), and
`lo chat` still drives it. See [`lo mcp`](/reference/cli#lo-mcp).

### What still calls bash

The binary is the entrypoint for everything, but three seams still run bash
code from the framework tree. They are deliberate, hermetic under test
(every child goes through the `execx.Runner` seam), and documented here so
nobody mistakes them for unported commands.

| Seam | Where | Why |
|---|---|---|
| **Providers** (`.lok8s/providers/<name>/main`, e.g. `hetzner`) | `internal/provider/bridge` (provision dispatch: `up`, `provision`, `destroy`, `recover`), `lo doctor`'s provider section, `lo recover`'s rebuild | Providers are bash plugins that source the argsh runtime. Each contract call runs as a `bash -c` child over the ORIGINAL libs; state lives at the cloud or on disk, never in shell variables, so per-call loading is correct. |
| **`lo drivers <name> …`** for a driver directory with no Go twin | `internal/cli/cmd_drivers.go` → `Shim` | A name that exists only as `.lok8s/drivers/<name>/main` is handed to the argsh implementation with argv verbatim; `--list` prints the union of both worlds. |
| **A routed command** (`lok8s.yaml`, `spec.implementation`) | `internal/cli/routing.go` → `internal/cli/shim.go` | The project file's choice: [Choosing the implementation](#choosing-the-implementation). |

::: warning Custom bash drivers and the provision dispatch
The Go provision dispatch (`lo up` / `provision` / `destroy` / `status` /
`bootstrap`) resolves the spec's `kind:` against the **Go driver registry**
(`lo`, `capi`, `kubeone`, `kkp`, `kubehz`). A custom driver that exists only
as `.lok8s/drivers/<kind>/main` is reported as `Unknown cluster kind` by the
binary. It still works through `lo drivers <kind> …` and through a project
whose `lok8s.yaml` routes `provision` to bash. The [Driver Contract](/reference/kind-contract)
describes the Go interface of the built-in drivers and the bash contract of
the frozen tree. A Go contract for drivers outside this repo is not
published yet.
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
| Needs in the project | `.bin/kustomize` + `.kustomize/{khelm…/ChartRenderer, secrets.lok8s.dev/…/Secret}` — what [`lo toolchain install`](cli.md#lo-toolchain) installs, pinned; `lo doctor` fails when they are missing | nothing for the render (the toolchain is still needed for kubectl/kind/tilt); `lo doctor` only warns about absent render tools |
| `lo --version` | `lo version 0.4.1 (core)` | `lo version 0.4.1 (full)` |

Everything else (every command, the parity harnesses, the tests) is the
same code. `go.mod` keeps khelm and the kustomize API for both; only the
tag-gated imports decide what is linked. The pins that keep the two
byte-identical live in **`internal/toolchain/pins.go`**: `KustomizeAPI`
(`v0.21.1`, what lo-full links) ↔ `KustomizeCLI` (`v5.8.1`, what core execs
and the b.yaml template pins; the `kustomizeAPIToCLI` table encodes the
release pairing), `KhelmVersion` (`2.9.0`: the library and the
`ChartRenderer` binary) and `HelmVersion` (`3.22.1`). `go test
./internal/toolchain/` fails when `go.mod`'s kustomize/api, khelm or helm
version moves without the pins, when the API↔CLI table lacks the pinned
API, or when the generated `.bin/b.yaml` template stops carrying the pins.
Bumping any one side alone is a red build. The plugin paths under
`.kustomize/` (`SecretPluginRel`, `ChartRendererPluginRel`) live in the
same file; the render, the doctor and the registry mint read them from there.

CI runs every gate against both builds (`go build`/`vet`/`test`/golangci
with and without `-tags inprocess`; all ten parity harnesses against
`bin/lo`, the exec path, i.e. the same pinned kustomize + plugins the bash
side runs, and against `bin/lo-full`; the goreleaser snapshot asserts both
archives). The toolchain is installed before the Go tests so the
byte-parity tests in `internal/render` run against the pinned binaries;
under `CI=true` a missing binary fails them instead of skipping. Locally:
`make build build-full test test-full vet vet-full lint lint-full`.

### External tools still exec'd

The port swapped shell for Go and, since phase 7, the kustomize renderer
too, on the lo-full build (see [In-process rendering](#in-process-rendering)
below); lo core keeps kustomize as a subprocess by design. The following
remain subprocesses, resolved through the project's `.bin/` first and `PATH`
second (`internal/execx.Look`):

| Tool | Used by | Why not in-process |
|---|---|---|
| `kustomize` (+ the `khelm` and `secrets.lok8s.dev` exec plugins under `.kustomize/`) | **lo core: every render** (`lo build`, the addon render, the KubeOne addon staging): the pinned binary and the two b-installed plugins `lo toolchain install` provisions. lo-full: only `LO_RENDER=exec` (the A/B escape hatch), `lo kustomize` (which builds the standalone plugin for the bash path), and a command routed to bash | Core stays small and exec-only on purpose (the owner decision behind the two builds); lo-full links the same releases. Both are byte-identical: the pins are drift-tested. |
| `yq` | `lo build` split mode (the YAML stream transforms and per-Secret re-renders), `lo env services` (the deep-merge the Tiltfile consumes) | yq's emitter has opinions (`---` on every non-first document, sequence-dash indentation) that `gopkg.in/yaml.v3` does not reproduce byte-for-byte. Field *extraction* from YAML is native everywhere. |
| `sops` | `lo build` split-mode Secret twins | The `.enc` files must stay interoperable with the sops CLI. (`lo secrets` itself uses the sops **library** in binary mode; the files it writes are the same format.) |
| `kubectl`, `kind`, `docker`, `tilt`, `clusterctl`, `kubeone`, `hcloud`, `mkcert`, `envsubst` | drivers, deploy, registries, tilt, trust | These are the tools lok8s orchestrates; calling them as-is keeps "what lok8s runs is what you'd run by hand" true. |

**Renderer-swap roadmap.** Moving yq/sops in-process is deferred by the
*renderer-drift rule*: a swap is allowed only once byte-parity with the
pinned tool's output is proven for the committed domains, not just the parity
fixtures (`internal/build/split.go` carries the TODO). The CRD render
(`lo crds`) was the first precedent: a native `yaml.Node` transform whose
output is byte-identical to the former `yq eval` render, with the committed
CRDs as the parity fixture. The kustomize render is the second (below).

### In-process rendering

Phase 7 (WP3 + WP4) moved `kustomize build`, and both exec generators every
lok8s render depends on, into the binary; since the core/full split this
is the **lo-full** build (`-tags inprocess`). There, `lo build`, the
bootstrap engine's addon render (`internal/addons`), the KubeOne driver's
addon staging, the legacy `lo k8s` paths and the Lo driver's registry TLS
mint no longer exec `kustomize`, `khelm` or the `Secret` plugin, and need
neither a `.kustomize/` directory nor `KUSTOMIZE_PLUGIN_HOME`. (On lo core
the registry TLS mint is the one piece that stays in-process, the imported
generator, while every kustomize render execs the pinned binary.) The
output is byte-identical to the exec pipeline's; that was the gate, not a
goal.

**What runs where** (`internal/render`):

| Piece | In the binary | Pinned to |
|---|---|---|
| `kustomize build --enable-alpha-plugins [--enable-exec] <dir>` | `sigs.k8s.io/kustomize/api/krusty`, driven option-for-option like the kustomize CLI's build command (`Reorder` unspecified, `EnabledPluginConfig(BploUseStaticallyLinked)`, the builtin helm inflator enabled with the default `helm` command, `KUSTOMIZE_ENABLE_MANAGEDBY_LABEL` honoured, `ResMap.AsYaml()` as the bytes) | `api v0.21.1` + `kyaml v0.21.1` — the modules behind the **kustomize v5.8.1** binary the repo pins in `.bin/b.yaml` (a downstream project pins `v5.8.1` explicitly) |
| `secrets.lok8s.dev/v1/Secret` | `kustomize/plugins/secret` **imported** (go.mod: `replace github.com/kernpilot/lok8s/kustomize => ./kustomize`) — the same `secret.Run` the standalone `kustomize-secret` binary's `main` calls; the nested module keeps building on its own for the release assets and the bash path | the repo's own module, one source tree |
| `khelm.mgoltzsche.github.com/v2/ChartRenderer` | `github.com/mgoltzsche/khelm/v2/pkg/{config,helm}` as a **library**, replicating khelm's kustomize-plugin `main` (config from `KUSTOMIZE_PLUGIN_CONFIG_STRING`, `helm.NewHelm()` from the helm `cli.New()` settings, `KHELM_TRUST_ANY_REPO`/`KHELM_DEBUG`/`HELM_DEBUG`, `ReadGeneratorConfig`, `Render`, then khelm's `output.Marshal` — the kyaml encoder, one `Encode` per RNode document — repeated verbatim because that package is internal to khelm) | **khelm v2.9.0** — the exact release the repo pins as the `ChartRenderer` binary (a downstream project pins khelm with kustomize v5.8.1 as the pair proven to reproduce its committed artifacts byte for byte); its go.mod requires **helm.sh/helm/v3 v3.22.1**, pinned in the root go.mod as well, so the chart inflation is the same helm code the binary was built from |

**The self-exec plugin home.** kustomize's exec-plugin protocol is a
subprocess: `<pluginhome>/<group>/<version>/<kind>/<Kind> <cfgfile>` run in
the kustomization directory with `KUSTOMIZE_PLUGIN_CONFIG_STRING` in the
environment. The binary keeps that protocol (nothing in the kustomize API
is patched) and points it at itself: on the first render of a process
`internal/render` creates a temp plugin home holding the two plugin paths
as symlinks to `os.Executable()` (a copy where symlinks are unavailable),
sets `KUSTOMIZE_PLUGIN_HOME` to it once for the rest of the process (the
kustomize API reads the plugin root from that variable only; a per-render
set/restore would race other goroutines), and
`main` dispatches on `argv[0]` **before anything else** (`render.DispatchPlugin`:
`…/secret/Secret` → the imported generator, `…/chartrenderer/ChartRenderer`
→ the khelm library). The child is therefore `lo` again, started under the
plugin's name; a non-zero exit fails the build with the child's stderr in
the message exactly as before (`secret plugin: …`, `khelm: …`). The home is
removed on exit (`render.Cleanup`, which also restores the caller's
`KUSTOMIZE_PLUGIN_HOME`); the rc passthroughs (`tilt ci`, `tilt status`,
`image list`, `deploy`, a bash driver) exit through the same cleanup.

The per-render environment the exec pipeline handed to the kustomize child
(`KUBECONFIG`, `KHELM_TRUST_ANY_REPO=true`, `LOK8S_SECRETS_DISABLE`, the
toolchain on `PATH`, an addon entry's `env:` overrides) is `render.Options.Env`.
The plugin children are children of the lo process itself, so the overlay
does not go through the process environment: the render writes it to a
per-render file under the self-exec plugin home (`env/<hash of the render
dir>.env`) for the duration of the run, and the plugin child installs it in
its own environment before it serves the generator, matched by the
kustomization root kustomize runs it in (`KUSTOMIZE_PLUGIN_CONFIG_ROOT`).
Renders of different directories run in parallel; two renders of the same
directory are serialized. A goroutine that reads the environment or starts
a child (kubectl via `execx`) while a render is in flight sees nothing of
the overlay, so the bootstrap DAG keeps `LOK8S_BOOTSTRAP_PARALLEL` on
lo-full as well. The two values the kustomize API reads from the
environment itself (`KUSTOMIZE_ENABLE_MANAGEDBY_LABEL`, the `helm` command
for the builtin inflator, resolved through the overlay's `PATH`) are taken
from the overlay first.

**Chart cache.** `helm.NewHelm()` reads the same `HELM_*` environment the
khelm binary read, so chart downloads and repository indexes land in the
same helm cache (`$HELM_REPOSITORY_CACHE`, else `$XDG_CACHE_HOME/helm/
repository`, else `~/.cache/helm/repository`) and a warm cache stays warm
across the switch. The `KHELM_TRUST_ANY_REPO=true` the pipeline always set
still decides whether an undeclared repository is trusted.

**`LO_RENDER=exec`** restores the subprocess pipeline everywhere
(`internal/render` execs the pinned `kustomize` from `.bin` with
`KUSTOMIZE_PLUGIN_HOME` set to `<project>/.kustomize` when the shell does
not export it — for every render: `lo build`, the addon render, `lo k8s` —
and the registry TLS mint execs the built `Secret` plugin as before). Use
it to A/B a render: `lo build` promotes `artifacts.yaml` only when the bytes
change, so a `DEBUG=1 lo build` that reports `render unchanged` under both
settings is the proof. An unknown value is rejected (`LO_RENDER: unknown
value`).

**The gate** (all held at the switch, 2026-09-03): `hack/parity-build.sh`
green (bash exec vs Go in-process, byte-diffed); a downstream project's
committed dev domain (244 documents, the Secret generator cache-first, 20+ khelm
charts, envsubst) `render unchanged` with identical SHA-256 under the
default and under `LO_RENDER=exec`, and still `render unchanged` with
`KUSTOMIZE_PLUGIN_HOME=/nonexistent` (where the exec pipeline fails to find
its plugins: the proof the in-process dispatch was taken);
`internal/render`'s tests byte-compare the in-process render with the
pinned binaries in the repo's `.bin`/`.kustomize` for a plain
kustomization, the Secret generator and a local-chart `ChartRenderer`;
`internal/driver/lo`'s in-process mint test verifies the leaf (SANs, CA
signature, key match) against a throwaway CAROOT.

What did **not** move: `yq` (split-mode transforms, `lo env services`) and
`sops` (WP5). `lo kustomize {build,test,clean,list}` still manages the
standalone plugin under `.kustomize/` for the frozen tree (and for a core
build without `b`), and `lo doctor` still reports `KUSTOMIZE_PLUGIN_HOME` /
the built plugin because a command routed to bash, the provider plugins, lo core and
`LO_RENDER=exec` need them (that text is unchanged; `hack/parity-configure.sh`
diffs it; the pinned-toolchain section doctor adds is gated on the
`lo toolchain install` marker or `--toolchain`, so the diff stays strict).
`kubectl kustomize` in `lo kubehz deploy` is kubectl's embedded kustomize,
not the pinned one, in both implementations. The in-process render grew the
binary from 49 MB to 123 MB (helm + client-go + the kustomize API), which
is why it is the `lo-full` build and `lo` core stays at ~50 MB with the
exec pipeline. khelm v2.9.0 requires `go 1.26.0` or later in the root
`go.mod`.

## Embedded assets: the eject model

Phase 7 / WP1 (done). The framework's first-party **data** ships inside
the binary; a project no longer needs a synced `.lok8s/` tree for it.

**What is embedded.** `internal/assets` embeds a committed mirror,
`internal/assets/lok8s/**` (113 files, ~168 KB):

| Mirror path | Content | Materialization unit |
|---|---|---|
| `addons/**` | all 24 bootstrap addons, byte-identical, vendored `chart/` dirs included | one unit per addon (`addons/<name>`) |
| `drivers/lo/cluster/**` | kind config, CoreDNS, registry and expose templates | `drivers/lo/cluster` |
| `drivers/kubeone/cluster/**` | the KubeOne core template | `drivers/kubeone/cluster` |
| `drivers/capi/cluster/**` | the CAPI core + provider templates | `drivers/capi/cluster` |
| `libs/inventory/manifests/` | the ClusterInventory CRD mirror | `libs/inventory/manifests` |
| `chat/` | `lo chat` defaults | `chat` |
| `tilt/` | the Tilt extension (`Tiltfile`, its README, the registry TLS note) that the project-root two-line `Tiltfile` loads | `tilt` |
| `VERSION` | the fallback for an unstamped build | — (never ejected) |

The embedded copy is canonical. The repo's `.lok8s/**` twin stays (the
frozen bash implementation and the parity harnesses read it) and is held
byte-identical by `hack/sync-legacy-assets.sh` (mirror → `.lok8s`;
`--from-legacy` the other way; `--check` diffs; it refuses to replace a
subtree that holds untracked files) and the Go test
`TestEmbeddedMirrorMatchesLegacyTree`, which fails on any divergence in
either direction. The subtree list is kept in the script and in
`drift_test.go`; `TestMirroredListMatchesSyncScript` pins the two to each
other.

**Resolver and precedence.** `assets.Resolve(paths, rel)` returns the
on-disk path for a rel like `addons/cilium` or
`drivers/lo/cluster/registry`: the project's `.lok8s/<rel>` when it
exists (whatever its content), else the embedded copy. `assets.Peek` is the
same lookup without side effects. `lo` never overwrites an existing local
file; the only writer of existing files is `lo assets update`. Every
runtime read of the framework tree in the binary goes through the
resolver: the bootstrap entry parser (`internal/bootstrap`, and the
twin parsers in `internal/lint` and `internal/audit`, which peek), the
`lo` driver's CoreDNS/registry/expose templates, the KubeOne core
template, the CAPI templates, the inventory CRD, the chat defaults, the
Tilt extension (`lo tilt up` and `lo tilt ci` eject `tilt/` before Tilt
starts, because Tilt reads `.lok8s/tilt/Tiltfile` from disk; under
`--no-eject` / `LO_ASSETS_EJECT=never` they stop with an error that names
`lo assets eject tilt`), and the version (`assets.Version()`: ldflags,
else the embedded `VERSION`).
What does NOT go through it, on purpose: the bash seams (`.lok8s/lo`,
`.lok8s/drivers/<name>/main`, `.lok8s/providers/*`), which are the frozen
implementation, not assets, and `lo crds generate`'s write of the
`.lok8s` CRD mirror, which is a generator output in this repo.

**Eject on first use** (the default). When a consumer needs an asset and
the project holds no copy, the whole unit is written into `.lok8s/<unit>/`
(atomically: a sibling temp dir renamed into place; when two processes
race, the loser finds the unit in place and drops its stage) with a
`.lo-origin` marker and one `[assets] ejected <rel> -> .lok8s/<rel>` line
on stderr.
Read-only commands (`lint`, `audit`, the `addons` listing, `assets`
itself) never eject. Opt-outs: `--no-eject` / `LO_ASSETS_EJECT=never`
(the embedded copy is served from a per-run temp dir, nothing is written);
`lo assets eject --check` and `lo assets diff --check` for CI. The
`.lo-origin` marker:

```yaml
# .lo-origin — written by lo when it ejected this asset. Do not edit.
lo: 0.4.1
ejectedAt: 2026-09-03T10:00:00Z
files:
  chart.yaml: sha256:…
  values.yaml: sha256:…
```

**Three-way diff.** `lo assets diff` classifies every file of a unit
from ORIGIN (marker) vs LOCAL vs EMBEDDED: `unchanged`, `local modified`,
`lo updated`, `both` (conflict), `local-only`, `builtin-only`; a copy with
no marker (a tree vendored by `b env sync`) classifies its differences as
`local modified` because nothing proves otherwise. `lo assets update`
applies the embedded copy only when local == origin for every file (else
`--force`); a copy already identical to the embedded unit is in sync,
marker or not. The full surface, the state table and the JSON shape are in
the [CLI reference](/reference/cli#lo-assets).

**Project marker.** `config.ResolvePaths` recognizes a project by
`clusters/` or a `kind: Project` `lok8s.yaml` (what `lo init project`
writes). A service's `lok8s.yaml` (`kind: Service`, one per service
submodule of an umbrella project) is not a marker: from inside a
service directory the walk-up continues to the umbrella project. `.lok8s/lo` is not a marker
either (D30): `.lok8s/` holds the ejected assets and, when a project wants
one, the bash tree. `config.FindProjectRoot` applies the same rules from an
explicit directory without consulting `PATH_BASE`; `lo init project` and
`lo toolchain install` resolve their default through it, so an ambient
project from a direnv/mise shell is never the target.

**Parity.** The bash implementation reads `.lok8s/**` from disk and has
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
entrypoint + libraries the parity gates and a routed command run. The data
files it used to need (addons, driver templates, the CRD mirror, the chat
defaults, `VERSION`) are embedded now. What changed for the user is only
the entrypoint: `lo` on your `PATH` is the binary instead of `.lok8s/lo`.

The `core` profile's `.bin/b.yaml` declares the binary
(`github.com/kernpilot/lok8s`, asset `lo-[^f]*.tar.gz`, alias `lo`), so `b
install` fetches it into `.bin/` alongside the rest of the toolchain. The
glob leaves out the `lo-full-*` archives: b scores both the same, so a
plain `lo-*.tar.gz` can install `lo-full` under the `lo` alias. The
[Toolchain](/guide/toolchain#what-b-manages-consumers-vs-contributors) page
states what `b` still manages today versus the intended end state.

## What still runs as bash

Three seams run bash from the frozen tree. Each one is a choice, not a
gap in the port. The provider plugins and `lo drivers <name>` need no
checkout: the binary embeds the whole tree and serves it from one of three
places, in this order. A routed command runs the project's own tree only.

1. `PATH_LOK8S`, when it is set and holds `lo` (a checkout, or a project
   that ran `lo assets eject bash`).
2. The project's `.lok8s/`, when it holds `lo`.
3. The versioned cache `${XDG_CACHE_HOME:-$HOME/.cache}/lok8s/<version>/lok8s/`,
   extracted once from the binary. A manifest with one sha256 per file
   is verified on every use, so a partial or stale extract is redone.
   With neither `XDG_CACHE_HOME` nor `HOME` set, a per-run temp dir
   serves the tree.

A local tree always wins over the cache and is never written to. The
cache lies outside the project, so `--no-eject` does not switch it off.
`lo doctor` prints a `bash mode` line that names the tree in use.

**The Hetzner provider.** `internal/provider/bridge` runs
`providers/hetzner/main` from that tree as `bash -c` children over the
frozen libs. Each contract call (`provider::provision`, `provider::destroy`,
`provider::output`, the KubeOne inventory hooks) starts one fresh process:
it sources argsh, loads the provider, calls one function, and exits.

A fresh process has no memory, and that is fine. The provider keeps its
state at the cloud (hcloud labels) and on disk
(`<work_dir>/hetzner.dump.json`), never in shell variables. Every child
goes through `execx.Runner`, so the dispatch stays hermetic under a fake.

**`lo drivers <name>`.** The binary hands a driver directory with no Go
twin (`drivers/<name>/main` in the bash tree only) to the argsh
implementation with argv untouched. A driver of the project's own needs
the tree beside it (`lo assets eject bash`). `--list` prints the union of
the Go registry, the project's `.lok8s/drivers/` and the embedded tree.

**A routed command.** The project file names it (`spec.implementation`,
[Choosing the implementation](#choosing-the-implementation)). The process
is replaced by `bash <project>/<tree>/lo <argv>`. The tree must be in the
project: never the cache, never a checkout elsewhere.

What each seam needs on disk:

| Seam | Needs |
|---|---|
| Hetzner provider | `argsh` in `.bin/` (the `bash` group of `.bin/b.yaml`, then `b install`), the `hcloud` CLI, `curl` for the Robot REST API, `jq` |
| `lo drivers <name>` | `argsh`, plus whatever the driver calls |
| A routed command | `argsh`, plus the full toolchain the bash tree execs (`kustomize`, the `.kustomize/` plugins, `yq`, `jq`, `envsubst`, `sops`) |

The binary prepares `PATH` and every `PATH_*` variable for these children
the way the project's `.envrc` would (`bridge.Env`), so a consumer does not
export them by hand. With the cache in use, `PATH_LOK8S` points at the
cache and `PATH_BASE` at the project; the bash tree then reads the data
files the binary ships, not the ones ejected into the project.

Why the provider stays bash: it is about 1650 lines of argsh that drive
real infrastructure (the hcloud CLI, the Robot REST API, a cloud-init
generator with its own template tree). A port of that size needs its own
change, with a real Hetzner account to prove it against. The bridge keeps
the provider correct today at no risk to live clusters. Port it when
provider behaviour needs to change, not before.

## Choosing the implementation

The project file names the implementation; nothing in the environment
does. `LO_IMPL` and `LO_GO_BASH` are removed, not deprecated: an
environment variable is inherited by every child, survives across
directories and can be set by anything that ran before `lo`. The file is
committed, reviewed with the project and scoped to it.

```yaml
kind: Project
spec:
  implementation:
    default: go              # go | bash; go when absent
    bash:
      commands: [registry]   # routed to the bash tree; top-level names only
      tree: .lok8s           # project-relative, inside the project
```

`default: bash` routes every command: the binary replaces itself with
`bash <project>/<tree>/lo <args>` after preparing the environment the way
the project's `.envrc` would (toolchain and framework directories on
`PATH`, `KUSTOMIZE_PLUGIN_HOME` defaulted). Nothing is parsed on the way;
argv reaches the argsh implementation untouched. `commands` routes the
listed top-level commands under a `go` default. The tree is always
`<project>/<tree>/lo`: the cache extract and a `PATH_LOK8S` checkout are
never routed to. The full rules, the error strings and what `lo lint` and
`lo doctor` report: [Choosing the implementation](cli.md#choosing-the-implementation)
in the CLI reference.

Use a routing when a command misbehaves: if the bash side is right and the
Go side is wrong, that is a parity bug. Please report it with the command
line and both outputs.

Routing is per command, not per lib: a routed `lo registry …` changes
nothing that Go calls internally (`lo up` still manages the registries with
Go code). A customised lib behind a routed command is the project's own
fork from that point: the parity harnesses prove the stock tree only, and
`lo doctor` warns for the commands whose state both sides write. The binary
never switches on the presence of a `.lok8s/libs/<x>` file: a stale tree
must not change the implementation without notice.

## Parity gates

A port is not "done" when it compiles. Every command is held to the bash
implementation by differential tests under `hack/`: each covered invocation
runs **both** implementations (the binary, and the same binary in a
project whose `lok8s.yaml` says `default: bash`) against a synthetic project and diffs stdout, stderr, and
exit codes byte-for-byte — and, where a command writes files, the resulting
trees.

| Gate | Surface | Notes |
|---|---|---|
| `hack/parity-test.sh` | `use`, `version`, `secrets`, and the general ported set | the original harness; `lo version` intentionally drops the `bash` row (allow-listed) |
| `hack/parity-build.sh` | `lo build` | artifacts bytes, split-dir file list, non-Secret split bytes; Secret twins compared by presence + `sops:` marker (sops mints a fresh data key per encrypt); one domain runs the Secret plugin built from the tree with the internal keys-only setting exported as 1, and both sides render it as 0 (`build` writes the real value, `build --no-secrets` writes no Secret) |
| `hack/parity-configure.sh` | `lint`, `kubeconfig`, `doctor` | isolated `HOME`; doctor's environment-driven lines must match since both run in the same environment |
| `hack/parity-audit.sh` | `lo audit` | human, `--json`, `--sarif` for every check family |
| `hack/parity-loop.sh` | `tilt`, `image`, `env`, `hooks` | read-only / error paths; stub `tilt`/`kubectl`/`docker`/`kind` in the synthetic `.bin` |
| `hack/parity-leaves.sh` | `init`, `crds`, `addons`, `drivers`, `chat`, `ai` | stateful sections get one project clone per implementation and byte-diff the trees |
| `hack/parity-ops.sh` | `deploy`, `recover`, `gitops` | cluster-free paths; stub `kubectl`, a scripted `mock` provider whose rebuild refuses outside `CLOUD_DRY_RUN` |
| `hack/parity-kubehz.sh` | `lo kubehz` | config validation, usage errors, hosting-axis routing, handover bundle checks, the bind-secret stage of `deploy` (stub `kubectl` that names its calls), the `register` request bodies, the node maintenance commands of a Space (`node cordon\|uncordon\|drain`, with the node lines of `status`) and the agent tools (`space`, `cluster`, the kubeconfig file, tool calls through `lo mcp`) against one local HTTPS stub (Linux, python3, openssl); no real api tokens set |
| `hack/parity-operator.sh` | `lo operator <hook>` vs the frozen bash hooks | `--config` bytes and stubbed `kubectl`/`clusterctl` call logs |
| `hack/parity-orchestrate.sh` | `up`, `down`, `clean`, `provision`, `destroy`, `bootstrap`, `status`, `registry` | stub `tilt`/`kind`/`docker`/`kubectl`/Secret plugin; consent gates driven with closed stdin; `LO_RENDER=exec` pinned so both implementations exec the (stub) Secret plugin for the registry TLS mint (D19), whose docker argv, plugin environment and `[warn]` lines are diffed byte for byte; one case makes the registry image absent and the first `docker pull` fail, so the explicit pull, its retry and the `[warn]` with the cause are diffed too; `lo bootstrap --dry-run` / `--render-dir` / `--no-diff` / `--only` and the same on `lo provision --bootstrap`: a stub kustomize renders a manifest, a stub kubectl logs its argv, answers `diff` from a script and fails loudly on `apply`; the report lines, the exit codes 0/1/2, the kubectl argv and the render dir's files are diffed byte for byte (the dry run runs its entries sequentially in a deterministic topological order on both sides, so the piped report is stable); on a project with a dangling symlink, an unreadable directory and an 8 GiB sparse file, the dry run leaves the tree and `.registries.json` alone and its render sees the registry values, the boundary's plugin home and refusing `docker`/`git`/`helm`, the empty `HELM_PLUGINS`, the boundary's `TMPDIR` and run store, and no `--enable-exec`; an entry `env:` that sets one of lo's own variables (dry run rc 2, real run rc 1); the exit contract at both command boundaries, the flag errors included (`--only=`, `--with-restore` without `--only`, `--ignore-nondeterministic` without `--dry-run`); an object whose content changes between the two renders of an entry (listed as non-deterministic, rc 1; rc 0 with `--ignore-nondeterministic`); a SIGTERM in the middle of a render leaves no temp file |

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

The Implementations column says where the behaviour of an entry lives. A
mark covers the headline of the entry (its bold text), and the entry text
names a part that differs. **Both implementations**: the binary and the
bash implementation have the headline behaviour. **Go only**: only the
binary has it, and the bash implementation keeps the behaviour that the
entry gives for bash. An entry without a mark has no decision yet.

The bash implementation under `.lok8s/` is frozen, by the rule in
[CONTRIBUTING.md](https://github.com/kernpilot/lok8s/blob/main/CONTRIBUTING.md#conventions):
new behaviour lands in `internal/`, and a change touches `.lok8s/**` only
when a parity harness would otherwise go red, in the same change. A new CLI
command lands in both implementations (D38, D39, D42), unless its entry
here says that it is Go only (D31, D32).

### CLI shape

| # | Implementations | Deviation | Where |
|---|---|---|---|
| D1 | Go only | **Parse errors exit 1, not 2.** argsh exits 2 on its own parse errors (`Error: too many arguments: …`, `Error: unknown flag: …`); the binary prints the identical message (including the `Run "lo -h"` hint) and exits 1 — the cli-wide convention. The parity harnesses tolerate exactly the rc pair (2, 1) and nothing else. | `internal/cli/cmd_secrets.go` (`argshErrorf`), `internal/cli/dispatch.go` (`argshFlagErrors`) |
| D2 | Go only | **Group help is cobra's, not argsh's.** A bare `lo gitops`, `lo registry`, `lo tilt`, … or `-h` prints cobra's help layout instead of the argsh usage block. Content is the same set of commands; the formatting differs. Not diffed by the harnesses. | every command group (`cmd_gitops.go` states it) |
| D3 | Go only | **Global flags are position-insensitive.** argsh's `main()` stops scanning global flags at the first flag it does not own, so `lo clean -a --domain x` left `--domain` unread in bash. cobra reads persistent flags anywhere on the line. The harnesses always place `--domain` first so both agree. | `hack/parity-orchestrate.sh` |
| D4 | Go only | **`--help` reaches nested commands.** argsh intercepted `-h` at the `drivers` level (`lo drivers lo status --help` printed the drivers usage). cobra resolves the nested command first — a documented improvement. | `internal/cli/cmd_drivers.go` |
| D5 | Go only | **`lo version` drops the `bash` row.** The binary has no interpreter to report. `lo doctor` still reports the bash the prepared `PATH` resolves, because the provider plugins run under it. | `internal/cli/cmd_version.go`, `cmd_doctor.go` |
| D6 | Go only | **`lo kubehz node … --cluster X` is refused.** Through the real `lo`, argsh's inherited global `--cluster` consumed the flag before the node guard saw argv, so the bash guard was dead and the flag silently ignored. The binary enforces the guard as intended. | `hack/parity-kubehz.sh` |
| D7 | Go only | **Spec parse errors carry the binary's own message.** An unparsable `cluster.lok8s.yaml` surfaces yq's `Error: bad file …` line in bash and `[error] cannot parse cluster spec: …` in Go; same rc. | `internal/kubehz`, `hack/parity-kubehz.sh` |
| D8 | Both implementations | **`lo status` on a domain without a spec.** Both implementations print the error of the spec lookup in the Cluster section and continue with the cluster-free sections. A domain without a spec prints `No cluster.lok8s.yaml or deploy.lok8s.yaml found in clusters/<domain>/`, and a traversal-shaped domain (`../evil`) prints the raw `error: invalid domain name: <domain>` line. Before #269, the bash `dispatch_status` ignored `resolve_spec`'s return and died on an unbound variable (`set -u`, defect B5). What still differs: on a terminal, the binary's line for a domain without a spec also names the next step (`ui.ErrorNext`); piped, nothing. No parity case diffs it. | `internal/provision/spec.go` (`ResolveSpec`), `internal/provision/dispatch.go` (`DispatchStatus`), `internal/cli/cmd_status.go` (`runStatus`), `.lok8s/libs/provision` (`provision::dispatch_status`), `.lok8s/libs/status` (`status::cluster`), `hack/parity-orchestrate.sh` |
| D9 | Go only | **`lo kubehz register` / `join` do not print `LOK8S_SPEC_FILE: unbound variable`.** See defect B3 below; the binary passes the spec path. | `hack/parity-kubehz.sh` |
| D23 | Go only | **A `-s` placed before the subcommand binds to the leaf's own `-s` on `secrets set`, `secrets env` and `kubehz handover receive`.** Those leaves give `-s` to `--namespace` / `--snapshot` (the bash spec); cobra hands the leaf every flag on the line regardless of position, so `lo -s myns secrets set …` reads `myns` as the namespace, where argsh's main consumed it as `--cluster` first. After the subcommand both agree. The harnesses place `-s` after the verb. | `internal/cli/cmd_secrets.go`, `cmd_kubehz.go` |
| D31 | Go only | **`lo init` scaffolds files only; the toolchain, the lint notes and the assets view moved.** `lo init project` writes `clusters/`, `lok8s.yaml`, the `.gitignore` entries and one environment file (`--env mise\|direnv\|none`; `both` is refused). The network step is `lo toolchain install`; `lo toolchain doctor` is the pinned-tools section of `lo doctor` on its own; `lo init toolchain` is a hidden alias for one release. `lo lint --notes` prints a `[note]` line per Lo spec key equal to its documented default. `lo assets show` is gone (`lo assets diff <rel>` lists the files). All Go-only: `init service`, `init test` and every `lint` case without `--notes` stay byte-identical to bash; bare `lo init` keeps its rc and shape pinned, and its help text carries three Go-only flags (D32). | `internal/cli/cmd_init.go`, `cmd_toolchain.go`, `cmd_lint.go`, `cmd_assets.go`, `internal/scaffold/project.go`, `internal/lint/defaults.go` |
| D32 | Go only | **Bare `lo init` on a terminal opens screens; `lo init cluster` is a Go-only verb.** With stdin and stdout on a TTY, `CI` unset and no `--yes`, the binary reads the room (`internal/initctx`: the project root by marker, a subdirectory, a service directory, a submodule under the umbrella, git with the branch and the uncommitted count, the pinned tools from `.bin/b.yaml`) and, with charm.land/huh/v2 in one house theme: without a project, or in a project without a git repository, a welcome line and the bootstrap screen with every value prefilled (`Create`, `Change details`, `Cancel`), then project mode; in a project with a repository, project mode — the state card in the `lo up` header's two-column layout, one choice from what the state allows (a cluster, a service, the test suite, the toolchain when a pin is missing, the active domain with several clusters, the bash tree or the implementation switch, `Exit`), the action's screen, the card again with a result line. Nothing is written before a screen's `Create`, and every action runs the verbs' own functions (`lo init project`, `lo init cluster`, `lo init service`, `lo init test`, `lo toolchain install`, `lo use`, `lo assets eject bash`, `lo init project --implementation`), which the screen's dim `equivalent` row prints. `lo init service` and `lo init test` open their screen on a terminal too (a value given on the command line is a fixed row; `--yes` asks nothing); off a terminal they run byte-identical to bash. Off a terminal, under `CI` or with `--yes`, bare `lo init` prints the help and exits 0 as the bash tree does (its help text gains the three flags). `lo init --plan` (and `--dry-run`) prints the mode's screen as text, rc 0, no writes, on and off a terminal. No environment variable selects any of it. Go-only. | `internal/initctx`, `internal/cli/cmd_init_wizard.go`, `internal/cli/cmd_init.go`, `internal/scaffold/cluster.go`, `hack/parity-configure.sh` (the `--plan` contract cases) |
| D30 | Go only | **The project root is found by a marker walk, and `.lok8s/lo` is not a marker.** The bash entrypoint derives `PATH_BASE` from its own location (`<project>/.lok8s/lo`) and needs no marker. The binary walks up from the working directory to the nearest `clusters/` directory or `kind: Project` `lok8s.yaml` (an exported `PATH_BASE` still wins, WP9 step 1 is parked). A vendored bash tree alone marks nothing since WP9: the code lives in the binary and its cache, so `.lok8s/` is the asset and tree directory only. A project that keeps its clusters elsewhere (`PATH_CLUSTERS`) writes the project file. D30 also names the entry about the space kubeconfig in [Process model](#process-model). | `internal/config/paths.go` (`isProjectRoot`, `FindProjectRoot`) |

### Fail-loud instead of fail-silent

| # | Implementations | Deviation | Where |
|---|---|---|---|
| D10 | Go only | **KubeOne provider detection errors out.** The bash call inside `extract_vars` ran under a disabled errexit, so a spec with no detectable provider silently rendered `cloudProvider: "": {}`. The binary prints `No provider found in cluster spec` and stops. | `internal/driver/kubeone/vars.go` |
| D11 | Go only | **KKP unsupported provider / non-numeric replicas abort before the wire.** The bash printed the error and then POSTed a mangled payload the server rejected. Same message, no request. | `internal/driver/kkp/kkp.go` |
| D12 | Go only | **Tool-not-found checks in `lo secrets`.** `sops` and `ssh-to-age` are libraries in the binary, so their "not installed" branches do not exist. | `internal/secrets/ops.go`, `internal/secrets/rekey.go` |
| D24 | Go only | **sops is the kernpilot age-only fork.** `go.mod` replaces `github.com/getsops/sops/v3` with `github.com/kernpilot/sops/v3` (upstream v3.13.3 minus every key backend except age). A file or a `.sops.yaml` rule with a KMS, GCP KMS, Azure Key Vault, Vault or PGP recipient is rejected with `unsupported key type <x> (age-only build)`; bash used the full sops CLI, which could serve them. age files stay interoperable with the sops CLI. | `go.mod` (`replace`), `internal/secrets/sops.go` |
| D28 | Both implementations | **A credential with a CR or LF is refused.** Both implementations hand `KKP_TOKEN` and the KKP request body to curl through a config on stdin, and the CAPI credentials to kubectl through an env file on stdin (D20 family). Both carriers are line based, so a newline in a value would end the line and start another option or key. The value is refused with `environment variable <NAME> must not contain a newline`, and no tool runs. The body escapes a line feed, a carriage return and a tab, which curl turns back into the same bytes. Earlier bash trees passed these values as arguments and let the tool fail or mangle them. | `internal/credentials/credentials.go`, `internal/driver/kkp/api.go`, `internal/driver/capi/generate.go` (`credentialEnvFile`), `.lok8s/drivers/kkp/api`, `.lok8s/drivers/capi/generate` (`capi::_credential_env_file`), `.lok8s/utils/http.sh` (`http::curl_config`) |
| D33 | Go only | **`split: failed to split …` and `split: failed to shape …` name the cause.** The bash prints the artifact path alone; the binary appends the error it observed (`: rename …: invalid cross-device link`, `: exit status 1`, `: yq not found — install the pinned toolchain (b install)`). The scratch dir moved with it: the bash stages the split under `mktemp -d` (`$TMPDIR`) and moves the files with `mv`, a copy across devices; the binary stages under `<domain>/.artifacts-tmp.*` next to `.artifacts-stage.*` (a rename across filesystems fails with EXDEV, and v0.3.0 failed every split-mode build on a host or CI runner whose `/tmp` is its own mount) and falls back to copy + fsync + remove when a rename still crosses a device. No harness pins the messages; `hack/parity-build.sh` runs one build with `TMPDIR` on another filesystem than the project. | `internal/build/split.go`, `internal/build/move.go`, `hack/parity-build.sh` |
| D34 | Go only | **`lo doctor` reports the registry containers' certificate mount.** Both implementations keep the registry TLS certificate in the docker volume `<network>-registry-tls` since v0.4.0 (the mint hands the Secret plugin a scratch `PATH_SECRETS` under `clusters/<domain>/.registry-tls-tmp.*`, populates and reads the volume through a throwaway container, every registry mounts it at `/etc/registry/certs`, a legacy `<Base>/.secrets/tls/registries` pair is imported once with one `[warn]`; `hack/parity-orchestrate.sh` diffs the docker argv and the plugin environment of `lo up` and `lo registry tls renew` byte for byte, and `lo registry tls status\|renew` print the same lines on both sides). Only the binary adds one line under `lo doctor`'s dev-TLS section when at least one registry container of the active domain exists: `registry TLS: N containers mount volume <vol>`, or a warning naming the legacy directory the containers still bind-mount and the recreate command. The bash doctor prints nothing there; the line is omitted without containers, so `hack/parity-configure.sh` keeps diffing doctor strictly. One more text can differ: `lo registry tls status` names an unreadable certificate `unreadable: <reason>`; the bash says `no PEM block` (openssl's failure carries no usable reason) or `openssl not found` (no openssl on the host), the binary prints the parser's reason (`no PEM block` for a non-PEM blob, the x509 error for a malformed one). No row claims a docker argv deviation: the argv of both implementations is diffed byte for byte. | `internal/cli/cmd_doctor.go` (`doctorRegistryTLS`), `internal/driver/lo/registrytls.go` (`RegistryTLSDoctor`) |
| D35 | Go only | **`lo doctor` reports `PATH_SECRETS` as it is.** The bash entrypoint defaults `PATH_SECRETS` to `<project>/.secrets` before doctor runs, so the bash doctor prints that path (`! PATH_SECRETS unset or not a directory (<project>/.secrets)` when the directory is absent). The binary defaults nothing (since v0.4.0 every store is per domain and the flat store is retired): with the variable unset it prints the info line `ℹ PATH_SECRETS unset (per-domain stores; the flat store is retired)`; with the variable set both print the same `PATH_SECRETS=<dir>` line. `hack/parity-configure.sh` allow-lists the one line in the strict doctor cases, pins the binary's text, and diffs the set case strictly. | `internal/cli/cmd_doctor.go` (`doctorEnvironmentSection`) |
| D36 | Go only | **`lo kustomize build\|clean\|list` and the generated `.bin/b.yaml` follow `KUSTOMIZE_PLUGIN_HOME`.** The bash `libs/kustomize` installs and lists under `${PATH_BASE}/.kustomize` whatever the variable says. The binary uses `config.KustomizePluginHome` (the variable when set, else `<project>/.kustomize`): the plugins land where the render and `lo doctor` look, so doctor's fix hints (`lo kustomize build`, `lo toolchain install`) hold with an exported home. The `file:` lines of a generated `b.yaml` name the home relative to `.bin` when it is inside the project (`../.kustomize/…` by default, byte-identical to before) and absolute otherwise. With the variable unset — the parity harnesses unset it — both implementations agree. | `internal/cli/cmd_kustomize.go`, `internal/cli/cmd_toolchain.go` (`pluginFileDir`), `internal/toolchain/template.go` (`PluginFileDir`) |
| D43 | Both implementations | **The node-IP heal reads the apiserver certificate with its own parser.** For a node whose `--node-ip` already agrees, both implementations check whether the apiserver serving certificate names the node address, and run the repair again when it does not (also when an interrupted heal left the pair in `/kind/heal-pki`). After a repair script that exited 0, both run the same check again and warn when it fails. The probe is one `docker exec <node> sh -c …` in both. The bash probe runs `openssl x509 -noout -ext subjectAltName` in the node and reads the SAN text (kind's node images ship openssl: checked on `kindest/node` v1.31.12 and v1.37.0). The binary runs `cat` there and parses the PEM with `crypto/x509`, so it needs no openssl in the node. On a node image without openssl the bash skips the check, which is the behaviour before the check; the binary still checks. The bash accepts only an IPv4 answer of `docker inspect` (`ip::validate_format`), the binary any IP (`net.ParseIP`); `.IPAddress` is IPv4. `TestHealScriptMatchesBash` compares the probes (before and after the repair) with that one substitution, the repair scripts and the warnings. | `internal/driver/lo/heal.go` (`certProbe`, `certRenewalReason`), `.lok8s/drivers/lo/utils/network.sh` (`lo::heal_cert_reason`) |
| D44 | Both implementations | **`lo secrets add-key` and `remove-key`: the lock, the private-key scan and the rule check.** Both implementations lock `.sops.yaml.lock` (flock(2) in the binary, `flock -w` in bash, so the two exclude each other), beside the file at the end of the links of `.sops.yaml` when it is a symlink, and print the same refusal when the wait runs out. Each run resolves that file once (`sopsConfigDest`, `secrets::_config_dest`) and locks, reads and writes it; a path that does not resolve to a regular file is refused, never locked beside the link. The encrypt after the edit still reads the creation rules through the `.sops.yaml` path (`encryptFile`, `secrets::_encrypt_file`). The lock excludes nothing where another user can change the entries of its directory. The bash keeps a newline at the end of the resolved name, as the binary does. Neither gives an `.enc` a time after now, on any encrypt, and neither sets a time from the clock of the process: the kernel stamps a write with a clock that can be behind that clock by one tick, so such a time can be newer than a file that the kernel writes after it. Both warn with one line when the plaintext is newer than the `.enc` just written and its time is after now (a time in the future), and leave the `.enc` at the time of its write. A plaintext that is newer but not after now gets no warning: the binary compares its time with `time.Now()`, the bash with a file that `mktemp` makes in the same directory (`secrets::_after_now`), so a time that a program set in the last tick of the clock of the kernel reads as after now in the bash only. The re-key of add-key touches the plaintexts with the clock of the kernel in both (`touchNow`, utimensat with no times; `touch`). The binary gives an `.enc` with the same time as its plaintext a time of the kernel after it, one try per millisecond and at most 50 (`nudgeNewer` in `encryptFile`); the bash gets that order from the sops subprocess (the test after the rename in `secrets::_encrypt_file`). The first add-key run (the key not in `.sops.yaml` yet) re-encrypts every file of the list that it checked, whatever the times say (`encryptChecked`, `secrets::_encrypt_store … list`): that is the store of the current domain; a rerun re-encrypts only the files that are not finished (D49); a later encrypt of another store of `--all` skips an `.enc` with a time in the future. Both refuse a lock path that is not a regular file before the open, and neither open truncates. The binary opens with `O_NOFOLLOW`; the bash opens with `>>`, which follows a link that is put there after the check (through a link to a path that does not exist it makes an empty file), and then refuses the run when the opened file is not the regular file that the path names (`openedPathOK`, the `/dev/fd` test of `secrets::_lock`). Without a `flock` binary (macOS has none by default) the bash refuses both commands, and `init` (D53), before any write and makes no lock file; the binary locks on every platform. The bash encrypt (of both commands, and of `set --encrypt` and `encrypt`) renames each `.enc` with a `mv` that takes `-T`: GNU `mv`, else `gmv`. Without one, each of these commands refuses before its first write, and `secrets::_encrypt_file` checks again before it stages a file. The binary renames with `os.Rename` (`writeReplacing`), which never moves a file into a directory. The binary scans the raw argv for a private key before cobra parses it, so a value before the subcommand (`lo secrets --domain <x> remove-key …`) is refused too; the bash scans the arguments of the subcommand only, and its group parser can print a value given before it. remove-key matches each `.enc` file against the creation rules through the sops config loader in the binary and through yq in bash. Both refuse a matched rule with an age recipient that does not parse: the binary parses each recipient as sops does (the bech32 checksum included), the bash checks its shape (`age1pq1…`, `age1<plugin>1…`, `age1` and 58 bech32 characters, `ssh-ed25519` or `ssh-rsa`). A recipient with a wrong checksum passes the bash check; its re-key then stops at the encrypt, and the printed command resumes it once the rule is fixed. A key group that sops cannot load (its `age` is not a list, or a list item is a map) is "cannot match the .enc files against the creation rules" in the binary and an age recipient that does not parse in bash. Both refuse a rule with a recipient that is not age, also where the sops CLI could encrypt to it. Both read the recipients of each `.enc` from its parsed sops metadata (encoding/json in the binary, jq in bash) and refuse a file whose metadata does not parse. Both refuse a file that parses and holds the `sops` member twice, or a member twice in an object of the `sops` value (at any depth): a map decode and jq keep the last of two members, so the first could hide a recipient. The binary walks the tokens of encoding/json (`metadataDuplicate`), the bash the stream form of jq (`jq --stream`, `secrets::_enc_duplicate`). Neither reads the other top-level members (the `data` payload) for a duplicate. Before any write both walk each selected store for another `.enc` file (a name that ends in `.enc`, at any depth and dot names included, that is not a `Secret.*.enc` at the top of the store) and refuse it by path: the binary with `fs.WalkDir` over `os.DirFS` (`otherEncs`), the bash with `find -H` and `sort -z` (`secrets::_other_encs`). Neither follows a symlink below the store, both walk a store that is a symlink to a directory, and both refuse a directory below the store that they cannot list or search. The bash also refuses the store when `find` fails and names no such directory. Both re-key every managed `Secret.*.enc` file of the selected stores from its twin, an orphan that `--skip-orphans` accepts the one exception: the labels are not trusted to choose the files. The labels count only in the summary (`removeKeySummary`, `secrets::_rm_summary`: the rules that changed, the files whose labels listed the key, the files re-keyed) and in the messages. A key in no rule and on no label is a warning in both, not a refusal: the run re-keys every managed file and changes no rule, so a mistyped key re-keys every file. Before the first write both decrypt each file that they re-key, in memory, with the identity of the caller, and compare it with its twin: the binary compares the byte slices (`twinsMatch`), the bash pipes the decrypt into `cmp -s` with tracing off and no plaintext file (`secrets::_twin_matches`). Both refuse a twin that differs, a file that they cannot decrypt and a twin that they cannot read, and print no value. After the re-key both read the stores again with the same reads and walk (`postCheck`, `secrets::_post_check`) and fail, with the command that resumes the run, on a file that still lists the key, a path that they cannot read, another `.enc` file, a managed `.enc` file with a twin that the run did not re-key, and, without `--skip-orphans`, an orphan; neither sees a file that is replaced in place under the same name during the run. Both refuse an explicit YAML tag under `creation_rules` other than `!!str`, `!!seq` or `!!map`, and a mapping key that is not a string. Two edges differ, both on the side of a refusal: a key with the non-specific tag `!` (`! x:`) is a string to the binary and refused by the bash; when the root has its own `creation_rules` and a root merge key brings in a second one, the tag check of the binary reads both, and yq reads the root's only. | `internal/secrets/lock.go`, `internal/secrets/sopsrules.go` (`yamlFormCheck`), `internal/secrets/rekey.go` (`encRecipients`, `otherEncs`, `twinsMatch`, `touchPlaintexts`), `internal/secrets/secrets.go` (`nudgeNewer`, `touchNow`), `internal/cli/cmd_secrets.go` (`GuardPrivateKeyArgv`), `.lok8s/libs/secrets` (`secrets::_lock`, `secrets::_mv_t`, `secrets::_rules_check`, `secrets::_rules_form`, `secrets::_enc_lists`, `secrets::_other_encs`, `secrets::_twin_matches`, `secrets::_after_now`) |
| D49 | Both implementations | **A rerun of `lo secrets add-key` with the key in `.sops.yaml` finishes the re-key that it can detect, and the bash needs jq for it.** Both list the store of the sweep once, with a listing whose error they check, and the later steps (the orphan check, the touch, the encrypt) use that list: the binary reads the directory with `os.ReadDir`; the bash runs `find -H <store> -mindepth 1 -maxdepth 1 -name 'Secret.*' -print0` under `LC_ALL=C`, sorts the names by byte, and takes the list only when a record at its end shows that `find` and `sort` exited 0. The bash also refuses a store without read or search permission before the listing (`-r`, `-x`); the binary finds it in the read. With `--all` both list each other store once the same way, and refuse a listing that fails with a line that names that store. Both read the sops metadata of the `.enc` file of each plaintext of the sweep and re-encrypt only the plaintexts whose `.enc` file is missing or does not pass the recipient envelope check; a finished file keeps its bytes and its time. The check is structural: a non-empty `mac` and `lastmodified`, and an `age` entry for the key whose `enc` is an armored age block (the header line and the footer line of `filippo.io/age/armor`); a label alone does not pass. It does not show that sops wrote the entry or that the key can decrypt the file. Only the `enc` fields of the entries of the key take part in the envelope test, and an `enc` spelled another way (`ENC`) fails that entry only. Both still read every entry: a recipient of another entry that is not a non-empty string, or a member that the metadata holds twice, fails the check of the whole file. After the re-encrypt both read the metadata again and fail, with the names of the files, when one still does not pass (the creation rule may not list the key); neither prints `done` then. Before any decision both refuse a store that they cannot list, a sweep `.enc` path that is not a regular file (the line of the encrypt), a `.sops.yaml` file (at the end of its links) that is also the `.gitignore` of the store or a sweep `.enc` (`os.SameFile`, `-ef`, so a hard link counts; one check before the first write; neither compares an orphan `.enc`, a plaintext or a file outside the store), an `.enc` file larger than 16 MiB, the size limit of the metadata check (one line with the file name; neither reads its metadata) and, without `--skip-orphans`, an orphan that does not pass the check; an orphan `.enc` that is a symlink to a path that does not exist counts as an orphan in both. A rerun with nothing left writes the `.gitignore` when it is missing or needs a change (D52), then prints `nothing to add`. The binary reads each file once, from one descriptor opened with `O_NOFOLLOW` and `O_NONBLOCK`, and checks the size with fstat and a read limit. The bash checks the size with `wc -c` and reads the file with jq, in separate opens of the path (`wc -c`, then two jq runs): its size limit holds only when no other writer changes the store during the run. The bash needs jq on this path and, when it has none, refuses before it changes `.sops.yaml`, the `.gitignore` or an `.enc` file (`jq not found: add-key reads the sops metadata of each .enc with jq; it stopped before it changed .sops.yaml, the .gitignore or an .enc file, but the lock file <lock> can be new`, then `install jq (b install), or use the lo binary: it needs no jq`); the binary needs no tool. | `internal/secrets/ops.go` (`rerunLeft`, `finishReKey`, `plaintextsOf`), `internal/secrets/rekey.go` (`listStore`, `storeListing`, `refuseUnlistable`, `refuseConfigAlias`, `unfinishedEncs`, `addKeyOrphans`, `readEncMeta`, `keyAdded`, `encAdded`, `armoredAge`), `internal/secrets/secrets.go` (`storeNames`), `.lok8s/libs/secrets` (`secrets::_list_store`, `secrets::_list_names`, `secrets::_store_names`, `secrets::_rekey_names`, `secrets::_refuse_unlistable`, `secrets::_config_alias`, `secrets::_unfinished`, `secrets::_add_key_orphans`, `secrets::_key_added`, `secrets::_enc_lists`, `secrets::_finish_rekey`) |
| D50 | Both implementations | **The open of the lock of `add-key` and `remove-key` after its check, and the lock of the bash.** The binary opens `.sops.yaml.lock` with `O_NOFOLLOW` and `O_NONBLOCK`: a link put there after the check fails the open with ELOOP, a FIFO with no reader fails it with ENXIO, and a FIFO with a reader or another kind of entry fails the fstat test (`openedPathOK`). Each prints the line of the check, and the open made no file. The bash opens with `>>`, which follows a link and waits on a FIFO until a reader opens it. Then `secrets::_opened_ok` tests that the descriptor is a regular file, that the path is not a link and that the two are one inode (`-ef`). On a failure the bash closes the descriptor and prints two lines: the path changed after its check and the command stopped before it changed `.sops.yaml`, a `.gitignore` or an `.enc` file, but the open can have made the lock file, or an empty file at the path that a link names (remove the lock path and that file). Another regular file at the path after the open fails the identity test in both; the binary then prints one line: the path changed after its check, and the command stopped before it changed `.sops.yaml`, a `.gitignore` or an `.enc` file, but its open can have made the lock file. The bash reads the descriptor through `/dev/fd`, so before the lock file it checks that it can (`secrets::_fd_ok`): without that (on Linux, no `/proc`) it refuses with two lines that name `/dev/fd` and `/proc`, and makes no lock file. The binary uses fstat and needs no `/dev/fd`. The bash keeps the descriptor, and so the lock, until its process exits; the binary releases the lock when the command returns. A lock that another run holds after the wait gives the same line in both (`<lock> is held by another lo secrets init, add-key or remove-key`); the bash knows it from the exit status 99 of `flock -E 99`. Another failure of the lock is `cannot lock <lock>` in the binary. In the bash it is `cannot lock <lock> (flock exited with <status>)`, then that the command stopped before it changed those files, that its open can have made the lock file, and the next step (fix the cause of the flock error, then run the command again). The bash also closes the descriptor when `flock` fails. Both lock the `.sops.yaml` file, not a store, and trust the directory of the lock. | `internal/secrets/lock.go` (`lockSopsConfig`, `openedPathOK`), `.lok8s/libs/secrets` (`secrets::_lock`, `secrets::_opened_ok`, `secrets::_fd_ok`) |
| D52 | Both implementations | **The `.gitignore` of a secret store: the block of lo, the rules of the user, and a check that reads the whole file once.** Both write the rules of lo as a block between the line `# BEGIN lok8s secrets. lo rewrites the lines up to the END line: add your own rules below it.` and the line `# END lok8s secrets`, and keep each other line byte for byte (a CR, an empty line, a last line with no newline). The block ignores every path (`*`), re-includes `.gitignore` and `Secret.*.enc`, then ignores every directory again (`*/`): a pattern with no trailing slash matches a directory too, and git reads a `.gitignore` in each directory that it goes into. A file with no marker line gets the block at the top, and its bytes follow the block. A file that starts with the five lines of an older lo gets the block in place of those lines. A UTF-8 BOM at the start of the file stays at the start: both read the first line without it, and the block goes after it. Both read a rule as git does: a line whose first byte is `#` is a comment, then one trailing CR goes, then the trailing spaces that a backslash does not escape; a rule that starts with `!` is a negation. Both refuse, with one line per cause (`<file>:<line>: <reason>`; the rule is never printed, and a byte of the path outside printable ASCII shows as `?`) and a final line that names the step that the command stopped before (`<file> was not changed, and lo stopped before <step>: fix each cause above, then run this command again`): each line after the block (below the END line, or in a file with no block) whose first byte is `!` (`\!x` is a positive rule and passes); a rule inside the block that is not one of its four rules; any marker shape other than one BEGIN line, then one END line; a NUL byte; a file of more than 1048576 bytes; a plan of more than 1048576 bytes (the next run would refuse the file that it writes). A file that it cannot read is refused with one line. A rule above the block is not read: the ignore-all rule comes after it. The steps: `init` and `add-key` check before their first write (`its first write`), `init` again at its write of the file, before `.sops.yaml` (`the .gitignore write`), `add-key` before its change of `.sops.yaml` (`the change of .sops.yaml`), `encrypt` and the re-key of `add-key` before the `.enc` writes, `set` before the plaintext write and `decrypt` before the first plaintext write; none writes a file that has the bytes of the plan already. `remove-key` runs no check: it writes the lock, `.sops.yaml` and the `.enc` files, and no plaintext. `set` refuses a key, before any write, when the name of its plaintext, in ASCII lowercase, ends in `.enc`; `decrypt` refuses each `.enc` whose plaintext name ends so, before its first plaintext write (one line per file, then a final line): `!Secret.*.enc` re-includes such a name, and with `core.ignorecase` also one that ends in `.ENC`. The binary reads the file into memory with a read limit. The bash reads it once into a private temp file (`umask 077`, `head -c`), checks the status of each read (`mapfile` included), takes the checks, the plan and the bytes of the write from that copy, and removes the copy on each path; when it cannot remove it, it refuses with one more line, which the binary does not have. The write goes to a temp file and a rename, with mode 0644. The bash asks for a `mv` that takes `-T` only when it writes the file. Before the value read and any write, `set` refuses a name, a namespace or a key with a `/`, a `\`, a control byte, or that is `.` or `..` (the error names the field and its text, never the value), and then a plaintext path whose directory is not the store. `set` and `decrypt` refuse a plaintext path that is not a regular file and not absent (a symlink, a directory, a FIFO, a socket or a device, or a lookup that fails) before any write; `decrypt` checks the path of each `.enc`, also for a plaintext that it would keep. Both write the plaintext to a new private temp file in the store (mode 0600) and rename it onto the path, so a hard link keeps the old content; the bash asks for the `mv -T` before the first write of `set` and `decrypt`, also without `--encrypt`. A write that fails prints a fixed line that names the step and the path, not the text of the file system error, and the temp file goes. Not covered: a tracked plaintext file, `git add -f`, a symlinked store or parent directory (followed), a change of the file (or of a plaintext path) between the check and the rename, a concurrent writer, the `.sha` and `.bash-allow` files that the secret generator of `lo build` writes (hashes, no values), and a plaintext named `*.enc` that exists already (`encrypt`, `list` and `print` read it as a ciphertext, and `decrypt` fails on it). `.git/info/exclude` and `core.excludesFile` have a lower precedence and cannot re-include a path of the store. | `internal/secrets/gitignore.go` (`reservedPlaintextName`, `printable`: these call storeguard), `internal/secrets/secrets.go` (`gitignoreOK`, `guard`, `writePlaintext`, `makeStore`, `ensureGitignore`), `internal/secrets/ops.go` (`Init`, `AddKey`, `Set`, `Decrypt`, `refuseReservedDestinations`, `refuseLinkedDestinations`), `kustomize/pkg/storeguard` (the code of these checks and of the write: `PlanGitignore`, `gitignoreRule`, `Guard.GitignorePlan`, `Guard.EnsureGitignore`, `Guard.WritePlaintext`, `Guard.CheckPlaintext`, `Guard.MakeDir`, `Guard.FailedWrite`, `SyncDir`, `CheckName`, `EntryName`, `PartProblem`, `Destination`; the binary syncs the store directory after a new `.gitignore` and after a plaintext, and the bash does not; the secret generator of `lo build` writes its cache through `CheckName` and `Guard.WritePlaintext` too, with the refusal texts of `set` (a failed write names no command), and runs `Guard.CheckPlaintext` before a generator and in a dry run), `.lok8s/libs/secrets` (`secrets::_gitignore_ok`, `secrets::_gitignore_plan`, `secrets::_gitignore_problem`, `secrets::_gitignore_rule`, `secrets::_gitignore_compose`, `secrets::_ensure_gitignore`, `secrets::_shown`, `secrets::_enc_reserved`, `secrets::_decrypt_reserved`, `secrets::_decrypt_links`, `secrets::_set_part_ok`, `secrets::_set_destination`, `secrets::_plain_temp`, `secrets::_plain_write_failed`) |
| D53 | Both implementations | **`lo secrets init` writes `.sops.yaml` through a temp file under the lock of `.sops.yaml`, and refuses a key that derives no recipient.** A key that is not ed25519 derives no age recipient: ssh-to-age skips it and exits 0. Both refuse that key before any other step, also on a rerun, with `no age recipient derived from <key> (ed25519 SSH keys only): nothing written to <path>` and status 1. Earlier bash trees wrote `age: ''` and exited 0, and a rerun took the empty recipient for a configured key (Q5). Then both check the `.gitignore` of the store (D52). When a regular file is at the path, they take the rerun branch. A first run checks the path without following a link. It refuses a symlink to no file or a directory before the lock, so it makes no lock file. Then both take the lock of `add-key` and `remove-key` (D44, D50) beside the path, and check the path again. A file that another run wrote while this run waited stays, and a line of its own names it. Next, both make the store and its `.gitignore`. Last, they write a temp file `..sops.yaml.tmp-*` in the same directory, give it mode 0644 with a chmod (the umask does not apply), and rename it onto the path. The run holds the lock until it ends. A store step that fails leaves no `.sops.yaml`. A write that fails prints one line that names the file, and one more line names a temp file that the run cannot remove. The binary syncs the temp file before the rename, and the bash does not. Before its first write, the bash needs `flock` and a `mv` that takes `-T` (GNU `mv`, else `gmv`). Without them it refuses and writes nothing. The binary needs neither. Not covered: a program that is not lo and writes `.sops.yaml` between the check and the rename. It takes no lock, and the rename replaces its file. Also not covered: the temp file of a bash run that a signal kills during the write. No trap removes it, but the `.gitignore` of `lo init project` ignores it, and no run reads it as the config. | `internal/secrets/ops.go` (`Init`, `initConfigFree`, `writeInitConfig`), `internal/secrets/lock.go`, `.lok8s/libs/secrets` (`secrets::init`, `secrets::_init_config_free`) |
| D54 | Both implementations | **The key of `lo secrets init`, and a key file that holds a private key.** `LOK8S_SSH_KEY` names the private key that `decrypt` reads (default `~/.ssh/id_ed25519`). `init` without `--ssh-key` reads its `.pub` file: the path itself when it ends in `.pub`, else the path with `.pub` added (default `~/.ssh/id_ed25519.pub`). Earlier trees read the private key there: `init` failed, and the bash printed the error of ssh-to-age, which quotes its input. The own-identity check of `remove-key` reads the same `.pub`. `init`, `add-key` and `remove-key` refuse a key file that holds a private key before ssh-to-age (bash) or the library (Go) reads it. Such a file has a line that starts a PEM or OpenSSH block (`-----BEGIN … PRIVATE KEY-----`, in any form, the PGP form `-----BEGIN PGP PRIVATE KEY BLOCK-----` and a type with a hyphen included), the SSH.com form or a PuTTY key file, also after a UTF-8 byte order mark, or an age identity anywhere in the file. Both read the key file once and at most 64 KiB. They refuse a bigger file, a file with a NUL byte and a file that they cannot read, with the path and the cause. The bash finds a NUL byte in the same read: `mapfile` splits the read at each NUL. The bash gives ssh-to-age the checked text on stdin, so the check and the tool see the same bytes, and it checks with a `[[ =~ ]]` match, not grep: no exit status of a check can read as "not private". The refusal names the path and the fix, and prints nothing from the file. The own-identity check takes such a file, or one it cannot read, as no match. Every `lo secrets` call is checked before a parser reads it. When the words of the argv, joined with spaces, have the shape of a private key, the call stops. That includes a PEM, OpenSSH, SSH.com or PuTTY paste split into words, and an age identity. The binary checks the raw argv before cobra parses it (`GuardPrivateKeyArgv`): the `secrets` tree, and an argv that cobra cannot resolve to a command. The bash checks it in `main` of `lo` (`secrets::_guard_lo_argv`), because that parser takes the global flags, `--domain` too, from anywhere in the argv. It checks again in `main::secrets` and in the four commands. Both print the same two lines. For `set` (its argument is a value) the second line names stdin (`RefusePrivateKeyValue`, `secrets::_no_private_value`). In a project that routes `lo secrets` to the bash tree, cobra stops at the shim, and the binary takes the subcommand from the words by the rule of `secrets::_argv_words` (`secretsSubcommand`). For `set` (and its alias) the check of the argv also refuses any word that holds a PEM line, `-----BEGIN` or `-----END` in any case (`holdsPEMLine`, which folds the ASCII letters only, as the bash does under `LC_ALL=C`; a certificate, a public key, a static key of OpenVPN), with the line that names stdin: such a value starts with `-`, so it is a flag to both parsers of `set`, and pflag's `bad flag syntax: <arg>` prints all of it. When the joined argv has no private key, the first line of that refusal names the PEM block: `a value holds a PEM line (-----BEGIN or -----END): it is not printed`. Else it names a private key (`RefusePrivateKeyValue`, `secrets::_no_private_value`). For the other commands of `lo secrets` the check of the argv refuses such a word too, with the line that names the key file: no command of `lo secrets` takes a PEM block as an argument (`lo secrets list <block>` and a typo of set gave the same flag error). The checks of `init`, `decrypt`, `add-key` and `remove-key` count any PEM line (`LooksLikeKeyArg`, `holdsPEMLine`, `secrets::_is_key_arg`): these commands take a path or an age public key, and cobra's flag error prints the first word of a paste split into words. The binary takes the subcommand from cobra's `Find`, and from the words (`secretsSubcommand`) when `Find` stops at the group or at the shim; the bash takes it from the words. For an unknown flag before `secrets` and for `lo -- secrets set`, `Find` does not reach set. Both refuse, and only the second line of the refusal differs: the binary refuses as for a command that it cannot resolve (fail closed, the line that names the key file), and the bash names stdin. The binary also refuses when the first word of the argv by the rule of the bash (a flag that it does not know is a switch, `namesSecrets`) is `secrets`. For `lo <block> secrets l`, cobra takes `secrets` as the value of the flag `<block>` and resolves `lo lint`, whose flag parser would print the block. The second line of a refusal can differ when `Find` and the words of the argv do not agree on the subcommand. For `lo secrets --typo X set k <block>`, the binary names stdin and the bash names the key file. For `lo secrets -z set list <block>` and `lo <block> secrets set k`, the binary names the key file and the bash names stdin. The first line is the same in both. With the same scope, both refuse an argv that holds a control byte (0x00-0x1F or 0x7F) and no key, `set` excepted, with lines of its own: `an argument holds a line break or a control character: it is not printed`, then the line that names `set` and the path of the key file (`RefuseControlByte`, `secrets::_no_control_byte`). A key body pasted without its BEGIN line has no other mark, and the parsers printed it (`unexpected argument: <body>`, `Invalid command: <body>`). The value of `set` is any text, so `set` passes. Where `Find` and the words of the argv do not agree on set (the argvs above), the binary refuses the control byte, and the bash tree leaves the call to its parser: it stops at the unknown flag, or it runs set after `--`. Neither prints the value. The four commands do not look for a control byte in their own arguments. Through cobra alone (the tests that skip the check of main), the flag error of a paste split into words holds only its first word, `-----BEGIN`: its first line then names the PEM block, not a private key. The binary runs the check of main first, on the whole argv. When nothing is at the path of the `.pub` that `init` reads (its lstat fails) and its private key is a regular file, both print the hint `ssh-keygen -y -f <key> > <key>.pub`, else `ssh-keygen -t ed25519`. Each path in the hint goes through the placeholder rule, then into single quotes (each `'` as `'\''`) when it has a byte that is not an ASCII letter, a digit or one of `_@%+=:,./-` (`shellQuoted`, `secrets::_shown_quoted`). When something that is not a regular file is at the path of the `.pub` (a directory, a FIFO, a link to no file), both print no hint. When the placeholder rule below replaces the path, both print no hint either: the placeholder would be in it, and a paste of it makes a file of that name. Both decide this with the placeholder rule itself (`shownKeyPath(path) != path`, `secrets::_path_is_shown`), and so does the fix line of the refusal of a private key file (`refusePrivateKeyFile`, `secrets::_read_public_key`). The private key is checked with a stat only. Each message that names the key file prints a placeholder for a key path that has the shape of a private key, `(not printed: it looks like a private key)`, or that holds any other PEM line, `(not printed: it holds a PEM block)`, or that holds a control byte (0x00-0x1F or 0x7F: a line break, a tab, an escape), `(not printed: it holds a line break or a control character)` (`shownKeyPath`, `keyPlaceholder`, `HoldsControlByte`, `secrets::_shown_path`, `secrets::_holds_control_byte`). Both read bytes, not runes: a C1 character prints as it is. A long paste on one line with no marker prints as it is: no rule tells it from a long path. The not-a-file line, the derive line and the not-a-valid-age-key line of `add-key` and `remove-key` print their argument through the same rule (the check of the argv refuses such an argument first). Such a path can come from `LOK8S_SSH_KEY`. The bash keeps the stderr of ssh-to-age from the terminal on every call, and prints the exit status of the tool on a failure. The binary links the library: it has no exit status, and prints no library error. Neither prints the skip note of ssh-to-age for a key that is not ed25519: the refusal of D53 names the cause. `decrypt` and the identity of a decrypt (`DecryptEntry`, `secrets::decrypt_entry`) read the private key by design, with the stderr of ssh-to-age contained. | `internal/secrets/keys.go` (`DefaultSSHKey`, `DefaultSSHPublicKey`, `privateKeyFileRe`, `readPublicKeyFile`, `refuseKeyFile`, `refusePrivateKeyFile`), `internal/secrets/rekey.go` (`LooksLikePrivateKey`, `LooksLikeKeyArg`, `RefusePrivateKey`, `RefusePrivateKeyValue`, `RefuseControlByte`, `shownKeyPath`, `keyPlaceholder`, `holdsPEMLine`, `HoldsControlByte`), `internal/secrets/ops.go` (`publicKeyHint`, `shellQuoted`), `internal/cli/cmd_secrets.go` (`GuardPrivateKeyArgv`, `isSecretsSet`, `isSecretsGroup`, `secretsSubcommand`, `underSecrets`, `sshKeyCommand`, `privateKeyGuarded`), `.lok8s/lo` (`main`), `.lok8s/libs/secrets` (`secrets::_ssh_public_key`, `secrets::_read_public_key`, `secrets::_shown_path`, `secrets::_path_is_shown`, `secrets::_shown_quoted`, `secrets::_shell_quoted`, `secrets::_holds_control_byte`, `secrets::_is_private_key`, `secrets::_is_key_arg`, `secrets::_argv_words`, `secrets::_guard_lo_argv`, `secrets::_no_private_argv`, `secrets::_no_private_key`, `secrets::_no_private_value`, `secrets::_no_control_byte`, `secrets::_tool_status`, `secrets::init`) |

### Rendering and display

Not a deviation: since v0.5.0 the `✓ ! ✗` markers of `lo doctor` and the
`[error]`/`[warn]`/`[debug]` prefixes carry ANSI colour only on a
terminal with `NO_COLOR` unset. **Both** implementations gate the colour
(`internal/ui`, `utils/verbose.sh`, `libs/doctor`). The piped bytes did
not change, and the harnesses still diff them strictly. Titles, sections
and tables on a terminal are the binary's presentation
([Output](cli.md#output)), which no harness diffs.

| # | Implementations | Deviation | Where |
|---|---|---|---|
| D13 | Go only | **Progress UI on a terminal is the final state, not a live spinner.** The bash tty path redraws one spinner line with the count of objects in place on `/dev/tty` (no kubectl line: the apply output stays private); the binary renders the identical *final* summary and the fixed error line after the phase. Off a terminal (CI, Tilt logs, `LOK8S_NONINTERACTIVE`, `DEBUG`) the output is byte-identical. | `internal/kapply` |
| D14 | Go only | **YAML edits re-serialize with yaml.v3 formatting.** The CAPI placement-group and KubeOne value edits keep the merged *content* as the contract; edited documents come out with 2-space indentation (comments preserved), untouched documents pass through byte-identical. yq's 4-space sequence indent is not reproduced. | `internal/driver/capi/pg.go`, kubeone `yamledit` |
| D15 | Go only | **`lo image cache` / `list` do not rewrite `.registries.json`.** The bash sourced the whole Lo driver, which regenerated the file as a side effect; the binary computes the same values from the spec without the rewrite. | `internal/image/image.go` |
| D16 |  | **SOPS output is not byte-compared.** Encryption is nondeterministic; cross-tool decrypt is the contract. | `internal/secrets/sops.go`, `hack/parity-build.sh` |
| D17 | Go only | **The kind config is a temp file, not a process substitution.** One `kind create cluster … --config <path>` argv line differs. | `hack/parity-orchestrate.sh` |
| D18 | Go only | **Operator hooks: the errors for a bad binding context.** For three inputs the exit code is the same in both: 1 for an unset `BINDING_CONTEXT_PATH`, 2 for a file that cannot be opened, 5 for a file that does not parse. `TestReadBindingContext` pins these codes. The harness checks only that the two codes are equal. Three messages differ. An unset variable: the bash `set -u` abort names the script line, the binary prints `error: BINDING_CONTEXT_PATH: unbound variable`. A missing file: jq prints `jq: error: Could not open file <file>: No such file or directory`, the binary adds the `os.ReadFile` error (`jq: error: Could not open file <file>: open <file>: no such file or directory`). A truncated file: jq prints where its parser stopped (`jq: parse error: Unfinished JSON term at EOF at line <n>, column <m>`), the binary names the file and the `encoding/json` error (`jq: parse error: <file>: unexpected end of JSON input`). Two inputs give different exit codes, and the harness has no case for them. An empty `BINDING_CONTEXT_PATH`: jq cannot open the file `""`, and the hook exits 2. The binary handles the empty value as unset and exits 1. An empty file: jq prints nothing and exits 0, so the hook handles no event and exits 0. The binary exits 5 with the `encoding/json` error. | `internal/operator/operator.go`, `internal/operator/operator_test.go`, `hack/parity-operator.sh`; the bash hooks are in `.archive/legacy/operator/hooks/` (`hack/parity-operator.sh` copies them) |
| D19 | Go only | **The render needs no `kustomize`/`khelm`/`.kustomize/` and no `KUSTOMIZE_PLUGIN_HOME`.** `lo build`, the addon render and the registry TLS mint run in-process ([In-process rendering](#in-process-rendering)); the bash tree execs the pinned binary and the built plugins. Output bytes are identical; the *failure* modes differ where a plugin binary is missing: bash's `lo up` stops on `the Secret plugin is not built at …`, the binary mints in-process and proceeds. `LO_RENDER=exec` reproduces the bash behaviour (the orchestrate harness pins it and ships a stub plugin, so the mint runs on both sides). | `internal/render`, `internal/driver/lo/registrytls.go`, `hack/parity-orchestrate.sh` |
| D37 | Go only | **The 503 `AT_CAPACITY` message names the control-plane shape, not a plan.** The bash `kubehz::render_capacity_rejection` reads `detail.tier` and prints `at capacity for the '<tier>' plan` with "per-plan availability". Plans are gone: the api keys capacity on the shape and `detail` carries `{ replicas, used, limit, retryAfter }` (the shape gate), `{ option: "dedicated", metalNodes, metalRequired, retryAfter }` (the metal gate) or `{ pool: "free", used, cap, retryAfter }` (the free pool). The binary prints the requested shape, the count in use and the free slots. Then come the retry hint, a smaller-shape hint (fewer apiserver replicas) and the `/api/capacity` pointer. A `detail.tier` from an older api still names the subject, without the plan words. Go-only; no parity case. | `internal/kubehz/hosted.go` (`renderCapacityRejection`) |
| D38 | Both implementations | **`lo kubehz token`, new in both implementations.** The kubectl exec credential plugin for a kubehz agent key: client_credentials at `--token-url` with `KUBEHZ_AGENT_CLIENT_ID` and `KUBEHZ_AGENT_CLIENT_SECRET` (or `--secret-file`), printed as an ExecCredential or, with `--format token`, as the bare token. Both implementations print the same bytes and the same refusal texts, and share the cache file (`${XDG_CACHE_HOME:-~/.cache}/lok8s/kubehz-token/<sha256 prefix>.json`, `{"access_token","expires_at"}`, 0600, reused until five minutes before expiry). The bash twin hands the secret to curl as a config on stdin (`-K -`), never on argv. curl runs with `-q` first, so a `~/.curlrc` never applies to that request. Both implementations try a grant again after 0.25, 0.5 and 1 s on a failed connection, a 5xx or `invalid_client` (a key made a moment ago), print only the last failure, and end each attempt after 30 s without a retry (a TLS handshake that stalls is a timeout too; a TCP connect ends after 10 s in bash, inside the 30 s in Go). Both refuse a C0 control character or DEL in the client id or the secret (bash tests under `LC_ALL=C`), floor a fractional `expires_in` and cap it at a day, read a `--secret-file` that is a pipe, and trust a cache file only when it is a regular file (not a link) of the same user with no group or other bits (Go opens it with `O_NOFOLLOW` and checks the open file). The command is hidden in both trees (`'#token'` in the argsh usage, `Hidden` in Go), so no MCP server offers it: the argsh `lo mcp` that `lo chat` drives skips hidden leaves whatever a project's chat config says. The Go `lo mcp` also refuses it by its annotation (`lok8s.dev/credential-output`), and the chat defaults deny `lo_kubehz_token`; `TestChatDeniesEveryCredentialOutputCommand` keeps the annotation and the deny list in step. The Go command reads no project and prints no run header, because kubectl runs it from any directory. `hack/parity-kubehz.sh` diffs the local refusals, the extra-argument parse error and three cache hits from a seeded cache; the Go tests and the bats file pin the grant, the retries and the timeout with the same values. | `internal/kubehz/token.go`, `internal/cli/cmd_kubehz.go` (`newKubehzToken`), `.lok8s/libs/kubehz/main` (`kubehz::token`); tests `internal/kubehz/token_test.go`, `tests/unit/kubehz_token_test.bats` |
| D39 | Both implementations | **`lo kubehz space list\|get\|create\|delete\|lease\|kubeconfig` and `lo kubehz cluster list\|get\|lease\|kubeconfig`, new in both implementations.** These are the agent tools. `lo mcp` turns them into tools by their markers. List and get are `@readonly`. `space create` has no marker, so it is mutating. Delete and lease are `@destructive`. Kubeconfig is credential output. The commands call the kubehz api at `KUBEHZ_API_URL`, which must be https. The bearer is the access token of the agent key in the environment, from the grant and the cache of `lo kubehz token` (bash `kubehz::token_access`, Go `agentAccessToken`). Without a key, `KUBEHZ_TOKEN` is the bearer. No flag names the URL or a credential. Both implementations print the same bytes in the text, `-o json` and `-o yaml` forms. Before any request, both check the id shape, the numbers and the kubeconfig path. A path in `/proc` or `/dev`, or a path that a link sends there, is refused: `/proc/self` names the process that asks, so in Go `/proc/self/exe` was the `lo` binary. A directory or a link to one is refused. A path that exists needs `--force`. With `--force`, `lo` writes through a link only when no other user can change it: the walk follows at most 8 links, each link belongs to root or to the user, each directory from `/` down to a link and to the target belongs to root or to the user and is not writable by its group or by others without the sticky bit, and the target is a regular file of the user (#248). The check reads the owners and the mode bits only, not ACLs. A refusal names the class that has the write bit (`writable by its group` or `writable by others`); the join ticket check of the temp dir uses the same words. Each name of the path that a `/` follows must be a directory that exists, in the argument and in the target of a link, as the kernel requires: `real/`, `real/.`, `missing/../real` and `file/../real` are refused before any request, and `..` after a link to a directory goes up from where the link points. A relative path starts at the working directory of the kernel (bash `pwd -P`, Go `os.Getwd`), not at `$PWD`; when the working directory cannot be read, both refuse with the same text. The path check runs again just before the write. Without `--force`, a hard link publishes the file, so a file that appears during the download stays. When the link fails for another cause than a path that exists, both write the file with an exclusive create, as on a file system with no hard links (vfat, exFAT, some FUSE and SMB mounts). Neither reads an errno list or the text of `ln`: Go refuses `EEXIST` and takes the create on every other link error, and the bash refuses when a file or a link is at the path after `ln` fails. Both also refuse when the temporary file is not at its name as the file that they wrote (Go: `os.SameFile` of its `Stat` and an `Lstat`; bash: the device and the inode of `stat` just after `mktemp`). The twins differ in how they name the files. Go opens the directory of the file one time as an `os.Root`. After a successful open, each step works relative to it: the create of the temporary file (`O_EXCL`, a name of 130 random bits), the rename, the link, the exclusive create, each `Lstat` check and each remove. So when a directory of the path changes after the open (another directory, or a link put in its place), no step goes to another directory. Go refuses a last name that is empty, `.` or `..` before it makes the temporary file, with the refusal of a directory (`<file> is a directory`); the path check refuses such a path first. The open needs the read right on the directory (`os.OpenRoot` opens it read-only): Go refuses a directory with only the write and the search rights (mode 0300) with `cannot write <file>` and the hint `lo also needs the read right on <dir>`. The bash writes the file in such a directory, so the hint is Go only and has no bash text to match. The bash does each step by path: there, a change of a directory of the path after the inode check can still send the exclusive create to another directory, and a change between a check and a remove can make the remove take another file. Each twin makes one attempt to remove the temporary file. Go removes it only while the name names the file that it wrote, and by name with no check when the `Stat` of the open file failed; after a rename that worked, Go removes nothing. The bash runs one `rm -f` of the temporary path with no check on every path, also after an `mv` that worked. In both, a process that can change the entries of the directory can still interfere: it can put another file in place of the temporary file before the link or the rename, or put a file at a name between a check and the remove after it (each check is a separate step before its operation). Go opens with `O_EXCL`. The bash opens under `noclobber` and umask 077, which uses `O_EXCL` for a missing path but opens a FIFO or a device without it: the bash refuses when descriptor 3 is not an empty regular file, and then writes, changes and removes nothing (on a FIFO with no reader the open waits). Before the first byte, both set 0600 on the open file (a default ACL of the directory replaces the umask, and the bash open asks for 0666), check that the path still names the new file (else nothing is written or removed), and read the mode back. The bash also reads the mode before the chmod: when the open gave group or other bits and the chmod took them away, the directory gives other users access to a new file (a default ACL), and a process could open the empty file before the chmod. Then the bash writes nothing and removes nothing: another process can put a file at the path between a check and a remove, so the empty file stays. It prints `cannot write <file> safely: its directory gives other users access to a new file (a default ACL)`, says that this run wrote no part of the kubeconfig and that the path can hold the empty file or a file of another process, and tells the user to check the file before they remove it. Go asks for 0600 at the open and writes the file with mode 0600 there. When group or other bits stay (vfat and exFAT set the mode at the mount), both write the file and print `[warn] kubehz <noun> kubeconfig <id>: <file> has mode 0644: the mount options of its file system set the mode`. When the write fails after the create, both remove the file that they made while the path still names it and print `cannot write <file>: <cause>` with the free-space hint, or, when the part stays, with `<file> can hold a part of the kubeconfig. Remove it before the next run.` Go prints the text of the system error, the bash the C-locale text of the shell with its first letter in lower case. The bash does not see an error of the close. Every api string and every echoed value loses the characters of one explicit list, which the destroy plan shows as `?` (the jq def `unshown` in `utils/verbose.sh`, Go `ui.Unshown`): C0, DEL, C1, U+2028, U+2029, the bidi controls, and the zero-width and invisible characters of the list. A message or a help that is not a string counts as absent. Neither the api call nor the token grant follows a redirect. The Go request also refuses a URL that is not https (bash: `curl --proto =https`). The bash twin hands the bearer to curl on stdin (`-q -K -`), never on argv. The argsh parse errors exit 2, and the Go binary exits 1 with the same message. The Go `lo mcp` refuses a positional argument that starts with a dash and never offers `chat`. The argsh `lo mcp` cannot run a command three levels deep (#245). `hack/parity-kubehz.sh` diffs every command against an https stub of the api, with the requests and the written kubeconfig. It also runs real tool calls through `lo mcp start` in a Go project and in two routed projects. | `internal/kubehz/agent.go`, `internal/cli/cmd_kubehz_agent.go`, `.lok8s/libs/kubehz/agent`; tests `internal/kubehz/agent_test.go`, `internal/kubehz/agent_link_unix_test.go`, `internal/kubehz/agent_publish_unix_test.go`, `internal/cli/cmd_kubehz_agent_test.go`, `tests/unit/kubehz_agent_test.bats` |
| D40 | Both implementations | **`lo audit` reads a dated Kubernetes support table, in both implementations.** The `k8s-version-support` check had a static list of supported minors (1.34, 1.35, 1.36) that nobody refreshed: it did not know 1.37, and it would have kept 1.34 "supported" after its EOL on 2026-10-27. Both implementations now hold one row per minor with its EOL date (Go `k8sMinorEOL`, bash `_AUDIT_K8S_MINOR_EOL`; `TestK8sMinorEOLMatchesBash` fails when they differ). A minor is supported while its date is in the future, so a minor reaches EOL on its date with no change; a new minor still needs a row. When every row is past its date, the check warns that the table is out of date. `_AUDIT_TODAY` (yyyy-mm-dd) pins the day for tests in both implementations; the bats files pin it in `setup()`. Both print the same bytes on the same day, so `hack/parity-audit.sh` needs no change. | `internal/audit/audit.go` (`k8sMinorEOL`, `k8sToday`), `internal/audit/checks.go` (`checkK8sVersion`), `.lok8s/libs/audit` (`audit::_k8s_today`, `audit::_k8s_supported`); tests `internal/audit/checks_test.go`, `tests/unit/audit_test.bats` |
| D41 | Both implementations | **The register calls try a busy platform again, in both implementations (#312).** `POST /api/clusters/register` (the direct claim, the claim key and the fingerprint announce) answers 503 or 429 with `Retry-After` when the platform cannot count registrations. Both implementations try again after the time in the header: at most 3 tries, one wait at most 60 s (more than six digits read as 999999 s, then the cap), and only the delta-seconds form counts (an HTTP-date reads as no header). Each wait prints `[warn] kubehz: the platform answered HTTP <status> <code>. Try <n> of 3 in <s>s.` After the third refusal the registration stops: no fall back to the next mode, and `[error] kubehz: the platform did not register <domain> (HTTP <status> <code>, 3 tries). Run '<command>' when the platform is available again.`, rc 1. The command is `lo kubehz register --domain <domain>`, or `lo provision --bootstrap --domain <domain>` from the provision (it stops before its bootstrap). The code is `.data.code // .code` and prints only when it has the shape `^[A-Z][A-Z0-9_]{0,63}$`. A 503 without `Retry-After` is not tried again and keeps the old fall back. The bash twin runs curl without `--fail` (`--fail-with-body` needs curl 7.76; RHEL 8 has 7.61): `-w` prints the status after the body, a `-D` dump gives `Retry-After`, and a non-zero curl rc is a transport failure only. A refusal that is not tried again falls back as before, and the fall back warnings of both name it (` (HTTP <status> <code>)`); a transport failure or a Hetzner Cloud refusal names nothing. The bound is per register call: a mode whose last answer is a different refusal falls back, so three modes can wait up to 6 minutes in total (documented, not a shared budget). Ctrl-C ends the wait: the binary waits through the `Sleep` seam and returns the context error, the bash `sleep` dies with the foreground group. On a terminal the binary prints the error as `error:` plus a `next:` line (`ui.ErrorNext`); piped, both print the `[error]` line. `hack/parity-kubehz.sh` diffs the three modes against stub domains that answer 503, 429, a 503 without the header, a `Retry-After` in the HTTP-date form, a `Retry-After: 00` and a code with an escape sequence, with the request logs. | `internal/kubehz/api.go` (`registerPost`, `apiError`, `registerRefusedError`, `retryAfter`; `TestRegisterRetryBoundMatchesBash` pins the bash constants), `internal/kubehz/register.go` (`registerStop`), `internal/kubehz/hooks.go`, `.lok8s/libs/kubehz/main` (`kubehz::register_post`), `.lok8s/libs/provision`; tests `internal/kubehz/register_retry_test.go`, `tests/unit/kubehz_register_test.bats`, `tests/unit/provision_test.bats` |
| D42 | Both implementations | **`lo kubehz node cordon\|uncordon\|drain`, new in both implementations.** One node of a Space (`hosting: shared`), through the api routes `POST /api/spaces/{id}/nodes/{name}/cordon\|uncordon\|drain` with the `KUBEHZ_TOKEN` bearer (`clusters:write`), the auth of the other node commands. A Space gives the tenant no rights on the nodes of the shared control plane, so the platform does the operation; a `hosted` or `self` domain is refused with a `kubectl` hint before any request. The space id is `--space-id` or the row of the domain's slug in `GET /api/spaces` (that list needs the `read` scope; `--space-id` sends no list request). An absent `--name` is a parse error; an empty one reaches the name check in both. Both check the node name (a DNS label), the space id (`^[A-Za-z0-9-]+$`, also the id of the row) and the drain `--timeout` (a whole number from 60 to 3600, leading zeros dropped, never octal) before any request. Cordon and uncordon send no body. Drain sends only the given fields: `{}`, `timeoutSeconds`, and `force` from `--evict-unmanaged` (the name `--force` belongs to the global flag). A drain refuses the global `--force`/`-f` at any place in argv, first and with one line that names `--evict-unmanaged`: bash reads the `force` of main, which argsh sets from the inherited flag, and Go reads the persistent flag. Both print the same bytes: the request line, one note per verb, a line for the `replaced` action of the answer (a drain over a drain keeps the first deadline and the first timeout only for the same machine, so the line says so, and with `--timeout` a second line says that the new value has no effect) and where to read the phase. Each refusal code of the three routes (`NODE_NOT_JOINED`, `NODE_MAINTENANCE_CONFLICT`, `_UNAVAILABLE`, `_AUDIT_UNAVAILABLE`, `_OUTCOME_UNKNOWN`) says whether something changed; a 404 names the node and the space. A gateway answer (502, 504, a 503 with no code), a transport error after the send and a 2xx without `status: requested` print the three lines of `_OUTCOME_UNKNOWN` with the cause in brackets, and the command fails: the api can still act on such a request. Go reads "after the send" from httptrace (`requestSent`, `WroteHeaders`), bash from the curl exit code (`node::curl_sent_nothing`: the URL, name, connect and TLS codes come before the send; a transport error before the send keeps the "unreachable" line). curl's 28 is a timeout of any phase, so bash counts it as after the send, while Go names a connect timeout unreachable. curl `-sS` prints its own error line first; the Go binary does not (the parity case allows that line). `lo kubehz status` of a Space adds `cordoned` and the last request with its phase to each node line, and the reason, clipped to 256 code points (jq) or runes (Go); a newline or a tab in a server string becomes a space, so one node is one line, and then each line loses the characters that a terminal does not show as they are (the unshown set, except a tab and a newline; bash: `kubehz::scrub`, Go: `scrub`). In `lo mcp`, cordon and uncordon are `@idempotent` (the mutating tier: they move no pod), drain is `@destructive`. The two node preflights share their halves in both (`nodeDomainConfig`/`node::domain_config`, `nodeAPIAccess`/`node::api_access`) and one URL id shape (`urlIDRe`/`node::url_id`). The shared half refuses a `KUBEHZ_TOKEN` with a control character (C0 or DEL, often a CR from a file with CRLF line ends) with the two lines of `http::credential_ok`, before any request, for all six node commands. Before, Go named a CR "unreachable" and sent a tab, and bash on `join\|remove\|status` without `--cluster-id` added a second line, that the cluster registry did not answer. An empty `--name` of `node remove` now reaches the name check in Go too, as in argsh. A backslash in the echoed name, space id or slug (the spec's value, which has no shape check) prints as typed in both (`error()` prints the message as it is). The `--cluster/-s` guard of the three commands is dead on dispatch in bash, as for the other node commands (D6). `hack/parity-kubehz.sh` diffs the local refusals, a closed port (a failure before the send) and, against the https stub, every answer and refusal of the three routes with the request logs, and the token refusals with no request. | `internal/kubehz/maintenance.go`, `internal/kubehz/node.go` (`nodeAPIAccess`), `internal/kubehz/shared.go` (`nodeMaintenanceSuffix`), `internal/cli/cmd_kubehz.go` (`newKubehzNodeMaintenance`), `.lok8s/libs/kubehz/node` (`node::cordon`, `node::uncordon`, `node::drain`), `.lok8s/libs/kubehz/shared` (`kubehz::space_status`); tests `internal/kubehz/maintenance_test.go`, `internal/cli/cmd_kubehz_test.go`, `tests/unit/kubehz_node_maintenance_test.bats` |
| D44 | Both implementations | **`lo kubehz node join`: the stop of `kubeadm` and the signal texts.** Both implementations hold INT, TERM and HUP from before the mint until the cleanup has finished: the bash twin with traps in `node::join`, the binary with a `signal.Notify` of its own (`holdJoinSignals`; a SIGHUP that `nohup` ignores stays ignored). The first signal stops the join. A second one does not end the command sooner; SIGKILL is the force path. Both keep the exit status of `kubeadm` through the stop: a 0 is a landed join, which prints its success line, and the command still exits 128+n (the binary: `cmd/lo/main.go`). Three differences stay. (1) The stop signal: the binary stops `kubeadm` through the shared runner (`cmd.Cancel` sends SIGINT, `Cmd.WaitDelay` kills it after `joinStopDelay`, 10 s). The bash twin runs `kubeadm` as a background job, which ignores SIGINT in a non-interactive shell, so the trap handler sends SIGTERM, waits at most 10 s, then sends SIGKILL and reaps it (`node::_join_stop`). The handler stops the recorded child itself, so a signal between the check of the flag and `wait` does not wait for `kubeadm`. (2) A signal to lo alone during the mint request: the binary aborts the request at once, prints `kubehz API unreachable (POST …)` and the note `interrupted during the mint`. The bash twin waits for curl to end: bash runs a trap only after the command substitution ends. When the api answers, the mint completes and the bash twin prints the slot note `interrupted after the ticket was minted`. When the api does not answer, curl ends at its bound, and the bash twin prints the mint note. That wait is at most 150 s. A second signal does not cut it short. A Ctrl-C stops curl at once (the INT goes to the process group), and then both print the mint note. Both twins bound the mint request: 30 s for the connect, 150 s for the whole request. bash uses `curl --connect-timeout 30 --max-time 150` (`kubehz::space_api_mint_max_time`). The binary uses the dialer of its client and `mintMaxTime`. The binary asks `kubelet --version` while it holds the signals and stops it as it stops `kubeadm`. The bash twin asks before its traps are in place: a signal then ends it at once, without a note. (3) A NUL byte in the mint response: inside the JSON value both refuse it (the bash twin maps NUL to `\001` before the capture, `mark-nul`, and jq refuses it). After the end of the first JSON value the binary's decoder ignores it and the bash twin refuses the body. The Space path (`lo kubehz join`) differs too: on a signal during the write of the ticket file, the bash writer removes what it wrote and prints `interrupted after the ticket was minted`; the binary writes the file in one step without a check of the signal, prints its path as usual, and exits 128+n. | `internal/kubehz/node.go` (`holdJoinSignals`, `runJoin`, `joinStopDelay`, `mintTicket`, `mintMaxTime`, `kubeletVersion`), `internal/execx/runner.go` (`Cmd.Exited`), `internal/interrupt/interrupt.go` (`Watch`), `.lok8s/libs/kubehz/node` (`node::_join_traps`, `node::_join_signalled`, `node::_join_stop`), `.lok8s/libs/kubehz/shared` (`kubehz::space_api_call`, `kubehz::space_api_mint_max_time`, `kubehz::_join_write_signalled`); tests `internal/kubehz/node_test.go`, `internal/interrupt/interrupt_test.go`, `tests/unit/kubehz_node_test.bats`, `tests/unit/kubehz_shared_test.bats` |

### Credentials on disk and on the terminal

Hardening of credential files and of what the terminal shows. The
harnesses never reach these paths (they stop at the local refusals, tokens
unset).

| # | Implementations | Deviation | Where |
|---|---|---|---|
| D20 | Go only | **CAPI and KKP kubeconfigs are written 0600.** The bash drivers' `> "${kc}"` redirects left cluster-admin kubeconfigs at the umask default (0644), readable by every local user. The binary writes them owner-only and tightens a file that already exists (`os.Chmod`), as the kind and hosted paths already did. | `internal/driver/capi/capi.go` (`writeKubeconfigFile`), `internal/driver/kkp/api.go` (`getKubeconfig`) |
| D21 | Go only | **`lo kubehz join` (hosting: shared): the join script is private and the ticket is not echoed.** The api-shipped script lands in a fresh `os.MkdirTemp` directory — `<TMPDIR>/kubehz-join-<random>/kubehz-join-<node>.sh`, 0700 over 0600 — so a shared `/tmp` offers no name to pre-plant. The terminal repeats the plaintext ticket only on `--print-token`. When no script came, the ticket alone goes to `<TMPDIR>/kubehz-join-<random>/kubehz-join-<node>.token` (0700 over 0600) and the terminal shows its path; the bash lib does the same, with the same line and the same bytes. A write failure after the mint prints the live-ticket note. Server strings go through `scrub` and the ticket must match the bootstrap-token shape (`[a-z0-9]{6}.[a-z0-9]{16}`, the bash lib spells the characters out) or the mint is refused, in both implementations. The Go-only parts are the script and `--print-token`. | `internal/kubehz/shared.go` (`spaceMintJoin`, `writeJoinFile`), `internal/cli/cmd_kubehz.go` |
| D27 | Both implementations | **Handover bundles: a link entry refuses the whole bundle, in both implementations.** The bash `tar -xzf` extracted the symlink and hardlink entries of a `.tar.gz` bundle. The binary skipped each one with a `[debug]` line. A link in a PKI bundle has no use. It can name a file outside the bundle (`ca.key -> /etc/shadow`), and `preseed` would send that file to the node. Now both implementations read the type of every entry before the unpack writes a byte. A symlink, hardlink, device or FIFO entry refuses the whole bundle before the unpack, with `handover: refusing <bundle>: an archive entry is not a regular file or a directory (a link, a device or a FIFO)`. Both also refuse a key file in a bundle directory that is a link or that has a second name (a hardlink). One difference stays. Bash reads the types from the `tar -tv` listing, so it also refuses an entry whose name holds ` -> ` or ` link to ` (fail closed). After the unpack, bash checks the tree on the file system again. The binary reads the tar headers, so it extracts a regular file with such a name. | `internal/kubehz/handover.go` (`extractBundle`, `validateBundle`), `.lok8s/libs/kubehz/handover` (`handover::resolve_bundle`, `handover::first_odd_entry`, `handover::validate_bundle`) |
| D51 | Both implementations | **The one-time claim ticket goes to stdout only on a terminal outside a CI run, or with `--print-token` on any stdout, in both implementations.** The claim key mode of `lo kubehz register`, and of the register step of `lo provision` and `lo up`, prints the ticket (the MD5 fingerprint of the claim key) in the `curl` line and in the `fingerprint:` line only with `--print-token` (the flag name of `lo kubehz join`, D21), or when stdout is a terminal (bash `[[ -t 1 ]]`, Go `ui.For(out).TTY`) and the run is not a CI run: `LOK8S_NONINTERACTIVE` nonempty (any value, `false` too), or `CI` nonempty and not exactly `false` (`CI=0` and `CI=FALSE` count too), the rule of the provision gate (Go `ui.NonInteractive`, bash `verbose::noninteractive`). Otherwise both print `<hidden: one-time claim ticket>` in the two lines, and the `fingerprint:` line names the Hetzner Console key and `lo kubehz register --domain <domain> --print-token`, which registers again and shows a new ticket. The placeholder is the string that the log filter of the spin-up workflows writes, and the hidden `curl` line is the line that the filter writes; the hidden `fingerprint:` line is not, because the filter keeps the tail of the ticket line. The entry points pass the flag down as an argument (`provision::dispatch`, `kubehz::register_cluster`, `kubehz::ensure_claim_key`), and the bash compares it as a string to `1`: a `print_token` in the environment or in the scope of a caller shows nothing, and `lo recover` passes none. In bash, `kubehz::register_cluster`, `kubehz::deregister_cluster`, `kubehz::ensure_claim_key`, `kubehz::direct_claim`, `kubehz::claim_key_own` and `kubehz::re-enroll` start with the two guard lines of `utils/keysonly.sh` and declare their value names `local +x +i`, so a caller's `set -x`, `set -a`, an exported name of the same name, and a DEBUG, RETURN or ERR trap (`set -T`, `set -E`) get no token, answer, key list or ticket from them; their helpers `kubehz::register_post` and `kubehz::persist_bind_secret` declare the request body, the answer and the bind secret `local +x +i` too. The limits: the guard covers these functions only, not a trace of their callers; `--print-token` puts the ticket on any stdout, a CI log too; a terminal is not a private place (a pty that a program reads, a recording and the scrollback keep the ticket); the Go `lo mcp` never offers `--print-token` (the word `token` marks it as sensitive), but the argsh `lo mcp` offers it, as it offers `--print-only` of `lo kubehz node join`, and gives its command no mark of a tool call: argsh cannot remove one flag from its MCP tools, so an MCP client of the bash server can get the live ticket in its tool result; `lo kubehz re-enroll` prints the code and the message of a refusal as the platform sends them, not sanitized. `hack/parity-kubehz.sh` diffs the claim key run without and with `--print-token` (the stdout of the harness is not a terminal), and checks the output of each implementation for the ticket or the placeholder. | `internal/kubehz/register.go` (`claimTicketShown`), `internal/ui/interactive.go` (`NonInteractive`), `internal/provision/gate.go`, `internal/cli/cmd_kubehz.go` (`printTokenFlag`), `internal/cli/dispatch.go`, `.lok8s/libs/kubehz/main` (`kubehz::ensure_claim_key`), `.lok8s/libs/provision`, `.lok8s/lo`, `.lok8s/utils/verbose.sh` (`verbose::noninteractive`); tests `internal/kubehz/register_ticket_test.go`, `internal/cli/cmd_kubehz_claim_ticket_test.go`, `internal/ui/interactive_test.go`, `internal/testutil/claimticket.go`, `tests/unit/kubehz_register_test.bats`, `hack/parity-kubehz.sh` |
| D22 | Go only | **`lo kubehz claim` reads the nonce from `KUBEHZ_CLAIM_NONCE`.** Both implementations read it from stdin with `--nonce -`, and both place it with a merge patch on kubectl's stdin, never on argv. The environment variable is Go-only: in bash the flag stays required, so the missing-flag refusal is the same in both. These forms keep the claim ticket out of shell history and `/proc/*/cmdline`. | `internal/kubehz/cluster.go` (`ClaimNonce`, `Claim`), `internal/cli/cmd_kubehz.go`, `.lok8s/libs/kubehz/main` (`kubehz::claim`) |

### Process model

How the binary starts and stops children. The bash tree waits for most
children in the foreground. Its traps clean up (they remove temporary
files, or note an interrupted step), or stop a child that it runs in the
background: the dry run's check of the Secret plugin (D26) and the bounded
yq calls. The binary cancels a context.

| # | Implementations | Deviation | Where |
|---|---|---|---|
| D25 | Go only | **Two children bypass `execx.Runner` on purpose.** `lo ai check` runs the chat binary as a raw `exec.Command` with the terminal's stdio: it is a foreground chat, and the terminal's Ctrl-C reaches it directly, the same way it reached the bash child. `lo tilt up` starts Tilt with a raw `exec.Command` in its own session (`setsid`) and releases the process: the bash contract is `nohup tilt up &`, and the child must outlive the command. A Runner child is waited on and cancelled with the command context, which neither case wants. Every other tool call goes through the Runner. | `internal/cli/cmd_ai.go` (`runProcess`), `internal/tilt/tilt.go` (`startDetached`) |
| D29 | Go only | **Routing exists only in the binary.** The bash entrypoint runs what it is. The binary reads `spec.implementation` from `lok8s.yaml` and execs `<project>/<tree>/lo` for a routed command, argv untouched. No environment variable selects the implementation (`LO_IMPL` is gone), and a routing never targets the cache extract or a checkout outside the project. | `internal/cli/routing.go`, `internal/config/implementation.go` |
| D26 | Go only | **A cancelled context sends SIGINT to the running child.** On Ctrl-C the terminal delivers SIGINT to the whole foreground process group in both implementations. On SIGTERM the bash shell died and left the child running, orphaned. The binary cancels the command context on either signal, `cmd.Cancel` sends the child SIGINT, and the parent waits for it: kubeone, terraform and kubectl finish their own cleanup, then `lo` exits 128+n. A caller can bound that wait with `Cmd.WaitDelay`: the dry run's check of the Secret plugin gives the plugin 5 s after the SIGINT (at its 30 s time limit or on a cancel), then kills it. The bash check runs the plugin as a background job, and a background job ignores SIGINT, so bash sends SIGTERM there, then SIGKILL after the same 5 s: at the time limit, and when the check gets INT or TERM. A watchdog times both: it reads lines on a FIFO ("stop", "end") and signals only the check, which sends SIGTERM and SIGKILL to the plugin only while the plugin is still its running job (a TERM to the watchdog could be lost on bash 5.2). lo's bash shell runs the check itself as a background job and waits for it with `wait`, so an INT or TERM to lo's shell alone (not to its process group) also stops the check: the shell sends the check TERM, and the interrupt line names the shell's own signal, as the binary's does (`hack/parity-orchestrate.sh`: Ctrl-C and a TERM to lo in the check). The toolchain checks exist only in the binary. They run a version probe of each tool. `lo toolchain doctor` runs these probes. `lo doctor` runs the same probes in its toolchain section. `lo toolchain install` runs `b --version` for an existing `.bin/b`. A probe gives the tool 2 s after the SIGINT, then kills it. A version query has no cleanup to finish. A broken or foreign `.bin/b` that ignores SIGINT must not hold the command. The doctor's probe gets the SIGINT at its 10 s time limit. The probe in `lo toolchain install` has no time limit. It gets the SIGINT only on a cancel. | `internal/execx/runner.go` (`cmd.Cancel`, `Cmd.WaitDelay`), `internal/addons/dryrun.go` (`checkSecretPlugin`), `internal/toolchain/doctor.go` (`probe`), `internal/cli/cmd_doctor.go` (`doctorToolchain`), `.lok8s/libs/addons` (`addons::dry_run_boundary`, `addons::_check_watchdog`, `addons::_check_stop`) |
| D40 | Both implementations | **The temp KubeOne manifest and a signal.** Both implementations render `kubeone.yaml` into a 0600 temp file (`.kubeone/.kubeone.yaml.*`) and rename it only on success. The bash traps INT, TERM and HUP for the render: it removes the temp file, then lets the signal end the run. The binary removes it in a `defer`: the first SIGINT or SIGTERM cancels the context and the render still returns. The binary does not catch SIGHUP (`signal.Notify` would undo the SIGHUP ignore of `nohup lo …`), and a second signal ends it at once, so in those two cases a temp file can stay. | `internal/driver/kubeone/config.go` (`GenerateConfig`), `.lok8s/drivers/kubeone/config` (`kubeone::_temp_traps`) |
| D45 | Both implementations | **The hosted kubeconfig and a signal.** Both implementations refuse a directory, or a link to one, at `.kubeconfig/<domain>.yaml` before the create, and again before the temp file. At the same two points, both resolve that name in one walk that reads each name once, and refuse a link of another user at that name or in the chain of links that it follows (at most 8 links, each of this user or root), a link in a directory that another user can write into without the sticky bit, a name whose lookup fails for a cause other than "not there" (ENOENT or ENOTDIR: the bash reads the C error text of `ls -ldn`), and a directory of the file that another user can write into: a directory of a user other than this user or root, and, without the sticky bit, a world-writable or a group-writable directory, whatever its group. They also refuse a path that is there but is not a directory, a directory that they cannot read, and a missing directory whose parent is not a directory. Neither resolves the end of the walk again: a link that another user puts at a name that was not there is replaced by the rename onto the exact path, not followed. The directory of the file is the text of the end of the walk before its last `/`, not a cleaned path, so a `..` after a link to a directory names the directory that the kernel uses. The twins differ in two cases. For a path with a `/` at its end, the binary would put the temp file in the directory before the last name (`a/b.*` for `a/b/`), and the bash would put it in `a/b/` (`a/b/.XXXXXX`). Both refuse a directory at `a/b` first. Else both writes fail and change nothing, and no caller gives such a path. For a name that is there and holds a line break, the bash refuses it only when the line of `ls -ldn` shows the line break (the lookup cannot read that line), and the binary accepts it. The command substitution of that line removes a line break at the end of the name, so the bash does not refuse such a name. A later command substitution (the end of the walk, the target of a link) can also remove a line break at the end of a path. A missing `.kubeconfig` directory passes: both make it with mode 0700, and its missing parents too (bash `mkdir -p` under umask 077; the binary `os.MkdirAll` with `0o700`, under the umask), so the second check accepts it. In a directory with the sticky bit that others can write into, the bash writes only with a `mv` that takes `-T`: without it (the BSD mv), it refuses the directory before the create. The binary renames with `os.Rename`, which goes onto the exact path, and accepts that directory. The threat model: a process of the same user is trusted, because it can read the kubeconfig anyway, and the check is for other users. It reads the directory itself, not its parents, and the mode bits, not an ACL. Both write the file and its `<metadata.name>` mirror into a 0600 temp file beside the file (`<file>.*`) and rename it onto the exact path only after a complete download. The bash writes through the descriptor of one exclusive create (noclobber, umask 077), not through the path. A signal during the write takes a different path: the bash sets INT, TERM and HUP traps for the write (before the temp file exists): the trap removes the temp file while it is the file of that descriptor, and the signal ends the run. A signal before the rename keeps the old file. During the rename the bash only records a signal and ends the run with it after the rename: it keeps the new file and removes nothing at the old temp path. Without `mv -T`, a directory can take the place of the file between the check and the rename, and the rename then moves the temp file into it: the bash names the path where the rename may have put the file in an error and fails. It deletes nothing in that directory. In the binary, `interrupt.Watch` (INT, TERM) and `interrupt.CatchHangup` (HUP, for the time of the write) cancel the contexts, but the write and the rename take no context: the binary finishes them (the answer is in memory already), then ends with the signal. So on a signal after the download and before the rename, bash keeps the old file and the binary writes the new one. A signal during the download keeps the old file in both. | `internal/kubehz/hosted.go` (`writePrivateFile`), `.lok8s/libs/kubehz/hosted` (`kubehz::_write_private`) |
| D30 | Both implementations | **`lo provision` (hosting: shared) writes the space's kubectl kubeconfig.** The api serves a kubelogin file for a shared space (`GET /api/spaces/{id}/kubeconfig`): the public endpoint, the shard's sign-in client, the context `kubehz-<slug>`, no credential. The Go provision fetches it once the space is Active and writes `.kubeconfig/<domain>.yaml` (0600, the hosted path's location); `lo kubeconfig` returns that path. An api without the route, a space still wiring up or one with a dedicated plane keep the two older access lines, and nothing fails. Both implementations (`.lok8s/libs/kubehz/shared` `kubehz::space_kubeconfig`, the driver's `kubeconfig`); no parity case diffs the provision output. D30 also names the entry about the project root in [CLI shape](#cli-shape). | `internal/kubehz/shared.go` `spaceKubeconfig`, `internal/driver/kubehz` `Kubeconfig`; tests `TestProvisionSharedWritesTheKubeconfig`, `TestProvisionSharedWithoutAKubeconfigKeepsTheOldLines` |
| D46 | Go only | **A control character in a Robot credential stops a KubeOne provision or destroy before its first step.** A C0 byte (a CR, an LF, a tab, …) or DEL in `HROBOT_USER` or `HROBOT_PASSWORD` gets the two lines of `http::credential_ok` and exit 1. The bash tree checks only in `kubeone::_name_robot_workers`, inside `kubeone::apply`: after the servers exist, after the stale-worker cleanup. `kubeone::reset` has no check, and `driver::destroy` has none. The binary checks at the start of `Provision` (after the `spec.auditLog` check, before the provider), at the start of `Destroy` (no reset, no infrastructure destroy, and the kubeconfig stays), and again in `Apply` and `Reset` for a direct caller. A failed reset only warns, so a destroy that went on would delete the cloud servers and leave the Kubernetes install on the Robot servers, with no control plane to reset them. | `internal/driver/kubeone/kubeone.go` (`Provision`, `Destroy`, `Apply`, `Reset`), `internal/credentials/credentials.go` (`NoControl`), `.lok8s/drivers/kubeone/config` (`kubeone::_name_robot_workers`); tests `internal/driver/kubeone/robotcred_test.go` |
| D47 | Go only | **Each set Robot credential is checked alone.** The bash check runs only when both `HROBOT_USER` and `HROBOT_PASSWORD` are set. The binary checks each set value, because `robotEnv` passes each one on to kubeone alone (`ROBOT_USER` and `HETZNER_ROBOT_USER`, `ROBOT_PASSWORD` and `HETZNER_ROBOT_PASSWORD`). An unset or empty value passes in both. | `internal/driver/kubeone/kubeone.go` (`robotEnv`); tests `TestRobotCredentialWithAControlCharacterIsRefused` (the `other-unset` rows), `TestRobotCredentialValidOrUnsetPasses` |
| D48 | Go only | **`lo chat` checks for `argsh.so` in the `.bin/` directory of the project only.** The `lo` child that the chat binary starts for `lo mcp` gets the `.bin/` directory of the project as PATH_BIN: the binary puts it in place of an exported PATH_BIN (`shimEnv`), and without an export the child derives the same directory. The argsh that the child sources loads `argsh.so` from `${PATH_BIN}/argsh.so` (or from `ARGSH_BUILTIN_PATH`, `PATH_LIB`, `LD_LIBRARY_PATH` or `BASH_LOADABLES_PATH`), never from beside an argsh on PATH. So the check of `lo chat` and `lo ai check` reads `<project>/.bin/argsh.so` and has no PATH fallback. The bash check reads `${PATH_BIN}/argsh.so`, an exported PATH_BIN included, else the `argsh.so` beside the argsh on PATH. Its child inherits that PATH_BIN, so the first part reads the directory of the child. The second part can pass when `lo mcp` cannot load the builtin. A `lo chat` that `lok8s.yaml` routes to bash gets the `.bin/` directory of the project as PATH_BIN too (`shimEnv`). The bash check keeps the old hint, `argsh builtins install`, which installs into a writable exported PATH_BIN, else `PATH_LIB`, else a `BASH_LOADABLES_PATH` directory, else `~/.local/lib/bash`, and the loader does not search `~/.local/lib/bash`. The hint of the binary is `argsh builtins install --force --path '<project>/.bin'`: without `--force`, argsh writes nothing when it can load an `argsh.so` that `ARGSH_BUILTIN_PATH` or `PATH_LIB` names, and the command takes the latest argsh release. With a control character in the directory name, the binary prints the name Go-quoted (`%q`), in the lochat error too, and shows no command. Bash prints the name as it is. Two setups still let the check and the child disagree: a PATH_BIN in `mcp.env` of `lo-chat.json` or of the `LO_CHAT_CONFIG` file, which lochat adds after the inherited environment (`ai/lochat/mcp.go`), and an inherited `ARGSH_BUILTIN_PATH` or `PATH_LIB`, which the loader reads before PATH_BIN. | `internal/cli/cmd_chat.go` (`argshBuiltinPresent`), `internal/cli/shim.go` (`shimEnv`, `projectBin`), `.lok8s/libs/chat` (`main::chat`); tests `TestChatArgshSoCheckReadsTheChildPathBin`, `TestChatArgshSoOnlyInAnExportedPathBinFailsTheCheck`, `TestChatArgshSoBesideAnArgshOnPathFailsTheCheck`, `TestChatArgshSoErrorQuotesADirWithAControlCharacter` |

### Reproduced on purpose (so nobody "fixes" them in one implementation only)

These are behaviours the binary mirrors exactly because the bash has them.
They read like bugs; they are kept so the two implementations stay
diffable. Change them in **both** trees, in one change, with the parity
harness updated.

| # | Behaviour | Where |
|---|---|---|
| Q1 | `yq -r '.path // "alt"'` semantics: the default fires on a missing key, `null` **and a boolean `false`** (jq's `//`). So `.spec.registries.tls // true` reads `true` for `tls: false`; the run header, `lo env`, the kubehz and driver spec readers all inherit it. A bare `yq -r '.path'` prints the literal word `null` for a missing key. | `internal/cli/specyq.go`, `internal/env`, `internal/kubehz/{jsonx,yamlspec}.go`, `internal/driver/*/yamlspec.go`, `internal/tilt`, `internal/addons` |
| Q2 | A mirror with no `name:` reads as `"null"`, which passes the name regex; the failure comes later on the URL check. | `internal/driver/lo/configregistry.go` |
| Q3 | `lo::coredns` runs under a caller's `\|\| return 1`, which disables errexit for the whole body: intermediate `kubectl` failures do not abort, the status is the last command's. `lo::expose` runs in the same context, but it checks each docker step itself: a failed step fails the command in both implementations, and the error names the step. | `internal/driver/lo/services.go`, `expose.go` |
| Q4 | `lo secrets` single-match lookups: zero matches also takes the "more than one" branch and prints an empty `Multiple matches found:` list. | `internal/secrets/readops.go` |
| Q5 | **Fixed in both implementations:** `lo secrets init` against an existing `.sops.yaml` with an empty derivation. `grep -qF ""` matches any non-empty file, so a rerun with a key that is not ed25519 printed `age key already configured` and exited 0. Both now refuse that key before the rerun branch (D53). The row stays for its link. | `internal/secrets/ops.go` (`Init`, `grepQF`) |
| Q6 | `lo lint` label check: yq emits one count per document and aborts at the first non-map `.metadata.labels`; a multi-document manifest whose first document carries the label never warns, an unparsable file always does. `labels` and `secrets` findings are advisory and never change the verdict. | `internal/lint/lint.go` |
| Q7 | `lo audit`: a malformed cilium bootstrap entry is skipped silently, so the cilium check reports "not in spec.bootstrap" (pass). SARIF and JSON reproduce jq's formatting (2-space indent, insertion-order keys, inline empty containers, no HTML escaping, `group_by(.id)` sorting the rules). | `internal/audit/bootstrap.go`, `sarif.go` |
| Q8 | `lo deploy -l k=v`: the subset apply ran as `deploy::_apply … \|\| rc=$?`, suspending errexit — a failing apply is logged and the phase continues; the status is the scoped wait's (always 0). | `internal/deploy/deploy.go` |
| Q9 | Operator `removeFinalizer`: `kubectl … \| jq … \|\| echo '[]'` under pipefail yields `""` when the object has no finalizers field, so the merge patch is malformed and kubectl's rejection becomes the warn line. The CAPI hook substitutes templates with an unrestricted `envsubst`. | `internal/operator/kube.go`, `capi.go` |
| Q10 | `lo kubehz register`: `.data.validation.writable // "unknown"` — only the *string* `"false"` reaches the read-only branch (a JSON `false` is swallowed by `//`). | `internal/kubehz/register.go` |
| Q11 | `lo down` / `lo clean` ignore positionals and unknown flags (`main::down` has no `:args`). `lo init service` and `lo audit` collect extra positionals and ignore all but the first (argsh array parameter). | `internal/cli/cmd_down.go`, `cmd_init.go`, `cmd_audit.go` |
| Q12 | `lo image list` against a dead registry endpoint prints the header only and exits 0 (the `curl \| jq` pipeline). | `hack/parity-loop.sh` |
| Q13 | `lo bootstrap` with nothing to apply prints its debug line twice. `LOK8S_BOOTSTRAP_ONLY` defaults to `0` when unset (defer to the driver), never `1`. | `internal/bootstrap/entries.go`, `engine.go` |
| Q14 | `--no-secrets` has no `=false` off-form: the flag ON wins over `LOK8S_BUILD_NO_SECRETS`, otherwise the env is honoured. | `internal/build/spec.go` |
| Q15 | The registry ConfigMap block is emitted with jq's blank-line separators, byte-for-byte. | `internal/driver/lo/registries.go` |

## Flagged bash defects

Defects found while porting. Which implementation still has each one:

- B2 is in both implementations. The binary reproduces it, and the Go
  source carries a `KNOWN DEFECT, PRESERVED ON PURPOSE` comment at the
  spot. A fix is a user-visible behaviour change that needs its own change
  and coordination with in-cluster consumers. Fix it in both trees
  together, and update the parity harness in the same change.
- B1 is fixed in both implementations. The row stays for its links.
- B3 and B4 are in the bash implementation only. The binary fixes them (D9,
  D6).
- B5 is fixed since #269. The row stays for its links.

| # | Defect | Where |
|---|---|---|
| B1 | **Fixed: remote-expose nginx TLS path mismatch.** The shipped `nginx.conf` referenced `/tls.cert` (with an E), but the copy lands the file at `/tls.crt`. Thus the HTTPS server block of the proxy could not find its certificate, and the reload failed on TLS. Now `nginx.conf` reads `/tls.crt`. A test in each implementation compares the `ssl_certificate` and `ssl_certificate_key` paths of `nginx.conf` with the targets of the two copies. | `internal/driver/lo/expose.go`, `.lok8s/drivers/lo/utils/expose.sh`, `.lok8s/drivers/lo/cluster/expose/nginx.conf` |
| B2 | **Registry ConfigMap port is hardcoded `5000`.** The registries listen on 80/443 (TLS-mode dependent). In-cluster consumers may compensate; correcting it is a coordinated change. | `internal/driver/lo/registries.go` |
| B3 | **`lo kubehz register` / `join` read `LOK8S_SPEC_FILE` before it is set.** `validate_config` reads a variable only `provision::resolve_spec` sets, and `register` calls it after `validate`, so bash prints a spurious `unbound variable` line and the per-kind validation rules are dead on those two verbs (kind reads as `""`). The binary passes the spec path (D9); the kind rules are exercised through `deploy`, where the bash sets the variable itself. | `hack/parity-kubehz.sh`, `.lok8s/libs/kubehz/` |
| B4 | **`lo kubehz node … --cluster` guard is dead on dispatch** in bash (D6). | `hack/parity-kubehz.sh` |
| B5 | **Fixed in #269: `lo status` on a domain without a spec crashed on an unbound variable** in bash. `dispatch_status` ignored `resolve_spec`'s return and died on `LOK8S_SPEC_KIND` under `set -u`, and a traversal-shaped domain (`../evil`) took the same path. Now `dispatch_status` returns 1 when the lookup fails, and `lo status` continues in both implementations (D8). The row stays, so that a link to B5 still works. | `.lok8s/libs/provision` (`provision::dispatch_status`), `hack/parity-orchestrate.sh` |

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

## Legacy: what moved under `.archive/legacy/`

Retired code is moved, never deleted (`.lok8s/` is the frozen reference; a
file retired from *outside* it moves *under* it):

| Path | What |
|---|---|
| `.archive/legacy/install/` | the argsh `lo-up` bootstrap installer (source, build script, `argsh.pin`); the published bundle stays at `docs/public/lo-up` and the `loup-bundle` CI job still rebuilds and diffs it |
| `.archive/legacy/operator/hooks/` | the original bash shell-operator hook bodies; `operator/hooks/*.sh` are now two-line shims that `exec lo operator <hook>` |

The frozen tree follows the rule at the top of the
[deviations catalogue](#deviations-catalogue).
