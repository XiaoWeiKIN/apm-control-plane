---
schema_version: "1.2"
metadata_schema: "1"
artifact_type: research
id: R-001
title: "Open-source architecture patterns for OpAMP Control Plane"
status: active
maturity: review_ready
research_type: comparative
synthesis: SYNTHESIS.md
manifest: RESEARCH_MANIFEST.json
created: 2026-10-19
updated: 2026-10-19
owner: "Unassigned"
author: "AI assistant"
current_round: RR-001
synthesis_revision: "1"
approved_by: ""
approved_at: ""
approval_ref: ""
---

# Open-source architecture patterns for OpAMP Control Plane

This controller is the bounded entrypoint for a multi-document Research
package. Keep current questions, routes, findings, and next actions here. Put
focused analysis in the declared corpus, raw evidence in `artifacts/`, and the
current decision-ready view in `SYNTHESIS.md`. Decision readiness never grants
permission to conclude or archive the Research.

## Research Metadata

| Field | Value |
|---|---|
| Date | 2026-10-19 |
| Last Updated | 2026-10-19 |
| Research Type | Comparative |
| Research Owner | Unassigned |
| Author | AI assistant |
| Lifecycle | active |
| Maturity | review_ready |
| Current Round | RR-001 |
| Synthesis Revision | v1 |
| Approval | Pending |

## Purpose and Decision to Enable

核对 Gitea、Grafana、Casdoor 和 OpenTelemetry Collector 的 ORM、多数据库、Migration、配置加载、生命周期架构，以决定本项目 P1 应使用多少存储抽象、是否保留 GORM + Goose + Koanf，并识别不能从知名项目直接外推的部分。研究结果是 DD-001 与技术选型的证据输入，而非自动批准。

## Current Snapshot

- Current state: 已读取四个开源项目固定 commit 的源码和开发者文档，见 notes/opensource-comparison.md；发现 Gitea/Grafana/Casdoor 实际均使用 XORM，而不是 GORM。
- Recommendation: 保留 GORM + Goose + Koanf 的有条件提议，理由是本项目简单 CRUD/多数据库与显式 Storage 边界；非上游项目对 GORM 的直接背书。
- Next inquiry: 评审 SYNTHESIS.md；实施阶段按 EP-001 在 PostgreSQL 和 MySQL 上运行相同 GORM Repository/Goose Migration/CAS 合同测试。
- Open blockers: none。双数据库 PoC 尚未执行，属于下游验收门禁，不可宣称已通过。

## Research Rounds

Use one round for one bounded pass over the shared Research purpose. A round
may add or reopen Research Questions and may reference any number of corpus
documents.

| Round | Focus | Status | Author | Started | Evidence and outcome |
|---|---|---|---|---|---|
| RR-001 | Baseline investigation | completed | AI assistant | 2026-10-19 | `rounds/rr-001_baseline.md` |

## Scope and Non-goals

范围：四个开源项目的数据库 Driver、Engine/SQLStore、Migration、配置来源以及组件生命周期；将可迁移原则映射到 OpAMP Control Plane。非目标：性能基准、全面项目源码审计、生产数据库迁移、GORM 集成实现、数据库 schema 决策批准。

## Research Questions

| ID | Status | Question | Answer or disposition | Evidence |
|---|---|---|---|---|
| RQ-001 | answered | 知名 Go 应用如何支持 PostgreSQL/MySQL 等多个 Driver？ | Gitea/Grafana/Casdoor 在固定提交中均主要使用 XORM；按 Driver/DSN 和方言配置存储 Engine。 | notes/opensource-comparison.md §2–4 |
| RQ-002 | answered | ORM 应如何与业务服务隔离、事务归谁管理？ | Grafana 的 Service 经 SQLStore，Gitea 全局 Engine 注释揭示关闭竞态；建议显式 StorageService 和业务事务边界，非每张表一个接口。 | notes/opensource-comparison.md §2–3 |
| RQ-003 | answered | 配置与模块生命周期、Migration 应参考什么？ | Collector 的 Provider/Converter/Resolver、Component Start/Shutdown 与 Grafana Migration 治理都提供可移植原则；不能照搬遥测 Pipeline。 | notes/opensource-comparison.md §3、§5 |
| RQ-004 | deferred | GORM + Goose + Koanf 在本项目双数据库上是否已完成正确性验证？ | 尚未验证；在 EP-001 的 P1 PoC 中完成真实 PostgreSQL/MySQL 事务、CAS、迁移与重启合同测试后再批准生产实现。 | docs/architecture/technology-selection.md；notes/opensource-comparison.md §7 |

