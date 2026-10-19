---
schema_version: "1.1"
metadata_schema: "1"
artifact_type: design-doc
id: DD-001
doc_type: design
layout: single
title: "OpAMP APM Agent Control Plane"
status: draft
working_revision: "1"
published_revision: "0"
research_refs: []
research_not_required_reason: "The user selected a custom Java Agent, Go OpAMP control plane and phased delivery; existing P0 code and upstream server API establish this baseline without an unresolved factual choice for the first design draft."
adr_refs: []
design_dependencies: []
decision_not_required_reason: ""
approved_by: ""
approved_at: ""
approval_ref: ""
superseded_by: ""
terminal_reason: ""
revision_reason: ""
author: "AI assistant"
owner: "Unassigned"
created: 2026-10-19
updated: 2026-10-19
---

# OpAMP APM Agent Control Plane

This document is the entrypoint for `DD-001`. The logical Design and all
managed package members share one review and approval boundary.

## Design Summary

本设计定义 Go 语言的 **通用 Agent Control Plane**：长期目标是通过 OpAMP 集中管理 Java、Go、Python 等 APM Agent，并在后续引入 OpenTelemetry Collector 等 Agent 类型。首期只交付自定义 OpenTelemetry Java Agent 发行版的管理能力。不同 Agent 采用相同的连接、资产、配置版本、分配和回执管理机制，但不强制它们共享同一套配置格式或运行时热更新语义。

Java APM Agent 的 Traces 仍通过 OTLP → Collector → Backend 发送；控制平面不转发任何 Span。Collector 将来作为 **被管理 Agent** 接入时，仍以其独立 OTLP 数据链路发送遥测数据，不能将 Collector 的控制连接与数据接收混为一谈。

目前 P0 已有基于 opamp-go v0.25.0 的 HTTP/WebSocket 接入、内存 Registry、Agent 专属采样 JSON 和测试 Agent；CI 验证 Go 单测及 Vet。目标演进为支持 PostgreSQL、MySQL 等关系型 Driver 的持久 Desired State + Agent Observed State + 可恢复 Reconciler。数据库选型可由部署配置切换，但不表示数据库 SQL 方言、DDL 或历史数据可以自动跨引擎迁移。设计状态是 **待评审草案**，不等于现有实现，也未获得 Design Owner 批准。

主要代价：必须处理身份绑定、顺序与幂等、Agent 离线、WebSocket 单连接写序列化以及 HTTP 拉取场景。第一版采用模块化单体；存储通过 Repository/UnitOfWork Port 隔离数据库实现，**首批目标数据库为 PostgreSQL 和 MySQL**。两者均需运行真实引擎的 Repository 合同测试；数据库方言 SQL 与 Migration 允许分别维护。HA 与发布性能目标需要测量后另行决定。

## Goals and Non-goals

**目标（对应 docs/requirements/APM_CONTROL_PLANE.md）**

- FR-001/002：稳定记录 Agent 实例、服务/环境声明、协议能力、连接新鲜度和独立健康状态。
- FR-003/004：通过环境/服务 Selector 解析版本化 Head Sampling 策略，在 Agent 声明 AcceptsRemoteConfig 时经 OpAMP 发送。
- FR-005/006：持久记录期望版本、发送状态、实际上报的 Hash/状态；重连或服务端重启后恢复一致。
- FR-007/008：受控灰度与审计；服务端权限和 Agent 身份隔离；只将可认证 Principal 授予的范围用于授权。
- FR-009：提供供自定义 Java 发行版消费的采样配置契约；JVM 热切换本身在另一个工作流验收。
- FR-010：将 Agent Kind、Distribution、版本、Capabilities、配置 Schema 能力建模为显式属性，避免将 Java SDK 语义写进通用注册和分发模块。
- FR-011：平台可挂载多语言 APM Agent 和 Collector 的 Config Adapter，按每种 Agent 实际支持的配置格式和生效方式进行编译与能力校验；非 Java Adapter 不属于首期交付。
- FR-012：区分 Service / Agent Instance / Agent Session；Service 级策略只作用于授权且实际匹配的 Agent，不把 Collector 的运行组件误当成 Java Service 实例。
- FR-013：关系型数据库约束：Control Plane 能通过驱动配置在 PostgreSQL、MySQL 的 Storage Adapter 之间选择。无论具体存储引擎，Agent Identity/Desired/Observed、CAS、事务发布和重启恢复的业务语义必须一致；首期不能假定 PostgreSQL 专有功能可移植。

