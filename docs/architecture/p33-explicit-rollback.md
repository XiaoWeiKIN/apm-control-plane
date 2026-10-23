# P3.3 显式补偿发布与回滚审计

> DD-001 仍为 Draft，EP-001 的生产身份安全阻塞仍有效。该功能不声称自动恢复真实 JVM 的历史状态。

## 接口与语义

`POST /api/v1/rollouts/{release_id}/rollback` 使用当前 Admin Bearer 对应的 `workspace_id`，请求示例：

```json
{"config_revision_id":"已存在的配置 UUID","row_version":2,"batch_size":1}
```

可带 `Idempotency-Key`。这是一项**显式补偿发布**：管理员指定要重新下发的不可变 ConfigRevision，服务端不会猜测之前哪一个配置代表安全回滚，也不会直接修改原 Release 或原 ConfigRevision。

- 从源 Release 的冻结 Target 集合读取全部 Agent UID（2–16 个）；不根据当前 Policy/Inventory 重新枚举，不改变原目标集合。
- 在新事务里按 UID 顺序锁 Agent，验证源 Release `row_version` CAS、原冻结 UID 列表与请求一致、目标 ConfigRevision 仍存在且兼容全部 Agent。所选 ConfigRevision 必须与源 Release 的冻结 ConfigRevision 不同。
- 新建一个独立 `config_releases`，设置 `rollback_of_release_id` 指向源 Release；原 Release 历史与其冻结目标不删除。新 Release 使用 P3.1 的 Batch/Target/Assignment CAS、ACK 与失败暂停规则。
- 同事务追加 `ROLLBACK_REQUESTED` 事件，`details_json` 记录源 Release ID 和明确选择的 ConfigRevision ID。事务任一环节失败全部回滚。
- 首批成功提交后才通知在线 WS；后续仍需显式 `/advance`。HTTP 202 表示 PENDING/RUNNING，不表示 Agent 已应用；只有匹配 Hash 的 ACK 才能完成回滚。
- `Idempotency-Key` 仍与其他发布入口共享 Workspace 唯一性。相同源 Release、RowVersion、ConfigRevision、BatchSize、Actor 和冻结目标返回原补偿 Release，不再次推进 Generation；同 Key 不同请求返回 409。

## 不支持

不推断“回滚到上一个成功配置”；不提供原地覆盖旧 Release、自动恢复失败目标、自动推进、跨节点事务、百分比灰度或生产 IAM。旧目标可能因为新的补偿发布被标记为 `SUPERSEDED`，这属于 P3.1 既有历史状态更新与审计语义，而非改写原始冻结目标集合。

## 验证

真实 PostgreSQL 17 / MySQL 8.4 验证 Workspace 隔离、源 Release CAS、显式 ConfigRevision 不同、目标集合冻结、独立来源链接、幂等回放、逐批 ACK、审计失败回滚。再运行 Go CI / Race / Vet 和 RepoFoundry Validation。
