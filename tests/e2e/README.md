# lok8s e2e tests

End-to-end scenarios that stand real kind clusters up with `lo`, exercise
the provision → bootstrap → tilt pipeline, and tear every piece of it
down again. Each scenario lives in its own directory and runs in its own
slot under `10.125.0.0/16` (see [SUBNETS.md](./SUBNETS.md)).

## The matrix

| Scenario | Slot | What it proves | Legs |
|---|---|---|---|
| `no-services` | 126 | provision + the framework bootstrap (cilium), and the empty-services render path | go, bash |
| `single-local-build` | 127 | the Tilt build → push → deploy roundtrip for one locally built service | go, bash (through `e2e-lo-up` in ci.yml) |
| `cache-mode` | 128 | `build: false` → cache pre-pull → image swap, pod pulling from `lok8s.cache` | go |
| `registry-tls` | 131 | the certificate volume: mint, mount, serve, `tls status`, `tls renew`, `registry clean`, the one-time import from `.secrets/tls/registries` | go, bash |
| `shared-registries` | 132 + 133 | two domains, one mirror set: reuse instead of a second set, `lo down` on one leaves it up, `registry clean --shared` removes it and its network | go, bash |
| `lifecycle` | 134 | up, up again (idempotent), `--force-recreate`, down, up, destroy — and the confirmation prompts on a real terminal | go, bash, mixed |
| `remote-lo` | 129 | a Lo cluster on a remote Hetzner VM, docker mode | opt-in (`E2E_REMOTE=1`) |
| `remote-ci` | 130 | the same, CI mode | opt-in (`E2E_REMOTE=1`) |

Every scenario ends by asserting that the machine is clean:
`e2e::assert_torn_down` after `lo down` and again after `lo destroy`.

The two remote scenarios need a Hetzner account, an `HCLOUD_TOKEN` and a
few minutes of a billed VM. They are opt-in, they are NOT in CI, and
nothing runs them unless you set `E2E_REMOTE=1` yourself.

## One binary, two implementations

Every scenario on every leg runs the SAME binary. `E2E_LO_BIN` (default
`bin/lo`) says which binary — `bin/lo`, `bin/lo-full`, a released `lo`.
It never says which implementation.

The implementation comes from committed configuration, the way a user
chooses it: `spec.implementation` in the project's `lok8s.yaml`, read by
that binary. `E2E_LO_IMPL` writes that block into the scenario directory
before the run:

| `E2E_LO_IMPL` | `lok8s.yaml` | What runs |
|---|---|---|
| `go` (default) | `default: go` | every command natively |
| `bash` | `default: bash` | every command through the frozen bash tree |
| `mixed` | `default: go` + `bash.commands: [...]` | `E2E_LO_ROUTED` through bash, the rest natively |

A routed command runs `<project>/<tree>/lo`, so the `bash` and `mixed`
legs get their own copy of the tree inside the scenario directory — the
explicit, in-project tree `lo assets eject bash` gives a user.
`internal/cli/routing.go` refuses the binary's cache extract and an
entrypoint that resolves outside the project, so a symlink will not do.

Both legs must reach the same end state. Where one legitimately differs
(the confirmation prompts are Go-only), the scenario skips that one test
and says so, rather than loosening the assertion.

## Running

```bash
make build                                   # E2E_LO_BIN defaults to bin/lo

E2E=1 tests/e2e/run.sh                       # every scenario
E2E=1 tests/e2e/run.sh lifecycle             # one scenario
E2E=1 bats tests/e2e/lifecycle/test.bats     # the same, directly

# one leg
E2E=1 E2E_LO_IMPL=bash  bats tests/e2e/registry-tls/test.bats
E2E=1 E2E_LO_IMPL=mixed E2E_LO_ROUTED="status registry" \
      bats tests/e2e/lifecycle/test.bats

# another binary
E2E=1 E2E_LO_BIN="${PWD}/bin/lo-full" bats tests/e2e/lifecycle/test.bats
```

Unset the ambient project pointers first. A developer shell in a lok8s
project exports `PATH_BASE`, `PATH_BIN`, `PATH_LOK8S`, `PATH_CLUSTERS`
and `PATH_SECRETS`; inherited, they redirect the run into that project:

```bash
env -u PATH_BASE -u PATH_BIN -u PATH_LOK8S -u PATH_CLUSTERS \
    -u PATH_SECRETS -u KUBECONFIG -u KUSTOMIZE_PLUGIN_HOME \
    E2E=1 bats tests/e2e/lifecycle/test.bats
```

Without `E2E=1`, every e2e scenario skips at setup time. The opt-in gate
keeps `argsh test` from pulling a five-minute cluster lifecycle into a
run you meant as unit tests.

## Prerequisites

Tests run under bats. `argsh test` brings it, with bats-support and
bats-assert. `b install` puts the same set under `.bin/bin` and
`.bin/lib`.

