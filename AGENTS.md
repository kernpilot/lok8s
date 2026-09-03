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

## Two implementations

`lo` is a Go binary (`cmd/lo`, `internal/**`). **Go is canonical**: every
command runs natively and every change lands there first. The argsh tree
under `.lok8s/` is the **frozen reference** the binary was ported from —
bugfix-only, never deleted, still runnable in full via `LO_IMPL=bash lo …`
(the binary execs `bash .lok8s/lo` with argv untouched). The two are held
together by ten differential parity harnesses (`hack/parity-*.sh`) and a
tree-drift `go test`; **they must stay green**. Full map, seams and the
deviations catalogue: [docs/reference/go-migration.md](docs/reference/go-migration.md).

Three seams still run bash from the frozen tree on purpose: provider plugins
(`.lok8s/providers/<name>/main`, via `internal/provider/bridge`), `lo drivers
<name>` for a driver without a Go twin, and `LO_IMPL=bash`. The kustomize
render is in-process (`internal/render`: the pinned kustomize API plus the
Secret and khelm generators served by the binary itself — byte-parity proven
on the committed kubehz.dev domain; `LO_RENDER=exec` restores the
subprocess pipeline). `yq` and `sops` stay subprocesses until the same proof
exists for them.

How to change or port behaviour (mirror the pattern of any `internal/`
package):

1. **Read the bash first.** `.lok8s/libs/<name>` (or the driver/util) is the
   spec: exact strings, exit paths, ordering. "Bash wins" on any divergence;
   a deliberate deviation gets a comment at the spot *and* a row in the
   catalogue.
2. **Hermetic tests via `execx.Runner`.** Every external tool call goes
   through the `Runner` seam; tests install a fake that records the
   `execx.Cmd` and answers scripted output. The recorded argv is the
   assertion. Nothing under `go test` reaches docker/kind/tilt/kubectl/network.
3. **Parity script.** Add the invocation(s) to the matching
   `hack/parity-*.sh` so CI diffs both implementations (stub binaries in the
   synthetic `.bin` for cluster-touching verbs; closed stdin for consent gates).
4. **Mutation-check.** Revert the change, watch the test and the harness go
   red, then restore. Verify artifacts (files, argv logs), not exit codes.
5. Touch `.lok8s/**` only when a harness would otherwise go red, in the same
   change. A file retired from outside `.lok8s/` moves under
   `.lok8s/legacy/` — **move, never delete**.

Rules that came from incidents:

- **Never run a destructive `lo`/`kind`/`docker`/`tilt` verb in a test or
  harness against ambient state.** Dev machines carry live kind clusters,
  Tilt sessions and registry containers. Stub the tool, or follow
  `hack/e2e-go-roundtrip.sh`'s naming + snapshot discipline.
- **Unset `PATH_BASE PATH_BIN PATH_LOK8S PATH_CLUSTERS PATH_SECRETS`** before
  any harness or ad-hoc comparison (the preamble every `hack/parity-*.sh`
  starts with). Inherited, they redirect both implementations' writes into
  the live project.
- **Prose goes through a file, not a shell argument.** Backticks in a
  double-quoted `gh --body` / `git -m` string are command-substituted; use
  `--body-file` / `-F`.
- **Move, never delete** (above).

## Project structure

