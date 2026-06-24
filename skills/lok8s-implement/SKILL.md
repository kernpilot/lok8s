---
name: lok8s-implement
description: >-
  Use when a user asks to build, scaffold, or set up an application or
  environment on lok8s from a high-level request — "set me up a PHP env with a
  MySQL DB", "build me a todo web app", "add a Redis cache". The decision tree
  that turns that into lok8s primitives: services, addons, secrets, and a running
  cluster. Always check existing state first, ask only what blocks, default to
  the local lok8s.dev cluster, and stay read-only until the plan is agreed.
  Composes lok8s-service, lok8s-addons, lok8s-secrets, lok8s-cluster-spec, lok8s-dev.
---

# Implementing an app/environment on lok8s

A "build me X" request is an *orchestration* over the lok8s primitives. This skill
is the procedure; the detail skills (referenced below) own each primitive's schema.

**Golden rules**
- **Look before you build.** Run the read-only commands first; never mutate a
  cluster to discover its state.
- **Ask only what blocks the plan.** Infer sensible defaults for the rest and say
  what you assumed.
- **Default to local.** With no target in play, use the `lok8s.dev` kind cluster —
  it's free, fast, and disposable. Only touch a remote/cloud target if the user
  named one (it can create billed infra — confirm first).
- **Never invent secrets.** Generate them with the secrets plugin (`passwd:`),
  see **lok8s-secrets**.

## Step 0 — clarify the ask (briefly)

From the request, pin down only the load-bearing unknowns and ask the rest as a
single batched question:
- the **app**: language/runtime + how it builds (a Dockerfile? a framework?).
- **datastores**: which engine (Postgres / MySQL / Redis / …), and does it need
  to persist across `lo down`?
- **exposure**: HTTP route / port, and any auth (OIDC is available).
- **target**: a specific domain, or the default local cluster?

If the request is concrete enough (e.g. "a todo web app"), pick a reasonable
stack, state it, and proceed — don't over-interrogate.

## Step 1 — check what's already there (read-only)

```bash
lo use                 # active domain + available clusters
lo status              # is a cluster up? what's running?
lo lint                # are the existing specs valid?
lo addons              # which addons the active driver bundles
cat services.yaml lok8s.yaml 2>/dev/null   # existing services
```

## Step 2 — pick the target cluster

- A spec/`lo use` already active → use it.
- The user named a domain → `lo use <domain>` (must exist under `clusters/<domain>/`).
- **Neither → default to `lok8s.dev`** (local kind). See **lok8s-cluster-spec** for
  how a `cluster.lok8s.yaml` / `deploy.lok8s.yaml` selects a driver; a fresh local
  project may need `lo init` first.

## Step 3 — decompose needs → primitives

| Need | lok8s primitive | Skill |
|---|---|---|
| the app container | a **service** (`lok8s.yaml` + `services.yaml` entry + Dockerfile) | **lok8s-service** |
| Postgres | the built-in **cnpg-operator** addon + a CNPG `Cluster` workload | **lok8s-addons** |
| Redis | the built-in **redis-operator** addon + a Redis workload | **lok8s-addons** |
| MySQL / other engines | a **chart addon** (khelm) or a plain workload — no bundled operator | **lok8s-addons** |
| credentials / TLS | the **secrets** kustomize plugin (`passwd:`, `cert:`) | **lok8s-secrets** |
| HTTP exposure | the service's `ports` + `links`, routed via the gateway | **lok8s-service** |
| persistence/storage | `local-path-provisioner` (default) or `rook-ceph` | **lok8s-addons** |

## Step 4 — scaffold + wire

1. **Service**: `lo init service <name>` scaffolds `lok8s.yaml`, a `services.yaml`
   entry, and a starter Dockerfile (dev `lok8s.Dockerfile` → prod `Dockerfile`
   swap). Fill in build, ports, and `live_update` per **lok8s-service**.
2. **Datastore**: add the operator addon (Step 3), then declare the DB instance as
   a workload. Mint its password with a `passwd:` secret (**lok8s-secrets**) into
   `clusters/<domain>/secrets/` — never a literal.
3. **Wire**: reference the secret in the app's env (e.g. `DATABASE_URL` from the
   generated secret), and connect the app↔DB via service `links`.

## Step 5 — bring it up

```bash
lo up          # local: kind + Tilt hot-reload (see lok8s-dev). Iterate live.
# remote target instead: lo build <target> then deploy — confirm first.
```

## Step 6 — verify

`lo status` (pods/health), `lo lint` (specs), the Tilt UI (`http://localhost:<port>`).
Iterate in the Tilt loop until green.

## Worked examples

- **"PHP env + MySQL DB"** → a PHP service (its Dockerfile) + MySQL via a chart
  addon or a workload (no bundled operator) + a `passwd:` DB-password secret, wired
  into the app's env; `lo up`.
- **"a todo web app"** → one service with frontend+backend `components` + Postgres
  via **cnpg-operator** + a CNPG `Cluster` + a `passwd:` secret; route the frontend;
  `lo up` and iterate.

When unsure which primitive fits, load the referenced skill and follow its schema —
don't guess the YAML.
