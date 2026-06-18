# lok8s troubleshooting — full symptom table

`Status`: **FIXED** = handled in current framework code (update lok8s if you still
hit it on an old build); **OPEN** = needs an operator workaround; **HOST** =
machine/host config, not lok8s. File refs are under `.lok8s/` unless noted.

| # | Symptom | Cause | Fix / workaround | Status |
|---|---------|-------|------------------|--------|
| 1 | `ImagePullBackOff`; containerd `:443 connection refused` after re-`lo up` | the kind node's bind-mounted `certs.d` got a new inode while the node still mounted the old one | refresh in place (current code); on an old build `docker restart <node>` | FIXED (`drivers/lo/.../render.sh`) |
| 2 | `error 20: unable to get local issuer certificate` | `lo::mkcert` only writes the leaf when absent — doesn't re-issue when the mkcert root CA rotates | `rm .secrets/tls/tls.*` then re-run `lo up` | OPEN (`drivers/lo/utils/services.sh`) |
| 3 | literal `${LOK8S_USER_API_HOST}/32` → invalid CIDR | a build path skipped `envsubst` | use `lo build` (substitutes via the kubeconfig API host); `lo build --split` still doesn't envsubst — avoid for templated targets | FIXED for `lo build` / PARTIAL for `--split` (`libs/build`) |
| 4 | new pods `ImagePullBackOff` on `lok8s.local/<svc>:latest` after `kubectl apply -f artifacts.yaml` | the bare image ref is rewritten to the pushed tag only by **Tilt** at apply time | deploy via Tilt/`lo`, not a raw apply; or `kubectl set image` to the pushed tag | OPEN (no raw-apply guard) |
| 5 | `lo up` exits 1 in CI / non-TTY though Tilt deployed fine | `lo up` backgrounds interactive `tilt up` (TTY-bound) | **`lo up --ci`** → foreground `tilt ci`, real exit code (`--timeout`) | FIXED (`lo`, `libs/tilt`) |
| 6 | Tiltfile reload crashes `lstat … no such file or directory` | `live_update` `sync.local_path`/`fall_back_on.files` resolved via `realpath` on a missing path | list only existing paths | OPEN (`tilt/Tiltfile`) |
| 7 | `docker push … connection refused` during `tilt ci` (pulls work) | host `daemon.json` `insecure-registries` CIDR doesn't cover the lok8s registry IPs | widen the CIDR (e.g. `10.125.0.0/16`) + restart dockerd | HOST (lok8s' own cloud-init template is correct) |
| 8 | `lo build`/`deploy` exits 1 with NO output for a deploy (`.cloud`) domain | a bare `yq` on the missing `cluster.lok8s.yaml` aborted under `set -euo pipefail`, stderr swallowed | guarded + `<metadata.name>.yaml` kubeconfig fallback added | FIXED (`libs/build`) |
| 9 | `lo up` aborts at coredns: `tmp: unbound variable` | a `RETURN` trap re-fired on the caller's return under `set -u` | trap removed; explicit cleanup | FIXED (`drivers/lo/utils/services.sh`) |
| 10 | gateway LoadBalancer stuck `<pending>` after a clean recreate | `coredns-external` grabbed `pool[0]` before MetalLB, racing the Envoy gateway pinned to it | `coredns-external` now annotated to the pool's LAST IP | FIXED (`services.sh`) |
| 11 | custom CoreDNS records (`*.<domain>→gateway`) lost on every `lo up` | the driver re-applied a static Corefile each up | declarative `coredns-custom` ConfigMap from `spec.coredns.*`, re-applied each up | FIXED |
| 12 | a `bash:`/`file:` secret keeps emitting a stale value after the file changed | the secrets cache keys on the command/path string, not the file contents | delete `$PATH_SECRETS/Secret.<name>.<ns>.<key>` and rebuild | OPEN (by design; see `lok8s-secrets`) |
| 13 | secret looks like bad creds right after `lo secrets set` | `lo secrets set` writes the cache, not the live in-cluster Secret | rebuild + `kubectl apply` the target afterward | OPEN |
| 14 | per-service `lok8s.yaml` `fail()`s deep in `tilt up`; people invent `kind:` | it's a bare untyped object, unlike other lok8s specs | `lo lint` now schema-checks it + `lo init service` scaffolds it; multi-image `components:` is first-class | MOSTLY FIXED (`libs/lint`) |
| 15 | khelm-rendered chart lands in `default` ns → `FailedMount` / wrong-ns operator | the chart omits `metadata.namespace` and relies on `helm -n` | put the chart in its own sub-kustomization with `namespace: <ns>` | OPEN (per-chart pattern) |
| 16 | khelm-rendered Helm **hook** Jobs run on install / aren't idempotent | kustomize has no Helm hook engine — hooks emit as ordinary Jobs | disable uninstall-only hooks, tolerate ordering (backoffLimit); `kubectl delete job` before re-apply | OPEN (per-chart pattern) |
| 17 | vendored `.lok8s` lags the pinned lok8s ref in a consuming repo | consumers `load()` a committed vendored copy; no one-shot re-sync | rsync + commit the framework copy (a `b env sync` one-command + CI lag check are proposed) | OPEN |

Items 7, 15, 16 are host/per-chart patterns rather than framework bugs, but they
recur in the lok8s workflow so they're indexed here. When a row is FIXED but you
still hit it, you're on an older framework build — update the vendored `.lok8s`.
