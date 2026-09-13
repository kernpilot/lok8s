# lok8s Tilt extension

This directory contains the lok8s Tilt extension — a Starlark extension
that the project's root `Tiltfile` loads via:

```python
load('./.lok8s/tilt/Tiltfile', 'lok8s')
lok8s()
```

## Layout

- `Tiltfile` — the extension entry point. Defines the `lok8s()` function
  consumers call from their root `Tiltfile`. Reads `services.yaml` (and
  optionally `services.local.yaml`), wires up `docker_build`, `k8s_yaml`,
  `live_update`, etc.

## Upstream PR plan

The extension is structured to be PR-compatible with the [official Tilt
extensions registry](https://github.com/tilt-dev/tilt-extensions). Each
extension there lives at `tilt-extensions/<name>/Tiltfile`, so our
`Tiltfile` here can be moved verbatim to `tilt-extensions/lok8s/Tiltfile`
when it's polished enough to upstream. After upstreaming, consumers
would replace the local `load(...)` with:

```python
load('ext://lok8s', 'lok8s')
lok8s()
```

Until then, the extension lives in-tree and is synced into consumer
repos via the lok8s `tilt` profile (`b env add github.com/kernpilot/lok8s#tilt`).
