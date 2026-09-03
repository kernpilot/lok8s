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

### External tools still exec'd

The port swapped shell for Go but did **not** swap the renderers. The
following remain subprocesses, resolved through the project's `.bin/` first
and `PATH` second (`internal/execx.Look`):

| Tool | Used by | Why not in-process |
|---|---|---|
| `kustomize` (+ the `khelm` and `secrets.lok8s.dev` exec plugins under `.kustomize/`) | `lo build`, the Lo driver's kustomize build hook, `lo kustomize` | The rendered `artifacts.yaml` is the product; only the pinned binary guarantees byte-identical output. |
| `yq` | `lo build` split mode (the YAML stream transforms and per-Secret re-renders), `lo env services` (the deep-merge the Tiltfile consumes) | yq's emitter has opinions (`---` on every non-first document, sequence-dash indentation) that `gopkg.in/yaml.v3` does not reproduce byte-for-byte. Field *extraction* from YAML is native everywhere. |
| `sops` | `lo build` split-mode Secret twins | The `.enc` files must stay interoperable with the sops CLI. (`lo secrets` itself uses the sops **library** in binary mode; the files it writes are the same format.) |
| `kubectl`, `kind`, `docker`, `tilt`, `clusterctl`, `kubeone`, `hcloud`, `mkcert`, `envsubst` | drivers, deploy, registries, tilt, trust | These are the tools lok8s orchestrates; calling them as-is keeps "what lok8s runs is what you'd run by hand" true. |

**Renderer-swap roadmap.** Moving kustomize/yq/sops in-process is deferred by
the *renderer-drift rule*: a swap is allowed only once byte-parity with the
pinned tool's output is proven for the committed domains, not just the parity
fixtures (`internal/build/split.go` carries the TODO). The CRD render
(`lo crds`) is the precedent — it is a native `yaml.Node` transform whose
output is byte-identical to the former `yq eval` render, with the committed
CRDs as the parity fixture.

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
| `hack/parity-orchestrate.sh` | `up`, `down`, `clean`, `provision`, `destroy`, `bootstrap`, `status`, `registry` | stub `tilt`/`kind`/`docker`/`kubectl`; consent gates driven with closed stdin |

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
address; they do not change. `make release-check` validates the goreleaser
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
