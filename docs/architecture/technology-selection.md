# APM Control Plane 技术选型（P1–P3）

> **状态**：技术选型建议 / 待评审（Proposed）；本文件不是 Accepted ADR、已批准的 DD-001，也不代表依赖已经引入或功能已实现。
>
> **时间与基线**：2026-10-19；`main` 已有 Go 1.25、`opamp-go v0.25.0`、`net/http`、`slog` 和 P0 内存 Registry。目标阶段见 [EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md)，系统边界见 [开发者架构指南](README.md) 和 [DD-001](../design-docs/dd-001_opamp-control-plane.md)。
>
> **范围**：为通用 OpAMP Agent Control Plane 选择基础 Go 设施。首个部署对象是自定义 Java APM Agent；多语言 Agent 和 Collector 保持平台扩展点，但不在此文档中选择它们的运行时实现。

## 1. 结论

推荐**模块化 Go 单体 + PostgreSQL + 标准库 HTTP + 专门的配置加载器和数据库工具链**。不采用全栈框架，不在 P1 引入 Kafka、Redis、ORM、微服务编排或动态配置中心。

| 领域 | 推荐 | P1 是否引入 | 说明 |
| --- | --- | --- | --- |
| 编译与语言 | Go 1.25.x | 沿用 | 当前 `go.mod`：`go 1.25.0`；CI 固定 1.25.x 补丁线 |
| OpAMP 接入 | `github.com/open-telemetry/opamp-go v0.25.0` | 已有 | 保持官方 Server.Attach 和消息类型 |
| REST API | 标准库 `net/http` / `http.ServeMux` | 已有 | Go 1.25 的 Method/PathValue 路由足够 P1 |
| 控制端自身配置 | **`github.com/knadh/koanf/v2`** | 是 | 只启用 file/env + YAML parser，解码为强类型 Config 并校验 |
| 数据库 | **PostgreSQL** | 是 | Desired/Observed、关联唯一性、事务、审计 |
| 驱动/连接池 | **`github.com/jackc/pgx/v5/pgxpool`** | 是 | 原生 PG、事务、连接池、Context 和可观测性 |
| SQL 代码生成 | **`sqlc`，独立工具** | 是 | SQL-first、生成类型安全的 Queries，无运行时 ORM |
| Migration | **`github.com/jackc/tern/v2`，独立工具** | 是 | PostgreSQL 专用、原生 pgx、事务迁移；无需进应用 `go.mod` |
| 结构化日志 | `log/slog` + `slog.JSONHandler` | 已有 | 不使用 zap/logrus，日志层可由 Handler 替换 |
| Traces/Metrics | OpenTelemetry Go SDK + `otelhttp`；pgx tracing 可选 `otelpgx` | P1 基础指标/Trace | 业务过程使用手工 Span 和低基数指标 |
| 进程生命周期 | `context`、`signal.NotifyContext`、`errgroup` | 是 | 手动装配、可回收并发任务、优雅关停 |
| 单元测试 | `testing`、`httptest`、`go test -race` | 已有 | 标准库为主，领域模型和状态机可纯内存测试 |
| 集成测试 | `testcontainers-go/modules/postgres` | 是（测试依赖） | 真实 PostgreSQL 的唯一性、事务和重启测试 |
| 容器构建 | 多阶段 Dockerfile、最小化运行时镜像 | P1 后期 | 不在运行时镜像中包含 sqlc/tern 的生成工具 |
| 管理 UI | 暂不选型 | 否 | 当前只交付 Management API；未来视 Console 需求另选 |

> 版本说明：以下兼容版本已从对应 tag 的 `go.mod` 核验（见第 8 节），**不是本仓库已锁定依赖**。应用版本必须通过后续 PoC/PR 的 `go.mod`、`go.sum` 固定；独立 CLI 在 CI 和开发文档中固定版本。不可无条件使用 `@latest`。

## 2. 选型准则

