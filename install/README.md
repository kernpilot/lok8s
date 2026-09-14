# install/ — the `lo` binary installer

`lo` ships as a single static Go binary per platform (linux/darwin ×
amd64/arm64), built and attached to every GitHub release by goreleaser
(`.goreleaser.yaml`, `.github/workflows/release.yml`). Every release carries a
`checksums.txt` (SHA-256) covering all assets, including this installer.

## Install

Download, verify, then run. Nothing here is meant to be piped into a shell.

```sh
curl -fsSLO https://github.com/kernpilot/lok8s/releases/latest/download/lo-install.sh
curl -fsSLO https://github.com/kernpilot/lok8s/releases/latest/download/checksums.txt
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt
less lo-install.sh                              # read what it does
bash lo-install.sh                              # → ~/.local/bin/lo
```

Flags: `--version <tag>` (default: latest release), `--dir <path>` (default
`~/.local/bin`), `--dry-run` (resolve and print, touch nothing). Environment:
`LO_VERSION`, `LO_INSTALL_DIR`, `LO_INSTALL_REPO`, `LO_INSTALL_BASE_URL`.

The script downloads `lo-<os>-<arch>.tar.gz` **and** `checksums.txt` from the
chosen release, refuses to extract anything whose SHA-256 does not match, and
only then copies `lo` into place.

Without the script — the same four steps by hand:

```sh
V=v0.4.0; A=lo-linux-amd64.tar.gz        # pick your tag and platform
curl -fsSLO "https://github.com/kernpilot/lok8s/releases/download/${V}/${A}"
curl -fsSLO "https://github.com/kernpilot/lok8s/releases/download/${V}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf "${A}" lo && install -m 0755 lo ~/.local/bin/lo
```

Inside a lok8s project, `b` can install the same asset from `.bin/b.yaml`
(`github.com/kernpilot/lok8s` with `asset: lo-[^f]*.tar.gz`, alias `lo`) — the
`core` profile already declares it. The glob leaves out `lo-full-*`: b scores
the two archives the same, so a plain `lo-*.tar.gz` can install `lo-full`.

## What the binary needs

The Go `lo` runs every command itself. A project still carries the framework
tree (`.lok8s/`: addons, the Tilt extension, the provider plugins, the CAPI
templates, and the frozen bash implementation a project can route commands to) and the
pinned toolchain the binary execs (kustomize, kind, Tilt, …), so a project
is bootstrapped the same way as before:

```sh
b env add github.com/kernpilot/lok8s#local && b install
```

See [docs/reference/go-migration.md](../docs/reference/go-migration.md) for
what the binary still execs, how a project chooses the implementation, and the parity
gates.

## On the docs site

`docs/public/lo-install.sh` is a committed copy of this script. VitePress
copies `docs/public/` into the site root, so the same installer is served at
`https://lok8s.io/lo-install.sh`, next to the legacy `lo-up`.
`tests/unit/lo_install_public_test.bats` fails when the copy differs from
`install/lo-install.sh` by one byte. After an edit here, refresh it:

```sh
cp install/lo-install.sh docs/public/lo-install.sh
```

The release asset is the copy `checksums.txt` covers. Point users at the
release page for the verified download; the site copy is a convenience.

## Legacy: the argsh `lo-up` installer

New installs use `lo-install.sh`. The previous installer (`lo-up`, an argsh
script bundled with its runtime and published at `https://lok8s.io/lo-up`) is
retired but not deleted: its source, build script and runtime pin moved to
[`.archive/legacy/install/`](../.archive/legacy/install/README.md), and the
published bundle at `docs/public/lo-up` stays served for existing users. It
does not install the Go binary. The `loup-bundle` CI job still rebuilds and
diffs it from the legacy path.

## Tests

`tests/unit/lo_install_test.bats` drives the script against a `file://`
fixture release (a fake `lo` archive + `checksums.txt`): a clean install, a
checksum mismatch (must install nothing), `--dry-run` (must fetch nothing),
and argument handling. `hack/lint-shell.sh` shellchecks `install/`.
