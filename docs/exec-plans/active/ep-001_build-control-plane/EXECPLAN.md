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
design_evidence: ["DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2"]
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

交付持久化的 **通用 Go OpAMP Agent Control Plane**：长期可管理 Java、Go、Python APM Agent 并预留 Collector 接入；本 EP 只实现 Java Agent 场景的控制端。运维人员可查询 AgentKind/Distribution 及实例健康，按 Service/Environment 发布 Java 采样策略，并准确区分配置传输与 Agent 实际应用结果。HTTP/WS 断线重连及服务器重启不应丢失 Desired State。部署时选择 PostgreSQL 或 MySQL 作为单一主关系型数据库，不承诺只改 Driver 配置就自动完成历史数据跨引擎迁移。用户验证接口为 Management API、数据库状态、OpAMP RemoteConfig 与 ACK；真实 JVM 采样更新在另一个工作流验收。

## Current Snapshot

- Latest checkpoint: none（本执行计划首次创建）。
- Current milestone: **P1–P3.3 已有合并实现（截至 38524c6，PR #19）**；具体业务验收仍须逐项核实，不以合并代替完整验收。DD-001 Revision 1 已正式发布并通过 RepoFoundry Validation，生产身份与授权方案继续受 BLK-002 阻塞。
- Verified baseline: PR #3 在 Go CI、Race Detector、PostgreSQL/MySQL Goose/Agent API 合同测试及 RF 中通过；当前 Workspace-scoped 清理以其独立 CI 为准；测试 Agent 的人工双进程烟测未执行。
- Next action: 以平台 Workspace 为统一隔离键完成 P1.1 的初始化 Migration、Credential JSON、`auth.Principal.WorkspaceID`、GORM/API 和双数据库测试；随后评审平台 Workspace/IAM 认证、Agent Principal 与 UID 所有权模型。
- Confirmed product scope (2026-10-19): 多语言 APM Agent + 后续 Collector；P1–P3 只交付通用抽象和 Java 适配链路，不把其他语言或 Collector 执行能力算入验收。
- Storage requirement (2026-10-19): 多数据库 Driver（至少 PostgreSQL 和 MySQL）是新增范围；技术候选与风险记录在 docs/architecture/technology-selection.md，必须有两数据库实际合同测试。
- P1 evidence (2026-10-19): PR #3 的 GORM/Goose/Koanf 与 Inventory 持久化已合并，通过 Go/DB/RF CI；本次 Workspace 作用域调整尚需各自最新 CI 和平台授权集成评审。
- Open: DD-001 Revision 1 已批准（证据见本 EP design_evidence）；没有 accepted ADR；生产 TLS、IAM、Agent 身份与 UID 所有权方案尚待正式批准并实现，BLK-002 保持 open。不能把这些条件标记为完成。

## Context and Orientation

仓库入口为 cmd/server/main.go（当前把 OpAMP Gateway、API、内存 Agent 状态耦合在一个文件），cmd/testagent/main.go（HTTP 测试 Agent），cmd/server/main_test.go（协议/HTTP 基础单测），docs/requirements/APM_CONTROL_PLANE.md（FR-001…FR-013），docs/architecture.md（P0 边界）。

目标为“**Universal Control Plane ←OpAMP→ Java/Go/Python APM Agent，以及未来 Collector**”。本 EP 的代码路径只有自定义 Java Agent + HTTP Test Agent。通用控制面不得绑定 Java Sampler 接口：注册的 AgentKind/Distribution 与版本/Capabilities 必须独立于 service.name，配置下发经 AgentKind Adapter 编译。JVM 侧单独负责动态采样。管理 REST API 面向人/系统，OpAMP 面向 Agent；OTLP 数据不经过这里。

P1 首先拆模块并持久化；P2 在此基础上引入策略版本和协调器；P3 实现发布与可审计回滚。三个阶段是独立可验收增量，不能以文档完成替代协议结果。

## Constraints and References