Per-scenario prerequisites (a scenario skips when one is missing):

- `docker` (the daemon must be running)
- `kind`
- `kustomize`, `yq`, `tilt` — the Tilt scenarios
- `openssl` — `registry-tls`, to read the certificate off the wire
- `script` (util-linux) — the confirmation-prompt tests, for the pty
- `dig` — the DNS preflight
- wildcard DNS for the scenario's slot (`*.<n>.lok8s.dev` → `10.125.<n>.x`)

Under `E2E_STRICT=1` a missing prerequisite FAILS instead of skipping.
CI sets it: the job installs every one of them, so a skip there would be
a green check over an empty run.

## Safety

A developer machine carries live kind clusters, live registry containers
and a live Tilt. The harness may not touch them, and is built so it
cannot:

- every cluster, network and volume a scenario creates starts with
  `e2e-`, and `e2e::guard_name` refuses any other name before a
  destructive verb runs;
- `kind get clusters` is snapshotted before the first cluster comes up
  and asserted after every teardown: every pre-existing cluster is still
  there, and nothing appeared that the scenario does not own;
- every docker filter is anchored (`^name$`, `^prefix-`), so a substring
  can never widen the blast radius;
- nothing runs `docker system prune`, `lo clean --all`, or any verb
  against an object the scenario did not create;
- `e2e::lo` closes stdin, so a destructive command cannot sit waiting on
  its confirmation when bats runs from a terminal. The prompt tests give
  it a real pty instead (`e2e::pty_run`).

## The teardown is an assertion

`e2e::assert_torn_down <down|destroy>` is what a scenario calls after
each teardown. It checks, and it reports whatever it had to clean up:

| Object | After `lo down` | After `lo destroy` |
|---|---|---|
| kind cluster | gone | gone |
| node containers, proxy | gone | gone |
| registry containers | gone — unless the set is shared, where `lo down` keeps them on purpose | gone |
| registry data volumes, certificate volume | kept (the next `lo up` reuses them) | gone |
| project docker network | kept, by design — asserted, not ignored | kept; `e2e::sweep` removes it at the end |
| shared mirrors and their network | kept | kept; `lo registry clean --shared` removes them |

The belt-and-braces `kind delete` is still there, and it now FAILS the
test when it had something to delete, saying that `lo <phase>` left it
behind. A cleanup that quietly repairs a broken teardown is how a broken
teardown passes for months.

`lo destroy` is always asserted against a LIVE cluster: a scenario stands
the cluster back up after its `lo down` case, because a destroy that runs
on an already-downed cluster removes nothing and would pass however
broken its own cluster deletion is.

## The confirmation prompts

Since v0.5.0 `lo down`, `lo destroy`, `lo clean`, `lo registry clean` and
`lo image clean` list what they remove and ask, when stdin AND stderr are
terminals. `--yes` is the only skip; no environment variable silences it.

The `lifecycle` scenario drives that through a pseudo-terminal
(`e2e::pty_run`, which is `script -qec`), and the answer is TYPED INTO
THE PTY. Piping an answer into the command's own stdin does not test the
prompt — it turns the prompt off and the command proceeds. Every
transcript is kept in the scenario's `.e2e-state/pty-transcript.log`.

## Scenario layout

```
tests/e2e/
├── run.sh                  # discover + run scenarios
├── README.md
├── SUBNETS.md              # slot allocation
├── lib/
│   └── helpers.bash        # shared bats helpers
├── no-services/            # slot 126 — provision smoke
│   ├── test.bats
│   ├── Tiltfile
│   └── clusters/126.lok8s.dev/
│       └── cluster.lok8s.yaml
├── single-local-build/     # slot 127 — full build/deploy
├── cache-mode/             # slot 128 — build:false cache pre-pull
├── registry-tls/           # slot 131 — the certificate volume
├── shared-registries/      # slots 132 + 133 — one mirror set, two domains
└── lifecycle/              # slot 134 — up/down/up/destroy + the prompts
```

Each scenario dir also holds per-run state, all of it gitignored: the
`lok8s.yaml` the leg wrote, the `.lok8s` tree a routing leg needs, a
`.bin` link, `.kubeconfig/`, and `.e2e-state/` (the world snapshot, the
rendered registry configs, the pty transcripts).

## How a scenario works

The scenario directory IS the `PATH_BASE`, and it IS the project.
Helpers point `PATH_BASE` at it, `PATH_CLUSTERS` at its local
`clusters/`, and `PATH_BIN` at the repo's toolchain. From `lo`'s
perspective the scenario is a complete project with its own clusters
tree, services config, secrets dir and kubeconfigs.

On the `go` leg `PATH_LOK8S` stays at the repo tree, so a framework
change reaches the next run with no copying. On a routing leg it points
at the scenario's own copy, because that is the tree the routed command
has to run.