Allowed statuses: `open`, `answered`, `deferred`, `invalidated`.

## Method and Sources

按“成熟多数据库业务应用（Gitea）—可观测性平台存储服务（Grafana）—多 Driver 身份系统（Casdoor）—模块/配置生命周期（Collector）”选样。读取源文件与贡献文档的固定提交，记录为：Gitea 458eda0f1d49，Grafana e4ff54aa88c6，Casdoor 2e0a10c97d94，Collector ebfdf90d82ab。完整的文件级永久链接及反证见 notes/opensource-comparison.md。

## Experiments and Prototypes

本轮未运行数据库 PoC，因为首先要验证外部系统设计事实，且外部源码结构本身不能证明我们的 ORM 兼容性。可复现实验将在 EP-001 P1 创建：真实 PostgreSQL/MySQL 容器、相同的 GORM Repository Contract Suite、Goose 升级/故障恢复、同时更新 CAS 和数据库关闭行为；结果必须记录真实 CI revision。

## Findings

- Gitea/Grafana/Casdoor 使用 XORM，不能写成“知名项目证明 GORM 可靠”；但可以证明多数据库与 ORM 并不冲突。
- Grafana 明确 Service 不直接操作 XORM，提供 SQLStore 和事务方法，并要求迁移不可直接改写已发布文件。
- Gitea 在 MySQL 明确设置事务隔离，并记载全局 Engine 的竞态与关闭困难。
- Collector 用 Factory 与 Start/Shutdown 为组件生命周期提供清楚契约；Confmap 为本地配置处理，与我们的 Agent RemoteConfig 发布不是同一职责。
以上各项出处在 notes/opensource-comparison.md §2–5。

## Contradictions and Uncertainty

与先前非源码验证的推测相反，这些代表项目并未统一采用 GORM。Casdoor 某些业务 object 直接调用 Engine，证明“所有成熟系统都必须一表一 Repository”也不成立。Grafana 规模远大于本项目，不能照搬它的完整 SQLStore。我们的 GORM 事务/CAS/迁移兼容尚未实测，当前建议为可推翻假设。

## Decision Drivers and Options

按多数据库维护成本、普通 CRUD 代码量、SQL 可审查性、事务/CAS 正确性、依赖生命周期和测试成本排序：A. GORM v2 + Goose + Koanf + 显式 Storage Service（当前候选第一，尚需 PoC）；B. sqlc + 两套 SQL/生成代码 + Goose（复杂查询增长后可能有利）；C. XORM（上游采用但未证明更适配本项目）；D. 全局 ORM / Handler 直达数据库（不推荐）。详见 SYNTHESIS.md 与 notes/opensource-comparison.md。

## Blockers

| ID | Status | Opened | Resolved | Missing capability | Impact | Unblock or resolution |
|---|---|---|---|---|---|---|

当前没有 Research 阶段的开放 Blocker。P1 双数据库验证作为 EP-001 交付门禁，不在这里伪称完成。

## Progress

- [x] (2026-10-19) 核查 4 个上游项目的固定源码和开发文档。
- [x] (2026-10-19) 编写 notes/opensource-comparison.md，区分事实、反证和本项目推论。
- [x] (2026-10-19) 提出有条件的 GORM + Goose + Koanf 建议，尚未批准。
- [ ] (EP-001 P1) 在 PostgreSQL/MySQL 执行真实 Repository/Goose 验证并留下证据。

## Outcome

Research is review-ready at Synthesis v1, but remains active. Milestone snapshot: `snapshots/synthesis-v001.md`. Only explicit Research Owner authorization may conclude it.

## Artifacts and Notes

- 详细源码证据与对比：notes/opensource-comparison.md。
- 决策摘要：SYNTHESIS.md；包 Manifest：RESEARCH_MANIFEST.json。
- 候选技术说明：docs/architecture/technology-selection.md。
- 下游设计：docs/design-docs/dd-001_opamp-control-plane.md。
- 下游交付计划：docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md。

## Revision Notes

- 2026-10-19T08:54:32Z — Initial Research package created by official researchctl.
- 2026-10-19 — Added source-pinned Gitea/Grafana/Casdoor/Collector findings; pending official Research review.
- 2026-10-19T09:01:33Z — Marked review-ready at Synthesis v1; Milestone snapshot: `snapshots/synthesis-v001.md`. Research remains active.

