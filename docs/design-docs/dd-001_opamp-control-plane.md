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

本设计定义 Go 语言的 APM Agent Control Plane，用 OpAMP 管理自定义 OpenTelemetry Java Agent 发行版的实例、健康状态、采样策略和应用结果。业务 JVM 的 Traces 仍通过 OTLP → Collector → Backend 发送；控制平面不转发任何 Span。

目前 P0 已有基于 opamp-go v0.25.0 的 HTTP/WebSocket 接入、内存 Registry、Agent 专属采样 JSON 和测试 Agent；CI 验证 Go 单测及 Vet。目标演进为持久 Desired State + Agent Observed State + 可恢复 Reconciler。设计状态是 **待评审草案**，不等于现有实现，也未获得 Design Owner 批准。

主要代价：必须处理身份绑定、顺序与幂等、Agent 离线、WebSocket 单连接写序列化以及 HTTP 拉取场景。第一版单体部署并使用 PostgreSQL；HA 与发布性能目标需要测量后另行决定。

## Goals and Non-goals

**目标（对应 docs/requirements/APM_CONTROL_PLANE.md）**

- FR-001/002：稳定记录 Agent 实例、服务/环境声明、协议能力、连接新鲜度和独立健康状态。
- FR-003/004：通过环境/服务 Selector 解析版本化 Head Sampling 策略，在 Agent 声明 AcceptsRemoteConfig 时经 OpAMP 发送。
- FR-005/006：持久记录期望版本、发送状态、实际上报的 Hash/状态；重连或服务端重启后恢复一致。
- FR-007/008：受控灰度与审计；服务端权限和 Agent 身份隔离；只将可认证 Principal 授予的范围用于授权。
- FR-009：提供供未来自定义 Java 发行版消费的采样配置契约；JVM 热切换本身在另一个工作流验收。

**非目标**：不实现 Collector Supervisor、OTLP 传输、Java Bytecode Instrumentation、二进制升级、Trace Retention/Tail Sampling、首期多活 HA，也不承诺无证据的吞吐量或采样准确率。

## Research and Decision Inputs

**已核实证据**：官方 OpAMP Go Server 通过 Attach() 接入 net/http，OnConnecting 负责连接级身份检查，OnMessage 返回 ServerToAgent；HTTP 模式的响应式下发与 WebSocket 推送语义不同。参考 upstream server/server.go、server/types/callbacks.go 与已有 cmd/server/main.go。P0 测试 Agent 仅确认 JSON 与回传状态，不能证明 JVM 动态采样成功。

**研究门禁**：本次方案由明确的项目方向（自定义 Java Agent 发行版、Go Control Plane、OpAMP 和先做控制端）及已检验源码接口直接确定，暂不创建虚构的 Concluded Research。需研究的问题列于末尾，在影响接口或安全决策前单独升级 RF Research/ADR。

**正式决定**：仓库当前没有 accepted ADR。本文比较候选架构，但不会把用户的实施授权记为 ADR 接受。数据库模式、Agent 凭证机制及批量灰度语义仍需审查；未来如涉及长期不可逆选择，应创建 proposed ADR 并由决策责任人明确接受。

## System Context and Invariants

~~~mermaid
flowchart TD
    UI["Management API / future UI"] --> P["Policy + Config Manager"]
    P --> PG[("PostgreSQL · Desired State")]
    P --> R["Reconciler"]
    R --> G["OpAMP Gateway · opamp-go"]
    G <-->|"Control: HTTP / WebSocket"| J["Custom Java Agent + OpAMP Client"]
    J -->|"Data: OTLP"| C["Collector"]
    C --> B["APM Backend"]
    J -.->|"ConfigStatus + Health"| G
~~~

**必须保持的边界**：
1. OPAMP 是控制面；OTLP 是数据面，两者不互相代发。
2. Instance UID 是 Agent 实例标识，不作为租户授权证明；service.name/environment 也是可伪造元数据。生产配置下发之前必须完成凭证到租户/Agent 范围的绑定。
3. Desired Policy Revision 不可变，Assignment 指向具体版本；Observed Hash/ConfigStatus 来自 Agent。发送成功不等于 APPLIED，只有同 Hash 的应用报告可更新对应 Assignment。
4. 连接在线和运行健康是独立状态；会话断开只能说明控制连接不可用，不能判定 JVM 已停止。
5. 规范化序列化产生稳定配置 Hash；重复下发同一策略必须幂等。
6. 避免在 OnMessage 持有数据库事务期间阻塞 WebSocket 网络写；每连接发送必须串行化。
7. 任意错误远程配置不得使服务器丢失上一个可用 Desired Revision。

## Proposed Architecture

**当前 P0**：cmd/server/main.go 同时承载协议、HTTP Admin API 和线程安全内存 Registry，cmd/testagent/main.go 模拟 OpAMP HTTP 拉取与 ACK。P0 仅测试通信路径，不应当宣称为生产控制平面。

