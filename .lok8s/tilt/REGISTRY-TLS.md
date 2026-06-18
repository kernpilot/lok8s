# Registry TLS via mkcert — IMPLEMENTED

**Status:** Implemented (2026-06-16). The deferred design has shipped.

`spec.registries.tls: true` makes every lok8s registry (framework-private
`build`/`cache` **and** the pull-through mirrors) serve HTTPS with a
mkcert-signed certificate, so the host Docker daemon no longer needs an
`insecure-registries` entry to push to them.

## What shipped

| Knob | Default | Effect |
|------|---------|--------|
| `spec.registries.tls` | `false` | `true` → HTTPS registries on `:443`, mkcert-signed |

- **Cert generation** — `lo::mkcert_registries`
  (`drivers/lo/utils/services.sh`) generates one cert into
  `.secrets/tls/registries/{tls.crt,tls.key}` whose SANs are built
  dynamically from `.registries.json`: every registry IP plus the
  framework hostnames `lok8s.local` / `lok8s.cache` (and each mirror's
  impersonated domain). Regenerated only when the SAN set changes
  (a `.sans` sidecar records what the cert was built for). Reuses the
  mkcert root the application wildcard cert already uses.
- **Listen port** — TLS registries listen on **`:443`** (not `:80`).
  This is the one correction to the original spec: the Docker client's
  TLS-vs-insecure decision is hostname-driven, but the *port* for a
  bare-IP `docker push <ip>/...` defaults to 443. Serving on 443 lets
  raw-IP pushes work over HTTPS with no port in the ref and no
  `insecure-registries`. (`LO_REGISTRY_PORT` / `LO_REGISTRY_PORT_TLS`
  in `defaults.sh`; recorded as `port` in `.registries.json`.)
- **Registry containers** — `lo::registries`
  (`drivers/lo/utils/registries.sh`) renders the registry config's
  `http:` block for the active mode (`lo::render_registry_config` swaps
  the plain `:80` block for a `:443` + `tls:` block) and mounts
  `.secrets/tls/registries` read-only into each container.
- **Containerd trust** — `lo::write_certs_d` (`drivers/lo/utils/render.sh`)
  writes `server = "https://<ip>"` + `ca = "/etc/containerd/certs.d/.ca/rootCA.pem"`
  (no `skip_verify`) and copies mkcert's `rootCA.pem` into the
  bind-mounted `certs.d/.ca/` tree. Plain mode keeps `http://` +
  `skip_verify`.
- **Host Docker trust** — relies on `mkcert -install` having placed the
  mkcert root CA in the system trust store. `docker push` to the
  registry IP (or `lok8s.local`/`lok8s.cache`) validates over HTTPS with
  no daemon config.
- **`image::_cache_one`** (`libs/image`) drops `--insecure` from
  `docker manifest inspect` in TLS mode (`image::_registry_tls` reads
  `.registries.json`); `image::list` and `lo registry status` use
  `https://` URLs in TLS mode.
- **Provision order** — `lo::mkcert_registries` runs before
  `lo::registries` (containers mount the cert) and `lo::write_certs_d`
  (references the CA). See `drivers/lo/main`.

## Prerequisite (host-level, cannot be automated)

`mkcert -install` must have been run once on the host. Installing a CA
into the system trust store needs the user's own privileges/consent, so
lok8s cannot do it. `lo provision`/`lo up` fail fast with a clear message
if `tls: true` but mkcert is unavailable or the CA isn't installed.
`~/.local/share/mkcert/rootCA.pem` is the trust anchor for both the host
Docker client and containerd inside the kind nodes.

## Back-compat

`tls` defaults to `false`. Existing clusters keep the plain-HTTP `:80`
registries reached via raw IP, which still need the registry IP range in
the host's `insecure-registries`. TLS is strictly opt-in.

## Verification

End-to-end verified on 2026-06-16 against the running kubehz host:

- `lo::mkcert_registries` → cert with 6 SANs (hostnames + IPs).
- `lo::registries` → containers serving `listening on [::]:443, tls`.
- `lo::write_certs_d` → `https://` + `ca=` `hosts.toml`, rootCA copied.
- `curl https://<build-ip>/v2/` with system trust → **200** (no `-k`).
- `docker push <build-ip>/...` over HTTPS → **Pushed**, with the IP range
  **not** in `insecure-registries` (real cert validation).
- containerd `crictl pull https://<ip>/...` on a live kind node, trusting
  the mkcert CA via `certs.d` → **succeeded**.

Tests: `tests/unit/registry_tls_test.bats` (17 cases — config parsing,
JSON fields, query helpers, config-block rendering, certs.d output,
SAN-list building, fail-fast on missing mkcert, image-lib detection).

## User-facing docs

- Guide: `docs/guide/shared-registries.md` → "TLS registries
  (no `insecure-registries`)".
- Reference: `docs/reference/specs.md` → "Registry TLS (mkcert)";
  `docs/reference/kind-contract.md` → provision order + containerd wiring.
