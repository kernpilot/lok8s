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
V=v0.3.0; A=lo-linux-amd64.tar.gz        # pick your tag and platform
curl -fsSLO "https://github.com/kernpilot/lok8s/releases/download/${V}/${A}"
curl -fsSLO "https://github.com/kernpilot/lok8s/releases/download/${V}/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf "${A}" lo && install -m 0755 lo ~/.local/bin/lo
```

Inside a lok8s project, `b` can install the same asset from `.bin/b.yaml`
(`github.com/kernpilot/lok8s` with `asset: lo-*.tar.gz`, alias `lo`) — the
`core` profile already declares it.

## What the binary needs

The Go `lo` runs every command itself. A project still carries the framework
tree (`.lok8s/`: addons, the Tilt extension, the provider plugins, the CAPI
templates, and the frozen bash implementation `LO_IMPL=bash` runs) and the
pinned toolchain the binary execs (kustomize, kind, Tilt, …), so a project
is bootstrapped the same way as before:

```sh
b env add github.com/kernpilot/lok8s#local && b install
```

See [docs/reference/go-migration.md](../docs/reference/go-migration.md) for
what the binary still execs, the `LO_IMPL=bash` escape hatch, and the parity
gates.

## Legacy: the argsh `lo-up` installer

The previous installer (`lo-up`, an argsh script bundled with its runtime and
published at `https://lok8s.io/lo-up`) is retired but not deleted: its source,
build script and runtime pin moved to
[`.lok8s/legacy/install/`](../.lok8s/legacy/install/README.md), and the
published bundle at `docs/public/lo-up` stays served for existing users. The
`loup-bundle` CI job still rebuilds and diffs it from the legacy path.

## Tests

`tests/unit/lo_install_test.bats` drives the script against a `file://`
fixture release (a fake `lo` archive + `checksums.txt`): a clean install, a
checksum mismatch (must install nothing), `--dry-run` (must fetch nothing),
and argument handling. `hack/lint-shell.sh` shellchecks `install/`.
