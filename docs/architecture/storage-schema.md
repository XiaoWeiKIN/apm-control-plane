# APM Control Plane：关系型数据库模型与 Schema 设计

> **文档类型**：面向开发者的详细数据模型 / 实现合同（Proposed）。
>
> **日期**：2026-10-19；**数据库**：PostgreSQL / MySQL；**运行架构**：单个部署选择一种主数据库，GORM v2 + Goose。
>
> **现状边界**：截至 `main@b6fc2563f765af90f36b09ada2c4d4830a9dcdec`，**仅 `agent_instances` 已有 Goose Migration 和 GORM 实现**；其他表均为 **P2/P3 目标 Schema 草案**，**没有执行 Migration、没有写入实际数据库**。本文不批准 DD-001、ADR 或身份方案；生产发布前须按 RepoFoundryAI 设计门禁评审。
>
> **来源**：[DD-001（Draft）](../design-docs/dd-001_opamp-control-plane.md)、[EP-001（Blocked）](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md)、[FR-001—FR-013](../requirements/APM_CONTROL_PLANE.md)、[P1.1 持久化实现](p1-durable-inventory.md)、[技术选型](technology-selection.md)。

## 1. 先理解存储模型

Control Plane **不存 Span、Metric 或大规模心跳历史**，而是存储 Agent 身份、配置内容、期望状态、实际报告以及配置发布记录。一个 Agent 可以不断重连，但其实例身份不应因网络 Session 改变；一个采样 Policy 可以发布很多次，同一配置 Hash 也可以被不同 Release 复用。

因此，将数据分成三条生命周期：

1. **身份和 Inventory**：Tenant → 稳定 Principal → Agent Instance。Credential 只是用于验证 Principal 的可轮换方式；网络连接在内存，不在数据库中保存 `net.Conn` 或 OpAMP Connection。
2. **不可变定义**：Policy → Policy Revision → 按 AgentKind/ConfigDialect 编译的 Config Revision。内容与 Hash 在创建后不再修改；调整策略创建新版本。
3. **可变协调与不可变证据**：Release 冻结目标 Agent 集合；Release Target 记录目标批次；Assignment 表表示**每个 Agent 当前的唯一 Desired/Observed 视图**；Config Event 以 append-only 记录发布、回执与回滚历史。

发布动作在数据库事务中先写 Desired 和审计事件，**提交以后**才通过 OpAMP 推送或等待 HTTP 轮询。网络发送成功不是 Agent 已应用。只有与当前 Desired Hash 匹配的 `APPLIED` 才能表示目标**内容**已应用；它不能仅凭 Hash 证明新 Release 本身发生过一次新的应用。

### 1.1 核心 ER 关系