**目标模块（模块化单体）**：
- cmd/server：装配依赖、启动/关闭与 HTTP Server 生命周期。
- internal/opamp：OpAMP Attach/Callbacks、Agent Capabilities、请求认证、实例连接与消息序列；协议结构不渗入 Policy Domain。
- internal/registry：Agent Identity、声明属性和最近 Observed Snapshot；持久化与内存活动 Session 分离。
- internal/policy：Policy/Revision/Selector/CompiledConfig。初期支持默认 rate、service + environment 策略；冲突采用显式优先级，优先级相同且内容不同直接拒绝发布，不能依赖遍历顺序。
- internal/config：按 Agent 解析最终不可变 ConfigMap、ContentType、ConfigHash；版本和 Hash 分开保存。
- internal/reconcile：按 Agent desired hash 与 reported applied hash 协调，监听重连、发布、ACK 和失败重试。
- internal/storage：PostgreSQL 数据模型和事务，避免把网络连接对象持久化。
- internal/api：查询 Agent、创建策略版本、发布与回滚请求；鉴权与租户授权在入口完成。

**候选持久实体**：agent_instances（immutable instance_uid 与所属已认证范围）、agent_observations（最后时间与健康快照）、policy_versions（immutable body/hash）、policy_bindings（selector 与发布关联）、agent_assignments（desired hash/revision + observed hash/status）、config_events（append-only 审计）。这是一份待验证的逻辑模型，不是已存在的数据库表。

**策略契约**：RemoteConfig ConfigMap 的名为 apm/sampling.json，ContentType 为 application/json；Body 包含 schema_version、sampling.type = parent_based_trace_id_ratio、sampling.rate ∈ [0,1]。Agent 可扩展后续规则，但未支持的 schema 必须明确拒绝，服务端不把未知能力当成已支持。

## Control and Data Flows

**1. Agent 上线/重连**

- 网络入口验证凭证 → 绑定租户/授权范围 → OnMessage 校验 16-byte InstanceUID、序列与 Capabilities → 更新 Agent 的 Observed Snapshot。
- AgentDescription 可部分上报；出现断序或服务端缺少快照时请求 ReportFullState。重复 UID 并发会话应制定显式冲突规则并测试，新建 Session 不盲目获得旧 Session 连接所有权。
- Reconciler 读取持久 Assignment：若代理支持远程配置且 reported hash 未达到 desired hash，下发完整配置；否则保持 Pending/Unsupported 并显式暴露原因。

**2. 策略发布**

- Admin API 验证权限、rate [0,1]、Selector 唯一性和冲突 → 在数据库事务中写入不可变 PolicyVersion 与目标 Assignment → 事务提交后触发 Reconcile。
- 服务端为所有目标生成确定性 Body 和 Hash。WebSocket 在线实例通过同连接的串行发送器推送；HTTP Agent 在后续轮询时获取。任何队列事件丢失可由周期性 Reconcile 修复。
- 状态从 PENDING → SENT/WAITING_ACK → APPLIED 或 ERROR/TIMED_OUT；只允许 matching hash 的 APPLIED 关闭当前版本，旧 ACK 保留历史但不得覆盖新 Desired。

**3. 失败和回滚**

- DB 提交失败不发配置；网络发送失败不回滚已提交 Desired；下次连接或定期 Reconcile 重试。
- Agent 报告拒绝或非法配置时保留最后一次已知适用版本，记录错误；若需恢复旧策略，创建新的发布动作指向已验证旧版本，记录发起人与审计事件。
- 控制平台断线不能阻断业务 JVM 的 OTLP Exporter。HTTP 与 WebSocket 都必须做协议级端到端测试，不用 HTTP 轮询模拟 WebSocket 推送成功。

## Alternatives, Open Questions, and Revisit Triggers

**选择记录（待 Design Owner 审阅）**

| 方案 | 优点 | 代价及当前处理 |
| --- | --- | --- |
| opamp-go + 单体 + PostgreSQL | 一套版本、易于事务协调；可先交付 P1/P2 | 单点扩容边界，HA 需再次设计 |
| 直接 Fork 历史 Bindplane Server | 现成管理模型 | 上游历史过久，迁移及安全审计成本难估；不选 |
| Redis/Kafka + 多个 Gateway 服务 | 跨实例消息路由和吞吐扩展 | 当前没有规模证据，不引入 |
| HTTP-only 管理 | 不需要驻留 WS 会话 | 配置只在轮询时生效；兼容保留但不能当推送 |

**未决问题**：Agent 身份绑定采用 mTLS 证书还是租户 Scoped Token；重复 UID 应如何换绑；Service/Environment Selector 的授权边界；严格回滚与 ACK timeout 默认值；生产规模和目标 SLO；Go Server v0.25.0 与 Java Client v0.14.0 规范差异。无法由当前 P0 单测证明这些约束。

**重访条件**：出现多实例部署需求、经过测量的连线数量或下发尾延迟超出单体能力、跨租户配置泄漏风险、Java Client 互操作失败、真实 JVM 采样行为与反馈不一致。

## Package Document Map

Single-file Design：本文件是 DD-001 的完整阅读入口；模块细节经批准后再独立拆分。

## Revision Notes

- 2026-10-19 — Draft revision 1, derived from explicit product requirements and current P0 source. Not yet approved.

