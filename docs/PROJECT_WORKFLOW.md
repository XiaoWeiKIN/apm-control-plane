# Engineering workflow: RepoFoundryAI

This repository uses [RepoFoundryAI v0.15.1](https://github.com/XiaoWeiKIN/RepoFoundryAI/releases/tag/v0.15.1) as its repository engineering Harness, with [EngineeringSpecifications v1.9.0](https://github.com/XiaoWeiKIN/EngineeringSpecifications/releases/tag/v1.9.0).

## Authoritative surfaces

| Concern | RepoFoundry capability | Repository output |
| --- | --- | --- |
| Functional requirements and acceptance | Project-owned requirements + applicable locked Engineering Specifications | `docs/requirements/`, `docs/.engineering/` |
| Research and unknowns | `engineering-research` | `docs/research/` |
| Architecture and module documentation | `detailed-design` | `ARCHITECTURE.md` and relevant module documents |
| Governed architecture design | `engineering-design` | `docs/design-docs/` |
| Decisions, delivery and bugfixes | `engineering-execution-plan` | `docs/adr/`, `docs/exec-plans/`, `docs/bugfixes/` |
| Reproducible performance evidence | `engineering-benchmark` | `benchmarks/` |
| Engineering case-study (explicitly requested only) | `engineering-case-study` | `docs/case-studies/` |

## Human-to-agent workflow

1. Record the business requirement, non-goals and testable acceptance in `docs/requirements/`, updating existing requirement IDs rather than duplicating sources of truth.
2. Inspect `AGENTS.md`, `ARCHITECTURE.md` and `.repo-foundry/skills/repo-foundry-ai/SKILL.md` before substantive implementation.
3. Start at adaptive **Explore** for bounded investigation. Move to **Build** for production modifications. Use **Governed** for security, durability, public interfaces, irreversible operations, releases or long-lived architecture decisions.
4. Activate only Engineering Specification Requirements relevant to the current task using the installed project Router. Keep exact references and checks in handoffs.
5. For an unresolved architecture choice, use Research or Design as appropriate. Do not invent accepted ADRs or approvals. Accepted ADRs require explicit decision-owner approval.
6. For cross-session milestones use a RepoFoundry ExecPlan with verifiable tasks. Record revision-specific test evidence, not just intentions.
7. At handoff run `repofoundry --repo . validate`, the applicable professional Skill validation, and `go test -race ./...` and `go vet ./...` for Go changes.

## Installation and portability

Project adapters are committed for Codex, Claude Code, and portable agents. On each developer workstation, install or upgrade the **personal CLI and six professional Skills** using the official versioned installer:

```bash
curl -fsSL https://raw.githubusercontent.com/XiaoWeiKIN/RepoFoundryAI/v0.15.1/install.py | python3 - --version 0.15.1
repofoundry --repo . validate
```

The personal installation is per machine and is **not** embedded in this Git repository. The project Harness and Spec lock are persisted in Git. Do not copy professional scripts by hand or claim that Git-hosted files install software on developer machines.

For this project, use the pinned version of the `repofoundry` CLI to preview, apply and validate changes. Update the distribution and Spec catalog only by explicit migrations.

## Historical P0 baseline

[PR #1](https://github.com/XiaoWeiKIN/apm-control-plane/pull/1) introduces an in-memory OpAMP server and HTTP test agent. Its unit tests and `go vet` passed in GitHub Actions, but manual Java SDK sampling and durable config reconciliation remain unverified. Installing RepoFoundry does not retrospectively manufacture an approved design or acceptance evidence.
