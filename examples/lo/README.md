# lo — local kind cluster

The zero-cost default: a kind cluster on a local Docker bridge with working TLS,
brought up by `lo up`.

```sh
cd examples/lo
lo use 42.lok8s.dev
lo up                 # kind + cilium + metallb, then Tilt
# headless (CI): lo up --ci
```

`42.lok8s.dev` is a project subdomain (slot 42 → bridge `10.125.42.0/16`) so it
won't collide with other lok8s projects on the same machine. Registry TLS is on
by default — run `lo trust` once so host `docker push` verifies (see
[Host push trust options](/guide/shared-registries#host-push-trust-options)).

End-to-end test (provision → nodes Ready → tear down):

```sh
examples/test lo
```
