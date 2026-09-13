# Registry TLS — IMPLEMENTED

**Status:** Implemented 2026-06-16 (mkcert). Migrated 2026-06-21 to the Secret
plugin and made the default; see "What shipped".

Every lok8s registry (framework-private `build`/`cache` **and** the pull-through
mirrors) serves HTTPS **by default**, so the host Docker daemon does not need an
`insecure-registries` entry to push to them. The cert is minted by the
`secrets.lok8s.dev` **Secret plugin** (the `cert:` generator — the binary lok8s
already ships), signed by the shared dev CA at `CAROOT`. **No `mkcert` binary is
needed to mint** (the CA is created on demand); mkcert is only one of the ways to
*trust* the CA for host pushes.

## What shipped

| Knob | Default | Effect |
|------|---------|--------|
| `spec.registries.tls` | `true` | HTTPS registries on `:443`, cert minted by the Secret plugin. `false` → plain HTTP `:80` (needs `insecure-registries`) |

- **Cert minting** — `lo::registries_tls_cert` (`drivers/lo/utils/registries.sh`)
  builds the SAN list from `.registries.json` (every registry IP plus the
  framework hostnames `lok8s.local` / `lok8s.cache` and each mirror's impersonated
  domain), pipes a `cert: {hosts: […]}` Secret manifest through the Secret plugin,
  and extracts `tls.crt`/`tls.key` (base64-decoded from the emitted Secret) into
  `.secrets/tls/registries/`. Re-minted only when the SAN set changes (a `.sans`
  sidecar records it; the plugin's name-keyed cache entry is dropped first to
  force a fresh signature). No `mkcert`/`certgen` binary.
- **Listen port** — TLS registries listen on **`:443`** (not `:80`), so a bare-IP
  `docker push <ip>/…` (which defaults to 443) reaches them with no port in the
  ref and no `insecure-registries`. (`LO_REGISTRY_PORT` / `LO_REGISTRY_PORT_TLS`
  in `defaults.sh`; recorded as `port` in `.registries.json`.)
- **Registry containers** — `lo::registries` renders the registry config's
  `http:` block for the active mode (`lo::render_registry_config` swaps the plain
  `:80` block for a `:443` + `tls:` block) and mounts `.secrets/tls/registries`
  read-only into each container.
- **Containerd trust** — `lo::write_certs_d` (`drivers/lo/utils/render.sh`) writes
  `server = "https://<ip>"` + `ca = "/etc/containerd/certs.d/.ca/rootCA.pem"` (no
  `skip_verify`) and copies the dev `rootCA.pem` (resolved from `CAROOT`,
  binary-free) into the bind-mounted `certs.d/.ca/` tree. In-cluster pulls trust
  the registries **without** any host trust step.
- **Host Docker trust** — host `docker push` validates against the host trust
  store, so the dev CA must be trusted there. `lo::registries_tls_nudge` warns
  (non-fatally) at the end of provision when it isn't. Three ways (see the guide):
  `lo trust` (system + browser-wide, recommended), `insecure-registries` (skip
  verification), or a per-registry `certs.d` CA (rootful Docker = `sudo`; rootless
  Docker / Podman = under `$HOME`, no `sudo`).
- **`image::_cache_one`** (`libs/image`) drops `--insecure` from
  `docker manifest inspect` in TLS mode (`image::_registry_tls` reads
  `.registries.json`); `image::list` and `lo registry status` use `https://` URLs.
- **Provision order** — `lo::registries_tls_cert` runs before `lo::registries`
  (containers mount the cert) and `lo::write_certs_d` (references the CA). See
  `drivers/lo/main`.

## Host push trust (one-time, pick one)

In-cluster pulls need nothing. Only host `docker push` (the Tilt build loop) must
trust the cert, because the host Docker daemon validates against its own store:

1. **`lo trust`** — wraps `mkcert -install`; installs the dev CA system + browser
   wide (also covers application `*.<domain>` TLS). Needs `sudo` once. Recommended.
2. **`insecure-registries`** — add the registry IP range to `daemon.json`; skips
   verification (unverified pushes). Least preferred.
3. **Per-registry CA / rootless runtime** — drop `$CAROOT/rootCA.pem` at the
   daemon's `certs.d/<registry>/ca.crt`: rootful Docker `/etc/docker/certs.d` =
   `sudo`; rootless Docker / Podman keep it under `$HOME` = no `sudo`.

Installing a CA needs the user's own privileges/consent, so lok8s can't fully
automate it. If `tls: true` but the Secret plugin isn't built, `lo provision`
fails fast.

## Verification

Originally end-to-end verified 2026-06-16 (mkcert impl) on the kubehz host:
`docker push <build-ip>/…` over HTTPS → **Pushed**, IP range **not** in
`insecure-registries` (real cert validation); containerd `crictl pull` on a live
kind node, trusting the CA via `certs.d` → **succeeded**.

After the 2026-06-21 migration the cert is minted by the Secret plugin instead;
verified the plugin path mints a leaf chaining to the CAROOT CA with the right
SAN classification (DNS vs IP) and byte-identical reuse. The `:443` listen,
`certs.d` wiring, and containerd trust are unchanged.

Tests: `tests/unit/registry_tls_test.bats` (20 cases — config parsing incl. the
default, JSON fields, query helpers, config-block rendering, certs.d output,
Secret-plugin-driven minting + extraction, re-mint skip, fail-fast on missing
plugin, the untrusted-CA nudge, image-lib detection).

## User-facing docs

- Guide: `docs/guide/shared-registries.md` → "TLS registries (default)" +
  "Host push trust options".
- Reference: `docs/reference/specs.md` → "Registry TLS";
  `docs/reference/kind-contract.md` → provision order + containerd wiring.
