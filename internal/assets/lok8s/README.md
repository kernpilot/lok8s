# `.lok8s/`: the frozen bash implementation of `lo`

`lo` is a Go binary (`cmd/lo`, `internal/`). This directory is the bash
implementation it was ported from, written in [argsh](https://github.com/arg-sh/argsh).
It is frozen. It gets bug fixes only, no new features. It is complete and it
still runs. Go is canonical. Every change lands in Go first.

## Why it is still here

- **Reference.** For every ported behaviour, the bash file is the spec: the
  exact strings, the exit codes, the order of output. On a divergence, bash
  wins. A deliberate deviation gets a comment at the spot and a row in the
  [deviations catalogue](../docs/reference/go-migration.md).
- **Parity oracle.** Ten harnesses under `hack/parity-*.sh` run both
  implementations and diff their output byte for byte. They run in CI for
  every PR, against the core and the full build of `lo`.
- **Second variant.** `LO_IMPL=bash lo …` runs this tree instead of the Go
  code, with the same arguments. Some pieces run from here even under Go: the
  Hetzner provider and the kubeone inventory hooks run as argsh children of
  the Go dispatch (see `providers/README.md`).

## Layout

| path | what it is | Go counterpart |
|---|---|---|
| `lo` | the entrypoint: usage array, dispatch, imports | `internal/cli` |
| `libs/` | one file per command or subsystem (`build`, `deploy`, `secrets`, `bootstrap`, `kubehz/`, …) | `internal/<name>` |
| `utils/` | shared helpers (`domain`, `spec`, `kapply`, `oidc`, `provider`, `template`, `types`, …) | `internal/{config,domain,kapply,oidc,yqsem,…}` |
| `drivers/` | the cluster drivers (`lo`, `kubeone`, `capi`, `kkp`, `kubehz`), each with `main` and its cluster manifests | `internal/driver/<name>` |
| `providers/` | infrastructure providers, `hetzner/` is the live one | `internal/provider/bridge` runs it |
| `addons/` | the addon catalogue, kustomize-buildable directories | embedded in `internal/assets` |
| `tilt/` | the Tilt extension a project's Tiltfile loads | embedded, ejected by `lo tilt up` |
| `chat/` | defaults for `lo chat` | embedded |
| `VERSION` | the framework version | `assets.Version()` |

Retired code does not stay here. It moves to [`../.archive/`](../.archive/README.md).

## Two rules

1. **Data is mirrored, edit both sides.** `addons/`, `drivers/*/cluster/`,
   `libs/inventory/manifests/`, `chat/`, `tilt/` and `VERSION` have a
   canonical copy under `internal/assets/lok8s/`. The binary embeds that copy.
   A drift test fails when the two differ. Edit the embedded copy, then run
   `hack/sync-legacy-assets.sh`. Never edit only one side.
2. **Move, never delete.** A file that leaves this tree or the repository
   root moves under `.archive/` with `git mv`. Deleting is an owner decision.

## How to run it

```bash
LO_IMPL=bash lo version          # the same argv, the bash implementation
LO_IMPL=bash lo build            # any command
bash hack/parity.sh bin/lo       # all ten harnesses, both implementations
```

The tree needs argsh with its builtin, `yq`, `jq`, `envsubst`, `sops` and
`ssh-to-age` on the path. `lo init toolchain` writes a `.bin/b.yaml` that carries those lines
as comments. Uncomment them and run `b install`. `lo doctor`
reports what is missing. The Go binary needs none of them.

## Conventions

- Every import carries the `^` prefix: `import ^libs/deploy`,
  `import ^utils/domain`. The prefix resolves against `PATH_SCRIPTS`. A
  file that calls a shared util's helpers imports that util itself. Do not
  rely on `lo` to import it first. `tests/unit/import_convention_test.bats`
  enforces the prefix and the missing-import rule.
- ShellCheck and argsh-lint check every shell file
  (`bash hack/lint-shell.sh`).
- Touch this tree only when a parity harness would otherwise go red, in the
  same change as the Go side.
