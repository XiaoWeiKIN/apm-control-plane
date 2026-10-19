---
schema_version: "1.1"
metadata_schema: "1"
artifact_type: research-round
id: RR-001
parent_id: R-001
title: "Open-source architecture patterns for OpAMP Control Plane — RR-001 Baseline investigation"
status: completed
created: 2026-10-19
updated: 2026-10-19
author: "AI assistant"
owner: "Unassigned"
---

# Open-source architecture patterns for OpAMP Control Plane — RR-001 Baseline investigation

This round is one bounded investigation pass inside R-001. It does not
create a new Research identity or independently authorize conclusion.

## Focus and Questions

本轮围绕 RQ-001～RQ-004：核实 Gitea/Grafana/Casdoor 多数据库 ORM、业务存储层与迁移机制；核实 Collector 的配置来源和组件生命周期；据此评估 PR #2 的 GORM + Goose + Koanf 方案。

## Scope

只核对四个开源项目的固定源码及贡献者文档；不模拟其生产性能、不运行本项目的 GORM/Goose 双数据库 PoC，也不批准架构。

## Evidence Added

- notes/opensource-comparison.md：四个项目固定 Git SHA 的源码链接、逐项观察、反证与推荐条件。
- Gitea：XORM Engine、MySQL 事务隔离、全局连接的历史问题。
- Grafana：SQLStore、业务/ORM 边界、Migration 治理。
- Casdoor：Driver/DSN 配置与 XORM 引擎初始化。
- Collector：Confmap Provider/Converter/Resolver、Component Factory/Start/Shutdown。

## Synthesis Delta

此前的“知名项目也在使用 GORM”推测被源码证伪。推荐理由修正为本项目领域查询简单、需要 PostgreSQL/MySQL、维护双份 SQL 查询成本暂不合理。GORM 仍为待验证候选，不视为实践证据。数据库与组件生命周期建议显式持有和 Shutdown，Production Migration 仍用 Goose。

## Next Inquiry

研究报告进入 Review 后，请 Design Owner 阅读 SYNTHESIS.md 和 docs/architecture/technology-selection.md。P1 PoC 按 EP-001 检验两库 CAS、事务、Migration 和连接池 Shutdown；若出现反证，在 R-001 继续新 Round，或按 Design/ADR 生命周期更新方案。

## Round Outcome

- 2026-10-19 — Completed for Synthesis review v1.

