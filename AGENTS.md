# lok8s — guidance for AI agents & contributors

How to work on the lok8s codebase. Human contributors: start with
[CONTRIBUTING.md](CONTRIBUTING.md).

## Security is paramount

Security applies to every change — features, fixes, refactors, tests.

- **Never guess versions.** Before adding/bumping any dependency (npm/bun, crate,
  pip, Docker image, Go module, Helm chart) confirm the latest stable version
  against its registry. Hallucinated versions waste time and can pull malicious
  packages.
- **Never pipe remote content into a shell.** Download, read, verify, then run.
- **No secrets in code** — no API keys, tokens, passwords, or credentials.
- **Validate external input** (user input, API responses, untrusted files) at
  boundaries; watch for command injection, path traversal, and friends.
- **Plain HTTP is a red flag** — prefer HTTPS; call out any non-TLS use.
- Stop and flag anything suspicious; don't fetch or execute it.

## Project structure

| Area | Language | Path |
|------|----------|------|
| cli | Bash / argsh | `.lok8s/lo`, `.lok8s/libs/` |
| utils | Bash | `.lok8s/utils/` (ip, http, credentials, targets, template, verbose, types) |
| drivers | Bash / argsh | `.lok8s/drivers/{lo,capi,kubeone,kkp}/` — each exposes `main::driver` |
| providers | Bash | `.lok8s/providers/hetzner/` (`main` + `utils/`) |
| kubehz | Bash / YAML | `.lok8s/libs/kubehz/` (main, hosted, manifests/) |
| tilt | Starlark | `.lok8s/tilt/`, `Tiltfile` |
| kubernetes | Bash / YAML | `.lok8s/libs/k8s`, `.lok8s/libs/deploy` |
| infra | YAML / Kustomize | `clusters/`, `.kustomize/` |
| kustomize-plugins | Go | `kustomize/` (source) → `.kustomize/<group>/<version>/<kind>/<Kind>` (built) |
| operator | Bash / YAML | `operator/` (shell-operator hooks + CRDs) |
| e2e | Bash / bats | `tests/e2e/` |
| docs | TS / Markdown | `docs/`, `ARCHITECTURE.md` |
| ci | YAML | `.github/workflows/` |

## Building & testing

```bash
GITHUB_TOKEN=$(gh auth token) ./.bin/b install   # pinned toolchain (argsh, kustomize, yq, …)
./.bin/argsh test tests/unit/ tests/operator/    # bats suites
npm run lint                                     # shellcheck --severity=warning (or: ./.bin/argsh lint)
```

Use conventional commits (`feat:`, `fix:`, `docs:`, `chore:`, …). Keep CI green —
no new shellcheck warnings (fix them, or add a justified `# shellcheck disable=`).
