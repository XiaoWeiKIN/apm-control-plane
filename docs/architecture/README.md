# APM Control Plane：开发者架构指南

> 面向贡献者和维护者的技术说明。本文介绍系统的整体结构、核心概念、控制流程、状态与接口边界，并为实现和扩展提供源码入口。
>
> **文档状态**：开发者技术说明，已按 2026-10-20 合并的 P1.1 Workspace-scoped Inventory 和 [DD-001 设计草案](../design-docs/dd-001_opamp-control-plane.md) 对照更新。文中的 **现状** 表示仓库已有代码或测试；**目标设计** 表示尚待实施或批准的方案；**未来扩展** 表示不在 EP-001 验收范围内的能力。本文件不是新的受治理 DD，不替代 DD-001 的批准流程，也不是已接受的 ADR。
>
> **主要读者**：熟悉 Go、OpenTelemetry 或可观测性系统，准备维护 OpAMP 接入、Agent Inventory、远程配置与策略发布模块的工程师。

## 阅读路径

第一次接触本项目，建议依次阅读「系统概览 → 三条主要流程 → 核心概念 → 模块与契约」。准备开发新功能时，再阅读「并发与一致性 → 安全与故障 → 扩展一个 Agent 类型 → 源码地图」。

核心模块职责、状态归属、Go Package 边界与 P2 开发顺序以 [核心模块与职责边界](modules.md) 为阅读入口；数据库字段、关系与 CAS 约束以 [Workspace Schema](storage-schema.md) 为准。已经采用的 Koanf/GORM/Goose 见 [P1–P3 技术选型](technology-selection.md)。这些开发者文档不替代 DD-001 的正式审批。

