# EP-001 / BLK-002：生产身份与授权决策材料（待审批）

> **状态：待 Design/Security Owner 评审的决策输入，不是已接受 ADR、已批准安全方案或已实现的生产控制。**
> 核对基线：`main@e8359037d2be0c09a6983ea1bbade7314fb127c4`（2026-10-26）。
> 对应治理源：[EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 中的 `BLK-002: open`；
> [DD-001 Revision 1](../design-docs/dd-001_opamp-control-plane.md) 已批准，但其批准不覆盖生产身份方案的独立决策。
> 本材料只列已检查的实现事实、待决定的方案和验收条件，不代表安全审计已通过。

## 1. 已确认的现状

| 边界 | 已检查的代码事实 | 尚不能据此声称 |
| --- | --- | --- |
| Credential → Workspace | `internal/appconfig/config.go` 从 `APM_AGENT_CREDENTIALS_JSON`、`APM_ADMIN_CREDENTIALS_JSON` 加载静态 `id/workspace_id/token`；启动时校验唯一 Credential ID/Token，并拒绝未知 JSON 字段 | 已接入可观测平台 IAM，具备在线撤销、动态授权或凭证轮换契约 |
| HTTP Bearer 认证 | `internal/auth/auth.go` 哈希并使用常数时间比较匹配静态 Token，返回 `Principal{WorkspaceID, CredentialID}`；不采信 Agent Description 里的身份自报 | 动态 Access Token、权限范围、Token 有效期、签名颁发方或在线撤销已经生效 |
| Agent UID 归属 | `internal/storage/gormrepo/store.go` 以 Workspace + UID 存 Agent，现有归属字段是 `credential_id`，不同凭证绑定冲突需明确处理 | 已有与 Credential 生命周期解耦的稳定 Agent Principal、UID 转移与轮换流程 |
| OpAMP 会话 | `cmd/server/main.go` 在 `OnConnecting` 认证，再通过连接回调处理 HTTP/WS；活跃 WS Session 保留在进程内 | 已证实 WS 长连接按凭证撤销即时失效、持续鉴权或跨节点 Session Fencing |
| 生产安全规范 | `docs/SECURITY.md` 仍是 `bootstrap scaffold`，`Last verified: unknown` | 有已完成的生产安全边界说明及发布验收 |

上述为源码结构事实；具体攻击可利用性和生产部署行为仍须以威胁建模、运行环境、测试证据核实。静态 Token 机制当前只可视为已验证开发边界。

## 2. 需要 Owner 明确批准的决策

| 决策 | 可选方向（仅供评审） | 必须明确的接口/失败语义 |
| --- | --- | --- |
| 权威 Workspace/IAM | A. 由受信任的平台身份服务颁发短期 Workspace/角色凭证；B. 由受信任网关认证并向内部服务传递不可伪造 Principal | 凭证颁发方、`workspace_id` 的权威来源、Agent 与 Admin 权限、过期/不可用时拒绝还是继续 |
| Agent Principal 与 UID 归属 | A. 引入稳定、独立于轮换 Credential 的 Agent Principal；B. 继续 Credential 绑定，但明确只支持停机重新注册/受控迁移 | 首次绑定、同 Workspace/跨 Workspace 重复 UID、Credential 轮换、冒领与显式转移的权限及审计 |
| 生产传输 | A. 受信任入口 TLS + 端到端身份上下文；B. mTLS 工作负载/Agent 证书绑定身份 | TLS 终止边界、受信任代理、证书/Token 生命周期、出错是否 Fail Closed |
| 撤销与长连接 | 颁发短期凭证并要求有效期内重新连接，或建立实时撤销/会话失效事件机制 | 被撤销 Agent/管理员的 HTTP/WS 具体生效期限、重连/并发会话、可观察测试 |
| 审计与隐私 | 复用 `config_events` 或批准单独安全事件存储；由合规/安全负责人确定保留策略 | 身份变更、换绑、拒绝访问、撤销/轮换所需事件；不得记录原始 Token/秘密 |

**决策门禁**：由 Design/Security Owner 明确选择上述实际方案并给出审查记录；若涉及长期身份模型、公开协议或不可逆迁移，先创建 *proposed* ADR，通过正式 `decide-adr` 审批后再实施。不得从该表自动推定方案 A/B 已批准。

## 3. 解除 BLK-002 所需验证

以下是待评审的最小验收矩阵。是否足以解除阻塞，由 Owner 根据获批准的身份方案确认。

| 场景 | 预期可观察结果 | 证据要求 |
| --- | --- | --- |
| Admin 无凭证、无权限、跨 Workspace | REST 创建/查询/发布/回滚均拒绝；不能读取其他 Workspace 的 ConfigRevision、Agent 或事件 | 真 HTTP 管理 API 负向用例，PostgreSQL 17 与 MySQL 8.4 |
| Agent 伪造 Workspace、Principal 或其他 UID | 自报标签不扩权，重复/冒领 UID 不能接管现有 Assignment 或 WS Session | 真实 OpAMP HTTP/WS 连接 + DB 所有权断言 |
| Credential 轮换 | 允许已授权的同 Principal 继续操作；旧 Credential 按批准的时间规则失效，不重复创建不同身份 | 轮换前/中/后端到端用例、Agent 断线重连 |
| 凭证撤销 | 后续 HTTP 被拒绝，现有 WS 在批准的撤销传播期限内失效，不能继续接收 RemoteConfig | 持续连接中的撤销事件与观察日志；审计事件和误投递断言 |
| 认证/授权数据源故障 | 按批准规则 Fail Closed，不静默退回全局 Token 或自报身份 | 身份服务网络故障/超时注入及负向检查 |
| 多 Workspace 同时发布 | 同 UID/不同 UID 并发发布、ACK、Rollback 不跨作用域；幂等键绑定真实 Workspace | 并发真实 PG/MySQL 合同测试 |
| 生产 TLS/代理边界 | 明文/伪造可信代理头/错误客户端身份不越过认证边界 | 经实际部署入口的握手及拒绝日志 |
| 合规日志 | 不泄露 Token、ConfigMap 秘密；身份变更可归因并按批准策略保留 | 日志检查、事件存储断言和密钥扫描 |

目前上述矩阵是**验收计划**，不是已执行测试的清单。已有 P1–P3 的静态凭证负向用例不能代替身份颁发、撤销和实际 TLS 的生产验收。

## 4. 推荐的最小实施顺序（未授权执行）

1. **安全决定**：Design/Security Owner 批准 Principal 来源、身份模型和撤销语义。必要时 RepoFoundry ADR 提案 → 审批。
2. **身份边界**：单独 PR 落地可信 Workspace/Role 认证和稳定 Agent Principal，继续拒绝自报身份扩权；不可破坏既有测试 Agent 契约而不说明迁移。
3. **凭证生命周期**：单独 PR 验证轮换、撤销、WS 失效和失败模式，明确兼容期及旧数据迁移要求。
4. **负向验收**：使用受控的真实 PG/MySQL + HTTP/WS + 部署入口证据核实审批契约；更新正式 `docs/SECURITY.md` 而非保留脚手架。
5. **治理闭环**：Owner 按 RepoFoundry 官方流程解除 BLK-002，使用完成证据更新 EP-001；在此之前 `EP-001 status: blocked` 不变。

## 5. 证据限制

本轮只检查仓库代码、文档与 GitHub 历史；未建立平台 IAM、未执行撤销/生产 TLS 实验、未创建或批准安全 ADR、未手工修改 EP-001 的状态。不得将本文件标记为安全认证、正式设计决定或生产上线许可。
