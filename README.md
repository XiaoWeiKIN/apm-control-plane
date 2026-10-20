# APM Control Plane

A Go-based **OpAMP Agent Control Plane** for managing Java, Go and Python APM Agent distributions, with future Collector support. The first operational increment is **durable, workspace-scoped Agent Inventory**; policy revision, reconciler and staged rollout remain future milestones.

## Developer documentation

- [核心模块与职责边界：六个功能模块 + 三个支撑模块](docs/architecture/modules.md) — Gateway、Registry、Policy、Config、Release、Reconciler 的状态归属、依赖关系、Go Package 和 P2 实施顺序。
- [完整数据库设计：Workspace Scope / P1–P3](docs/architecture/storage-schema.md) — 11 张逻辑表、外键、CAS、审计与 Goose Schema。
- [P1.1 Agent Inventory 运行说明](docs/architecture/p1-durable-inventory.md) — Workspace 凭证、双数据库启动、重连与当前限制。
- [总体架构](docs/architecture/README.md)、[技术选型](docs/architecture/technology-selection.md)、[受治理 DD-001（Draft）](docs/design-docs/dd-001_opamp-control-plane.md)。
- [开源架构研究 R-001](docs/research/active/r-001_opamp-oss-architecture/RESEARCH.md)。

## Implemented P1.1

- OpAMP HTTP/WebSocket server at `/v1/opamp`, with credential-based Agent authentication.
- Agent Inventory in PostgreSQL or MySQL via GORM; schema installed separately through Goose.
- A **trusted deployment-configured `workspace_id`** on each Agent/Admin credential. Admin inventory queries and Agent ownership are scoped to the same platform Workspace.
- Instance identity `(workspace_id, instance_uid)`; the credential that first registers the UID retains ownership within that Workspace.
- Service metadata, Agent Kind/Distribution/Version, capabilities, latest health and RemoteConfigStatus snapshots.
- HTTP polling freshness and WebSocket session handling have separate connection semantics.
- The existing per-agent `PUT /sampling` endpoint remains an **ephemeral P0 demonstration**. ConfigRevision, durable Desired State, Release and Reconciler are not yet implemented.

**Workspace means a workspace of the larger observability platform**, shared conceptually with logs, metrics, traces and dashboards. This service does **not** create a separate Tenant table or accept Agent-declared Workspace IDs as permission. Current P1.1 obtains trusted scope from operator-supplied credential configuration; integration with the platform's authoritative Workspace/IAM service is a separate security/design task.

## Local startup

Requires Go 1.25+, PostgreSQL or MySQL and a fresh development database. Because this project has not yet been deployed with persistent production data, the initial `00001_agent_instances.sql` definition uses `workspace_id` directly. **No migration from a previous `tenant_id` schema is provided.**

First run the correct Goose migration:

```bash
go install github.com/pressly/goose/v3/cmd/goose@v3.27.0

export APM_DB_DRIVER=postgres
export APM_DATABASE_DSN='postgres://app:replace@127.0.0.1:5432/controlplane?sslmode=disable'
goose -dir db/migrations/postgres postgres "$APM_DATABASE_DSN" up
```

For MySQL, use `APM_DB_DRIVER=mysql`, a DSN containing `parseTime=true&loc=UTC`, and `goose -dir db/migrations/mysql mysql "$APM_DATABASE_DSN" up`.

In local development, use two distinct high-entropy replacement tokens. The names below are example shell variables; only the credential JSON variables are read by the **server**:

```bash
export APM_OPAMP_TOKEN='replace-with-random-agent-token-at-least-16-chars'
export DEMO_ADMIN_TOKEN='replace-with-different-admin-token-at-least-16-chars'
export APM_AGENT_CREDENTIALS_JSON="$(printf '[{"id":"dev-agent","workspace_id":"dev-workspace","token":"%s"}]' "$APM_OPAMP_TOKEN")"
export APM_ADMIN_CREDENTIALS_JSON="$(printf '[{"id":"dev-admin","workspace_id":"dev-workspace","token":"%s"}]' "$DEMO_ADMIN_TOKEN")"

go run ./cmd/server
```

The server **requires** `APM_AGENT_CREDENTIALS_JSON` and `APM_ADMIN_CREDENTIALS_JSON`; the legacy `APM_OPAMP_TOKEN` / `APM_ADMIN_TOKEN` server-side fallback has been removed. `APM_OPAMP_TOKEN` above is used by the separate `cmd/testagent` client only.

In another terminal, configure the matching Agent token and run:

```bash
export APM_OPAMP_TOKEN='replace-with-random-agent-token-at-least-16-chars'
go run ./cmd/testagent -service checkout
```

Query Inventory with the Admin token:

```bash
curl -H "Authorization: Bearer $DEMO_ADMIN_TOKEN" \
  http://127.0.0.1:4320/api/v1/agents
```

Never commit real secrets. Local plain HTTP is not a public production authentication strategy. Use TLS termination, a secret manager and authoritative Workspace permission checks before production.

## Routes

| Method | Route | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Liveness |
| HTTP/WebSocket | `/v1/opamp` | Credential-authenticated OpAMP |
| GET | `/api/v1/agents` | Workspace-scoped Agent list |
| GET | `/api/v1/agents/{uid}` | Workspace-scoped instance detail |
| PUT | `/api/v1/agents/{uid}/sampling` | Ephemeral demo sampling rate, [0,1] |

## Validation

```bash
go test -race ./...
go vet ./...
```

The [P1 Database Contracts workflow](.github/workflows/p1-database-contracts.yml) runs Goose and the same Repository and OpAMP HTTP/API integration tests on **PostgreSQL 17 and MySQL 8.4**. This proves those test contracts, not production readiness or actual Java SDK sampling.

## Engineering governance

RepoFoundryAI v0.15.1 and EngineeringSpecifications v1.9.0 control project documentation and validation. [DD-001](docs/design-docs/dd-001_opamp-control-plane.md) remains Draft, and [EP-001](docs/exec-plans/active/ep-001_build-control-plane/EXECPLAN.md) retains its security/identity review blockers. Merging a P1.1 implementation does not constitute design approval.

[RepoFoundryAI](https://github.com/XiaoWeiKIN/RepoFoundryAI) · [OpAMP specification](https://github.com/open-telemetry/opamp-spec) · [opamp-go v0.25.0](https://github.com/open-telemetry/opamp-go/tree/v0.25.0)
