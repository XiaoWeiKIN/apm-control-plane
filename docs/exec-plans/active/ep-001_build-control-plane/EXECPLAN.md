---
schema_version: "2.8"
metadata_schema: "1"
artifact_type: exec-plan
id: EP-001
title: "Deliver durable OpAMP Control Plane"
status: blocked
latest_checkpoint:
research_refs: []
research_gate: not_required
research_gate_reason: "Explicit user direction, existing implemented OpAMP P0 source and documented upstream API are sufficient to plan durable state and policy milestones without a new concluded Research artifact; security and scaling unknowns remain tracked."
adr_refs: []
adr_constraint_refs: []
adr_evidence: []
design_refs: ["docs/design-docs/dd-001_opamp-control-plane.md"]
design_evidence: []
architecture_entrypoint: "docs/design-docs/index.md"
architecture_decision_gate: not_required
architecture_decision_gate_reason: "This active plan executes the user-selected Go OpAMP control-plane scope, not a new accepted durable architecture choice; security and schema alternatives remain unapproved and must be governed before irreversible implementation."
architecture_compliance: applicable
architecture_compliance_reason: ""
required_benchmark_scenarios: []
verified_revision:
verification_evidence: []
archive_sha256:
created: 2026-10-19
updated: 2026-10-19
author: "AI assistant"
owner: "Unassigned"
---

# Deliver durable OpAMP Control Plane

This ExecPlan is a bounded living document. Keep current truth synchronized. Preserve historical events without rewriting them, and seal older events into immutable history checkpoints when the root working set grows.

## Purpose / Big Picture

交付持久化的 **通用 Go OpAMP Agent Control Plane**：长期可管理 Java、Go、Python APM Agent 并预留 Collector 接入；本 EP 只实现 Java Agent 场景的控制端。运维人员可查询 AgentKind/Distribution 及实例健康，按 Service/Environment 发布 Java 采样策略，并准确区分配置传输与 Agent 实际应用结果。HTTP/WS 断线重连及服务器重启不应丢失 Desired State。用户验证接口为 Management API、数据库状态、OpAMP RemoteConfig 与 ACK；真实 JVM 采样更新在另一个工作流验收。

## Current Snapshot

- Latest checkpoint: none（本执行计划首次创建）。
- Current milestone: **P1 — Durable Inventory and authenticated identity boundary**（尚未实现）。
- Verified baseline: PR #1 的 P0 Go 编译、单测（含 Race Detector）和 Vet 在 GitHub Actions 通过；测试 Agent 的实际双进程手工烟测未执行。
- Next action: 评审 DD-001 中的凭证到实例/租户绑定模型，确定 PostgreSQL/MySQL 共通 Repository 契约和独立 Migration；增加 internal/storage 与 internal/registry，并在 PostgreSQL/MySQL 均执行重启恢复合同测试。
- Confirmed product scope (2026-10-19): 多语言 APM Agent + 后续 Collector；P1–P3 只交付通用抽象和 Java 适配链路，不把其他语言或 Collector 执行能力算入验收。
- Open: DD-001 尚未批准；没有 accepted ADR；生产 TLS/身份策略尚待明确。不能把这些条件标记为完成。

## Context and Orientation

仓库入口为 cmd/server/main.go（当前把 OpAMP Gateway、API、内存 Agent 状态耦合在一个文件），cmd/testagent/main.go（HTTP 测试 Agent），cmd/server/main_test.go（协议/HTTP 基础单测），docs/requirements/APM_CONTROL_PLANE.md（FR-001…FR-009），docs/architecture.md（P0 边界）。

目标为“**Universal Control Plane ←OpAMP→ Java/Go/Python APM Agent，以及未来 Collector**”。本 EP 的代码路径只有自定义 Java Agent + HTTP Test Agent。通用控制面不得绑定 Java Sampler 接口：注册的 AgentKind/Distribution 与版本/Capabilities 必须独立于 service.name，配置下发经 AgentKind Adapter 编译。JVM 侧单独负责动态采样。管理 REST API 面向人/系统，OpAMP 面向 Agent；OTLP 数据不经过这里。

P1 首先拆模块并持久化；P2 在此基础上引入策略版本和协调器；P3 实现发布与可审计回滚。三个阶段是独立可验收增量，不能以文档完成替代协议结果。

