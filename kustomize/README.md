# lok8s kustomize plugins

Go-based exec generator plugins for kustomize, used by lok8s and downstream
consumers (kubehz, etc.) to generate Kubernetes resources at build time
without external tooling.

## Plugins

| Group | Version | Kind | Binary | Status |
|-------|---------|------|--------|--------|
| `secrets.lok8s.dev` | `v1` | `Secret` | `cmd/secret` | active |

## Quick start

```bash
# Build all plugins
make build

# Or via lok8s CLI
lo plugins build
```

The build outputs go to `../.kustomize/<group>/<version>/<kind>/<Kind>`,
which is the layout kustomize expects under `KUSTOMIZE_PLUGIN_HOME`.
The lok8s `.envrc` exports `KUSTOMIZE_PLUGIN_HOME=${PATH_BASE}/.kustomize`
automatically.

## Layout

```
kustomize/
├── cmd/                 # one binary per plugin
│   └── secret/          # secrets.lok8s.dev/v1/Secret
├── plugins/             # plugin-specific code
│   └── secret/
│       ├── spec/        # CRD types + UnmarshalYAML
│       ├── generator/   # generator implementations
│       └── plugin.go    # plugin assembly
├── pkg/                 # shared, reusable across plugins
│   ├── plugin/          # Generator interface, Registry, Runner
│   ├── cache/           # $PATH_SECRETS-backed deterministic cache
│   ├── random/          # crypto/rand helpers
│   ├── charset/         # password charset DSL
│   ├── htpasswdfmt/     # bcrypt + htpasswd line formatting
│   ├── kyaml/           # yaml.v3 wrappers with line-aware errors
│   ├── kresource/       # generic k8s resource builder
│   ├── fileio/          # path-traversal-safe file reads
│   └── errs/            # user-facing error formatting
└── internal/version/    # build-time version (ldflags)
```

## Adding a new plugin

To add e.g. `configmap.lok8s.dev/v1/ConfigMap`:

1. Create `cmd/configmap/main.go` (~10 lines, copy from `cmd/secret/main.go`)
2. Create `plugins/configmap/{spec,generator}/` for plugin-specific code
3. Create `plugins/configmap/plugin.go` wiring spec → generators → builder
4. Add a target in `Makefile` for the new binary
5. Reuse everything in `pkg/`

No changes to existing plugins. No shared-package collisions.

## Cache-first determinism

The load-bearing principle for cached generators (`passwd`, `secretRef`,
`htpasswd`): the cache directory `$PATH_SECRETS` is the **source of truth**
for stability. First run generates and stores; subsequent runs read the
cached value. Output is byte-stable across runs.

To regenerate a cached value, delete its file from `$PATH_SECRETS` and
re-run.

## Bash plugin compatibility

The cache file naming is identical to the legacy bash plugin's
`Secret.<name>.<namespace>.<key>` convention. This means:

- An existing `.secrets/` directory written by the bash plugin works
  unchanged with the new Go plugin
- Cross-secret references (`secretRef:`) read bash-stored values via the
  same path convention
- htpasswd uses the same `.username` and `.password` cache file suffixes
  (plus a new `.bcrypt` cache for output stability — see below)

The htpasswd generator additionally caches a `.bcrypt` file containing
the final `username:hash` line so output is byte-stable across runs
despite bcrypt's non-deterministic salt. The bash plugin re-hashed on
every invocation. To rotate, delete `.bcrypt` (or all three files).

## Testing

```bash
make test          # all unit + integration tests
make test-cover    # with coverage report
make lint          # golangci-lint
```

## Build outputs

```
../.kustomize/
└── secrets.lok8s.dev/v1/secret/Secret    # the v1 Secret plugin binary
```

The `BIN_ROOT` is `../.kustomize` (relative to `kustomize/`), which is the
lok8s repo's `.kustomize/` directory at the repo root.
