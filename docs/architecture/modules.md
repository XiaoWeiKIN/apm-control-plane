# APM Control Plane：核心模块与职责边界

> **性质**：开发者详细架构 / P2 实现合同草案。本文记录模块划分、依赖方向与增量开发顺序；不是新的 DD-NNN、ADR，也不代表 [DD-001](../design-docs/dd-001_opamp-control-plane.md) 已获批准。[EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 的身份安全阻塞继续有效。
>
> **事实基线**：`main@61700ac72b0472994f13a39642f9e36a85228b11` 已合并 P1.1、P2.1 ConfigRevision 和 P2.2 单 Agent Release/Target/Assignment/Event 原子 Pending 发布。本 P2.3a 分支在持久 Desired 上增加 OpAMP 响应式配置投递与 ACK 收敛。**P2.3a 已合并 HTTP 响应式投递与 ACK；P2.3b 实现单实例 WS 主动推送/重连恢复，跨节点 HA 和 P3 灰度仍未实现；本 P2.4 分支新增 Java Policy Selector 的只读解析。** `internal/release` 是目标模块边界；现在仅有 `cmd/server/releases.go` 的 API 与 `gormrepo/releases.go` 的事务逻辑，不为了目录对称新增空包。
>
> **读者任务**：决定某个功能应该放在哪个模块，理解一次配置发布与回执如何流转，按照最小的跨模块依赖开发 P2。表结构与数据库约束的详细来源见 [Workspace Schema 设计](storage-schema.md)。

## 1. 一个 Go 进程，明确六条功能边界

推荐 **模块化单体（Modular Monolith）**：当前业务没有多服务部署、分布式消息或独立服务扩容的测量需求。六个主要功能边界分别是 **Agent Registry、OpAMP Gateway、Policy、Config、Release、Reconciler**。其中 Gateway 是协议适配模块，另外五个偏向业务/状态管理；不把“六个模块”理解成六个微服务。

`Workspace/Auth`、`Management API`、`Storage` 是三个横向支撑模块，而不是六个额外独立领域。`AgentConfigAdapter` 属于 Config 的可扩展内部边界，`Rollout` 属于 Release 的 P3 能力。

~~~mermaid
flowchart TB
    UI["可观测平台 Console / API Consumer"]
    AG["OpAMP Agents<br/>Java 首期 · Go/Python/Collector 后续"]
    subgraph CP["一个 Go Control Plane 进程"]
      API["Management API"]
      AUTH["Workspace / Auth"]
      GW["OpAMP Gateway"]
      REG["Agent Registry"]
      POL["Policy"]
      CFG["Config + Agent Adapter"]
      REL["Release"]
      REC["Reconciler"]
      STORE["Storage · GORM"]
      API --> AUTH
      GW --> AUTH
      API --> REG
      API --> REL
      REL --> POL
      REL --> CFG
      REL --> REG
      REL --> STORE
      REC --> STORE
      REC --> GW
      GW --> REG
      GW --> REC
      REG --> STORE
      POL --> STORE
      CFG --> STORE
    end
    UI --> API
    GW <-->|"HTTP / WS OpAMP"| AG
    STORE --> DB[("PostgreSQL 或 MySQL")]
~~~

图是**业务协作示意**，不表示这些箭头要实现成互相持有的循环 Go 依赖。具体入口分别向各模块注入所需的最小方法；如 Gateway 和 Reconciler 双向协作，双方通过一处进程装配与回调/发送接口连接，不能让两个 Go 包相互 import。

最重要的一条写入规则：**发布事务成功提交 Desired 后，Reconciler 才尝试通过 Gateway 发送；Gateway 把 Agent 的 ACK 交给协调逻辑，后者按 Hash 和 CAS 更新 Observed。**

## 2. 六个核心模块的责任表

| 模块 | 接收什么 | 拥有什么状态和行为 | 明确不负责 | 关联数据 | 状态 |
| --- | --- | --- | --- | --- | --- |
| **Agent Registry** | 已认证的 Agent Status / 描述 / Health | UID 归属、AgentKind、Distribution、Capabilities、最新 Observation、查询和活跃度视图 | 解析 Policy、生成配置、决定发布版本 | `agent_instances` | **P1.1 已实现基础能力** |
| **OpAMP Gateway** | AgentToServer、HTTP/WS 连接、发送请求 | OpAMP 编解码、连接回调、Session 生命周期、协议错误、按传输模式发送 | 选择策略、保存唯一 Desired、判断业务发布成功 | WS Session 在内存 | **P1.1 已实现接入与响应下发** |
| **Policy Manager** | 规则版本、Scope、Selector、Priority | 不可变 Policy Revision、Binding、匹配排序与冲突检测 | OpAMP 网络写、生成所有 Agent 共用的固定 JSON | `policies`、`policy_revisions`、`policy_bindings` | **P2.4 分支实现 Java Policy/Revision/Binding 与单 Agent 只读解析；待 PR 评审** |
| **Config Manager** | 选中的规则、AgentKind/发行版/能力 | 确定性完整 ConfigMap/Hash、不可变 Config Revision；后续按 Agent 能力判定 | 持有网络连接、管理灰度批次 | `config_revisions` | **P2.1 Java 编译和存储已实现；通用 Adapter/能力匹配仍待实现** |
| **Release Manager** | 经过授权的配置发布/回滚请求 | Release、冻结 Target、Agent 当前 Desired Assignment、提交事务和审计 | 把 SENT 当 APPLIED、在事务内发送 OpAMP | `config_releases`、`release_targets`、`config_events`；与 Assignment 同 Tx | **P2.2 已实现单 Agent Pending 发布；P3 灰度/回滚未实现** |
| **Reconciler** | 持久 Assignment、当前 Agent 能力/会话、OpAMP ACK | Desired/Observed 对账、ACK 归因、响应式投递、重试期限 | 修改不可变 Revision、绕过授权写新策略 | `agent_assignments`（当前版本与 CAS） | **P2.3a 已合并；P2.3b 单进程 WS Push、在线重试和断线重连已通过双数据库/Go/RF CI** |

这张表描述**拥有者**而非“每张表都要一层 Repo”。`agent_assignments` 的 Desired 由 Release 在发布事务内变更；Observed 和投递进度由 Reconciler/ACK 处理，但二者属于同一持久 Assignment 记录。应通过存储事务和 CAS 保护，不为拆分代码而制造两个相互冲突的事实源。

### 2.1 Agent Registry

AgentKey 必须是 `(workspace_id, instance_uid)`，而不是裸 UID。Workspace 来自已认证的 Credential/平台身份上下文，不来自 AgentDescription；Agent 自报的 Service/Environment 只用于被授权范围内的 Inventory 展示和后续策略匹配。

P1.1 目前将最新 Status Snapshot 放在 `agent_instances` 中，运行时 `runtimeAgent` 还保存 WS Connection、Sequence、临时 Demo RemoteConfig。P2 实施后 **Config Desired 不再由内存 `runtimeAgent.remote` / `desiredHash` 作为事实源**；Inventory 继续管理 Agent 元数据及连接可用性。这是迁移业务责任，不要求现在单独拆出 `agent_observations` 热表。

### 2.2 OpAMP Gateway

Gateway 把 `protobufs.AgentToServer` 映射为 Agent Observation/配置回执，把待发完整 `AgentRemoteConfig` 交给 HTTP 响应或 WS 的单连接串行 Writer。它负责 OpAMP 级 `error_response`、能力协商、断序 `ReportFullState`，但不应把 Protocol Protobuf 传入 Policy Domain。

- **HTTP**：一次请求/响应；新的配置只能在 Agent 下一次 Polling 时回传，在线按 `last_seen + TTL` 推断。
- **WebSocket**：长连接，可主动发新配置；连接所有权、断开及 Writer 并发管理留在运行时，不能直接写数据库。
- **收到 ACK**：先保存 Agent 自报的 Hash/状态事实，再交给 Reconciler 对比当前 Desired；发送成功绝不等于 `APPLIED`。

当前已实现的是 P1.1 的 HTTP/WS 基础接入和响应式 Demo 配置；**P2.2 的 Pending Release 已通过 P2.3 Gateway HTTP/WS 投递**。单进程 WS 推送与重试已实现；跨 Gateway 会话转移仍未实现。

### 2.3 Policy Manager

Policy 表达业务意图，例如：在 Workspace A 的 `prod/checkout` Service 使用 Java Head Sampling 10%。Policy Manager 管理不可变 `policy_revisions` 与当前 `policy_bindings` 的 Scope/Selector/Priority。

Resolver 必须先确认 Workspace Scope，再判断 AgentKind、Selector 和优先级；同级发生内容冲突时**显式失败**，不得依赖遍历顺序。首期只支持 Java 采样策略，不能将其 JSON 作为 Go/Python/Collector 的公共格式。

P2.4 以确定性 Selector 规则实现 Workspace/Environment/Service/Instance Scope，服务端按具体 Agent 解析并返回命中 Revision；不自动编译并发布 ConfigRevision，具体合同见 [P2.4 Policy 解析](p24-policy-resolution.md)。

### 2.4 Config Manager 与 AgentConfigAdapter

Config 是**Agent 实际能够理解的不可变完整配置快照**，不同于 Policy 规则。Config Manager 对 AgentKind、DistributionVersion、ConfigDialect、Capabilities 做兼容检查，输出确定性完整 ConfigMap、Hash 和 `config_revision_id`，然后保存在 `config_revisions`。

Java 首期示例：`apm/sampling.json`、`application/json`、`schema_version=1`，规则为 `parent_based_trace_id_ratio` 和 `rate`。这属于自定义 Java 发行版契约，其他 Kind 无 Adapter 时返回明确 `UNSUPPORTED`。

Adapter 是真实的变化点。P2.1 先提供具体的 `CompileJavaSampling`，将一个 Java JSON 文件编译为确定性 Protobuf ConfigMap bytes/Hash，并经 GORM 创建独立 ConfigRevision；**尚未在发布时检查目标 Agent 的版本或 Capabilities**，因为发布接口尚未实现。只有第二个实际 Adapter 使用者出现后，才需要抽出稳定接口；不要为所有语言提前创建空 Factory 或占位实现。Hash 不依赖数据库 JSON 的重排序。

### 2.5 Release Manager

Release 是一次**被授权的配置变更意图**。发布操作的事务职责：冻结目标、持久化 ConfigRevision（如需要）、创建 Release/Target、更新对应 Agent Assignment 的 Desired Generation、追加事件。全部写入成功后返回 Release ID，再通知 Reconciler；不能在持有数据库事务时调用 OpAMP `Send`。

`release_targets` 冻结“这一次发布作用于哪些 Agent”，`agent_assignments` 表示“这些 Agent **当前** 应执行什么”。新发布不得覆盖旧 Release 的审计证据。回滚在 P3 表示**新建一次引用旧 ConfigRevision 的 Release**，不是直接删除历史。

P2 最小版本先只提供单 Agent、单批次发布；P3 再扩展批次进度、失败阻断和受控回滚。若没有独立审批/暂停语义，不必提前增加 `release_batches` 物理表。

### 2.6 Reconciler

Reconciler 在 DB 中读取当前 `agent_assignments`（Desired）和 Agent 回执（Observed），判断应发送、等待、重试还是标记 Unsupported。它通过 Gateway 触发 WS 发送，或使 HTTP 下一次轮询获取当前配置；**网络通知可以丢，Desired 不能丢**。

状态归因必须满足：

- 仅当 ACK 报告 `APPLIED` 且 Hash 等于当前 Desired Hash，才能标记**当前内容**已应用；仅 `SENT` 或 `WAITING_ACK` 不构成应用证明。
- 旧 Hash 或失败回执可以记录历史 `config_events`，但不能覆盖新的 Desired。
- 持久 Assignment 的 `desired_generation`、`row_version` 由服务端维护，更新时检查 `RowsAffected == 1`；Agent 并不直接确认 Generation。
- 同 Hash 的不同 Release 不等于发生了两次真实应用动作，不能据此虚增 Rollout 成功次数。
- Agent 离线、网关或 DB 重启后，应从持久 Assignment/ConfigRevision 恢复下一次协调，不能以进程内队列作为唯一任务源。

## 3. 三个横向支撑模块

| 支撑模块 | 正确职责 | 当前状态与限制 |
| --- | --- | --- |
| **Workspace / Auth** | 分离 Agent/Admin Credential，生成可信 Workspace Principal，授权后才执行 Registry/发布操作 | `internal/auth` 已支持受信配置里的 `workspace_id`；**尚未真正接入平台 Workspace/IAM 的存在性、角色和撤销验证** |
| **Management API** | HTTP 路由、入参验证、认证/授权、资源响应与错误映射；调用 Release/Registry | `cmd/server/main.go` 已有 Inventory 和 P0 Demo API；P2.2 Release 与 P2.4 Policy Authoring/Resolution API 已实现 |
| **Storage** | 通过 GORM + 选定 PostgreSQL/MySQL Dialector 实现业务查询、事务、CAS 与错误映射；Goose 独立迁移 | P1.1 + P2.1 已有；**P2.2 新增 Goose 00003、单 Agent 原子 Release/Assignment CAS**，P2.3a 已提交 ACK 状态更新 |

**优先复用现有 GORM Repository**，不要为每张业务表都建立一次转发型 Service/Repository/UnitOfWork。只有发布事务确实需要跨多组操作、或测试需要隔离边界时，才定义必要的事务方法与接口。PostgreSQL 与 MySQL 采用**同一套业务实现与行为合同测试**，仅保留需要的 Dialector 与 SQL Migration 差异。

Workspace 概念与 Logs/Metrics/Traces/Dashboard 属于同一可观测平台，但本仓库并不管理 Workspace 主数据，也不能由 Agent 自报值创建 Workspace。生产上接入权威 IAM 和 Token 生命周期，仍是单独待批准的安全工作。

## 4. 状态所有权与依赖方向

| 状态 | 当前 P1.1 | P2/P3 目标 owner | 持久性 |
| --- | --- | --- | --- |
| Agent 身份、发行版、Capabilities、Health、LastSeen | Registry → `agent_instances` | Registry | **数据库** |
| 活动 WS Connection、HTTP 请求上下文、发送 Writer | `cmd/server` 的运行时 Registry | Gateway/Session | **仅内存** |
| P0 Demo Sampling RemoteConfig/DesiredHash | `runtimeAgent` | ConfigRevision + Release/Assignment 替代 | **当前仅内存，P2 改为数据库** |
| Policy 规则和 Selector | P2.4 实现单 Agent Java Resolver | Policy | 数据库中的不可变 Revision + 可变 Binding |
| 完整 ConfigMap/Hash | P0 Demo 按需生成；**P2.1 已能独立编译并持久化 Java ConfigRevision** | Config | 不可变 `config_revisions`；目前与 P0 Demo 分开 |
| 目标快照与发布审计 | P2.2 已实现单 Agent Pending Release、冻结 Target 和 TARGET_ASSIGNED 事件 | Release | `config_releases`、`release_targets`、`config_events` |
| 当前 Desired/Observed 与 CAS | P2.2 已实现 Desired + Generation/RowVersion CAS；Observed 回执对账待 P2.3 | Release 写 Desired，Reconciler 写 Observed | `agent_assignments` |

### 4.1 代码依赖原则

- `cmd/server` 负责初始化配置、DB、认证、Handler、Gateway 和生命周期装配；不要在 `main` 内实现所有 Policy/Release 算法。
- Gateway 只处理 OpAMP 通信和 Status/ACK 映射，调用 Registry/Reconciler 的**窄接口**；Policy/Release/Config 不依赖 OpAMP 网络类型。
- Release 负责协调 Registry、Config/Policy 与同事务 Storage 更新，**不**调用网络 Writer。
- Reconciler 通过存储读 Desired，通过 Gateway 的窄发送接口完成传输；Gateway ACK 经装配层的回调交给协调逻辑，避免 Go 包循环依赖。
- Storage 只暴露被业务实际调用的查询、事务和 CAS；不在各处复制数据库访问判断或创建无意义的“万能 Repository”。

## 5. Go 包布局：从已有代码渐进拆分

这只是建议的**实现到达形态**，不是要求一个 PR 创建所有目录，也不是把所有表映射成独立包。

~~~text
cmd/
  server/                    进程装配，现有主程序约 490 行
  testagent/                 HTTP OpAMP 测试客户端
internal/
  appconfig/                 [已有] Koanf 启动配置
  auth/                      [已有] 可信 Workspace Credential
  storage/
    gormrepo/                [已有] GORM + 两种 Dialector
  registry/                  [待拆] AgentKey、资产/Observation
  opamp/                     [待拆] 协议 Handler、Session、WS Writer
  config/                    [P2.1 已有] Java Sampling 编译与不可变 Snapshot
  release/                   [按需迁出] P2.2 发布目前位于 gormrepo 与 cmd/server
  reconcile/                 [P2] Desired/Observed、ACK、重试
  policy/                    [P2 后半] Policy Revision、Selector
  api/                       [按需迁出] Management HTTP API
db/migrations/
  postgres/                  [已有 00001 + 00002 + P2.2 00003]
  mysql/                     [已有 00001 + 00002 + P2.2 00003]
~~~

**不为拆目录单独提交全量重构**。当前 `cmd/server/main.go` 中同时包含 `registry.update/get/list`、`makeGateway`、`adminHandler` 和临时 `samplingConfig/assign`。随着持久 Config 和 Reconciler 引入，先把被新功能真正复用或阻碍测试的职责迁出，并维持 `/v1/opamp`、`/api/v1/agents` 等现有协议与路由。

## 6. P2/P3 增量顺序与验收

推荐先完成**单 Agent 的持久配置闭环**，再开发 Workspace/Environment/Service 级 Policy，避免高频主路径一开始就包含 Selector/灰度的复杂分支。

| 增量 | 优先落地的模块 | 交付时必须可观察的行为 |
| --- | --- | --- |
| **P2.1** 配置内容 | Config + Storage | 编译一个 Java `apm/sampling.json`，持久化完整 ConfigRevision/Hash，双数据库读回后字节一致；不冒充动态 JVM 采样已成功 |
| **P2.2** 发布事务 | Release + Storage | Admin 针对**单个 Agent** 发布，Release/Target/Assignment/Event 同一个事务提交或全部失败；Desired 重启后不丢 |
| **P2.3a/b** 协调闭环 | Reconciler + Gateway + Registry | HTTP 轮询与在线 WS 主动 Push 获取 ConfigMap；匹配 ACK 更新状态；旧 ACK 不污染新 Generation；离线重连从 DB 恢复（见 [P2.3 文档](p2-reconcile-delivery.md)） |
| **P2.4** 策略解析 | Policy + Storage + Management API | Workspace/Environment/Service/Instance Selector 确定性匹配；最高同级冲突显式拒绝；只读结果不触发 Java 配置发布（见 [实现合同](p24-policy-resolution.md)） |
| **P3** 受控发布 | Release（扩展 Rollout） | 冻结目标、分批、失败停止扩散、显式回滚产生新 Release/审计事件 |

P2.1 ConfigRevision 与 P2.2 发布事务已作为相邻独立增量实施。P2.2 的可观察结果是数据库已有 Pending Desired，而非 Agent 已应用配置。执行顺序按实际依赖调整，但不要跳过存储原子性与 Agent 回执验证。

### 6.1 必须覆盖的错误路径

| 情况 | 预期责任方 / 结果 |
| --- | --- |
| Workspace 凭证无法认证或越权 | Auth / API/Gateway 拒绝，绝不因 Agent Description 扩权 |
| 目标 Agent 不存在或 UID 所有权冲突 | Registry/Storage 直接失败，不继续编译或下发 |
| Agent 不支持配置格式或缺少 Capability | Config 返回 Unsupported，Release/Assignment 保留原因，Gateway 不发送不兼容内容 |
| ConfigRevision/Release 事务提交失败 | Storage 回滚，Release 不触发发送 |
| 网络投递失败、Agent 离线或 HTTP 等待轮询 | Reconciler 保留 Desired，重试/重连后恢复 |
| 当前 Desired 已变更但收到旧 ACK | Reconciler 追加事实，CAS 不覆盖较新 Generation |
| PostgreSQL/MySQL 行为不同 | Storage 合同测试阻止合并；不能用多级 fallback 掩盖 |
| 同一 Hash 在多次 Release 中出现 | 记录各自发布历史，不虚构另一轮应用操作 |

## 7. 决策边界与维护方式

目前可直接确认的产品/技术输入是：通用多语言 Agent 管理、**Java 配置 Adapter 首期**、平台统一 `workspace_id`、Go 模块化单体、GORM PostgreSQL/MySQL + Goose、单 Agent 配置闭环优先。这不表示 P2 已实现。

**仍待正式评审**：Workspace/IAM 权威校验、稳定 Agent Principal 与 Credential 轮换、UID Session Fencing、Policy 重叠 Selector 的最终冲突合同、Config 相同 Hash 跨 Release 的统计语义、HA 与实际规模/SLO。凡涉及真实安全授权或长期不可逆数据库约束，应与 DD-001/ADR 流程保持一致，不能以本文替代审批。

**更新规则**：本文件是模块所有权和实现顺序的阅读入口；[Workspace Schema](storage-schema.md) 拥有表级字段、约束、索引和 Migration 合同；[整体架构](README.md) 描述端到端生命周期；[EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 跟踪里程碑与验证证据。实现 PR 若移动方法或改变 owner，应同步更新这里，避免维护两份矛盾的模块边界。
