# P3.2：策略目标冻结与显式暂停恢复

> 增量实现合同，继承 P3.1 的有界冻结和 ACK 验收；不替代仍处于 Draft 的 [DD-001](../design-docs/dd-001_opamp-control-plane.md)，也不解除 [EP-001](../exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) 的身份安全阻塞。

## 1. 策略选择与冻结

新增 `POST /api/v1/policy-rollouts`，从已认证 Admin Credential 获取 Workspace，输入：

```json
{"policy_revision_id":"UUID","environment":"prod","service_name":"checkout","batch_size":1}
```

`environment`、`service_name` 可省略，但它们只筛选可信 Workspace 内 Agent *报告* 的 Inventory 属性，不可用于扩权。服务端事务内读取该 Workspace 的 Java Agent 候选集合，按 UID 排序并锁行；**最多 16 个候选，超过即返回 400，不截断**。统一读取一次当前有效 Binding 集合，再对每个候选按 P2.4 的 Scope/Selector/Priority 求解，只冻结最终命中请求指定 PolicyRevision 的 Agent。最终必须命中 2–16 个兼容目标，`batch_size` 不超过命中数量。不属于当前 Revision 的更高优先级覆盖规则不会被下发；有同级内容冲突时拒绝发布。

单事务创建一份 Java ConfigRevision（带 `source_policy_revision_id`），复用 P3.1 的完整 Release/Target/Assignment/Event 冻结。Release 记录 `policy_revision_id` 和实际选中的 `instance_uids`，并将初始请求中的过滤条件一起存入 `target_selector_json`。后续 Inventory 新增 Agent 或 Binding 更新不会改变已经冻结的成员；网络唤醒仅发生在事务成功提交后。

`Idempotency-Key` 的请求 Hash 由策略 Revision、过滤条件、BatchSize、Actor 和发布模式确定，重试读取**原发布快照**，不重新扫描可能变化的 Inventory，也不重复创建 ConfigRevision。并发相同 Key 在唯一键竞争失败后回滚，再读取已提交 Release；相同 Hash 返回原发布，不同 Hash 返回 409。编译/冻结/审计任一失败时整个外层事务回滚。本实现复用 GORM 的嵌套事务 Savepoint 与 P3.1 代码，不新增数据表、Goose Migration 或独立队列。

**一致性边界**：已锁定的候选 Agent 元数据与现有 Binding 在提交前得到相应锁保护；新 Agent 或 PostgreSQL 中在匹配读取后插入的更高优先级 Binding 对下一次发布生效。MySQL InnoDB `FOR SHARE` 的范围/间隙锁可能阻塞新绑定插入。此增量不声称部署了可串行化的全 Workspace 全局选择器，也不支持海量 Target。

## 2. 受控恢复

新增 `POST /api/v1/rollouts/{release_id}/resume`，Body 必须指定 GET 当前返回的 `row_version`：

```json
{"row_version":2}
```

- 仅对 `PAUSED` 的 P3 Rollout 生效，使用 Release `row_version` CAS，失败返回 409；跨 Workspace 不泄露。
- 所有已激活 Target 必须为 `APPLIED`，并由当前 Assignment 的相同 ReleaseID、Generation、APPLIED 状态证实；否则不能恢复。
- 所有未激活 Target 必须保留 `QUEUED` 和原先冻结的预期 Generation。已 `SUPERSEDED` / `UNSUPPORTED` 的目标**不能被原地覆盖或复活**，需要新建 Release。
- 成功后仅把 Release 改为 `RUNNING`（若全部目标已经 APPLIED，则为 `COMPLETED`），清除结束时间、增加 RowVersion、追加 `ROLLOUT_RESUMED` 审计事件。恢复不直接改 Assignment、不给 Agent 发送网络配置；下一批仍须显式调用现有 `/advance`。
- 同样的 `row_version` 重复恢复失败，不会重复发配置。晚到的匹配 `APPLIED` 回执只能更新 Target，不能自动解除 `PAUSED`。

## 3. API 与状态

| 方法 | 路径 | 成功 | 失败 |
| --- | --- | --- | --- |
| POST | `/api/v1/policy-rollouts` | 202，冻结的 Policy Release 与首批激活 UID | 400 边界/候选过多，404 PolicyRevision 不存在，409 同级冲突，422 不兼容 Target |
| POST | `/api/v1/rollouts/{release_id}/resume` | 200，新的 RUNNING / COMPLETED 快照 | 400 无效 RowVersion，404 不存在，409 已阻断、状态或 CAS 冲突 |

现有 `GET /api/v1/rollouts/{release_id}` 和 `POST /api/v1/rollouts/{release_id}/advance` 继续使用同一状态机。P3.1 直接指定 UID 的 Rollout 行为及 P2 的单 Agent 发布不变。

## 4. 验证与明确未实现

真实 PostgreSQL 17/MySQL 8.4 合同必须验证：按绑定正确冻结、Instance 覆盖排除、2–16 上限、跨 Workspace 拒绝、Policy 来源、幂等回放、初始未激活 Target 无 Desired、失败后的粘性 PAUSED、ACK 后 RowVersion CAS 恢复、再手动 Advance、最终 ACK 收敛；同时执行 Go race/vet、严格 gofmt 和 RepoFoundry。

P3.2 不实现自动推进、百分比灰度、失败 Target 的原地强制重试、历史回滚编排、集群 Session Fencing、生产 IAM 或 Java Runtime 真实采样回调的验证。
