---
name: lok8s-skill-maintainer
description: >-
  Use to refresh, audit, or fix a lok8s-* Agent Skill in skills/ against the
  bash readers that are its source of truth — when a `.lok8s/**` reader,
  provider, driver, or the secrets kustomize plugin changed and a skill may now
  be stale, or when a skill's claim is suspected wrong. Re-derives each skill's
  schema/commands from the code, diffs against SKILL.md, and patches the drift.
  Maintainer-facing (keeps the other skills honest), not for authoring lok8s
  artifacts.
---

# Maintaining lok8s skills (drift audit → patch)

The skills in `skills/` are hand-written caches of behavior that actually lives
in the lok8s bash readers, drivers, providers, and the secrets kustomize plugin.
Those move fast; the skills rot. **The code is authoritative — if a skill and
the code disagree, the code wins** (same rule as `skills/README.md`). This skill
is the procedure for finding and fixing that drift, one target skill at a time.

It is the public-skill counterpart to the internal `/improve` command (which
maintains `.claude/agents/experts/*`): same learning loop, different target.

## The contract: every skill declares its sources

A skill is auditable only if it says *which code is its source of truth*. The
long-term contract is a `sources:` list in each skill's frontmatter, e.g.:

```yaml
---
name: lok8s-secrets
description: >- …
sources:                       # authoritative readers — the audit re-derives from these
  - kustomize/plugins/secret/  # the generator spec + registry
  - kustomize/pkg/charset/
  - .lok8s/libs/secrets        # `lo secrets` subcommands + cache
  - .lok8s/lo                  # `lo trust`
---
```

When auditing a target, **read its `sources:` first**. If it has none, use the
bootstrap map below, do the audit, and **add the `sources:` block** while you're
in there (so the next run is deterministic).

### Bootstrap source-of-truth map

Discovered by the 2026-06-21 audit. Run `ls` on these before trusting them — the
layout has moved before (e.g. `.claude/domain-map.conf` still points at the old
`.lok8s/.scripts/` tree).

| Skill | Authoritative readers |
|-------|-----------------------|
| `lok8s-cluster-spec` | `.lok8s/drivers/{lo,kubeone,kkp,capi}/`, `.lok8s/libs/{provision,bootstrap,lint}`, `.lok8s/libs/kubehz/main`, `operator/crds/` (incomplete vs readers) |
| `lok8s-addons` | `.lok8s/libs/{bootstrap,addons,build,deploy,manifest,lint}`, `.lok8s/addons/*/`, `.lok8s/tilt/Tiltfile` (labels) |
| `lok8s-service` | `.lok8s/tilt/Tiltfile`, root `Tiltfile`, `.lok8s/libs/{lint,init,env}` |
| `lok8s-secrets` | `kustomize/plugins/secret/`, `kustomize/pkg/charset/`, `.lok8s/libs/secrets`, `.lok8s/lo` (`trust`) |
| `lok8s-bare-metal` | `.lok8s/providers/hetzner/{main,utils/,cloud-config,cloud-init/}`, `.lok8s/drivers/{kubeone,capi}/` |
| `lok8s-dev` | `.lok8s/lo`, `.lok8s/libs/{tilt,status,init}`, `.lok8s/libs/init.d/test/`, `tests/` |
| `lok8s-doctor` | cross-cutting: `.lok8s/libs/{build,doctor,trust,secrets,lint}`, `.lok8s/drivers/lo/utils/{render,services}.sh`, `.lok8s/providers/hetzner/cloud-init/` — verify each symptom string + each recommended fix command exists |

## Procedure (one skill at a time)

1. **Read** the target `SKILL.md` (+ `reference.md` if present) and its `sources:`.
2. **Discover the real layout** — `ls` the source dirs. Note any path the skill
   names that no longer exists; never audit from memory.
3. **Extract every concrete, checkable claim**: field/key names, accepted
   values/enums, defaults, required-vs-optional, file names, `lo`
   subcommands/flags, and stated behaviors ("X defaults to Y", "Z is required").
   For `lok8s-doctor`, each *symptom string* and each *fix command* is a claim.
4. **Verify each against the code** — grep the readers for the field/flag/string.
   Confirm or refute with a `path:line` citation.
5. **Classify**:
   - **Drifted** — skill says X, code does Y. *Must* cite `path:line`. No citation → not drift.
   - **Unverifiable** — no code found (possibly stale/aspirational); or a symptom
     legitimately owned by an external tool (kind/docker/tilt/curl). Say which.
   - **Correct** — positively confirmed; list these so coverage is visible.
6. **Patch** the `SKILL.md`/`reference.md` to match the code. Critically, also
   **regenerate the frontmatter `description`** — it is the router signal the
   orchestrator uses to pick the skill; a description that omits a new generator
   or subcommand causes mis-routing, not just stale prose.
7. **Preserve** the skill's voice, structure, and its inline doc-vs-code
   divergence notes (e.g. "this field isn't in the CRD but the reader reads it").
   Add such a note wherever the CRDs lag the readers rather than deleting it.
8. **Re-check** — run `lo lint` where the skill governs a linted artifact, and
   re-read the patched section against the cited code one more time.

## Rules

- **Code wins, always.** Never invent a field, flag, or default to make the skill
  look complete. If the code doesn't have it, the skill doesn't claim it.
- **Evidence or it didn't happen.** Every change traces to a `path:line`.
- **One skill per agent.** Keeps each diff reviewable and citations honest. To
  refresh several, fan out — one subagent per skill (see Batch mode), not one
  agent juggling all seven in a single context.
- **Examples must be real.** A worked example (a sample `cilium` kustomization, a
  `cert:` block) is copied/adapted from an actual file in the tree, not composed.
  A wrong example teaches the bug.

## Scope it from changed files

To find *which* skills a commit may have staled, map changed paths → skills via
the source map above (e.g. a diff touching `kustomize/plugins/secret/` or
`.lok8s/libs/secrets` ⇒ audit `lok8s-secrets`). A CI check or pre-push hook that
prints "these readers changed without their skill" is the natural trigger that
makes this skill fire on time instead of months late.

## Batch mode (fan-out across skills)

This skill is the per-target *unit*; refreshing many is an *orchestration* on top
of it — the caller (a main agent or a workflow) spawns one subagent per target,
each instructed to load `lok8s-skill-maintainer` and run the procedure for its
one skill. Do **not** ask a single agent to do all seven in one context: it
blows the context budget and tangles the citations.

- **Pick the targets.** Default to only the skills whose `sources:` a recent diff
  touched (see "Scope it from changed files"); audit all seven only on demand.
- **Fan out, one subagent per skill.** Each gets: the target name, its `sources:`
  (or the bootstrap-map row), and "audit → classify → patch your one skill;
  return the drift list with `path:line` and the patch you applied."
- **Parallel patching is safe here** because the write sets are **disjoint** —
  each agent writes only `skills/lok8s-<target>/`. No worktree isolation needed.
  (The read-only audit was trivially safe; this is the argument that makes the
  *write* phase safe too.) The lone shared file is this skill's source map, which
  is read-only during a run.
- **Synthesize after.** One pass to collect the per-skill drift reports, run
  `lo lint` once, and confirm no skill's frontmatter `description` regressed.

The 2026-06-21 audit was exactly this fan-out with the patch step omitted — so
the pattern is already proven on these seven skills.
