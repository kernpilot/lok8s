# Contributing to lok8s

Thanks for your interest in lok8s! This guide covers the dev setup, tests, and
conventions. By contributing you agree your work is licensed under the
project's [MIT License](LICENSE).

## Project layout

lok8s is a Go binary (the `lo` CLI) plus two smaller Go modules (the kustomize
exec plugins and the `lo chat` engine), a shell-operator, and a framework tree
that ships to every project. See [ARCHITECTURE.md](ARCHITECTURE.md) for the
full map — the short version:

| Path | What |
|------|------|
| `cmd/lo`, `internal/` | the `lo` binary — canonical implementation of every command |
| `.lok8s/` | the framework tree every project carries: addons, the Tilt extension, provider plugins, CAPI templates — **and** the frozen argsh implementation (`lo`, `libs/`, `utils/`, `drivers/`) the binary was ported from; `legacy/` holds retired code |
| `kustomize/` | the Go kustomize exec plugins (own module) → built into `.kustomize/` |
| `ai/lochat/` | the `lo chat` engine (own module) |
| `operator/` | the shell-operator deployment: CRDs, deploy manifests, hook shims (`exec lo operator <hook>`) |
| `hack/` | the parity harnesses, the manual Go round-trip, lint and release helpers |
| `install/` | the verified-checksum installer for the release binary |
| `docs/` | the VitePress documentation site |
| `tests/` | bats suites for the frozen tree (`unit/`, `operator/`, `e2e/`) |

Two implementations exist and both matter: Go is where behaviour lands; the
argsh tree is a frozen reference that must keep agreeing with it
(`LO_IMPL=bash lo …` runs it). Read
[docs/reference/go-migration.md](docs/reference/go-migration.md) before
touching either, and [AGENTS.md](AGENTS.md) for the porting pattern.

## Setup

You need Go (the version in `go.mod`) and Docker. The rest of the toolchain
is pinned and managed by [`b`](https://github.com/fentas/b) (see
`.bin/b.yaml`); the bash side — the provider plugins, the bats suites, the
parity harnesses — really runs it, so install it even for Go-only work:

```bash
make build                # → bin/lo
./.bin/b install          # pinned toolchain into .bin/ (argsh, kustomize, yq, sops, kind, tilt, …)
./.bin/argsh builtins install   # argsh.so — bats runtime for `argsh test`
./bin/lo doctor           # what is still missing, with a fix hint each
```

Public sources need no token; set `GITHUB_TOKEN` only for private repos or
to lift GitHub API rate limits. `mise.toml` is an alternative to `b` for the
tools (it does not provide `argsh.so`).

## Tests & lint

```bash
# Go: unit tests (incl. the tree-drift gate), vet, golangci-lint
go test ./... && go vet ./... && make lint

# Parity: the binary vs the argsh implementation, byte-for-byte
make build && bash hack/parity-test.sh     # …and the other nine, see TESTING.md

# bats: the frozen bash tree
./.bin/argsh test tests/unit/ tests/operator/

# shell lint (the exact check CI runs): shellcheck + argsh-lint
npm run lint            # = bash hack/lint-shell.sh
```

**Before running a parity harness or any ad-hoc comparison, unset
`PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS`.** Inherited from
a project shell they redirect both implementations into that live project.
Every harness starts with that `unset` — copy it.

CI (`.github/workflows/ci.yml`) runs:

| Job | What |
|---|---|
| `shellcheck` | `hack/lint-shell.sh` — shellcheck (`.shellcheckrc`) + argsh-lint over `.lok8s/` (including `.lok8s/legacy/`, on purpose), `operator/hooks/`, `docs/.vitepress/`, `hack/`, `install/` |
| `yamllint` | `.lok8s/`, `operator/`, `.github/` |
| `unit-tests` | bats `tests/unit/` + the CRD drift gate (`lo crds check`) |
| `go-tests` | `go build`, `go vet`, `go test ./...`, golangci-lint, then all ten `hack/parity-*.sh` |
| `operator-tests` | `make build` + bats `tests/operator/` (the hook shims exec the binary) |
| `e2e-lo-up` | a real `lo up --ci` on the `single-local-build` fixture (kind + registries + Cilium + `tilt ci`) |
| `loup-bundle` | `docs/public/lo-up` is a byte-exact rebuild of the legacy installer |
| `integration-tests` | CRD install / schema / inventory smoke on kind (push to `main` only) |

`hack/e2e-go-roundtrip.sh` — the one real provision → destroy round-trip of
the Go orchestration — is a **manual** gate, not in CI. Run it before
merging anything under `internal/provision`, `internal/driver/lo` or the
registry lifecycle. Everything else is in [TESTING.md](TESTING.md).

Keep CI green — no new lint findings (Go: fix them; shell: fix them, or add a
justified `# shellcheck disable=SCxxxx` with a one-line reason).

## Docs

```bash
npm install            # or: npx -y yarn@1.22.22 install
npm run docs:dev       # local VitePress preview
npm run docs:build     # the strict build (dead links fail)
```

## Conventions

- **Conventional commits**: `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`,
  `test:`. Keep each commit focused.
- **Go first; bash wins on parity.** New behaviour lands in `internal/`.
  Touch `.lok8s/**` only when a parity harness would otherwise go red, in the
  same change. A deliberate divergence gets a comment at the spot and a row
  in the [deviations catalogue](docs/reference/go-migration.md#deviations-catalogue).
- **Move, never delete.** Retired code goes under `.lok8s/legacy/`.
- **Security is paramount** (see [AGENTS.md](AGENTS.md)): never pipe a remote
  script into a shell (download, read, verify, then run); never commit secrets;
  never guess a dependency version (check the registry and pin it); validate
  external input at boundaries.
- Match the style and structure of the surrounding code.

## Pull requests

1. Fork and branch off `main`.
2. Make the change and add/adjust tests — a Go test through the `execx.Runner`
   seam, and a parity-harness case when user-visible output changes.
3. Make sure `go test ./...`, the relevant parity harness, the bats suites and
   the lint pass locally.
4. Open a PR describing the **what** and **why**; link any related issue.

Found a security issue? Please follow [SECURITY.md](SECURITY.md) instead of
opening a public issue.