## Constraints and References

| Source | Required constraint | Check |
| --- | --- | --- |
| docs/requirements/APM_CONTROL_PLANE.md | FR-001…FR-012 与 P1–P3 范围 | 按 FR 做验收映射 |
| docs/design-docs/dd-001_opamp-control-plane.md | Proposed Design; NOT approved, no immutable evidence pin | 实施前逐项 review 并按结论更新 |
| docs/.engineering/specs.lock.json | EngineeringSpecifications v1.9.0 lock | RF spec validate + 精确 Requirement 激活 |
| cmd/server/main.go | 现有 opamp-go 回调、P0 Token、内存模型 | Go tests/race and protocol tests |
| docs/SECURITY.md | 未知项不虚构；严禁未认证范围下发 | 负向权限测试 |

不变量：Desired 和 Observed 分离；配置 Hash 与 Revision 分离；Agent 成功接收消息不等于 APPLIED；身份声明属性无授权力；相同配置下发幂等；控制故障不影响 OTLP。AgentKind 不代表能力：必须按 Capabilities + ConfigDialect + 发行版版本解析，缺少能力时拒绝 Java 专有策略。

## Research and Architecture Inputs

- Research gate: not_required。已经由明确用户路线、现有 P0 实现及 opamp-go 公开接口建立实施初始事实，不虚构 concluded Research。
- Architecture decision gate: not_required for plan creation。Go + opamp-go + 首期自定义 Java Agent 和先交付 Control Plane 已由用户选择；2026-10-19 用户进一步确认未来多语言 APM Agent 及 Collector 的产品范围；本计划不接受未批准 ADR。若 mTLS/Token、重复 UID 或 HA 的长期方案需要正式选择，先创建 proposed ADR 并经 Owner 接受再实施该部分。
- Architecture compliance: applicable。精确 Design 输入路径为 docs/design-docs/dd-001_opamp-control-plane.md，architecture entrypoint 为 docs/design-docs/index.md。DD-001 为 draft，并不构成 approved revision；未批准时本 EP 不能归档 completed。
- ADR references: []；ADR constraint evidence: []；approved Design evidence: []。不存在不得虚构。
- Source-backed evidence: PR #1 的 P0 代码及 GitHub Actions 绿色 Go / RF 校验，只能证明当前范围的测试通过。
- 负面边界：没有证明 JVM 热更采样、生产流量 SLA、跨实例同步或批量发布可靠性；这些均保留为单独的后续验收。

## Architecture Compliance Matrix

| ADR constraint or architecture input | Implementation or preservation | Verification |
| --- | --- | --- |
| docs/design-docs/dd-001_opamp-control-plane.md | Preserve Control/Data separation, credential-bound Agent identity, immutable policy versions, Desired vs Observed state, capability checks, AgentKind/Distribution-aware configuration adapters, and HTTP/WS delivery semantics; Java remains the only first-delivery config dialect, all proposals provisional until Design approval | Protocol integration tests, database restart test, role-boundary negative tests, stale ACK/concurrency tests |

There are no accepted ADR constraints to map. docs/design-docs/index.md is the navigation entrypoint, not an extra normative constraint row.

## Benchmark Gate Set

- Required Scenario IDs: []。P1/P2 正确性、授权和故障恢复使用确定性集成测试；暂无用户批准的生产规模或 SLO，不制造性能基准数字。
- 在开始规模/延迟优化前，使用 RF engineering-benchmark **预声明** BS 场景（连接数、消息大小、掉线率、下发 p95 和内存），再执行并封存结果；若测试推翻单体方案，返回 Research/ADR。当前 EP 不以未创建的 Benchmark Run 声称通过。

## Plan of Work

先锁定 P0 测试行为与安全缺口，把当前 main.go 分拆为 HTTP 装配、OpAMP Gateway、通用 Agent Registry 与 Admin API，保持现有路由兼容。通用 Agent Identity 包含 AgentKind/Distribution/Version/Capabilities，避免把业务 Service 版本当 Agent 发行版版本。P1 使用 database/sql 的 Repository/UnitOfWork Port + PostgreSQL/MySQL Adapter 存储 Agent Identity 与 Observed Snapshot；双方言使用独立 SQL/Migration，同一业务行为合同测试；Session 保留在内存。

