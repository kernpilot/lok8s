---
name: lok8s-doctor
description: >-
  Use when a lok8s command fails or behaves oddly — `lo up`/`build`/`deploy`
  errors, ImagePullBackOff, TLS/cert failures, registry push refusals, Tiltfile
  reload crashes, stale secrets, or a CI/headless bring-up. Maps symptoms to
  cause and fix. See reference.md for the full table.
---

# lok8s troubleshooting (symptom → cause → fix)

First, gather context: `lo status <domain>`, `lo lint <domain>`, and (for the dev
loop) the Tilt logs. Then match the symptom below; the **full table with file
citations and FIXED/OPEN status is in [`reference.md`](reference.md)**.

## Most common

| Symptom | Cause → Fix |
|---------|-------------|
| **`ImagePullBackOff`, containerd `:443 connection refused`** after re-`lo up` | The kind node's `certs.d` bind-mount went stale. **Fix:** `docker restart <node>` (current builds refresh in place, so update lok8s if it recurs). |
| **`lo up` exits 1 in CI / non-TTY** though Tilt started | `lo up` backgrounds interactive `tilt up`. **Fix:** use **`lo up --ci`** (foreground `tilt ci`, exits with the real build+deploy status; add `--timeout 600s`). |
| **`error 20: unable to get local issuer certificate`** (dev TLS) | mkcert root CA rotated but the committed leaf wasn't re-issued. **Fix:** `rm .secrets/tls/tls.*` and re-run `lo up`. |
| **Literal `${LOK8S_USER_API_HOST}` / invalid CIDR** in applied manifests | a build path that skipped `envsubst`. **Fix:** use `lo build` (it substitutes); avoid `lo build --split` for templated targets until consolidated. |
| **`lo build`/`deploy` exits 1 with NO output** on a `.cloud`/deploy domain | older silent-fail on the missing `cluster.lok8s.yaml`. **Fix:** update lok8s (guarded + kubeconfig fallback added); ensure the deploy domain's `clusterRef.domain` resolves. |
| **New pods `ImagePullBackOff` on `lok8s.local/<svc>`** after a raw `kubectl apply -f artifacts.yaml` | the bare image ref is only rewritten by Tilt at apply time. **Fix:** deploy via Tilt/`lo`, not a raw apply of dev artifacts (or `kubectl set image` to the pushed tag). |
| **`docker push … connection refused`** during `tilt ci` (pulls work) | host `/etc/docker/daemon.json` `insecure-registries` CIDR doesn't cover the lok8s registry range. **Fix:** widen it (e.g. `10.125.0.0/16`) + restart dockerd. |
| **Tiltfile reload crashes: `lstat … no such file or directory`** | a `live_update` `sync.local_path`/`fall_back_on.files` entry points at a missing path. **Fix:** list only paths that exist. |
| **A `bash:`/`file:` secret keeps serving a stale value** after the file changed | the secrets cache keys on the command/path string, not contents. **Fix:** delete `.secrets/Secret.<name>.<ns>.<key>` and rebuild. |
| **A secret looks like bad creds right after `lo secrets set`** | `lo secrets set` writes the cache, not the live Secret. **Fix:** rebuild + re-apply the target. |
| **khelm chart lands in the `default` namespace** | the chart omits `metadata.namespace` and relies on `helm -n`. **Fix:** render it in its own sub-kustomization with `namespace: <ns>`. |

## General diagnostics
```bash
lo status <domain>     # cluster + nodes + target builds + Tilt
lo lint <domain>       # spec / bootstrap / services.yaml / lok8s.yaml / apex checks
lo tilt status         # `tilt doctor` (must report Env: kind)
kubectl get events -A --sort-by=.lastTimestamp | tail -30
```

For the complete table (incl. `lo up` coredns crashes, gateway LB `<pending>`,
Helm-hook Job idempotency, the vendored-`.lok8s` drift, and which items are
already FIXED in the framework vs. an operator workaround) see `reference.md`.