| Source | Required constraint | Check |
| --- | --- | --- |
| docs/requirements/APM_CONTROL_PLANE.md | FR-001…FR-013 与 P1–P3 范围 | 按 FR 做验收映射 |
| docs/design-docs/dd-001_opamp-control-plane.md | Approved Design Revision 1; immutable evidence pin `DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2` | 设计批准已验证；生产 IAM/安全方案及 EP 实现验收仍独立受控 |
| docs/.engineering/specs.lock.json | EngineeringSpecifications v1.9.0 lock | RF spec validate + 精确 Requirement 激活 |
| cmd/server/main.go | 现有 opamp-go 回调、P0 Token、内存模型 | Go tests/race and protocol tests |
| docs/SECURITY.md | 未知项不虚构；严禁未认证范围下发 | 负向权限测试 |
| docs/architecture/technology-selection.md | 多 Driver 技术候选及双方言实现/验证 | PostgreSQL/MySQL Contract Suite |

不变量：数据库 Domain Port 与具体 Driver 解耦；PostgreSQL/MySQL 的提交原子性和 CAS 行为必须经合同测试证明。Desired 和 Observed 分离；配置 Hash 与 Revision 分离；Agent 成功接收消息不等于 APPLIED；身份声明属性无授权力；相同配置下发幂等；控制故障不影响 OTLP。AgentKind 不代表能力：必须按 Capabilities + ConfigDialect + 发行版版本解析，缺少能力时拒绝 Java 专有策略。

## Research and Architecture Inputs

- Research gate: not_required。已经由明确用户路线、现有 P0 实现及 opamp-go 公开接口建立实施初始事实，不虚构 concluded Research。
- Architecture decision gate: not_required for plan creation。Go + opamp-go + 首期自定义 Java Agent 和先交付 Control Plane 已由用户选择；2026-10-19 用户进一步确认未来多语言 APM Agent 及 Collector 的产品范围；本计划不接受未批准 ADR。若 mTLS/Token、重复 UID 或 HA 的长期方案需要正式选择，先创建 proposed ADR 并经 Owner 接受再实施该部分。
- Architecture compliance: applicable。精确 Design 输入路径为 docs/design-docs/dd-001_opamp-control-plane.md，architecture entrypoint 为 docs/design-docs/index.md。DD-001 Revision 1 已发布为 current，证据为 `DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2`；设计发布不等于 EP 验收完成，BLK-002 继续阻塞归档。
- ADR references: []；ADR constraint evidence: []；approved Design evidence: [`DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2`]（授权记录 commit 1038ffeb；发布 commit 2101af6d；RepoFoundry Validation [run 40179601964](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/40179601964) success）。不存在的 accepted ADR 不得虚构。
- Source-backed evidence: PR #1 的 P0 代码及 GitHub Actions 绿色 Go / RF 校验，只能证明当前范围的测试通过。
- 负面边界：没有证明 JVM 热更采样、生产流量 SLA、跨实例同步或批量发布可靠性；这些均保留为单独的后续验收。

## Architecture Compliance Matrix

| ADR constraint or architecture input | Implementation or preservation | Verification |
| --- | --- | --- |
| docs/design-docs/dd-001_opamp-control-plane.md | Preserve Control/Data separation, credential-bound Agent identity, immutable policy versions, Desired vs Observed state, driver-neutral Repository/UnitOfWork with PostgreSQL/MySQL storage adapters, capability checks, AgentKind/Distribution-aware configuration adapters, and HTTP/WS delivery semantics; Java remains the only first-delivery config dialect; DD-001 rev1 is approved, while unresolved production identity/IAM and HA choices remain separately gated | Protocol integration tests, PostgreSQL/MySQL shared Repository contract suite, restart and role-boundary negative tests, stale ACK/concurrency tests |

There are no accepted ADR constraints to map. docs/design-docs/index.md is the navigation entrypoint, not an extra normative constraint row.

## Benchmark Gate Set

- Required Scenario IDs: []。P1/P2 正确性、授权和故障恢复使用确定性集成测试；暂无用户批准的生产规模或 SLO，不制造性能基准数字。
- 在开始规模/延迟优化前，使用 RF engineering-benchmark **预声明** BS 场景（连接数、消息大小、掉线率、下发 p95 和内存），再执行并封存结果；若测试推翻单体方案，返回 Research/ADR。当前 EP 不以未创建的 Benchmark Run 声称通过。

