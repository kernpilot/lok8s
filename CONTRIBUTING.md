# Contributing to lok8s

Thanks for your interest in lok8s! This guide covers the dev setup, tests, and
conventions. By contributing you agree your work is licensed under the
project's [MIT License](LICENSE).

## Project layout

lok8s is a Bash framework (the `lo` CLI) plus a Go kustomize plugin and a
shell-operator. See [ARCHITECTURE.md](ARCHITECTURE.md) for the full map — the
short version:

| Path | What |
|------|------|
| `.lok8s/` | the `lo` CLI, drivers, providers, addons, Tilt extension (Bash/argsh) |
| `kustomize/` | the Go secrets plugin (source) → built into `.kustomize/` |
| `operator/` | the shell-operator-based CRD reconciler |
| `docs/` | the VitePress documentation site |
| `tests/` | bats suites (`unit/`, `operator/`, `e2e/`) |

## Setup

The toolchain is pinned and managed by [`b`](https://github.com/fentas/b)
(see `.bin/b.yaml`):

```bash
# install the pinned tools (argsh, kustomize, yq, …) into .bin/
GITHUB_TOKEN=$(gh auth token) ./.bin/b install
```

## Tests & lint

```bash
# bats unit + operator suites
./.bin/argsh test tests/unit/ tests/operator/

# shellcheck (the exact check CI runs) — needs shellcheck on PATH
npm run lint
# …or via argsh (bundles shellcheck + argsh-lint):
./.bin/argsh lint '.lok8s/**/*.sh'
```

CI (`.github/workflows/ci.yml`) runs shellcheck (`--severity=warning`),
yamllint, the bats suites, and a kind integration smoke test. Keep them green —
no new shellcheck warnings (fix them, or add a justified
`# shellcheck disable=SCxxxx` with a one-line reason).

## Docs

```bash
npm install            # or: npx -y yarn@1.22.22 install
npm run docs:dev       # local VitePress preview
```

## Conventions

- **Conventional commits**: `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`,
  `test:`. Keep each commit focused.
- **Security is paramount** (see [AGENTS.md](AGENTS.md)): never pipe a remote
  script into a shell (download, read, verify, then run); never commit secrets;
  never guess a dependency version (check the registry and pin it); validate
  external input at boundaries.
- Match the style and structure of the surrounding code.

## Pull requests

1. Fork and branch off `main`.
2. Make the change and add/adjust tests.
3. Make sure `./.bin/argsh test …` and the lint pass locally.
4. Open a PR describing the **what** and **why**; link any related issue.

Found a security issue? Please follow [SECURITY.md](SECURITY.md) instead of
opening a public issue.
