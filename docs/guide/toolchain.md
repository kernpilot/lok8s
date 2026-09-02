# The Toolchain (`.bin/b.yaml`)

Every lok8s project carries a pinned toolchain in `.bin/`. The file that
declares it is `.bin/b.yaml`, managed by [`b`](https://github.com/fentas/b),
a binary manager and env-file syncer ([binary.help](https://binary.help)).
lok8s itself is distributed as a `b` environment: `b` installs the `lo` CLI,
the `.lok8s/` framework tree, and every tool the framework calls.

This page documents what `b` does in a lok8s project and the `b.yaml`
fields lok8s actually uses. For the full `b` feature set, see the
[upstream docs](https://binary.help).

::: info Documented against `b` v4.18.x
`b` ignores keys it does not know, so a field name from a different
version fails silently: the binary still installs, just with default
behavior. If something here does not match what you see, check
[binary.help](https://binary.help) for your version.
:::

## How `b` fits the `lo` workflow

`b` owns two things in a lok8s project:

1. **Binaries**: kubectl, kustomize, sops, tilt, kind, and the rest land
   in `.bin/`, pinned per project. Nothing touches your system.
2. **Framework files**: `.lok8s/**`, the Tilt extension, the kustomize
   plugins, and `.bin/b.yaml` itself sync from the upstream lok8s repo.

The commands you run:

```bash
# Join an existing lok8s project: install the exact pinned toolchain
b install

# Start a new project: add a lok8s profile, then install
b env add github.com/kernpilot/lok8s#local
b install

# Pull framework updates + binary upgrades
b update

# CI: exit non-zero when anything is out of date
b version --check

# Verify installed artifacts against b.lock checksums
b verify
```

`b install` reads `.bin/b.yaml` and writes `b.lock` (versions + SHA256),
so teammates and CI get the identical toolchain.
[Getting Started](/guide/#installation) walks through the same commands,
after installing the `lo` release binary; the `core` profile's `b.yaml`
declares that binary too (`github.com/kernpilot/lok8s`, asset
`lo-*.tar.gz`, alias `lo`), so `b install` fetches it into `.bin/`.

`b` picks the install directory from the first of these that is set:
`PATH_BIN`, then `PATH_BASE`, then `<git-root>/.bin`, then `<cwd>/.bin`.
Note that `b` uses `PATH_BIN` and `PATH_BASE` **verbatim**: it does not
append `.bin` to them, it only does that for the git-root and working-
directory fallbacks. The `.envrc` that ships with every profile exports
these for [direnv](https://direnv.net/) users.

::: tip Authentication
Public sources need no token. Set `GITHUB_TOKEN` only for private repos
or to raise GitHub API rate limits.
:::

## The `binaries` section

`binaries` is a map. Each key is either a **pre-packaged name** (`kubectl`,
`jq`, `sops`, …: `b search <name>` lists them) or a **provider ref**
(`github.com/arg-sh/argsh`, `oci://docker`, `go://…`, `git://…`). An empty
value `{}` means "latest, defaults".

Fields lok8s uses, from the real `.bin/b.yaml`:

```yaml
binaries:
  oci://docker: {}                  # docker CLI from an OCI image, daemonless

  renvsubst:
    alias: envsubst                 # install under a different name
    groups: [core]                  # profile tag (see below)

  github.com/arg-sh/argsh:
    asset: argsh                    # pick this release asset by glob
    groups: [core]
    onPost: "${B_BIN} builtin ${B_EVENT}"   # hook after install/update

  github.com/mgoltzsche/khelm:
    file: ../.kustomize/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer
    groups: [kustomize]             # custom install path (relative to b.yaml)
```

| Field | Purpose |
|---|---|
| `version` | Pin a version (tag). Without it, `b` installs the latest and `b update` upgrades. |
| `alias` | Install the binary under a different name on `PATH`. |
| `asset` | Glob that selects one release asset when a release ships several. |
| `file` | Custom install path, relative to the `b.yaml` location. lok8s uses this to place kustomize exec plugins under `.kustomize/`. |
| `onPost` | Shell hook that runs after a successful install or update, only when the binary on disk changed. Gets `B_EVENT` (`install`\|`update`), `B_NAME`, `B_VERSION`, `B_FILE`. |
| `groups` | **Not a `b` field.** A lok8s convention: `b` preserves unknown keys, and the profile `select` expressions below filter on this tag. |

## The `profiles` section

`profiles` is `b`'s env-sync feature: an upstream repo publishes named
file sets, and consumers subscribe with
`b env add github.com/kernpilot/lok8s#<profile>`. lok8s publishes five:

| Profile | Includes | Adds |
|---|---|---|
| `core` | (none) | `.lok8s/**`, `.envrc`, `.gitignore`, `.mcp.json`, skills, and the `core`-tagged binaries |
| `kustomize` | (none) | `.kustomize/**` plugins and their binaries |
| `local` | core + kustomize | `Tiltfile`, `services.yaml`, kind/Tilt/mkcert/bats |
| `capi` | local | `clusterctl`, `hcloud` |
| `kubeone` | local | `kubeone`, `hcloud` |

Each profile entry has a `description`, optional `includes` (compose from
other profiles), and a `files` map of glob patterns to sync. One pattern
deserves a note. The profile syncs a **filtered** `b.yaml`:

```yaml
profiles:
  core:
    files:
      .lok8s/**:
      .bin/b.yaml:
        select:
          - "{binaries: from_items(items(binaries)[?[1].groups && contains([1].groups, 'core')])}"
```

`select` extracts keys from a YAML file instead of syncing it whole; a
[JMESPath](https://jmespath.org/) expression here keeps only the binaries
tagged with the profile's group. The effect: a `core` consumer's
`.bin/b.yaml` lists only the `core` binaries. Each profile ships the
tools it needs and nothing else.

You rarely touch `profiles` as a consumer. `b env add` copies the resolved
profile into your local `b.yaml`, and `b update` keeps it in sync.

## Adding your own tools

Your project's `.bin/b.yaml` is yours after sync. To add a tool:

```bash
b install --add github.com/derailed/k9s     # install + record in b.yaml
b install --fix jq@1.7                      # install + pin the version
```

Or edit `.bin/b.yaml` directly and run `b install`. Commit `b.yaml` and
`b.lock` so the whole team gets the same tool.

## See also

- [Getting Started](/guide/): profiles and the bootstrap path
- [`b` on GitHub](https://github.com/fentas/b) · [binary.help](https://binary.help): the full manual (providers, env-sync strategies, Docker usage)