本项目的业务需求以 [FR-001～FR-013](../requirements/APM_CONTROL_PLANE.md) 为准；架构审批与版本身份由 [DD-001](../design-docs/dd-001_opamp-control-plane.md) 管理；里程碑和验证由 [EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 管理。

## 1. 系统要解决什么问题

APM Agent 通常与业务进程一起部署。以 Java 为例，开发者通过 `-javaagent` 启动 OpenTelemetry Java Agent。应用运行后，运维人员仍需要知道哪些 Agent 存活、部署了哪个发行版、使用什么采样策略、配置有没有更新成功，以及错误配置能否安全撤回。

单独依靠 JVM 启动参数或静态环境变量，难以对大规模、不断重建的工作负载执行集中策略发布。本项目采用 **OpAMP（Open Agent Management Protocol）作为管理协议**，构建一个统一的 Agent Control Plane。

平台的**长期产品范围**是 Java、Go、Python 等多语言 APM Agent，并在未来支持 OpenTelemetry Collector。**第一条落地链路**仍是自定义 OpenTelemetry Java Agent 发行版。通用控制面保持语言中立，实际配置格式、运行时热更新与重启语义由各 Agent 类型的 Adapter/Runtime 负责。

### 1.1 目标与非目标

| 目标 | 说明 |
| --- | --- |
| Agent Inventory | 发现并追踪 Agent 实例、发行版、能力、连接新鲜度和运行健康 |
| 配置管理 | 维护不可变配置版本、目标选择器、实例期望配置与实际报告状态 |
| 安全分发 | 通过 OpAMP HTTP/WebSocket 下发目标 Agent 确认能够接受的配置 |
| 一致性协调 | 通过 Reconciler 处理离线、重连、重复消息、旧回执和控制端重启 |
| 发布治理 | 记录一次发布的目标、批次、进度、审计和回滚操作 |
| 可扩展性 | 允许 Java、Go、Python、Collector 使用不同的配置方言与生效规则 |

首期**不实现**：Agent 二进制升级、Collector Supervisor 生命周期管理、其他语言 Runtime Adapter、Collector 配置执行、OTLP Trace 转发、Tail Sampling、跨地域多活。架构预留接口不代表这些能力可用。

### 1.2 控制面与数据面

**OpAMP 控制连接**只负责管理消息；**OTLP 数据连接**负责传输 Traces、Metrics 和 Logs。管理端不能因配置下发失败而阻止已经运行的业务 SDK 向 Collector 上报数据。

~~~mermaid
flowchart TB
    Console["管理界面 / 外部 API 使用者"] --> API["Management API"]

    subgraph CP["APM Control Plane · 逻辑模块，首期同一 Go 进程"]
      API --> Registry["Inventory / Registry"]
      API --> Policy["Policy / Configuration"]
      Policy --> Release["Release / Rollout"]
      Release --> Reconciler["Reconciler"]
      Reconciler --> Gateway["OpAMP Gateway"]
    end

    Registry <--> DB[("Relational DB · PostgreSQL/MySQL 目标设计")]
    Policy <--> DB
    Release <--> DB
    Reconciler <--> DB

    Gateway <-->|"OpAMP 控制消息"| Java["自定义 Java Agent · 首期"]
    Gateway <-.->|"未来"| Other["Go / Python Agent"]
    Gateway <-.->|"未来"| CollectorAgent["被管理的 Collector"]

    Java -->|"OTLP"| Collector["Collector / OTLP Receiver"]
    Other -.->|"OTLP"| Collector
    Collector --> Backend["APM Backend / ClickHouse"]
~~~

上图描述**P1–P3 目标拓扑**，不是所有模块都已部署。P1.1 已通过 GORM/Goose 在 PostgreSQL 和 MySQL 上实现持久化 Agent Registry；Policy Release 与 Reconciler 仍未实现。Workspace 是可观测平台统一的工作空间边界，当前 P1.1 的作用域来自受信的部署凭证配置，平台 Workspace/IAM 联通还未完成。所谓“未来 Collector”是**被管理对象**，不能与图中的 OTLP Receiver 自动视为同一个实例。

## 2. 从一次采样策略发布理解系统

假设运维人员希望把 `prod` 环境 `checkout` 服务的 Head Sampling 目标从 10% 调整到 20%。该场景说明各模块为什么存在。

1. **Management API** 验证调用者权限，读取策略选择器和采样规则。
2. **Policy Manager** 生成不可变策略版本，解析授权范围内的目标服务和 Agent 实例。
3. **Config Resolver** 检查每个目标的 Agent Kind、Distribution、配置格式和 OpAMP Capabilities，再由 Java Config Adapter 生成实例可执行的配置快照。
4. **Config Release** 保存本次发布的目标集合和预期配置版本。**Reconciler** 为每个 Agent 建立或更新 Desired State。
5. **OpAMP Gateway** 向在线 WebSocket Client 主动发送，或在 HTTP Client 下一次请求中返回配置。
6. **Java Agent Runtime** 验证、编译和应用配置，随后通过 OpAMP 报告配置 Hash 与应用状态。
7. **Registry/Assignment** 保存 Observed State。只有状态为 `APPLIED` 且 Hash 对应当前 Desired Config 时，才能将该 Agent 的当前配置标记为已应用；即使如此，也不代表已经通过遥测验证真实采样效果。

~~~mermaid
sequenceDiagram
    participant U as 管理调用者
    participant API as Management API
    participant DB as Relational Storage
    participant RC as Reconciler
    participant GW as OpAMP Gateway
    participant JA as Java Agent

    U->>API: 发布 checkout/prod 的新采样策略
    API->>API: 鉴权、校验、解析目标
    API->>DB: 事务提交 PolicyRevision、Release、Assignments
    DB-->>API: 已提交版本与发布 ID
    API-->>U: 发布已接受（不是已应用）
    RC->>DB: 查询尚未收敛的 Desired Config
    RC->>GW: 请求发送 ConfigHash / ConfigMap
    alt WebSocket 且连接仍有效
      GW->>JA: ServerToAgent.remote_config
    else HTTP 拉取
      JA->>GW: AgentToServer（下一次轮询）
      GW-->>JA: ServerToAgent.remote_config
    end
    JA->>JA: 校验并尝试应用
    JA->>GW: AgentToServer.remote_config_status
    GW->>DB: 记录原始状态和报告的 Hash
    DB-->>RC: 当前 Assignment 可核对
    RC->>DB: 匹配 Hash 的 APPLIED 才标记 Applied
~~~

**关键结论**：API 接受发布、协议层发送、Agent 报告应用、数据面证明效果，是四种不同的成功边界。不要把它们折叠为一个布尔值 `success`。

## 3. 核心概念

这些概念按所有权和生命周期分组，不等同于数据库的一张张物理表。

| 概念 | 标识和生命周期 | 职责与边界 |
| --- | --- | --- |
| **Workspace / Principal** | 经认证的组织/调用者范围 | 决定可以查看、控制哪些 Agent；不得从客户端自报的 `service.name` 推导 |
| **Service** | Workspace + Environment + Namespace/Name | 业务工作负载分组；一个 Service 可以关联多个 APM Agent；Collector 不必映射为 Java Service |
| **Agent Instance** | OpAMP `instance_uid`，限定在已认证范围内 | 某个运行中的被管理 Agent；UID 按协议是 16 字节，并非长期资产或授权凭据 |
| **Agent Session** | 一次 WS 会话或 HTTP 请求上下文 | 短生命周期通信状态。HTTP 活跃程度由最近轮询时间推断，不能当作长连接 |
| **Agent Kind** | `java-apm` / `go-apm` / `python-apm` / `otel-collector` 等平台标识 | 选择元数据规则和配置 Adapter；仅是类型，不代表已经支持该配置能力 |
| **Distribution** | 发行版名与版本 | 区分具体 Runtime 兼容性，不应把应用的 `service.version` 当作发行版版本 |
| **Capability Set** | OpAMP 位标记 + 平台/发行版能力声明 | 判断能否接收远程配置、报告 Effective Config、热更新某一配置类别 |
| **Policy Revision** | 不可变策略 ID/Revision | 用户定义的规则；例如 `checkout/prod` Head Sampling 20% |
| **Config Revision** | 不可变 ConfigMap/Schema/Hash | 针对某类 Agent 编译出的配置字节；和业务策略版本分离 |
| **Config Release** | 一次发布操作及目标集合 | 追踪操作者、批次、时间、失败阈值和回滚 |
| **Agent Assignment** | 某个 Agent 当前的 Desired Config 与状态 | 不应由发送结果直接认定已应用；旧回执不能覆盖新目标 |
| **Agent Observation** | Agent 最后一次报告的状态快照 | 报告 Health、Capabilities、RemoteConfigStatus 和可选 EffectiveConfig |

### 3.1 Agent 身份不是连接，也不是业务服务

同一个 `checkout` Service 可以有十个 JVM，各自拥有不同的 Agent Instance。某个 JVM 重启后，新进程是否延续原 UID，必须遵守 OpAMP 的 UID 生命周期约定与我们的身份策略；不能仅凭 Pod 名称推断同一 Agent。

OpAMP `instance_uid` 是一个 16 字节标识，规范建议使用 UUIDv7，尽量在 Agent 进程生命周期内保持不变。`AgentDescription` 中的 Identifying Attributes 用来将 Agent 与其遥测关联，但客户端上报的业务标签**不能替代认证凭据**。

OpenTelemetry [Instrumentation Agent Guidelines](https://github.com/open-telemetry/opamp-spec/blob/main/opentelemetry-guidelines.md) 对嵌入业务进程的语言 Agent 和 Collector 给出了不同的属性建议：语言 Agent 需要对齐 SDK Resource 中的 `service.name`、`service.instance.id` 等；Collector 的 `service.*` 是 Collector **自身遥测的身份**，并不自动代表它所采集的业务服务。

## 4. 关键组件及责任边界

建议使用 **Go 模块化单体（modular monolith）**。模块名称表示代码职责，不要求每个模块都是独立服务。只在容量、发布节奏或会话所有权有实测依据时拆部署单元。

**模块所有权与实施合同统一见 [核心模块与职责边界](modules.md)**：六个主要功能是 Registry、OpAMP Gateway、Policy、Config、Release、Reconciler；Workspace/Auth、Management API 和 Storage 为三个支撑模块。AgentConfigAdapter 属于 Config，Rollout 属于 Release。下表是概览，不能把模块名或逻辑表名自动映射成独立服务/Repository。

| 逻辑模块 | 输入 | 负责的状态与行为 | 不负责 |
| --- | --- | --- | --- |
| Management API | 管理请求、Authenticated Principal | 授权、校验、查询、创建发布操作 | 不直接写 OpAMP WebSocket |
| Registry | Agent 状态上报、身份绑定 | Agent 实例、最近观察、连接活跃度 | 不生成策略配置 |
| Policy Manager | 策略规则、Selector | 优先级、版本、冲突检查 | 不读取/控制具体 JVM Sampler |
| Config Resolver | Policy + Agent Kind/Version/Capabilities | 解析规则、编译完整 AgentConfigMap、计算 Hash | 不持有 Agent 连接 |
| Config Release / Rollout | 目标集合、Revision、批次策略 | 可审计发布、分批进度、暂停与回滚意图 | 不把 `SENT` 当成 `APPLIED` |
| Reconciler | Desired/Observed、会话可用性 | 重试、失步修复、下一次发送决策 | 不修改规范性业务策略 |
| OpAMP Gateway | HTTP/WS、AgentToServer/ServerToAgent | 协议编解码、认证入口、回调、单连接发送 | 不保存唯一的业务事实源 |
| Storage | 事务、版本、事件 | GORM PostgreSQL/MySQL Dialector 持久化，统一 Repository/UnitOfWork 业务语义 | 不保存活着的网络连接 |
| Agent Config Adapter | Agent Kind + 版本与能力 + 规则 | 输出某类 Agent 能理解的配置并声明应用限制 | 不假设所有 Agent 能热更新 |

### 4.1 建议的依赖方向

~~~mermaid
flowchart LR
    API["api"] --> Domain["policy / config / registry / release"]
    OpAMP["opamp adapter"] --> Domain
    Reconcile["reconcile"] --> Domain
    Domain --> Ports["domain interfaces"]
    Storage["storage/gormrepo · PG/MySQL Dialector"] -.->|"实现"| Ports
    AgentAdapters["Java AgentConfigAdapter"] --> Domain
~~~

协议模型与业务模型必须隔离：上游 `protobufs.AgentToServer` 可以进入 OpAMP Adapter，但不应直接成为 Policy Manager 或数据库无关 Repository Port 的核心类型。这一点与 Collector 社区通过 Component/Factory 接口隔离具体实现的设计风格类似，**借鉴的是模块边界，不是照搬 Collector 的 Receiver–Processor–Exporter Pipeline**。

### 4.2 核心接口示意

下列 Go 代码为**建议的逻辑接口草图**，不是仓库已有的可编译 API。具体类型、签名与错误分类应在 P1/P2 实现中确定。

```go
type AgentDescriptor struct {
    InstanceUID    [16]byte
    Kind           AgentKind
    Distribution   string
    Version        string
    Capabilities   CapabilitySet
    Service        *ServiceIdentity // Collector may have no business service
}

type AgentConfigAdapter interface {
    Supports(agent AgentDescriptor, kind ConfigKind) SupportResult
    Compile(policy ResolvedPolicy, agent AgentDescriptor) (ConfigSnapshot, error)
}

type ConfigSnapshot struct {
    Files map[string]ConfigFile // immutable after compilation
    Hash  []byte                // deterministic canonical hash
}

type AssignmentRepository interface {
    Desired(ctx context.Context, agentID AgentKey) (Assignment, error)
    RecordObservation(ctx context.Context, observation ConfigObservation) error
}

type ReconcileQueue interface {
    Enqueue(agentID AgentKey)
}
```

接口必须保留无法支持配置的显式结果：`UnsupportedAgentKind`、`UnsupportedConfigDialect`、`MissingCapability`、`IncompatibleDistributionVersion`。这些是**平台错误分类建议**，不是 OpAMP 原生枚举。

### 4.3 Control Plane 进程启动与停止（目标生命周期）

Collector 的 [Internal Architecture](https://github.com/open-telemetry/opentelemetry-collector/blob/main/docs/internal-architecture.md) 不只介绍组件，还说明从启动入口到组件就绪的流程。本项目沿用这种写法，但具体步骤是**P1 的拟议启动合同**，不是当前主程序已经具备的流程。

~~~mermaid
flowchart TD
    A["读取启动配置 / 验证 TLS 与认证参数"] --> B["按 Driver 连接 PostgreSQL/MySQL / 校验 Schema Version"]
    B --> C["构造 Repository、Registry、Config Adapter Registry"]
    C --> D["构造 Policy / Release / Reconciler"]
    D --> E["Attach OpAMP callbacks + Management Router"]
    E --> F["启动 HTTP Listener"]
    F --> G["从数据库恢复未完成 Desired Assignments"]
    G --> H["启动 Reconciler 和健康探针"]
    H --> I["Ready：接收受控发布与 Agent 请求"]
    I --> J["收到 SIGTERM"]
    J --> K["停止新发布 → 停止接入 → 协调器排空"]
    K --> L["关闭活动会话 / 数据库连接"]
~~~

各阶段的失败条件必须明确。例如：数据库不可用或 Schema 不兼容时，不能启动一个“看起来 Ready”但无法持久化 Desired Config 的管理接口；应报告启动失败或维持 Not Ready。配置内容校验、权限系统尚未可用时，也不能为了让 HTTP Listener 可访问而绕过鉴权。

关闭时应停止新的发布和数据写入、取消重试任务、为正在发送的连接提供有界关闭时间，并让未完成 Assignment 保留在数据库中供下次启动重新协调。**P0 目前只是通过 net/http 启动并 Shutdown；上述持久化恢复和排空行为还没有实现**。

## 5. OpAMP 协议与平台契约

OpAMP 是供应商中立的 Agent 管理协议，支持 HTTP 和 WebSocket。它定义消息格式与能力协商，不定义 Java SDK Sampler、Go Agent 或 Collector 的实际配置文件语义。

官方协议参考：[OpAMP Specification](https://github.com/open-telemetry/opamp-spec/blob/main/specification.md)、[Instrumentation Guidelines](https://github.com/open-telemetry/opamp-spec/blob/main/opentelemetry-guidelines.md)、[opamp-go](https://github.com/open-telemetry/opamp-go)。

| OpAMP 结构/字段 | 本平台消费方式 | 需要避免的误解 |
| --- | --- | --- |
| `AgentToServer.instance_uid` | 识别单个 Agent 进程实例 | UID 自报，不是授权凭据 |
| `AgentToServer.agent_description` | 构建声明属性和资源身份 | 字段可因状态压缩省略，不能当作每次都重传 |
| `AgentToServer.sequence_num` | 检测状态压缩下消息缺失或失步 | 不能把间断序号视作业务发布版本 |
| `AgentToServer.capabilities` | 协议能力协商 | 支持 RemoteConfig 不等于支持 Java 采样 DSL |
| `AgentToServer.health` | Agent 主动报告的健康状态 | HTTP 请求结束不等于 Agent 不健康 |
| `AgentToServer.remote_config_status` | 记录最后远程配置 Hash、APPLIED/APPLYING/FAILED、错误信息 | Hash 是最后收到的远程配置，不等于 Release ID |
| `AgentToServer.effective_config` | 在声明 ReportsEffectiveConfig 后记录实际有效配置 | 可由本地配置和远程配置合成，不必与 Remote Config 字节相同 |
| `ServerToAgent.remote_config` | 向目标 Agent 下发 ConfigMap、Hash | 配置体格式由 Agent Kind 决定 |
| `ServerToAgent.flags` | 在需要时请求 ReportFullState | 不能假设 Client 每次发送完整快照 |

**重要的协议细节**：`RemoteConfigStatus` 的值是 `APPLIED`、`APPLYING`、`FAILED`（外加未设置），平台自定义的 `PENDING`、`SENT`、`TIMED_OUT`、`UNSUPPORTED` 不能发送或记录为 OpAMP 原生 Status。

### 5.1 配置如何表示

OpAMP 的 `AgentRemoteConfig` 包含 `AgentConfigMap` 与 `config_hash`；一个 ConfigMap 是多个具名配置对象的集合。每项可包含 `body`、`content_type` 和可选 `role`。规范不规定其正文格式；文件合并规则也由 Agent 自己定义。

首期 Java 配置约定如下（**本项目自定义 Schema**，不是 OpAMP 标准）：

```json
{
  "schema_version": 1,
  "sampling": {
    "type": "parent_based_trace_id_ratio",
    "rate": 0.2
  }
}
```

Java Config Adapter 将该 JSON 序列化为命名文件 `apm/sampling.json`，`content_type=application/json`，并为完整的 ConfigMap 计算确定性 Hash。**所有组成 Hash 的命名文件、字节、content_type/role 应在同一规则下排序与编译**；配置版本与内容 Hash 分开保存。上面 JSON 只表示编译结果，不应由 Admin API 直接作为跨发行版的通用配置。

### 5.2 Java SDK 中的应用

目标发行版内部可复用 [OpenTelemetry Java OpAMP Client](https://github.com/open-telemetry/opentelemetry-java-contrib/tree/main/opamp-client)，由 Runtime Config Manager 验证策略后替换一个在 SDK 初始化时已安装的 Dynamic Sampler。这里涉及 SDK 初始化生命周期、ClassLoader 隔离与 ParentBased 采样语义，不能仅靠改变 `otel.traces.sampler.arg` 来实现热更新。

关于 Java Agent 的分层、ClassLoader 和扩展 SPI，参见 [Java Agent Structure](https://github.com/open-telemetry/opentelemetry-java-instrumentation/blob/main/docs/contributing/javaagent-structure.md) 与 [Extensions](https://opentelemetry.io/docs/zero-code/java/agent/extensions/)。自定义发行版不意味着必须 Fork 全部上游 Instrumentation。

**能力限制**：Head Sampling 通常在 Span 创建时决策。HTTP 最终状态码和请求耗时等结束时才知道的字段，不可直接承诺在 Head Sampler 中按最终值判断。Collector Tail Sampling 属于另一阶段；Head 已丢弃的完整 Trace 无法靠 Tail Sampling 恢复。

## 6. 配置解析、发布与一致性

### 6.1 策略层级和冲突

建议的业务层级为：

```text
Global Default
    └── Environment / Service policy
            └── Instance Override（具备 TTL 与审计）
```

Resolver 首先检查 Workspace 授权与 Agent Kind，再选择该目标唯一适用的策略集合。一个 Agent 可能匹配多条规则，因此必须定义稳定优先级并拒绝同级冲突，不能取 Go Map 遍历中的“第一个”。

首期仅 Java Sampling Adapter 消费 Sampling Policy；如果目标为 Collector，即使它宣告 `AcceptsRemoteConfig`，也不能收到 `apm/sampling.json`。其他 Runtime 的配置方式、重启要求和编译器要单独扩展。

发布时应**冻结本次 Release 的目标 Agent 集合**，用于审计本次批次的真实成功率。之后新上线并满足同一服务级 Binding 的 Agent，可以由 Reconciler 为它创建新的 Assignment；不能默默把它追认为已经完成的旧 Release 成员。这条规则属于**建议设计**，需要在 DD-001 正式评审时确认。

### 6.2 状态机：Desired 与 Observed

~~~mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> WaitingAck: 已向可用连接发送
    Pending --> Unsupported: 能力或配置不兼容
    WaitingAck --> Applied: matching hash + APPLIED
    WaitingAck --> Applying: matching hash + APPLYING
    Applying --> Applied: matching hash + APPLIED
    WaitingAck --> Failed: matching hash + FAILED
    Applying --> Failed: matching hash + FAILED
    WaitingAck --> TimedOut: 等待超过发布期限
    Failed --> Pending: 新一轮调度或显式重试
    TimedOut --> Pending: 恢复协调
    Unsupported --> Pending: Agent 能力变化并重新解析
    Applied --> Pending: 新 Desired Revision 发布
~~~

此图描述**平台的每实例 Assignment 状态**，不是 OpAMP Protobuf 的状态枚举。`SENT` 或 `WaitingAck` 是投递信息，不是 Agent 真正执行成功的证据。

- Desired Config 指向目标 Config Revision/Hash，由管理发布事务负责更新。
- Observed Config 记录 Agent 上报的 Hash、状态、错误及时间戳，由协议接入负责采集。
- **只有**报告为 `APPLIED` 且上报 Hash 等于该 Agent 当前 Desired Hash，平台才能认定当前配置已应用。
- 如果 Agent 上报旧 Hash 的 `APPLIED`，保留原始历史，但不得更新新目标的 Applied 状态。
- **相同 Hash 可能被多个业务 Revision 或 Release 复用**：回执只能证明某个配置内容状态，不能单独识别业务发布事件。Release 与 Assignment 的归因必须通过当时的持久映射完成。
- 报告 `APPLIED` 只证明 Agent 的声明；是否真的改变采样行为，需要额外的运行时指标或 Trace 流量验证。
- 本地启动配置可能覆盖或合并远程配置。未收到 `ReportsEffectiveConfig` 就不能宣称已知最终 Effective Config。

### 6.3 为什么需要 Reconciler

从数据库读到的 Desired State 与 Agent 报告的 Observed State 不会永远同步。Agent 可能在断线时重启，服务端可能在投递后宕机，也可能有旧的 ACK 晚于新配置到达。

Reconciler 应根据**持久真相**重试，而不是把一次 WebSocket Push 当成“可靠消息”：

1. 读取当前目标 Assignment 和最后 Observed State。
2. 检查 Agent 身份、目标是否仍受授权、Kind/Distribution/Capabilities 是否仍兼容。
3. 若已匹配当前目标 Hash 且报告为 `APPLIED`，停止重复下发。
4. 若无兼容能力，标记 Unsupported 并保留未完成原因。
5. 若 WS Session 有效，排入单连接顺序发送队列；若 Agent 使用 HTTP，则留到下一次轮询请求响应。
6. 按退避策略重试失败的投递，并定期扫描未收敛 Assignment，修复事件丢失。

Reconciler 提供的是**最终收敛倾向和至少一次交付尝试**，不是跨多 Agent 的原子事务，也不保证每个策略只执行一次。Agent 端配置应用必须具备幂等性。

## 7. 三条影响实现的生命周期

### 7.1 Agent 注册和心跳

~~~mermaid
sequenceDiagram
    participant A as Agent Client
    participant G as OpAMP Gateway
    participant R as Agent Registry
    participant DB as Storage

    A->>G: 带凭证连接 / AgentToServer
    G->>G: 认证并绑定 Principal Scope
    G->>G: 校验 UID / Capabilities / Sequence
    G->>R: Merge Status（未出现的压缩字段保持旧值）
    R->>DB: 更新 Identity / Observation
    DB-->>R: 写入完成
    G-->>A: ServerToAgent（必要时 ReportFullState）
~~~

如果消息丢失、序号异常或服务端失去完整快照，应依据 OpAMP 的状态压缩规则请求 Agent 上报完整状态。重复 Instance UID 但来自不同授权身份或不同活动会话的情况，必须使用明确的 ownership/fencing 规则处理；不能靠“后来的连接覆盖先来的连接”当成安全证明。

**HTTP 模式的当前 P0 缺陷**：`registry.update()` 在每次请求中将 `Connected=true`，而 `OnConnectionClose` 会在 HTTP 请求完成时清除该状态。因而 HTTP Test Agent 在轮询间隔通常被显示为离线。P1 必须用 `last_seen + 心跳/轮询期限` 表示 HTTP 活跃度，不能套用 WebSocket 连接语义。

### 7.2 WebSocket 与 HTTP

| 项目 | WebSocket | HTTP |
| --- | --- | --- |
| 通信模型 | 持续的双向连接 | Agent 请求、Server 响应 |
| 主动变更配置 | 可向现有连接发 `ServerToAgent` | 必须等到下一次客户端请求 |
| 在线判定 | 连接生命周期 + 最近活跃 | 最近请求时间 + 配置的超时阈值 |
| 会话所有权 | 需要连接对象、单 writer、断开时 fencing | 一次请求上下文结束后不保留可推送的连接 |
| 失败重试 | 重连和 Reconciler 协调 | 下次轮询和 Reconciler 协调 |

使用 `opamp-go/server.Attach()` 允许将 OpAMP Endpoint 和管理路由挂载到同一 HTTP Server；这属于通信层复用，**不表示管理 API 与 Agent 接口应共享一套身份权限**。

### 7.3 发布、失败和回滚

发布操作按顺序完成：权限校验 → 策略解析 → 创建不可变 Policy/Config Revision → 在一个数据库事务中保存 Release 与 Targets/Assignments → 提交后触发 Reconciler。

数据库事务提交失败时，不应通过网络对 Agent 产生新的期望配置；发送失败时，保留已提交的 Desired State，等待重连和重试。

Rollout Controller 将发布目标分成可审计批次。每批使用匹配 Hash 的应用状态与超时/失败门槛决定是否继续；**当前没有产品级固定阈值，不能在文档中虚构 SLO**。

回滚是一项新的可审计发布操作，引用某个已验证的旧 Config Revision。历史 Release 的状态和事件不可被“抹掉”，也不能直接把数据库 Desired Hash 改回去却不留下操作者和原因。

## 8. 数据与一致性模型（目标设计）

**数据库字段、复合 Workspace 外键、事务/CAS、双方言和迁移路径的详细合同**见 [Workspace Schema 设计](storage-schema.md)：目标共 11 张核心逻辑表与三张按需扩展表，其中目前仅 `agent_instances` 已由 P1.1 持久化。**模块职责与状态归属**见 [核心模块设计](modules.md)。

一个 Control Plane 部署只选择 PostgreSQL 或 MySQL 中的一种主数据库；GORM 使用统一 Repository，Goose Migration 按数据库方言分别维护。当前只有 `agent_instances(workspace_id, instance_uid)` 已落地。未来的 Policy、ConfigRevision、Release、Target、Assignment 和 Event 都需要以数据库为可靠事实源，活动网络 Connection 则只存在于进程内存。

逻辑关键约束：

- AgentKey 必须包含被授权的 Workspace 范围，不能把可冲突的客户端 UID 裸值当全局授权键。
- PolicyRevision 与 ConfigRevision 不可变；同样 ConfigHash 可以被不同发布复用。
- 更新 Assignment 的 Desired Version 应带版本检查（CAS/乐观并发），避免较旧 Reconciler 回写覆盖新发布。
- 一个 Agent 的当前 Assignment 不应由独立的两个发布并发写入；冲突要么明确排队，要么明确失败。
- 网络发送不能放在持有长事务锁的路径中；数据库保存了哪个目标版本必须在回执关联时可追溯。
- 连接对象不写入数据库。未来 HA 需要明确 Gateway 节点的 Session 所有权以及转发机制，第一版不依赖 Redis/Kafka。
- 不能把审计事件当作唯一状态源；Desired/Observed 当前视图要能从一致的事务写入恢复，事件用于解释变化。

### 8.1 Management API 的目标资源形态

管理 API 应暴露业务资源，而不是让用户直接构造 `ServerToAgent` Protobuf。建议的 API 形态如下，**除 P0 已列出的接口外均属于候选契约**；需要通过正式 API 设计和兼容性评审后实施。

| Endpoint | 意图 | 当前状态 |
| --- | --- | --- |
| `GET /api/v1/agents` | 按已授权范围列出 Agent 实例及观察快照 | P0 已实现基础版本 |
| `GET /api/v1/agents/{uid}` | 查看单 Agent 的配置与健康摘要 | P0 已实现基础版本 |
| `PUT /api/v1/agents/{uid}/sampling` | 给单实例设置采样率 | P0 演示接口，长期是否保留需决定 |
| `POST /api/v1/config-policies` | 创建某 AgentKind 可使用的业务策略 | 目标设计 |
| `POST /api/v1/config-policies/{id}/revisions` | 添加不可变策略版本 | 目标设计 |
| `POST /api/v1/config-releases` | 校验目标并启动一次受控发布 | 目标设计 |
| `GET /api/v1/config-releases/{id}` | 查询目标数量、Applied/Failed/TimedOut、具体错误 | 目标设计 |
| `POST /api/v1/config-releases/{id}/pause` | 暂停后续批次，不篡改已应用实例 | 目标设计 |
| `POST /api/v1/config-releases/{id}/rollback` | 发起新发布操作，引用之前的有效 Revision | 目标设计 |

对修改类 API 建议使用显式幂等键或版本条件，避免客户端超时重试引发两次不可区分的发布。请求校验和兼容性错误应返回明确的机器可识别错误类型；具体 HTTP 状态码、分页与过滤、RBAC action、请求/响应 Schema 尚未定稿，不能把表格误用为现有 REST 合同。

一个目标发布请求可表达为以下**示意**（不保证可直接调用）：

```json
{
  "policy_revision": "checkout-sampling@12",
  "target": {
    "agent_kind": "java-apm",
    "environment": "prod",
    "service_name": "checkout"
  },
  "rollout": {
    "mode": "manual-batches"
  }
}
```

Server 应先验证调用者是否能控制相应 Workspace/Service，再在授权范围内解析 AgentKind 和 Selector。HTTP 成功响应仅说明发布意图已被持久化，不说明任一 Agent 已应用。

### 8.2 不同 Agent Kind 的首期能力矩阵

| Agent Kind | Inventory/Health 基础结构 | Java Sampling JSON | 当前交付 |
| --- | --- | --- | --- |
| 自定义 Java APM Agent | P1 通用模型适用 | P2 Java Config Adapter 目标 | 首期 |
| Go APM Agent | 仅验证元数据模型可表示 | 不支持；需 Go Adapter 与 Runtime 验证 | 未来 |
| Python APM Agent | 仅验证元数据模型可表示 | 不支持；需 Python Adapter 与 Runtime 验证 | 未来 |
| OTel Collector | 仅预留 AgentKind/Distribution/Capabilities | 不支持；需独立配置所有权与运行时契约 | 未来 |

能力判断不能只看 Agent Kind。即便两个 Java 发行版采用同一 Kind，版本不兼容或 Runtime 不支持动态字段，也必须拒绝对应配置，不能利用 `AcceptsRemoteConfig` 位标记推断能够安全应用。

## 9. 信任边界与安全

远程配置具有修改采集行为、Exporter 目标甚至潜在数据访问范围的能力。OpAMP 规范将 Remote Configuration 视为需要特殊保护的能力。

**P0 事实**：`cmd/server/main.go` 使用两个不同的全局 Bearer Token，默认监听 `127.0.0.1:4320`，并未实现每 Agent/工作空间的可信身份绑定、mTLS、RBAC 或生产 TLS 终止。它只能作为本地 PoC，不能被宣称为生产多工作空间边界。

**目标设计的最低要求**：

1. Management Principal 与 Agent Principal 分离。Admin API 对 Workspace、Service、策略操作执行授权；Agent token/mTLS 身份只被授予限定的实例及所属范围。
2. TLS 保护所有外部网络上的控制连接。若由反向代理终止 TLS，必须防止客户端伪造转发身份头。
3. 不从 `AgentDescription.service.name`、`service.version`、`host.name` 或 AgentKind 自报信息推导工作空间权限。
4. Config Adapter 不得让不兼容的 Agent 接受任意文件格式；Java 专用 JSON 不发送给 Go/Python/Collector 实例。
5. 发布前校验配置的 Exporter Endpoint、敏感字段和允许的目标。秘密配置需使用单独的加密与轮换机制，避免将凭证直接写进日志或审计事件。
6. Management API 与 OpAMP Endpoint 应分别进行速率限制、请求大小限制、超时及可审计拒绝处理。
7. Agent 主动报告的错误文本属于不可信输入；展示和日志需要限长、转义和适当的脱敏。
8. 鉴权、越权访问、重复 UID 争用、Token 撤销和不合法 ConfigMap 要有负向测试，而不仅是成功路径测试。

Agent 凭证最终采用 Scoped Token 还是 mTLS、以及重复 UID 的转移规则，仍是 DD-001 的待评审事项。此处只固定必须满足的安全不变量，不擅自宣布最终认证机制已获批准。

## 10. 故障处理与可观测性

| 故障场景 | 目标行为 | 验证入口 |
| --- | --- | --- |
| Agent 不可达 | 保留 Desired，状态显示 Pending/Offline；重连重新协调 | 断网/重连集成测试 |
| HTTP 请求结束 | 不直接认定进程死亡 | LastSeen 与轮询活跃度测试 |
| WebSocket 发送失败 | 不撤销已提交 Desired，串行重试或等待重连 | 并发发送与断线测试 |
| 当前选用的 PostgreSQL/MySQL 不可用 | 拒绝无法持久化的配置发布；不谎称发布成功 | 双数据库事务故障注入 |
| ConfigHash 不匹配 | 保留旧 ACK 历史，不认定新发布成功 | 旧回执乱序测试 |
| Agent 报告 FAILED | 记录错误并暂停相关批次或按明确策略重试 | 错误配置模拟 |
| Agent 能力变化 | 重新编译或标记 Unsupported，不能继续发送不兼容配置 | Capability 变化测试 |
| Gateway 重启 | Activity Session 重建，Desired/Observed 可从库恢复 | 进程重启测试 |
| 双连接重复 UID | 用认证范围和 Session generation/fencing 防止旧连接覆盖 | 连接竞争测试 |
| 控制平面故障 | 不修改业务应用已初始化的 OTLP Exporter | 业务 JVM 隔离测试 |

运维侧应具备自身 Telemetry。建议按低基数维度（AgentKind、Environment、状态、错误类别）提供：当前活跃 Agent 数量、上报间隔、待处理 Assignment、发布耗时分布、ACK 延迟、FAILED/Unsupported 数量、重试次数和队列积压。**不要把 Instance UID 或 ReleaseID 直接作为指标 Label**；高基数明细进入日志、审计和管理 API。

这些指标名称和容量/SLO 目前未确定；文档不将建议值写成现有实现或已验证性能。后续做容量优化前，应按 RepoFoundry Benchmark 先定义可复现的测量场景。

## 11. 扩展一个新的 Agent 类型

平台的共通能力不应复制一份“Go 版控制平面”或“Python 版控制平面”。增加 Agent Kind 应遵循以下扩展合同：

1. **身份映射**：声明 Distribution 与 Kind 的来源，区分该 Agent 自身遥测 Resource 和其采集的业务服务；检查 AgentDescription 与可信 Principal 的边界。
2. **Capabilities**：验证对应 Client 实际支持哪些 OpAMP 消息与状态上报；确认是否支持远程配置及其粒度。
3. **配置格式**：实现新的 AgentConfigAdapter，定义 Dialect/Schema/ContentType、静态/动态参数、优先级与版本兼容。
4. **应用语义**：列出每个配置字段是在当前进程热更新、需要重启，还是根本不支持。无法保证热更时不能返回“已热更新”。
5. **集成验证**：用该发行版真实的 Client/Runtime 测试配置应用、无效配置回退、重连、上游版本升级及混合发行版共存。
6. **发布约束**：只有上述验证足够后才允许生产 Release 选择该 Agent Kind；未实现 Adapter 时管理 API 应显示 Unsupported。

对 Collector 的支持要进一步界定其配置所有权：Collector 可能由 Kubernetes Operator、静态 YAML、Supervisor 或其他系统管理。只有配置权属明确、没有互相覆盖风险时，才允许本平台向其发送 RemoteConfig。Collector 的 Receiver/Processor/Exporter Pipeline 生命周期不等于 Java Agent 的动态 Sampler 生命周期，不能直接复用 Java Runtime 逻辑。

### 11.1 从社区借鉴什么，不借鉴什么

| 社区项目 | 借鉴 | 不直接照搬 |
| --- | --- | --- |
| [Collector Architecture](https://opentelemetry.io/docs/collector/architecture/) | 概念清晰、组件职责、扩展机制与 Pipeline 解释 | Control Plane 并不是 Receiver–Processor–Exporter 数据处理管道 |
| [Collector Internal Architecture](https://github.com/open-telemetry/opentelemetry-collector/blob/main/docs/internal-architecture.md) | 启动流程、组件生命周期、实现入口与维护者视角 | 不要求 Registry/Policy/Release 都变为独立服务 |
| [Collector Component Lifecycle](https://github.com/open-telemetry/opentelemetry-collector/blob/main/component/component.go) | 创建、Start、Running、Shutdown 明确区分 | 不把 Agent 健康和 Gateway 会话生命周期混为一谈 |
| [Java Agent Structure](https://github.com/open-telemetry/opentelemetry-java-instrumentation/blob/main/docs/contributing/javaagent-structure.md) | ClassLoader 隔离和 SDK 初始化的真实边界 | 不让 Go Control Plane 依赖 ByteBuddy 或 Java Sampler 内部类型 |
| [Java Agent Extensions](https://opentelemetry.io/docs/zero-code/java/agent/extensions/) | SPI/自定义发行版打包与配置装配 | 不假定修改启动参数等于运行时热更新 |
| [OpAMP Specification](https://github.com/open-telemetry/opamp-spec/blob/main/specification.md) | 消息字段、能力协商、状态压缩和应用回执 | 不把平台 Rollout、Policy DSL 和审计状态称为协议原生功能 |

## 12. 源码地图：从哪里开始开发

### 12.1 当前 P1.1 实现代码

| 路径 | 可直接阅读的内容 | 状态 |
| --- | --- | --- |
| [`cmd/server/main.go`](../../cmd/server/main.go) | OpAMP Server.Attach、OnConnecting、OnMessage、运行时 Session 和 Workspace-scoped 管理路由 | 已实现 P1.1 |
| [`cmd/server/main_test.go`](../../cmd/server/main_test.go) | UID、鉴权、采样配置分配与 ACK Hash 测试 | 已有单元测试 |
| [`cmd/testagent/main.go`](../../cmd/testagent/main.go) | HTTP 轮询测试 Client，验证 JSON 并回报 APPLIED | 测试模拟，不是真实 SDK |
| [`docs/architecture.md`](../architecture.md) | P0 协议、认证和持久化边界 | 已有事实说明 |
| [`docs/requirements/APM_CONTROL_PLANE.md`](../requirements/APM_CONTROL_PLANE.md) | 需求和验收准则 | 产品需求草案 |
| [`docs/.engineering/specs.lock.json`](../.engineering/specs.lock.json) | 锁定的工程规范 | RF 管理 |
| [`docs/design-docs/dd-001_opamp-control-plane.md`](../design-docs/dd-001_opamp-control-plane.md) | 架构 Design 和开放取舍 | Draft，未批准 |
| [`docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md`](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) | P1–P3 任务、阻塞项、验收 | Blocked |

### 12.2 Go Package 目标布局（按需求增量演进）

```text
cmd/
  server/                      [已有] 进程入口和路由、Gateway 装配
  testagent/                   [已有] OpAMP HTTP 测试 Agent
internal/
  appconfig/                   [已有] Koanf 启动配置
  auth/                        [已有] Workspace Credential / Principal
  storage/
    gormrepo/                  [已有] 统一 GORM Repository，PG/MySQL Dialector
  registry/                    [待拆] Agent Instance、Observation
  opamp/                       [待拆] OpAMP Handler、HTTP/WS Session
  config/                      [P2] ConfigRevision、Java AgentConfigAdapter
  release/                     [P2] Release、Target、审计和回滚
  reconcile/                   [P2] Desired/Observed 对账与重试
  policy/                      [P2 后半] Policy Revision、Selector
  api/                         [按需迁出] Management HTTP API
db/migrations/
  postgres/                    [已有 00001]，P2 增量 Schema
  mysql/                       [已有 00001]，P2 增量 Schema
```

具体代码所有权、跨包调用方向、P2.1–P3 验收及当前内存 Demo 的迁移方法见 [核心模块与职责边界](modules.md)。**不要把目标目录当作已存在的文件，也不要仅为分包进行全量重构**。目前 `cmd/server/main.go` 仍包含 Registry、Gateway 与 HTTP API 的部分职责；只有实际 P2 开发需要时再作局部迁移。

## 13. 本地验证和贡献者交接

### 13.1 复现 Workspace-scoped P1.1

Go 1.25+、本地 PostgreSQL 或 MySQL。先按 [P1.1 运行手册](p1-durable-inventory.md) 执行对应的 Goose 初始化 Migration，再使用受信的 Workspace Credential JSON 配置启动 Server。下面只展示认证变量部分：

```bash
export APM_AGENT_CREDENTIALS_JSON='[{"id":"dev-agent","workspace_id":"dev-workspace","token":"replace-with-a-long-random-agent-token"}]'
export APM_ADMIN_CREDENTIALS_JSON='[{"id":"dev-admin","workspace_id":"dev-workspace","token":"replace-with-a-different-long-admin-token"}]'
# 另需配置 APM_DB_DRIVER 和 APM_DATABASE_DSN，并执行对应 Goose migration
go run ./cmd/server
```

`cmd/testagent` 的客户端 Bearer Token 仍通过 `APM_OPAMP_TOKEN` 设置；**该变量不会为服务端创建一个默认 Workspace**。测试客户端与 Admin 请求必须各自使用正确的 Token。服务端只信任认证后的 Workspace 作用域，不会从 AgentDescription 获取授权。

完整初始化、运行与查询命令以 [P1.1 运行手册](p1-durable-inventory.md) 为准。P0 采样 JSON 模拟回执仍不是 JVM Sampler 热更新证明。

### 13.2 代码与 RF 校验

```bash
go test -race ./...
go vet ./...

# 在开发机器已安装 RepoFoundryAI CLI 后
repofoundry --repo . validate
repofoundry --repo . spec validate
```

若任务需要新增 API、存储约束或安全模型，先核对 [DD-001](../design-docs/dd-001_opamp-control-plane.md) 的当前状态和 [EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 的对应验收任务，不以这份开发者说明代替审查或更新。

建议的验证矩阵：

| 变更 | 最低验证 |
| --- | --- |
| Registry/Identity | UID 冲突、HTTP/WS 活跃度、能力更新、授权负向测试 |
| Config Adapter | Schema 兼容、canonical hash、Unsupported、拒绝未知字段 |
| Reconciler | 旧 ACK、重复 ACK、Agent 断线、DB 重启、连续版本发布 |
| Rollout | 目标冻结、失败阈值、回滚新事件、并发发布冲突 |
| Storage | PostgreSQL/MySQL 分方言 Migration、事务/CAS、幂等、重启恢复、并发写入及 MySQL DDL 隐式提交恢复 |
| Java Runtime（后续） | 真正 JVM 不重启时 Sampler 更新、ParentBased 传播与采样观测 |

## 14. 当前未解决的问题

以下问题影响将来的设计形态，必须保留其未知状态：

- **认证身份**：Scoped Token 与 mTLS 的最终选择；重复 `instance_uid` 和会话换绑的所有权规则。
- **配置发布归属**：Agent 本地配置与平台 RemoteConfig 的优先级；Collector 已由其他控制器管理时如何避免双控制器。
- **动态成员**：Service Selector 发布后，新 Agent 的继承和已有 Release 目标冻结如何在数据模型中实现。
- **运行时差异**：Java/Go/Python/Collector 对动态参数、需要重启的参数分别有哪些实际支持。
- **一致性等级**：单 Gateway 到多副本后，如何管理活动 Session、数据库 CAS、消息转发与有界重试。
- **容量目标**：预期实例数、连接比例、发布规模、延迟、可用性和资源预算尚未有经过授权的数值。
- **采样语义**：跨进程 ParentBased 传播、头采样与尾采样的组合，以及数据面验证方法。

数据库层新增架构要求（2026-10-19）：支持多种关系型 Driver，至少 PostgreSQL 与 MySQL；公共 Domain 只看 Repository/UnitOfWork Port，具体 SQL 和 Migration 分方言实现。详见 [P1–P3 技术选型](technology-selection.md)，该方案尚未通过双数据库真实 PoC。

这些问题应通过 RepoFoundry 的 Research、Design 和必要的 proposed ADR 逐步解决。**在 Design Owner 批准之前，本文只描述目标方案和可检验的工程约束；不能把这些问题当成已解决。**

## 参考资料与维护规则

本文主要参考了 OpenTelemetry 社区“用户可见概念 + 贡献者内部设计”两条文档路径：

1. [OpenTelemetry Collector — Architecture](https://opentelemetry.io/docs/collector/architecture/)
2. [Collector — Internal Architecture](https://github.com/open-telemetry/opentelemetry-collector/blob/main/docs/internal-architecture.md)
3. [Collector — Component Lifecycle](https://github.com/open-telemetry/opentelemetry-collector/blob/main/component/component.go)
4. [OpenTelemetry Java Agent — Structure](https://github.com/open-telemetry/opentelemetry-java-instrumentation/blob/main/docs/contributing/javaagent-structure.md)
5. [OpenTelemetry Java Agent — Extensions](https://opentelemetry.io/docs/zero-code/java/agent/extensions/)
6. [OpenTelemetry Java Agent — Configuration](https://opentelemetry.io/docs/zero-code/java/agent/configuration/)
7. [OpAMP — Specification](https://github.com/open-telemetry/opamp-spec/blob/main/specification.md)
8. [OpAMP — OpenTelemetry Guidelines](https://github.com/open-telemetry/opamp-spec/blob/main/opentelemetry-guidelines.md)
9. [OpenTelemetry Java Contrib — OpAMP Client](https://github.com/open-telemetry/opentelemetry-java-contrib/tree/main/opamp-client)

维护原则：**代码证明“现状”，DD-001/ADR 保存经过相应生命周期治理的设计和决定，EP-001 保存实施/验收，本文负责解释开发者如何理解和延展它们**。当某项实际实现改变本文的流程或源码映射时，应在同一个 PR 中同步修正文档；不要创建两份彼此独立的规范性状态机。
