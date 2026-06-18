# Project — Claude Code Instructions

## Security is Paramount

Security applies to every task — features, fixes, refactors, tests. No shortcuts, unless the user explicitly asks to skip or acknowledges the risk.

- **Supply chain**: When a hook prints `SUPPLY_CHAIN_CHECK`, you MUST perform the verification checks (WebFetch the registry, verify the package). Do not skip.
- **Version numbers**: NEVER guess a version. Before installing or updating any dependency (npm/bun package, crate, pip package, Docker image, Go module, Helm chart), WebFetch the registry to confirm the latest stable version. Hallucinated versions waste time and can pull malicious packages.
- **Remote code**: Never pipe remote content to a shell. Download first, read with Read tool, verify, then execute.
- **OWASP top 10**: No command injection, XSS, SQL injection, path traversal. Check in every implementation.
- **No secrets in code**: No API keys, tokens, passwords, credentials. Check before staging.
- **External input is hostile**: Validate at system boundaries (user input, API responses, untrusted files).
- **HTTP (non-SSL) is a red flag**: Plain HTTP and non-HTTPS protocols require explicit user acknowledgment before use.
- **Suspicious content**: Add to `.claude/.cache/blocked-domains.txt` and notify the user.

## Project Structure

| Sub-project | Language         | Path                                  |
|-------------|------------------|---------------------------------------|
| cli         | Bash / argsh     | `.lok8s/lo`, `.lok8s/libs/`           |
| utils       | Bash             | `.lok8s/utils/` (ip, http, credentials, targets, template, verbose, types) |
| drivers     | Bash / argsh     | `.lok8s/drivers/{lo,capi,kubeone,kkp}/` — all argsh with `main::driver` |
| providers   | Bash             | `.lok8s/providers/hetzner/` (`main` + `utils/`) |
| kubehz      | Bash / YAML      | `.lok8s/libs/kubehz/` (main, hosted, deploy, runner, manifests/) |
| tilt        | Starlark         | `.lok8s/tilt/`, `Tiltfile`            |
| kubernetes  | Bash / YAML      | `.lok8s/libs/k8s`, `deploy`           |
| infra       | YAML / Kustomize | `clusters/`, `overlays/`, `.kustomize/` |
| kustomize-plugins | Go         | `kustomize/` (source) → `.kustomize/<group>/<version>/<kind>/<Kind>` (binaries) |
| e2e         | Bash / bats      | `tests/e2e/` (no-services, single-local-build, cache-mode, remote-lo, remote-ci) |
| docs        | TS / Markdown    | `docs/`, `ARCHITECTURE.md`              |
| ci          | YAML             | `.github/workflows/`                  |

## Hooks System

Active hooks enforce boundaries automatically:

- **PreToolUse** (`scope-enforcement.sh`): Project boundaries, network egress whitelist, supply chain verification, remote code review, HTTP red flags
- **PostToolUse** (`validate-intent.sh`): Syntax checks on edited shell scripts
- **PostToolUse** (`detect-injection.sh`): Prompt injection detection for fetched web content
- **PostToolUse** (`track-learnings.sh`): Tracks touched domains for `/improve`
- **SubagentStart** (`inject-expertise.sh`): Injects domain expertise into spawned agents
- **SessionStart** (`session-context.sh`): Reports domains, activity, blacklist status

## Project Tuning

`tuning.conf` contains project-level behavioral knobs. Settings are printed at session start so all agents share the same expectations. **Always check tuning values before git operations, reviews, or security decisions.**

Key settings:

- `git.gpg-sign` — whether to use `--no-gpg-sign`
- `git.auto-push` — never push unless user explicitly asks (when `no`)
- `git.auto-commit` — commit after each task / before branch switches (when `yes`)
- `git.commit-style` — `conventional` for `feat:`, `fix:`, etc.
- `security.injection-sensitivity` — `strict` | `normal` | `permissive`

## Agent Commands

- `/do <task>` — Single-domain orchestration (plan-build-review-improve cycle)
- `/do-teams <task>` — Multi-domain parallel work with agent teams
- `/improve [domain]` — Update domain expertise from recent work
