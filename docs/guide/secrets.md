# Secrets

lok8s manages secrets through a small, deterministic cache plus a kustomize
generator, with optional SOPS/age encryption so you can commit secrets to git
safely. You need no external secret store to get started.

## The model

- The **secrets kustomize plugin** (`secrets.lok8s.dev/v1/Secret`) turns a
  declarative YAML spec into a Kubernetes `Secret` at build time. See the
  [plugin reference](/reference/kustomize-plugins#secrets-generator) for every
  generator type (`passwd`, `key`, `template`, `bash`, `env`, `file`,
  `secretRef`, `cert`, …). `template:` composes multi-part secrets from typed
  sub-sections + a pattern; `key:` mints RSA/Ed25519 private keys as PKCS#8 PEM
  (no `openssl`, no approval gate).
- The cache holds generated values one file per key
  (`Secret.<name>.<namespace>.<key>`) in a **per-domain store**,
  `clusters/<domain>/secrets/`. **The store is the source of truth**: first
  build generates, later builds reuse, so output is stable. Each domain has its
  **own** store, so environments never share a secret (see
  [Per-environment isolation](#per-environment-isolation)).
- **`$PATH_SECRETS` is the active domain's store.** `lo build` and `lo deploy`
  export `PATH_SECRETS=clusters/<domain>/secrets` for the selected domain (set
  with `lo use`, recorded in `clusters/.active`), so every generator, and every
  `secretRef:` that reads another secret, resolves *within that cluster*. The
  per-domain store is the only secrets store. A flat `.secrets/` at the project
  root is a leftover of releases before v0.4.0 (see
  [Migrating a flat `.secrets/` store](#migrating-a-flat-secrets-store)).
- **A new domain gets no flat fallback.** When one domain has its own store,
  a domain directory without `clusters/<domain>/secrets/` stops `lo build`,
  `lo bootstrap`, `lo provision`, `lo k8s` and `lo secrets` with an error.
  An explicit `PATH_SECRETS` does not override this refusal. Create the empty
  directory to generate new values for the domain. (A value the domain must
  share with another domain is a deliberate manual copy, see
  [Per-environment isolation](#per-environment-isolation).) `lo build
  --no-secrets` does not read the store and does not stop. A project with only
  the flat `.secrets/` store works as before.
- Plaintext cache files are **gitignored**. To share them, commit them
  **encrypted** with SOPS/age (below).

```
clusters/app.example.com/secrets/Secret.myapp.default.PASSWORD       ← plaintext (gitignored)
clusters/app.example.com/secrets/Secret.myapp.default.PASSWORD.enc   ← SOPS-encrypted (committed)
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

`lo build <domain>` generates `SESSION_KEY` into that domain's store
(`clusters/<domain>/secrets/`), caches it, and emits the `Secret`. To rotate a
value, delete its cache file and its `.enc` twin, then rebuild. A cache file
that is missing while its `.enc` twin exists stops the build: run
`lo secrets decrypt --domain <domain>` first. (Charset options and how to generate
proper cryptographic keys are covered in the
[reference](/reference/kustomize-plugins#generating-cryptographic-keys).)

## Setting an external value

For secrets that come from outside (an API token, a vendor key), write the value
straight into the cache instead of generating it. Pass `--domain` so it lands in
that domain's store (the flag also creates the store on first use):

```bash
lo secrets set --domain app.example.com --name myapp --namespace default API_TOKEN <value>
# omit the value to read it from a prompt / piped stdin (keeps it out of shell history):
printf %s "$TOKEN" | lo secrets set --domain app.example.com --name myapp --namespace default API_TOKEN
# or pass `-` explicitly (POSIX stdin placeholder), e.g. from a file
# (the frozen bash variant needs an argsh build with arg-sh/argsh#176):
lo secrets set --domain app.example.com --name myapp --namespace default API_TOKEN - < token.txt
```

To store the literal string `-` as a value, use the pipe form:
`printf %s - | lo secrets set … API_TOKEN`.

It's then cached like any generated value and can be encrypted + committed.

Pass **`--encrypt`** (short `-e`; the alias `--enc` also works) to SOPS-encrypt the
value in the same step: it writes the plaintext cache **and** its `.enc` in one go,
so you never leave a fresh value lying unencrypted:

```bash
printf %s "$TOKEN" | lo secrets set --domain app.example.com --name myapp API_TOKEN --encrypt
```

`--encrypt` encrypts **only** the file it just wrote (not a whole-store sweep; see the
staging note under [Committing secrets](#committing-secrets-sops-age)) and needs
encryption already set up (`.sops.yaml` present, via `lo secrets init`); without it the
set fails. When encryption **is** configured but you write plaintext-only (no
`--encrypt`), `set` warns that this value has no matching `.enc` yet, whether that's a
first write (none exists) or an edit (the committed one is now stale). Re-run with
`--encrypt` or `lo secrets encrypt` before committing.

## Provision-time credentials (shell env)

The **CLI / provisioner** consumes some secrets as shell environment
variables, *before* a cluster (or any Kubernetes `Secret`) exists: a cloud API
token, bare-metal credentials. Keep those in the managed store too, named after
the env vars you need, and load them with `lo secrets env` instead of a loose
`.env` file:

```bash
lo secrets --domain prod set HCLOUD_TOKEN <token> --name hetzner --namespace provisioning
# …then, in your provisioning script:
eval "$(lo secrets --domain prod env --name hetzner --namespace provisioning)"   # exports HCLOUD_TOKEN
```

`lo provision` and `lo recover` load `HCLOUD_TOKEN`, `HROBOT_USER` and
`HROBOT_PASSWORD` from `Secret.hetzner.provisioning.*` in the domain store by
themselves, when the variable is not set. A fresh checkout holds only the
`.enc` files: they decrypt them in memory with your age identity
(`SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE`, or `LOK8S_SSH_KEY`) and never write the
plaintext.

`env` emits an `export <key>=<value>` line per key of the named secret, with the
value shell-quoted (`%q`) so the `eval` is injection-safe. The creds get SOPS
encryption + per-domain isolation like everything else: no plaintext `.env`
sitting outside the model.

## No literals in the spec — generate, or pin once

The Secret spec has **no literal/static value field**, by design: a plaintext
value must never be baked into a committed `Secret.*.yaml`. The rule:

- **Default: generate it.** Every key comes from a generator (`passwd`, `bash`,
  …) and gets cached. This holds even for values you might be tempted to fix by
  hand (a password, an HMAC key): let it be random.
- **Need a *specific* value?** (a chosen password, or one value two components
  must share.) Still declare the generator (so the key always exists and a build
  never fails), then pin the exact value **once** with `lo secrets set` (above).
  The cache is the source of truth, so the set value sticks; the operator does it
  a single time per environment, and it encrypts + commits like any other key.
- **Not actually a secret?** An *identifier* (an OIDC `client_id`, a username, a
  hostname, a public URL) does not belong in a `Secret` at all. Put it in plain
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
teammates, commit them **encrypted**: no separate key ceremony, your SSH key
*is* your encryption identity (via `ssh-to-age`; ed25519 only).

```bash
# one-time: derive an age key from ~/.ssh/id_ed25519 and write .sops.yaml
lo secrets init                                 # needs `sops` + `ssh-to-age` (b install)

lo secrets --domain app.example.com encrypt     # write Secret.*.enc for committing
git add clusters/app.example.com/secrets/*.enc  # each store's .gitignore commits only *.enc

# on another machine / for a teammate whose age key is in .sops.yaml:
lo secrets --domain app.example.com decrypt     # restore plaintext from the .enc files
```

`init` writes `.sops.yaml` only when no file is at that path. A key that is
not ed25519 gives no age recipient: `init` stops and writes nothing, also
when a `.sops.yaml` is there. A symlink to no file or a directory at the
path also stops `init` before it writes anything.

Then `init` takes the lock of `add-key` and `remove-key` (see below), and
checks the path again. It makes the store and its `.gitignore`. Then it
writes a temp file (`..sops.yaml.tmp-*`) that a rename puts in place.

Thus a run that stops, or a full disk, leaves no part of a `.sops.yaml`.
A store step that fails leaves no `.sops.yaml`, so the next `init` is a
first run again.

The lock keeps other lo runs out. When another run wrote the file while
`init` waited for the lock, `init` stops and writes nothing. Run `init`
again when a file is there: it then checks that file.

A program that is not lo takes no lock. The rename replaces a `.sops.yaml`
that such a program writes between the check and the rename.

The `lo` binary syncs the temp file to disk before the rename. The bash
implementation does not: after a crash of the host, its `.sops.yaml` can be
incomplete. The bash implementation needs `flock` and a `mv` that takes
`-T` for this write: on macOS, install `flock` and GNU coreutils (`gmv`).
The `lo` binary needs neither.

The lock file and a temp file that a killed write leaves are not for git.
`lo init project` adds `.sops.yaml.lock`, `.sops.yaml.tmp-*` and
`..sops.yaml.tmp-*` to the `.gitignore` of the project. In a project that an
older `lo` made, `lo toolchain install` and a rerun of `lo init project` add
the entries that are missing, and do not add a line twice. A project that
runs neither command gets the three lines by hand.

Each store has a `.gitignore` that commits only the `.enc` files. lo writes
its rules as a block between two marker lines:

```gitignore
# BEGIN lok8s secrets. lo rewrites the lines up to the END line: add your own rules below it.
# Ignore everything except this file and the SOPS-encrypted secret files.
# A plaintext secret file must never be committed.
*
!.gitignore
!Secret.*.enc
*/
# END lok8s secrets
```

Git uses the last rule that matches a path. The block ignores every path,
then re-includes this file and the `Secret.*.enc` files. A pattern with no
trailing slash also matches a directory, so `!Secret.*.enc` alone would
re-include a directory such as `Secret.x.enc/`, and git would then read a
`.gitignore` in it. The last rule, `*/`, ignores every directory again. No
file that lo writes and git must commit is in a directory of the store.

Add your own rules below the END line. lo rewrites only the lines of the
block and keeps each line above and below it byte for byte. A UTF-8 byte
order mark (BOM) at the start of the file stays at the start, because git
skips a BOM only there.

A `.gitignore` with no block gets the block at the top, and its lines follow
the block. A `.gitignore` that an older lo wrote (five lines that start with
`# Auto-generated by lok8s`) gets the block in place of those lines.

`init`, `add-key`, `encrypt`, `set` and `decrypt` read the whole `.gitignore`
as git does, before the step that the file protects. `init` and `add-key`
read it before their first write. A rerun of `init` or `add-key` that has
nothing else to do reads it too, and writes it when it needs a change. The
commands refuse these files:

- A negation (a line whose first character is `!`) below the END line, or
  in a file with no block. After the block, each negation can re-include a
  plaintext file. Remove the line, or keep the file outside the store. A
  line `\!name` is not a negation (it ignores a file whose name starts with
  `!`), and it passes.

- A rule inside the block that is not one of the four rules of lo. Move it
  below the END line.

- A marker line that occurs twice, or the two marker lines in the wrong
  order.

- A NUL byte, because git ends a rule at that byte.

- A file larger than 1 MiB (1048576 bytes), a file that would be larger
  than 1 MiB with the block of lo, or a file that lo cannot read. Thus a
  run never writes a `.gitignore` that the next run refuses.

A rule above the block has no effect, because the `*` of the block comes
after it. A rule below the block that ignores more files passes.

A refusal names the file, the line number and the reason. It does not print
the rule, because a rule can hold a pasted value. The last line of the
refusal names the step that the command did not do, for example
`lo stopped before the plaintext write`. An earlier step of the same command
can have written a file: `init` writes `.sops.yaml` before its last read of
the `.gitignore`, and `add-key` changes `.sops.yaml` before it reads the file
again ahead of its `.enc` writes.

The rule `!Secret.*.enc` re-includes each name that ends in `.enc`, and with
`core.ignorecase` set to true also a name that ends in `.ENC`. Thus `set`
refuses a key when the name of its plaintext file ends in `.enc` in any
letter case (for example the keys `enc`, `ENC` and `password.enc`), before
it writes a file. `decrypt` refuses an `.enc` file whose plaintext name
would end in `.enc` (for example `Secret.app.default.PW.ENC.enc`), before
its first plaintext write. lo cannot tell a plaintext with such a name from
a ciphertext: `encrypt`, `list` and `print` read it as a ciphertext, and
`decrypt` fails on it. Move such a file out of the store, and set the value
again with a key that does not end in `.enc`.

`set` writes its plaintext file to `Secret.<name>.<namespace>.<key>` at the
top of the store. Before it reads the value (from the argument, a prompt or
stdin) and before any write, it refuses a name, a namespace or a key that
contains a `/`, a `\` or a control byte, or that is `.` or `..`. Such a part
can make the file name a path into another directory: with a directory
`Secret.app.default.anchor/` in the store, the key `anchor/../.gitignore`
names the `.gitignore`. The error names the field and its text, never the
value. Letters, digits, `.`, `-` and `_` (the characters of the names and
keys of Kubernetes) pass, and so does each other byte.

`set` and `decrypt` write a plaintext file only to a regular file or to a
path that does not exist yet. When the plaintext path is a symlink (for
example, to an `.enc` file or to the `.gitignore`), a directory, a FIFO, a
socket or a device, or when its lookup fails, the command stops with an
error that names the path. It writes nothing there or through it. `decrypt`
checks the plaintext path of each `.enc` file before its first write, also
for a plaintext that it would keep. Remove the entry, then run the command
again. The plaintext goes to a new private temp file in the store (mode
0600), then a rename puts it at the plaintext path. Thus a hard link to the
old file keeps the old content. When the write fails, the command names the
plaintext file and removes the temp file, and the old file stays. The check
and the rename are two steps: lo does not stop a program that replaces the
path between them, and `set` and `decrypt` do not lock the store. The
binary syncs the store directory after a new `.gitignore` and before the
first plaintext, and again after the plaintext, so the order holds after a
power loss too. When a sync fails after a rename, the error says that the
file was written. In the bash implementation, `set` and `decrypt` need a
`mv` that takes `-T` (see the encrypt below), also without `--encrypt`. The
bash implementation does not sync, and its `decrypt` checks the entries once
before its first write; the binary checks each entry again at its write.

The secret generator of `lo build` writes its cache into the same store
with the same checks as `set`. It refuses a name, a namespace or a key with
a path part, a key whose plaintext name ends in `.enc` (in any letter case),
and a plaintext path that is not a regular file and not absent. Before it
writes a plaintext, it writes the block into the `.gitignore`, or it refuses
a `.gitignore` that cannot keep the plaintext out of git. For a value that
it makes on a cache miss, it does these checks before the generator runs:
for a refused key, no generator runs (a `bash:` command has side effects).
An `env:` entry and a `bash:` entry with `update: true` get their value
first and then write it: for them, the check refuses the write, and a
`bash:` command with `update: true` has run already. A refusal has the text
of `set`. It
names a file of a store `clusters/<domain>/secrets` relative to the project,
as `set` does, and a file of any other store relative to the parent
directory of the store. When a write fails, the error names the file and
no command.

Thus a build can fail where it passed before:

- The `.gitignore` of the store has a line that starts with `!` below the
  lok8s block, or anywhere in a file with no block. The build refuses each
  such line, also a line such as `!README.md` that matches no plaintext.
  Remove the line, or move the file out of the store, then build again. The
  other refusals of the `.gitignore` above apply too.
- A key ends in `.enc` (in any letter case). Before this check, the build
  wrote its plaintext, and the rule `!Secret.*.enc` let git commit it.
- The plaintext path is a symlink, a FIFO, a socket or a device. Before this
  check, the build replaced it.

A build that writes no plaintext (each value is in the store already, and
no entry has `update: true`) does not read the `.gitignore` and changes no
file of the store. A build that writes a plaintext into a store with no
`.gitignore` makes the file. Into a store whose `.gitignore` has no lok8s
block, it puts the block at the top of the file (in place of the old lines
of lo, if the file has them). Thus the committed `.gitignore` changes:
commit that change. A build still reads an entry whose name ends in `.enc`
that the store has already: a read writes nothing. The generator writes the
`.sha` file of a `bash:` entry and the `.bash-allow` file without these
checks: they hold hashes, not values.

A dry run (`lo bootstrap --dry-run`) writes no file into the store, and no
`.gitignore`. Before it generates or keeps a value, it does the same checks
as the build: it reads the `.gitignore` and does not write the block. Thus a
dry run refuses a key, an entry or a `.gitignore` that the build refuses,
with the same text. It cannot tell that a write of the build will fail (for
example, on a full disk).

The `.gitignore` of the store does not cover these cases:

- A `.gitignore` in a directory of the store has an effect only when git
  goes into that directory. The `*/` rule keeps git out of each directory of
  the store. The `!.gitignore` rule does not protect a file in a directory.

- `.git/info/exclude` and `core.excludesFile` have a lower precedence than
  the `.gitignore` of the store. A rule there cannot re-include a file that
  the store ignores.

- The rules keep an untracked plaintext out of git only. `git add -f` adds
  an ignored file, and git keeps a file that it tracks already: `set`,
  `decrypt` and a build write a tracked plaintext as any other, and git
  shows the change. No ignore rule stops these. Run `git ls-files` on the
  store to see the files that git tracks.

- lo trusts the store directory and its parent directories. It follows a
  symlink to the store or to a parent directory. The checks do not stop a
  program of the same user that replaces the store, a parent directory or
  the `.gitignore` during a write.

- lo checks only the files that it writes. A plaintext file that a user
  renames (for example, to a name that ends in `.enc`) is outside these
  checks.

A rewrite of the `.gitignore` goes to a temp file in the store and a rename,
so a hard link to the old file keeps its content. The new file gets mode
0644, also when the old file had another mode: the file is committed, each
user of the repository reads the same rules, and it holds no secret. When
the `.gitignore` is not a regular file (a symlink, a directory, a FIFO, a
socket or a device), the command stops before the step that the file
protects: remove it, then run the command again. When the write fails, the
command stops and names the file, and the old file stays.

`encrypt` skips a plaintext whose `.enc` file is newer. No command gives an
`.enc` file a time after the current time, and no command sets a time from
its own clock: each time comes from the clock of the kernel, which stamps
every write. A plaintext with a time in the future (after the current time)
stays the newer file: each encrypt writes its `.enc` file again and
prints a warning that names the plaintext. Set the time of that plaintext
to now (`touch` it). An `.enc` file that is older than its plaintext, with a
plaintext that is not after the current time, gets no warning: `lo lint`
names the secret, and the next `encrypt` writes the `.enc` file again. An
`.enc` file with a time in the future (for example,
from an older `lo`) stays the newer file until the clock passes that time:
`encrypt` skips its plaintext, also after a `touch` of the plaintext, and
`decrypt` writes the value of the `.enc` file over the plaintext. The
re-encrypt of `add-key` (in its own store) and of `remove-key` does not read
the times. Set the time of such an `.enc` file to a time before the last
edit of its plaintext.

Add a teammate with `lo secrets add-key`. It takes their age public key, or the
path to their ed25519 SSH public key. It adds the key to every creation rule in
`.sops.yaml`, then re-encrypts the store so that the new recipient can read it:

```bash
lo secrets --domain app.example.com add-key ~/keys/alice_ed25519.pub
```

`add-key` re-encrypts the store of the current domain, also with `--all`. It
re-encrypts every plaintext of that store, also one whose `.enc` file is
newer. The other stores of `--all` get a plaintext time of now, and a later
`lo secrets encrypt` of each domain re-encrypts them. That encrypt skips a
file whose `.enc` file has a time in the future, so such a file keeps its
old recipients. When
that store does not resolve (the domain directory has no `secrets` directory,
and the project keeps a store per domain), `add-key` stops before it changes
`.sops.yaml`. Create the directory, or select a domain that has a store, then
run the command again. When the command cannot list the store (it is not a
directory, you have no read or search permission on it, or the read of the
directory fails), it also stops before it changes `.sops.yaml`. The command
lists the store once, and its later steps use that list. With `--all`, it
lists each other store once. On the first run that is after the write of
`.sops.yaml`: when such a listing fails, the command names the store and
stops before it re-encrypts a file.

When `.sops.yaml` (at the end of its links, or through a hard link) is the
same file as the `.gitignore` of the store or as the `.enc` file of a
plaintext of the store, `add-key` stops before it changes `.sops.yaml`, the
`.gitignore` or an `.enc` file: the write of that file would replace
`.sops.yaml`. Only the lock file can be new. The check compares these files
only: an orphan `.enc` file, a plaintext and a file outside the store are
not compared. It runs once, before the first write. Give `.sops.yaml` a file
of its own.

`add-key` writes the `.gitignore` of the store before it changes
`.sops.yaml`. When that write fails, the command stops, and `.sops.yaml` and
the `.enc` files stay as they were.

When `.sops.yaml` lists the key already, `add-key` does not change
`.sops.yaml`: it finishes the re-key of an earlier run that stopped (for
example, after its write of `.sops.yaml`), as far as it can find that work.
It reads the sops metadata of the `.enc` file of each plaintext of the
store. A finished file passes the recipient envelope check: its metadata has
a `mac` and a `lastmodified` value, and an `age` entry for the key with an
`enc` value that is an armored age block (its header line and its footer
line). Only the `enc` values of the entries of the key take part in this
test. The check still reads every entry: an entry of another key whose
recipient is missing, empty or not a string, or a member that the metadata
holds twice, makes the file fail the check.

The command re-encrypts only the plaintexts whose `.enc` file is missing or
not finished. A finished `.enc` file keeps its bytes and its time, also when
its plaintext is older. When each file is finished, the command prints
`nothing to add`. It does not change `.sops.yaml` or an `.enc` file, but it
writes the `.gitignore` of the store when that file is missing or needs a
change. The check is structural: it reads
the sops metadata only, and does not decrypt the file. It does not show that
sops wrote the entry, or that the key can decrypt the file. A damaged file
whose metadata passes the check is not found.

After that re-encrypt, the command reads the metadata again. When a file is
still not finished, the command names it and stops with an error, and it
does not print `done`. The creation rule of the file may then not list the
key, for example when the key is only in a comment of `.sops.yaml`. Fix
`.sops.yaml`, then run the command again.

When `.sops.yaml` lists the key already, the command also stops, before it
changes `.sops.yaml`, the `.gitignore` or an `.enc` file, in these cases:

- An `.enc` path of the store is a symlink, a directory, a FIFO, a socket or
  a device. The first run refuses these too.
- An `.enc` file is larger than 16 MiB, the size limit of the metadata
  check. The command does not read the metadata of such a file. Restore or
  remove the file.
- An `.enc` file has no decrypted twin and does not pass the check, and you
  do not pass `--skip-orphans`. This is the refusal of the first run. An
  orphan that passes the check needs no re-key. An `.enc` symlink to a path
  that does not exist counts as an orphan too, on each run.
- The bash implementation cannot find `jq`, which reads the metadata. The
  `lo` binary needs no `jq`.

The first run (the key is not in `.sops.yaml` yet) re-encrypts every
plaintext of the store. A plaintext that is older than its `.enc` file (for
example, after a `git pull` that changed the `.enc` file) then replaces the
value of the `.enc` file. Run `lo secrets decrypt` before `add-key`.

Remove a recipient with `lo secrets remove-key`, for example when a teammate
leaves or a key is lost. It removes the key from every creation rule, then
re-encrypts every managed `.enc` file of the selected stores from its
plaintext, each with a new data key. A managed file is a `Secret.*.enc` file
at the top of a store. The labels are not trusted to choose the files: the
labels (the recipients that the sops metadata of a file lists) count only in
the summary and in the messages. Without `--all` the selected store is the
store of the current domain. With `--all` it is every
`clusters/<domain>/secrets` store. The command does not re-key the flat
`.secrets/` store. A warning names each other store that still lists the key:

```bash
lo secrets --domain app.example.com remove-key ~/keys/alice_ed25519.pub
lo secrets remove-key age1<recipient> --all     # every clusters/<domain>/secrets store
```

`remove-key` changes nothing when one of these is true:

- The key argument looks like a private key. The command does not print it.
  `add-key` refuses it too.
- `.sops.yaml` has a YAML tag under `creation_rules` other than `!!str`,
  `!!seq` or `!!map` (for example `!!binary`), or a mapping key that is not a
  string (for example `1:`, `true:` or `~:`). The command does not read such
  a value. Remove the tag, or quote the key.
- `.sops.yaml` holds the key in a place that the edit does not change: an
  unquoted list, an escaped or double-quoted string, a YAML list, `key_groups`,
  an alias or a merge key. Remove the key there by hand.
- The key is the last recipient of a creation rule. Nothing could then decrypt
  the files that the rule matches. Add another recipient first.
- The command cannot stat or read a store path, a directory in a store, or
  an `.enc` file. A symlink whose target is missing counts as a file that it
  cannot read.
- A managed `.enc` file that the command re-keys is a symlink. The command
  reads the file through the link, but the re-key does not write through a
  link. Replace the link with the file that it names, or remove it from the
  store. `add-key` refuses such a file too, before it changes `.sops.yaml`,
  and also an `.enc` path that is a directory, a FIFO, a socket or a device,
  or a path that it cannot look up (for example, a name that is too long).
- A selected store holds another `.enc` file: a name that ends in `.enc`, at
  any depth (in a subdirectory, or a name that starts with a dot), that is
  not a `Secret.*.enc` file at the top of the store. An example is a
  `backup.enc` copy of a managed file. The command re-keys only the managed
  files, so such a file can keep the removed key. Re-key it or remove it by
  hand. The command does not follow a symlink into a directory: a symlink
  whose name ends in `.enc` counts as such a file.
- The sops metadata of an `.enc` file does not parse, or lists no age
  recipient. The command reads the recipients of each file from that
  metadata, so a JSON escape cannot hide one. Fix or restore the file first.
- The sops metadata of an `.enc` file holds a member twice: the `sops` object
  itself, or a member in it (for example two `age` lists, or two `recipient`
  values in one entry). A JSON read keeps the last of two members, so the
  first one could hide a recipient from the check. Encrypt the file again
  with sops from its plaintext.
- An `.enc` file matches no creation rule, a rule with a recipient that is not
  age, or a rule with an age recipient that does not parse.
- An `.enc` file has no decrypted twin. The re-key cannot rewrite that file, so
  it would keep the removed recipient. Run `lo secrets decrypt` first, or pass
  `--skip-orphans` to accept it. With `--skip-orphans` the command names the
  files that still list the key.
- A managed `.enc` file holds other bytes than its decrypted twin. The re-key
  encrypts the twin, so it would replace the value of the `.enc` file. Run
  `lo secrets decrypt` for this store, or encrypt the twin first.
  `lo secrets decrypt` keeps a twin that is newer than its `.enc` file: to
  take the value of the `.enc` file, remove that twin first.
- The command cannot decrypt a managed `.enc` file, or cannot read its twin.
  Before the first write it decrypts each file that it re-keys with your age
  identity (`SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE`, or `LOK8S_SSH_KEY`) to
  compare it with its twin, so that identity must decrypt every managed file
  of the selected stores. The decrypted value stays in memory: the command
  does not print it or write it to a file.

Before the first write, the command warns when the key is your own decrypt
identity: the public key line of the sops age keys file (`SOPS_AGE_KEY_FILE`,
by default `~/.config/sops/age/keys.txt`), or the `.pub` of the SSH key that
`lo` decrypts with (`LOK8S_SSH_KEY`, by default `~/.ssh/id_ed25519`). After
the removal, that identity cannot decrypt the re-keyed files, so a resume of
a stopped run needs another identity that decrypts every file. Every run that
comes to this check prints a line that says which sources the check reads.
It does not read `SOPS_AGE_KEY`, `SOPS_AGE_KEY_CMD` or other files, so no
warning does not mean that the key is not yours.

After the re-key the command reads the selected stores again, with the same
reads and the same walk as before the first write. It fails when a file that
it re-keyed still lists the removed key, when it cannot read a path, and when
a file changed during the run: another `.enc` file, a managed `.enc` file with
a decrypted twin that it did not re-key, or, without `--skip-orphans`, an
`.enc` file with no decrypted twin. Each error names the path. If the re-key
stops after the `.sops.yaml` write (for example, when an encrypt fails or
this check fails), the command prints the command to run again. That run resumes the re-key.
The resume needs an identity that decrypts every managed file of the selected
stores, because it compares each file with its twin again.

The selected stores must be quiet while the command runs: no other command
or program may write to them. The lock (below) keeps a second `add-key` or
`remove-key` out. Other `lo` commands and other programs do not take it, and
a writer that does not take the lock is outside the guarantee below. The
check after the re-key does not see a file that is replaced in place, under
the same name, during the run.

When the key is in no creation rule and in no `.enc` label, the command
warns and goes on: it re-keys every managed `.enc` file and changes no rule.
A run that resumes a stopped re-key is such a run. A mistyped key is such a
run too: it re-keys every file and changes no rule. The cost is churn, a new
`.enc` file for each secret in the next commit.

When the command ends without an error, every managed `Secret.*.enc` file of
the selected stores is re-encrypted with a new data key, and none lists the
removed key. `--skip-orphans` is the one exception: an orphan is not
re-encrypted and keeps the removed key, so that key can still decrypt it. At
the end the command prints the number of creation rules that changed, of
`.enc` files whose labels listed the key, and of `.enc` files that it
re-keyed. Commit the `.enc` files together with `.sops.yaml`.

`add-key` and `remove-key` hold a lock on `.sops.yaml.lock`, next to
`.sops.yaml`, for the whole run, so that two runs do not undo each other.
`init` holds the same lock from its check of the path to the end of its
run: it writes the store, its `.gitignore` and `.sops.yaml` under it.
When `.sops.yaml` is a symlink, the lock is next to the file at the end of
its links, so two links to one file use one lock. The command finds that
file once, at the start: the lock and the edit of `.sops.yaml` (its read
and its write) use it, also when the link changes during the run. The
re-encrypt after the edit reads the creation rules through the `.sops.yaml`
path: when the link changes during the run, the re-encrypt uses the rules
of the file that the link names then. When the link does not name a regular
file, the command stops and changes nothing. A second run waits
`LO_SECRETS_LOCK_WAIT` seconds (60 by default), then stops
and changes nothing. The lock file holds no data. The command opens the lock
file for writing, and makes it when it is not there. The open fails when
there is no lock file and your account cannot write to its directory. It
also fails when the lock file exists and your account cannot write to it
(for example, a lock file of another user). Then the command stops and
changes nothing, also when there is nothing to add or remove. The error
names the lock file and its directory, and gives one next step for both
cases: "Make sure that your account can write to `<directory>` and to the
lock file, if it exists." A lock file that is there and that the command
can write opens also in a directory that it cannot write.
A lock path that is not a regular file (a symlink, a directory, a FIFO, a
socket or a device) stops the command before it opens the lock, and no open
empties the lock file. The `lo` binary opens the lock without following a
link and without a wait on a FIFO, and it stops when the opened file is not
a regular file. The bash implementation opens it with `>>`, which follows a
link that appears after that check: through a link to a path that does not
exist, it can make an empty file. It then stops because the open file is not
the lock path, and its error says to remove the lock path and the file that
a link there names. A FIFO that appears after the check makes the bash open
wait until a program opens the FIFO to read. When another regular file
replaces the lock file after the open, both stop with an error that says
that the lock path changed after its check, and that the command stopped
before it changed `.sops.yaml`, a `.gitignore` or an `.enc` file. The open
can have made the lock file. The bash implementation keeps
the lock until its process exits; the `lo` binary releases it when the
command ends. When `flock` fails for a cause other than a lock that another
run holds, the bash implementation says that it cannot lock the file and
gives the exit status of `flock`. The directory of the lock must
be one that you trust. The lock excludes runs only where no other user can
change the entries of that directory. When another user can write in that
directory (for example, the directory of the file that a `.sops.yaml` link
names), that user can remove or replace the lock file, and the lock gives
no exclusion. The lock belongs to the `.sops.yaml` file, not to a store:
each store that uses one `.sops.yaml` file uses the same lock, so a run for
one domain waits for a run for another. The `lo` binary locks on every
platform. The bash
implementation locks with `flock`: without it (macOS
has none by default), both commands stop before they change a file. Both
commands replace `.sops.yaml` through a temporary file and a rename. When
`.sops.yaml` is a symlink, they replace the file that it names, and the link
stays a link. Each
encrypt writes its `.enc` file the same way, so an encrypt that stops leaves
the old file whole, and a rerun of `add-key` finishes the work that it can
find (see the check above). This holds when a command stops or a signal
kills it, not for a power loss or a crash of the host: the commands do not sync the files
to the disk (no `fsync`), so after a power loss a write can be lost, or the
writes can be on the disk in another order.

An encrypt (`lo secrets set --encrypt`, `lo secrets encrypt` and the re-key of
`add-key` and `remove-key`) writes an `.enc` file only to a regular file or to
a path that does not exist yet. When the `.enc` path is a symlink (to a file or
to a directory), a directory, a FIFO, a socket or a device, or when the lookup
of the path fails for a reason other than "does not exist" (for example, a
name that is too long), the encrypt stops with an error that names the path,
and it writes nothing there or through it. A symlinked `.enc` file is not
followed and not replaced: remove the link, then encrypt again. `add-key` and
`remove-key` check the `.enc` paths that they re-key before they change
`.sops.yaml`, so such an entry stops them before the first write. The error
names each entry, and its last line says what to do with what it found:
replace each link, remove or fix each other entry, or both. The encrypt of
`add-key` reads the list of files that `add-key` checked: it does not list
the store again. The bash implementation needs GNU `mv`: it renames the new
file with `mv -T`, which never moves a file into a directory, or with `gmv -T`
when `mv` has no `-T` (the `mv` of BSD and macOS). Without one of them, each
command that encrypts stops before it writes a file, with an error that says
to install GNU coreutils or to use the `lo` binary. The `lo` binary needs no
`mv`.

These checks read the last part of the `.enc` path only. `lo` trusts the
store directory and its parent directories: they must belong to the user who
runs `lo`, and no other user may write to them. A symlink in a parent
directory is followed. A user who can replace a parent directory can move
the temporary file and the rename of an encrypt out of the store.

::: warning A removal does not revoke access
The removed key can still decrypt the older `.enc` files in the git history,
and its holder keeps every value that they decrypted before. After a removal,
rotate the secrets that the key could read.
:::

Scope the creation rules by path to give each environment **different**
recipients. See below.

## Per-environment isolation

The per-domain store isn't just organisation: it's the security boundary. A repo
driving more than one instance (dev and prod, or several tenants) must never let
them share a store:

- **No silent sharing.** Cache keys are `Secret.<name>.<ns>.<key>` with no
  environment in them, so a *single* flat store hands dev and prod the same value
  for a colliding (name, namespace), including the same *generated* password or
  master key, which nobody chose. Per-domain stores make that impossible.
- **No shared tier, on purpose.** There is no fallback from a domain's store to a
  shared one. A value genuinely needed in two instances is a deliberate manual
  copy (the operator consents to that exposure), never something a default did
  quietly. Prefer issuing a *separate* credential per instance: most providers
  can (a per-instance registry robot account, a project-scoped API token, …).
- **The real boundary is the decrypt key, not the folder.** Folders alone are
  cosmetic if one age key decrypts everything. Scope SOPS `creation_rules` by
  path so each environment encrypts to its **own** recipients. Then a dev/CI key
  cannot decrypt prod:

```yaml
# .sops.yaml — first match wins, so specific rules go BEFORE any catch-all
creation_rules:
  - path_regex: clusters/example\.com/secrets/Secret\..*
    age: 'age1prod…'                 # prod-key holders only
  - path_regex: clusters/.*/secrets/Secret\..*
    age: 'age1dev…,age1prod…'        # dev/CI (+ prod, who may read everything)
```

### Migrating a flat `.secrets/` store

Releases before v0.4.0 kept a flat `.secrets/` store at the project root. It
was the default `PATH_SECRETS` of the bash entrypoint. `lo up` also wrote the
registry TLS certificate there. The per-domain store is now the only secrets
store. The registry certificate is registry infrastructure, not a secret of a
domain. It lives in the docker volume `<network>-registry-tls` next to the
registries' data volumes (see
[TLS registries](/guide/shared-registries#tls-registries-default)).
Migrate an existing project in three steps:

1. Move each domain's cache files into its store, then encrypt them in place:

   ```bash
   mkdir -p clusters/<domain>/secrets
   git mv .secrets/Secret.<…>* clusters/<domain>/secrets/   # the keys that domain owns
   lo secrets --domain <domain> encrypt
   ```

2. Run `lo up`. The first run imports the registry certificate from
   `.secrets/tls/registries/` into the volume and prints one `[warn]` line: the
   running registry containers still mount the old directory.
3. Run `lo registry down && lo registry up`. The registries come back with the
   volume mounted. `lo registry tls status` shows `volume` for each container.
4. Add `clusters/*/.registry-tls-tmp.*/` to the project `.gitignore`. The mint
   writes its scratch store there. A mint that stops before its cleanup leaves
   the directory with the private key in it. New projects get the pattern from
   `lo init project`. `lo up` removes stale directories before each mint.

After step 4, lok8s reads nothing in `.secrets/`. Remove the directory.

Two `lo up` runs on the same registry network at the same time can collide on the helper container `<network>-registry-tls-io`: that run fails, and a retry succeeds because the read-out checks both files and mints again into an incomplete volume.

Anything two instances were *sharing* must be **re-issued per instance** (or, if
truly unavoidable, copied deliberately). The cleanest reset is to regenerate at
go-live: create the per-domain stores, drop the old flat cache, scope the
`.sops.yaml` rules to per-environment keys, and let the next build mint fresh,
isolated values.

`lo lint` helps enforce the split: it warns when a `Secret.*` cache entry exists
in **both** a domain's store and the flat `.secrets/` store, a deprecated
shadow. Identical copies are a stale duplicate to delete; **differing** copies
are active drift: different tools then read different stores, which can re-key a
live cluster from the wrong one. Keep domain secrets in the per-domain store
only.

## Using a secret

Mount the `Secret` as a **file** rather than injecting it via an env var: a
mounted file isn't exposed through `/proc`, crash dumps, or child processes, and
rotates without a pod restart. The mounted file contains exactly the generated
bytes.

## Sealing crown jewels (`immutable: true`)

For secrets whose **silent rotation would orphan stateful data** (an encryption
masterkey, a database password, a signing key), add `immutable: true` to the
spec. It passes through to the k8s Secret's native `immutable` field (stable
since 1.21), so the apiserver rejects any apply that would *change* the live
value instead of quietly re-keying a running system. Rotation then requires
an explicit delete + re-apply. See
[Sealing a secret](/reference/kustomize-plugins#sealing-a-secret-immutable) for
the full semantics and trade-offs.

## Trusting the dev CA (`lo trust`)

The [`cert:` generator](/reference/kustomize-plugins#development-certificates-cert)
signs development leaf certs (the app wildcard, registry TLS) with a local CA at
`$CAROOT` (default `~/.local/share/mkcert`), created on first use. Nothing trusts
that CA until you install it into your system + browser trust stores, once per
machine:

```bash
lo trust        # wraps `mkcert -install` (needs mkcert: b install mkcert)
```

`lo trust` is the **only** step that needs the `mkcert` binary: minting never
does. After it, browsers accept `https://*.<domain>` and the host Docker daemon
accepts pushes to TLS registries. `lo doctor` reports whether the CA is trusted.

## Inspecting

```bash
lo secrets --domain <domain> list             # what's in that domain's store
lo secrets --domain <domain> print [pattern]   # show value(s)
lo secrets --domain <domain> path              # the resolved store path for the context
```

(Omit `--domain` to act on the store named by `$PATH_SECRETS`.)

## See also

- [Kustomize Plugins → Secrets Generator](/reference/kustomize-plugins#secrets-generator):
  every generator type, charsets, cryptographic-key guidance.
