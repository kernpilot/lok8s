FROM ghcr.io/flant/shell-operator:v1.14.0

# Install dependencies:
#   kubectl, kustomize, yq, jq — core k8s operations
#   docker-cli, kind — Lo driver (kind clusters via the host's Docker)
#   clusterctl — CAPI management (kubeconfig extraction, move)
#   flux — GitOps bootstrap (flux bootstrap git)
#   git, openssh — git operations for GitOps
#   envsubst — template rendering (part of gettext)
#   khelm — kustomize exec plugin inflating bootstrap addon charts
RUN apk add --no-cache \
      kubectl \
      kustomize \
      yq \
      jq \
      git \
      openssh-client \
      gettext \
      docker-cli \
    && ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
    && wget -qO /usr/local/bin/clusterctl \
      "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.12.2/clusterctl-linux-${ARCH}" \
    && chmod +x /usr/local/bin/clusterctl \
    && wget -qO /tmp/flux.tar.gz \
      "https://github.com/fluxcd/flux2/releases/download/v2.7.5/flux_2.7.5_linux_${ARCH}.tar.gz" \
    && tar -xzf /tmp/flux.tar.gz -C /usr/local/bin flux \
    && rm -f /tmp/flux.tar.gz \
    && chmod +x /usr/local/bin/flux \
    && wget -qO /usr/local/bin/kind \
      "https://github.com/kubernetes-sigs/kind/releases/download/v0.32.0/kind-linux-${ARCH}" \
    && chmod +x /usr/local/bin/kind \
    && mkdir -p /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer \
    && wget -qO /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer \
      "https://github.com/mgoltzsche/khelm/releases/download/v2.7.3/khelm-linux-${ARCH}" \
    && chmod +x /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer

# Copy shell-operator hooks
COPY operator/hooks/ /hooks/

# Copy driver contract implementations (used by hooks)
COPY .lok8s/drivers/ /hooks/drivers/

# Copy physical infra providers (hcloud, ...) used by drivers
COPY .lok8s/providers/ /hooks/providers/

# Copy shared utilities (IP arithmetic, etc.)
COPY .lok8s/utils/ /hooks/utils/

# Copy shared libraries (sourced by hooks)
COPY .lok8s/libs/ /hooks/lib/

# Copy framework bootstrap addons (cilium, metallb, ...)
COPY .lok8s/addons/ /hooks/addons/

# Copy CAPI templates (for resource generation)
COPY .lok8s/drivers/capi/cluster/ /hooks/capi-templates/

# Copy CRD definitions (applied by shell-operator on startup)
COPY operator/crds/ /crds/

# shell-operator executes every executable file under /hooks as a hook.
# The framework trees are libraries, not hooks — strip their exec bits
# (runtime.sh is data too; the three *-reconcile/*-sync hooks stay).
RUN find /hooks/lib /hooks/utils /hooks/drivers /hooks/providers /hooks/addons \
      /hooks/capi-templates -type f -exec chmod a-x {} + \
    && chmod a-x /hooks/runtime.sh \
    && mkdir -p /var/lib/lok8s
