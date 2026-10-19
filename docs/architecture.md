# Control plane architecture

For the full Chinese developer guide, see [APM Control Plane：开发者架构指南](architecture/README.md). This file documents the narrower P0 implementation boundary.

## P0: protocol interoperability

- The server is a modular starting point based on `opamp-go/server.Attach()` and a shared `net/http` server.
- `/v1/opamp` supports official OpAMP HTTP and WebSocket messages with an Agent bearer token.
- Management APIs have an independent Admin bearer token. Tokens MUST be non-empty and distinct.
- Agent status, identity, and per-instance desired configuration are stored **in memory only**.
- `cmd/testagent` is an HTTP polling test peer (not an instrumented JVM).
- The server sends configuration in the next `AgentToServer` response. A quiet WebSocket connection does not receive an immediate push.
- A config is applied only after its hash is acknowledged by `RemoteConfigStatus`. Delivery is not interpreted as application.

## Deliberately out of scope for P0

- PostgreSQL, durable desired state, policy revisions and service/environment selectors.
- Full Agent Inventory persistence, global rollout, HA and scoped tenant identity.
- Authenticated identity binding to an agent instance: `service.name` and `deployment.environment.name` are **untrusted metadata**.
- Any Java SDK sampler modification, instrumentation toggling, agent upgrade or OTLP data handling.
- Production-ready mTLS, per-agent credentials, RBAC and secrets rotation.

## Planned P1 / P2

1. Agent Registry with PostgreSQL and instance identity, credential-to-tenant binding.
2. Immutable sampling policy versions, selectors, and assignments with desired/observed state.
3. Reconciliation, offline recovery, retry/backoff, audit history, configuration status.
4. WS push scheduling, multi-replica connection ownership and rollout metrics.

## Deployment safety

Development defaults to `127.0.0.1` on plaintext HTTP. Do not expose this listener to untrusted networks. Terminate TLS before externally reachable OpAMP and management endpoints, and provide narrowly scoped credentials.
