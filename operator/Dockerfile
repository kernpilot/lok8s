# ── stage 1: the lo binary ──────────────────────────────────────────────
# The hooks under /hooks are shims that exec `lo operator <hook>`; the hook
# bodies are Go (internal/operator). Built here from the same tree the image
# copies, stamped from the embedded mirror's VERSION with the Makefile's
# -ldflags (internal/assets.BuildVersion; the assets version test pins the
# flag across the Makefile, goreleaser and this file). The golang tag
# equals the root go.mod `go` line: the image sets GOTOOLCHAIN=local, so an
# older tag cannot build the module (the same test pins this).
# The base is pinned by its tag and by the digest of its multi-arch index
# (it has linux/amd64 and linux/arm64, the platforms docker.yml builds),
# because this stage builds the lo binary that the final image runs. A bump
# changes the tag and the digest in the same edit: with a digest, the tag
# alone does not change the image, and no test sees it. Take the index
# digest from the "Digest:" line of
# `docker buildx imagetools inspect golang:<tag>`, never the digest of one
# platform's manifest: the arm64 build would then get the amd64 image, and
# nothing would fail.
FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS lo-build
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
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/kernpilot/lok8s/internal/assets.BuildVersion=$(cat internal/assets/lok8s/VERSION)" \
      -o /out/lo ./cmd/lo

# ── stage 2: shell-operator + tools + hooks ─────────────────────────────
# The base is pinned by its tag and by the digest of its multi-arch index
# (linux/amd64 and linux/arm64 are the platforms docker.yml builds). A bump
# changes the tag and the digest in the same edit: with a digest, the tag
# alone does not change the image, and no test sees it. Take the index
# digest from the "Digest:" line of
# `docker buildx imagetools inspect ghcr.io/flant/shell-operator:<tag>`,
# never the digest of one platform's manifest: the arm64 build would then
# get the amd64 base, `uname -m` below would pick the amd64 tools, and
# nothing would fail.
FROM ghcr.io/flant/shell-operator:v1.19.5@sha256:2b1127a267538899bcf62e1ae6cf03035a0ad7480058c4d6f911f3640b46c914

