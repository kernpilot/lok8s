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
