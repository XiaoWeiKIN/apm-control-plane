# APM Control Plane

A Go-based **OpAMP control plane** for a future custom OpenTelemetry Java Agent distribution.

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

## Upstream

- [OpAMP specification](https://github.com/open-telemetry/opamp-spec)
- [opamp-go](https://github.com/open-telemetry/opamp-go), pinned to v0.25.0
