# APM Control Plane 技术选型（P1–P3）

> **状态**：技术方案 / 待评审（Proposed）。本文不是 Accepted ADR、已批准的 DD-001，也不代表数据库兼容性已经实现。
>
> **评审基线**：2026-10-19；`main` 已使用 Go 1.25、`opamp-go v0.25.0`、`net/http`、`log/slog`。相关 [开发者架构指南](README.md)、[DD-001](../design-docs/dd-001_opamp-control-plane.md) 与 [EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 需保持设计边界一致。
>
> **用户确认的新增约束**：Control Plane **必须具备多种关系型数据库驱动能力，可能使用 MySQL**。因此不能以 PostgreSQL 独占作为平台前提。**首批目标数据库为 PostgreSQL 和 MySQL**；两者通过实际测试前，不能宣称生产多数据库兼容。
>
> **不变的业务范围**：长期管理 Java、Go、Python 等 APM Agent，未来可能管理 Collector；首个可执行的 Agent Runtime 契约仍为 Java Agent。

## 1. 总体结论

选用**模块化 Go 单体 + `database/sql` + 分数据库 SQL Adapter + 多方言 Migration + 严格配置加载**。**一个部署实例只连接其配置指定的一种主数据库**，不是同时跨 MySQL、PostgreSQL 执行一个分布式事务。数据库类型在进程启动时确定；不支持运行时随意切换。

| 能力 | 推荐 | 阶段 | 取舍 |
| --- | --- | --- | --- |
| 语言与协议 | Go 1.25.x + `opamp-go v0.25.0` | 已有 | 延续 P0 |
| Management API | `net/http` / `ServeMux` | 已有 | 不引入 Gin/Echo |
| 应用配置 | `knadh/koanf/v2` | P1 | file + yaml + 显式 env 覆盖，强类型 Decode/Validate |
| 通用 DB 接口/连接池 | **`database/sql` / `sql.DB` / `sql.Tx`** | P1 | 标准接口，连接池与事务统一 |
| PostgreSQL 驱动 | **`jackc/pgx/v5/stdlib`** | P1 | 保留 pgx 生态驱动，但不通过 pgxpool 暴露到 Domain |
| MySQL 驱动 | **`go-sql-driver/mysql`** | P1 | 实现 `database/sql/driver`，连接池由 sql.DB 管理 |
| SQL 访问 | **`sqlc v1.30.0`**，2 份配置集 | P1 | 两种方言生成各自的 `database/sql` 查询代码 |
| Migration | **`pressly/goose/v3 v3.27.0`** | P1 | 同一工具，数据库特定 SQL Migration |
| 日志 | `log/slog` | 已有 | 结构化 JSON，不引入 Logrus/Zap |
| Telemetry | OTel Go SDK + `otelhttp` + Repository Span | P1 | 不将 pgx-only tracing 注入通用存储端口 |
| 进程生命周期 | `context` + `signal.NotifyContext` + `errgroup` | P1 | 手动装配，显式生命周期 |
| 单元测试 | `testing`、`httptest`、`go test -race` | 已有 | 纯领域测试 |
| 集成测试 | **Testcontainers PostgreSQL + MySQL** | P1 | 两种引擎执行相同 Repository 合规测试 |
| Redis/Kafka/ORM/DI 框架 | 不引入 | 非 P1 | 避免提前增加系统与一致性复杂度 |

**推荐将 `database/sql` 限制在 `internal/storage` 及其驱动 Adapter 内**。Agent Registry、Policy、Reconciler 只消费领域 Repository 接口；不可跨层持有 `*sql.DB`、`*pgx.Conn`、`*sql.Tx` 或 MySQL 驱动错误类型。

## 2. 多数据库支持的准确含义

目标能力是：同一版 Control Plane 二进制能够根据 **`database.driver` 配置选择 PostgreSQL 或 MySQL**，保持 Management API 和业务层行为语义一致。允许独立迁移和查询方言实现，**不承诺** SQL Schema、物理执行计划或故障响应的字节级一致。

~~~mermaid
flowchart TB
    App["Registry / Config / Release / Reconciler"]
    Port["Repository / UnitOfWork Port"]
    Factory["Storage Factory<br/>配置 driver: postgres | mysql"]
    PG["postgres Adapter<br/>sqlc generated queries"]
    MY["mysql Adapter<br/>sqlc generated queries"]
    SQL["Go database/sql"]
    PGDRV["pgx/v5/stdlib"]
    MYDRV["go-sql-driver/mysql"]
    P[("PostgreSQL")]
    M[("MySQL")]

    App --> Port
    Factory --> PG
    Factory --> MY
    PG -.->|"实现"| Port
    MY -.->|"实现"| Port
    PG --> SQL
    MY --> SQL
    SQL --> PGDRV --> P
    SQL --> MYDRV --> M
~~~

此图是目标结构，不是已存在于 P0 代码中的数据访问架构。**配置工厂只创建一个数据库实例**；图中两条路径表示备选部署方式，不代表每条业务请求访问两套数据库。

### 2.1 存储层必须保证的语义

数据库 Adapter 的一致性契约至少包含：

- Agent Identity 唯一性：在授权 Tenant 范围内唯一，UID 长度 16 字节；写入不存在的 Tenant 不会被授权模型默默接受。
- Desired Config 可恢复：Server/Agent 重启后，Assignment 的 revision 与 hash 仍存在。
- 配置发布原子性：一次发布事务中的 Policy/Config Revision、Release、Target Assignment 必须**一起提交或不提交**。
- 乐观并发与旧回执：Agent 状态写入使用期望版本条件（`UPDATE ... WHERE generation=?`），旧 ACK 不覆盖新的 Desired 状态。
- 幂等：同一操作重复请求返回等价的业务结果，不依赖 Driver 的自增 ID 或语法。
- 错误分类：唯一约束、超时、已取消、瞬时连接故障、序列化/死锁冲突映射为平台错误类别，保留原始 Driver 错误以供日志诊断。
- 时间与序列化：业务时间以 UTC 保存和解释；UID、Hash、JSON 与布尔状态有跨方言的明确编解码规则。

### 2.2 方言差异必须隔离，不能依赖字符串替换

| 问题 | PostgreSQL | MySQL | 领域层看到的语义 |
| --- | --- | --- | --- |
| SQL 参数 | `$1`、`$2` | `?` | SQL 由各自 sqlc 生成，不在业务逻辑里拼装 |
| UID 存储 | `BYTEA` | `BINARY(16)` | `[16]byte` / AgentKey |
| Hash 存储 | `BYTEA` | `BINARY(32)` 或定长二进制 | 固定哈希字节 |
| JSON | `JSONB` 或 TEXT | `JSON` 或 TEXT | 配置规范字节序列由应用层持有，不能依赖 DB JSON 规范化 |
| Upsert | `ON CONFLICT` | `ON DUPLICATE KEY UPDATE` | `SaveOrUpdateAgent` |
| 插入返回值 | `RETURNING` | `LastInsertId` 或显式 UUID/业务 ID | 领域不依赖方言产生的 ID |
| 时间存储 | `TIMESTAMPTZ` | `DATETIME(6)` 配 UTC 会话/Driver | `time.Time`（UTC） |
| 并发控制 | 行锁/CAS/事务 | InnoDB 行锁/CAS/事务 | 同一 Assignment 的版本号 CAS |
| Schema 迁移 | 部分 DDL 可事务化 | 多种 DDL 会隐式提交 | 独立迁移和恢复策略，不伪装相同 |

**不要把 MySQL、PostgreSQL 的生产兼容性缩减为占位符从 `$1` 转成 `?`**。JSON 比较、默认时间精度、排序/字符集、锁等待、受影响行计数以及 Migration 失败场景都需要有合同测试。

MySQL 使用 **InnoDB**、`utf8mb4` 与明确的 Collation；必须识别 DDL 隐式提交（[MySQL 官方说明](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html)）。即便使用 Goose，MySQL Migration 中的 `CREATE/ALTER TABLE` 也不自动获得完整的事务回滚能力。不得将 Goose 的默认事务选项当作跨数据库原子 DDL 保证。

### 2.3 数据库抽象应该多厚？

不建议为了多数据库引入“万能 SQL Builder”或在领域层维护一个 `Dialect` 对象，不建议把所有 CRUD 都包装为通用动态查询 DSL。

真正稳定的边界是**业务语义接口**，例如：

```go
// 设计草图：不是当前仓库已有代码。
type AgentRepository interface {
    UpsertIdentity(ctx context.Context, agent AgentIdentity) error
    Observe(ctx context.Context, obs AgentObservation) error
    Get(ctx context.Context, key AgentKey) (AgentSnapshot, error)
}

type AssignmentRepository interface {
    GetDesired(ctx context.Context, key AgentKey) (Assignment, error)
    CompareAndSetObserved(
        ctx context.Context,
        key AgentKey,
        expectedGeneration int64,
        obs ConfigObservation,
    ) (updated bool, err error)
}

type UnitOfWork interface {
    WithinTx(ctx context.Context, fn func(Repos) error) error
}

type Repos interface {
    Agents() AgentRepository
    Assignments() AssignmentRepository
    Releases() ReleaseRepository
}
```

`UnitOfWork` 必须保证回调中的所有 Repository 使用**同一个数据库事务**；不能让每个 Repository 偷偷使用自己的 `*sql.DB`，也不能把已经关闭的 `*sql.Tx` 留在全局变量中。Go API 类型名属于推荐草图，实际签名将在 PoC 和 DD 审查后确定。

## 3. 配置加载：Koanf v2

配置从**默认值 → YAML → 显式白名单环境变量**覆盖，解析为强类型 `AppConfig`，最后统一验证。优先建议 Koanf v2，而非因为多数据库就采用更重的依赖注入配置框架。

**严格区分两套配置体系**：

- **Control Plane 启动配置**：监听、TLS、认证、Database Driver/DSN/Pool、观测/重试；由 Koanf 加载，默认启动后不可变。
- **Agent RemoteConfig**：采样策略、不可变 ConfigRevision、Release/Assignment 与 OpAMP ConfigMap；由数据库作为事实源，经 Release 和 Reconciler 分发。Koanf 不参与这些业务状态变更。

示例（只说明结构，不是可直接运行的现有 `config.yaml`）：

```yaml
server:
  listen_address: "127.0.0.1:4320"
  read_header_timeout: "5s"

database:
  driver: "postgres"      # postgres | mysql
  dsn_env: "APM_DATABASE_DSN"
  max_open_conns: 10
  max_idle_conns: 5
  conn_max_lifetime: "30m"
  connect_timeout: "5s"

reconcile:
  interval: "15s"

logging:
  level: "info"
  format: "json"
```

`APM_DATABASE_DSN` 由 Secret 或部署环境提供，不能进入 Git 或日志。例如 PostgreSQL DSN 通常是 URI；MySQL 驱动使用其定义的 DSN 格式，不能把 URI 强行传给 MySQL。启动时只允许已声明的 `postgres`、`mysql`，其它值直接拒绝。

MySQL 连接应由 Driver 配置明确 `parseTime=true`、`loc=UTC`、`tls` 和允许的字符集；生产环境必须验证 TLS，而不是因默认方便关闭证书验证。PostgreSQL 同样要审查 `sslmode`。

强类型配置草图：

```go
type DatabaseConfig struct {
    Driver          string // postgres | mysql
    DSN             string // injected from trusted secret; never log
    MaxOpenConns    int
    MaxIdleConns    int
    ConnMaxLifetime time.Duration
}

type AppConfig struct {
    Server   ServerConfig
    Database DatabaseConfig
    Logging  LoggingConfig
}

func LoadConfig(path string) (AppConfig, error) // proposed
func ValidateConfig(cfg AppConfig) error       // proposed
```

Koanf 负责 Providers/Parsers 与覆盖，**不代替业务验证**。验证必须拒绝未知键、空秘密、非法 Duration、非白名单 Driver、没有 TLS 的生产连接等，不从环境变量下划线推断任意深层键。避免全局 `var K = koanf.New(...)` 作为运行时动态 Config。

## 4. Driver、SQL 生成和 Migration 的具体选型

### 4.1 Driver：标准 database/sql + 两个实现

| 候选 | 用途 | 选择 |
| --- | --- | --- |
| `database/sql`（Go 标准库） | DB/Tx/连接池/Context 接口 | **统一入口** |
| `github.com/jackc/pgx/v5/stdlib` | PostgreSQL Driver（`sql.Open("pgx", dsn)`） | **PG Adapter 使用** |
| `github.com/go-sql-driver/mysql` | MySQL Driver（`sql.Open("mysql", dsn)`） | **MySQL Adapter 使用** |
| `pgxpool` | PostgreSQL 原生高性能连接池 | 不作为公共 Port；非首期必要 |
| `sqlx` | `database/sql` 扩展工具 | 当前不引入；与 sqlc 职责重叠 |
| GORM/ent | ORM/Schema Model | 当前不引入：SQL 方言和并发语义仍须显式验证 |

Go `database/sql` 自带连接池：统一使用 `SetMaxOpenConns`、`SetMaxIdleConns`、`SetConnMaxLifetime`，`PingContext` 校验连接。注意 `sql.Open` 本身通常并不立即检查实际网络连接。**不同数据库驱动的连接参数格式不同**，Factory 按 Driver ID 显式分支，不能通过任意字符串 `sql.Open(cfg.Driver,cfg.DSN)` 允许未知实现静默进入。

建议可执行的骨架方式（接口和包名为示例，不是已提交实现）：

```go
import (
    "database/sql"
    _ "github.com/jackc/pgx/v5/stdlib"
    _ "github.com/go-sql-driver/mysql"
)

func OpenDB(driver, dsn string) (*sql.DB, error) {
    var sqlDriver string
    switch driver {
    case "postgres":
        sqlDriver = "pgx"
    case "mysql":
        sqlDriver = "mysql"
    default:
        return nil, fmt.Errorf("unsupported database driver: %s", driver)
    }
    return sql.Open(sqlDriver, dsn)
}
```

真实实现必须在建池后设置上限、完成 `PingContext` 与健康检查、保证关闭、处理 MySQL DSN 与 TLS。**以上仅显示 Driver 选择机制**，不应直接复制后当生产启动器。

### 4.2 sqlc：两个方言、两个代码生成输出

`sqlc v1.30.0` 支持 Go 下 PostgreSQL、MySQL，推荐**生成两套 Repository 内部实现**，不将生成的 `DBTX` 或实体直接作为领域层 Port。

建议 `sqlc.yaml`（未运行，PoC 时需要真实验证）：

```yaml
version: "2"
sql:
  - engine: "postgresql"
    schema: "db/migrations/postgres"
    queries: "db/queries/postgres"
    gen:
      go:
        package: "pgqueries"
        out: "internal/storage/postgres/pgqueries"
        sql_package: "database/sql"
  - engine: "mysql"
    schema: "db/migrations/mysql"
    queries: "db/queries/mysql"
    gen:
      go:
        package: "mysqlqueries"
        out: "internal/storage/mysql/mysqlqueries"
        sql_package: "database/sql"
```

Migration 与 Query 分方言维护是**明确的技术成本**，但实现契约清晰；共通逻辑可以存在应用层和单独的合同测试中，而不是假设所有 SQL 字面量可通用。

### 4.3 Migration：从 Tern 调整为 Goose

**不再推荐 PostgreSQL 专用的 `tern/v2` 作为基础 Migration 工具。** 改为 `pressly/goose/v3 v3.27.0`，因为它能以同一 CLI 操作 PostgreSQL/MySQL。v3.27.0 的 `go.mod` 要求 Go 1.25，符合项目当前基线；后续 v3.28+ 已要求 Go 1.26，暂不采用。

两套独立 Migration 目录中的**业务版本号对应，但文件 SQL 不必一致**：

```text
db/
  migrations/
    postgres/
      00001_initial.sql
      00002_agent_observations.sql
    mysql/
      00001_initial.sql
      00002_agent_observations.sql
  queries/
    postgres/
      agents.sql
      assignments.sql
    mysql/
      agents.sql
      assignments.sql
```

例如 PostgreSQL UID 用 `BYTEA`，MySQL 用 `BINARY(16)`；JSON 存储、Timestamp、索引、唯一约束和 Upsert 可分别设计。两个 Migration 目录均使用 Goose 注解：

```sql
-- +goose Up
-- 方言专用 CREATE TABLE ...

-- +goose Down
-- 方言专用 DROP TABLE ...
```

**MySQL DDL 隐式提交**：对含 DDL 的迁移，需要按实际 MySQL/InnoDB 行为设计。Goose 支持 `NO TRANSACTION` 标记，但不能由此获得自动回滚；必须具备启动前备份/演练、迁移失败检测、人工或幂等前向修复流程。不能机械地在生产执行所有 `Down`。生产迁移采用 expand → backfill → contract，服务启动前检查 schema 版本兼容，不允许所有控制平面副本自行升级表结构。

示意独立 CLI（验证前不能视为已执行命令）：

```bash
go install github.com/pressly/goose/v3/cmd/goose@v3.27.0

goose -dir db/migrations/postgres postgres "$POSTGRES_DSN" up
goose -dir db/migrations/mysql mysql "$MYSQL_DSN" up
```

项目实际部署只运行**所选目标 Driver 对应**的 Migration 命令，不会对两个数据库同时执行；CI 要同时检查两种方言。

## 5. 服务、协议和模块装配：保留现状路线

| 领域 | 继续采用 | 原因 |
| --- | --- | --- |
| HTTP Router | 标准 `net/http` / `ServeMux` | P0 已有，业务路由数量不多 |
| OpAMP Gateway | `opamp-go v0.25.0` / `Server.Attach` | 官方协议实现，与 DB Driver 无关 |
| 日志 | `log/slog` | 结构化日志即可满足本期要求 |
| 进程生命周期 | `context`、`signal.NotifyContext`、必要的 `errgroup` | 可见的启动/排空/Shutdown 顺序 |
| 依赖装配 | `cmd/server` 显式构造 | 不引入 Fx/Wire |
| 状态协调 | DB Desired/Observed + 内存触发 + 周期扫描 | 不提前引 Redis/Kafka |
| 配置生成 | AgentKind ConfigAdapter | Java Sampling 为首个 Adapter，其它不在 P1 实现 |

存储选择**不应渗入** OpAMP Gateway、Auth Principal、Release、Policy 或 Config Adapter。数据库故障只影响持久化管理能力，不能把 JVM 侧 OTLP Telemetry 数据面重新绑定到控制平面。

### 5.1 并发及事务的跨方言要求

- 发布操作：`UnitOfWork.WithinTx` 保存 immutable PolicyRevision、ConfigRelease、Assignments。事务提交后触发 Reconcile，不能在 Tx 中发送 WS 消息。
- 幂等：推荐应用生成 UUID/业务 ID，而不是依赖 `RETURNING`、MySQL 自增值或 LastInsertId 作为跨数据库权威身份。
- 避免在公共 Repository 接口中暴露 PG Advisory Lock、`SKIP LOCKED` 等方言优化。若未来需要多个 Gateway 抢占任务，设计一个独立的 Lease/Claim 接口并为两库分别实现合同。
- MySQL 的默认事务隔离级别与 PostgreSQL 可能不同；不能假设代码逻辑相同就不存在幻读/写偏斜。发布 CAS、唯一性冲突和死锁重试必须测试。
- 操作失败必须分类：`Conflict`、`NotFound`、`Unavailable`、`Timeout`、`InvalidInput` 等业务错误由 Adapter 转换，不允许 MySQL 错误码泄漏成 Management API 的公共契约。

## 6. 可观测性和安全

HTTP 入口使用 OpenTelemetry Go SDK 与 `otelhttp`，数据库操作初期在 **Repository 边界**打手工 Span/低基数指标，使 PostgreSQL 与 MySQL 的观测行为可比较。`otelpgx` 可在特定 PostgreSQL 优化期引入，但**不能将仅 PG 可用的 Driver 插桩当成唯一数据访问 Telemetry**。

禁用把 AgentUID/ReleaseID 当做 Metrics Label 的默认做法。Token、数据库 DSN、SQL 参数中的秘密、RemoteConfig 正文敏感内容不得进入日志。Agent 的宣告属性只能作为数据，不可决定 Tenant 授权；Principal 绑定机制仍在 DD-001/ADR 中审查。

进程启动时：Load/Validate AppConfig → 创建 Logger/Telemetry → 通过 Storage Factory 打开所选 Driver/Pool → PingContext/Schema 兼容性验证 → 构造 Repository + Registry + OpAMP Gateway + Reconciler → Ready。DB 不可用时不能返回管理服务已经 Ready。数据库连接池指标可统一由 `sql.DBStats` 导出，但不同引擎的 Query 语义和延迟应分开观测。

## 7. 开发工具与双数据库验证

| 维度 | PostgreSQL | MySQL | 最低验收 |
| --- | --- | --- | --- |
| SQL Generation | `sqlc v1.30.0` PG engine | `sqlc v1.30.0` MySQL engine | 两套生成代码编译，无漂移 |
| Migration | Goose PG 目录 | Goose MySQL 目录 | 两库 Up、状态检查、受控失败及恢复 |
| Testcontainers | `modules/postgres` | `modules/mysql` | 固定测试镜像版本与必要的网络权限 |
| 连接生命周期 | `pgx/v5/stdlib` | `go-sql-driver/mysql` | Ping、取消、连接断开、超时和 Pool 关闭 |
| 约束 | UID/Scope 唯一性、Foreign Key | 同等业务约束，InnoDB | 两库跑同一语义测试 |
| 事务 | 创建 Release + Assignments 原子性 | 同等业务原子性 | 提交失败无部分发布 |
| CAS 与 ACK | revision 条件更新 | revision 条件更新 | 旧 ACK 不覆盖新 Desired |
| 恢复 | 重启重新拉起未收敛 Assignment | 相同 | 不将旧 Session 当在线 |
| 授权 | Tenant Principal 范围检查 | 相同 | 越权操作被拒绝 |

建议用 `testcontainers-go v0.44.0` 加 `modules/postgres` 和 `modules/mysql`；两者源码的 `go.mod` 为 Go 1.25，toolchain 提示是 Go 1.25.9，CI 应明确使用兼容补丁版本。Docker 不可用应在测试报告中标注**跳过**而非虚报通过。

**P1 若宣称多数据库支持，验收必须包含 PostgreSQL 和 MySQL 两套真实引擎的相同 Repository Contract Suite。** 可以先用 PostgreSQL 实现第一个 Adapter，再加入 MySQL 实现，但直到两套测试都通过之前，文档/API 不得宣称 MySQL 已可用。

### 7.1 目标目录树（尚未实现）

```text
cmd/server/
internal/
  appconfig/                     Koanf + typed config + Validate
  api/                           net/http Management API
  auth/                          Principal + Tenant Scope
  opamp/                         Official OpAMP server adapter
  registry/                      Agent inventory and observations
  policy/                        Sampling policy domain
  configuration/                 ConfigRevision + Config Adapter
  release/                       Release / Rollout
  reconcile/                     Desired/Observed convergence
  storage/
    repository.go                Domain ports / UnitOfWork contracts
    factory.go                   Driver allowlist and selection
    postgres/
      adapter.go
      pgqueries/                 sqlc-generated PostgreSQL code
    mysql/
      adapter.go
      mysqlqueries/              sqlc-generated MySQL code
db/
  migrations/
    postgres/
    mysql/
  queries/
    postgres/
    mysql/
sqlc.yaml
configs/example.yaml
tests/
  integration/
    postgres/
    mysql/
```

`internal/appconfig` 管理进程本地 YAML；`internal/configuration` 管理远程 Agent 配置。数据库连接池只被 `internal/storage` 装配和关闭，领域和 OpAMP 协议模块不感知运行时选用哪一种数据库。

## 8. 版本兼容性（2026-10-19 官方 Tag 核对）

以下是**候选版本与其源码 `go.mod` 的最低 Go 要求**；尚未经过该项目完整依赖树和双数据库 PoC 验证。独立 CLI 版本应固定于 CI；不要无条件引用最新版本。

| 组件 | 版本 | `go.mod` 的 Go 要求 | 处理 |
| --- | --- | --- | --- |
| `opamp-go` | v0.25.0 | 1.25.0 | P0 当前已引入 |
| `koanf/v2` | v2.4.1 | 1.23.0 | 配置加载候选 |
| `jackc/pgx/v5` | v5.11.0 | 1.25.0 | 通过 `stdlib` 驱动 PG |
| `go-sql-driver/mysql` | v1.10.1 | 1.24.0 | MySQL Driver 候选 |
| `sqlc` | v1.30.0 | 1.23.0 + toolchain 1.24.1 | 独立工具候选 |
| `goose/v3` | v3.27.0 | 1.25.0 | 多方言 Migration 工具候选 |
| `testcontainers-go` | v0.44.0 | 1.25.0 + toolchain 1.25.9 | 双数据库集成测试候选 |
| `goose/v3` | v3.28.0 / v3.29.0 | 1.26.0 | 当前 Go 1.25 不直接采用 |
| `sqlc` | v1.31.0 / v1.31.1 | 1.26.0 | 当前 Go 1.25 不直接采用 |
| `tern/v2` | v2.4.3 | 1.25.0 | PG 专用，已不满足多方言 Migration 诉求 |

`sqlc` 与 `goose` 可以作为独立 CLI 工具安装，默认不需要链接进 Control Plane 服务二进制。Go Driver 引入会改变应用依赖树和最终镜像体积，具体版本锁要在 P1 验证后落入 `go.mod`、`go.sum`。

## 9. 仍需审查的设计选择

| 待决定的问题 | 推荐基线 | 需要的证据 |
| --- | --- | --- |
| 第一批目标数据库 | PostgreSQL 与 MySQL；每个部署选一种 | 两库合同测试 |
| MySQL 具体兼容范围 | MySQL 8.0+ / 推荐先验证维护中的 LTS 版本 | 测试引擎版本、SQL Mode、InnoDB |
| 事务隔离等级 | 不强行统一 Isolation Level；在 Adapter 实现相同 CAS 语义 | 并发发布/死锁/旧 ACK 测试 |
| Schema 迁移策略 | Goose 分目录；MySQL DDL 单独恢复手册 | Up/受控 Down/故障注入 |
| 配置加载 | Koanf + Env 允许列表 | 覆盖、验证、Secrets 安全测试 |
| 新 Driver 接口 | 独立 Storage Adapter + Contract Suite | 新增第三种数据库不修改业务服务 |
| 主数据库迁移 | **不支持在线跨引擎迁移**，须新项目单独设计 | 数据导出、停写/回填/一致性检查 |
| 身份认证 | Scoped Principal + TLS，机制待安全 ADR | 越权、撤销、重复 UID 验证 |
| HA | 暂不引入 Redis/Kafka | 流量与部署实测 |

**多驱动≠跨引擎在线迁移**：从 PostgreSQL 切到 MySQL 需要专门的数据迁移、停写/回填、校验与回退方案；不能通过把 `database.driver` 从 postgres 改成 mysql 就让已有数据自动移动。

## 10. P1 最小技术验证计划

1. **配置加载**：Koanf 读取 `driver=postgres/mysql`；非法 Driver 或缺少 DSN 必须在启动前报错；进程日志不打印凭证。
2. **代码生成**：固定 `sqlc v1.30.0`，生成 PG/MySQL 各自的 `database/sql` 查询代码，Go 编译与生成文件一致；PostgreSQL 方言不能流入 MySQL 包。
3. **Migration**：Goose v3.27.0 分别运行两套 Migration；测试 MySQL DDL 失败后可诊断和恢复，不虚构事务性回滚。
4. **Repository 契约**：同一测试套件在 PostgreSQL 和 MySQL 中验证 Agent Identity 唯一性、Tenant 范围、Upsert、Observation、当前 Desired、CAS、过期 ACK、事务原子发布和重启恢复。
5. **OpAMP 集成**：两种 DB Driver 下，OpAMP Agent 的状态都能正确持久化；旧 P0 REST/协议行为保持兼容。
6. **验收记录**：`go test -race ./...`、`go vet ./...`、RF Validate、数据库容器测试结果、实际 Git SHA 和 CI URL 均可复核。

本 PR 只更新技术选择和设计约束，**不实现两个 Driver，也不修改已批准制品**。真正达到 P1 验收需要后续 PoC/实现 PR 以及符合 RF 生命周期的验证。

## 11. 官方资料

- [Go `database/sql`](https://pkg.go.dev/database/sql)；[pgx stdlib](https://pkg.go.dev/github.com/jackc/pgx/v5/stdlib)
- [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql)
- [sqlc v1.30.0 语言/数据库支持](https://docs.sqlc.dev/en/v1.30.0/reference/language-support.html)；[sqlc 配置](https://docs.sqlc.dev/en/v1.30.0/reference/config.html)
- [Goose 文档](https://pressly.github.io/goose/)；[Goose SQL 注解](https://pressly.github.io/goose/documentation/annotations/)
- [MySQL 隐式提交说明](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html)
- [Koanf v2](https://github.com/knadh/koanf)
- [Testcontainers-Go PostgreSQL](https://golang.testcontainers.org/modules/postgres/)；[MySQL](https://golang.testcontainers.org/modules/mysql/)
- [当前项目 `go.mod`](https://github.com/XiaoWeiKIN/apm-control-plane/blob/main/go.mod)