# Install dependencies:
#   kustomize, yq, jq — core k8s ops (kubectl is pinned from dl.k8s.io below —
#     Alpine's kubectl lags badly, dragging in stale golang.org/x/net)
#   docker-cli, kind — Lo driver (kind clusters via the host's Docker)
#   clusterctl — CAPI management (kubeconfig extraction, move)
#   flux — GitOps bootstrap (flux bootstrap git). The release is the Flux
#     that the fluxcd addon installs: the appVersion on the first line of
#     .lok8s/addons/fluxcd/values.yaml. A CLI of another release would
#     apply its own controllers over the addon's. A test checks it.
#   git, openssh — git operations for GitOps
#   envsubst — template rendering (part of gettext; the Go hooks render
#     natively, the legacy bash reference under /hooks/legacy still needs it)
#   khelm — kustomize exec plugin inflating bootstrap addon charts
# The tools that are not Alpine packages come from their upstream releases.
# Each download goes to /tmp/dl, and `sha256sum -c` checks it against the
# sum below for its architecture before it is installed: a mismatch, or an
# architecture with no sums, fails the build. The sums are the ones the
# upstreams publish for these exact versions:
#   kubectl     dl.k8s.io/release/<version>/bin/linux/<arch>/kubectl.sha256
#   clusterctl  the asset digest that GitHub shows for the release asset
#               (cluster-api publishes no checksums file)
#   flux        flux_<version>_checksums.txt of the release
#   kind        kind-linux-<arch>.sha256sum of the release
#   khelm       checksums.txt of the release
# A version bump changes the URL and both sums of that tool.
# The base image's /etc/apk/repositories names its vendor's own mirror,
# over plain HTTP. Use Alpine's CDN over HTTPS, for the base's Alpine
# release. The packages are signed with Alpine's keys either way, but a
# mirror can give an old index, and a build fails when the mirror is down.
RUN v="$(cut -d. -f1,2 /etc/alpine-release)" \
    && printf 'https://dl-cdn.alpinelinux.org/alpine/v%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/v%s/community\n' \
      "${v}" "${v}" > /etc/apk/repositories \
    && apk upgrade --no-cache \
    && apk add --no-cache \
      kustomize \
      yq \
      jq \
      git \
      openssh-client \
      gettext \
      docker-cli \
    && ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
    && case "${ARCH}" in \
      amd64) \
        KUBECTL_SHA256=1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82 \
        CLUSTERCTL_SHA256=a6b94e0da68df254191480b11c0b50019a7a5df87560d6f70a76d3453116f21e \
        FLUX_SHA256=6e984c92aa02250ea3f907367dc7829411e31d667b100ce2005cb87d5509e5b3 \
        KIND_SHA256=50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54 \
        KHELM_SHA256=f9a36519c147fc846aa69ca5a19f6259cb2c94f88550ae34de2bdb971b39495c ;; \
      arm64) \
        KUBECTL_SHA256=c957eb8c4bea27a3bb35b269edd9082e27f027f7b76b20b5bf4afebc726c6d3e \
        CLUSTERCTL_SHA256=a9b32e0179cdbb66ba67ebdb45be818c983dedcc827f6d31bca72277250df52c \
        FLUX_SHA256=16409df2fbd40b23458d81c22dd57abd39e82450831bdf522716d9d1030976c6 \
        KIND_SHA256=b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d \
        KHELM_SHA256=40da3b53961b5a1b91a5713c536533899f3fc580d8ae31803d9c31aacda8f2e0 ;; \
      *) echo "operator image: no download checksums for ${ARCH}" >&2; exit 1 ;; \
    esac \
    && mkdir -p /tmp/dl \
    && wget -qO /tmp/dl/kubectl \
      "https://dl.k8s.io/release/v1.36.2/bin/linux/${ARCH}/kubectl" \
    && echo "${KUBECTL_SHA256}  /tmp/dl/kubectl" | sha256sum -c - \
    && install -m 0755 /tmp/dl/kubectl /usr/local/bin/kubectl \
    && rm -f /bin/kubectl /usr/bin/kubectl \
    && wget -qO /tmp/dl/clusterctl \
      "https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.3/clusterctl-linux-${ARCH}" \
    && echo "${CLUSTERCTL_SHA256}  /tmp/dl/clusterctl" | sha256sum -c - \
    && install -m 0755 /tmp/dl/clusterctl /usr/local/bin/clusterctl \
    && wget -qO /tmp/dl/flux.tar.gz \
      "https://github.com/fluxcd/flux2/releases/download/v2.9.1/flux_2.9.1_linux_${ARCH}.tar.gz" \
    && echo "${FLUX_SHA256}  /tmp/dl/flux.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/dl/flux.tar.gz -C /usr/local/bin flux \
    && chmod +x /usr/local/bin/flux \
    && wget -qO /tmp/dl/kind \
      "https://github.com/kubernetes-sigs/kind/releases/download/v0.32.0/kind-linux-${ARCH}" \
    && echo "${KIND_SHA256}  /tmp/dl/kind" | sha256sum -c - \
    && install -m 0755 /tmp/dl/kind /usr/local/bin/kind \
    && wget -qO /tmp/dl/khelm \
      "https://github.com/mgoltzsche/khelm/releases/download/v2.8.0/khelm-linux-${ARCH}" \
    && echo "${KHELM_SHA256}  /tmp/dl/khelm" | sha256sum -c - \
    && mkdir -p /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer \
    && install -m 0755 /tmp/dl/khelm /usr/local/kustomize-plugins/khelm.mgoltzsche.github.com/v2/chartrenderer/ChartRenderer \
    && rm -rf /tmp/dl

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
COPY .archive/legacy/operator/hooks/ /hooks/legacy/

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
