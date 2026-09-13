# .archive — retired code kept for reference

> **Deprecated. Scheduled for deletion.** Nothing in this directory is part
> of the `lo` binary or of the frozen bash reference under `.lok8s/`. It is
> kept so that the history of a retired path stays readable and so that the
> published artifacts it produced can still be rebuilt byte-exact. When the
> last consumer of those artifacts is gone, the owner deletes the directory.

## What is here

| Path | What it was | Replaced by | Why it is still here |
|---|---|---|---|
| `legacy/install/` | `lo-up`, the argsh-bundled installer, its `build` script, the minified template and the argsh revision pin | `install/lo-install.sh` and the goreleaser release archives | `docs/public/lo-up` is still published behind the `get.lok8s.io` redirect for projects that adopted it. The `loup-bundle` CI job and `tests/unit/loup_*_test.bats` rebuild and diff it from this source. |
| `legacy/operator/hooks/` | The original shell-operator hook bodies (`lo-reconcile`, `capi-reconcile`, `capi-status-sync`, `runtime`) | `lo operator <hook>` in `internal/operator/`; the files under `operator/hooks/` are two-line shims | `tests/operator/hooks_test.bats` and `hack/parity-operator.sh` diff the Go hooks against these bodies. The operator image copies them to `/hooks/legacy/` for the same reason. |

## Rules

- Bug fixes only, and only when a test or a harness would otherwise go red.
- Nothing new is added here. A file retired from outside `.lok8s/` moves
  here (`git mv`), it is never deleted in the same change.
- Deleting this directory is an owner decision. Before that: repoint the
  `get.lok8s.io` redirect to `lo-install.sh`, drop the `loup-bundle` job
  and the two `loup_*` unit tests, drop the `/hooks/legacy/` copy from
  `operator/Dockerfile`, and retire the hook parity cases.

History: retired from `.lok8s/legacy/` to `.archive/legacy/` on 2026-09-13
(owner decision: the bash reference under `.lok8s/` holds only the live
frozen implementation, retired code lives at the repository root).
