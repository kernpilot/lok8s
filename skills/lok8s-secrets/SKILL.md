---
name: lok8s-secrets
description: >-
  Use when writing a lok8s secrets.lok8s.dev Secret generator (a Secret.*.yaml
  used by a kustomization) or managing secrets via `lo secrets`. Covers the
  generator forms (passwd/bash/env/file/secretRef/htpasswd/literals/b64), the
  $PATH_SECRETS cache, and the caching/approval/path gotchas.
---

# lok8s secrets (`secrets.lok8s.dev` kustomize plugin)

lok8s generates Kubernetes Secrets at kustomize build time from a
`secrets.lok8s.dev/v1` `Secret` manifest (referenced under `generators:` in a
kustomization). Generated/derived values are cached under **`$PATH_SECRETS`**
(default `.secrets/`) so output is byte-stable across builds; commit them
encrypted with SOPS+age (`lo secrets init` / `encrypt` → `Secret.*.enc`).

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
- **No plaintext secrets in committed manifests** — generate them, or set once with
  `lo secrets set --name <n> [--namespace <ns>] <KEY> [value]` (reads stdin if value
  omitted). Put non-secret identifiers (client IDs, usernames) in plain config.
- `$PATH_SECRETS` unset → cached generators error. `lo` exports it for you; set it
  if invoking kustomize directly.

## `lo secrets` workflow
```bash
lo secrets init                 # derive an age key from your SSH key
lo secrets set --name myapp SESSION_KEY    # seed/override a cached value (reads stdin)
lo secrets allow                # approve new bash: generator hashes
lo secrets encrypt              # SOPS-encrypt the cache → Secret.*.enc (commit these)
lo secrets list | lo secrets print <pattern>
```
> Note: `lo secrets set` updates the **cache**, not the live in-cluster Secret —
> rebuild + re-apply the target afterward, or the live value stays stale.
