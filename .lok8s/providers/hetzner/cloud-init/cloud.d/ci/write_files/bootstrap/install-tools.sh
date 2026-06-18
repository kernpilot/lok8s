#!/bin/bash
# Install the lok8s toolchain for CI mode.
# Runs on first boot via cloud-init runcmd.
set -euo pipefail

ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"

# kind
KIND_VERSION="v0.27.0"
curl -Lo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${ARCH}"
chmod +x /usr/local/bin/kind

# kubectl
KUBECTL_VERSION="$(curl -sL https://dl.k8s.io/release/stable.txt)"
curl -Lo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"
chmod +x /usr/local/bin/kubectl

# kustomize
curl -s "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash
mv kustomize /usr/local/bin/

# yq
YQ_VERSION="v4.44.6"
curl -Lo /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${ARCH}"
chmod +x /usr/local/bin/yq

# argsh
curl -sL https://arg.sh/install | bash

# tilt
TILT_VERSION="0.37.0"
curl -fsSL "https://github.com/tilt-dev/tilt/releases/download/v${TILT_VERSION}/tilt.${TILT_VERSION}.linux.${ARCH}.tar.gz" | tar xz -C /usr/local/bin tilt

echo "lok8s CI tools installed"
