# EP-001 收尾证据核对（只读审计交接）

> 基线：`main@e8359037d2be0c09a6983ea1bbade7314fb127c4`（2026-10-25 核对）。
> **本文件不是正式 EP-001 验收记录，不变更 ExecPlan 元数据、Checklist、Checkpoint、Design 或安全审批。**
> 证据级别：Merged = GitHub 已确认合并；CI link = 可追溯历史 Workflow，尚需对最新 main 重跑；Accepted = 治理状态及证据 Pin 已核对；Unverified = 尚无满足 EP 门禁的直接证据。

## 1. 已合并实现与证据索引

| 阶段 | PR（已核对 merged） | 对应能力 | 验收状态 |
| --- | --- | --- | --- |
| P1.1 | [#3](https://github.com/XiaoWeiKIN/apm-control-plane/pull/3) | Workspace-scoped Durable Agent Inventory / PostgreSQL / MySQL | Merged；仍需 EP M1 的独立重启与真实 Agent 证据 |
| P2.1 | [#8](https://github.com/XiaoWeiKIN/apm-control-plane/pull/8) | Immutable Java Sampling ConfigRevision | Merged |
| P2.2 | [#9](https://github.com/XiaoWeiKIN/apm-control-plane/pull/9) | Atomic single-Agent Release / Target / Assignment / Event | Merged |
| P2.3 | [#12](https://github.com/XiaoWeiKIN/apm-control-plane/pull/12) | WS reliability / ReportFullState / restart | Merged；[#12 Go](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39429412733)、[DB](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39429412756)、[RF](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39429412794) |
| P2.4 | [#14](https://github.com/XiaoWeiKIN/apm-control-plane/pull/14) | Policy / immutable Revision / Binding / Selector | Merged |
| P2.5 | [#15](https://github.com/XiaoWeiKIN/apm-control-plane/pull/15)、[#16](https://github.com/XiaoWeiKIN/apm-control-plane/pull/16) | Policy-sourced atomic Release and preview/locking fixes | Merged；[#16 Go](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39888574278)、[DB](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39888574198)、[RF](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39888574192) |
| P3.1 | [#17](https://github.com/XiaoWeiKIN/apm-control-plane/pull/17) | Frozen multi-Agent targets and staged ACK advancement | Merged；[#17 Go](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39797945042)、[DB](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39797945541)、[RF](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39797944924) |
| P3.2 | [#18](https://github.com/XiaoWeiKIN/apm-control-plane/pull/18) | Policy-selected frozen rollout and guarded Resume | Merged；[#18 Go](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39893971623)、[DB](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39893971626)、[RF](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39893971595) |
| P3.3 | [#19](https://github.com/XiaoWeiKIN/apm-control-plane/pull/19) | Explicit compensating Release, provenance, audit | Merged；[#19 Go](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39905652364)、[DB](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39905652388)、[RF](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/39905652521) |

历史 Workflow 仅对应当时 PR Head；**不能替代**对当前 `main` 的端到端验收、Benchmark 或安全审核。各 PR 的真实测试覆盖范围应由审计人员读取 Workflow/Job Log 和测试源码再确认。

## 2. 治理门禁

| 门禁 | 已核对事实 | 剩余行动 |
| --- | --- | --- |
| DD-001 / BLK-001 | `status: current`、`published_revision: 1`；批准记录 [1038ffeb](https://github.com/XiaoWeiKIN/apm-control-plane/commit/1038ffebd1ea694c0612af460c7f0643190fecd0)；Design Evidence Pin `DD-001@rev:1@sha256:b379e90d2d7c98dc81b5f50e548c8e2c978533539d4dcd04a4e15028deea24b2`；[RepoFoundry run 40179601964](https://github.com/XiaoWeiKIN/apm-control-plane/actions/runs/40179601964) | 保留已批准设计证据；不得重新标记 Draft |
| BLK-002 | EP 中仍为 `open`；现有身份采用静态 Workspace-scoped Credential，未发现正式生产身份安全方案的批准证据；`docs/SECURITY.md` 仍是 bootstrap scaffold | 根据 [BLK-002 决策与验收材料](ep-001-blk002-security-decision-packet.md)，由 Security/Design Owner 明确可信 Principal、UID 归属、轮换/撤销和 HTTP/WS/TLS 边界；落实批准后的负向测试 |
| EP-001 | `status: blocked`，`verified_revision` 与 `verification_evidence` 为空；M1–M3 未完成正式验收 | 在最新 main 上执行独立合同与恢复验证，通过规范 CLI 更新进度、Checkpoint 和验收 |
| Benchmark | EP 说明规模/延迟测量应预声明，但 `required_benchmark_scenarios: []`：尚未登记强制 BS 验收门禁，亦无已封存 Run | Owner 先决定本 EP 是否需要正式 Benchmark 场景并明确门槛；如决定纳入验收，再用规范工具先创建 BS、后运行和封存 BR。不得自动将候选数值当作 EP 完成门槛 |

## 3. 待 Design/Security Owner 决定

- **身份权威**：Admin 与 Agent Principal 如何从可信平台 IAM/注册凭证映射到 `workspace_id`，Agent 自报标签不授予权限。
- **UID 所有权**：首次绑定、同 Workspace 重复 UID、跨 Workspace UID 重用、失窃凭证重新绑定的拒绝/审批路径。
- **凭证生命周期**：短期凭证、轮换、撤销、重连、HTTP/WS 连接续期及审计。
- **生产传输**：TLS/mTLS、终止代理的信任边界、跨节点会话和重放防护；不得把开发 Token 测试等同生产验收。

以上均是待审批问题，不在本 PR 里代替 Owner 作出决定。

## 4. 正式 EP 收尾建议顺序

1. 审计人员以最新 main SHA 检查 P1–P3.3 的具体接口/事务/回滚/ACK/双库测试证据，标记 Verified / Partially Verified / Unverified。
2. 使用 RepoFoundry `epctl` 的规范流程修正过期的 `Current Snapshot`、`Next action` 和 M1–M3 验收矩阵，保留历史记录和 blocked 状态；不手改受治理元数据。
3. 由 Owner 对 BLK-002 作出明确安全设计决定；实现与测试独立 PR，不以批准 DD-001 自动解除 BLK-002。
4. Owner 先决定 Benchmark 是否成为 EP 完成门禁；需要正式测量时，在测量前预声明 Scenario、固定环境/负载/阈值，并保存真实 PG/MySQL 样本、失败和无效 Run。
5. 只有全部验收及安全门禁均具备证据后，才通过 RepoFoundry 正式完成并归档 EP-001。

## 5. 本轮不作的声明

未在本 PR 运行 Benchmark；未对最新 main 执行全部业务合同；未完成生产 IAM/Agent Principal/UID 安全验收；未创建或批准 ADR；未修改 EP-001 状态；未宣称 EP 已完成。
