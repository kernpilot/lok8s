# Security Policy

Security is a first-class concern in lok8s — it provisions and manages
Kubernetes clusters and handles credentials, so we take vulnerability reports
seriously.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately via GitHub's **[Report a vulnerability](https://github.com/kernpilot/lok8s/security/advisories/new)**
(the repository's **Security** tab → *Report a vulnerability*). This opens a
private advisory visible only to you and the maintainers.

Please include, where you can:

- a description of the issue and its impact,
- steps to reproduce (a minimal `cluster.lok8s.yaml` or command sequence helps),
- the affected version / commit,
- any suggested fix or mitigation.

We aim to acknowledge reports within a few days and to keep you informed as we
investigate. Once a fix is ready we will coordinate disclosure with you and
credit you if you wish.

## Supported versions

lok8s is pre-1.0 and moves quickly: security fixes land on `main` and in the
latest release. Please reproduce against the latest release before reporting.

## Scope

**In scope:** the `lo` CLI, drivers, providers, addons, the kustomize secrets
plugin, and the operator.

**Out of scope:** vulnerabilities in the third-party tools lok8s installs or
orchestrates (kubectl, kustomize, Helm, kind, KubeOne, cert-manager, etc.) —
please report those to their respective upstream projects.