1. **先满足产品不变量**：已认证 Agent 身份、持久 Desired State、明确 ACK 状态、重连恢复优先于框架便利性。
2. **版本可运行**：所有纳入编译链的库必须与当前 Go 1.25 工具链兼容；独立工具同样固定版本与工具链。
3. **尽量显式**：优先显式 SQL、依赖、生命周期、错误分支和 Config 类型；避免全局可变对象、自动注册和隐藏执行。
4. **便于验收**：用真实 Postgres + 原生 OpAMP 消息证明行为，不能只凭 Mock 或状态码宣布可靠。
5. **控制新增依赖**：每个依赖要有明确独立职责；只因未来可能扩展，不提前装 Redis/Kafka/ORM 或动态注入框架。
6. **产品域与协议分离**：`AgentKind`、`ConfigAdapter`、`Release` 属于我们的平台；OpAMP Protobuf 不能成为数据库领域模型。

这是当前项目的取舍建议，不是声称其他方案不成熟。

## 3. 配置加载：Koanf v2

### 3.1 备选方案

| 方案 | 优点 | 代价 | 判断 |
| --- | --- | --- | --- |
| `koanf/v2` | Providers/Parsers 解耦；YAML、Environment、默认值组合方便；按需引模块 | 必须自己做严格类型和语义校验，合并和 Environment key 需要规范 | **推荐** |
| `spf13/viper` | 社区常见，功能多、配置源丰富 | 对 P1 来说抽象和依赖范围较大，须特别注意全局状态与默认值/覆盖语义 | 不选 |
| `cleanenv` | 强类型环境变量映射非常简洁 | 配置源多级合并、CLI 覆盖和来源说明需要额外实现 | 轻量备选 |
| 仅 `os.Getenv` + 标准库 | 零第三方运行时依赖 | 字段不断增加后容易出现散落的默认值、空值歧义、重复解析 | P0 可用，P1 不建议继续扩散 |

**注意区别两套完全不同的配置**：

- **Control Plane 进程本地配置**：HTTP 监听、数据库 DSN、TLS、观测导出、轮询和限额；使用 Koanf，在服务启动时加载。
- **被管理 Agent 的 Remote Configuration**：Policy、ConfigRevision、Release、Assignment 和 AgentConfigMap；事实源在 PostgreSQL，不由 Koanf 文件加载器管理。

不能通过热改进程 YAML 偷偷变更所有 Agent 的 Desired State，也不应让 RemoteConfig 写入 Control Plane 的 TLS/数据库启动参数。

### 3.2 配置覆盖与校验语义

**建议优先级（低 → 高）**：内置 Defaults → 可选配置文件 `config.yaml` → 白名单环境变量；CLI 只提供 `--config`、`--check-config`、`--version` 等启动控制选项，若启用可覆盖对应明确定义的键。不要同时支持多个无解释的默认来源。

示意 `configs/example.yaml`（只包含非敏感字段；**未在仓库实现**）：

```yaml
server:
  listen_address: "127.0.0.1:4320"
  read_header_timeout: "5s"
  opamp_path: "/v1/opamp"

database:
  max_conns: 10
  connect_timeout: "5s"

reconcile:
  interval: "15s"

logging:
  level: "info"
  format: "json"
```

秘密字段通过 Secret 挂载或环境变量提供，例如 `APM_DATABASE_URL`、`APM_AGENT_TOKEN_PEPPER`，不能填入公共 YAML、Git、日志或 `--dump-config` 输出。环境变量到字段的映射建议维护**显式白名单**，不要机械地把每个下划线改成点（`listen_address` 这样的字段会产生歧义）。

拟议强类型模型：

```go
type Config struct {
    Server    ServerConfig    // listen address, timeouts, TLS
    Database  DatabaseConfig  // DSN from secret, pool settings
    Reconcile ReconcileConfig // interval, retry policy
    Logging   LoggingConfig   // level, format
}
func Load(path string) (Config, error) // merge + decode + validate; proposed API
```

