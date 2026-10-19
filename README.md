# APM Control Plane

A Go-based **OpAMP Agent Control Plane**. Long-term scope: Java, Go, Python and other APM Agent distributions, with a later path to Collector management. **First deliverable: a custom OpenTelemetry Java Agent**; the current code is only the Go P0 interoperability PoC.

## Developer architecture documentation

- [**P1–P3 技术选型（中文、待评审）**](docs/architecture/technology-selection.md) — HTTP、配置加载、PostgreSQL/MySQL 多数据库 Driver、SQL 生成、Goose Migration、Telemetry、测试和兼容性选择。

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

## Local startup

Requires Go 1.25+ and access to Go dependencies.

```bash
export APM_OPAMP_TOKEN='replace-with-long-random-agent-token'
export APM_ADMIN_TOKEN='replace-with-a-different-long-admin-token'
go run ./cmd/server
```

Default listener: `127.0.0.1:4320`. Override with `APM_LISTEN_ADDR`. In another terminal:

```bash
export APM_OPAMP_TOKEN='replace-with-long-random-agent-token'
go run ./cmd/testagent -service checkout
```

The test agent prints its 32-character `instance_uid`. Publish a per-instance policy:

```bash
export APM_ADMIN_TOKEN='replace-with-a-different-long-admin-token'
export UID_HEX='<uid-from-testagent>'
curl -H "Authorization: Bearer $APM_ADMIN_TOKEN" http://127.0.0.1:4320/api/v1/agents
curl -X PUT "http://127.0.0.1:4320/api/v1/agents/$UID_HEX/sampling" \
  -H "Authorization: Bearer $APM_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"sampling_rate":0.1}'
```

The test agent should print `Applied policy rate=0.100` after its next poll. The reported configuration hash and status are visible using `GET /api/v1/agents/{uid}`. The demo **only validates and acknowledges JSON**, without modifying any Java SDK sampler.

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

See [architecture, threat model, and P0 limitations](docs/architecture.md).

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
