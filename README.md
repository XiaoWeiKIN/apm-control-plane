# APM Control Plane

A Go-based **OpAMP Agent Control Plane**. Long-term scope: Java, Go, Python and other APM Agent distributions, with a later path to Collector management. **First deliverable: a custom OpenTelemetry Java Agent**. P0 is a protocol interoperability PoC; branch feat/p1-durable-agent-inventory adds durable multi-database Agent Inventory and scoped credentials, pending PR review.

## Developer architecture documentation

- [**Control Plane 数据库 Schema 设计（P1–P3，待评审）**](docs/architecture/storage-schema.md) — 12 张核心逻辑表、ER 关系、字段/主外键、索引、CAS、审计、跨数据库 Migration。

- [**P1.1 Durable Inventory runbook（中文）**](docs/architecture/p1-durable-inventory.md) — PostgreSQL/MySQL Migration、Scoped Credentials、实例持久化与当前限制。

- [**RF Research R-001：开源项目架构对比**](docs/research/active/r-001_opamp-oss-architecture/RESEARCH.md) — Gitea / Grafana / Casdoor / Collector 的源码证据与迁移边界。

- [**P1–P3 技术选型（中文、待评审）**](docs/architecture/technology-selection.md) — HTTP、配置加载、PostgreSQL/MySQL 多数据库 Driver、GORM、Goose Migration、Telemetry、测试和兼容性选择。

- [**APM Control Plane：开发者架构指南（中文）**](docs/architecture/README.md) — end-to-end control flow, concepts, module contracts, OpAMP messaging, desired/observed state, persistence, security, extension points, and source navigation.
- [DD-001 governed design (draft)](docs/design-docs/dd-001_opamp-control-plane.md) — design authority and unresolved decisions; the developer guide does not imply approval.

## P0 scope

- Official `opamp-go` HTTP/WebSocket server at `/v1/opamp`
- Bearer-authenticated agent connections and a separate administrative token
- In-memory Agent Inventory (instance UID, service, environment, version, last seen)
- OpAMP health and RemoteConfigStatus snapshots
- Per-agent remote sampling-policy JSON with hash-based acknowledgement
- Standalone HTTP polling test agent, Go unit tests, and GitHub Actions CI

**P0 is a protocol interoperability PoC.** There is no durable database, global service-level policy rollout, or real JVM sampling change yet.

## Local startup (P1 feature branch)

Requires Go 1.25+, PostgreSQL or MySQL, and an explicit schema migration before running the service. Production migrations use **Goose**, not GORM AutoMigrate.

```bash
go install github.com/pressly/goose/v3/cmd/goose@v3.27.0

export APM_DB_DRIVER=postgres
export APM_DATABASE_DSN='postgres://app:replace@127.0.0.1:5432/controlplane?sslmode=disable'
goose -dir db/migrations/postgres postgres "$APM_DATABASE_DSN" up

export APM_OPAMP_TOKEN='replace-with-long-random-agent-token'
export APM_ADMIN_TOKEN='replace-with-a-different-long-admin-token'
go run ./cmd/server
```

To use MySQL instead, set `APM_DB_DRIVER=mysql`, supply a MySQL DSN (with `parseTime=true&loc=UTC`) and run `goose -dir db/migrations/mysql mysql "$APM_DATABASE_DSN" up`.

The two legacy token variables are still supported for local P0 compatibility, scoped to a single `default` tenant. For separate tenants, use `APM_AGENT_CREDENTIALS_JSON` and `APM_ADMIN_CREDENTIALS_JSON` with credential id, tenant id and token as described in the [P1 runbook](docs/architecture/p1-durable-inventory.md). Do not expose the development listener or plain HTTP to an untrusted network.

Default listener: `127.0.0.1:4320`. Set `APM_CONFIG_FILE=configs/example.yaml` to load Koanf startup configuration. In another terminal:

```bash
export APM_OPAMP_TOKEN='replace-with-long-random-agent-token'
go run ./cmd/testagent -service checkout
```

The test agent prints its 32-character `instance_uid`; this can now be queried through the tenant-scoped admin API and is retained after a server restart. The P0-only `PUT /api/v1/agents/{uid}/sampling` endpoint remains an **in-memory demo**, not durable P2 policy revision management.

```bash
export APM_ADMIN_TOKEN='replace-with-a-different-long-admin-token'
curl -H "Authorization: Bearer $APM_ADMIN_TOKEN" http://127.0.0.1:4320/api/v1/agents
```

## Routes


| Method | Route | Purpose |
| --- | --- | --- |
| GET | `/healthz` | liveness (public) |
| HTTP/WebSocket | `/v1/opamp` | OpAMP (agent token) |
| GET | `/api/v1/agents` | snapshots (admin token) |
| GET | `/api/v1/agents/{uid}` | instance detail (admin token) |
| PUT | `/api/v1/agents/{uid}/sampling` | sampling rate in [0,1] (admin token) |

## Tests

```bash
go mod tidy
go test -race ./...
go vet ./...
```

See [P1 deployment guide](docs/architecture/p1-durable-inventory.md), [architecture](docs/architecture/README.md), and [P0 limitations](docs/architecture.md).

## Engineering governance (RepoFoundryAI)

This repository is bootstrapped with [RepoFoundryAI v0.15.1](https://github.com/XiaoWeiKIN/RepoFoundryAI/releases/tag/v0.15.1) and the locked [EngineeringSpecifications v1.9.0](https://github.com/XiaoWeiKIN/EngineeringSpecifications/releases/tag/v1.9.0). The versioned project Harness includes Codex, Claude Code and portable adapters. The active governance profile is **adaptive**.

- [Requirements and acceptance criteria](docs/requirements/APM_CONTROL_PLANE.md)
- [Engineering workflow and installation](docs/PROJECT_WORKFLOW.md)
- [Repository guide](AGENTS.md)
- [Engineering specification routing index](docs/agent-guides/managed/index.md)
- [DD-001: Control Plane architecture (draft)](docs/design-docs/dd-001_opamp-control-plane.md)
- [EP-001: durable Control Plane delivery (blocked, review prerequisites)](docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md)
- [Decision index](docs/DECISIONS.md), [design index](docs/DESIGN-DOCS.md), [execution plans](docs/PLANS.md)

A developer installs the CLI and professional Skills into **their own** environment using the official installer (see the engineering workflow). Project manifests, adapters and locked specifications are committed here. PRs automatically validate Harness, Spec and engineering lifecycle state, in addition to Go tests.

## Upstream

- [OpAMP specification](https://github.com/open-telemetry/opamp-spec)
- [opamp-go](https://github.com/open-telemetry/opamp-go), pinned to v0.25.0
