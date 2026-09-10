# shellcheck shell=bash
# runtime.sh — RETIRED. The argsh-runtime shims + library loading the bash
# hooks needed moved with them to .lok8s/legacy/operator/hooks/runtime.sh;
# the hooks in this directory are shims that exec `lo operator <hook>`, whose
# runtime layout (LOK8S_STATE_DIR → PATH_BASE, PATH_LOK8S=/hooks,
# KUSTOMIZE_PLUGIN_HOME default) lives in internal/operator (Env).
#
# Kept non-executable so shell-operator never runs it as a hook; the file can
# go with the staged rename.