## Plan of Work

先锁定 P0 测试行为与安全缺口，把当前 main.go 分拆为 HTTP 装配、OpAMP Gateway、通用 Agent Registry 与 Admin API，保持现有路由兼容。通用 Agent Identity 包含 AgentKind/Distribution/Version/Capabilities，避免把业务 Service 版本当 Agent 发行版版本。P1 使用 database/sql 的 Repository/UnitOfWork Port + GORM PostgreSQL/MySQL Dialector 存储 Agent Identity 与 Observed Snapshot；统一使用 GORM Repository，只有 Migration DDL 分方言；同一业务行为合同测试；Session 保留在内存。

P2 将通用 Configuration/Revision/Assignment 与 Java SamplingPolicy 解耦；通过可显式拒绝不兼容目标的 `AgentConfigAdapter` 把 Java Head Sampling 规则编译为 `apm/sampling.json`。按 service/environment/instance 解析分配并存入 Desired State，Reconciler 对接 opamp-go，区分 HTTP 拉取与 WebSocket 推送，使用 ACK Hash 更新 Observed。P3 追加带审计的受控 rollout、失败隔离、显式 rollback 发布和必要的容量测量。

## Milestones

### M1 — P1 Durable Agent Inventory / Identity

修改 internal/opamp、internal/registry、internal/storage（Repository Port + GORM PostgreSQL/MySQL Dialector）、db/migrations/postgres、db/migrations/mysql 和 cmd/server 的装配。新增 AgentKind/DistributionVersion/Capabilities 字段与服务身份映射（Collector 不强制关联 Java Service）。可观察结果：已认证 Agent 的状态持久化；数据库或服务端重启后保留身份与 Last Seen；重复 UID 和工作空间越权请求被拒绝或明确标记。保持现有 P0 API 兼容。

验证范围：使用 Java、Go、Python、Collector 的**身份元数据样本**验证 Registry 通用性；不要求后三类 Agent 运行时真实接入。

验收：执行 go test -race ./...；在 PostgreSQL 与 MySQL 的本地/CI 集成环境分别启动 Agent、重启服务、查询 GET /api/v1/agents/{uid}，验证对应记录未消失并可回连；运行未认证与跨工作空间负向用例。保存测试结果/CI Run URL。

### M2 — P2 Sampling Policy + Reconciler

修改 internal/policy、internal/config、internal/reconcile、storage migrations、Management API，增加不可变通用 ConfigRevision、ConfigRelease/Assignment 与 deterministic hash，并只实现 Java Sampling AgentConfigAdapter。可观察结果：在 Admin API 发布 rate 0.1 后，在线 HTTP Test Agent 下一次轮询收到 Config；匹配 Hash 的 APPLIED 将状态变为 APPLIED；服务端重启后待下发任务被恢复；不支持 RemoteConfig 或不兼容 Java ConfigDialect 的 Agent 不会收到 Java JSON，也不被误标成功。

验收：Go 集成测试覆盖 stale ACK、重复 ACK、配置冲突、离线/重连、错误配置、WS writer 序列化及 HTTP polling，使用真实 OpAMP Client/Server 对话而非仅 mock，并在 PostgreSQL/MySQL 各运行同一业务场景。

### M3 — P3 Audited Rollout / Recovery

增加发布批次、并发上限、进度统计、失败阈值与显式回滚，以及审计事件；Release 必须记录目标 AgentKind、具体配置 Schema 及目标集合，不跨不同 Agent Kind 默认复制同一策略。可观察结果：小批量 Agent 先确认，再进入后续批次；失败时停止扩散；指向旧版本的回滚有独立审计记录。

验收：故障注入与回滚回放；持久性/权限测试；新建的 Benchmark Scenario 必须先声明再执行，不得事后调整门槛。

## Concrete Steps

项目根目录为仓库根（相对路径均从这里开始）。

