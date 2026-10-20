# P2.3：持久 Desired → OpAMP 投递 → ACK 收敛

> 状态：P2.3 增量实现说明；不取代 DD-001 的 Draft 或 EP-001 的身份安全阻塞。本文件描述当前单 Agent Java RemoteConfig 的可执行范围和待实现边界，不批准生产身份认证或多实例 HA 语义。

## 1. 目标与边界

基于 P2.2 已提交的 `config_revisions`、`config_releases`、`release_targets`、`agent_assignments` 和 `config_events`，让已经建立可信 Workspace/Agent 凭证绑定的 Java Agent 从 OpAMP HTTP 轮询或 WS 消息响应中获取完整 ConfigMap，并以报告的 RemoteConfigStatus 和 Hash 收敛当前 Assignment。

本增量不增加物理表、消息队列或独立进程。只有已经发布到指定 `workspace_id + instance_uid` 的 Revision 可以下发。发送响应不表示接收、应用或真实 JVM 动态采样成功。

## 2. 执行顺序

```mermaid
sequenceDiagram
    participant Admin
    participant DB as PostgreSQL/MySQL
    participant GW as OpAMP Gateway
    participant Agent as Java Agent

    Admin->>DB: P2.2 事务：写 Release/Target/Desired/Event
    Agent->>GW: AgentToServer + Capabilities + RemoteConfigStatus
    GW->>DB: Observe：认证 Credential → UID 归属
    GW->>DB: Reconcile：锁 Agent → Assignment
    DB->>DB: 匹配 ACK Hash + CAS 更新 Observed / Audit
    DB->>DB: 读取 Revision，校验 Kind/Capability/Hash
    DB->>DB: 记录 CONFIG_OFFERED + WAITING_ACK，提交
    GW-->>Agent: ServerToAgent.RemoteConfig（完整 ConfigMap）
    Agent->>GW: 后续轮询/WS 消息：APPLIED + Hash
    GW->>DB: 只有当前 Desired Hash 匹配且曾被提供才标记 APPLIED
```

1. 无 Assignment 的心跳只做一次存在性查询，不写审计、不打开协调事务。
2. 有 Assignment 时遵循发布路径的 Agent → Assignment 加锁顺序，防止并发发布与 ACK 的锁反转。
3. 仅当 Observed Hash/Status 或业务状态发生改变，才更新 `row_version`、`observed_at`，并在同事务追加事件。
4. 已经 `APPLIED`、`FAILED`、`UNSUPPORTED` 的当前版本不重发；`WAITING_ACK` 每 15 秒允许下一次 Agent 消息触发重复提供。
5. `CONFIG_OFFERED` 和 `last_sent_at` 表示网关准备响应配置的尝试，不保证网络发送成功。真实响应发送发生在事务提交之后。
6. 单 Agent Target/Release 在回执收敛时更新为 `APPLIED` 或 `FAILED`；每次状态写入均通过 Generation/RowVersion CAS 守护当前 Assignment。

## 3. Hash 和事件合同

| Agent 回执 | 当前状态处理 | 审计事实 |
| --- | --- | --- |
| 当前 Hash + APPLIED，且当前版本曾提供 | `APPLIED` | `CONFIG_APPLIED` |
| 当前 Hash + FAILED，且当前版本曾提供 | `FAILED` | `CONFIG_FAILED` |
| 当前 Hash + APPLYING，且当前版本曾提供 | `APPLYING` | `CONFIG_APPLYING` |
| 旧 Hash 或未曾提供的 Hash | 当前 Pending 不冒认成功；保留 Observed Hash/Status | `CONFIG_ACK_STALE` |
| 没有新回执但等待期限结束 | 再次提供完整 ConfigMap，`WAITING_ACK` | `CONFIG_OFFERED` |
| AgentKind、Dialect、Distribution 或 Capability 不兼容 | `UNSUPPORTED`，不返回 ConfigMap | `CONFIG_UNSUPPORTED` |

**相同 Hash、不同 Release**：当前 Hash 匹配可以证明内容状态，不能证明新 Release 发生了独立的 JVM 应用动作。Agent 回执不携带服务端 Generation，Generation 只用于数据库 CAS。

ConfigMap 内容使用 P2.1 确定性 protobuf Bytes。投递前以 SHA-256 重新验证 `config_hash` 与 Assignment DesiredHash 一致，禁止把不一致内容发送给 Agent。

## 4. 恢复和后续工作

- **HTTP**：每次轮询读取数据库 Desired，不依赖进程内待发队列；进程重启后可以继续。
- **WebSocket**：本增量在 Agent 发来消息时可以经 OnMessage 返回 RemoteConfig。**Admin 发布后立即主动 WS 推送尚未实现**。该路径需要统一 Writer / 发送顺序控制，避免与上游 OnMessage 自动回复竞争。
- **离线 Agent**：Desired 持久保留；不会在离线期间伪造送达。下一次 Agent 消息触发协调。
- **重试**：当前使用固定 15 秒重试期限、依赖下一次 Agent 消息；没有周期扫描、可观测度量或 WS 唤醒队列。终态 FAILED/UNSUPPORTED 需要新发布。
- **平台安全**：继续使用现有静态配置绑定的 Workspace/Agent Token；未解决 DD-001、EP-001 所跟踪的正式 IAM 与稳定 Principal 阻塞。
- **状态查询**：Assignment 是 P2.3 当前状态事实源，P0 `PUT /sampling` 与旧 Agent List 视图仍属于兼容演示，不承诺展示持久 Desired。

## 5. 验证

要求 PR Head 上严格 gofmt、go test -race、go vet、RepoFoundry 校验通过；真实 PostgreSQL 17 / MySQL 8.4 运行 OpAMP HTTP 端到端用例，验证发布后响应完整 Hash/ConfigMap、等待 ACK 不重复提供、相同 Hash APPLIED 收敛、新发布旧 ACK 不污染 Generation、跨 Workspace 查询不泄露。

不能以这些测试替代 WS 主动推送、网络断开、实际 JVM 采样与生产身份/HA 的验收。
