# shellcheck shell=bash disable=SC2034
# defaults.sh — Lo driver constants

LO_DEFAULT_DOMAIN="lok8s.dev"
LO_DEFAULT_SLOT=125
LO_DEFAULT_POD_CIDR="10.244.0.0/16"
LO_DEFAULT_SVC_CIDR="10.96.0.0/12"
LO_DEFAULT_BOOTSTRAP=(cilium)

LO_REGISTRY_IMAGE="registry:2.8.3"
# Listen ports. Plain-HTTP registries serve on :80; TLS registries serve on
# :443 so that a bare-IP `docker push <ip>/...` (which the Docker client
# resolves to the HTTPS default port 443) reaches them without an explicit
# port in the ref. See REGISTRY-TLS.md for why the port is TLS-mode-dependent.
LO_REGISTRY_PORT=80
LO_REGISTRY_PORT_TLS=443
LO_REGISTRY_OFFSET_BUILD=101
LO_REGISTRY_OFFSET_CACHE=102
LO_REGISTRY_OFFSET_MIRRORS=103
# In-container mount point for the mkcert registry cert+key (TLS mode).
LO_REGISTRY_TLS_MOUNT="/etc/registry/certs"
# Durable home for rendered registry configs. Each container bind-mounts
# <state-dir>/<container-name>.yaml — the file MUST outlive the `lo` run:
# with --restart=always the Docker daemon re-binds it on every restart, and
# a vanished source is recreated as an empty DIRECTORY, killing the registry
# (the old mktemp-and-delete approach broke every registry on reboot).
LO_REGISTRY_STATE_DIR="${LO_REGISTRY_STATE_DIR:-${XDG_STATE_HOME:-${HOME}/.local/state}/lok8s/registries}"

LO_SHARED_REGISTRY_NETWORK="lok8s-registries"
LO_SHARED_REGISTRY_CIDR="10.125.200.0/24"