~~~mermaid
erDiagram
  tenants ||--o{ agent_principals : owns
  agent_principals ||--o{ credential_bindings : authenticates
  agent_principals ||--o{ agent_instances : owns
  tenants ||--o{ policies : manages
  policies ||--o{ policy_revisions : versions
  policy_revisions ||--o{ policy_bindings : selected_by
  policy_revisions ||--o{ config_revisions : compiles
  policy_revisions ||--o{ config_releases : initiated_from
  config_releases ||--o{ release_targets : freezes
  agent_instances ||--o{ release_targets : targeted
  config_revisions ||--o{ release_targets : compiled_for
  agent_instances ||--o| agent_assignments : current_desired
  config_revisions ||--o{ agent_assignments : assigned
  config_releases ||--o{ agent_assignments : caused
  config_releases ||--o{ config_events : audit
  agent_instances ||--o{ config_events : observed
~~~

上述为**逻辑关联**，不是已执行的物理外键。表中所有引用 `Agent Instance`、`Policy`、`Revision`、`Release` 的业务 ID 都必须携带同一个 `tenant_id`；实现时应采用**复合外键或等价的数据库约束**，不能单靠 API 已检查租户来避免跨租户关联。

### 1.2 表清单与落地阶段

| 表 | 数据职责 | 生命周期 | 计划阶段 | 当前物理表 |
| --- | --- | --- | --- | --- |
| `tenants` | Tenant 的稳定标识与状态 | 基本不变 | P2 身份治理 | 未创建 |
| `agent_principals` | Agent 的稳定授权主体 | 低频更新 | P2 身份治理 | 未创建 |
| `credential_bindings` | Credential 引用 → Principal，可轮换 | 低频更新 | P2 身份治理 | 未创建 |
| `agent_instances` | Agent 身份与最新状态 | 低频身份更新、高频状态更新 | **P1.1**；P2 补字段 | **已创建** |
| `policies` | 策略逻辑身份 | 低频更新 | P2 | 未创建 |
| `policy_revisions` | 不可变策略版本 | Append-only | P2 | 未创建 |
| `policy_bindings` | Scope/Selector → 生效版本 | 有 CAS 的少量修改 | P2 | 未创建 |
| `config_revisions` | 按 AgentKind 编译的不可变完整配置 | Append-only | P2 | 未创建 |
| `config_releases` | 一次发布的命令、状态、幂等键、计划 | 发布创建，状态变更 | P2 基本 / P3 灰度 | 未创建 |
| `release_targets` | 某次发布的**冻结目标快照**与批次结果 | 目标不可变，执行状态可更新 | P2 基本 / P3 灰度 | 未创建 |
| `agent_assignments` | 每 Agent **当前** Desired/Observed 和 CAS | 可变、可恢复 | P2 | 未创建 |
| `config_events` | 发布/回执/错误/回滚的审计轨迹 | Append-only | P2 基本 / P3 审计 | 未创建 |

**首选 12 张逻辑表**是全生命周期的总体设计，不表示 P2 必须一次性创建全部。后文另外说明 3 张**非首期必要**的扩展表，避免过早按所有可能场景拆分。

## 2. 统一的数据类型和约束

### 2.1 跨数据库字段策略

| 逻辑类型 | PostgreSQL 候选 | MySQL 候选 | 约束与原因 |
| --- | --- | --- | --- |
| Tenant ID | `VARCHAR(96)` | `VARCHAR(96)` | 延续 P1.1，所有租户数据复合主键包含 `tenant_id` |
| Agent Instance UID | `VARCHAR(32)` | `CHAR(32) CHARACTER SET ascii COLLATE ascii_bin` | **保留现状**；仅允许 16 字节 UID 的小写 hex，避免本次设计顺带变更线上主键 |
| 新的业务 ID | `VARCHAR(36)` | `CHAR(36) CHARACTER SET ascii COLLATE ascii_bin` | 建议由应用产生 UUID 字符串；不依赖自增 ID、`RETURNING` |
| Principal / Credential 外部引用 | `VARCHAR(128)` | `VARCHAR(128)` | 字符串范围由发行凭证的可信系统定义；不存明文 Token |
| Config Hash / Payload Hash | `CHAR(64)` | `CHAR(64) CHARACTER SET ascii COLLATE ascii_bin` | SHA-256 lower-hex；解码成 32 字节后才放入 OpAMP `config_hash` |
| 不可变 JSON 正文 | `TEXT` | `LONGTEXT` | UTF-8 规范化序列化由应用负责，不使用 DB JSON 重排来计算 Hash |
| 完整 ConfigMap 二进制 | `BYTEA` | `LONGBLOB` | 必须保持原始字节；大小上限由平台协议和业务限额共同约束 |
| UTC 时间 | `TIMESTAMPTZ` | `DATETIME(6)` | 应用、连接会话统一 UTC，写入精度至微秒，读回做一致性测试 |
| 状态 | `VARCHAR(24)` | `VARCHAR(24)` | 不选 PG enum，方便双方言 Migration；Go 校验 + 适当 CHECK |
| 版本和计数 | `BIGINT` | `BIGINT` | 非负、受控增长；显式 `generation/row_version` 用于 CAS |
| 布尔 | `BOOLEAN` | `BOOLEAN`（TINYINT） | 统一 GORM bool |
| OpAMP Capabilities | 当前 `BIGINT` | 当前 `BIGINT` | **已有 P1.1 限制**：无符号 uint64 高位无法表达；变更前必须约定无损的 8 字节或二进制编码 |

**不要现在把主键改为 `BINARY(16)`**。虽然它更紧凑，但会改变已落地表、引用方式和 API / GORM 适配，在当前规模没有足够的收益证据。若以后确有容量压力，应作为独立、受控的数据迁移。

### 2.2 全局不变量

- **租户边界**：每个管理实体都有 `tenant_id`，跨实体引用时携带相同的 `tenant_id`。读取、更新、删除和 CAS 条件均以 Tenant 作为首要过滤维度。MySQL 需要统一二进制或大小写敏感 Collation 处理标识符。
- **身份边界**：OpAMP Agent 自报的 UID、Service、Kind、Distribution **均不是授权凭据**。Credential 映射到可信 Principal 后才能访问指定实例。
- **不可变定义**：`policy_revisions`、`config_revisions` 的正文和 Hash 创建后不得 UPDATE/DELETE。需要改变时创建新 Revision；直接 SQL 写也应受权限限制。
- **同一 Agent 的当前期望**：每个 `(tenant_id,instance_uid)` 至多只有一条当前 Assignment。发布状态通过 `desired_generation` 与 `row_version` 保护，不用 Agent 上报 Hash 替代服务端生成的版本号。
- **真实回执**：Observed 状态来自 OpAMP，不得根据 HTTP/WS 发送成功直接标记 APPLIED。旧 Hash 的 ACK 应记录为事件，但不得将新的 Desired 状态改为 APPLIED。
- **可重启**：Release、目标快照、Assignment、完整 Config Revision 和关键事件必须全部可从 DB 恢复，Reconciler 的唤醒队列可丢失但可以通过周期扫描重建。
- **引用与删除**：生产系统对已被 Release/Target/Event 引用的内容默认采用 `RESTRICT`，不通过 `ON DELETE CASCADE` 删除历史。Tenant/Agent 退出通过状态或受控归档表达。

## 3. 身份与资产域

### 3.1 `tenants` — 租户

**主键**：`tenant_id`。若未来与外部 IAM 集成，Tenant ID 仍作为本地业务数据的稳定键，不把可修改的显示名称用作键。

| 字段 | 类型（逻辑） | 约束 | 说明 |
| --- | --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK | 不可变的租户键 |
| `display_name` | VARCHAR(128) | NOT NULL | 展示名，不参与权限判断 |
| `status` | VARCHAR(24) | NOT NULL | ACTIVE / SUSPENDED |
| `created_at` | UTC Timestamp | NOT NULL | 创建时刻 |
| `updated_at` | UTC Timestamp | NOT NULL | 状态/名称更新时刻 |

`tenant_id` 外键被后续身份、策略和发布表引用；建议先完成迁移与 backfill 后再启用全部强制外键。

### 3.2 `agent_principals` — 稳定授权主体

**主键**：`(tenant_id, principal_id)`；`tenant_id` FK → `tenants`。一个 Principal 可以拥有多个 Agent Instance，可以对应多枚轮换中的 Credential。**Principal 和 OpAMP Instance UID 不是同一概念**。

| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK | 租户 |
| `principal_id` | VARCHAR(128) | PK | 服务端颁发的稳定 Agent 所有权主体 |
| `display_name` | VARCHAR(128) | NOT NULL | 人类可读标记 |
| `status` | VARCHAR(24) | NOT NULL | ACTIVE / DISABLED |
| `created_at` | UTC Timestamp | NOT NULL | 首次创建时间 |
| `updated_at` | UTC Timestamp | NOT NULL | 最近修改时间 |

稳定 Principal 只表达**Agent 身份所属主体**，Admin 用户身份可以先继续由现有认证系统映射成 `actor_id`；不要误将 Agent Principal 用于管理员权限校验。

### 3.3 `credential_bindings` — 凭证引用与轮换

**主键**：`(tenant_id, credential_ref)`；`(tenant_id,principal_id)` FK → `agent_principals`。这一表只保存**外部可信 Credential ID** 和生命周期元数据，**不存**明文 Token、可重放密钥、数据库 DSN、证书私钥。

| 字段 | 类型 | 约束 | 说明 |
| --- | --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK | 租户 |
| `credential_ref` | VARCHAR(128) | PK | Secret Manager / IAM 的稳定凭证引用 |
| `principal_id` | VARCHAR(128) | NOT NULL/FK | 绑定稳定 Principal |
| `credential_kind` | VARCHAR(24) | NOT NULL | SCOPED_TOKEN / MTLS_CERT（未来） |
| `status` | VARCHAR(24) | NOT NULL | ACTIVE / REVOKED |
| `valid_from` | UTC Timestamp | NULL | 生效时间 |
| `expires_at` | UTC Timestamp | NULL | 到期时间 |
| `revoked_at` | UTC Timestamp | NULL | 撤销时间 |
| `created_at` | UTC Timestamp | NOT NULL | 建立绑定时间 |

`credential_ref` 可在轮换时增加另一条指向**同一 Principal**；旧 Credential 被撤销不改变 Agent Instance 的所有权。 对只传 Bearer Token 的 OpAMP 接入，认证系统必须能够从凭证**无歧义地反查可信 Tenant 和 Principal**，不能采用客户端自报 Tenant 来消除同名冲突；当前 P1.1 的启动配置已经禁止重复 Token。**是否采用本地 Credential 表、外部 IAM 真源及撤销传播机制尚需安全评审**；此表是业务关系设计，不宣称已有 Token 轮换能力。

### 3.4 `agent_instances` — Agent Inventory（已存在；目标为原位演进）

**当前已落库主键**：`(tenant_id,instance_uid)`。详细物理 DDL 以 [PostgreSQL Migration](../../db/migrations/postgres/00001_agent_instances.sql) 与 [MySQL Migration](../../db/migrations/mysql/00001_agent_instances.sql) 为准。下表中“现有”不表示已实现稳定 Principal 字段。

| 字段 | 状态 | 类型 | 约束 / 语义 |
| --- | --- | --- | --- |
| `tenant_id` | **现有** | VARCHAR(96) | PK，可信 Tenant |
| `instance_uid` | **现有** | 32-char hex | PK，Agent Instance Key |
| `credential_id` | **现有（过渡）** | VARCHAR(128) | P1.1 首次注册凭证所有权；以后由 `owner_principal_id` 替代 |
| `owner_principal_id` | **拟新增** | VARCHAR(128) | FK → `agent_principals`；换 Token 不改变主体 |
| `agent_kind` | **现有** | VARCHAR(64) | java-apm / go-apm / python-apm / collector 等；Agent 声明不扩权 |
| `distribution` | **现有** | VARCHAR(128) | 发行版名称 |
| `distribution_version` | **现有** | VARCHAR(64) | Agent 发行版版本 |
| `service_name` | **现有** | VARCHAR(255) | 业务服务名称，Collector 可为空 |
| `service_namespace` | **现有** | VARCHAR(255) | 业务服务命名空间 |
| `environment` | **现有** | VARCHAR(128) | 业务环境声明 |
| `service_version` | **现有** | VARCHAR(64) | 业务应用版本；不能当 Agent 发行版版本 |
| `last_seen` | **现有** | UTC Timestamp | 最新被接受的报告时间 |
| `last_transport` | **现有** | VARCHAR(16) | http / websocket |
| `healthy` | **现有** | Nullable Boolean | 最新 Agent 健康快照；NULL 表示未知 |
| `capabilities` | **现有** | BIGINT | 兼容性限制见 §2.1 |
| `reported_hash` | **现有** | VARCHAR(64) | 最近 Agent 报告的远程配置 Hash；与 Desired 独立 |
| `config_status` | **现有** | VARCHAR(32) | Agent 原始 RemoteConfigStatus 的规范化值 |
| `config_error` | **现有** | TEXT | 最近配置错误 |
| `created_at` / `updated_at` | **拟新增** | UTC Timestamp | Identity / Metadata 生命周期 |
| `retired_at` | **拟新增** | Nullable UTC Timestamp | 受控退役，不执行级联删除 |

**已有索引**：PK `(tenant_id,instance_uid)`、`(tenant_id,last_seen)`。后续如确有 Inventory UI 按 AgentKind/Service/Env 分页的需求，可针对访问模式增加 `(tenant_id,environment,service_name,instance_uid)` 或 `(tenant_id,agent_kind,instance_uid)`。没有真实查询证据前不预建所有组合索引。

**重要边界**：P1.1 把最新 Observation 内联在实例表中，避免额外写时 Join。只有真实报告写压力造成身份表大量 UPDATE/索引维护后，才考虑 §10 的 `agent_observations` 一对一拆表；不要为了概念分层而提前实施。

## 4. 策略定义域

### 4.1 `policies` — 策略逻辑身份

**主键**：`(tenant_id,policy_id)`。Policy 本身可改名称、停用，但不包含易失的策略正文。当前唯一实际 Adapter 是自定义 Java Head Sampling；PolicyKind 的可扩展性**不表示**其他 AgentKind 已支持该策略。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `policy_id` | UUID text | PK，服务端生成 |
| `policy_type` | VARCHAR(48) | 如 `head_sampling`，以注册的 Adapter 能力约束 |
| `agent_kind` | VARCHAR(64) | 目标 AgentKind（如 java-apm） |
| `name` | VARCHAR(128) | NOT NULL，展示名 |
| `status` | VARCHAR(24) | ACTIVE / ARCHIVED |
| `created_by` | VARCHAR(128) | 管理员 Principal/Actor 引用（由 Admin IAM 授权） |
| `created_at` / `updated_at` | UTC Timestamp | NOT NULL |

可按产品 UX 决定 `UNIQUE(tenant_id,name)`；建议不要在没有“名称必须唯一”需求时添加不可逆业务约束。

### 4.2 `policy_revisions` — 不可变业务策略版本

**主键**：`(tenant_id,policy_revision_id)`；`(tenant_id,policy_id)` FK → `policies`；**唯一**：`(tenant_id,policy_id,revision_number)`。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `policy_revision_id` | UUID text | PK |
| `policy_id` | UUID text | FK → policies |
| `revision_number` | BIGINT | NOT NULL，严格单调，范围在单一 Policy 内 |
| `schema_version` | INTEGER | NOT NULL，策略正文解释版本 |
| `body_json` | TEXT / LONGTEXT | NOT NULL，应用层 canonical JSON |
| `body_sha256` | CHAR(64) | NOT NULL，正文内容摘要 |
| `created_by` | VARCHAR(128) | NOT NULL，操作者 |
| `created_at` | UTC Timestamp | NOT NULL |

`revision_number` 必须在并发创建时通过事务/CAS 或唯一冲突重试取得，不使用 `MAX(revision)+1` 的无锁读。业务规则变化产生新行；`policy_revisions` 不设可修改的 `status`，启用哪个版本由 Binding 或 Release 持有。

### 4.3 `policy_bindings` — Scope/Selector 对应生效版本

**主键**：`(tenant_id,binding_id)`。一条 Binding 是一个可更新的**选择器入口**，引用一个既有不可变 Policy Revision。所谓 Global Default 指当前 Tenant 范围默认规则，不是跨 Tenant 的隐式超级策略。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `binding_id` | UUID text | PK |
| `policy_revision_id` | UUID text | FK → policy_revisions |
| `agent_kind` | VARCHAR(64) | NOT NULL，和 Policy/Adapter 兼容 |
| `scope_type` | VARCHAR(24) | TENANT / ENVIRONMENT / SERVICE / INSTANCE |
| `selector_json` | TEXT / LONGTEXT | canonical Selector，所有可匹配键明确版本化 |
| `selector_hash` | CHAR(64) | SHA-256(selector_json) |
| `priority` | INTEGER | 同 Scope 内的显式优先级 |
| `enabled` | BOOLEAN | NOT NULL |
| `expires_at` | Nullable UTC Timestamp | Instance override TTL 或临时策略到期时间 |
| `row_version` | BIGINT | NOT NULL，Binding 修改时 CAS |
| `created_by` / `updated_by` | VARCHAR(128) | 操作者引用 |
| `created_at` / `updated_at` | UTC Timestamp | NOT NULL |

**建议索引**：`(tenant_id,agent_kind,enabled,scope_type,priority)`；**精确 Selector 防重复**可选择 `(tenant_id,agent_kind,scope_type,selector_hash,priority)` 唯一（需在正式评审中确认是否允许同 Selector 多条 Disabled 版本）。**部分重叠 Selector 的冲突不能只依赖 SQL 唯一索引**，必须由 Resolver 在稳定排序/冲突检测中拒绝同级不同内容；更新 Binding 与相关发布必须遵循事务和版本检查。Binding 中的 `service.name` 匹配值来自 Agent 声明，匹配动作必须先完成 Tenant 授权和 Adapter 能力判定。

## 5. 配置内容、发布与协调域

### 5.1 `config_revisions` — 不可变编译结果

**主键**：`(tenant_id,config_revision_id)`。一个 `policy_revision` 可因不同 AgentKind、ConfigDialect、发行版/能力要求，编译成多个不同 Config Revision。每条记录对应**完整的 OpAMP RemoteConfig ConfigMap**，不是单独的一个 `sampling.rate` 字段。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `config_revision_id` | UUID text | PK |
| `source_policy_revision_id` | Nullable UUID text | FK → policy_revisions；预留未来直接配置输入 |
| `agent_kind` | VARCHAR(64) | NOT NULL，目标类型 |
| `distribution` | VARCHAR(128) | 可为空表示某 ConfigDialect 的通用发行版 |
| `config_dialect` | VARCHAR(96) | NOT NULL，例如 `java-apm-sampling/v1` |
| `payload_format` | VARCHAR(64) | 例如 `opamp-config-map-proto` |
| `config_map_bytes` | BYTEA / LONGBLOB | NOT NULL；可以精确恢复待发 ConfigMap |
| `config_hash` | CHAR(64) | NOT NULL；实际 OpAMP `AgentRemoteConfig.ConfigHash` 的 lower-hex |
| `hash_algorithm` | VARCHAR(32) | NOT NULL，例如 `sha256-canonical/v1` |
| `payload_size_bytes` | BIGINT | NOT NULL，应用控制最大大小 |
| `created_at` | UTC Timestamp | NOT NULL |
| `created_by` | VARCHAR(128) | 编译操作者或 `system` |

**关键决定**：Hash 由 Config Adapter 按确定性规范计算并写入，不能依赖数据库 `JSONB`/JSON 的字节排列。应用必须将**同一个** `config_hash` 作为 OpAMP 的 32-byte ConfigHash 发送。不同 `config_revision_id` 可以拥有相同 `config_hash`：内容相同不等于业务版本或发布相同。必要的 `config_map_bytes` 存储对象应做大小校验、密钥/秘密扫描与访问控制；不要将内容或秘密写入普通审计日志。

### 5.2 `config_releases` — 一次发布操作

**主键**：`(tenant_id,release_id)`。Release 代表**一次人或系统发起的操作**，不是当前生效配置；即使目标为空、全部失败或被下一次发布覆盖，也应有明确的结果记录。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `release_id` | UUID text | PK |
| `policy_revision_id` | Nullable UUID text | FK → policy_revisions |
| `agent_kind` | VARCHAR(64) | NOT NULL，只对明确匹配的 Kind 发布 |
| `target_selector_json` | TEXT / LONGTEXT | NOT NULL，创建时冻结的目标查询表达式 |
| `request_sha256` | CHAR(64) | NOT NULL，规范请求摘要，判断幂等键冲突 |
| `idempotency_key` | Nullable VARCHAR(128) | `UNIQUE(tenant_id,idempotency_key)` |
| `rollout_plan_json` | TEXT / LONGTEXT | NOT NULL，P2 可以为单批次，P3 扩展灰度参数 |
| `status` | VARCHAR(24) | PREPARING / PENDING / RUNNING / PAUSED / COMPLETED / FAILED / CANCELLED |
| `row_version` | BIGINT | NOT NULL，状态转换 CAS |
| `rollback_of_release_id` | Nullable UUID text | 同 Tenant 内原 Release 的 FK |
| `requested_by` | VARCHAR(128) | 管理 API 已授权 Actor |
| `created_at` / `started_at` / `finished_at` | UTC Timestamp | created NOT NULL，后两者可空 |

`idempotency_key` 相同且 `request_sha256` 相同的重试返回**同一个 Release**；同 Key 但请求不同返回明确 Conflict。**不要通过“再次 INSERT Release”处理已提交后的网络失败。** `status` 可变，但请求正文、目标选择器和计划一旦进入执行不得静默改写。

### 5.3 `release_targets` — 冻结发布目标与结果

**主键**：`(tenant_id,release_id,instance_uid)`。三个关键 FK：

- `(tenant_id,release_id)` → `config_releases`
- `(tenant_id,instance_uid)` → `agent_instances`
- `(tenant_id,config_revision_id)` → `config_revisions`

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `release_id` | UUID text | PK/FK |
| `instance_uid` | 32-char hex | PK/FK |
| `config_revision_id` | UUID text | NOT NULL/FK，Adapter 为该目标编译的内容 |
| `batch_no` | INTEGER | NOT NULL，P2 固定 0；P3 灰度分批 |
| `status` | VARCHAR(24) | PENDING / WAITING_ACK / APPLIED / FAILED / TIMED_OUT / UNSUPPORTED / SUPERSEDED |
| `desired_generation` | BIGINT | 本次发布为此 Agent 产生的版本号；不随更晚 Release 改写 |
| `last_error` | TEXT | 最近目标执行失败信息 |
| `created_at` / `updated_at` | UTC Timestamp | NOT NULL |

**目标集合冻结**：Release 在准备阶段查询符合条件的 Agent，完成目标身份/能力校验，写入这些 Target。之后匹配的新 Agent **不能作为旧 Release 的隐式成功目标**。Target 的成员身份、Config Revision、Batch Number、Generation 不允许变更；只有本 Release 的执行状态与错误可更新。后续新 Release 使旧 Target 变成 SUPERSEDED 时，保留它的结果和原因。

索引：`(tenant_id,release_id,batch_no,status)` 用于 Rollout 进度；`(tenant_id,instance_uid,created_at)` 便于查看单 Agent 发布历史（仅在查询实际需要时建立）。

### 5.4 `agent_assignments` — 每 Agent 的当前 Desired/Observed

**主键**：`(tenant_id,instance_uid)`；FK → Agent Instance。**一个 Agent 首期只有一个完整的 OpAMP RemoteConfig Assignment**：不要为每个 JSON 子项分配互相冲突的独立 Hash。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `instance_uid` | 32-char hex | PK/FK |
| `release_id` | UUID text | NOT NULL/FK，创建当前 Desired 的 Release |
| `config_revision_id` | UUID text | NOT NULL/FK，当前完整配置 |
| `desired_hash` | CHAR(64) | NOT NULL，取自 Config Revision |
| `desired_generation` | BIGINT | NOT NULL，每次改变当前发布意图时严格递增 |
| `row_version` | BIGINT | NOT NULL，每次成功状态更新 +1，供 CAS |
| `state` | VARCHAR(24) | PENDING / WAITING_ACK / APPLYING / APPLIED / FAILED / TIMED_OUT / UNSUPPORTED |
| `observed_hash` | Nullable CHAR(64) | 最后一次上报 Hash，可与 Desired 不同 |
| `observed_status` | Nullable VARCHAR(32) | Agent 原始状态映射 |
| `observed_at` | Nullable UTC Timestamp | 最后一次有效回执时间 |
| `attempt_count` | INTEGER | NOT NULL，当前 Desired 的投递尝试次数 |
| `last_sent_at` / `next_retry_at` | Nullable UTC Timestamp | 投递与重试调度 |
| `last_error` | TEXT | 不泄漏配置正文或 Secret |
| `updated_at` | UTC Timestamp | NOT NULL |

**归属一致性**：建议 `(tenant_id,release_id,instance_uid)` 在 `agent_assignments` 中以复合 FK 指向 `release_targets`，确保当前 Assignment 确实属于这次发布的冻结目标。写入时还须检查 `config_revision_id`、`desired_generation` 与目标快照一致；必要时为 Target 建立含这些列的唯一键并设置相应复合 FK，而不是只在 Go 中分别验证独立 Release ID 与 ConfigRevision ID。

**CAS 示例（伪 SQL，GORM 负责方言占位符）**：

~~~sql
UPDATE agent_assignments
   SET state = 'APPLIED',
       observed_hash = :reported_hash,
       observed_status = 'APPLIED',
       observed_at = :received_at,
       row_version = row_version + 1
 WHERE tenant_id = :tenant_id
   AND instance_uid = :instance_uid
   AND desired_generation = :expected_generation
   AND desired_hash = :reported_hash
   AND row_version = :expected_row_version;
~~~

必须检查 `RowsAffected == 1`；没有命中时是过期、并发冲突或已被新 Release 替代，不得直接 `Save()` 覆盖。客户端 OpAMP 回执**没有服务端的 desired_generation**：它是服务端读到当前 Assignment 后用于保护 CAS 的版本，不能声称 Agent 直接确认了 Generation。**相同 Hash 跨 Release 重用时，匹配 Hash 只能证明内容状态，不能为新 Release 生成无根据的“新应用成功”证据**；需要状态机显式继承先前内容证明或记录缺乏新的回执。

索引：`(tenant_id,state,next_retry_at)` 支持 Reconciler 扫描；主键支持按 Agent 查询；`(tenant_id,release_id)` 用于按 Release 归集当前成员状态。避免为频繁变化的状态列无节制增加索引。

### 5.5 `config_events` — 审计与回执历史

**主键**：`(tenant_id,event_id)`。这是**事实/审计事件记录**，不是替代 Agent Assignment 的唯一状态源；只记录有意义的变更、OpAMP 配置回执、失败或管理操作，不把每次心跳和每个 Span 写成审计事件。

| 字段 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `tenant_id` | VARCHAR(96) | PK/FK |
| `event_id` | UUID text | PK |
| `event_type` | VARCHAR(48) | 如 RELEASE_CREATED、TARGET_ASSIGNED、CONFIG_SENT、CONFIG_APPLIED、CONFIG_FAILED、ROLLBACK_REQUESTED |
| `release_id` | Nullable UUID text | FK → config_releases |
| `instance_uid` | Nullable 32-char hex | FK → agent_instances |
| `desired_generation` | Nullable BIGINT | 回执关联时服务端看到的 Generation |
| `config_hash` | Nullable CHAR(64) | 当次报告的 Hash |
| `reported_status` | Nullable VARCHAR(32) | Agent 上报状态（不造一个成功状态） |
| `actor_type` | VARCHAR(24) | ADMIN / AGENT / SYSTEM |
| `actor_id` | VARCHAR(128) | 授权 Actor 引用；非 Agent 自报字段 |
| `correlation_id` | Nullable VARCHAR(128) | 请求/Trace/发布链路相关性 |
| `details_json` | TEXT / LONGTEXT | 非敏感、受大小限制的事件内容 |
| `occurred_at` | UTC Timestamp | NOT NULL，平台记录的时间 |

**建议索引**：`(tenant_id,release_id,occurred_at)`、`(tenant_id,instance_uid,occurred_at)`，以及必要时 `(tenant_id,occurred_at)`。审计写入和核心状态更新应在同一个业务事务内提交，避免出现“发布已经成功却无事件”或“事件存在但业务状态未提交”。

事件 append-only 的要求应通过写路径、数据库角色权限和运维审计保证；真实保留期限、归档与脱敏/删除策略需与合规要求另行评审，不在这里发明固定 TTL。

## 6. 生命周期与数据一致性

### 6.1 Agent 注册和 Token 轮换

~~~text
Credential Token -> 可信认证系统 -> (tenant_id, credential_ref, principal_id)
     -> 检查 Principal ACTIVE -> 校验 UID
     -> agent_instances(tenant_id, instance_uid, owner_principal_id)
     -> 观察快照 -> 返回 OpAMP 响应
~~~

P1.1 **实际实现**只有 `credential_id` 首次绑定：在同一 Tenant 内通过其他 Credential 上报相同 UID 会被拒绝。上述稳定 `principal_id` 的模型是后续目标设计，必须通过新增字段、回填和迁移实现，不能靠把新 Token 的 `credential_id` 随意覆盖来“完成轮换”。

身份删除/轮换授权必须是显式受控操作，不允许 Agent 单方面报告一个新 Principal 即完成换绑。HTTP 请求执行完毕后不保留 Session；WebSocket Session 在内存里，有连接所有权和关闭回调。未来多实例 HA 的 Fencing / Session Ownership **不在此 Schema 中冒充已解决**。

### 6.2 创建策略与绑定

Admin 在 Tenant Scope 内创建 `policies`；新增 `policy_revisions` 后，必要时更新 `policy_bindings` 的当前 Revision 与 RowVersion。Resolver 根据 Tenant/Kind、Scope、Selector、Priority 计算唯一结果。同级不同内容的重叠 Selector 应返回 Conflict，而不是依赖数据库排序选中第一条。业务正文改变 → 新 Revision；生效指向变化 → 更新 Binding 并追加事件。

### 6.3 发布事务与冻结目标

~~~mermaid
sequenceDiagram
    participant API as Management API
    participant DB as Database
    participant R as Reconciler
    participant A as OpAMP Agent
    API->>DB: 校验 Tenant / PolicyRevision / Adapter
    API->>DB: TX: INSERT ConfigRevision（如未存在）
    API->>DB: TX: INSERT Release + Frozen Targets
    API->>DB: TX: CAS 更新 AgentAssignments
    API->>DB: TX: INSERT ConfigEvents
    DB-->>API: COMMIT
    API-->>R: 触发扫描（可丢的通知）
    R->>DB: 读取 Pending Assignment 与 ConfigRevision
    R->>A: WS 推送，或等待 HTTP 下一次请求
    A-->>R: RemoteConfigStatus + ConfigHash
    R->>DB: TX: 追加 Event + CAS 更新 Observed
~~~

**P2 的发布请求必须定义一个有界的原子目标集合**：请求中所有目标的 Revision、Frozen Target、Assignment、Event 应同一事务成功或失败。不能先向 Agent 发配置、再尝试保存 Desired。批量特别大时要另外设计 PREPARING 状态、批量事务与恢复过程，不能继续声称“一个跨多事务发布完全原子”。当前不宣称已有百万目标发布能力。

新 Agent 若在冻结之后才匹配 Binding，不应偷偷计入旧 Release；未来自动纳管应生成**新的、可审计的系统发布意图**，或等待下一次显式发布。这一行为在 P2 API 契约批准前保留为提议。

### 6.4 Reconciler、重连、旧 ACK 与重复 Hash

| 场景 | 正确存储行为 |
| --- | --- |
| 发布事务失败 | 不发送新配置；无半成品 Target/Assignment |
| Release 提交，但 Gateway 推送失败 | Assignment 保持 Pending/WaitingAck，记录失败/下次重试；Desired 不回滚 |
| Agent 通过 HTTP 轮询 | 下次请求返回完整 ConfigMap；不能写“已通过 WebSocket 推送” |
| Agent 重连或 Control Plane 重启 | 从 DB 读当前 Assignment/Revision，按能力重新协调 |
| 回执 Hash == 当前 Desired 且状态 APPLIED | CAS 更新当前 Assignment 的**内容应用状态**并写事件 |
| 回执 Hash 是更早 Revision | 追加晚到回执事件；新 Desired 不变 |
| 两次 Release 使用同一个 Hash | 保留不同 Release ID；不能因内容相同杜撰第二次真实应用操作 |
| Agent 声明不支持 RemoteConfig / Dialect 不兼容 | Target/Assignment 明确 UNSUPPORTED，不向 Agent 发送 Java 专有 JSON |
| 相同 idempotency_key 的重试 | 原请求相同返回原 Release；不同请求 Conflict |
| 配置回滚 | **创建新 Release** 指向已验证旧 Revision，原 Release 与历史事件不删除 |

### 6.5 批次灰度（P3）

P2 先只使用 `release_targets.batch_no = 0`。P3 的灰度批次可以先在 Release 的 `rollout_plan_json` 中记录批次规则，并通过 `release_targets` 归集状态；只有出现需要**单批次审批、独立暂停/重试、批次负责人**的真实需求，才引入 `release_batches` 表。灰度失效时应将后续目标保持未投递；回滚的执行记录属于一个新的 Release。

## 7. 索引和常见查询

| 读写路径 | 访问条件 | 所需索引 / 约束 |
| --- | --- | --- |
| Agent 详情 / 状态写入 | Tenant + UID | `agent_instances` PK |
| 按时间列出 Agent | Tenant + LastSeen 范围 | 现有 `agent_instances(tenant_id,last_seen)` |
| 获取策略的版本 | Tenant + PolicyID + RevisionNumber | `policy_revisions` 唯一键 |
| 查询适用 Binding | Tenant + Kind + Scope + Enabled | `policy_bindings` 复合索引 |
| 根据 Release 列出某 Batch 的目标及结果 | Tenant + ReleaseID + BatchNo + Status | `release_targets` 复合索引 |
| 获取某 Agent 当前期望 | Tenant + InstanceUID | `agent_assignments` PK |
| 扫描待重试任务 | Tenant + State + NextRetryAt | `agent_assignments` 扫描索引 |
| 查看 Release 进度 | Tenant + ReleaseID | `config_releases` PK 和 `release_targets` |
| 单 Agent 审计时间线 | Tenant + InstanceUID + OccurredAt | `config_events` 时间索引 |
| 按请求幂等键去重 | Tenant + IdempotencyKey | `config_releases` UNIQUE（NULL 允许多个） |

**写入热点注意**：`agent_instances` 的 LastSeen 变化会维护现有 `(tenant_id,last_seen)` 索引。Agent 高频心跳应在 OpAMP 层有节制地刷新数据库，不必把每次 Polling 变成一次同步 UPDATE；但延迟上限和刷新策略不能未经产品 SLO 确认就硬编码成不可靠的“在线”定义。

首次不引入 Redis、Kafka、分库分表、数据库物化 View、分布式消息事务或复杂 ORM 多层泛型；GORM 使用显式的 Repository/Tx 边界即可。真正出现查询和并发问题时再依据指标增加索引、热表或协调队列。

## 8. 可参考的物理 DDL 片段（**未执行**）

这段只说明 PostgreSQL 的复合键与 CAS 约束风格，**不是**可直接执行的全量 Migration，也不意味着 P1.1 Schema 已变更：

~~~sql
-- 提议示例：首先在独立 Goose Migration 中建立 tenants/principals
-- 并完成已有 agent_instances 的 tenant/credential 数据回填。
CREATE TABLE policies (
    tenant_id VARCHAR(96) NOT NULL,
    policy_id VARCHAR(36) NOT NULL,
    policy_type VARCHAR(48) NOT NULL,
    agent_kind VARCHAR(64) NOT NULL,
    name VARCHAR(128) NOT NULL,
    status VARCHAR(24) NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, policy_id),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id)
);

CREATE TABLE policy_revisions (
    tenant_id VARCHAR(96) NOT NULL,
    policy_revision_id VARCHAR(36) NOT NULL,
    policy_id VARCHAR(36) NOT NULL,
    revision_number BIGINT NOT NULL,
    schema_version INTEGER NOT NULL,
    body_json TEXT NOT NULL,
    body_sha256 CHAR(64) NOT NULL,
    created_by VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, policy_revision_id),
    UNIQUE (tenant_id, policy_id, revision_number),
    FOREIGN KEY (tenant_id, policy_id)
        REFERENCES policies(tenant_id, policy_id)
);
~~~

注意 PostgreSQL 的外键引用要求被引用列组合有 PK/UNIQUE；MySQL 还必须统一列长度、类型、字符集与 Collation，否则复合外键可能无法建立。实际 MySQL Migration 应独立编写，不将 PostgreSQL SQL 做机械字符串替换。

## 9. 从已落地 P1.1 演进的 Migration 路线

**当前的 `00001_agent_instances.sql` 不重写。** 历史 Migration 一旦合入并执行，应通过新增 Goose 版本变更数据库，而不是修改第 1 版 SQL。以下阶段与文件名**仅表示建议顺序，不代表文件已经存在**。

| 阶段 | 新的 Migration 内容 | 应用兼容/回填要求 |
| --- | --- | --- |
| 先决：P1.1 | `agent_instances` 已部署 | 保留现有租户/UID PK、Credential Owner、Admin API 契约 |
| M-Auth（P2） | `tenants`、`agent_principals`、`credential_bindings`，向 `agent_instances` 添加 nullable `owner_principal_id` | 按 `(tenant_id,credential_id)` 安全构建一对一过渡 Principal，不自动跨 Tenant 合并；旧记录身份可追溯 |
| M-Auth switch | 双读/受控双写 → 回填校验 → 应用改用 Principal → 按正式批准的安全流程约束非空 | Token 轮换必须可用；核对异步请求和跨库约束后才清理旧字段 |
| M-Policy（P2） | `policies`、`policy_revisions`、`policy_bindings` | revision 不可变；旧演示采样率不自动成为已批准业务 Policy |
| M-Config（P2） | `config_revisions`、`config_releases`、`release_targets`、`agent_assignments`、`config_events` | 冻结目标、CAS、内容 Hash、审计同 Tx；重启可恢复 |
| M-Rollout（P3） | 在已有 Release/Target 中扩展灰度字段与索引 | 状态迁移无歧义，旧 P2 Release 仍可读；需要时再新增 release_batches |
| M-Scale（仅按证据） | 独立 `agent_observations`、`apm_services`、分区归档或可选 `release_batches` | 先以真实查询/写压力与保留义务证明必要性 |

**安全迁移步骤（建议）**：先新建表/新增 Nullable 列 → 部署兼容读写 → 离线或受控批次回填 → 验证行数、Tenant/UID 归属与 FK 冲突 → 切换读路径 → 可选强化 NOT NULL/约束 → 另一次发布清理旧列。发生冲突应停止并人工审计，不通过“最后一次凭证胜出”消解 Agent 所有权。

在 MySQL 中 `CREATE/ALTER TABLE` 可能隐式提交；即使用 Goose，也不能把 Migration 失败理解为所有 DDL 已回滚。Goose 命令应由独立 Job 管控；服务只检查 DB/Schema 兼容版本，不通过 GORM AutoMigrate 改生产表。真实兼容必须分别在 PostgreSQL/MySQL 的 CI 和升级/恢复演练中确认。

## 10. 延迟引入的三个扩展表

| 可选表 | 引入条件 | 推荐关键字段 / 约束 |
| --- | --- | --- |
| `agent_observations` | Agent 状态写入压力使 Inventory 表写放大、索引维护成为明确瓶颈 | `(tenant_id,instance_uid)` PK/FK，一对一保存 LastSeen、Health、Capabilities、Reported Hash/Status；现有 agent_instances 最新状态字段迁移后收敛 |
| `apm_services` | 需要服务目录属性/Ownership/标签，且不再满足于 Agent 自报 Service 坐标 | `(tenant_id,service_id)` PK，`(tenant_id,environment,namespace,name)` 业务唯一键；Collector 不强制属于 Java Service |
| `release_batches` | P3 需要每批独立审批、人员权限、暂停或恢复记录 | `(tenant_id,release_id,batch_no)` PK/FK，Batch Status、ApprovedBy、StartedAt、CompletedAt |

这些都是**候选扩展**。即使逻辑概念中有 Agent Observation、APM Service 或 Rollout Batch，也不表示首个关系型实现必须拥有同名物理表。

## 11. 评审与验收清单

| 设计/实现校验 | 应提供的可验证证据 |
| --- | --- |
| Tenant FK / PK | 每个跨表引用都包括 Tenant；跨租户 FK/写入负向测试 |
| Agent 凭证轮换 | 新旧 Credential 指向同一稳定 Principal，轮换后原 UID 不被错误拒绝；撤销旧凭证后不能写入 |
| Release 事务 | Revision + Release + Frozen Target + Assignment + Event 全成功或全失败 |
| Assignment CAS | 双发布竞争、过期 Generation、并发 ACK 不覆盖新 Desired |
| ACK 内容语义 | 旧 Hash 事件保留；相同 Hash 跨 Release 不虚构新的应用证据 |
| Config 不可变 / 完整恢复 | 同字节 ConfigMap、Hash、Dialect 和 AgentKind，重启后可再次发送 |
| Selector 冲突 | 相同优先级、部分重叠 Selector、Instance Override TTL 等确定性行为 |
| HTTP / WS | HTTP 回执和 freshness、WS 单 writer 和会话所有权分别测试 |
| MySQL / PostgreSQL | 分方言 Goose 升级、约束冲突、UTC、字符集、事务和重试语义实测 |
| 审计与回滚 | 新 Release/事件留痕、旧 Release 不删除、Config 与 Token Secret 不落普通日志 |
| 容量与保留 | 无未经测量的吞吐或固定 TTL 声明；根据实际需求确定索引、保留/归档策略 |

## 12. 开放决策与后续任务

| 决策 | 本文暂定方向 | 尚待确认的权威/条件 |
| --- | --- | --- |
| 稳定 Principal 真源 | 从可轮换 Credential 分离；DB 表保存引用而非 Secret | **Identity/Security Design Owner**：mTLS vs Token、授权主体边界、撤销传播 |
| UID 接管和 Session Fencing | 单实例拒绝活动 WS 并发抢占；不基于 Agent 声明换绑 | **Identity/Security Design Owner**：断线重连、令牌轮换、未来多实例 HA |
| 多目标发布与重叠 Selector | 发布事务冻结目标、Resolver 显式冲突 | **Policy/API Owner**：优先级、重试、批次、目标数量和自动纳管语义 |
| OpAMP 相同 Hash 多 Release | 内容证明可复用但不能宣称新的应用操作 | **Config Contract Owner**：业务 UI/APPLIED 定义与统计口径 |
| JSON 与二进制配置大小 | 由 Adapter 规范编码，遵循协议限制与安全扫描 | **Config Adapter Owner**：最大可接受配置、完整 ConfigMap Hash 算法 |
| 生产数据保留与合规删除 | 审计追加，保留/归档独立治理 | **Security/Compliance Owner**：TTL、备份、法律保留与删除策略 |

**推荐的下一个可执行增量**：在审阅此逻辑模型、尤其是稳定 Principal、Release Frozen Target、Assignment CAS 三个边界后，为 P2 创建**独立 Migration/实现 PR**。不要把此文档当作已执行 Schema 或让文档的 Proposed 状态自动解除 DD-001 / EP-001 的 Blocker。

## 13. 维护与溯源

- P1.1 真实 Migration：[PostgreSQL](../../db/migrations/postgres/00001_agent_instances.sql) / [MySQL](../../db/migrations/mysql/00001_agent_instances.sql)。
- 当前 GORM `AgentRecord`、`Observe`、`Get`、`List`：[store.go](../../internal/storage/gormrepo/store.go)。
- 需求不变量：[FR-001–FR-013](../requirements/APM_CONTROL_PLANE.md)。
- 主架构与发布状态：[开发者架构指南](README.md)、[DD-001](../design-docs/dd-001_opamp-control-plane.md)、[EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md)。
- 设计归属：本文为独立技术详设，**不是**新的受治理 DD-NNN，也不是批准快照；长期权威仍归 DD-001/后续被批准的设计与 ADR。实施后如出现偏差，应先更新对应批准的设计和此文件，再增加新的 Goose Migration。
