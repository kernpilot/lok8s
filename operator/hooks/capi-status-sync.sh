#!/usr/bin/env bash
# shell-operator hook shim — the CAPI Cluster status bridge is `lo operator
# capi-status-sync` (internal/operator/capistatus.go). shell-operator
# discovers hooks by path, so this file keeps its name; `--config` and the
# binding context pass straight through. The original bash body is frozen at
# .lok8s/legacy/operator/hooks/capi-status-sync.sh.
exec lo operator capi-status-sync "$@"
