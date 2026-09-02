#!/usr/bin/env bash
# shell-operator hook shim — the Lo CRD reconciler is `lo operator lo-reconcile`
# (internal/operator/lo.go). shell-operator discovers hooks by path, so this
# file keeps its name; `--config` and the binding context pass straight
# through. The original bash body is frozen at
# .lok8s/legacy/operator/hooks/lo-reconcile.sh.
exec lo operator lo-reconcile "$@"
