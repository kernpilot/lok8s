# Secrets

lok8s manages secrets through a small, deterministic cache plus a kustomize
generator — with optional SOPS/age encryption so secrets can be committed to git
safely. No external secret store is required to get started.

## The model

- The **secrets kustomize plugin** (`secrets.lok8s.dev/v1/Secret`) turns a
  declarative YAML spec into a Kubernetes `Secret` at build time. See the
  [plugin reference](/reference/kustomize-plugins#secrets-generator) for every
  generator type (`passwd`, `bash`, `env`, `file`, `secretRef`, …).
- Generated values are cached under **`$PATH_SECRETS`** (default `.secrets/`),
  one file per key: `Secret.<name>.<namespace>.<key>`. **The cache is the source
  of truth** — first build generates, later builds reuse, so output is stable.
  Repos driving **multiple** instances isolate this **per domain** — see
  [Per-environment isolation](#per-environment-isolation-multiple-instances).
- Plaintext cache files are **gitignored**. To share them, commit them
  **encrypted** with SOPS/age (below).

```
.secrets/Secret.myapp.default.PASSWORD       ← plaintext (gitignored)
.secrets/Secret.myapp.default.PASSWORD.enc   ← SOPS-encrypted (committed)
```

## Generating a secret

Declare a generator and reference it from your kustomization:

```yaml
# secret.yaml
apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: { name: myapp, namespace: default }
passwd:
  SESSION_KEY: { length: 64, chars: hex }   # 256-bit key as hex
```

```yaml
# kustomization.yaml
generators:
  - secret.yaml
```

The first build generates `SESSION_KEY`, caches it, and emits the `Secret`. To
rotate a value, delete its cache file and rebuild. (Charset options and how to
generate proper cryptographic keys are covered in the
[reference](/reference/kustomize-plugins#generating-cryptographic-keys).)

## Setting an external value

For secrets that come from outside (an API token, a vendor key), write the value
straight into the cache instead of generating it:

```bash
lo secrets set --name myapp --namespace default API_TOKEN <value>
```

It's then cached like any generated value and can be encrypted + committed.

## No literals in the spec — generate, or pin once

The Secret spec has **no literal/static value field**, by design: a plaintext
value must never be baked into a committed `Secret.*.yaml`. The rule:

- **Default — generate it.** Every key comes from a generator (`passwd`, `bash`,
  …) and is cached. This holds even for values you might be tempted to fix by
  hand (a password, an HMAC key): let it be random.
- **Need a *specific* value?** (a chosen password, or one value two components
  must share.) Still declare the generator — so the key always exists and a build
  never fails — then pin the exact value **once** with `lo secrets set` (above).
  The cache is the source of truth, so the set value sticks; the operator does it
  a single time per environment, and it encrypts + commits like any other key.
- **Not actually a secret?** An *identifier* — an OIDC `client_id`, a username, a
  hostname, a public URL — does not belong in a `Secret` at all. Put it in plain
  config (Helm values / a `ConfigMap`), where a literal is fine and reviewable in
  the diff. (Don't smuggle an identifier through a Secret just to colocate it.)

## Approving `bash:` generators

`bash:` generators run shell commands at build time, so they're gated: after
cloning, or whenever a `bash:` command changes, approve the current set once:

```bash
lo secrets allow
```

Until then the build refuses to execute them.

## Committing secrets (SOPS/age)

The cache is gitignored by default. To share secrets across machines or
teammates, commit them **encrypted** — no separate key ceremony, your SSH key
*is* your encryption identity (via `ssh-to-age`; ed25519 only).

```bash
# one-time: derive an age key from ~/.ssh/id_ed25519 and write .sops.yaml
lo secrets init            # needs `sops` + `ssh-to-age` (b install)

lo secrets encrypt         # write Secret.*.enc (SOPS-encrypted) for committing
git add .secrets/*.enc     # .gitignore allows: .secrets/ + !.secrets/Secret.*.enc

# on another machine / for a teammate whose age key is in .sops.yaml:
lo secrets decrypt         # restore the plaintext cache from the .enc files
```

Add teammates by putting their age public keys (derived from their SSH keys)
into `.sops.yaml`'s `creation_rules`, then re-`encrypt`.

## Per-environment isolation (multiple instances)

A repo that drives **more than one** instance — dev and prod, or several
tenants — must not let those instances share a secret store. lok8s isolates them
**per domain**: a domain keeps its own store under `clusters/<domain>/secrets/`,
and both the CLI and `lo build <domain>` use **only that store** for that domain.

```
clusters/kubehz.dev/secrets/Secret.app.default.PASSWORD{,.enc}
clusters/kubehz.cloud/secrets/Secret.app.default.PASSWORD{,.enc}
```

Opt in by creating the directory; from then on it's automatic:

```bash
lo secrets --domain kubehz.cloud set --name app --namespace default API_TOKEN <v>
lo build kubehz.cloud            # the secrets plugin reads clusters/kubehz.cloud/secrets/
```

Why this is the right default for anything multi-environment:

- **No silent sharing.** Cache keys are `Secret.<name>.<ns>.<key>` with no
  environment in them, so a *single* flat store hands dev and prod the same value
  for a colliding (name, namespace) — including the same *generated* password or
  master key, which nobody chose. Per-domain stores make that impossible.
- **No shared tier, on purpose.** There is no fallback from a domain's store to a
  shared one. A value genuinely needed in two instances is a deliberate manual
  copy — the operator consenting to that exposure — never something a default did
  quietly. Prefer issuing a *separate* credential per instance: most providers
  can (a per-instance registry robot account, a project-scoped API token, …).
- **The real boundary is the decrypt key, not the folder.** Folders alone are
  cosmetic if one age key decrypts everything. Scope SOPS `creation_rules` by
  path so each environment encrypts to its **own** recipients — then a dev/CI key
  cannot decrypt prod:

```yaml
# .sops.yaml — first match wins, so specific rules go BEFORE any catch-all
creation_rules:
  - path_regex: clusters/kubehz\.cloud/secrets/Secret\..*
    age: 'age1prod…'                 # prod-key holders only
  - path_regex: clusters/.*/secrets/Secret\..*
    age: 'age1dev…,age1prod…'        # dev/CI (+ prod, who may read everything)
```

### Migrating a flat `.secrets/` store

Per-domain is opt-in, so existing flat stores keep working until you split them:

```bash
mkdir -p clusters/<domain>/secrets
git mv .secrets/Secret.<…>* clusters/<domain>/secrets/   # the keys that domain owns
lo secrets --domain <domain> encrypt                     # re-encrypt in place
```

Anything two instances were *sharing* must be **re-issued per instance** (or, if
truly unavoidable, copied deliberately). The cleanest reset is to regenerate at
go-live: create the per-domain stores, drop the old flat cache, scope the
`.sops.yaml` rules to per-environment keys, and let the next build mint fresh,
isolated values.

## Using a secret

Mount the `Secret` as a **file** rather than injecting it via an env var — a
mounted file isn't exposed through `/proc`, crash dumps, or child processes, and
rotates without a pod restart. The mounted file contains exactly the generated
bytes.

## Inspecting

```bash
lo secrets list                     # what's in the cache
lo secrets print [pattern...]       # show value(s)
lo secrets path                     # the resolved $PATH_SECRETS for this context
```

## See also

- [Kustomize Plugins → Secrets Generator](/reference/kustomize-plugins#secrets-generator)
  — every generator type, charsets, cryptographic-key guidance.
