# The Go `lo` binary

`lo` is moving from an [argsh](https://github.com/arg-sh/argsh) script tree to
a single static Go binary. During the migration both implementations ship,
and the Go binary is the entrypoint: it runs the commands that are ported and
passes everything else through to the bash tree, so a project behaves the
same whichever command you call.

This page is the map: what runs natively, what still runs in bash, how to
force one or the other, and the gates that keep the two in agreement.

## Install

The binary is a GitHub release asset per platform (linux/darwin × amd64/arm64),
with a `checksums.txt` for every asset. See [Getting Started](/guide/#installation)
for the download-verify-run steps, or use `install/lo-install.sh` from the
same release (it verifies the archive against `checksums.txt` before
extracting; `--dry-run` shows the plan).

Local builds stamp `.lok8s/VERSION`; release builds stamp the git tag
(`lo --version`).

## What runs where

```text
$ lo <command>
   │
   ├─ ported? ──yes──▶ Go implementation (internal/cli/cmd_<command>.go)
   │
   └─ no ───────────▶ exec bash .lok8s/lo <command> …   (verbatim argv)
```

Ported commands (native Go) today: `addons`, `ai`, `audit`, `build`, `chat`,
`crds`, `deploy`, `doctor`, `drivers`, `env`, `gitops`, `hooks`, `image`,
`init`, `k8s`, `kubeconfig`, `kustomize`, `lint`, `recover`, `secrets`,
`tilt`, `trust`, `use`, `version`.

Everything else — notably the cluster lifecycle (`up`, `down`, `provision`,
`destroy`, `status`, …) — is a **passthrough**: the binary replaces itself
with `bash .lok8s/lo <args>` after preparing the environment the way the
project's `.envrc` would (toolchain and framework directories on `PATH`,
`KUSTOMIZE_PLUGIN_HOME` defaulted). The passthrough parses nothing; argv
reaches the argsh implementation untouched, so flags and error messages are
identical.

The list moves as ports land; `lo --help` marks nothing yet, so the
authoritative list is the `registerPorted(...)` calls under `internal/cli/`.

## The framework tree still ships

Because of the passthrough, a project still carries `.lok8s/` (the bash
tree, the drivers, the addons) and the pinned toolchain in `.bin/` — exactly
what `b env add github.com/kernpilot/lok8s#<profile> && b install` has always
put there. The Go binary needs it for unported commands and reads
`.lok8s/VERSION` from it. What changes is only the entrypoint: `lo` on your
`PATH` is now the binary instead of `.lok8s/lo`.

The `core` profile's `.bin/b.yaml` declares the binary
(`github.com/kernpilot/lok8s`, asset `lo-*.tar.gz`, alias `lo`), so `b
install` fetches it into `.bin/` alongside the rest of the toolchain.

## `LO_IMPL=bash` — the escape hatch

```sh
LO_IMPL=bash lo build          # this one call: bash implementation, Go skipped
export LO_IMPL=bash            # this shell: every command via .lok8s/lo
```

`LO_IMPL=bash` bypasses the Go implementation entirely, for every command,
ported or not. Use it when a ported command misbehaves: if the bash side is
right and the Go side is wrong, that is a parity bug — please report it with
the command line and both outputs.

There is no `LO_IMPL=go`; the binary is the default.

## Parity gates

A port is not "done" when it compiles. Each one is held to the bash
implementation by differential tests under `hack/`: every covered invocation
runs **both** implementations (the binary, and the same binary with
`LO_IMPL=bash`) against a synthetic project and diffs stdout, stderr, and
exit codes byte-for-byte — and, where a command writes files, the resulting
trees.

| Gate | Surface |
|---|---|
| `hack/parity-test.sh` | the ported command set, general |
| `hack/parity-build.sh` | `lo build` (artifacts, split output, secret twins) |
| `hack/parity-configure.sh` | `lint`, `kubeconfig`, `doctor` |
| `hack/parity-audit.sh` | `lo audit` (human, `--json`, `--sarif`) |
| `hack/parity-loop.sh` | `tilt`, `image`, `env`, `hooks` (read-only paths) |
| `hack/parity-leaves.sh` | `init`, `crds`, `addons`, `drivers`, `chat`, `ai` |
| `hack/parity-ops.sh` | `deploy`, `recover`, `gitops` (cluster-free paths) |

All of them run in CI on every push (`go-tests` job), after `go build`,
`go vet`, `go test` and golangci-lint. Deliberate divergences are listed per
gate in an allow-list (for example `lo version` no longer reporting a bash
version) — anything else that differs fails the build. `go test` also carries
a tree-drift gate: the cobra command tree must match the usage list in
`.lok8s/lo` while both implementations exist.

## Release artifacts

Every tag publishes, via goreleaser (`.goreleaser.yaml`):

| Asset | What |
|---|---|
| `lo-<os>-<arch>.tar.gz` | the `lo` binary (+ LICENSE) |
| `kustomize-secret-<os>-<arch>` | the `secrets.lok8s.dev/v1/Secret` kustomize exec plugin |
| `lochat-<os>-<arch>` | the `lo chat` engine |
| `lok8s-<tag>.tar.gz` | the framework tree (`.lok8s/`, operator CRDs + deploy, `.kustomize/`) |
| `lo-install.sh` | the installer, so it can be verified like everything else |
| `checksums.txt` | SHA-256 of all of the above |

The `kustomize-secret-*` names are a contract other projects' `b.yaml` files
address; they do not change.

## Legacy: the argsh installer

The previous bootstrap path (`lo-up`, a self-contained argsh script) is
retired, not removed. Its source and build live under
`.lok8s/legacy/install/`, and the published bundle stays online for existing
users. New projects take the binary path above.
