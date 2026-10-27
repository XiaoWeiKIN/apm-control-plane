---
schema_version: "1.4"
metadata_schema: "1"
artifact_type: adr
id: ADR-001
title: "Bind OpAMP Agent UIDs to stable principals, not credentials"
status: proposed
research_refs: []
depends_on: []
amends: []
amends_constraints: []
design_refs: ["docs/design-docs/dd-001_opamp-control-plane.md"]
supersedes: []
superseded_by:
decision_maker:
decided:
decision_outcome:
effect_changed_by:
effect_changed:
effect_reason:
payload_sha256:
created: 2026-10-26
updated: 2026-10-26
author: "AI assistant"
owner: "Unassigned"
---

# Bind OpAMP Agent UIDs to stable principals, not credentials

> **待批准提案（status: proposed）**。下面的推荐方案与 Constraints 在 Decision Owner 明确批准前不生效，不授权生产实现、Schema 迁移、凭证换绑或解除 EP-001 的 BLK-002。

## Context and Problem Statement

当前 Agent 注册路径使用静态 Workspace-scoped Bearer Credential：`internal/auth/auth.go` 将 Token 映射为 `auth.Principal{WorkspaceID, CredentialID}`，`internal/storage/gormrepo/store.go` 的 `agent_instances` 以 `(workspace_id, instance_uid)` 标识记录，同时用 `credential_id` 限制后续写入。这个机制能够拒绝其他 Credential 对既有 UID 的直接覆盖；其静态边界已由 [PR #22](https://github.com/XiaoWeiKIN/apm-control-plane/pull/22) 的双数据库集成测试进一步覆盖。

但 **Token 是可轮换、可撤销的认证材料；Agent Principal 是应当跨不同 Credential 生命周期保持稳定的身份**。当前模型的 UID 所有权绑定 `credential_id`：若仅替换同一个 Credential ID 对应的 Token，并重启服务加载静态配置，UID 所有权不变；若改用新的 Credential ID，现有 UID 的后续写入会被拒绝。配置还要求 Credential ID 和 Token 分别唯一，无法让新旧两枚 Token 同时以同一 Credential ID 生效。因此，**不同 Credential 的重叠轮换、分别撤销及可信身份连续性**仍缺少明确模型。以上是设计风险，不代表已证明生产安全故障。

本 ADR 只回答：**OpAMP Agent 的 `(workspace_id, instance_uid)` 应绑定 Credential ID，还是由受信任身份来源证明的稳定 Agent Principal？**

**必须明确的权限粒度**：现有 [Workspace Schema](../architecture/storage-schema.md) 将一个 `agent_principal_id` 设计为可拥有多个 `instance_uid`。这意味着 Agent Principal 是**所有权主体**，不自动等于唯一的运行中实例。若多个实例共用能够证明相同 Principal 的 Credential，该 Credential 的持有者可能报告该 Principal 名下其他 UID；仅替换 `credential_id` 为 `agent_principal_id` 并不能解决这种同 Principal 范围内的冒用。被盗用的同 Principal Credential 也会带来相同风险。是否允许共享凭证、是否需要更细的实例授权或可信首次绑定流程，必须由 Owner 在实施前明确，不能把该 ADR 当作实例级强身份的证明。

它不决定最终凭证协议（Token、JWT、mTLS）、身份颁发服务、WS 会话撤销期限或跨 Gateway HA。那些能力仍受 [EP-001 BLK-002](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 独立约束；[DD-001 Revision 1](../design-docs/dd-001_opamp-control-plane.md) 已批准，不能因此推断本提案也已批准。

## Decision Drivers

- **权限边界**：从受信任认证路径取得 Workspace 和 Agent 身份；AgentDescription、`service.name`、`agent.kind`、`instance_uid` 的自报属性不授予身份或跨 Workspace 权限。
- **轮换与冒领边界**：同一合法 Agent Principal 换用新的 Credential，不能被当成另一个 Principal；**不同 Principal** 的凭证不能通过自报 UID 接管历史 Desired。同 Principal 共享或失窃 Credential 的 UID 冒用风险，必须另有明确授权粒度及处理规则。
- **写入一致性**：首次绑定、Owner 冲突和换绑之间不允许竞态；原有 `agent_assignments` 的 Desired Generation/ACK CAS 语义保持不变。
- **变更代价**：使用现有 `(workspace_id, instance_uid)` 主键，避免为了轮换而给每个 Credential 创建重复 Agent 记录。
- **审计与迁移**：初次绑定和经审批的所有权变化可归因；未知历史 Credential 不能自动映射为新 Principal。
- **明确边界**：不凭空设计 IAM、审批端点、转移工作流、兼容双写或新增缓存；由 Owner 确认现有部署的真实 Credential/UID 数据规模和迁移要求。

## Research Evidence

本 ADR 不引用已封存的 Research Synthesis（`research_refs: []`），不声称已经完成独立的威胁模型、生产 mTLS/Token 实验或凭证轮换测试。决策依据包括：

- `internal/appconfig/config.go`：当前只加载静态 `APM_AGENT_CREDENTIALS_JSON` 与 `APM_ADMIN_CREDENTIALS_JSON`。
- `internal/auth/auth.go`：从已配置凭证计算受信任的 Workspace 和 Credential ID。
- `internal/storage/gormrepo/store.go`：Agent 行持久化 `credential_id` 并拒绝不同 Credential 的直接接管。
- `cmd/server/main.go`：OpAMP 在连接建立时认证，活动 WS 会话属于进程内管理。
- [EP-001 的 BLK-002 安全决策材料](../audits/ep-001-blk002-security-decision-packet.md) 与 [PR #22](https://github.com/XiaoWeiKIN/apm-control-plane/pull/22)：现有静态凭证模型的证据及生产缺口。
- 基线审阅版本：`main@853b393f812b077a9c9e3c55cb11d85c1ee81005`。正式实施应重新核对当时的最新源码与批准 Design。

## Considered Options

| 选项 | 行为 | 收益 | 风险与代价 |
| --- | --- | --- | --- |
| A. 保持 Credential ID 直接拥有 UID | 首次注册 `credential_id`；后续只有同 Credential 能更新 | 变更最小；同 ID 替换 Token 可保持 UID 所有权 | 改用新 Credential ID 时无法继续更新原 UID；不支持新旧 Credential 重叠证明同一 Principal，缺乏独立撤销和可审计的跨 Credential 身份连续性 |
| **B. 用可信稳定 Agent Principal 拥有 UID（推荐，待批准）** | 认证层从平台权威身份来源解析出 `workspace_id` 和稳定 `agent_principal_id`；Credential 仅证明该 Principal | 合法轮换不改变 UID 归属；清晰隔离身份与认证材料，适合现有 Workspace-scoped Desired 模型 | 需要批准 Principal 颁发、共享 Principal 的 UID 授权范围、已有绑定数据的安全迁移及撤销语义；它**不保证同 Principal 内不同实例互不冒用**，也可能涉及 Schema 变更 |
| C. 直接以客户端证书/Token 实例标识拥有 UID | 每个证书或 Token fingerprint 作为 Owner | 绑定到强认证材料可降低某些冒领风险 | 证书或 Token 更新仍改变 Owner；若使用稳定 subject 标识，本质上转化为 B；传输选型会不必要地绑死数据模型 |

**建议 B**：它把“是谁”与“用什么证明身份”分开，同时不规定最终使用哪一种认证协议。A 可继续作为开发/测试历史实现，但不能默认取得生产授权。C 的证书验证可作为 B 的认证方式之一，不应把可轮换的证书指纹当稳定身份。

## Decision Outcome

**状态：proposed，待 Design/Security Owner 正式接受或拒绝。** 产品侧已明确选择**工作负载级 Principal**：同一 Workspace 内，一个受信任 Workload Principal 可以拥有多个 Agent Instance UID。提议采用 B 并按该粒度实施。没有真实 Owner 的审批决定，`decision_maker`、`decided` 和 `decision_outcome` 继续为空；此处的推荐不形成现行约束。

选择理由是：`workspace_id`、`agent_principal_id` 与 `instance_uid` 的含义独立，可以在不改变现有 Assignment 主键的前提下支持凭证轮换，并拒绝**其他 Principal** 对已绑定 UID 的越权接管。它不能单凭 Principal ID 阻止同一 Principal 范围内的实例冒用。只有通过受信任身份源、明确的 Principal/UID 授权粒度与安全迁移证明该模型可实施后，才能开展生产改造。

## Decision Statement

**提议：生产 OpAMP Agent 的 Instance UID 所有权绑定到受信任身份来源确认的、在 Credential 轮换期间保持稳定的 Workspace-scoped Workload Principal，而不是某个 Credential ID 或凭证指纹。一个 Workload Principal 允许拥有多个 Instance UID，不要求每个 Pod/Agent 单独颁发 Principal。**

## Normative Constraints

以下约束**仅在本 ADR 被正式 accepted 后**成为实现门禁。

| ID | Strength | Scope | Constraint | Confirmation |
| --- | --- | --- | --- | --- |
| C-001 | must | OpAMP 认证边界 | 可信认证层必须生成 `workspace_id` 与稳定 `agent_principal_id`；Agent 自报的属性/UID 不得构造或修改这些权威身份值 | HTTP/WS 身份伪造与跨 Workspace 负向合同 |
| C-002 | must | Agent Inventory 写入 | 在同一 Workspace 内，首次 UID 绑定必须原子确定 Principal；已绑定 UID 仅允许同 Principal 更新，其他 Principal 必须拒绝且不更改已有 Inventory/Desired | PostgreSQL/MySQL 并发首次绑定、同 UID 冒领拒绝与 CAS 测试 |
| C-003 | must | Credential 轮换 | 已授权的新旧 Credential 若都证明同一个未被撤销的 Principal，必须指向同一个 Agent 记录和持续的 Desired；轮换不能被解释为所有权转移 | Token/证书轮换、HTTP/WS 重连与 Assignment Generation 保持测试 |
| C-004 | must_not | Agent 所有权变化 | 不得用“重新登录”“静默覆盖 Credential ID”或 Agent 自报 UID 绕过所有权校验；需要转移时必须经过另行批准的授权/审计流程，否则明确拒绝 | 无权限重新绑定、旧凭证重放与跨 Principal 转移拒绝测试 |
| C-005 | must | 历史身份迁移 | 如果已有数据，只有在来源受信任且可追溯的 Credential→Principal 映射确认后才能迁移；未知/冲突映射明确阻断，不做猜测或自动认领 | PG/MySQL 迁移前检查、冲突回滚、恢复演练和审计验证 |
| C-006 | must | Desired/Observed 保持 | 身份升级不得允许未经授权的 Principal 操作现有 Assignment/Release，也不得削弱 ACK Hash/Generation/RowVersion 的校验 | 复用 P2/P3 跨 Workspace、并发 Release/ACK 及重启恢复测试 |
| C-007 | must | 工作负载级 Principal/UID 授权 | 同一 Workspace 的一个可信 Workload Principal 可以拥有并注册多个 Instance UID；共享该 Principal 凭证的实例处于同一授权范围，不承诺同 Principal 内逐实例身份隔离。不同 Principal 不得读写或接管其他 Principal 已绑定的 UID；UID 归属须在数据库中原子固定，不能仅凭自报 Workspace/Principal 授权 | PG/MySQL 同 Principal 多 UID 注册、跨 Principal 冒领拒绝、凭证轮换保持 UID/Desired、失败无副作用测试 |

## Consequences

- **正面**：合法的凭证生命周期可与长期 Agent Inventory 分离；UID 所有权可沿着已知 Principal 审计；原来 `(workspace_id, instance_uid)` 的应用数据模型保持不变。
- **代价**：需要可信 Principal 映射（例如由平台身份服务或受信任网关提供）。具体来源、网络信任边界、Token/mTLS、缓存、角色权限和撤销期限尚未决定，不能提前实现一套自创 IAM。
- **数据库**：正式批准后才评估是否增加 `agent_principal_id` 与约束；不得假定历史数据为空，也不得把 MySQL DDL 的隐式提交当成 PostgreSQL 等价回滚。
- **兼容性**：现有环境变量静态 Credential 与 `credential_id` 所有权属于开发基线；是否保留测试模式或提供迁移期配置须经实施评审。不能悄然把不同 Credential 当同一 Principal。
- **安全**：稳定 Principal 仅解决 Credential 与授权主体生命周期分离，以及不同 Principal 的所有权边界；一个 Principal 仍可能管理多 UID。若 Credential 在该 Principal 范围内共享，持有者可以发出相同 Principal 下的请求，不能因此声称提供了每实例冒用保护。该决定也不等于实现了撤销/吊销、WS 长连接重新认证、TLS/mTLS、跨节点 Session Fencing 或平台 IAM。
- **实施**：不预设新的转移 API、通用认证框架、额外数据库或跨节点缓存。缺少可信映射时拒绝，不引入静默 fallback。

## Confirmation

接受本 ADR 后，再按批准的身份方案编排验证：

1. 在 PostgreSQL 17 和 MySQL 8.4 验证原子首次绑定、不同 Principal 争用同一 `(workspace_id, instance_uid)` 时明确拒绝，以及请求失败后 Inventory/Assignment 无副作用。
2. 真实 OpAMP HTTP/WS 验证不同 Credential 解析到**同一个**稳定 Principal 时，重连和 Desired/Observed ACK 连续，且不增加重复 Agent 记录；跨 Workspace 和不同 Principal 不可接管。
3. 验证历史记录迁移的冲突处理、失败回滚和审计留存，使用与真实部署一致的数据样本；对无法证实所有权的旧记录 fail closed。
4. 在生产身份来源已批准且实际集成后，验证过期/撤销行为和部署 TLS 边界；这些需要另外的安全决策及验收证据，不能用本 ADR 的格式校验替代。
5. 对实际选择的 Principal 授权粒度执行专门测试：若共享 Credential 是支持用法，要验证同 Principal 内多个 UID 的可操作范围；若要求每实例互不冒用，则必须用可信的逐 UID 授权或实例身份证据证明拒绝。不能将不同 Principal 的拒绝测试充当同 Principal 内的实例隔离证明。
6. 复用 [PR #21](https://github.com/XiaoWeiKIN/apm-control-plane/pull/21) 进程重启和 [PR #22](https://github.com/XiaoWeiKIN/apm-control-plane/pull/22) 静态凭证隔离用例作为回归起点；它们并非新 Principal 模型或共享凭证下 UID 安全的验证证明。

所有报告须固定实际受测 SHA、Go/PG/MySQL 版本及 GitHub Actions/原始日志；由 EP-001 引用正式验收证据，再由 Owner 判断 BLK-002 是否解除。

## Implementation prerequisites requiring Owner decisions

本 ADR 的推荐方向不代替以下实施输入；若 Owner 无法明确其中的 Principal 授权粒度，则应暂缓接受本提案，不要推断“稳定 Principal”已满足实例级安全目标。

| Owner 需要确认 | 当前已知情况 | 对实现与验收的影响 |
| --- | --- | --- |
| Principal 粒度 | **产品侧已选定工作负载级**：一个 Workload Principal 可以拥有多个 Agent UID，且不要求每实例 Principal | Security/Design Owner 正式确认该授权粒度与威胁模型；实现可信 Workload Principal 解析和跨 Principal UID 所有权保护 |
| Credential 是否共享 | 工作负载级身份允许同 Principal 的多个实例共享凭证；因此同 Principal 内实例不具备相互独立的认证边界 | Security Owner 确认该风险可接受，并批准凭证颁发、轮换、撤销和首次 UID 注册规则 |
| 首次 UID 注册权限 | `Store.Observe` 在 `(workspace_id, instance_uid)` 尚不存在时创建记录，再检查当前 Credential 所有权 | 明确可由谁注册新 UID、是否需要先经过受信任注册、克隆实例复用 UID 如何拒绝或恢复 |
| Credential 泄露/撤销时效 | 当前静态 Token 与 WS 会话不支持即时撤销 | 由独立 IAM/会话安全合同规定撤销、重新认证和长连接失效期限；不能由本 ADR 兜底 |
| 历史 UID 归属迁移 | 目前数据库以 `credential_id` 记录首次绑定；实际历史数据规模未经此 PR 检查 | 未找到可信 Credential→Principal 映射时停止迁移，不允许因同一 Workspace 而全部合并 |

## Revisit Triggers

- 平台 IAM 无法提供稳定、不可伪造且可审计的 Agent Principal。
- 已有部署的 Credential/UID 数据无法建立安全的 Principal 映射。
- 安全需求变更为同 Principal 内逐实例 UID 隔离，或共享 Credential 的风险超出 Security Owner 正式批准的威胁模型。
- 未来 mTLS、代理终止、跨节点会话或 UID 转移合同要求改变 Principal 语义。
- 经批准的性能、可靠性或安全验收揭示此模型引入不可接受的代价。

## More Information

- Research references: []（未有为本决定封存的 Research Synthesis）。
- Prerequisite ADRs: []。
- Amended ADRs: []。
- Amended constraints: []。
- Design documents: [DD-001 Revision 1](../design-docs/dd-001_opamp-control-plane.md)。
- Related ExecPlans: [EP-001 — Deliver durable OpAMP Control Plane](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md)。
- Security decision packet: [BLK-002](../audits/ep-001-blk002-security-decision-packet.md)。

## Revision Notes

- 2026-10-26T07:23:59Z — Proposed ADR created.
- 2026-10-26 — Clarified that a stable Principal may own multiple Instance UIDs and does not itself prevent impersonation by another holder of the same Principal's Credential; added C-007 and explicit Owner decisions. Status remains proposed.
- 2026-10-26 — Product direction clarified: Workload-scoped Principal with multiple Instance UIDs; shared-credential instances are one authorization domain. Formal Security/Design decision remains pending.
