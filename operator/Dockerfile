# ── stage 1: the lo binary ──────────────────────────────────────────────
# The hooks under /hooks are shims that exec `lo operator <hook>`; the hook
# bodies are Go (internal/operator). Built here from the same tree the image
# copies, stamped with .lok8s/VERSION like `make build`.
FROM golang:1.26.8-alpine AS lo-build
WORKDIR /src
COPY go.mod go.sum ./
# The root module replaces github.com/kernpilot/lok8s/kustomize with the
# local ./kustomize tree, so `go mod download` needs that module's go.mod
# (and the build its sources: internal/render + driver/lo import it).
COPY kustomize/go.mod kustomize/go.sum kustomize/
RUN go mod download
COPY kustomize/ kustomize/
COPY cmd/ cmd/
COPY internal/ internal/
COPY .lok8s/VERSION .lok8s/VERSION
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/kernpilot/lok8s/internal/cli.version=$(cat .lok8s/VERSION)" \
      -o /out/lo ./cmd/lo

# ── stage 2: shell-operator + tools + hooks ─────────────────────────────
FROM ghcr.io/flant/shell-operator:v1.19.5

# Install dependencies:
#   kustomize, yq, jq — core k8s ops (kubectl is pinned from dl.k8s.io below —
#     Alpine's kubectl lags badly, dragging in stale golang.org/x/net)
#   docker-cli, kind — Lo driver (kind clusters via the host's Docker)
#   clusterctl — CAPI management (kubeconfig extraction, move)
#   flux — GitOps bootstrap (flux bootstrap git)
#   git, openssh — git operations for GitOps
#   envsubst — template rendering (part of gettext; the Go hooks render
#     natively, the legacy bash reference under /hooks/legacy still needs it)
#   khelm — kustomize exec plugin inflating bootstrap addon charts
RUN apk upgrade --no-cache \
    && apk add --no-cache \
      kustomize \
      yq \
      jq \
      git \
      openssh-client \
      gettext \
      docker-cli \
    && ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
    && wget -qO /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/v1.36.2/bin/linux/${ARCH}/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && rm -f /bin/kubectl /usr/bin/kubectl \
    && wget -qO /usr/local/bin/clusterctl \
      "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.3/clusterctl-linux-${ARCH}" \
    && chmod +x /usr/local/bin/clusterctl \
    && wget -qO /tmp/flux.tar.gz \
      "https://github.com/fluxcd/flux2/releases/download/v2.8.8/flux_2.8.8_linux_${ARCH}.tar.gz" \
    && tar -xzf /tmp/flux.tar.gz -C /usr/local/bin flux \
    && rm -f /tmp/flux.tar.gz \
    && chmod +x /usr/local/bin/flux \
    && wget -qO /usr/local/bin/kind \
      "https://github.com/kubernetes-sigs/kind/releases/download/v0.32.0/kind-linux-${ARCH}" \
    && chmod +x /usr/local/bin/kind \
    && mkdir -p /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer \
    && wget -qO /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer \
      "https://github.com/mgoltzsche/khelm/releases/download/v2.8.0/khelm-linux-${ARCH}" \
    && chmod +x /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer

# The lo binary the hook shims exec (`lo operator <hook>`). On PATH for the
# shims; `lo operator` derives its runtime layout from LOK8S_STATE_DIR /
# PATH_LOK8S (defaults /var/lib/lok8s and /hooks — see internal/operator).
COPY --from=lo-build /out/lo /usr/local/bin/lo

# Copy shell-operator hooks (the shims)
COPY operator/hooks/ /hooks/

# The frozen bash hooks — the reference the Go port is measured against
# (hack/parity-operator.sh). Present in the image for a manual fallback
# (`bash /hooks/legacy/<hook>.sh` reads the same /hooks tree + state dir);
# NOT executable, so shell-operator never runs them as hooks.
COPY .lok8s/legacy/operator/hooks/ /hooks/legacy/

# Copy driver contract implementations (used by the legacy hooks; the Go
# drivers are compiled into lo)
COPY .lok8s/drivers/ /hooks/drivers/

# Copy physical infra providers (hcloud, ...) used by drivers
COPY .lok8s/providers/ /hooks/providers/

# Copy shared utilities (IP arithmetic, etc.)
COPY .lok8s/utils/ /hooks/utils/

# Copy shared libraries (sourced by the legacy hooks)
COPY .lok8s/libs/ /hooks/lib/

# Copy framework bootstrap addons (cilium, metallb, ...) — the Go bootstrap
# engine resolves them under PATH_LOK8S/addons = /hooks/addons
COPY .lok8s/addons/ /hooks/addons/

# Copy CAPI templates (for resource generation: `lo operator capi-reconcile`
# renders /hooks/capi-templates, as the bash hook did)
COPY .lok8s/drivers/capi/cluster/ /hooks/capi-templates/

# Copy CRD definitions (applied by shell-operator on startup)
COPY operator/crds/ /crds/

# shell-operator executes every executable file under /hooks as a hook.
# The framework trees are libraries, not hooks — strip their exec bits
# (runtime.sh and the legacy hooks are data too; the three shims stay).
RUN find /hooks/lib /hooks/utils /hooks/drivers /hooks/providers /hooks/addons \
      /hooks/capi-templates /hooks/legacy -type f -exec chmod a-x {} + \
    && chmod a-x /hooks/runtime.sh \
    && mkdir -p /var/lib/lok8s
