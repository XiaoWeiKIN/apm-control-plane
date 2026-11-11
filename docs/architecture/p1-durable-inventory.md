# P1.1 — Org-scoped Durable Agent Inventory

> **状态**：P1.1 的 Agent Inventory 已在 `main` 合并；当前实现修订使用 `org_id`。此说明为开发/测试运行手册，不宣称真实 Java Agent 热更新、企业 IAM 集成或生产可用性。DD-001 仍是 Draft，EP-001 仍是 Blocked。

## 1. 数据与边界

```text
可观测性平台已有 Org（统一组织/组织）
      │  可信部署凭证映射 org_id
      ▼
OpAMP HTTP / WebSocket Agent -------> Registry -------> GORM Store
                                             │             ├ PostgreSQL
Admin Bearer Credential ---------------------┘             └ MySQL
      │                                 活动 WS 会话只保存在内存
      └---- 仅查询被授权 Org 的 Agent
```

本项目的 `Org` 是**与平台 Org/IAM 同一实体的顶层组织/租户边界（int64 / BIGINT）**，而不是独立的 APM 专属组织、项目或环境。Org 主数据、成员关系、角色和停用状态由现有平台负责；P1.1 从部署者配置的认证凭证提取作用域，**目前并未集成平台 IAM / Org 查询**。Agent 自报 `org_id`、`service.name` 和 `environment` 不具有授权能力。

Agent 身份键为 `(org_id,instance_uid)`，首次注册保留 `credential_id` 的所有权绑定。数据库保存 Agent Kind、发行版/版本、业务 Service 与最近 Observation；WebSocket 连接不写数据库。HTTP 活跃度按最近上报与配置 TTL 推断，不能解释为持久 Socket。

## 2. 数据库初始化

PostgreSQL / MySQL 使用统一的 `datafox-schema/admin/migrations` Go DSL，所有业务表以 `org_id` 作为组织隔离字段。

先在空数据库上运行独立 Schema Job：

```bash
export DATAFOX_SCHEMA_DSN="$APM_DATABASE_DSN"
datafox-schema migrate up --domain=admin
```

MySQL 使用 schema CLI 支持的 URL DSN，运行服务仍使用对应的 Go MySQL DSN（参见根目录 README）。成功后运行时验证 `admin_agent_management_schema` 迁移历史和必需列；若已存在未管理的表会直接失败，不自动接管或回填。**运行时不执行 AutoMigrate。**

## 3. 启动配置与凭证

可选非敏感配置：`APM_CONFIG_FILE=configs/example.yaml`；数据库 DSN 从 `APM_DATABASE_DSN` 环境变量或 Secret 读取。

Agent 和 Admin Credential **均须通过两个独立环境变量**传入，JSON 格式：

```json
[
  {"id":"agent-dev","org_id":1,"token":"replace-with-a-long-random-agent-token"}
]
```

分别设置 `APM_AGENT_CREDENTIALS_JSON` 和 `APM_ADMIN_CREDENTIALS_JSON`，Admin 使用不同凭证，例如：

```bash
export APM_AGENT_CREDENTIALS_JSON='[{"id":"agent-dev","org_id":1,"token":"replace-with-a-long-random-agent-token"}]'
export APM_ADMIN_CREDENTIALS_JSON='[{"id":"admin-dev","org_id":1,"token":"replace-with-a-different-long-admin-token"}]'
go run ./cmd/server
```

服务端**不再支持**仅配置 `APM_OPAMP_TOKEN` / `APM_ADMIN_TOKEN` 就创建默认 Org 的旧回退；旧 `tenant_id` JSON 字段会被拒绝。必须明确提供可信 `org_id`。不同 Org 可以有独立 Agent 与 Admin 凭证；凭证 ID / Token 在 Agent、Admin 两类中均不得冲突。示例中的 Token 需要替换成高熵生成的真实值，不能作为生产秘密使用。

`cmd/testagent` 作为客户端**仍读取** `APM_OPAMP_TOKEN` 来设置 Authorization Bearer Header（这个变量不用于服务端身份自动映射）。在另一个终端运行：

```bash
export APM_OPAMP_TOKEN='replace-with-a-long-random-agent-token'
go run ./cmd/testagent -service checkout
```

使用对应 Org 的 Admin Token 查询：

```bash
curl -H 'Authorization: Bearer replace-with-a-different-long-admin-token' \
  http://127.0.0.1:4320/api/v1/agents
```

响应字段 `org_id` 表示可信凭证的 Org 作用域，不会返回 `tenant_id`。跨 Org 查询、配置发布和 Agent UID 冒领必须拒绝，不能信任 Agent 自报属性扩大权限。

## 4. 当前状态和未来工作

| 功能 | P1.1 实现 | 未来阶段 |
| --- | --- | --- |
| PostgreSQL/MySQL Inventory | GORM 与 Goose 初始化表，Org 作为复合主键第一列 | 数据量增长后再决定是否增加独立 Observation 热表 |
| Agent 凭证 | Token → 已配置 Org/Credential | 平台 IAM、Token 轮换/撤销、稳定 Agent Principal |
| Agent 连接 | HTTP freshness / WS Session 内存状态 | 多实例 Session Fencing / HA |
| Admin API | Org-scoped Inventory CRUD 读取 | 统一的 Org RBAC/权限检查 |
| 配置管理 | P0 Sampling 仍在内存 | P2 ConfigRevision、Release、Assignment、Reconciler |
| Rollout | 尚未实现 | P3 批次、回滚、审计 |

本代码不自动创建 Org。Org 是否存在以及 Agent 是否拥有该 Org 的权限，现阶段依赖**可信部署/Secret 管理者**配置；对接统一可观测平台的权威 Org/IAM 是独立安全设计任务。不要将具备 `org_id` 字段等同于已经实现平台级权限集成。

## 5. 验收与状态

- `go test -race ./...`、`go vet ./...`
- `.github/workflows/p1-database-contracts.yml`：在真实 PostgreSQL/MySQL 上跑两份 Goose 初始 Migration、Agent CRUD、Credential Ownership、跨 Org 隔离、OpAMP HTTP → Management API → Store 重建恢复。
- RepoFoundry Validation：源码与文档状态保持 Draft/Blocked，没有伪造正式 Design Owner 批准。

以上检查通过只能证明既定 P1.1 约束；**不代表已完成生产 TLS、IAM、可信 Org 存在性校验、Agent 二进制升级或实际 JVM Sampling**。