P2 将通用 Configuration/Revision/Assignment 与 Java SamplingPolicy 解耦；通过可显式拒绝不兼容目标的 `AgentConfigAdapter` 把 Java Head Sampling 规则编译为 `apm/sampling.json`。按 service/environment/instance 解析分配并存入 Desired State，Reconciler 对接 opamp-go，区分 HTTP 拉取与 WebSocket 推送，使用 ACK Hash 更新 Observed。P3 追加带审计的受控 rollout、失败隔离、显式 rollback 发布和必要的容量测量。

## Milestones

### M1 — P1 Durable Agent Inventory / Identity

修改 internal/opamp、internal/registry、internal/storage（Repository Port + PostgreSQL/MySQL Adapter）、db/migrations/postgres、db/migrations/mysql 和 cmd/server 的装配。新增 AgentKind/DistributionVersion/Capabilities 字段与服务身份映射（Collector 不强制关联 Java Service）。可观察结果：已认证 Agent 的状态持久化；数据库或服务端重启后保留身份与 Last Seen；重复 UID 和租户越权请求被拒绝或明确标记。保持现有 P0 API 兼容。

验证范围：使用 Java、Go、Python、Collector 的**身份元数据样本**验证 Registry 通用性；不要求后三类 Agent 运行时真实接入。

验收：执行 go test -race ./...；在 PostgreSQL 与 MySQL 的本地/CI 集成环境分别启动 Agent、重启服务、查询 GET /api/v1/agents/{uid}，验证对应记录未消失并可回连；运行未认证与跨租户负向用例。保存测试结果/CI Run URL。

### M2 — P2 Sampling Policy + Reconciler

修改 internal/policy、internal/config、internal/reconcile、storage migrations、Management API，增加不可变通用 ConfigRevision、ConfigRelease/Assignment 与 deterministic hash，并只实现 Java Sampling AgentConfigAdapter。可观察结果：在 Admin API 发布 rate 0.1 后，在线 HTTP Test Agent 下一次轮询收到 Config；匹配 Hash 的 APPLIED 将状态变为 APPLIED；服务端重启后待下发任务被恢复；不支持 RemoteConfig 或不兼容 Java ConfigDialect 的 Agent 不会收到 Java JSON，也不被误标成功。

验收：Go 集成测试覆盖 stale ACK、重复 ACK、配置冲突、离线/重连、错误配置、WS writer 序列化及 HTTP polling，使用真实 OpAMP Client/Server 对话而非仅 mock。

### M3 — P3 Audited Rollout / Recovery

增加发布批次、并发上限、进度统计、失败阈值与显式回滚，以及审计事件；Release 必须记录目标 AgentKind、具体配置 Schema 及目标集合，不跨不同 Agent Kind 默认复制同一策略。可观察结果：小批量 Agent 先确认，再进入后续批次；失败时停止扩散；指向旧版本的回滚有独立审计记录。

验收：故障注入与回滚回放；持久性/权限测试；新建的 Benchmark Scenario 必须先声明再执行，不得事后调整门槛。

## Concrete Steps

项目根目录为仓库根（相对路径均从这里开始）。

1. RF 状态与范围：执行 repofoundry --repo . validate、repofoundry --repo . spec validate，读取 docs/requirements 和 DD-001；对 Go + security + data boundaries 精确激活适用规范。
2. 验证起点：执行 go mod tidy、go test -race ./...、go vet ./...；查看 cmd/server/main.go 的现有接口，不静默修改其路由。
3. M1：创建迁移、Repository/Transaction Interface、数据库集成测试与身份证明绑定；执行 postgres 启动、schema migration、断线/重启 API 测试。
4. M2：实现 Policy Resolver、Immutable Revision、Reconciler 和 Config Status 归因。使用 cmd/testagent 做 HTTP 链路，再增加 WebSocket 测试客户端。
5. M3：定义受控发布批次、审计和回滚。只在证据足够后确定默认超时、容量和 SLO。
6. 每阶段更新本 EXECPLAN.md 的 Current Snapshot、Progress、验收结果、Revision Notes；执行 epctl validate、designctl validate 和 RF validate。

