# P1.1 — Durable Agent Inventory 开发与部署说明

> 状态：功能分支 `feat/p1-durable-agent-inventory`，**尚未合并或宣称生产就绪**。本文件描述本 PR 的实现、运行步骤和已知边界；DD-001 仍为 Draft，EP-001 保持 Blocked，身份架构尚须正式评审。

## 1. 设计目标

第一条完整链路为 OpAMP Agent 状态上报 → 可信 Credential 映射 Tenant/Principal → Registry → GORM 持久化 → Management API 查询。在进程重启后，数据库仍保留最近的 Agent Identity、Capabilities、Health、ConfigStatus 和 LastSeen。

本阶段**不实现** PolicyRevision、ConfigRelease、Reconciler、真实 Java Runtime 动态采样、HA Session 转发或全量生产 IAM。P0 演示的单 Agent `PUT /sampling` 继续使用内存状态，因此重启后**该演示配置仍可能丢失**；这与 Inventory 持久化是不同承诺。

## 2. 模块与状态所有权

```text
OpAMP Gateway (cmd/server)
     │  认证+协议描述转换
     ▼
Registry  ───────────────▶ GORM Agent Store
     │                      │
     │  内存 WS 连接/临时配置  ├─ PostgreSQL
     │                      └─ MySQL
     ▼
Management API (按 Admin Principal 的 tenant_id 过滤)
```

- `internal/appconfig`：Koanf 加载可选 YAML，按白名单环境变量覆盖，验证启动参数。Agent/Admin Credential 只能通过环境变量或 Secret 提供。
- `internal/auth`：对 Bearer Token 计算 SHA-256 并定长比较，映射到可信 `tenant_id` / `credential_id`。
- `internal/storage/gormrepo`：唯一键 `(tenant_id, instance_uid)`，首次注册绑定 `credential_id`；相同 UID 如由不同 Credential 冒领会拒绝。GORM Model 不暴露到 OpAMP Protobuf。
- `cmd/server`：维护短生命周期 WS Session、P0 采样临时配置和 HTTP Polling 活跃度计算。WS 旧连接关闭后可重连；活跃同 UID 的并发连接拒绝覆盖。
- `db/migrations/postgres`、`db/migrations/mysql`：Goose SQL，**生产禁止 AutoMigrate**。

只有 Agent 最新 Observation 入库；活动 `types.Connection` 不会写数据库。API 中的 `connected` 字段对 WS 表示当前活动连接，对 HTTP 表示 LastSeen 在 `http_polling_ttl` 期限内的**估计活跃**，不是持久 TCP 连接。

## 3. 运行准备

Go 1.25+。数据库须事先建立 `controlplane` 数据库并赋予所需 DDL/DML 权限，且仅在应用启动前通过 Goose 迁移。示例只适合本地开发；实际部署需要 TLS、Secret 管理和明确的 Tenant 范围。

先安装固定版本 Goose CLI：

```bash
go install github.com/pressly/goose/v3/cmd/goose@v3.27.0
```

PostgreSQL 示例：

```bash
export APM_DB_DRIVER=postgres
export APM_DATABASE_DSN='postgres://app:replace@127.0.0.1:5432/controlplane?sslmode=disable'
goose -dir db/migrations/postgres postgres "$APM_DATABASE_DSN" up
```

MySQL 示例：

```bash
export APM_DB_DRIVER=mysql
export APM_DATABASE_DSN='app:replace@tcp(127.0.0.1:3306)/controlplane?parseTime=true&loc=UTC'
goose -dir db/migrations/mysql mysql "$APM_DATABASE_DSN" up
```

Goose 执行的目录必须与当前选定 Driver 对应。MySQL DDL 可能发生隐式提交，不能依赖整个建表 Migration 的数据库事务回滚。

## 4. 配置与凭证

设置 `APM_CONFIG_FILE=configs/example.yaml` 可加载非敏感 YAML。必须设置 `APM_DATABASE_DSN`；驱动只支持 `postgres`、`mysql`。若未指定配置文件，监听默认 `127.0.0.1:4320`。

