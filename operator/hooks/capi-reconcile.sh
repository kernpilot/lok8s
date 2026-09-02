#!/usr/bin/env bash
# shell-operator hook shim — the Capi CRD reconciler is `lo operator
# capi-reconcile` (internal/operator/capi.go). shell-operator discovers hooks
# by path, so this file keeps its name; `--config` and the binding context
# pass straight through. The original bash body is frozen at
# .lok8s/legacy/operator/hooks/capi-reconcile.sh.
exec lo operator capi-reconcile "$@"