**首期非目标**：不实现 Go/Python Agent Runtime Adapter、Collector 配置执行或 Collector Supervisor、OTLP 传输、Java Bytecode Instrumentation、二进制升级、Trace Retention/Tail Sampling、首期多活 HA，也不承诺无证据的吞吐量或采样准确率。**预留 Collector 适配能力不代表 Collector 已获得可用的配置管理能力。**

## Research and Decision Inputs

**已核实证据**：官方 OpAMP Go Server 通过 Attach() 接入 net/http，OnConnecting 负责连接级身份检查，OnMessage 返回 ServerToAgent；HTTP 模式的响应式下发与 WebSocket 推送语义不同。参考 upstream server/server.go、server/types/callbacks.go 与已有 cmd/server/main.go。P0 测试 Agent 仅确认 JSON 与回传状态，不能证明 JVM 动态采样成功。

**研究门禁**：本次方案由明确的项目方向（自定义 Java Agent 发行版、Go Control Plane、OpAMP 和先做控制端）及已检验源码接口直接确定，暂不创建虚构的 Concluded Research。需研究的问题列于末尾，在影响接口或安全决策前单独升级 RF Research/ADR。

**已确认的产品范围（2026-10-19 用户明确回答）**：未来统一管理 Java、Go、Python 等 APM Agent，后续可能管理 Collector；Java 是首个实际交付的 Agent Kind。这个确认属于设计输入，不代表本 DD 获得正式 Design approval 或已有 accepted ADR。

**正式决定**：仓库当前没有 accepted ADR。本文比较候选架构，但不会把用户的实施授权记为 ADR 接受。数据库支持范围与两类引擎的 Schema/Migration 方言、Agent 凭证机制及批量灰度语义仍需审查；未来如涉及长期不可逆选择，应创建 proposed ADR 并由决策责任人明确接受。

## System Context and Invariants

~~~mermaid
flowchart TB
    UI["Management API / future UI"] --> P["Policy + Config Manager"]
    P --> C["Config Resolver + AgentKind Adapter"]
    C --> PG[("Relational Storage Port · Desired/Observed")]
    PG <--> R["Reconciler / Rollout"]
    PG --> PSQL["PostgreSQL Adapter"]
    PG --> MYSQL["MySQL Adapter"]
    R --> G["OpAMP Gateway · opamp-go"]
    G <-->|"OpAMP"| J["Java Agent · P0/P1 target"]
    G -.->|"Future OpAMP"| L["Go/Python Agent"]
    G -.->|"Future OpAMP"| O["Collector Agent"]
    J -->|"OTLP Data Plane"| D["Collector / OTLP Receiver"]
    L -.->|"OTLP Data Plane"| D
    O -.->|"Telemetry Data Plane"| D
    D --> B["APM Backend"]
~~~

**必须保持的边界**：
1. OPAMP 是控制面；OTLP 是数据面，两者不互相代发。
2. Instance UID 是 Agent 实例标识，不作为租户授权证明；service.name/environment 也是可伪造元数据。生产配置下发之前必须完成凭证到租户/Agent 范围的绑定。
3. Desired Policy Revision 不可变，Assignment 指向具体版本；Observed Hash/ConfigStatus 来自 Agent。发送成功不等于 APPLIED，只有同 Hash 的应用报告可更新对应 Assignment。
4. 连接在线和运行健康是独立状态；会话断开只能说明控制连接不可用，不能判定 JVM 已停止。
5. 规范化序列化产生稳定配置 Hash；重复下发同一策略必须幂等。
6. 避免在 OnMessage 持有数据库事务期间阻塞 WebSocket 网络写；每连接发送必须串行化。
7. 任意错误远程配置不得使服务器丢失上一个可用 Desired Revision。
8. `AgentKind`、`Distribution`、`CapabilitySet` 与 `ConfigDialect` 是显式的管理属性；`service.name` 是工作负载身份线索，不是所有 Collector 都必须具备的 Java 服务标识。
9. Config Resolver 只能生成 Agent Kind 已支持的 RemoteConfig 内容。未知类型、未知 Schema、缺少 `AcceptsRemoteConfig` 或不能满足运行时语义时，必须明确返回 Unsupported/Rejected，不能推测为热更新成功。
10. 有共同的 Config Delivery 状态机不代表不同 Agent 的配置解释、重启需求或采样语义相同。
11. 数据库存储端口以业务语义为契约（AgentKey、Assignment、CAS、UnitOfWork），不得从 Domain 暴露 *sql.DB、pgxpool、方言 SQL 或 Driver 错误码。PostgreSQL 与 MySQL 的 Migration、查询和数据类型可以分别实现，但必须有同一套行为验收。
12. 一个 Control Plane 实例在启动时选择一种主数据库；切换驱动不构成已实现的跨引擎数据迁移、双写或全局分布式事务。