脚本所在 RF 发行包由仓库 PROJECT_WORKFLOW.md 的固定版本安装；不要在 Git 中手工制造 ADR/EP 编号。

## Validation and Acceptance

- [ ] **P1 Identity**：从仓库根执行 go test -race ./...，并在 PostgreSQL 与 MySQL 运行同一套 Agent Registry/Repository 合同测试；期待凭证隔离、重复 UID 检测和重启后记录可恢复。证据：CI Job URL + test output。
- [ ] **P1 Compatibility**：GET /api/v1/agents 与 GET /api/v1/agents/{uid} 保持兼容；非法 UID/越权返回 4xx；Java/Go/Python/Collector 身份样本应正确区分 AgentKind、Distribution 与业务 Service；不声称四种 Agent 都可实际连接。证据：HTTP contract + Registry tests。
- [ ] **P2 Policy**：发布 rate=0.1，测试 Agent 在下一次轮询收到 schema=1 的 JSON；相同 Hash 的 APPLIED 更新 Observed，旧 ACK 不覆盖新 Desired。证据：OpAMP integration trace + DB assertions。
- [ ] **P2 Reconciliation**：先发布后断开 Agent、重启控制平面、重新接入；期待 Desired Revision 未丢、自动重发并最终收敛。证据：CI/PostgreSQL integration logs。
- [ ] **P2 Capabilities/Concurrency**：不支持 RemoteConfig 或不兼容 Java 配置 Schema 的 Agent 均不下发 Java Sampling JSON，且明确返回 UNSUPPORTED；WebSocket 每连接串行推送，HTTP 模式不谎称实时 WS 推送。证据：race test + protocol tests。
- [ ] **P3 Release**：灰度组失败阻断下一批、回滚产生新审计、跨租户发策略被拒绝；Release 目标 AgentKind 与实例分配一一对应，混合 Kind 下不能错发 Java 配置。证据：批次故障注入和审计记录。
- [ ] **RF Contract**：执行 repofoundry --repo . validate 与 Spec validate、designctl validate、epctl validate；期待零 error。设计批准和 completion 需以后续真实审批、真实 revision 与证据为依据。

尚未完成任何新里程碑；这些复选框都保持未勾选。完成归档必须由 RF epctl archive-ep 写入 verified_revision、verification_evidence 和 seal。

## Idempotence and Recovery

数据库 migration 必须可回滚或提供演练过的前向修复方案；Schema 操作独立于 OpAMP 网络写。重复策略发布按 canonical body hash 幂等，拒绝相同优先级冲突的 Selector。网络中断/配置拒绝不覆盖上次已生效观测值。旧 ACK 只更新历史记录，不更新当前版本完成状态。

若 M1/P2 实现失败，先停止发布入口、保留 DB Desired 状态与 audit，然后将服务切回已验证兼容版本；不能在回滚时删除未应用历史。若需要反向发布旧 PolicyVersion，应通过新的 Audit Event 执行。不要以手工修改 RF 已批准制品修复校验错误。

## Progress

- [x] (2026-10-19) 既有 P0 代码和 RepoFoundryAI Harness/Specs 已存在于 PR #1；Go 和 RF 的既有 CI 校验通过（针对当时提交）。
- [x] (2026-10-19) 建立 DD-001 工作草案和本次 EP-001 计划；当前尚未获得 Design Owner 批准。
- [ ] M1：身份边界、PostgreSQL Registry、断线恢复及验收。
- [ ] M2：版本化策略、配置协调和 ACK 追踪。
- [ ] M3：灰度批次、审计、失败隔离和回滚。
- [ ] 完整验收、Design 决策批准、证据归档。

## Surprises & Discoveries

- (2026-10-19) P0 已含 OpAMP 的 HTTP/WS 接入及测试 Agent，但 Inventory 与期望策略均在内存；此前 Go CI 通过不能推断生产级可靠性。
- (2026-10-19) 官方 opamp-go 的 OnMessage/Send 语义决定：HTTP 通过请求响应下发；WebSocket 需要串行写和连接所有权管理。

## Decision Log

