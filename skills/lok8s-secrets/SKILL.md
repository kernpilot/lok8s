---
name: lok8s-secrets
description: >-
  Use when writing a lok8s secrets.lok8s.dev Secret generator (a Secret.*.yaml
  used by a kustomization) or managing secrets via `lo secrets` / `lo trust`.
  Covers the generator forms
  (passwd/bash/env/file/secretRef/htpasswd/literals/b64/cert), the per-domain
  `clusters/<domain>/secrets/` cache store, and the caching/approval/path gotchas.
---

# lok8s secrets (`secrets.lok8s.dev` kustomize plugin)

lok8s generates Kubernetes Secrets at kustomize build time from a
`secrets.lok8s.dev/v1` `Secret` manifest (referenced under `generators:` in a
kustomization). Generated/derived values are cached one file per key
(`Secret.<name>.<ns>.<key>`) so output is byte-stable across builds; **the cache
is the source of truth** (first build generates, later builds reuse). Commit the
cache encrypted with SOPS+age (`lo secrets init` / `encrypt` → `Secret.*.enc`).

**The store is PER-DOMAIN.** When a domain is in context, the cache lives in
**`clusters/<domain>/secrets/`** and that store is used exclusively — `lo build`
and the CLI mirror this. The flat **`$PATH_SECRETS`** store (default `.secrets/`)
is a fallback only for single-instance projects with **no** domain context; for
domain secrets it is deprecated (keep only global, non-domain material there).
Each domain has its own store, so dev/prod never share a generated value, and
SOPS `creation_rules` can encrypt each store to **different** recipients. Pass
`--domain <domain>` (or set the domain context) to act on a domain's store.

## Manifest shape

```yaml
apiVersion: secrets.lok8s.dev/v1     # the only supported group/version
kind: Secret
metadata: { name: myapp, namespace: default }
type: Opaque                          # default; validates required keys for tls/basic-auth/etc.
passwd:
  SESSION_KEY: { length: 64, chars: hex }     # 256-bit key as hex
```
Reference it from the kustomization:
```yaml
generators: [ Secret.myapp.default.yaml ]
```

## Generator sections (mix freely; keys are the Secret data keys)

| Section | Produces | Cached? | Example |
|---------|----------|---------|---------|
| `literals` | verbatim value | no | `API_MODE: prod` |
| `passwd` | random password | yes | `PW: { length: 32, chars: alphanum+symbols }` or `PW: 32` |
| `env` | value of a host env var | yes (unless `update: true`) | `TOKEN: MY_ENV_VAR` (null → use the key name) |
| `file` | contents of a local file | no | `ca.crt: ./certs/ca.crt` or `{ path: ..., mode: passthrough }` |
| `b64` | pre-base64 passthrough | no | `DATA: <base64>` |
| `secretRef` | another cached Secret's value | — | `DB_PW: db-secret/password` or `db-secret/<ns>/password` |
| `htpasswd` | `user:$2y$…` bcrypt line | yes | `auth: { username: { length: 16 }, password: { length: 32 } }` |
| `bash` | output of a command/script | yes (unless `update: true`) | see below |
| `cert` | dev CA or leaf cert (`crypto/x509`, no mkcert binary) | yes | `cert: { hosts: [example.test, "*.example.test"] }` — see below |

`chars` charsets: `alphanum`, `alphanum+symbols`, `hex`, `base64url`,
`custom:<chars>`. For crypto keys prefer **`passwd`** (e.g. `{length: 64, chars: hex}`)
over `bash`+openssl — no approval gate.

```yaml
bash:
  BUILD_SHA: "git rev-parse HEAD"          # string shorthand
  RSA_KEY:
    exec: openssl genrsa 4096              # 'exec' (bash -c) OR 'file' (a script path) — mutually exclusive
    output: stdout                          # stdout | stderr | combined
    encode: ""                              # "" raw | base64 | hex  (applied BEFORE newline)
    newline: strip                          # strip | keep | ensure
  LIVE:
    exec: kubectl config view --minify --flatten
    update: true                            # bypass cache; re-run EVERY build (live state)
```

## `cert:` — dev CA + leaf certs