1. RF 状态与范围：执行 repofoundry --repo . validate、repofoundry --repo . spec validate，读取 docs/requirements 和 DD-001；对 Go + security + data boundaries 精确激活适用规范。
2. 验证起点：执行 go mod tidy、go test -race ./...、go vet ./...；查看 cmd/server/main.go 的现有接口，不静默修改其路由。
3. M1：直接实现 Koanf 启动配置、GORM 统一 Repository/UnitOfWork、PostgreSQL/MySQL Dialector 和 Goose 两套 Migration，不再安排独立 ORM 选型 PoC；在功能交付过程中执行 schema 升级、事务回滚、CAS、Agent Identity/Observation、断线重启及越权测试。
4. M2：实现 Policy Resolver、Immutable Revision、Reconciler 和 Config Status 归因。使用 cmd/testagent 做 HTTP 链路，再增加 WebSocket 测试客户端。
5. M3：定义受控发布批次、审计和回滚。只在证据足够后确定默认超时、容量和 SLO。
6. 每阶段更新本 EXECPLAN.md 的 Current Snapshot、Progress、验收结果、Revision Notes；执行 epctl validate、designctl validate 和 RF validate。

脚本所在 RF 发行包由仓库 PROJECT_WORKFLOW.md 的固定版本安装；不要在 Git 中手工制造 ADR/EP 编号。

## Validation and Acceptance

- [ ] **P1 Identity**：从仓库根执行 go test -race ./...，并在 PostgreSQL 与 MySQL 运行同一套 Agent Registry/Repository 合同测试；期待凭证隔离、重复 UID 检测和重启后记录可恢复。证据：CI Job URL + test output。
- [ ] **P1 Compatibility**：GET /api/v1/agents 与 GET /api/v1/agents/{uid} 保持兼容；非法 UID/越权返回 4xx；Java/Go/Python/Collector 身份样本应正确区分 AgentKind、Distribution 与业务 Service；不声称四种 Agent 都可实际连接。证据：HTTP contract + Registry tests。
- [ ] **P2 Policy**：发布 rate=0.1，测试 Agent 在下一次轮询收到 schema=1 的 JSON；相同 Hash 的 APPLIED 更新 Observed，旧 ACK 不覆盖新 Desired。证据：OpAMP integration trace + DB assertions。
- [ ] **P2 Reconciliation**：先发布后断开 Agent、重启控制平面、重新接入；期待 Desired Revision 未丢、自动重发并最终收敛。证据：PostgreSQL/MySQL CI integration logs。
- [ ] **P2 Capabilities/Concurrency**：不支持 RemoteConfig 或不兼容 Java 配置 Schema 的 Agent 均不下发 Java Sampling JSON，且明确返回 UNSUPPORTED；WebSocket 每连接串行推送，HTTP 模式不谎称实时 WS 推送。证据：race test + protocol tests。
- [ ] **P3 Release**：灰度组失败阻断下一批、回滚产生新审计、跨工作空间发策略被拒绝；Release 目标 AgentKind 与实例分配一一对应，混合 Kind 下不能错发 Java 配置。证据：批次故障注入和审计记录。
- [ ] **RF Contract**：执行 repofoundry --repo . validate 与 Spec validate、designctl validate、epctl validate；期待零 error。设计批准和 completion 需以后续真实审批、真实 revision 与证据为依据。

尚未完成任何新里程碑；这些复选框都保持未勾选。完成归档必须由 RF epctl archive-ep 写入 verified_revision、verification_evidence 和 seal。

## Idempotence and Recovery

数据库 Migration 应为 PostgreSQL/MySQL 分别提供演练过的前向修复/受控回退措施；MySQL DDL 隐式提交不能被误认为有 PostgreSQL 等价的事务回滚语义。Schema 操作独立于 OpAMP 网络写。重复策略发布按 canonical body hash 幂等，拒绝相同优先级冲突的 Selector。网络中断/配置拒绝不覆盖上次已生效观测值。旧 ACK 只更新历史记录，不更新当前版本完成状态。

若 M1/P2 实现失败，先停止发布入口、保留 DB Desired 状态与 audit，然后将服务切回已验证兼容版本；不能在回滚时删除未应用历史。若需要反向发布旧 PolicyVersion，应通过新的 Audit Event 执行。不要以手工修改 RF 已批准制品修复校验错误。

