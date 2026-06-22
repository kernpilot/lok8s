# lo-up — the lok8s installer

A single, self-contained script (the argsh runtime is bundled) that bootstraps
or updates a lok8s project's environment. Published at
**https://lok8s.io/lo-up**, behind the `get.lok8s.io` redirect.

## Use

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

Edit `install/lo-up`, then rebuild the published bundle:

```sh
./install/build          # → docs/public/lo-up
```

`build` needs the argsh runtime (`libraries/*.sh`) **and** the `minifier`
binary — the sibling `arg-sh/argsh` checkout provides both, or set
`ARGSH_SRC=/path/to/arg-sh/argsh`. The bundle (`docs/public/lo-up`) is committed
because it is the published artifact; rebuild after every edit to `lo-up`.

## How the bundle works

`install/lo-up.min.tmpl` wraps the minified `argsh runtime + lo-up` with a POSIX
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
