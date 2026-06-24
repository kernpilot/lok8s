# lok8s Agent Skills

A collection of [Agent Skills](https://docs.claude.com/en/docs/claude-code/skills)
that give an LLM the know-how to work with lok8s — the schemas, decision trees,
and troubleshooting playbooks for the `lo` CLI, its specs, addons, secrets, and
providers.

These **complement the MCP server** lok8s ships (`.mcp.json` → `lo mcp`): the MCP
gives a model live *tools* (run `lo`, read cluster state); these skills give it
the *procedures and schemas* to author lok8s artifacts correctly and to debug
them — knowledge that otherwise lives scattered across the docs and hard-won
experience.

## Skills

| Skill | Use when |
|-------|----------|
| [`lok8s-implement`](lok8s-implement/SKILL.md) | a high-level "build me X / set up Y" request — the orchestration decision tree |
| [`lok8s-cluster-spec`](lok8s-cluster-spec/SKILL.md) | writing/editing `cluster.lok8s.yaml` or `deploy.lok8s.yaml` |
| [`lok8s-service`](lok8s-service/SKILL.md) | adding a service — `lok8s.yaml` / `services.yaml`, build & `live_update` |
| [`lok8s-dev`](lok8s-dev/SKILL.md) | the dev loop (`lo up` + Tilt hot-reload, headless `--ci`) and the Playwright test suite (`lo init test`) |
| [`lok8s-addons`](lok8s-addons/SKILL.md) | adding/changing an addon, a target, or an inline bootstrap value |
| [`lok8s-secrets`](lok8s-secrets/SKILL.md) | writing a `secrets.lok8s.dev` Secret generator |
| [`lok8s-bare-metal`](lok8s-bare-metal/SKILL.md) | a Hetzner `hetzner.json` descriptor or cloud-init config |
| [`lok8s-doctor`](lok8s-doctor/SKILL.md) | a `lo up` / build / deploy failure — symptom → cause → fix |
| [`lok8s-ai`](lok8s-ai/SKILL.md) | lok8s's own AI features — `lo chat`, `lo ai`, the `lo mcp` bridge, skill delivery |
| [`lok8s-skill-maintainer`](lok8s-skill-maintainer/SKILL.md) | refreshing/auditing a lok8s-* skill against the code (drift → patch) |

## Using them

Each skill is a directory with a `SKILL.md` (and, for the larger ones, a
`reference.md` loaded on demand).

- **Claude Code**: `lo ai link claude` symlinks these into `.claude/skills/`
  (`--copy` to copy them instead); or ship the repo as a Claude Code plugin.
- For agents without a skill system, `lo chat` injects the relevant skill into
  context for a turn instead. See **lok8s-ai**.
- They are plain Markdown, so they also work as context for any LLM/agent.

## Source of truth

lok8s itself is authoritative. If a skill and the code disagree, **the code
wins** — the bash readers/validators (`.lok8s/drivers/*`, `.lok8s/libs/*`, the
`.lok8s/tilt/Tiltfile` validators) and `operator/crds/`. The CRDs are
*incomplete* relative to the readers (several real fields aren't in any CRD), so
these skills are written against the readers and flag known doc-vs-code
divergences inline. When in doubt, `lo lint --domain <domain>`.