- (2026-10-19, provenance: user direction) 先实现 Control Plane，后实现自定义 Java Agent；不将 Agent 发行版工作纳入本 EP 的完成门槛。
- (2026-10-19, proposal only) 优先模块化 Go 单体 + PostgreSQL + opamp-go，待 DD-001 审阅；不把提案当成 accepted ADR。
- (2026-10-19, delivery rule) 采用 Desired/Observed 双状态和 matching hash ACK，其他能力按 M1–M3 递增交付；SLO 尚未定义。
- (2026-10-19, explicit user scope confirmation) Control Plane 长期面向 Java、Go、Python 等 APM Agent，未来支持 Collector；M1–M3 仅验证 Java 发行版的策略配置和其他 Kind 的类型安全注册，其他 Runtime Adapter 不在 EP-001 交付范围。这是产品范围确认，不是新的 accepted ADR，也不代表 DD-001 正式批准。

## Blockers

| ID | Status | Opened | Resolved | Missing capability | Impact | Unblock or resolution |
| --- | --- | --- | --- | --- | --- | --- |
| BLK-001 | open | 2026-10-19 | | Design Owner 对 DD-001 的正式评审/批准 | 不能作为已批准设计完成 EP 归档 | 审核 DD-001，按 RF designctl 明确审批；实施中的可逆 P1 实验可先进行 |
| BLK-002 | open | 2026-10-19 | | Agent 身份绑定机制和重复 UID 冲突规则的批准契约 | 禁止把当前全局 P0 Token 当生产多租户授权 | 提出选项、风险和测试证据，必要时创建 ADR 并获 Owner 接受 |

## Outcomes & Retrospective

当前计划 active，尚无 M1–M3 实现结果。待实际完成后记录相对目标的行为变化、CI/基准证据、剩余缺口和迁移经验；不得依据计划创建日期填入完成证明。

### Knowledge promotion candidates

- 经验证的身份绑定与 Desired/Observed 机制可在交付后沉淀到 ARCHITECTURE.md 和 RF 规范激活样例；现在不能当成已验证结论。

## Interfaces and Dependencies

- Go toolchain 1.25+；github.com/open-telemetry/opamp-go v0.25.0；当前既有 API 入口为 cmd/server/main.go。
- 目标控制面类型：AgentKind、Distribution、DistributionVersion、CapabilitySet、ConfigDialect、AgentConfigAdapter（拟定接口，尚未存在于代码）；Java Adapter 首先支持 `apm/sampling.json`，Go/Python/Collector Adapter 明确 defer。
- Management endpoints：/api/v1/agents、/api/v1/agents/{uid}、/api/v1/agents/{uid}/sampling；保持 P0 功能兼容，扩展 service-level policy endpoints 时引入版本化契约测试。
- OpAMP transport：/v1/opamp（HTTP + WS），AgentToServer、ServerToAgent.RemoteConfig、RemoteConfigStatus 和 AgentCapabilities。
- PostgreSQL/MySQL（P1 才引入，当前均不存在）：Repository/UnitOfWork Port、两种 SQL Adapter、独立 Migration。候选实体 AgentInstances、PolicyVersions、Assignments、ConfigEvents；活动 Session 不入库。
- Java Agent（后续独立实施）：必须支持 schema_version=1 的 apm/sampling.json 和 matching config hash 应用回报；本 EP 不实现 JVM Sampler。

## Artifacts and Notes

- 本计划：docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md。
- 设计输入：docs/design-docs/dd-001_opamp-control-plane.md；需求：docs/requirements/APM_CONTROL_PLANE.md。
- 现有 P0 PR：https://github.com/XiaoWeiKIN/apm-control-plane/pull/1；Go CI / RF CI 在 PR checks 中可核验。
- 未来测试的完整日志、数据库状态和 OpAMP 消息录制应保存至计划 artifacts/，索引仅记录可核验证据路径；当前未产生这些数据。

## Revision Notes

- 2026-10-19 — 由 RF epctl 创建初始 active EP，基于用户确认的控制平面范围与 DD-001 工作草案；未假造审批和实现验收。
- 2026-10-19 — 更新产品范围：通用 OpAMP Agent Control Plane，预留 Java/Go/Python APM Agent 和未来 Collector；本 EP 实施仍仅限 Java 适配与通用控制面，现有 blocked 状态和未完成验收保持不变。

