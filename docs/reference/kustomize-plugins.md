# Kustomize Plugins

lok8s ships Go-based kustomize exec generator plugins for common operations
that would otherwise need external tooling. The Go source lives under
[`kustomize/`](https://github.com/kernpilot/lok8s/tree/main/kustomize) at the repo root and builds to
`.kustomize/<group>/<version>/<kind>/<Kind>` — the layout kustomize expects
under `KUSTOMIZE_PLUGIN_HOME`.

## Building

```bash
lo kustomize build   # compile all plugin binaries
lo kustomize test    # run the Go unit tests
lo kustomize clean   # remove built binaries
lo kustomize list    # list discoverable plugins
```

`lo kustomize build` compiles the **framework** plugins from lok8s's own
`kustomize/` source and installs them into the **current project's**
`KUSTOMIZE_PLUGIN_HOME` (`${PATH_BASE}/.kustomize`). So a fresh project gets
the secrets generator without carrying any Go source of its own; if a project
*does* ship a `kustomize/` dir with custom plugins, those are built too. The
lok8s `.envrc` exports `KUSTOMIZE_PLUGIN_HOME=${PATH_BASE}/.kustomize`
automatically — no manual configuration after `direnv allow`.

The build picks a real `go` from goenv or mise (not the bare PATH shim, which
on some dev boxes is an unset/stale wrapper). Install one with `b install go`
or your version manager.

## Secrets Generator

**Plugin:** `secrets.lok8s.dev/v1/Secret`
**Source:** [`kustomize/cmd/secret/`](https://github.com/kernpilot/lok8s/tree/main/kustomize/cmd/secret) +
[`kustomize/plugins/secret/`](https://github.com/kernpilot/lok8s/tree/main/kustomize/plugins/secret)
**Binary:** `.kustomize/secrets.lok8s.dev/v1/secret/Secret`

Generates Kubernetes Secret resources from a structured YAML CRD with
seven generator types. The cache directory `$PATH_SECRETS` is the source
of truth for stable output across runs.

### Quick example

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
generators:
  - secret.yaml
```

```yaml
# secret.yaml
apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: ut-user
  namespace: default
type: Opaque

literals:
  DATABASE_USERNAME: ut_user
  VAPID_PUBLIC_KEY: BPAMuQnvhvnzZ...

passwd:
  NUXT_SESSION_PASSWORD: 128            # length only
  REDIS_PASSWORD:
    length: 32
    chars: alphanum+symbols
  IDP_USER_PASSWORD:                    # guarantee every character class
    length: 48
    chars: alphanum+symbols
    require: [upper, lower, digit, symbol]

env:
  GOOGLE_KEY: AUTHENTIK_UT_GOOGLE_CONSUMER_KEY    # explicit env var
  GOOGLE_SECRET: ~                                # null: use the key as var name
  HOT_VAR:
    var: SOME_VAR
    update: true                                  # re-read env on every run

secretRef:
  DB_PASSWORD: db-secret/password                 # shorthand: secret/key
  ALT_PASSWORD:
    secret: db-secret
    namespace: other-ns
    key: password

htpasswd:
  smtp.htpasswd:
    username: {length: 16}                        # generate
    password: {length: 32}                        # generate

file:
  ca.crt: ./certs/ca.crt                          # raw, base64-encode at emit
  tls.crt:
    path: ./certs/tls.crt
    mode: passthrough                             # already base64

b64:
  legacy_token: dGVzdC10b2tlbi1mcm9tLXNvbWV3aGVyZQ==

bash:
  RSA_KEY:
    exec: openssl genrsa 4096                       # run a command, cache output
    newline: ensure                                 # PEM needs a trailing newline
  SEED_KUBECONFIG:
    exec: kubectl config view --minify --flatten    # read live cluster state
    update: true                                    # bypass cache: re-run EVERY build
  BUILD_SHA: "git rev-parse HEAD"                   # string shorthand → exec
```

`update: true` makes a `bash:` entry **regenerate on every build** instead of the
default run-once-then-cache. Use it for values bound to **live cluster state** that
go stale when the cluster is recreated — e.g. an in-cluster kubeconfig embedding
the current cluster CA + client cert (a fresh `lo up` mints new ones, so a cached
copy authenticates against the old cluster). It does not change the entry's hash,
so it needs no re-`lo secrets allow`.

### Generators

| Generator | Behavior | Cache | Notes |
|-----------|----------|-------|-------|
| `literals:` | Plain key/value map | No | Verbatim, base64-encoded at emit |
| `passwd:` | Random password from charset | Yes | Cache-first; delete the cache file to rotate |
| `env:` | Read from env var | Yes (unless `update: true`) | Falls back to key as var name when value is null |
| `secretRef:` | Read from another Secret's cache file | Reads cross-secret | Shorthand `"secret/key"` or `"secret/ns/key"`; no path traversal |
| `htpasswd:` | Bcrypt-hashed username:password line | Yes (3 files: `.username`, `.password`, `.bcrypt`) | Username generator starts with a letter; cost factor 10 |
| `file:` | Read local file | No | 1 MiB max; path traversal rejected; `mode: raw` (default) or `passthrough` |
| `b64:` | Pre-base64-encoded passthrough | No | Validates the input is valid base64 |
| `bash:` | Run a shell command, use its output | Yes | Each command is SHA256-pinned in a committed `.sha` file; on change the build fails until re-approved via `lo secrets allow` |

> **How a value reaches your pod:** a generator emits raw bytes → kustomize
> emits `data.<key> = base64(bytes)` → Kubernetes decodes on mount, so a
> **mounted secret file contains exactly the generated bytes** (env vars get the
> same decoded value). Prefer mounting secrets as files over env vars.

### Password charsets (`passwd`)

`chars` selects the alphabet for `passwd:` (default `alphanum`):

| `chars` | Alphabet | Bits/char |
|---------|----------|-----------|
| `alphanum` | `A–Z a–z 0–9` | ~5.95 |
| `alphanum+symbols` | adds punctuation | ~6.5 |
| `hex` | `0–9 a–f` | 4 |
| `base64url` | `A–Z a–z 0–9 - _` | 6 |
| `custom:<chars>` | exactly the characters you list | varies |

#### Required character classes (`require`)

`require` lists classes the generated password **must** contain at least one
of — `upper`, `lower`, `digit`, `symbol`:

```yaml
passwd:
  IDP_USER_PASSWORD:
    length: 48
    chars: alphanum+symbols
    require: [upper, lower, digit, symbol]
```

Use it when a downstream policy (e.g. an identity provider's password
complexity rules) demands all four classes. A plain uniform draw can omit one
by chance — and because the value is **cached** (the cache is the source of
truth, never re-rolled), a single non-compliant draw would be a *permanent*
reject. `require` guarantees the classes are present at generation time, so
what `lo secrets print` shows is the exact, policy-valid password.

The charset must be able to supply every required class (`require: [symbol]`
needs `chars: alphanum+symbols`, not the default `alphanum`) and `length` must
be ≥ the number of required classes — otherwise the build fails with a clear
config error rather than a bad secret.

### Running commands (`bash:`)

`bash:` runs a shell command (`exec:`) or script (`file:`) at build time and
caches the output like `passwd`:

```yaml
bash:
  KEY:
    exec: openssl genrsa 4096      # inline command (bash -c)
    output: stdout                 # stdout (default) | stderr | combined
    newline: strip                 # strip (default) | keep | ensure
    encode: ""                     # "" (raw) | base64 | hex
  INFO: "git rev-parse HEAD"       # string shorthand → exec
```

`newline` acts on the trailing **line terminator only** (`\r`/`\n`) — `strip`
(default) removes it, `ensure` normalizes to exactly one `\n`, `keep` is
byte-exact. It does **not** trim spaces/tabs, which are value bytes.

Processing order is **encode, then newline**, so `encode: base64`/`hex` captures
the exact command bytes (the line-terminator cleanup then runs on the encoded
text) — binary key material like `openssl rand 32` + `encode: base64` is
preserved. For raw binary with *no* encoding, use `newline: keep` to be
byte-exact.

**Approval gate.** Because `bash:` executes arbitrary shell, each command is
SHA-256-pinned into a committed `Secret.<name>.<ns>.<key>.sha`, and a local,
**un-committed** `.bash-allow` must approve the current set — the "direnv allow"
moment. After cloning, or whenever a `bash:` command changes, run:

```bash
lo secrets allow
```

Until then the build refuses to execute the `bash:` entries.

### Generating cryptographic keys

For a random **symmetric key**, prefer `passwd` with an explicit charset — it's
the trusted, shell-free generator (no `bash` approval gate):

```yaml
passwd:
  # 256-bit AES key as 64 hex chars; decode hex in your app → 32 bytes
  AES_KEY: { length: 64, chars: hex }
```

Mind the entropy: the default `alphanum` charset is ~5.95 bits/char, so
`length: 32` is only ~190 bits — fine for a password, but **not** a full 256-bit
key. Use `chars: hex` (4 bits/char × 64 = 256) or `chars: base64url`, and size
`length` for the bit count you need.

Reserve `bash:` + `openssl` for material `passwd` can't produce — e.g. an RSA
private key (`exec: openssl genrsa 4096`, `newline: ensure`). Equally valid, but
it pulls in the approval gate above.

### Cache-first determinism

The cache directory `$PATH_SECRETS` is the **source of truth** for stable
output. Cached generators (`passwd`, `secretRef`, `htpasswd`, `bash`) check the
cache before generating; on cache hit, they return the existing value
unchanged. This produces byte-stable kustomize output across runs.

To rotate a value, delete its file from `$PATH_SECRETS` and re-run
`kustomize build`. For htpasswd, deleting just `<key>.bcrypt` regenerates
the hash with a new salt while preserving the username/password.

### Cross-secret references

The cache filename convention is `Secret.<name>.<namespace>.<key>`. A
producer Secret writes its values under this path; a consumer Secret
reads them via `secretRef:`:

```yaml
# Producer
apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: db-secret, namespace: default}
passwd:
  password: 32
---
# Consumer (in a different kustomization but the same $PATH_SECRETS)
apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: app-secret, namespace: default}
secretRef:
  DB_PASSWORD: db-secret/password
```

### Type validation

The plugin validates that the data map contains the keys k8s requires
for the chosen `type:`. For example, `kubernetes.io/tls` requires
`tls.crt` and `tls.key`:

```yaml
type: kubernetes.io/tls
literals:
  tls.crt: ...
  # missing tls.key → plugin errors out at build time
```

To opt out (e.g. when generating intermediate state), set
`validate: false`:

```yaml
type: kubernetes.io/tls
validate: false
literals:
  tls.crt: ...
```

Supported types and their required keys:

| Type | Required keys |
|------|--------------|
| `Opaque` (default) | None |
| `kubernetes.io/tls` | `tls.crt`, `tls.key` |
| `kubernetes.io/basic-auth` | `username`, `password` |
| `kubernetes.io/dockerconfigjson` | `.dockerconfigjson` |
| `kubernetes.io/dockercfg` | `.dockercfg` |
| `kubernetes.io/ssh-auth` | `ssh-privatekey` |

Unknown types pass through without validation.

### Security

- **Path traversal rejected** in `file:` and `secretRef:` (no `..`, no absolute paths)
- **File size limit** of 1 MiB on `file:` reads
- **Atomic writes** to the cache (tmp file + rename) so concurrent reads never see partial data
- **0600 file mode** on cache entries
- **0700 directory mode** on the cache root
- **No secret values logged** to stderr at any verbosity
- **bcrypt cost 10** for htpasswd (apache default)
- **`crypto/rand`** for all random generation (no `/dev/urandom + tr` bias)

### Error messages

Errors are reported with line numbers from the source CRD:

```
secret plugin: line 14: passwd.NUXT_SESSION_PASSWORD: length must be > 0, got 0
```

## Helm Charts via khelm

lok8s also uses [khelm](https://github.com/mgoltzsche/khelm) as a kustomize
generator plugin for Helm charts, so no Helm CLI dependency is needed.

### Usage

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
generators:
  - chart.yaml
```

```yaml
# chart.yaml — flat top-level fields (NOT a nested helmChart: block)
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: cert-manager
  namespace: cert-manager
kubeVersion: "1.31.12"          # set explicitly — helm otherwise defaults to v1.20.0
repository: https://charts.jetstack.io
chart: cert-manager
version: v1.16.3
valueFiles:
  - values.yaml                 # per-chart values live in a sibling values.yaml
```

khelm renders the Helm chart into plain YAML at build time, which kustomize
then processes like any other resource.

## Adding a new plugin

To add e.g. `configmap.lok8s.dev/v1/ConfigMap`:

1. Create `kustomize/cmd/configmap/main.go` (~10 lines, copy from `cmd/secret/main.go`)
2. Create `kustomize/plugins/configmap/{spec,generator}/` for plugin-specific code
3. Create `kustomize/plugins/configmap/plugin.go` wiring spec → generators → builder
4. Add a target in `kustomize/Makefile` for the new binary
5. Reuse everything in `kustomize/pkg/`

Each plugin has its own `cmd/<name>/` and `plugins/<name>/` namespaces;
shared infrastructure (cache, random, charset, htpasswdfmt, kyaml,
kresource, fileio, errs, plugin runtime) lives under `kustomize/pkg/`
with no per-plugin coupling. See `kustomize/README.md` for details.
