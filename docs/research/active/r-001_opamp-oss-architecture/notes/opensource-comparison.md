# 四个开源项目的存储、配置与生命周期架构

> Research R-001，2026-10-19。**直接证据**来自下列 GitHub 固定提交的源代码或开发文档；**方案推论**仅针对本项目。尚未运行本项目 GORM / PostgreSQL / MySQL 集成测试。

## 1. 研究目标

验证 Gitea、Grafana、Casdoor 和 OTel Collector 的真实设计：数据库 Driver 如何选择、ORM 是否直接暴露给业务、Migration 如何管理、配置与组件生命周期如何装配；再判断哪些做法适合目前的 Go OpAMP Control Plane。

本项目的领域负载主要是 Agent Inventory、Configuration、Release、Assignment 和审计的 CRUD、事务、CAS。Trace/Metric 写入不经过这套数据库。已明确支持 PostgreSQL 和 MySQL，但首期不做在线跨引擎迁移。

## 2. Gitea：ORM 共享，方言与生命周期仍需管理

**证据（固定 Gitea commit `458eda0f1d49f9413afdb353c561402b18267d43`）**

- [`modules/setting/database.go`](https://github.com/go-gitea/gitea/blob/458eda0f1d49f9413afdb353c561402b18267d43/modules/setting/database.go) 定义 `SupportedDatabaseTypes`，包括 mysql、postgres、mssql；SQLite 的可用性与构建驱动相关。`loadDBSetting` 读取 DB_TYPE、连接信息及连接池等参数。
- [`models/db/engine_init.go`](https://github.com/go-gitea/gitea/blob/458eda0f1d49f9413afdb353c561402b18267d43/models/db/engine_init.go#L26-L74) 使用 `xorm.NewEngine` 和数据库方言选项构造 Engine。MySQL 情况单独设置默认 `sql.LevelReadCommitted`。这是**明确的方言事务行为处理**，并非 ORM 自动消除语义差异。
- [同文件关于全局 Engine 的注释](https://github.com/go-gitea/gitea/blob/458eda0f1d49f9413afdb353c561402b18267d43/models/db/engine_init.go#L81-L102) 记载默认 Engine 可被覆盖、相关函数存在竞态和关闭困难。
- [`routers/common/db.go`](https://github.com/go-gitea/gitea/blob/458eda0f1d49f9413afdb353c561402b18267d43/routers/common/db.go) 在启动阶段连接重试、初始化、检查/执行版本迁移。

**设计推论**：多数据库业务应用可以共享 ORM，但要显式处理 Driver 和事务差异。我们不复制全局 DB Engine；由服务启动器构造并在 Shutdown 关闭存储实例。Gitea 使用的是 **XORM，不是 GORM**。

## 3. Grafana：Service 经 SQLStore，迁移独立治理

**证据（固定 Grafana commit `e4ff54aa88c6c71107f2cc6f9be4a2a93453c58e`）**

- [`contribute/backend/database.md`](https://github.com/grafana/grafana/blob/e4ff54aa88c6c71107f2cc6f9be4a2a93453c58e/contribute/backend/database.md) 明确 SQLite、PostgreSQL、MySQL 和 XORM；规范性说明 **Services don't use XORM directly**，而是通过 `SQLStore`。文档区分 `WithDbSession` 与 `WithTransactionalDbSession`，并说明旧 SQLStore Handler 路由已被标记 deprecated。
- [`pkg/services/sqlstore/sqlstore.go`](https://github.com/grafana/grafana/blob/e4ff54aa88c6c71107f2cc6f9be4a2a93453c58e/pkg/services/sqlstore/sqlstore.go) 中 `SQLStore` 拥有 ORM Engine、Migrate 和存储生命周期；`Run` 在服务退出时关闭 Engine，注明依赖图使其被最后关闭。
- [`database_config.go`](https://github.com/grafana/grafana/blob/e4ff54aa88c6c71107f2cc6f9be4a2a93453c58e/pkg/services/sqlstore/database_config.go) 包含 DB 类型、SSL、连接池、事务重试、Migration Lock 和超时配置。
- Grafana 的开发者数据库文档要求**已合入的 Migration 不得改写**，修正使用新 Migration；项目以 Golden Test 对迁移 ID 变更施加人工审查。

**设计推论**：推荐把 GORM 封装在少量 `StorageService` / Domain Repository 后面，保持事务所有权明确；生产数据库升级单独管理。但我们的 Control Plane 规模远小于 Grafana，**无须复制 Grafana 的全部 SQLStore、消息总线和 Migration 基础设施**。

## 4. Casdoor：配置切库，但不代表所有业务都经过严格 Repository

**证据（固定 Casdoor commit `2e0a10c97d949fc88b1f1505fe19db35c880645d`）**

- [`conf/app.conf`](https://github.com/casdoor/casdoor/blob/2e0a10c97d949fc88b1f1505fe19db35c880645d/conf/app.conf) 明确 `driverName`、`dataSourceName`、`dbName`。
- [`object/adapter.go`](https://github.com/casdoor/casdoor/blob/2e0a10c97d949fc88b1f1505fe19db35c880645d/object/adapter.go#L141-L216) 的 `InitAdapter` 按 mysql、postgres、mssql、sqlite 等情况构建不同 DSN，再通过 `xorm.NewEngine` 创建引擎；同文件 `AddAdapter` / `DeleteAdapter` 等可直接操作 ORM Engine。

**设计推论**：Driver 是明确的部署配置，各 Driver 的 DSN 和初始化确有差别；“知名项目都实施严格 Repository Port”并不成立。我们可以比 Casdoor 更明确地隔离 GORM，但不应为每张表机械创建一个单独接口。Casdoor 核对代码也**不是 GORM**。

## 5. OpenTelemetry Collector：Provider、Factory、Start/Shutdown

**证据（固定 Collector commit `ebfdf90d82abe991c69554b31f2efc4a96622803`）**

- [`component/component.go`](https://github.com/open-telemetry/opentelemetry-collector/blob/ebfdf90d82abe991c69554b31f2efc4a96622803/component/component.go#L14-L62) 将组件生命周期明确为 Creation → Start → Running → Shutdown；Shutdown 允许在 Start 未完成或重复调用时安全执行。`Factory` 提供 `CreateDefaultConfig`。
- [`confmap/README.md`](https://github.com/open-telemetry/opentelemetry-collector/blob/ebfdf90d82abe991c69554b31f2efc4a96622803/confmap/README.md) 将配置切为 `Conf`、`Provider`、`Converter`、`Resolver`；Resolver 整合配置来源、转换及变更观察。
- [`service/service.go`](https://github.com/open-telemetry/opentelemetry-collector/blob/ebfdf90d82abe991c69554b31f2efc4a96622803/service/service.go) 显式接收 Receivers/Processors/Exporters/Extensions 的 Factory 和 TelemetryFactory，再创建 Service。

**设计推论**：我们的 Koanf `Load → Decode → Validate`、GORM DB Pool、OpAMP Gateway、Reconciler 应有清晰启动顺序与停止清理，并按 `AgentKind` 扩展配置 Adapter。**不可照搬 Collector Receiver → Processor → Exporter 数据面 Pipeline**；Collector Confmap Watch 也不是可绕开 Release/Audit 的 OpAMP RemoteConfig 管道。

## 6. 比较矩阵

| 关注点 | Gitea | Grafana | Casdoor | Collector | 适合本项目的约束 |
|---|---|---|---|---|---|
| ORM / 多数据库 | XORM + Driver 选项 | XORM + SQLStore | XORM + 多 DSN/Engine | 非关系数据库产品 | GORM + PostgreSQL/MySQL Dialector 是我们的候选，而不是这些项目的选型 |
| 业务与存储边界 | DB Engine/Model 分层，存在全局 Engine | Service 不直接用 XORM | 部分 object 层直接使用 Engine | 工厂/组件边界 | 少量 Storage Service/Repository，不让 GORM 泄漏 |
| 事务正确性 | MySQL ReadCommitted 特殊处理 | 明确 TransactionalDbSession | 需审查每个 ORM 调用 | 不适用 | Release 原子事务、Assignment CAS |
| Schema Migration | 启动 Migration / 版本检查 | 迁移受严格审查 | 多类 DB 的引擎初始化 | 配置校验独立 | Goose，Migration 需审查；禁用生产 AutoMigrate |
| Config | setting.DB_TYPE | DatabaseConfig / Migrator 配置 | driverName/DSN | Provider/Converter/Resolver | Koanf Typed Config，不能与 Agent RemoteConfig 混用 |
| 生命周期 | 全局 Engine 的关闭存在历史问题 | SQLStore 末尾关闭 Engine | Engine 初始化持有 | Start/Shutdown 合同 | 显式创建、Readiness、Graceful Shutdown |

## 7. 反证、结论与验证

**反证**：四个开源项目并未共同使用 GORM；其中 Gitea、Grafana、Casdoor 的取证版本使用 XORM。因此这些源码只能证明“多数据库应用通常保留 ORM、方言配置和迁移控制”，不能作为“GORM 已在这些系统验证”的证据。Casdoor 也表明所谓“所有项目都通过严格 Repository Port”不是事实。

**条件性推荐**：维持 P1 的 GORM v2 + Goose + Koanf，采取一套 GORM Repository + 显式 Storage 生命周期；并由业务层显式实现 Release 事务、CAS、身份授权和旧 ACK 检查。理由是本项目的查询简单、需要多 RDBMS、维护两套 SQL 查询的收益暂未证明，并**非**知名项目的库选择投票。

这只是**合理的工程假设（置信度中等）**，尚缺本项目真实数据支持。可能推翻它的信号有：某些复杂 SQL 难以安全表达、同一 GORM Model 在两库持续漂移、Lock/CAS 行为不符合合同、内存/连接池成为瓶颈或实际扩展令 ORM Query 与业务模型深度耦合。

**P1 PoC 的可核验条件**：使用 Testcontainers 分别运行 PostgreSQL/MySQL 的 UID 唯一约束、Tenant Scope、Agent CRUD、Release+Assignments 同一事务、Assignment CAS、旧 ACK、重启恢复；Goose Up 和 MySQL DDL 失败恢复；压根不要以 `go test ./...` 的非数据库单测替代。还须测试 DB Pool 的 Shutdown、OpAMP HTTP/WS 事件与数据库异常。

## 8. 证据边界

本轮只对比固定源码/文档，没有克隆、构建或运行这四个完整项目，也没有跑本项目双数据库 PoC。正式设计通过 RepoFoundry 的 Research/Design/ADR 生命周期处理；本 Research 不直接完成架构批准。