加载器必须检测：未知键、空/非法值、Duration 格式、监听地址、TLS 开关与证书组合、数据库连接池边界、危险的 `sslmode=disable` 生产配置。Koanf 只负责来源合并和反序列化，验证应由独立 `Validate(Config) error` 完成。服务启动后 Config 应作为不可变快照注入模块，不引入隐式 `Watch` 热重载；将来确需热更新时必须列出允许动态变更的字段和原子替换语义。

## 4. PostgreSQL 持久化技术栈

### 4.1 pgx/v5 + pgxpool，而非 ORM

推荐 `pgx/v5`，当前检查到 `v5.11.0` 的 `go.mod` 最低 Go 为 1.25；具有原生 PostgreSQL API、连接池、事务、`LISTEN/NOTIFY` 和类型映射能力。[pgx 官方 README](https://github.com/jackc/pgx) 也建议专用 PostgreSQL 应用优先使用原生接口。

不选择 GORM/ent 的主要原因：本平台的关键操作依赖“预期版本 CAS + SQL 唯一性约束 + 原子发布事务 + 有条件更新 Observed”。这些语义以 SQL 明确表达更容易审查、做并发测试和分析慢查询；生成 ORM 模型不会降低这些业务复杂度。

**连接池建议**：通过 `pgxpool.Config` 设置 MaxConns、MinConns/MinIdleConns、连接健康和空闲/生命周期参数。不要直接依赖 `MaxConns=CPU 数` 的默认行为在高核心机器上无限放大总连接数。上述 YAML 的 10 仅是示例，不是容量基准或批准阈值。

Repository 不要泄漏 `pgx.Row` / `pgtype` 到 Policy Domain；可在 `internal/storage/postgres` 内完成转换。事务的生命周期由 Application Service 协调，不允许在持有写锁/事务时调用 WS Send。

### 4.2 SQL-first：sqlc

推荐 `sqlc v1.30.0` 作为**开发时代码生成工具**，它的源 `go.mod` 是 Go 1.23 + toolchain 1.24.1，可在当前 Go 1.25 线进行 PoC；较新的 `v1.31.1` 源 `go.mod` 要求 Go 1.26，暂不直接升级。

数据库结构的事实源是一系列 Migration；sqlc 从 Migration 目录解析 `Up` SQL，并与手写查询一起生成 Go 方法。文档确认它支持 `tern` 的 `---- create above / drop below ----` 格式。

示意 `sqlc.yaml`（下一阶段才引入）：

```yaml
version: "2"
sql:
  - engine: "postgresql"
    schema: "db/migrations"
    queries: "db/queries"
    gen:
      go:
        package: "pgqueries"
        out: "internal/storage/postgres/pgqueries"
        sql_package: "pgx/v5"
```

CI 应固定 `sqlc` 工具版本，执行 `sqlc generate` 后检查生成文件是否与 Git 一致。生成代码只用于 Adapter/Storage，不进入对外公共 API。对于 sqlc 内置解析器不能处理的复杂 PostgreSQL SQL，应增加真实数据库集成校验或按需要启用数据库分析，而非把生成成功当作所有 SQL 都已验证。

### 4.3 数据库迁移：Tern

**推荐 `github.com/jackc/tern/v2 v2.4.3`**。该版本的 `go.mod` 要求 Go 1.25；面向 PostgreSQL，支持独立 CLI/库、按版本升降、事务迁移，并与 pgx 同技术生态。

备选是 `pressly/goose/v3 v3.27.0`，它也适配 Go 1.25、支持多数据库和 SQL Migration。由于此服务只使用 PostgreSQL 且希望减少无关 Driver/Tooling，当前倾向 Tern。若团队已有标准 Goose 工具链，可在 ADR 评审时重新考虑。

**不要使用最新 goose v3.29.0 或 sqlc v1.31.1 强制推进 P1**：两者的当前 `go.mod` 都声明 Go 1.26，而仓库目前是 Go 1.25。相对地 Tern v2.4.3 和 sqlc v1.30.0 可作为 Go 1.25 候选版本；仍需端到端实际试验。

示意 SQL Migration：

```sql
CREATE TABLE agent_instances (
    tenant_id TEXT NOT NULL,
    instance_uid BYTEA NOT NULL,
    agent_kind TEXT NOT NULL,
    distribution TEXT NOT NULL,
    PRIMARY KEY (tenant_id, instance_uid),
    CHECK (octet_length(instance_uid) = 16)
);

---- create above / drop below ----

DROP TABLE agent_instances;
```

表名与列结构只是一项 P1 方案，**不是已批准的数据库 Schema**。迁移文件用固定宽度顺序编号（`001_initial.sql`、`002_add_observation.sql`），以兼容 sqlc 的字典序解析；团队并行开发时使用 Tern 的冲突重编号流程，审查每次 `Down` 的破坏性后果。

数据库升级建议作为**独立部署/运维步骤**运行（示意命令，需在 P1 PoC 验证环境变量和路径）：

```bash
go install github.com/jackc/tern/v2@v2.4.3
# 配置 PGHOST/PGPORT/PGUSER/PGDATABASE/PGPASSWORD 或 PGSERVICE
tern migrate --migrations db/migrations
```

应用服务启动时执行 Schema 兼容性检查；不要让每个 Control Plane 副本自动迁移数据库。正式环境推荐向前兼容的 expand/migrate/contract 发布顺序，避免运行中的旧版本查询被新 Schema 破坏。

## 5. HTTP、通信与管理接口

### 5.1 延续 net/http

当前 P0 使用 Go 标准库 `http.ServeMux`、Method Route、`PathValue` 和 `http.Server`。P1 的接口数量有限，迁入 Gin/Echo 不会解决身份/配置状态的一致性问题，反而引入新的 Context 与中间件生命周期。

| 方案 | 适用场景 | 当前判断 |
| --- | --- | --- |
| 标准库 `net/http` | 路由可控、API 数量中等、项目已有实现 | **沿用** |
| `go-chi/chi` | 大量嵌套路由、共享 Middleware 和复杂子 Router | 若实际增长再评估 |
| Gin / Echo | 团队已有统一框架标准或需要其生态 | 当前无必要 |

仍应为所有边界补充：认证 Middleware、请求体大小限制、JSON 严格解析、超时、可观测性和一致的错误响应。`http.Server` 需要 ReadHeaderTimeout、IdleTimeout 及针对 HTTP 与 WebSocket 的分别处理，不能只通过一个全局 WriteTimeout 解决 WS 长连接行为。

### 5.2 OpAMP 与业务模块边界

沿用 `opamp-go v0.25.0`，`OnConnecting` 完成凭证校验并绑定可信 Principal，`OnMessage` 转换 Agent 状态为 Domain Observation。业务模块**不直接依赖** `protobufs.AgentToServer` 或 `types.Connection`。

HTTP 请求结束不是 Agent 离线事件；WS 断链只更新该会话的连接可用性。需要真实的双传输测试，以防当前 P0 以 `Connected` 布尔值表示 HTTP 活跃造成误报。

Reconciler 初期依赖**数据库状态 + 进程内触发队列 + 周期性兜底扫描**，在内存事件丢失后仍可恢复 Desired Assignment。单连接 Push 有独立的串行 Sender；不借用 Kafka/Redis 作为“保证可靠”的替代品。未来分布式 Gateway 才评估连接所有权转发与一致性协调。

## 6. 生命周期、Telemetry、错误与安全

### 6.1 启动与依赖装配

选择 Go 标准库 `context`、`signal.NotifyContext`、`net/http`，需要并发启动/停止时使用 `golang.org/x/sync/errgroup`。不用 Fx/Wire 作为第一步强制依赖；`cmd/server` 中的显式构造能让工程师看清 DB、Registry、Gateway、Reconciler 的真实生命周期。

~~~mermaid
flowchart TD
    A["Load / Validate Config"] --> B["初始化 slog 与 OTel"]
    B --> C["连接 pgxpool / 检查 Schema"]
    C --> D["构造 Storage、Registry、Gateway"]
    D --> E["启动 HTTP Server + Reconciler"]
    E --> F["Ready"]
    F --> G["SIGTERM / graceful shutdown"]
    G --> H["停止发布、排空、关闭资源"]
~~~

DB/Schema 或必要认证材料不可用时不进入 Ready。`/healthz` 表示进程存活；`/readyz` 在目标实现中检查是否可安全提供管理服务，两者不能合并成一个“HTTP 200 就健康”。

### 6.2 日志与 OpenTelemetry

继续使用已有 `log/slog`；正式部署提供 JSON Handler、可注入 Logger、日志等级和稳定字段（`component`、`agent_kind`、`release_id` 等）。禁止输出 Token、DSN、RemoteConfig 原文中的密钥；Agent 自报错误字符串需要限长、过滤与脱敏。

对 HTTP 使用 `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`，数据库连接可评估 `github.com/exaring/otelpgx v0.13.0`（已确认该版本支持 Go 1.25 / pgx v5）。OpAMP WebSocket 的单条消息生命周期**不等于 HTTP Upgrade Span**，消息解码、队列等待、Config ACK 和重试需要自定义 Span/Metric。

初期指标建议围绕：已认证连接数、Agent 最近上报时延、待收敛 Assignment、配置应用延迟、失败类别、数据库查询/连接池状态及 Release 进度。`instance_uid`、`release_id` 不得作为常规指标 Label，以免高基数导致费用和性能失控；明细放 Traces/Logs/API。

OpenTelemetry Logs SDK 或将 `slog` 全部转换为 OTLP 日志当前不是关键路径，避免在 P1 引入额外实验性耦合。

### 6.3 认证、身份和权限

P1 必须有**认证主体（Principal）+ 授权范围**模型，但最终凭证形式应作为待审查的安全决策保留在 DD/ADR 中。技术库方面尽量使用 `crypto/rand`、`crypto/hmac`、`crypto/sha256`、`crypto/subtle` 和 TLS 标准库，而不是为共享开发 Token 专门引入 JWT 库。

推荐安全原则：Agent 使用足够高熵的独立 Token/可吊销凭证；持久化单向校验材料，不保存可还原 Token；可信 Principal 的 Tenant/Instance Scope 从服务端凭证记录推导；`service.name` 和 AgentKind 仅是申报信息，不能提升访问权限。证书管理、多租户 OIDC、企业 RBAC、Token 生命周期与 mTLS 仍需设计与安全验收。

当前 P0 的 `APM_OPAMP_TOKEN` 和 `APM_ADMIN_TOKEN` 属于共享演示 Token，**不是合格的生产 Agent Identity 方案**。

## 7. 测试与开发工具

推荐保持当前 `testing` / `httptest` / `go test -race`，而不是先引 Ginkgo/Gomega 或大型 Mock 框架。

- 单元测试：配置合并/缺省/非法值、Domain Selector、ConfigHash、错误分类、并发状态更新。
- PostgreSQL 集成测试：`testcontainers-go/modules/postgres` 启动固定版本的 PostgreSQL 镜像，测试唯一性、事务、CAS、Session 重连和服务进程重启；容器镜像在 CI 用 digest 固定。
- 协议测试：`opamp-go` 真实 HTTP/WS Client，测试 AgentDescription 压缩、Capabilities、不同的 ConfigStatus、旧回执及连接关闭。
- 安全测试：无凭证、撤销凭证、跨 Tenant、重复 UID、无效配置、错误文本过大等负向用例。
- CI：Go 单测与 Race Detector、`go vet`、`sqlc generate` 无漂移、`tern` Migration Up/Down/Up 和 PostgreSQL 集成测试、RF Harness/Specs 验证。

Testcontainers-Go `v0.44.0` 的源 `go.mod` 是 `go 1.25.0`，并声明 `toolchain go1.25.9`。因此 CI 应使用足够新的 Go 1.25 补丁版本并确保 Docker/镜像环境可用，不把“单测通过”误判为已执行需要 Docker 的测试。

### 7.1 推荐目录布局（目标，非当前实现）

```text
cmd/server/                   进程入口、启动装配
internal/
  appconfig/                 Koanf 配置与 Validate
  api/                       net/http 管理 API
  auth/                      Principal + Scope + middleware
  opamp/                     Server.Attach、协议转换与 Sender
  registry/                  Agent/Service/Session 领域对象
  policy/                    Sampling Policy（业务）
  configuration/             ConfigRevision/Adapter/Resolver（通用）
  release/                   Release/Rollout
  reconcile/                 Desired/Observed 协调
  storage/postgres/          Repository + Tx 管理
  storage/postgres/pgqueries/ sqlc 生成代码
db/
  migrations/                Tern 源 SQL（Schema 唯一事实源）
  queries/                   sqlc SQL 查询
configs/example.yaml         脱敏启动配置范例
sqlc.yaml                    SQL codegen 配置
tests/integration/           PostgreSQL 和 OpAMP 测试
```

`internal/appconfig` 属于**控制端进程加载**；`internal/configuration` 属于**对远程 Agent 的配置管理**。用两个命名避免 “config” 同时表示 YAML 本地配置和 OpAMP ConfigMap。

## 8. 版本兼容性核验

以下版本与最低 Go 要求来自 **2026-10-19 核对的官方 GitHub Release 与 tag `go.mod`**，不代表这些依赖在当前项目中均已完成相互兼容的编译测试。

| 组件 | 核验版本 | Tag `go.mod` 的 Go 要求 | 结论 |
| --- | --- | --- | --- |
| `opamp-go` | v0.25.0 | 1.25.0 | 当前已实际依赖 |
| `pgx/v5` | v5.11.0 | 1.25.0 | P1 候选 |
| `koanf/v2` | v2.4.1（2026-10-18 发布） | 1.23.0 | P1 候选；新补丁需回归 |
| `tern/v2` | v2.4.3（2026-08-23 发布） | 1.25.0 | 推荐 Migration 工具 |
| `sqlc` | v1.30.0 | 1.23.0 / toolchain 1.24.1 | 推荐固定 CLI 工具 |
| `goose/v3` | v3.27.0 | 1.25.0 | 兼容备选 |
| `goose/v3` | v3.28.0、v3.29.0 | 1.26.0 | 当前不使用 |
| `sqlc` | v1.31.0、v1.31.1 | 1.26.0 | 当前不使用 |
| `otelpgx` | v0.13.0 | 1.25.0 | P1 可选观测依赖 |
| `testcontainers-go` | v0.44.0 | 1.25.0 / toolchain 1.25.9 | 集成测试候选 |

**重要**：`sqlc` 和 `tern` 是独立 CLI，默认不加入运行时服务的 `go.mod`；代码生成输出可以提交 Git 以支持没有生成器的 Go Build。Koanf Providers/Parsers、otelhttp、otelpgx 和 Testcontainers 子模块都有独立的兼容/版本检查，必须在实际 P1 PoC 中执行 `go mod tidy`、`go test` 和真实数据库测试，不能仅凭核心模块的 `go` 指令宣布完整兼容。

## 9. 尚需实际验证和评审的决策

| 问题 | 初步倾向 | 决策或实测门槛 |
| --- | --- | --- |
| PostgreSQL 版本 | 本地/CI 优先使用团队可维护的受支持版本，如 PostgreSQL 17 | 选定部署环境、镜像 digest 与升级策略 |
| 认证模型 | TLS + 高熵 Scoped Token，未来可扩展 mTLS | 安全 ADR：Principal 绑定、吊销、重复 UID 和跨租户访问 |
| 多实例 Gateway | 首期单实例 | 压测和实际部署需求触发 Session Owner/Fencing 设计 |
| 配置热重载 | 启动 Config 不热重载；Agent RemoteConfig 经发布流程变更 | 配置变更安全审计与原子应用要求 |
| PostgreSQL Migration | Tern 优先，Goose 作为备选 | P1 PoC 使用真实 PG 验证 Up/Down、sqlc 代码生成 |
| 领域事务和隔离 | PostgreSQL 事务 + 唯一约束 + CAS，先不假定 SERIALIZABLE | 并发发布、旧 ACK 和重连竞争测试 |
| API 契约生成 | 先维护显式 REST 处理器，公开契约稳定后引入 OpenAPI 生成 | API 数量、版本管理及兼容要求 |
| 控制台 | 暂不选择前端框架 | UI 工作流及交互需求定义 |

这份文档记录的是**候选方案和推荐依据**；用户确认某项长期架构选择后，涉及持续治理的决定应通过 RepoFoundryAI `engineering-execution-plan` 起草 proposed ADR，并在 Decision Owner 明确授权后接受。不能通过合并本文自动把 DD-001 标记为 approved 或 EP-001 标记为 unblocked。

## 10. P1 最小技术验证计划

建议在 P1 代码开发前做一个集中 PoC，而不是逐个引依赖却没有边界验收。完成标准：

1. Koanf：文件缺失与非法键行为清晰，环境变量覆盖明确，敏感配置不出现在日志或 Dump，`Validate` 正确拒绝非法配置。
2. pgx/sqlc/Tern：真实 PostgreSQL 的 Migration Up、Down、重新 Up；`sqlc generate` 与编译一致；Agent UID 唯一性与 Tenant Scope 约束被数据库实际执行。
3. Persistence：按业务事务保存 Identity/Observation，重启 Control Plane 后状态可恢复，离线 Session 不被重放为有效连接。
4. HTTP/OpAMP：`net/http` 管理接口保留 P0 路由兼容；不同身份跨租户操作被拒绝；HTTP Polling 的状态不由 `OnConnectionClose` 误判。
5. Telemetry：HTTP/DB Traces 和低基数业务指标可在测试后端观察；WS 消息生命周期可单独定位错误；日志不包含秘密材料。
6. 验证：`go test -race ./...`、`go vet ./...`、PostgreSQL 集成测试和 RepoFoundryAI Validation 全部通过，有准确的 CI 记录与 Git revision。

只有通过这个 PoC，才将建议版本写入应用依赖，并按 RF 的 DD/ADR/EP 生命周期记录正式选择。

## 11. 参考来源

- [当前项目 go.mod](https://github.com/XiaoWeiKIN/apm-control-plane/blob/main/go.mod)、[P0 代码](https://github.com/XiaoWeiKIN/apm-control-plane/blob/main/cmd/server/main.go)
- [OpAMP Go Server](https://github.com/open-telemetry/opamp-go/tree/main/server)；[OpAMP 规范](https://github.com/open-telemetry/opamp-spec)
- [Go ServeMux](https://pkg.go.dev/net/http#ServeMux)、[Go slog](https://pkg.go.dev/log/slog)
- [Koanf v2](https://github.com/knadh/koanf)
- [pgx](https://github.com/jackc/pgx) 与 [pgxpool API](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool)
- [sqlc + pgx](https://docs.sqlc.dev/en/latest/guides/using-go-and-pgx.html)、[sqlc Migration 格式](https://docs.sqlc.dev/en/latest/howto/ddl.html)
- [Tern](https://github.com/jackc/tern)、[Goose](https://github.com/pressly/goose)
- [OpenTelemetry Go instrumentation](https://opentelemetry.io/docs/languages/go/libraries/)、[otelpgx](https://github.com/exaring/otelpgx)
- [Testcontainers-Go](https://github.com/testcontainers/testcontainers-go)、[PostgreSQL Module](https://golang.testcontainers.org/modules/postgres/)

**版本声明**：版本表记录 2026-10-19 观察结果，随后的上游 Release 可能改变“最新”版本；工程采用已验证的 pin 与锁定工具，而不是运行时自动跟随最新版。