## Proposed Architecture

**当前 P0**：cmd/server/main.go 同时承载协议、HTTP Admin API 和线程安全内存 Registry，cmd/testagent/main.go 模拟 OpAMP HTTP 拉取与 ACK。P0 仅测试通信路径，不应当宣称为生产控制平面。

**目标模块（模块化单体）**：
- cmd/server：装配依赖、启动/关闭与 HTTP Server 生命周期。
- internal/opamp：OpAMP Attach/Callbacks、Agent Capabilities、请求认证、实例连接与消息序列；协议结构不渗入 Policy Domain。
- internal/registry：Agent Identity、AgentKind、Distribution、声明属性、CapabilitySet 和最近 Observed Snapshot；持久化 Agent Instance 与内存 Session 分离。Service（Tenant + Environment + Namespace + Name）作为 APM 工作负载关联；Collector 不强制进入同一服务层级。
- internal/policy：通用 Policy/Revision/Selector 与目标类型约束，首期只有 Java Head Sampling Policy；冲突采用显式优先级，优先级相同且内容不同直接拒绝发布，不能依赖遍历顺序。
- internal/config：通用配置 Revision、Assignment、Config Release、ConfigHash、ConfigStatus；`AgentConfigAdapter` 按 AgentKind/Distribution/Schema/Capabilities 编译成不可变 AgentConfigMap。首期只有 `java-otel` 适配器；后续 Go/Python/Collector 适配器由各自配置契约提供，不复用 Java JSON 作为所有发行版的公共标准。
- internal/reconcile：按 Agent desired hash 与 reported applied hash 协调，监听重连、发布、ACK 和失败重试。
- internal/storage：驱动无关的 Repository/UnitOfWork 契约，使用 GORM v2 统一 CRUD 和事务，通过 gorm.io/driver/postgres 与 gorm.io/driver/mysql 选择驱动，连接池使用 database/sql。数据库物理 Schema 和 Migration 可以按方言区分；普通 CRUD 由一套 GORM Repository 实现，必要的方言差异和错误分类在存储层局部处理；避免把网络连接对象持久化。
- internal/api：查询 Agent、创建策略版本、发布与回滚请求；鉴权与租户授权在入口完成。

**候选持久实体（逻辑模型，非 PostgreSQL 专有 Schema）**：agent_instances（UID、AgentKind、Distribution、版本、已认证范围）、agent_observations（最后时间、Capabilities、Health、Observed Config）、apm_services（租户/环境/Namespace/Name）、policy_versions（不可变规则）、policy_bindings（Scope/Selector）、config_revisions（类型化配置正文）、config_releases（一次发布的目标/批次/审计）、agent_assignments（AgentUID、Desired revision/hash、Observed hash/status）、config_events（append-only 审计）。活动的 agent_sessions 不持久化 Connection 对象。逻辑实体可在 MVP 合并实现，但不能合并生命周期语义。这是一份待验证的数据模型，不是现有数据库表。

**首期 Java 配置契约**：RemoteConfig ConfigMap 的名为 `apm/sampling.json`，ContentType 为 `application/json`；Body 包含 `schema_version`、`sampling.type = parent_based_trace_id_ratio`、`sampling.rate ∈ [0,1]`。这只是我们对 Java 发行版的自定义契约，不代表 OpAMP 或其他语言 Agent 的规范字段。未来 Collector Adapter 必须输出它实际理解的配置对象，并明确 apply/restart 能力，不得把 Java 的动态采样能力外推到 Collector。

## Control and Data Flows

**1. 任意 Agent 上线/重连（首期验证 Java Agent）**

- 网络入口验证凭证 → 绑定租户/授权范围 → OnMessage 校验 16-byte InstanceUID、序列与 Capabilities → 读取 AgentKind/Distribution/Version 元数据 → 更新 Agent 的 Observed Snapshot。未认证的 AgentKind 声明不能扩大凭证授权。
- AgentDescription 可部分上报；出现断序或服务端缺少快照时请求 ReportFullState。重复 UID 并发会话应制定显式冲突规则并测试，新建 Session 不盲目获得旧 Session 连接所有权。
- Reconciler 读取持久 Assignment：若代理支持远程配置且 reported hash 未达到 desired hash，下发完整配置；否则保持 Pending/Unsupported 并显式暴露原因。

**2. 类型化策略发布（Java 采样策略先行）**