## Progress

- [x] (2026-10-25) DD-001 Revision 1 已按 Design Snapshot/Manifest 契约发布；设计证据 `DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2` 通过同 SHA 的 RepoFoundry Validation；BLK-001 resolved，BLK-002 仍 open。

- [x] (2026-10-19) 既有 P0 代码和 RepoFoundryAI Harness/Specs 已存在于 PR #1；Go 和 RF 的既有 CI 校验通过（针对当时提交）。
- [x] (2026-10-19) 建立 DD-001 工作草案和本次 EP-001 计划；当前尚未获得 Design Owner 批准。
- [x] (2026-10-19) P1.1 代码草案：完成 Koanf 配置、GORM 双 Driver、Goose Schema、基本凭证到 Workspace/UID 绑定与持久化 Inventory；PR #3，非已批准生产实施。
- [ ] M1：身份边界、PostgreSQL/MySQL Registry 及双数据库合同测试、断线恢复和验收。
- [ ] M2：版本化策略、配置协调和 ACK 追踪。
- [ ] M3：灰度批次、审计、失败隔离和回滚。
- [ ] 完整验收、BLK-002 生产身份与授权门禁、证据归档。

## Surprises & Discoveries

- (2026-10-19) P0 已含 OpAMP 的 HTTP/WS 接入及测试 Agent，但 Inventory 与期望策略均在内存；此前 Go CI 通过不能推断生产级可靠性。
- (2026-10-19) 官方 opamp-go 的 OnMessage/Send 语义决定：HTTP 通过请求响应下发；WebSocket 需要串行写和连接所有权管理。

## Decision Log

- (2026-10-20, user direction) 控制面资源作用域是整个可观测系统的 Workspace，而非独立 Tenant；确认无需要兼容的旧生产数据，本增量将初始数据库、Go 领域命名和凭证配置直接统一为 `workspace_id`，不引入旧列双写、历史映射或回填。平台统一 IAM/Workspace 真实性验证仍保留安全设计任务。

- (2026-10-19, provenance: user direction) 先实现 Control Plane，后实现自定义 Java Agent；不将 Agent 发行版工作纳入本 EP 的完成门槛。
- (2026-10-19, proposal only) 优先模块化 Go 单体 + PostgreSQL + opamp-go，待 DD-001 审阅；不把提案当成 accepted ADR。
- (2026-10-19, delivery rule) 采用 Desired/Observed 双状态和 matching hash ACK，其他能力按 M1–M3 递增交付；SLO 尚未定义。
- (2026-10-19, user scope) 数据库驱动可配置，可能采用 MySQL；新增 PostgreSQL/MySQL 双实现与真实合同测试。
- (2026-10-19, implementation direction) 用户同意直接落地成熟的 GORM v2 + Goose + Koanf v2 方案，取消独立技术选型 PoC 前置阶段。数据库事务、CAS、迁移和重启恢复的测试仍是 P1 功能验收要求；此指令不等于 DD-001 正式批准或身份安全方案批准。
- (2026-10-19, explicit user scope confirmation) Control Plane 长期面向 Java、Go、Python 等 APM Agent，未来支持 Collector；M1–M3 仅验证 Java 发行版的策略配置和其他 Kind 的类型安全注册，其他 Runtime Adapter 不在 EP-001 交付范围。这是产品范围确认，不是新的 accepted ADR，也不代表 DD-001 正式批准。

## Blockers

