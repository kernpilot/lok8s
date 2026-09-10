# lo-up — the LEGACY lok8s installer

> **Deprecated.** `lo-up` is the installer from before the Go binary. Use
> [`install/lo-install.sh`](../../../install/lo-install.sh) instead: it
> downloads a release archive over HTTPS, verifies its checksum and only then
> installs `lo` — nothing remote is piped into a shell. `lo-up` stays
> published at https://lok8s.io/lo-up for projects that already use it and
> prints this notice when it runs; it gets bug fixes only.
>
> The `curl … | sh` lines below document what existing users run. New
> setups must not adopt them (AGENTS.md: never pipe remote content into a
> shell). If you still need `lo-up`, download it, read it, then run it:
> `curl -fsSL https://lok8s.io/lo-up -o lo-up && less lo-up && sh lo-up`.

A single, self-contained script (the argsh runtime is bundled) that bootstraps
or updates a lok8s project's environment. Published at
**https://lok8s.io/lo-up**, behind the `get.lok8s.io` redirect.

## Use (legacy)

```sh
curl -fsSL https://get.lok8s.io | sh            # interactive when a TTY is present
curl -fsSL https://get.lok8s.io | sh -s -- -y   # unattended (CI)
```

It auto-detects where it runs:

- **bootstrap** (fresh directory): installs `b` if missing, runs
  `b env add github.com/kernpilot/lok8s#<profile> --version <ref>`, then `b install`.
- **update** (a `.lok8s/` is already present): `b install`.

It also copies itself into the project's `.bin` (`PATH_BIN`), so a later
`.bin/lo-up` updates in place.

Flags: `-y`/`--non-interactive`, `-p`/`--profile` (`core|kustomize|local|capi|kubeone`,
default `local`), `-r`/`--git-ref` (default `main`), `-d`/`--dir`.

## Build

Edit `.lok8s/legacy/install/lo-up`, then rebuild the published bundle:

```sh
./.lok8s/legacy/install/build          # → docs/public/lo-up
```

`build` needs the argsh runtime (`libraries/*.sh`) **and** the `minifier`
binary — the sibling `arg-sh/argsh` checkout provides both, or set
`ARGSH_SRC=/path/to/arg-sh/argsh`. The bundle (`docs/public/lo-up`) is committed
because it is the published artifact; rebuild after every edit to `lo-up`.

### The runtime is pinned

The bundle embeds argsh's version and commit, so it is only reproducible
against one runtime revision. `.lok8s/legacy/install/argsh.pin` records it and `build`
refuses any other checkout. That is what lets CI rebuild and diff: the
`loup-bundle` job checks argsh out at the pin, downloads the pinned `minifier`
release asset, runs `.lok8s/legacy/install/build`, and fails on
`git diff --exit-code -- docs/public/lo-up`. Before the pin existed nothing in
CI noticed a stale bundle, and every `curl … | sh` user kept getting the old
script.

The pinned commit is on argsh's `feat/process-trace-phase2` branch, not on
`main`. A force-push or a deletion of that branch makes the commit unreachable
and the `loup-bundle` job then fails while CHECKING OUT argsh, before it builds
anything — a "could not find the ref" error that says nothing about this pin.
`.lok8s/legacy/install/argsh.pin` repeats the warning next to the value.

To move to a newer argsh:

```sh
git -C /path/to/arg-sh/argsh checkout <new-ref>
ARGSH_PIN_UPDATE=1 ARGSH_SRC=/path/to/arg-sh/argsh ./.lok8s/legacy/install/build
```

Commit `.lok8s/legacy/install/argsh.pin` and `docs/public/lo-up` together — a unit test
compares the pin against the runtime baked into the bundle, so a bump without a
rebuild fails even where the byte-exact job does not run.

## How the bundle works

`.lok8s/legacy/install/lo-up.min.tmpl` wraps the minified `argsh runtime + lo-up` with a POSIX
`/bin/sh` preamble that re-execs under bash from a real file — so `curl … | sh`
works even where `/bin/sh` is dash, or when the script arrives on a stdin pipe
(where `${BASH_SOURCE[0]}` is unset under `set -u`). Two gotchas the build
handles:

- **Dispatch** is lo-up's own `… || main "$@"` tail; the template does *not*
  append `argsh::shebang` (that re-dispatches after `main` returns and trips on
  an obfuscated unbound under `set -u`).
- **Obfuscation** must skip the variables `:args` addresses by literal name —
  the spec array `args` and each flag's destination var — via the build's `-i`
  list. Note `ref` is a reserved argsh nameref, so the flag's variable is
  `git_ref` (`--git-ref`).