`cert:` mints development TLS with `crypto/x509` (no `mkcert` binary in build/CI).
One cert per Secret; a `kubernetes.io/tls` Secret holds exactly `tls.crt` +
`tls.key`. Cache-first like `passwd` (rotate by deleting the cache file).

```yaml
type: kubernetes.io/tls
cert:
  hosts: [example.test, "*.example.test"]   # leaf SANs: DNS names, wildcards, IPs
```
- **Leaf (default):** signed by the **shared mkcert CA at `$CAROOT`** (one CA per
  developer, across all projects; loaded if present, created there if not).
- **`cert: { ca: true }`** — declare an **own** CA Secret in the lok8s store
  (emits `ca.crt`; the `ca.key` stays cached for signing, never in the Secret).
  Mutually exclusive with `hosts` and `caRef`.
- **`caRef: <secret>[/<namespace>]`** on a leaf signs with that own store CA
  instead of the shared CAROOT one — deterministic, no home-dir writes, SOPS-able
  (prefer in CI). The CA is auto-created on first use, so build order is irrelevant.
- **`cert: { caRoot: true }`** (no other fields) emits the shared CAROOT CA's
  **public cert** as `ca.crt`, for distributing trust into the cluster.
- The default writes `rootCA.pem` under `$CAROOT` (a side effect outside
  `$PATH_SECRETS`); `caRef` keeps everything inside the store.
- **Trust is out of scope of the build.** The plugin only *generates* the CA;
  install it into OS/browser trust stores once with **`lo trust`** (wraps
  `mkcert -install`, same CAROOT; `mkcert` is needed only here, never to build).

## ⚠️ Gotchas
- **`bash`/`file` cache by the COMMAND/PATH string, not the output.** A
  `bash: { file: ./gen.sh }` or `file: ./x.crt` will keep serving the cached value
  even after the file's *contents* change (the path is unchanged). To rotate:
  delete the cache file `$PATH_SECRETS/Secret.<name>.<ns>.<key>` and rebuild.
- **`bash` approval gate:** a new/changed `bash:` command hash isn't in the local
  (un-committed) `.bash-allow` set → the **build fails** until you run
  `lo secrets allow`. `update: true` does not change the hash (no re-allow needed).
- **`file` rejects absolute paths and `..`** — paths are relative to the kustomize
  build root, max 1 MiB. `secretRef` likewise rejects path traversal.
- **No plaintext secrets in committed manifests** — generate them, or pin once with
  `lo secrets [--domain <d>] set --name <n> [--namespace <ns>] <KEY> [value]` (reads
  stdin if value omitted). Put non-secret identifiers (client IDs, usernames) in
  plain config.
- **Per-domain vs flat store:** without `--domain`, `lo secrets` (and a raw
  `kustomize build`) hits the flat `$PATH_SECRETS` store — NOT a domain's
  `clusters/<domain>/secrets/`. The two diverge silently; building from the wrong
  one can re-key a live cluster. Pass `--domain` for domain secrets; reserve flat
  `.secrets/` for global, non-domain material.
- `$PATH_SECRETS` unset → cached generators error when there's no domain store to
  resolve. `lo build` exports the right path for you; set it if invoking kustomize
  directly without a domain.

## `lo secrets` workflow

Subcommands: `init`, `set`, `encrypt`, `decrypt`, `allow`, `list`, `print`,
`env`, `path`. Add `--domain <d>` to target that domain's store (omit it only for
the flat single-instance store).

```bash
lo secrets init                            # derive an age key from your SSH key → .sops.yaml
lo secrets --domain <d> set --name myapp SESSION_KEY    # seed/pin a cached value (reads stdin)
lo secrets allow                           # approve new/changed bash: generator hashes
lo secrets --domain <d> encrypt            # SOPS-encrypt the cache → Secret.*.enc (commit these)
lo secrets --domain <d> decrypt            # restore plaintext from .enc on another machine
lo secrets --domain <d> list | lo secrets --domain <d> print [pattern]
lo secrets --domain <d> env --name hetzner # `export KEY=value` lines (eval for provision creds)
lo secrets --domain <d> path               # resolved store path for the current context
lo trust                                   # install the dev CA into OS/browser trust stores
```
> Note: `lo secrets set` updates the **cache**, not the live in-cluster Secret —
> rebuild + re-apply the target afterward, or the live value stays stale.