**推荐的 Scoped Credential 格式**（环境变量，不存 Git 或 YAML）：

```bash
export APM_AGENT_CREDENTIALS_JSON='[
  {"id":"agent-prod-a","tenant_id":"team-a","token":"replace-with-a-high-entropy-random-secret"}
]'
export APM_ADMIN_CREDENTIALS_JSON='[
  {"id":"admin-prod-a","tenant_id":"team-a","token":"replace-with-a-different-random-secret"}
]'
```

示例中的 Token 必须替换成安全生成的高熵随机凭证。Agent 携带相应 Bearer Token，服务端将该凭证绑定到 `team-a`。Admin 对 `/api/v1/agents` 的列表与单 Agent 查询仅能看到自己的 Tenant；不同凭证即使自报相同 `instance_uid` 也不能无授权地接管同一 Tenant 内已有 Agent。

为兼容 P0 本地演示，若没有提供 JSON 凭证配置，仍可以设置 `APM_OPAMP_TOKEN` 和 `APM_ADMIN_TOKEN`，两者被映射到 `default` Tenant 的不同 Credential。**这只是开发兼容路径，不应当当作生产多租户凭证发行机制。**

启动：

```bash
go run ./cmd/server
```

另一个终端使用测试 Agent。该程序当前读取 `APM_OPAMP_TOKEN`；若使用 Scoped Agent Token，给它赋予与 `APM_AGENT_CREDENTIALS_JSON` 中对应的 Token：

```bash
export APM_OPAMP_TOKEN='replace-with-a-high-entropy-random-secret'
go run ./cmd/testagent -service checkout
```

API 查询：

```bash
curl -H 'Authorization: Bearer replace-with-a-different-random-secret' \
  http://127.0.0.1:4320/api/v1/agents
```

调用者应替换测试机地址与 Secret；不建议将真实 Token 放进 Shell 历史或 CI 日志。

## 5. 与 P0 相比的变化

| 能力 | P0 | P1.1 目标实现 |
|---|---|---|
| Inventory | 内存 Map | GORM 关系型持久化 |
| 数据库 | 无 | Postgres 或 MySQL 二选一 |
| 认证 | 共享 Agent/Admin Token | Credential ID + Tenant Scope；保留 P0 兼容 |
| Agent Identity | UID + service.version | UID、Kind、Distribution、Distribution Version 与 Service Version 分离 |
| HTTP 在线状态 | OnConnectionClose 可能置离线 | LastSeen + TTL |
| WS 会话 | 新连接覆盖可能产生状态竞争 | 同 UID 活跃连接禁止覆盖，关闭后可重连 |
| Management API | 三个基础路由 | 兼容原路由，增加 Tenant Scope 和元数据字段 |
| P0 Sampling Demo | 内存 | 继续内存；持久版本化配置放在 P2 |

**安全限制**：P1.1 还没有 mTLS、Token 撤销/轮换控制台、生产 RBAC、会话 Generation Fencing 或跨 Gateway HA。Credential ID 在配置中是可信声明，但 Token 到 Instance UID 的长期发行和撤销仍需在身份设计评审中确认。

## 6. 功能验收

- 常规：`go test -race ./...`、`go vet ./...`。
- PostgreSQL/MySQL：`.github/workflows/p1-database-contracts.yml` 启动真实服务、运行对应 Goose Migration，再通过 `go test -tags=integration -race ./internal/storage/gormrepo` 检查唯一性、身份所有权、Tenant 隔离、状态压缩后字段保留、服务端重建 Store 后的数据恢复。
- RF：`repofoundry --repo . validate` 和锁定 Specs 的校验。
- P0 回归：原有 GET List、GET Detail 和 PUT Sampling JSON 契约继续按租户生效；测试 Agent 不代表真实 JVM。

本说明将“已经有代码”与“已经通过真机/生产验收”分开表述；PR CI 没有通过之前，不得标记 P1 完成。
