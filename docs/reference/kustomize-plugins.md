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
ten generator types (`literals`, `passwd`, `template`, `env`, `secretRef`,
`htpasswd`, `file`, `b64`, `bash`, `cert`). The cache directory `$PATH_SECRETS`
is the source of truth for stable output across runs.

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

template:
  MATRIX_SIGNING_KEY:                   # composite: literal prefix + fields
    pattern: "ed25519 a_{kid} {seed}"
    fields:
      kid:  {length: 4, chars: "custom:abcdefghijklmnopqrstuvwxyz"}
      seed: {bytes: 32, encoding: base64-unpadded}

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
| `template:` | Composite value: a pattern with `{name}` placeholders filled by typed sub-sections (literals/passwd/bytes/env/secretRef/key) | Yes | Cache-first (the *composed* value is cached by key, not the pattern). For multi-part secrets — retires fragile `bash:` format-glue. See [Composite secrets](#composite-secrets-template) |
| `env:` | Read from env var | Yes (unless `update: true`) | Falls back to key as var name when value is null |
| `secretRef:` | Read from another Secret's cache file | Reads cross-secret | Shorthand `"secret/key"` or `"secret/ns/key"`; no path traversal |
| `key:` | Asymmetric private key (RSA or Ed25519) as PKCS#8 PEM | Yes | Cache-first (existing key reused verbatim — no re-key). Retires `bash: openssl genpkey`. See [Private keys](#private-keys-key) |
| `htpasswd:` | Bcrypt-hashed username:password line | Yes (3 files: `.username`, `.password`, `.bcrypt`) | Username generator starts with a letter; cost factor 10 |
| `file:` | Read local file | No | 1 MiB max; path traversal rejected; `mode: raw` (default) or `passthrough` |
| `b64:` | Pre-base64-encoded passthrough | No | Validates the input is valid base64 |
| `bash:` | Run a shell command, use its output | Yes | Each command is SHA256-pinned in a committed `.sha` file; on change the build fails until re-approved via `lo secrets allow` |
| `cert:` | Development CA or leaf cert (crypto/x509, no `mkcert` binary) | Yes | One cert per Secret; CA auto-created on first use. See [Development certificates](#development-certificates-cert) |

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

### Composite secrets (`template:`)

`template:` builds a secret whose value has a **fixed structure** — a literal
pattern with `{name}` placeholders — where each placeholder is produced by a
typed **sub-section** that reuses the top-level generator types. Think of it as a
**mini-Secret embedded in a pattern**: `literals:`, `passwd:`, `env:`,
`secretRef:`, `key:`, plus a template-local `bytes:` (raw `crypto/rand` bytes +
encoding). It exists for multi-part values that `passwd:` can't express (it makes
exactly one random string, no prefix/composition) and that would otherwise fall
back to a brittle `bash:` pipeline.

The motivating case is the matrix-hookshot **registration.yml**, which inlines
two appservice tokens generated elsewhere in the same Secret, plus its RSA
**passkey.pem**:

```yaml
passwd:
  as_token: { length: 64, chars: hex }   # the shared appservice tokens
  hs_token: { length: 64, chars: hex }
key:
  passkey.pem: rsa                        # RSA-4096 PKCS#8 PEM (see key: below)
template:
  registration.yml:
    pattern: "id: matrix-hookshot\nas_token: \"{as_token}\"\nhs_token: \"{hs_token}\"\n"
    secretRef:                            # sibling shorthand — keys in THIS Secret
      as_token: as_token
      hs_token: hs_token
```

Because `secretRef` pulls the *same* cached `as_token`/`hs_token`, the tokens in
`registration.yml` are byte-identical to the top-level keys — one source of
truth, so hookshot and Synapse always agree.

**Typed sub-sections** (each is `map[placeholder] → <entry>`; a placeholder must
be produced by **exactly one** sub-section):

| Sub-section | Entry type | Example |
|---|---|---|
| `literals:` | string | `{ tag: matrix-hookshot }` |
| `passwd:` | [`passwd`](#password-charsets-passwd) entry | `{ pw: { length: 32, chars: hex } }` |
| `bytes:` | `{ bytes: N, encoding: … }` (or shorthand `N`) | `{ seed: { bytes: 32, encoding: base64-unpadded } }` |
| `env:` | [`env`](#env-contract) entry (`optional`→empty, `default`, `passwd` fallbacks) | `{ region: "$AWS_REGION" }` |
| `secretRef:` | [`secretRef`](#cross-secret-references) entry + **sibling shorthand** | `{ token: as_token }` (sibling) · `{ dbpw: db/password }` (cross) |
| `key:` | [`key`](#private-keys-key) entry | `{ pem: ed25519 }` |

The **sibling shorthand** is the key addition to `secretRef`: a bare string with
**no `/`** means "a key in **this** Secret" (current secret + namespace) —
`{ as_token: as_token }` above. The `secret/key` and `secret/ns/key` forms (and
the full mapping) still reference **other** secrets, exactly as top-level
`secretRef:`.

The template-local `bytes:` sub-section reads N raw `crypto/rand` bytes and
encodes them — true N-bytes-of-entropy (not a charset draw), so `bytes: 32` is a
full 256 bits regardless of alphabet. Encodings and the url-safety caveat are the
same as the legacy bytes field below (`bytes` is capped at 4096).

**Legacy `fields:` (backward-compat, still supported).** Before the typed
sub-sections, a template declared its fields under `fields:`, each in one of two
modes. This still works byte-identically — the synapse **ed25519 signing key**
uses it — but the typed sub-sections above are **preferred** for new templates:

```yaml
template:
  signing.key:
    pattern: "ed25519 a_{kid} {seed}\n"
    fields:
      kid:  {length: 4, chars: "custom:abcdefghijklmnopqrstuvwxyz"}  # charset field
      seed: {bytes: 32, encoding: base64-unpadded}                    # bytes field
```

renders to e.g. `ed25519 a_nsnx Oji0Bha34hv3gcoEkRMLAg2Q4jTFG0n4WQd+I9bx77s`. A
`fields:` entry is **charset-mode** (`length` + optional `chars`/`require`, same
DSL as [`passwd:`](#password-charsets-passwd)) **XOR bytes-mode** (`bytes` +
optional `encoding`) — exactly one; both/neither is a config error. The bytes
encodings:

| `encoding` | Output alphabet | Length for N bytes |
|---|---|---|
| `base64` (default) | standard `+/`, padded | `4·⌈N/3⌉` |
| `base64url` | url-safe `-_`, padded | `4·⌈N/3⌉` |
| `base64-unpadded` | standard `+/`, no `=` | `⌈4N/3⌉` |
| `base64url-unpadded` | url-safe `-_`, no `=` | `⌈4N/3⌉` |
| `hex` | lowercase `0-9a-f` | `2N` |

> **`base64` (the default) and `base64-unpadded` emit `+` and `/`** — the
> standard alphabet. Those are not safe in a URL, a filename, or a k8s
> label/annotation value. If the secret lands anywhere url-safe, choose
> `base64url` / `base64url-unpadded` (`-_` alphabet). `bytes` is capped at 4096.

`fields:` and the typed sub-sections **may coexist** in one entry (each producing
distinct placeholders).

**Why not `bash:`.** Gluing a composite together in shell (`printf 'ed25519 a_%s %s' "$(tr -dc a-z </dev/urandom | head -c4)" "$(head -c32 /dev/urandom | base64)"`)
has two failure modes `template:` avoids: (1) the `tr … | head` pipeline
**SIGPIPEs** in a non-tty render (`head` closes the pipe early → the process
exits 141), and (2) `bash:` is SHA-pinned, so **any edit to the command forces a
`lo secrets allow` re-approval** and re-keys the value across every plane.
`template:` composes the value in-process from `crypto/rand`, caches the
composed string by the entry key (like `passwd:`), and has **no approval gate**.

**Pattern rules.** The pattern is validated at parse time: it must be non-empty,
every `{name}` must resolve to a field declared by exactly one sub-section (or
`fields:`), and **every declared field must be referenced** (an unused field is a
typo, so it's an error). Write a **literal brace** by doubling it — `{{` → `{`,
`}}` → `}`. An unterminated `{` or a stray `}` is rejected. A field referenced
**twice** (`pattern: "{a}-{a}"`) is generated **once** and the single value is
substituted at both sites (→ `V-V`), never drawn twice. Substitution is a
**single pass**: a generated value that happens to contain `{...}` is emitted
literally, not re-expanded — so field values can never inject placeholders.

#### Substitution operators

A placeholder may carry a **bash-style parameter-expansion operator**, applied to
the resolved value at substitution time. Operators are **literal** (no
glob/regex) and parse **after** the field name:

| Operator | Effect | `{x…}` on `x = "matrix-hookshot.pem"` |
|---|---|---|
| `{x^^}` / `{x,,}` | upper / lower (whole string) | `MATRIX-HOOKSHOT.PEM` / `matrix-hookshot.pem` |
| `{x^}` / `{x,}` | upper / lower **first char** | `Matrix-hookshot.pem` / `matrix-hookshot.pem` |
| `{x%%suf}` / `{x%suf}` | strip trailing literal (longest / shortest — identical for a literal) | `{x%%.pem}` → `matrix-hookshot` |
| `{x##pre}` / `{x#pre}` | strip leading literal | `{x##matrix-}` → `hookshot.pem` |
| `{x//old/new}` / `{x/old/new}` | replace all / first (literal) | `{x//-/_}` → `matrix_hookshot.pem` |
| `{x:off}` / `{x:off:len}` | substring by **byte** offset (+ optional length) | `{x:0:6}` → `matrix` |

Plain `{x}` is unchanged; `{{`/`}}` are still literal braces. A **malformed**
operator (`{x%%%y}`, `{x/onlyold}`, `{x:abc}`) is rejected at **decode** time; a
substring whose offset/length runs past the *value* errors at render time (byte
offsets, not bash's silent clamping). First-char case ops are rune-aware. Field
names that literally contain an operator sigil (`^ , % # / :`) can't take an
operator — but no Secret data key uses those.

**Caching.** Cache-first like `passwd:` — the **composed** value is stored under
the entry key (`Secret.<name>.<ns>.<key>`), not the pattern or per-field state.
The pattern/fields are deliberately **not hashed** (that hash is the `bash:`
approval-gate model this generator escapes): editing the pattern only changes
*newly generated* keys; existing cached values are reused verbatim. Rotate by
deleting the cache file.

> **Migrating an existing `bash:`-generated composite to `template:` re-keys it
> once.** The value is keyed by the entry name, but the *bytes* a `template:`
> composes won't match whatever the old `bash:` command produced, and there's no
> shared cache file between the two generators. Plan a one-time rotation (delete
> the old cache entry, let `template:` mint a fresh value, roll the consumer) —
> after that it's byte-stable like any other cached generator.

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

### Private keys (`key:`)

`key:` mints an **asymmetric private key** — RSA or Ed25519 — as **PKCS#8 PEM**,
the format `openssl genpkey` produces by default. It's the shell-free, approval-
gate-free replacement for `bash: { exec: "openssl genpkey …" }`:

```yaml
key:
  passkey.pem: rsa                 # shorthand: algorithm → all defaults (rsa/4096/pkcs8)
  signing.pem:
    algorithm: ed25519             # rsa (default) | ed25519
  db-client.pem:
    algorithm: rsa
    bits: 2048                     # RSA modulus size (rsa only; default 4096)
    encoding: pkcs8                # only pkcs8 (the default) is supported
```

- **Cache-first** like `passwd:` — an existing cached key is reused **verbatim**,
  so `key:` **never re-keys**. A key minted earlier by `openssl` and dropped into
  `$PATH_SECRETS` (or committed via SOPS) is served unchanged; delete the cache
  file to rotate.
- **RSA** defaults to **4096** bits; sizes below 2048 are rejected at decode.
  **Ed25519** has a fixed size, so it takes no `bits`.
- `key:` is also usable as a **[template sub-section](#composite-secrets-template)**
  (`template: { config.yml: { pattern: "…{pem}…", key: { pem: ed25519 } } }`) to
  inline a generated key into a config file, via the same reuse path.

Use `key:` for private keys; use [`cert:`](#development-certificates-cert) when
you need an X.509 certificate (leaf or CA), not a bare key.

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

For an **asymmetric private key** (RSA/Ed25519), use [`key:`](#private-keys-key) —
PKCS#8 PEM, cache-first, no shell. For a **composite** value — random bytes
wrapped in a fixed format, like a Matrix/synapse `ed25519 a_<id> <base64 seed>`
signing key — use [`template:`](#composite-secrets-template) rather than `bash:`.
A bytes field (`{bytes: 32, encoding: base64-unpadded}`) is a full 256 bits of
`crypto/rand` entropy in the exact wire shape, with no shell pipeline and no
approval gate.

Reserve `bash:` + `openssl` for material none of `passwd` / `key` / `template`
can produce. (`key:` now covers the common `openssl genpkey`/`genrsa` cases
without the approval gate.)

### Cache-first determinism

The cache directory `$PATH_SECRETS` is the **source of truth** for stable
output. Cached generators (`passwd`, `key`, `template`, `secretRef`, `htpasswd`,
`bash`) check the cache before generating; on cache hit, they return the
existing value unchanged. This produces byte-stable kustomize output across
runs.

To rotate a value, delete its file from `$PATH_SECRETS` and re-run
`kustomize build`. For htpasswd, deleting just `<key>.bcrypt` regenerates
the hash with a new salt while preserving the username/password.

### Env contract

Beyond `$PATH_SECRETS` (the cache directory), two env vars shape a run. A
caller — chiefly the `lo build` pipeline — sets them on the `kustomize build`
invocation to steer the generator without editing any Secret CRD.

| Env var | Value | Effect |
|---|---|---|
| `LOK8S_SECRETS_DISABLE` | `1` / `true` (case-insensitive) | **Store-free OFF switch.** The plugin emits **nothing** and short-circuits before any store access: it never reads `$PATH_SECRETS`, never touches or mints the cache, and never runs a generator. Returns success with empty output — kustomize accepts a generator that yields zero resources. Any other value (incl. unset, `0`) is off. |
| `LOK8S_SECRETS_OUTPUT` | `none` | **Run-but-suppress.** Runs the **full** pipeline — generators run, so store reads/mints and type validation still happen (side effects intact) — but suppresses the final output write, so zero resources are rendered. Use case: prime or validate the cache without rendering. An empty/unset value is a normal emit; any other non-empty value is **rejected** (fail closed) so a typo can't silently emit secrets. |

**Precedence:** `LOK8S_SECRETS_DISABLE` wins over `LOK8S_SECRETS_OUTPUT` — disable
means "do nothing at all", so the store is never consulted even if
`LOK8S_SECRETS_OUTPUT=none` is also set.

`LOK8S_SECRETS_DISABLE=1` is what [`lo build --no-secrets`](../guide/deployment.md#ci-render-without-secrets-no-secrets)
exports to make a CI render **store-free**: the split shaping alone leaves the
committed encrypted Secrets inert, but without `DISABLE` the `kustomize build`
render would still invoke this generator and hit the store.

> **Known limitation.** `LOK8S_SECRETS_DISABLE` emits *nothing*, which is safe
> when no consumer references the Secret by name (kustomize transformers only
> touch resources present in the render). A consumer that strategic-merge-patches
> a *specific* Secret by name would need a placeholder-emitting variant instead —
> not built today.

### Cross-secret references

The cache filename convention is `Secret.<name>.<namespace>.<key>`. A
producer Secret writes its values under this path; a consumer Secret
reads them via `secretRef:`. Both resolve against `$PATH_SECRETS`, which
`lo build`/`lo deploy` bind to the **active domain's** store
(`clusters/<domain>/secrets`) — so a `secretRef` resolves within the selected
cluster, never a global store:

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

### Development certificates (`cert:`)

`cert:` generates leaf certificates (and, when you ask, the CA that signs them)
with `crypto/x509` — **no `mkcert` binary** in the build or CI. One cert per
Secret (a `kubernetes.io/tls` Secret holds exactly `tls.crt` + `tls.key`).

By **default** a leaf is signed by the **shared mkcert CA at CAROOT** — one CA per
developer, across all your projects, trusted once with `mkcert -install`:

```yaml
apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: wildcard-tls, namespace: default}
type: kubernetes.io/tls
cert:
  hosts: [example.test, "*.example.test"]   # DNS names, wildcards, IPs
```

The CA at CAROOT (`$CAROOT`, else mkcert's OS default) is **loaded if present,
created there if not** — exactly mkcert's flow, so an existing mkcert CA is reused.

For **CI, separated instances, or special CAs**, sign with an **own, managed CA in
the lok8s store** via `caRef` instead — deterministic, no machine dependency, and
SOPS-encryptable. Declare it with `ca: true`:

```yaml
# own CA (Opaque): emits ca.crt. The CA key is cached as ca.key for signing and
# is NEVER written into the Secret.
metadata: {name: ci-ca, namespace: kube-system}
cert: {ca: true}
---
metadata: {name: ci-tls, namespace: default}
type: kubernetes.io/tls
cert:
  hosts: [example.test, "*.example.test"]
  caRef: ci-ca/kube-system                  # <secret>[/<namespace>]
```

- **Default = shared CAROOT CA; `caRef` = own store CA** (auto-created on first
  use, so build order is irrelevant).
- **`cert: {caRoot: true}`** (no `hosts`) emits the shared CAROOT CA's **public
  cert** as `ca.crt` — for distributing trust into the cluster (e.g. a configmap
  containerd or a pod reads), mkcert-free. The leaves it signed chain to it.
- **Cache-first** like `passwd` — CA, key, and leaf are byte-stable across runs;
  rotate by deleting the cache file. RSA-3072 CA / RSA-2048 leaf; leaf validity
  2 y 3 m (under Apple's 825-day cap).
- **CAROOT is a side effect**: the default writes `rootCA.pem`/`rootCA-key.pem`
  under your CAROOT (a dev convenience). `caRef` keeps the whole build inside
  `$PATH_SECRETS` with no home-directory writes — prefer it in CI.
- **Trust is out of scope.** The plugin *generates* the CA; installing it into
  OS/browser trust stores needs root, so trust it once with `mkcert -install`
  (same CAROOT) — a local convenience, never a build dependency.

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
