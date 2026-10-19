# Project documentation

This is the index for the apm-control-plane codebase. Product capability and implementation state must be distinguished from proposed architecture.

## Start here

- [README](../README.md) — start P0 and run the demonstration test agent.
- [**P1–P3 技术选型（待评审）**](architecture/technology-selection.md) — 技术栈与 Go 1.25 兼容性矩阵。
- [**P1.1 Durable Agent Inventory（开发和部署指南）**](architecture/p1-durable-inventory.md) — GORM / Goose、PostgreSQL/MySQL、身份绑定、运行和验收。
- [**APM Control Plane：开发者架构指南（中文）**](architecture/README.md) — complete developer-oriented architecture, lifecycle, state, protocol contracts, extension model and implementation map.
- [ARCHITECTURE](../ARCHITECTURE.md) — source-backed P0 boundaries and proposed P1–P3 dependencies.
- [Functional requirements](requirements/APM_CONTROL_PLANE.md) — FR-001 to FR-013 and testable acceptance.
- [Engineering workflow](PROJECT_WORKFLOW.md) — RepoFoundryAI v0.15.1 / EngineeringSpecifications v1.9.0.

## Source-backed engineering research

- [R-001 — Gitea / Grafana / Casdoor / Collector 架构对比](research/active/r-001_opamp-oss-architecture/RESEARCH.md) — 固定提交的源码证据、反证与技术选型 Synthesis，未归档结论。

## Governed engineering artifacts

- [DD-001 OpAMP Control Plane (draft)](design-docs/dd-001_opamp-control-plane.md) — proposed persistent architecture; not approved.
- [EP-001 Deliver durable Control Plane (blocked)](exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) — P1–P3 milestones, verification, identity decisions and blockers.
- [Design index](DESIGN-DOCS.md), [architecture decisions](DECISIONS.md), [execution plans](PLANS.md), [Research](RESEARCH.md), [Bugfixes](BUGFIXES.md), and [technical debt](exec-plans/tech-debt-tracker.md).

## Engineering controls

- [Locked Engineering Specifications](agent-guides/managed/index.md) and [Spec lock](.engineering/specs.lock.json).
- [Quality score](QUALITY_SCORE.md), [Reliability](RELIABILITY.md), [Security](SECURITY.md).
- [P0 protocol and limitations](architecture.md).
- [Agent instructions](../AGENTS.md).

RepoFoundry generated index regions are projections: use the official designctl/epctl commands to create IDs, update statuses, rebuild indexes and archive evidence. Do not turn planned features into implementation claims.