| Area | Go (canonical) | Frozen bash reference |
|------|----------------|-----------------------|
| cli | `cmd/lo`, `internal/cli/` (cobra tree, one `cmd_<name>.go` per command, `shim.go`, `dispatch.go`) | `.lok8s/lo`, `.lok8s/libs/` (argsh) |
| utils | `internal/{config,domain,execx,ui,kapply,oidc,env,hooks}` | `.lok8s/utils/` (ip, http, credentials, targets, template, verbose, types, kapply, oidc, spec, domain) |
| drivers | `internal/driver/{lo,capi,kubeone,kkp,kubehz}` (registry in `internal/driver`, linked from `internal/cli/drivers.go`) | `.lok8s/drivers/{lo,capi,kubeone,kkp}/` — each exposes `main::driver` |
| providers | `internal/provider/bridge` (runs the bash plugins as `bash -c` children) | `.lok8s/providers/hetzner/` (`main` + `utils/`) — **still the live implementation** |
| provisioning | `internal/provision` (dispatch, gates, spec), `internal/bootstrap` (the addon DAG), `internal/inventory`, `internal/recover` | `.lok8s/libs/{provision,bootstrap,inventory,recover}` |
| build / deploy | `internal/build`, `internal/deploy`, `internal/image`, `internal/gitops` | `.lok8s/libs/{build,deploy,image,gitops}` |
| render | `internal/render` — `kustomize build` in-process (sigs.k8s.io/kustomize/api, pinned to the binary's release), the self-exec plugin home, the plugin dispatch (`Secret` → `kustomize/plugins/secret` imported; `ChartRenderer` → khelm v2.8.0 as a library), `LO_RENDER=exec` | the pinned `kustomize` + `.kustomize/` exec plugins (what `LO_IMPL=bash` and `LO_RENDER=exec` run) |
| kubehz | `internal/kubehz`, `internal/driver/kubehz` | `.lok8s/libs/kubehz/` (main, hosted, manifests/) |
| secrets / lint / audit | `internal/secrets`, `internal/lint`, `internal/audit` | `.lok8s/libs/{secrets,lint,audit}` |
| scaffolding | `internal/scaffold` (+ `templates/`, `project.go` for `lo init project`), `internal/crds`, `internal/addons` | `.lok8s/libs/{init,crds,addons}` |
| assets (eject model) | `internal/assets` — the embedded mirror `internal/assets/lok8s/**` (**canonical**: addons, `drivers/*/cluster`, the inventory CRD mirror, `chat/`, `VERSION`), `Resolve`/`Peek`, eject + `.lo-origin`, the three-way diff, `update`; `internal/cli/cmd_assets.go` | `.lok8s/{addons,drivers/*/cluster,libs/inventory/manifests,chat,VERSION}` — the synced twin (`hack/sync-legacy-assets.sh`; drift-gated by `go test ./internal/assets/`). Edit the mirror, then sync — never only one side |
| tilt | `internal/tilt` (`lo tilt`, port slots) | `.lok8s/tilt/Tiltfile` (Starlark — still the live extension), `Tiltfile` |
| mcp | `internal/cli/cmd_mcp.go` (ophis) | the argsh `mcp` builtin (`.mcp.json` still points here) |
| operator | `internal/operator` (hook bodies), `operator/hooks/*.sh` (two-line shims), `operator/crds`, `operator/deploy` | `.lok8s/legacy/operator/hooks/` |
| installer | `install/lo-install.sh`, `.goreleaser.yaml`, `hack/release-tarball.sh` | `.lok8s/legacy/install/` (`lo-up`) |
| addons | `internal/assets/lok8s/addons/` (embedded, kustomize-buildable dirs; ejected into a project's `.lok8s/addons/<name>` on first use) | `.lok8s/addons/` (synced twin) |
| infra | `clusters/`, `.kustomize/` (YAML / Kustomize) | |
| kustomize-plugins | `kustomize/` (own Go module, ALSO imported by the root module via the `replace` in go.mod — the binary serves it in-process) → `.kustomize/<group>/<version>/<kind>/<Kind>` (built standalone for the bash path + releases) | |
| lo chat engine | `ai/lochat/` (own Go module) | |
| tests | `internal/**/*_test.go`, `hack/parity-*.sh`, `hack/e2e-go-roundtrip.sh` | `tests/unit/`, `tests/operator/`, `tests/e2e/` (bats) |
| docs | `docs/` (VitePress), `ARCHITECTURE.md`, `TESTING.md` | |
| ci | `.github/workflows/` | |

### Imports (frozen tree)

Every argsh import carries the `^` prefix — `import ^libs/deploy`,
`import ^utils/domain`. The prefix resolves against `PATH_SCRIPTS`; a bare path
resolves against the importing file, and the two differ as soon as something is
sourced from outside the `lo` entrypoint. A driver's `main` must import all of
its own siblings (see `.lok8s/drivers/README.md`) — do not rely on `lo`
pre-importing them. The same holds for the shared utils: a lib that calls
`domain::…` or `spec::…` imports `^utils/domain` / `^utils/spec` itself, even
though `lo` already pulled them in. `tests/unit/import_convention_test.bats`
fails on a non-prefixed import and on a missing one.

The TypeScript under `internal/scaffold/templates/test/` (and its frozen twin
`.lok8s/libs/init.d/test/`) is Playwright scaffolding for a user's project,
not framework code. Its imports are ESM and stay relative.

## Building & testing

```bash
make build                                       # bin/lo (stamps internal/assets/lok8s/VERSION)
go test ./... && go vet ./... && make lint       # Go unit + tree-drift + assets-drift gates, vet, golangci-lint
bash hack/sync-legacy-assets.sh                  # after editing internal/assets/lok8s/**: resync the .lok8s twin
bash hack/parity-test.sh                         # one parity harness (ten exist; see TESTING.md)
./.bin/b install                                 # pinned toolchain (argsh, kustomize, yq, …) — the bash side needs it
./.bin/argsh test tests/unit/ tests/operator/    # bats suites for the frozen tree
npm run lint                                     # shellcheck + argsh-lint via hack/lint-shell.sh (covers .lok8s/legacy too)
```

The full matrix — including the manual `hack/e2e-go-roundtrip.sh` gate —
is in [TESTING.md](TESTING.md).

Use conventional commits (`feat:`, `fix:`, `docs:`, `chore:`, …). Keep CI green —
no new lint findings (Go: fix them; shell: fix them, or add a justified
`# shellcheck disable=`).