- Admin API 验证权限、目标 AgentKind、Java rate [0,1]、Selector 唯一性和冲突 → 解析每个目标的 `AgentConfigAdapter` 及协议 Capabilities → 在数据库事务中写入不可变 PolicyVersion、ConfigRevision、ConfigRelease 与目标 Assignment → 事务提交后触发 Reconcile。无法匹配的目标单独显示 UNSUPPORTED，不强行下发。
- 服务端为所有目标生成确定性 Body 和 Hash。WebSocket 在线实例通过同连接的串行发送器推送；HTTP Agent 在后续轮询时获取。任何队列事件丢失可由周期性 Reconcile 修复。
- 状态从 PENDING → SENT/WAITING_ACK → APPLIED 或 ERROR/TIMED_OUT；只允许 matching hash 的 APPLIED 关闭当前版本，旧 ACK 保留历史但不得覆盖新 Desired。

**3. 失败和回滚**

- 任一所选数据库的发布事务提交失败都不得发送新配置；网络发送失败不回滚已提交 Desired；下次连接或定期 Reconcile 重试。MySQL DDL 的隐式提交与 PostgreSQL DDL 的事务行为不能被视为相同，Schema Migration 的失败修复必须独立设计。
- Agent 报告拒绝或非法配置时保留最后一次已知适用版本，记录错误；若需恢复旧策略，创建新的发布动作指向已验证旧版本，记录发起人与审计事件。
- 控制平台断线不能阻断业务 JVM 的 OTLP Exporter。HTTP 与 WebSocket 都必须做协议级端到端测试，不用 HTTP 轮询模拟 WebSocket 推送成功。

## Alternatives, Open Questions, and Revisit Triggers

**选择记录（待 Design Owner 审阅）**

| 方案 | 优点 | 代价及当前处理 |
| --- | --- | --- |
| opamp-go + 模块化单体 + database/sql + GORM PostgreSQL/MySQL Dialector | 统一 Domain/Release 语义，多种环境可配置关系型数据库 | 双方言 Migration 与一致性测试成本；HA 仍需设计 |
| 直接 Fork 历史 Bindplane Server | 现成管理模型 | 上游历史过久，迁移及安全审计成本难估；不选 |
| PostgreSQL-only pgxpool + Tern | PostgreSQL 原生功能更丰富，PG 方案可快速实现 | 不满足 MySQL 等多 Driver 的产品约束，不作为基础架构 |
| Redis/Kafka + 多个 Gateway 服务 | 跨实例消息路由和吞吐扩展 | 当前没有规模证据，不引入 |
| HTTP-only 管理 | 不需要驻留 WS 会话 | 配置只在轮询时生效；兼容保留但不能当推送 |

**数据库候选技术**：GORM v2 + PostgreSQL/MySQL Dialectors + Goose Migration + Koanf v2，参见 [P1–P3 技术选型](../architecture/technology-selection.md)。这些仍是待验证的组件提议，不代表已批准的 ADR 或已在 go.mod 中引入。

**未决问题**：PostgreSQL/MySQL 兼容范围与测试版本、MySQL DDL Migration 恢复、两库事务隔离级别/CAS 并发行为以及未来是否增加第三种 Driver；服务的命名与租户分组边界、AgentKind/Distribution 识别和能力来源、跨发行版版本兼容性、Collector 的配置职责与 Kubernetes Operator 之间的边界；Agent 身份绑定采用 mTLS 证书还是租户 Scoped Token；重复 UID 应如何换绑；Service/Environment Selector 的授权边界；严格回滚与 ACK timeout 默认值；生产规模和目标 SLO；Go Server v0.25.0 与 Java Client v0.14.0 规范差异。无法由当前 P0 单测证明这些约束。

**重访条件**：出现多实例部署需求、经过测量的连线数量或下发尾延迟超出单体能力、跨租户配置泄漏风险、Java Client 互操作失败、真实 JVM 采样行为与反馈不一致。

## Package Document Map

Single-file Design：本文件是 DD-001 的完整阅读入口；模块细节经批准后再独立拆分。

## Revision Notes

- 2026-10-19 — Draft revision 1, derived from explicit product requirements and current P0 source. Not yet approved.
- 2026-10-19 — 用户确认未来统一管理 Java/Go/Python APM Agent 并扩展 Collector。将 AgentKind/Distribution/Capabilities/ConfigAdapter 作为类型边界加入工作草案；首期交付和未批准状态保持不变。
- 2026-10-19 — 用户补充要求：Control Plane 必须允许多数据库驱动，可能选用 MySQL。更新 Storage Port、Adapter 和数据库兼容性边界。保持 Draft、未批准状态。