| ID | Status | Opened | Resolved | Missing capability | Impact | Unblock or resolution |
| --- | --- | --- | --- | --- | --- | --- |
| BLK-001 | resolved | 2026-10-19 | 2026-10-25 | DD-001 Revision 1 正式 Design 审批与不可变快照证据 | 设计门禁已满足；不解除独立生产安全阻塞 | Owner 授权 [commit 1038ffeb](https://github.com/XiaoWeiKIN/apm-control-plane/commit/1038ffebd1ea694c0612af460c7f0643190fecd0)；发布 [commit 2101af6d](https://github.com/XiaoWeiKIN/apm-control-plane/commit/2101af6d59693843b283540e9615d8366bc02e2c)；RepoFoundry Validation [run 40179601964](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/40179601964) success；Evidence Pin `DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2` |
| BLK-002 | open | 2026-10-19 | | Agent 身份绑定机制和重复 UID 冲突规则的批准契约 | 禁止把当前全局 P0 Token 当生产多工作空间授权 | 提出选项、风险和测试证据，必要时创建 ADR 并获 Owner 接受 |

## Outcomes & Retrospective

当前计划 active，尚无 M1–M3 实现结果。待实际完成后记录相对目标的行为变化、CI/基准证据、剩余缺口和迁移经验；不得依据计划创建日期填入完成证明。

### Knowledge promotion candidates

- 经验证的身份绑定与 Desired/Observed 机制可在交付后沉淀到 ARCHITECTURE.md 和 RF 规范激活样例；现在不能当成已验证结论。

## Interfaces and Dependencies

- Go toolchain 1.25+；github.com/open-telemetry/opamp-go v0.25.0；当前既有 API 入口为 cmd/server/main.go。
- 目标控制面类型：AgentKind、Distribution、DistributionVersion、CapabilitySet、ConfigDialect、AgentConfigAdapter（拟定接口，尚未存在于代码）；Java Adapter 首先支持 `apm/sampling.json`，Go/Python/Collector Adapter 明确 defer。
- Management endpoints：/api/v1/agents、/api/v1/agents/{uid}、/api/v1/agents/{uid}/sampling；保持 P0 功能兼容，扩展 service-level policy endpoints 时引入版本化契约测试。
- OpAMP transport：/v1/opamp（HTTP + WS），AgentToServer、ServerToAgent.RemoteConfig、RemoteConfigStatus 和 AgentCapabilities。
- PostgreSQL/MySQL（P1 才引入，当前均不存在）：Repository/UnitOfWork Port、一套 GORM Repository、独立 Migration。候选实体 AgentInstances、PolicyVersions、Assignments、ConfigEvents；活动 Session 不入库。
- Java Agent（后续独立实施）：必须支持 schema_version=1 的 apm/sampling.json 和 matching config hash 应用回报；本 EP 不实现 JVM Sampler。

## Artifacts and Notes

- 本计划：docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md。
- 设计输入：docs/design-docs/dd-001_opamp-control-plane.md；需求：docs/requirements/APM_CONTROL_PLANE.md。
- 现有 P0 PR：https://github.com/XiaoWeiKIN/apm-control-plane/pull/1；Go CI / RF CI 在 PR checks 中可核验。
- 未来测试的完整日志、数据库状态和 OpAMP 消息录制应保存至计划 artifacts/，索引仅记录可核验证据路径；当前未产生这些数据。

## Revision Notes

- 2026-10-25 — 记录 DD-001 Revision 1 的 Owner 授权、不可变 Design Snapshot 和 Evidence Pin；截至发布 commit 2101af6d 的 RepoFoundry Validation 通过。BLK-001 已解除；BLK-002 和 EP-001 blocked 状态保留。旧进度条目属于 2026-10-19 历史快照，未勾选新验收。

- 2026-10-20 — P1.1 Workspace-scoped 命名/初始化数据模型整理作为独立实现增量推进。保留 EP-001 blocked 状态与身份安全审批门禁。

- 2026-10-19 — 由 RF epctl 创建初始 active EP，基于用户确认的控制平面范围与 DD-001 工作草案；未假造审批和实现验收。
- 2026-10-19 — 更新产品范围：通用 OpAMP Agent Control Plane，预留 Java/Go/Python APM Agent 和未来 Collector；本 EP 实施仍仅限 Java 适配与通用控制面，现有 blocked 状态和未完成验收保持不变。
- 2026-10-19 — 同步关系型数据库多 Driver 需求；P1 需要验证 PostgreSQL 与 MySQL，保持 blocked 状态和未完成验收记录。
- 2026-10-19 — P1.1 代码及数据库合同测试已经开始纳入 PR #3；DD-001 未获正式批准且生产身份/会话所有权未决，因此本计划继续 blocked，M1 不提前勾选。

