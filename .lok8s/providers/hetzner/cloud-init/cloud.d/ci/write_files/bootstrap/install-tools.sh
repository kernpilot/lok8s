#!/bin/bash
# lok8s CI-mode toolchain — installs kind/kubectl/kustomize/yq/tilt on a fresh
# remote VM. Runs on first boot via cloud-init runcmd.
#
# Scope: this is e2e/CI scaffolding (tests/e2e/remote-ci). The remote flow rsyncs
# the project's pinned .bin/ and puts it FIRST on PATH, so these are a fallback
# for a bare VM, not the primary toolchain — a candidate for relocation into a
# self-contained e2e fixture (see docs/guide/cloud-init.md § Module library).
# Pinned, direct downloads only — never pipe a remote script into a shell.
set -euo pipefail

ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"

# kind
KIND_VERSION="v0.27.0"
curl -fLo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${ARCH}"
chmod +x /usr/local/bin/kind

# kubectl (latest stable)
KUBECTL_VERSION="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
curl -fLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"
chmod +x /usr/local/bin/kubectl

# kustomize — pinned to the project's mise/b version, direct release tarball
# (NOT the piped install_kustomize.sh script).
KUSTOMIZE_VERSION="v5.8.1"
curl -fsSL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/kustomize_${KUSTOMIZE_VERSION}_linux_${ARCH}.tar.gz" \
  | tar xz -C /usr/local/bin kustomize

# yq
YQ_VERSION="v4.44.6"
curl -fLo /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${ARCH}"
chmod +x /usr/local/bin/yq

# tilt
TILT_VERSION="0.37.0"
curl -fsSL "https://github.com/tilt-dev/tilt/releases/download/v${TILT_VERSION}/tilt.${TILT_VERSION}.linux.${ARCH}.tar.gz" | tar xz -C /usr/local/bin tilt

# NOTE: argsh is intentionally NOT installed here — the rsync'd .bin/ ships it
# (with its matching argsh.so) and is first on PATH; piping arg.sh/install would
# both violate the no-pipe-to-shell rule and risk an .so version mismatch.

echo "lok8s CI tools installed"
