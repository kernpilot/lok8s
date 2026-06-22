# kkp — user cluster via Kubermatic (KKP)

The `Kkp` driver calls an **existing** Kubermatic (KKP) installation's API to
create a user cluster — KKP owns the control plane (etcd/apiserver), Hetzner
provides the worker nodes. Unlike `lo`/`capi`, this one needs a running KKP to
point at.

## Prerequisites

- A reachable **KKP endpoint** (`spec.kkp.apiUrl`) — e.g. the kubehz internal
  plane `https://kkp.kubehz.in.net`.
- A KKP **project id** (`spec.kkp.projectId`) and **datacenter**
  (`spec.kkp.datacenter`, e.g. `hetzner-fsn1`).
- A **KKP API token** and a **Hetzner token**, in the gitignored
  `.secrets/hetzner.env`:
  ```sh
  cat > examples/kkp/.secrets/hetzner.env <<'EOF'
  KKP_TOKEN=<your-kkp-token>
  HCLOUD_TOKEN=<your-hetzner-token>
  EOF
  ```

## Run

```sh
examples/test kkp
# or: cd examples/kkp && lo use kkp-example.lok8s.dev && lo up
```

Until you have a KKP to point at, this example is a template. The hosted path
(kubehz running the KKP control plane for you) is [`../kkp-hosted`](../kkp-hosted/).
