# APM Control Plane 技术选型（P1–P3）

> **状态**：技术方案 / 待评审（Proposed）。本文不是 Accepted ADR、已批准的 DD-001，也不代表数据库兼容性已经实现。
>
> **评审基线**：2026-10-19；`main` 已使用 Go 1.25、`opamp-go v0.25.0`、`net/http`、`log/slog`。相关 [开发者架构指南](README.md)、[DD-001](../design-docs/dd-001_opamp-control-plane.md) 与 [EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 需保持设计边界一致。
>
> **用户确认的新增约束**：Control Plane **必须具备多种关系型数据库驱动能力，可能使用 MySQL**。因此不能以 PostgreSQL 独占作为平台前提。**首批目标数据库为 PostgreSQL 和 MySQL**；两者通过实际测试前，不能宣称生产多数据库兼容。
>
> **不变的业务范围**：长期管理 Java、Go、Python 等 APM Agent，未来可能管理 Collector；首个可执行的 Agent Runtime 契约仍为 Java Agent。

## 研究证据与适用边界

本次选型有正式 RF 研究输入：[R-001 开源架构比较](../research/active/r-001_opamp-oss-architecture/RESEARCH.md)、[源码对照报告](../research/active/r-001_opamp-oss-architecture/notes/opensource-comparison.md) 和 [Synthesis](../research/active/r-001_opamp-oss-architecture/SYNTHESIS.md)。Gitea、Grafana、Casdoor 的核对源码主要采用 XORM，**不是 GORM**。选择 GORM 是基于本项目 CRUD 简单、多数据库目标和维护成本的独立技术判断，仍须 P1 双数据库 PoC 验证。

## 1. 总体结论

选用**模块化 Go 单体 + GORM v2 + Goose + Koanf v2**。数据库访问以统一 GORM Repository 为主，只有确实存在方言差异时才写专用 SQL；不再采用双 sqlc 查询代码生成方案。**一个部署实例只连接其配置指定的一种主数据库**，不是同时跨 MySQL、PostgreSQL 执行一个分布式事务。数据库类型在进程启动时确定；不支持运行时随意切换。

| 能力 | 推荐 | 阶段 | 取舍 |
| --- | --- | --- | --- |
| 语言与协议 | Go 1.25.x + `opamp-go v0.25.0` | 已有 | 延续 P0 |
| Management API | `net/http` / `ServeMux` | 已有 | 不引入 Gin/Echo |
| 应用配置 | `knadh/koanf/v2` | P1 | file + yaml + 显式 env 覆盖，强类型 Decode/Validate |
| 通用 DB 接口/连接池 | **`database/sql` / `sql.DB` / `sql.Tx`** | P1 | 标准接口，连接池与事务统一 |
| PostgreSQL 驱动 | **`gorm.io/driver/postgres`** | P1 | GORM PostgreSQL Dialector，底层 pgx |
| MySQL 驱动 | **`gorm.io/driver/mysql`** | P1 | GORM Dialector，底层复用 go-sql-driver/mysql |
| ORM / SQL 访问 | **GORM v2 (`gorm.io/gorm`)** | P1 | 一套 CRUD Repository；CAS、事务和复杂查询显式处理 |
| Migration | **`pressly/goose/v3 v3.27.0`** | P1 | 同一工具，数据库特定 SQL Migration |
| 日志 | `log/slog` | 已有 | 结构化 JSON，不引入 Logrus/Zap |
| Telemetry | OTel Go SDK + `otelhttp` + Repository Span | P1 | 不将 pgx-only tracing 注入通用存储端口 |
| 进程生命周期 | `context` + `signal.NotifyContext` + `errgroup` | P1 | 手动装配，显式生命周期 |
| 单元测试 | `testing`、`httptest`、`go test -race` | 已有 | 纯领域测试 |
| 集成测试 | **Testcontainers PostgreSQL + MySQL** | P1 | 两种引擎执行相同 Repository 合规测试 |
| Redis/Kafka/ORM/DI 框架 | 不引入 | 非 P1 | 避免提前增加系统与一致性复杂度 |

**推荐将 GORM 限制在 `internal/storage/gormrepo` 内**，领域只依赖业务 Repository/UnitOfWork 接口。Agent Registry、Policy、Reconciler 只消费领域 Repository 接口；不可跨层持有 `*sql.DB`、`*pgx.Conn`、`*sql.Tx` 或 MySQL 驱动错误类型。

## 2. 多数据库支持的准确含义

目标能力是：同一版 Control Plane 二进制能够根据 **`database.driver` 配置选择 PostgreSQL 或 MySQL**，保持 Management API 和业务层行为语义一致。允许独立迁移和查询方言实现，**不承诺** SQL Schema、物理执行计划或故障响应的字节级一致。

~~~mermaid
flowchart TB
    App["Registry / Config / Release / Reconciler"]
    Port["Repository / UnitOfWork Port"]
    Factory["Storage Factory<br/>配置 driver: postgres | mysql"]
    PG["GORM PostgreSQL Dialector"]
    MY["GORM MySQL Dialector"]
    SQL["Go database/sql"]
    PGDRV["gorm.io/driver/postgres"]
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
| SQL 参数 | `$1`、`$2` | `?` | 由 GORM Dialector 处理；少量手写 SQL 必须按方言验证 |
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

### 4.1 GORM v2：统一 Repository

选用 `gorm.io/gorm` + `gorm.io/driver/postgres` + `gorm.io/driver/mysql`。GORM 通过 `database/sql` 管理连接池；Domain 不直接依赖 ORM 类型。P1 使用**一套 GORM Repository**，避免为简单 CRUD 重复编写 PostgreSQL/MySQL 查询代码。

```go
// 示例，具体版本和依赖在 P1 PoC 中锁定。
switch cfg.Database.Driver {
case "postgres":
    db, err = gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{})
case "mysql":
    db, err = gorm.Open(mysql.Open(cfg.Database.DSN), &gorm.Config{})
default:
    return nil, ErrUnsupportedDriver
}
```

事务由 `db.Transaction` 管理，事务回调内所有 Repository 必须共享同一个 `*gorm.DB` 事务句柄。Agent UID 唯一性、发布原子性和 Assignment CAS 仍需显式约束及 `RowsAffected` 检查。GORM 的 `Save` 不等于乐观并发控制。仅在实际方言差异出现时引入局部 SQL，不预建两套完整 Repository。

### 4.2 Migration：Goose

**不使用 GORM AutoMigrate 管理生产 Schema。** 采用 `pressly/goose/v3 v3.27.0`（Go 1.25 候选）统一迁移版本和执行入口。PostgreSQL/MySQL 的 DDL 允许按数据库分别维护：

```text
db/migrations/
  postgres/
    00001_initial.sql
  mysql/
    00001_initial.sql
```

两种数据库的逻辑模型应等价，但列类型、索引和 DDL 不强制相同。MySQL DDL 可能隐式提交，Goose 不会使其自动具备 PostgreSQL 式事务回滚能力。应用启动只检查 Schema 版本，不让多个实例自动执行迁移。

### 4.3 不采用 sqlc 的原因

当前存储主要是 Inventory、Policy、Configuration、Release 和 Audit 的 CRUD 与简单条件更新。使用 sqlc 需要维护 PostgreSQL/MySQL 两套 SQL 查询及生成代码，投入与当前复杂度不匹配。若未来出现大量复杂查询或性能热点，再以真实 Profiling 和 SQL 证据评估局部引入，而不是提前构建双方言查询体系。

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

## 7. 测试与目标工程结构

单元测试保持标准库 `testing`、`httptest` 和 `go test -race`。集成测试使用 Testcontainers-Go 的 PostgreSQL/MySQL 模块，运行同一套 Repository 业务合同：Agent UID 唯一性、CRUD、事务原子性、CAS、旧 ACK、租户授权、重启恢复及连接池关闭。Goose 分数据库目录执行 Migration，并验证 MySQL DDL 失败后的恢复流程。

**不使用 sqlc**，不生成双份 SQL Query 文件。目标结构如下（尚未实现）：

```text
cmd/server/
internal/
  appconfig/                Koanf + Validate
  api/                      net/http
  auth/                     Principal + Scope
  opamp/                    官方 OpAMP Gateway
  registry/                 Agent Inventory
  policy/                   Sampling Policy
  configuration/            ConfigRevision / Agent Adapter
  release/                  ConfigRelease / Rollout
  reconcile/                Desired/Observed
  storage/
    repository.go           Domain Repository / UnitOfWork 接口
    gormrepo/
      models.go             GORM 模型
      agents.go             Agent CRUD
      assignments.go        CAS 与状态更新
      releases.go           事务发布
      errors.go             错误归一化
db/migrations/
  postgres/
  mysql/
configs/example.yaml
tests/integration/
```

GORM 的实体模型不直接作为公开 API；Domain 不依赖 ORM 结构体。生产 Schema 变更通过 Goose，禁止在服务启动时调用 AutoMigrate。

## 8. 版本兼容性核验

项目目前固定 Go 1.25。GORM v2、PostgreSQL/MySQL Dialector 的官方模块声明兼容 Go 1.25 基线，但仍须在 P1 PoC 中锁定相互兼容的具体版本并运行完整依赖图测试。

| 组件 | 选型状态 |
| --- | --- |
| `opamp-go v0.25.0` | P0 已依赖 |
| `gorm.io/gorm` | P1 候选，固定版本待验证 |
| `gorm.io/driver/postgres` | P1 候选 |
| `gorm.io/driver/mysql` | P1 候选 |
| `koanf/v2` | P1 候选 |
| `goose/v3 v3.27.0` | Go 1.25 兼容的 Migration CLI 候选 |
| `testcontainers-go` | PostgreSQL/MySQL 双数据库集成测试候选 |

这里不再选择 sqlc 或 Tern。Goose 作为独立 CLI 运行，不强制加入 Control Plane 的运行时依赖。

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

1. Koanf YAML + 白名单环境变量覆盖、非法 Driver/DSN/TLS 校验和敏感信息脱敏。
2. GORM 同一套 Repository 在 PostgreSQL/MySQL 上执行 CRUD、唯一性约束、CAS、事务原子性与旧 ACK 测试。
3. Goose 分数据库 Migration，包含 MySQL DDL 失败后的恢复验证；禁止生产 AutoMigrate。
4. OpAMP HTTP/WS 状态接入持久化，重连及服务端重启后恢复 Desired/Observed。
5. `go test -race ./...`、`go vet ./...`、真实双数据库集成测试与 RF Validate 通过，记录 Git SHA 和 CI 证据。

## 11. 官方资料

- [Go `database/sql`](https://pkg.go.dev/database/sql)；[pgx stdlib](https://pkg.go.dev/github.com/jackc/pgx/v5/stdlib)
- [GORM PostgreSQL](https://github.com/go-gorm/postgres) / [MySQL](https://github.com/go-gorm/mysql)
- [GORM Transactions](https://gorm.io/docs/transactions.html)；[GORM Updates](https://gorm.io/docs/update.html)
- [Goose 文档](https://pressly.github.io/goose/)；[Goose SQL 注解](https://pressly.github.io/goose/documentation/annotations/)
- [MySQL 隐式提交说明](https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html)
- [Koanf v2](https://github.com/knadh/koanf)
- [Testcontainers-Go PostgreSQL](https://golang.testcontainers.org/modules/postgres/)；[MySQL](https://golang.testcontainers.org/modules/mysql/)
- [当前项目 `go.mod`](https://github.com/XiaoWeiKIN/apm-control-plane/blob/main/go.mod)
